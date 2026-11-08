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

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/protocol"
)

// ErrConfiguration reports a complete candidate failure: nothing was
// published, the source error is preserved inside the wrap, and cancellation
// stays unwrapped.
var ErrConfiguration = errors.New("runtime: configuration failure")

// The first-run document enables the supplied optional extensions without
// choosing a model. Existing configuration files are never rewritten here.
const (
	mainConfigSkeleton = `{
  "providers": {},
  "extensions": {"agents": {"enabled": true}, "adaptation": {"enabled": true}}
}
`
)

// configurationService is the private Runtime configuration service. It
// owns plugin registrations, captures the main document once per build and
// composes only enabled installed plugins. Provider assembly uses
// Loader.LoadCaptured; active Agent definitions and extension settings are
// validated before factories run and the candidate can publish.
//
// One service-local build mutex serializes initial load and reload from input
// reads through publication. The current snapshot is one atomic pointer that
// readers Load once without a lock. A failed or canceled build publishes
// nothing: initial publication uses generation 1, only a success increments
// it, and the increment happens under the build mutex. Builds prepare the
// candidate's long-lived plugin scope before publication. Each
// publication carries its passive configuration event inside the same
// observation section as its atomic Store.
type configurationService struct {
	loader     *catalog.Loader
	configPath string
	natives    []extension.Native
	nativeIDs  map[string]bool

	owner       context.Context
	obs         *observation
	composition *composition
	dataDir     string
	cleanupMu   sync.Mutex
	cleanupErr  error

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

	// heldMu owns the owner-wide private reference counts for credential
	// names currently protected by a reserving capture or a live resource
	// set. Only names live here; no value, token or public manager.
	heldMu    sync.Mutex
	heldNames map[string]int
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

	// releaseNames drops this reserving capture's private name reference
	// counts after its resource cleanup finishes. It is nil for non-reserving
	// captures.
	releaseNames func()
}

// newConfigurationService binds the service to the composition's owned
// declarations, the Loader rooted at the home the Runtime resolved once, the
// Runtime owner context, and the Runtime's passive observation. Neither DataDir nor ConfigPath
// relocates the home-based discovery cache or its locks and TTL records, and
// the service reads no dotenv: that belongs to Runtime startup, not to any
// build or reload.
func newConfigurationService(owner context.Context, c *composition, loader *catalog.Loader, configPath string, obs *observation) *configurationService {
	nativeIDs := make(map[string]bool, len(c.natives))
	for _, native := range c.natives {
		nativeIDs[native.Declaration.ID] = true
	}
	return &configurationService{
		loader:     loader,
		configPath: configPath,
		natives:    c.natives,
		nativeIDs:  nativeIDs,
		owner:      owner,
		obs:        obs,
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
	return s.captureFor(ctx, false)
}

func (s *configurationService) captureFor(ctx context.Context, reserve bool) (configurationCapture, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	return s.captureBody(ctx, reserve)
}

// capturePreparation is the explicit preparation admission capture: the same
// common capture body under captureMu, followed by its owner-wide name hold
// before the mutex releases. No consumer can observe an accepted preparation
// whose names are not yet registered.
func (s *configurationService) capturePreparation(ctx context.Context) (configurationCapture, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	captured, err := s.captureBody(ctx, true)
	if err != nil {
		return configurationCapture{}, err
	}
	if captured.snapshot != nil && captured.snapshot.resources != nil {
		names := make([]string, 0, len(captured.credentials))
		for name := range captured.credentials {
			names = append(names, name)
		}
		s.holdCredentialNames(names)
		captured.releaseNames = func() { s.releaseCredentialNames(names) }
	}
	return captured, nil
}

// captureBody is the common reservation body under captureMu: cancellation,
// one ready pointer, optional resource retention and the one credential
// observation. Generic resource captures do not become admission name holds.
func (s *configurationService) captureBody(ctx context.Context, reserve bool) (configurationCapture, error) {
	if err := s.canceled(ctx); err != nil {
		return configurationCapture{}, err
	}
	snapshot := s.published.Load()
	if snapshot == nil {
		return configurationCapture{}, nil
	}
	if reserve && snapshot.resources != nil {
		snapshot.resources.retain()
	}
	return configurationCapture{snapshot: snapshot, credentials: s.captureCredentials(snapshot)}, nil
}

func (c configurationCapture) releaseHeldNames() {
	if c.releaseNames != nil {
		c.releaseNames()
	}
}

// holdCredentialNames registers one private reference count per name. The
// caller owns the matching release; names remain excluded until every holder
// from both reserving captures and resource sets has released.
func (s *configurationService) holdCredentialNames(names []string) {
	if len(names) == 0 {
		return
	}
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	if s.heldNames == nil {
		s.heldNames = make(map[string]int)
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		s.heldNames[name]++
	}
}

func (s *configurationService) releaseCredentialNames(names []string) {
	if len(names) == 0 {
		return
	}
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	for _, name := range names {
		if name == "" {
			continue
		}
		if s.heldNames[name] <= 1 {
			delete(s.heldNames, name)
			continue
		}
		s.heldNames[name]--
	}
}

// heldCredentialNames returns a snapshot of the owner-wide held name union.
func (s *configurationService) heldCredentialNames() []string {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	if len(s.heldNames) == 0 {
		return nil
	}
	names := make([]string, 0, len(s.heldNames))
	for name := range s.heldNames {
		names = append(names, name)
	}
	return names
}

// subprocessEnvironment is the one production child-environment projection:
// it takes the managed-environment snapshot, the current revision's provider
// and declared names, and the owner-wide held-name union under one
// publication/capture ownership. A capture or publication therefore cannot
// land between the held and current reads. No lock survives this call. A nil
// manager yields no environment, preserving fail-closed command behavior; a
// nil ready snapshot contributes no current names.
func (s *configurationService) subprocessEnvironment() []string {
	if s.env == nil {
		return nil
	}
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	env := s.env.SubprocessEnv()
	if env == nil {
		return nil
	}
	var names []string
	s.heldMu.Lock()
	for name := range s.heldNames {
		names = append(names, name)
	}
	s.heldMu.Unlock()
	current := s.published.Load()
	if current != nil {
		for _, prov := range current.catalog.Providers {
			if prov != nil && prov.Transport.APIKeyEnv != "" {
				names = append(names, prov.Transport.APIKeyEnv)
			}
		}
		names = append(names, current.declaredSecretNames()...)
	}
	return config.EnvWithoutKeys(env, names)
}

// captureCredentials samples the union of every nonempty api_key_env the
// captured catalog references and every declared reference name in one
// environment-owner observation. The returned EnvValue records are the
// owner's actual observations, unchanged; declared candidate bindings govern
// extension activation/Operation resources and never rewrite provider
// Value/Defined/Managed. Capture owns the dedupe; a nil manager samples the
// environment alone (no managed ownership).
func (s *configurationService) captureCredentials(snapshot *configuration) map[string]config.EnvValue {
	names := make([]string, 0, len(snapshot.catalog.Providers)+len(snapshot.secrets))
	for _, prov := range snapshot.catalog.Providers {
		if prov != nil && prov.Transport.APIKeyEnv != "" {
			names = append(names, prov.Transport.APIKeyEnv)
		}
	}
	for _, secret := range snapshot.secrets {
		names = append(names, secret.name)
	}
	return s.env.Capture(names)
}

// captureDeclaredBindings takes one private candidate observation of the
// active declared field names and values. A pending connection key action is
// applied to the matching binding so a build that only later writes the
// managed key does not publish a stale declared binding. The result is used
// for resource-set adoption and every component activation value; it is never
// durable.
func (s *configurationService) captureDeclaredBindings(secrets []declaredSecret, key *connectionEffects) map[declaredField]declaredBinding {
	if len(secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(secrets))
	seen := make(map[string]bool, len(secrets))
	for _, secret := range secrets {
		if seen[secret.name] {
			continue
		}
		seen[secret.name] = true
		names = append(names, secret.name)
	}
	captured := s.env.Capture(names)
	out := make(map[declaredField]declaredBinding, len(secrets))
	for _, secret := range secrets {
		value := captured[secret.name].Value
		if key != nil && key.keyEnv == secret.name {
			switch key.keyAction {
			case keyActionSet:
				value = key.keyValue
			case keyActionRemove:
				value = ""
			}
		}
		out[declaredField{extensionID: secret.extensionID, field: secret.field}] = declaredBinding{name: secret.name, value: value}
	}
	return out
}

// operationSecrets derives the private per-extension Operation credential
// fields from this preparation's one credential observation. Only present
// values enter the map; absent, unresolved and undefined values are simply
// missing lookups, never a separate map shape or Operation-presence signal.
func (c configurationCapture) operationSecrets() map[string]map[string]string {
	if c.snapshot == nil {
		return nil
	}
	var out map[string]map[string]string
	for _, secret := range c.snapshot.secrets {
		if secret.lifetime != extension.SecretOperation {
			continue
		}
		value := c.credentials[secret.name].Value
		if value == "" {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]string)
		}
		perExtension := out[secret.extensionID]
		if perExtension == nil {
			perExtension = make(map[string]string)
			out[secret.extensionID] = perExtension
		}
		perExtension[secret.field] = value
	}
	return out
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
	if prePublishBarrier != nil {
		prePublishBarrier()
	}
	// Immediately before the atomic Store, caller and owner cancellation are
	// checked once more under the caller-first rule. A publication that wins
	// this check may finish despite later cancellation; shutdown joins it.
	if err := s.canceled(ctx); err != nil {
		fresh := s.freshResources(candidate)
		s.buildMu.Unlock()
		if fresh != nil {
			_ = fresh.release()
		}
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
	credentials := s.captureCredentials(candidate)
	captured := configurationCapture{snapshot: candidate, credentials: credentials}
	previous := s.published.Load()
	s.obs.publish(func() []Event {
		warningChanged := false
		if s.warnings != nil {
			warningChanged = s.warnings.setGlobal(setupSource, setupWarnings(candidate, credentials)) || warningChanged
			warningChanged = s.warnings.setGlobal(catalogSource, catalogWarnings(candidate.catalogWarnings)) || warningChanged
		}
		var events []Event
		if candidate.resources != nil && (previous == nil || previous.resources != candidate.resources) {
			candidate.resources.runtime.obs = s.obs
			warningChanged = candidate.resources.commitWarnings() || warningChanged
			events = append(events, scopeEvent(protocol.ScopeOpened, candidate.resources.runtime.info))
		}
		revision := s.warnings.storeRevision()
		s.published.Store(candidate)
		s.captureMu.Unlock()
		s.buildMu.Unlock()
		events = append(events, configurationChangedEvent(candidate.generation))
		if warningChanged {
			events = append(events, warningChangedEvent(runtimeEventScope(), revision))
		}
		return events
	})
	if previous != nil && previous.resources != nil && previous.resources != candidate.resources {
		_ = previous.resources.release()
	}
	return captured
}

func (s *configurationService) freshResources(candidate *configuration) *resourceSet {
	if candidate != nil && candidate.resources != nil {
		previous := s.current()
		if previous == nil || previous.resources != candidate.resources {
			return candidate.resources
		}
	}
	return nil
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

// build captures the main document once and passes it to candidate construction.
func (s *configurationService) build(ctx context.Context, generation uint64) (*configuration, error) {
	configData, err := captureConfiguredFile(s.configPath, mainConfigSkeleton)
	if err != nil {
		return nil, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	return s.buildCaptured(ctx, generation, configData, nil, nil)
}

// buildCaptured is the one candidate construction every publisher — Reload's
// capture and a mutation's edited bytes — runs: the captured document is
// decoded once, the captured providers layer is delegated to the Loader so
// provider assembly and every cost protection use that one read, sessions
// and agent definitions are parsed against the ordinary visible export IDs
// and the compiled tool universe, the Workspace permission inventory is
// enumerated once into the candidate, and the plugin section is validated
// against the owned declarations and its resource set is prepared or reused.
// Nothing is returned until the complete candidate survives all of it. conn is
// the catalog-loader input: nil builds over the ordinary LoadCaptured inputs
// (which may refresh due providers and write their cache); a connection
// candidate instead selects the non-refreshing LoadCapturedConnection entry,
// optionally overlaying the fetched discovery for that one provider. Only the
// connection edit supplies conn; key is the separate pending managed-key
// action used by declared-binding capture, present for every key-bearing edit
// including custom creates, and it never changes the catalog-loader choice. A
// connection candidate is fully validated before its caller's first
// persistence and is never rebuilt after it.
func (s *configurationService) buildCaptured(ctx context.Context, generation uint64, configData []byte, conn, key *connectionEffects) (*configuration, error) {
	if candidateBarrier != nil {
		candidateBarrier()
	}
	doc, err := decodeCapturedConfig(configData)
	if err != nil {
		return nil, configurationFailure(err)
	}
	var active []extension.Native
	for _, native := range s.natives {
		if sectionEnabled(doc.Extensions[native.Declaration.ID]) {
			active = append(active, native)
		}
	}
	graph, err := newNativeComposition(active)
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
	snapshot, err := newConfiguration(generation, doc, built, captureWorkspacePermissions())
	if err != nil {
		return nil, configurationFailure(err)
	}
	snapshot.composition = graph
	snapshot.secrets = resolveDeclaredSecrets(active, doc.Extensions)
	snapshot.declaredBindings = s.captureDeclaredBindings(snapshot.secrets, key)
	if err := s.validateExtensions(ctx, graph, doc.Extensions); err != nil {
		return nil, err
	}
	if err := s.prepareResources(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := s.projectDefinitions(ctx, snapshot); err != nil {
		if fresh := s.freshResources(snapshot); fresh != nil {
			_ = fresh.release()
		}
		return nil, err
	}
	return snapshot, nil
}

// candidateBarrier and prePublishBarrier are test-only construction seams;
// nil in production. They add no behavior to the owner.
var (
	candidateBarrier  func()
	prePublishBarrier func()
)

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

// sectionEnabled recognizes the host-owned participation flag without
// interpreting other members of an inactive document.
func sectionEnabled(raw json.RawMessage) bool {
	return config.SectionEnabled(raw)
}

// validateExtensions validates only active installed extension sections
// against their compiled local schemas before publication.
func (s *configurationService) validateExtensions(ctx context.Context, graph *composition, sections map[string]json.RawMessage) error {
	if err := s.canceled(ctx); err != nil {
		return err
	}
	if err := graph.validateSettings(sections); err != nil {
		return configurationFailure(err)
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
