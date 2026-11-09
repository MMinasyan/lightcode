package melta

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
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/protocol"
)

// ErrConfiguration reports a complete candidate failure: nothing was
// published, the source error is preserved inside the wrap, and cancellation
// stays unwrapped.
var ErrConfiguration = errors.New("melta: configuration failure")

// configurationService is the private Melta configuration service. It
// owns plugin registrations, reads the supplied document once per build and
// composes only enabled installed plugins. Provider assembly uses the
// catalog loader's captured entries; active Agent definitions and extension
// settings are validated before factories run and the candidate can publish.
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
	loader        *catalog.Loader
	configuration ConfigurationSource
	credentials   CredentialSource
	locations     Locations
	strategy      *harness.StrategyBinding
	natives       []extension.Native
	nativeIDs     map[string]bool

	owner       context.Context
	obs         *observation
	composition *composition
	dataDir     string
	instance    string
	cleanupMu   sync.Mutex
	cleanupErr  error

	// warnings is the Melta-owned presentation store every successful
	// publication's global setup/catalog/agents refresh reports into; nil
	// (isolated service tests) drops only that passive presentation.
	warnings *warningStore

	// buildMu serializes initial load, reload, and every mutation's input
	// read through candidate construction. Readers load the ready snapshot
	// pointer without it; credentials are published inside that snapshot.
	buildMu   sync.Mutex
	published atomic.Pointer[configuration]

	// heldMu owns the owner-wide private reference counts for credential
	// names currently protected by a reserving capture or a live resource
	// set. Only names live here; no value, token or public manager.
	heldMu    sync.Mutex
	heldNames map[string]int
}

// configurationCapture joins one published immutable configuration revision
// with the credential observations its catalog references. The named snapshot
// field keeps the captured revision explicit at every consumer: no promoted
// access can read a field without naming the snapshot, and the credential map
// is infrastructure data that never reaches a protocol DTO, durable capture,
// error, or log.
type configurationCapture struct {
	snapshot *configuration

	// releaseNames drops this reserving capture's private name reference
	// counts after its resource cleanup finishes. It is nil for non-reserving
	// captures.
	releaseNames func()
}

// newConfigurationService binds the service to the composition's owned
// declarations, the supplied configuration, credential and location sources,
// the Melta owner context, and the Melta's passive observation. No build
// reads a home directory, process environment, or path not named by these
// inputs.
func newConfigurationService(owner context.Context, c *composition, loader *catalog.Loader, configuration ConfigurationSource, credentials CredentialSource, locations Locations, strategy *harness.StrategyBinding, obs *observation) *configurationService {
	nativeIDs := make(map[string]bool, len(c.natives))
	for _, native := range c.natives {
		nativeIDs[native.Declaration.ID] = true
	}
	return &configurationService{
		loader:        loader,
		configuration: configuration,
		credentials:   credentials,
		locations:     locations,
		strategy:      strategy,
		natives:       c.natives,
		nativeIDs:     nativeIDs,
		owner:         owner,
		obs:           obs,
	}
}

// current returns the latest published snapshot, or nil before the initial
// publication. Readers never take the build mutex.
func (s *configurationService) current() *configuration {
	return s.published.Load()
}

// capture takes one owned capture: cancellation, the ready snapshot pointer,
// and optional resource retention. Readers never take the build mutex and
// never sample a credential source; the published snapshot carries the
// configuration and its credential observations as one pair.
func (s *configurationService) capture(ctx context.Context) (configurationCapture, error) {
	return s.captureFor(ctx, false)
}

func (s *configurationService) captureFor(ctx context.Context, reserve bool) (configurationCapture, error) {
	return s.captureBody(ctx, reserve)
}

// capturePreparation is the explicit preparation admission capture: the
// published-pointer load and its owner-wide name hold happen under heldMu, so
// the child-environment union never observes an accepted preparation whose
// names are not yet registered. No source call or publication runs here.
func (s *configurationService) capturePreparation(ctx context.Context) (configurationCapture, error) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	captured, err := s.captureBodyLocked(ctx, true)
	if err != nil {
		return configurationCapture{}, err
	}
	if captured.snapshot != nil && captured.snapshot.resources != nil && len(captured.snapshot.credentialNames) != 0 {
		names := captured.snapshot.credentialNames
		s.holdCredentialNamesLocked(names)
		captured.releaseNames = func() { s.releaseCredentialNames(names) }
	}
	return captured, nil
}

// captureBody is the common reservation body: cancellation, one ready pointer
// and optional resource retention. A reserving load and its retain run under
// heldMu, the same short mutex the publication Store takes, so a retain can
// never follow a Store that released the old set. Generic resource captures do
// not become admission name holds.
func (s *configurationService) captureBody(ctx context.Context, reserve bool) (configurationCapture, error) {
	if !reserve {
		return s.captureBodyLocked(ctx, false)
	}
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	return s.captureBodyLocked(ctx, true)
}

// captureBodyLocked is captureBody under the Caller-held heldMu when reserving.
func (s *configurationService) captureBodyLocked(ctx context.Context, reserve bool) (configurationCapture, error) {
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
	return configurationCapture{snapshot: snapshot}, nil
}

func (c configurationCapture) releaseHeldNames() {
	if c.releaseNames != nil {
		c.releaseNames()
	}
}

// holdCredentialNames registers one private reference count per name. The
// Caller owns the matching release; names remain excluded until every holder
// from both reserving captures and resource sets has released.
func (s *configurationService) holdCredentialNames(names []string) {
	if len(names) == 0 {
		return
	}
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.holdCredentialNamesLocked(names)
}

func (s *configurationService) holdCredentialNamesLocked(names []string) {
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
// under heldMu it collects the owner-wide held-name union, the current
// revision's provider and declared names, then releases the lock and asks the
// credential source for the process environment without those names. No lock
// survives the source call. An error fails the spawn through the command
// path.
func (s *configurationService) subprocessEnvironment(ctx context.Context) ([]string, error) {
	var names []string
	s.heldMu.Lock()
	for name := range s.heldNames {
		names = append(names, name)
	}
	current := s.published.Load()
	if current != nil {
		for _, prov := range current.catalog.Providers {
			if prov != nil && prov.Transport.APIKeyEnv != "" {
				names = append(names, prov.Transport.APIKeyEnv)
			}
		}
		names = append(names, current.declaredSecretNames()...)
	}
	s.heldMu.Unlock()
	return s.credentials.SubprocessEnv(ctx, names)
}

// captureCredentials performs the build's one credential observation over the
// union of every nonempty api_key_env the candidate catalog references, every
// declared reference name, and the pending connection key name, then folds the
// pending key action into that same map. Every name is observed exactly once
// in the build; provider and declared observations derive from the one
// resulting map, and no source call runs after the owning write. The returned
// names are the full requested union, independent of a source that returns
// only some keys.
func (s *configurationService) captureCredentials(ctx context.Context, names []string, key *connectionEffects) (map[string]CredentialValue, []string, error) {
	seen := make(map[string]bool, len(names)+1)
	unique := make([]string, 0, len(names)+1)
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	if key != nil && key.keyEnv != "" && !seen[key.keyEnv] {
		unique = append(unique, key.keyEnv)
	}
	captured := make(map[string]CredentialValue, len(unique))
	if len(unique) != 0 {
		fresh, err := s.credentials.Capture(ctx, unique)
		if err != nil {
			return nil, nil, err
		}
		for name, value := range fresh {
			captured[name] = value
		}
	}
	if key != nil && key.keyEnv != "" {
		switch key.keyAction {
		case keyActionSet:
			captured[key.keyEnv] = CredentialValue{Value: key.keyValue, Defined: true, Managed: true}
		case keyActionRemove:
			captured[key.keyEnv] = CredentialValue{}
		}
	}
	return captured, unique, nil
}

// credentialValues projects one captured credential map onto the catalog
// loader's value-only view. Discovery readiness and authorization consume only
// the non-empty value; ownership stays in the published revision.
func credentialValues(captured map[string]CredentialValue) map[string]string {
	out := make(map[string]string, len(captured))
	for name, value := range captured {
		if value.Value != "" {
			out[name] = value.Value
		}
	}
	return out
}

// capturedCredentialNames returns the candidate catalog's provider credential
// names and every active declared name, the one capture input set.
func capturedCredentialNames(cat *catalog.Catalog, secrets []declaredSecret) []string {
	var names []string
	if cat != nil {
		names = make([]string, 0, len(cat.Providers)+len(secrets))
		for _, prov := range cat.Providers {
			if prov != nil && prov.Transport.APIKeyEnv != "" {
				names = append(names, prov.Transport.APIKeyEnv)
			}
		}
	}
	for _, secret := range secrets {
		names = append(names, secret.name)
	}
	return names
}

// captureDeclaredBindings derives the active declared field bindings from the
// same build capture. A pending connection key action is applied to the
// matching binding so a build that only later writes the managed key does not
// publish a stale declared binding. The result is used for resource-set
// adoption and every component activation value; it is never durable.
func captureDeclaredBindings(secrets []declaredSecret, captured map[string]CredentialValue) map[declaredField]declaredBinding {
	if len(secrets) == 0 {
		return nil
	}
	out := make(map[declaredField]declaredBinding, len(secrets))
	for _, secret := range secrets {
		out[declaredField{extensionID: secret.extensionID, field: secret.field}] = declaredBinding{name: secret.name, value: captured[secret.name].Value}
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
		value := c.snapshot.credentials[secret.name].Value
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

// attachWarnings wires the Melta-owned warning store. Every successful
// publication refreshes the global setup/catalog/agents groups from its
// published candidate at this shared publication path; a failed or canceled
// build refreshes nothing.
func (s *configurationService) attachWarnings(warnings *warningStore) {
	s.warnings = warnings
}

// publish runs one serialized initial-load or reload build and publishes the
// complete candidate with its next generation as a single atomic Store,
// carrying the configuration event in the same observation section as that
// Store. No publication mutex is held during the build; readers load the ready
// pointer without waiting, and the commit releases the build mutex before the
// enqueue.
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
	// Immediately before the atomic Store, Caller and owner cancellation are
	// checked once more under the Caller-first rule. A publication that wins
	// this check may finish despite later cancellation; shutdown joins it.
	if err := s.canceled(ctx); err != nil {
		fresh := s.freshResources(candidate)
		s.buildMu.Unlock()
		if fresh != nil {
			_ = fresh.release()
		}
		return configurationCapture{}, err
	}
	return s.commit(candidate), nil
}

// nextGeneration reserves the next publication generation; the Caller holds
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
// events; the Caller holds the build mutex. The candidate already carries the
// one credential capture its construction consumed, so the published revision
// and its credential observations are one frozen pair. The global warning
// groups follow the published candidate inside the one observation section, so
// a later publication can never interleave an earlier candidate's refresh and
// no nested publication exists. The section stores the candidate, releases the
// build mutex, and enqueues the configuration event plus one final
// runtime-scoped warning event exactly when any group changed — each changed
// group keeps its own revision increment.
func (s *configurationService) commit(candidate *configuration) configurationCapture {
	captured := configurationCapture{snapshot: candidate}
	previous := s.published.Load()
	s.obs.publish(func() []Event {
		warningChanged := false
		if s.warnings != nil {
			warningChanged = s.warnings.setGlobal(setupSource, setupWarnings(candidate, candidate.credentials)) || warningChanged
			warningChanged = s.warnings.setGlobal(catalogSource, catalogWarnings(candidate.catalogWarnings)) || warningChanged
		}
		var events []Event
		if candidate.resources != nil && (previous == nil || previous.resources != candidate.resources) {
			candidate.resources.runtime.obs = s.obs
			warningChanged = candidate.resources.commitWarnings() || warningChanged
			events = append(events, scopeEvent(protocol.ScopeOpened, candidate.resources.runtime.info))
		}
		revision := s.warnings.storeRevision()
		s.heldMu.Lock()
		s.published.Store(candidate)
		s.heldMu.Unlock()
		s.buildMu.Unlock()
		events = append(events, configurationChangedEvent(s.instance, candidate.generation))
		if warningChanged {
			events = append(events, warningChangedEvent(s.instance, runtimeEventScope(), revision))
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

// canceled applies the Caller-first cancellation rule: a done Caller context
// reports its own error even when the owner is done too; otherwise a done
// owner ends admission with ErrClosed, and a live Caller and owner pass.
func (s *configurationService) canceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.owner.Err() != nil {
		return ErrClosed
	}
	return nil
}

// build reads the supplied configuration document once and passes it to
// candidate construction. The source, not the owner, decides how the document
// is stored; a missing document is a configuration failure and is never
// created here.
func (s *configurationService) build(ctx context.Context, generation uint64) (*configuration, error) {
	configData, err := s.configuration.Read(ctx)
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
// connection candidate is fully validated before its Caller's first
// persistence and is never rebuilt after it. Every build takes its one
// credential capture over the candidate's full name union; the only extra
// observation is the mutation's pre-edit seed, which feeds edit decisions and
// never the published map.
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
	graph.defaultStrategy = s.strategy
	var built catalog.BuildResult
	if conn == nil {
		built, err = s.loader.AssembleCaptured(ctx, doc.Providers)
	} else {
		built, err = s.loader.LoadCapturedConnection(ctx, doc.Providers, conn.providerID, conn.transport, conn.discovered)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, configurationFailure(err)
	}
	secrets := resolveDeclaredSecrets(active, doc.Extensions)
	credentials, credentialNames, err := s.captureCredentials(ctx, capturedCredentialNames(built.Catalog, secrets), key)
	if err != nil {
		return nil, configurationFailure(err)
	}
	if conn == nil {
		built, err = s.loader.RefreshCaptured(ctx, doc.Providers, built, credentialValues(credentials))
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, configurationFailure(err)
		}
	}
	snapshot, err := newConfiguration(generation, doc, built, captureWorkspacePermissions(s.locations.WorkspacePolicyDir))
	if err != nil {
		return nil, configurationFailure(err)
	}
	snapshot.composition = graph
	snapshot.credentials = credentials
	snapshot.credentialNames = credentialNames
	snapshot.secrets = secrets
	snapshot.declaredBindings = captureDeclaredBindings(secrets, credentials)
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

// captureWorkspacePermissions enumerates the selected per-Workspace
// permission inventory during one configuration build and captures each
// present readable permissions.json's raw bytes keyed by its directory ID. An
// empty location is the absent Workspace policy level; a missing directory is
// the fresh-install absent case and a failed enumeration leaves the whole
// Workspace policy level absent for this revision; a missing or unreadable
// file inside the inventory is absent policy for that Workspace alone. A
// successfully read malformed file is captured unchanged — resolution
// determines validity from the raw bytes. No enumeration failure fails
// publication, and nothing here reads project records, meta.json, or locks.
func captureWorkspacePermissions(root string) map[string]json.RawMessage {
	if root == "" {
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

// configurationFailure classifies a complete candidate failure while
// preserving the source error inside the wrap; cancellation never reaches it
// and stays unwrapped.
func configurationFailure(err error) error {
	return fmt.Errorf("%w: %w", ErrConfiguration, err)
}
