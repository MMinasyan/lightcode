package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
)

// ErrConfiguration reports a complete candidate failure: nothing was
// published, the source error is preserved inside the wrap, and cancellation
// stays unwrapped.
var ErrConfiguration = errors.New("runtime: configuration failure")

// The first-run skeletons match what the retained catalog and agent loaders
// create, so a captured build on a fresh machine leaves the same editable
// files behind.
const (
	mainConfigSkeleton = `{
  "providers": {}
}
`
	agentsSkeleton = "{}\n"
)

// configurationService is the private Runtime configuration service. It
// receives the owned compiled plugin declarations from composition before any
// factory runs, captures the main and agents bytes once per build, delegates
// provider assembly to Loader.LoadCaptured, parses sessions and agent
// definitions against the ordinary visible export IDs, and validates the
// plugin JSON against the declarations itself. The catalog receives no plugin
// types or validators, and declarations, not constructed instances, define
// the capability universe.
//
// One service-local build mutex serializes initial load and reload from input
// reads through publication. The current snapshot is one atomic pointer that
// readers Load once without a lock. A failed or canceled build publishes
// nothing: initial publication uses generation 1, only a success increments
// it, and the increment happens under the build mutex. Builds run outside any
// Runtime-wide lock and start no plugin or other service work. Each
// publication carries its passive configuration event inside the same
// observation section as its atomic Store.
type configurationService struct {
	loader        *catalog.Loader
	configPath    string
	agentsPath    string
	plugins       []Plugin
	pluginIDs     map[string]bool
	capabilityIDs []string
	toolIDs       []string

	// defaultCapabilityIDs is empty, or the single composed ModelAdaptation
	// export ID installed on the built-in primary definition before ordinary
	// inheritance. No plugin callback supplies it.
	defaultCapabilityIDs []string

	owner context.Context
	obs   *observation

	// warnings is the Runtime-owned presentation store every successful
	// publication's global setup/catalog/agents refresh reports into; nil
	// (isolated service tests) drops only that passive presentation.
	warnings *warningStore

	// env is the Runtime's retained managed-environment manager, attached
	// like the warning store; nil (isolated service tests) makes every key
	// action the manager's own typed failure — no manager is constructed on
	// demand.
	env *config.ManagedEnv

	// buildMu serializes initial load, reload, and every mutation's input
	// read through candidate construction. captureMu is the separate
	// publication/capture owner: readers sample the ready pointer and the
	// credential names it references under it, and a writer takes it before
	// any mutable credential effect and holds it through publication. A slow
	// build therefore never blocks reads, and no reader can observe a
	// managed effect before the generation that owns it.
	buildMu   sync.Mutex
	captureMu sync.Mutex
	published atomic.Pointer[configuration]
}

// configurationCapture joins one published immutable configuration revision
// with the owner-coherent credential observations its catalog references.
// The named snapshot field keeps the captured revision explicit at every
// consumer: no promoted access can read a field without naming the snapshot,
// and the credential map is infrastructure data that never reaches a
// protocol DTO, durable capture, error, or log.
type configurationCapture struct {
	snapshot    *configuration
	credentials map[string]config.EnvValue
}

// newConfigurationService binds the service to the composition's owned
// declarations, the Loader rooted at the home the Runtime resolved once, the
// Runtime owner context, and the Runtime's passive observation. Neither
// DataDir nor ConfigPath relocates the home-based discovery cache or its
// locks and TTL records, and the service reads no dotenv: that belongs to
// Runtime startup, not to any build or reload.
func newConfigurationService(owner context.Context, c *composition, loader *catalog.Loader, configPath string, obs *observation) *configurationService {
	pluginIDs := make(map[string]bool, len(c.plugins))
	for _, p := range c.plugins {
		pluginIDs[p.ID] = true
	}
	defaultCapabilityIDs := []string(nil)
	if c.modelAdaptation != "" {
		defaultCapabilityIDs = []string{c.modelAdaptation}
	}
	return &configurationService{
		loader:               loader,
		configPath:           configPath,
		agentsPath:           agents.PathForConfig(configPath),
		plugins:              c.plugins,
		pluginIDs:            pluginIDs,
		capabilityIDs:        c.capabilityIDs,
		toolIDs:              c.toolIDs,
		defaultCapabilityIDs: defaultCapabilityIDs,
		owner:                owner,
		obs:                  obs,
	}
}

// current returns the latest published snapshot, or nil before the initial
// publication. Readers never take the build mutex.
func (s *configurationService) current() *configuration {
	return s.published.Load()
}

// capture takes publication/capture ownership, checks cancellation and owner
// lifetime, loads the ready pointer once, and samples the credential names
// that pointer's catalog references through the environment owner. The mutex
// is released before any projection, preparation, plugin, or network work, so
// callers hold one owned observation and never a lock. A nil pointer (no
// publication yet) yields the zero capture, matching current()'s contract.
func (s *configurationService) capture(ctx context.Context) (configurationCapture, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if err := s.canceled(ctx); err != nil {
		return configurationCapture{}, err
	}
	snapshot := s.published.Load()
	if snapshot == nil {
		return configurationCapture{}, nil
	}
	return configurationCapture{snapshot: snapshot, credentials: s.captureCredentials(snapshot.catalog)}, nil
}

// captureCredentials samples every nonempty api_key_env the captured catalog
// references through the one environment owner; Capture owns the dedupe. A
// nil manager samples the environment alone (no managed ownership).
func (s *configurationService) captureCredentials(cat *catalog.Catalog) map[string]config.EnvValue {
	names := make([]string, 0, len(cat.Providers))
	for _, prov := range cat.Providers {
		if prov != nil && prov.Transport.APIKeyEnv != "" {
			names = append(names, prov.Transport.APIKeyEnv)
		}
	}
	return s.env.Capture(names)
}

// attachWarnings wires the Runtime-owned warning store. Every successful
// publication refreshes the global setup/catalog/agents groups from its
// published candidate at this shared publication path; a failed or canceled
// build refreshes nothing.
func (s *configurationService) attachWarnings(warnings *warningStore) {
	s.warnings = warnings
}

// attachEnv wires the Runtime's retained managed-environment manager: the
// one connection key actions write through.
func (s *configurationService) attachEnv(env *config.ManagedEnv) {
	s.env = env
}

// publish runs one serialized initial-load or reload build and publishes the
// complete candidate with its next generation as a single atomic Store,
// carrying the configuration event in the same observation section as that
// Store. Publication/capture ownership is taken only after the candidate
// validated, so the build never blocks reads of the ready revision; the
// commit releases both mutexes before the enqueue.
func (s *configurationService) publish(ctx context.Context) (configurationCapture, error) {
	if err := s.canceled(ctx); err != nil {
		return configurationCapture{}, err
	}
	s.buildMu.Lock()
	// A cancellation observed while waiting is returned after the active
	// builder releases the mutex, without starting another build.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	generation, err := s.nextGeneration()
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	candidate, err := s.build(ctx, generation)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	// Immediately before the atomic Store, caller and owner cancellation are
	// checked once more under the caller-first rule. A publication that wins
	// this check may finish despite later cancellation; shutdown joins it.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	s.captureMu.Lock()
	return s.commit(candidate), nil
}

// nextGeneration reserves the next publication generation; the caller holds
// the build mutex. Initial publication uses generation 1 and only a success
// increments it.
func (s *configurationService) nextGeneration() (uint64, error) {
	generation := uint64(1)
	if snapshot := s.published.Load(); snapshot != nil {
		generation = snapshot.generation + 1
		if generation == 0 {
			return 0, fmt.Errorf("configuration publication generation exhausted: %w", ErrConfiguration)
		}
	}
	return generation, nil
}

// commit publishes the validated candidate as the ready snapshot with its
// events; the caller holds the build and capture mutexes. It captures the
// candidate catalog's credential observations after every accepted key effect
// and uses those values for the setup warnings, so the published revision and
// the credentials its projection consumes are one frozen pair. The global
// warning groups follow the published candidate inside the one observation
// section, so a later publication can never interleave an earlier candidate's
// refresh and no nested publication exists. The section stores the candidate,
// releases the capture then the build mutex, and enqueues the configuration
// event plus one final runtime-scoped warning event exactly when any group
// changed — each changed group keeps its own revision increment. No
// observation callback reacquires capture ownership.
func (s *configurationService) commit(candidate *configuration) configurationCapture {
	credentials := s.captureCredentials(candidate.catalog)
	captured := configurationCapture{snapshot: candidate, credentials: credentials}
	s.obs.publish(func() []Event {
		warningChanged := false
		if s.warnings != nil {
			warningChanged = s.warnings.setGlobal(setupSource, setupWarnings(candidate, credentials)) || warningChanged
			warningChanged = s.warnings.setGlobal(catalogSource, catalogWarnings(candidate.catalogWarnings)) || warningChanged
			warningChanged = s.warnings.setGlobal(agentsSource, agentWarnings(candidate.agentWarnings)) || warningChanged
		}
		revision := s.warnings.storeRevision()
		s.published.Store(candidate)
		s.captureMu.Unlock()
		s.buildMu.Unlock()
		events := []Event{configurationChangedEvent(candidate.generation)}
		if warningChanged {
			events = append(events, warningChangedEvent(runtimeEventScope(), revision))
		}
		return events
	})
	return captured
}

// canceled applies the caller-first cancellation rule: a done caller context
// reports its own error even when the owner is done too; otherwise a done
// owner ends admission with ErrClosed, and a live caller and owner pass.
func (s *configurationService) canceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.owner.Err() != nil {
		return ErrClosed
	}
	return nil
}

// build reads and interprets exactly one input set: the main and agents bytes
// are captured once (preserving the first-run skeletons), then the shared
// candidate construction consumes those captured bytes.
func (s *configurationService) build(ctx context.Context, generation uint64) (*configuration, error) {
	configData, err := captureConfiguredFile(s.configPath, mainConfigSkeleton)
	if err != nil {
		return nil, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	agentsData, err := captureConfiguredFile(s.agentsPath, agentsSkeleton)
	if err != nil {
		return nil, configurationFailure(fmt.Errorf("read agent definitions: %w", err))
	}
	return s.buildCaptured(ctx, generation, configData, agentsData, nil)
}

// buildCaptured is the one candidate construction every publisher — Reload's
// capture and a mutation's edited bytes — runs: the captured documents are
// decoded once, the captured providers layer is delegated to the Loader so
// provider assembly and every cost protection use that one read, sessions
// and agent definitions are parsed against the ordinary visible export IDs
// and the compiled tool universe, the Workspace permission inventory is
// enumerated once into the candidate, and the plugin section is validated
// against the owned declarations. Nothing is returned until the complete
// candidate survives all of it. A nil conn builds over the ordinary
// LoadCaptured inputs (which may refresh due providers and write their
// cache); a connection candidate instead selects the non-refreshing
// LoadCapturedConnection entry, optionally overlaying the fetched discovery
// for that one provider — a connection candidate is fully validated before
// its caller's first persistence and is never rebuilt after it.
func (s *configurationService) buildCaptured(ctx context.Context, generation uint64, configData, agentsData []byte, conn *connectionEffects) (*configuration, error) {
	doc, err := decodeCapturedConfig(configData)
	if err != nil {
		return nil, configurationFailure(err)
	}
	var built catalog.BuildResult
	if conn == nil {
		built, err = s.loader.LoadCaptured(ctx, doc.Providers)
	} else {
		built, err = s.loader.LoadCapturedConnection(ctx, doc.Providers, conn.providerID, conn.transport, conn.discovered)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, configurationFailure(err)
	}
	snapshot, err := newConfiguration(generation, doc, built, agentsData, s.capabilityIDs, s.toolIDs, s.defaultCapabilityIDs, captureWorkspacePermissions())
	if err != nil {
		return nil, configurationFailure(err)
	}
	if err := s.validatePlugins(ctx, doc.Plugins); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// captureWorkspacePermissions enumerates the home-based per-Workspace
// permission inventory during one configuration build and captures each
// present readable permissions.json's raw bytes keyed by its directory ID. A
// missing directory is the fresh-install absent case and a failed enumeration
// leaves the whole Workspace policy level absent for this revision; a missing
// or unreadable file inside the inventory is absent policy for that Workspace
// alone. A successfully read malformed file is captured unchanged —
// resolution determines validity from the raw bytes. No enumeration failure
// fails publication, and nothing here reads project records, meta.json, or
// locks.
func captureWorkspacePermissions() map[string]json.RawMessage {
	root, err := config.ProjectsDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	captured := make(map[string]json.RawMessage, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "permissions.json"))
		if err != nil {
			continue
		}
		captured[entry.Name()] = data
	}
	return captured
}

// validatePlugins checks the captured plugins section against the compiled
// declarations: it is an object keyed by plugin ID (absent passes nil), every
// value must be a JSON object, an unknown ID rejects the candidate, and each
// declared plugin receives step 3's no-settings rule or has its owned input
// validated unchanged, constructing and mutating no instance. Cancellation
// observed between validators prevents later validation and publication;
// validation starts no service work.
func (s *configurationService) validatePlugins(ctx context.Context, plugins map[string]json.RawMessage) error {
	for pluginID, raw := range plugins {
		if !s.pluginIDs[pluginID] {
			return configurationFailure(fmt.Errorf("plugins entry %q is not a compiled plugin", pluginID))
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return configurationFailure(fmt.Errorf("plugins entry %q is not a JSON object", pluginID))
		}
	}
	for _, p := range s.plugins {
		if err := s.canceled(ctx); err != nil {
			return err
		}
		if err := acceptSettings(p, plugins[p.ID]); err != nil {
			return configurationFailure(err)
		}
	}
	return nil
}

// captureConfiguredFile reads one configured input file's bytes once and
// preserves the first-run skeleton: a missing file is exclusively created
// with the same content the retained loaders write, then re-read, so a losing
// concurrent creator observes the winner's content.
func captureConfiguredFile(path, skeleton string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if _, err := atomicfs.CreateExclusive(path, []byte(skeleton), 0o600); err != nil {
			return nil, err
		}
		data, err = os.ReadFile(path)
	}
	return data, err
}

// configurationFailure classifies a complete candidate failure while
// preserving the source error inside the wrap; cancellation never reaches it
// and stays unwrapped.
func configurationFailure(err error) error {
	return fmt.Errorf("%w: %w", ErrConfiguration, err)
}
