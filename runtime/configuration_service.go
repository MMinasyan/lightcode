package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
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
// Runtime-wide lock and start no plugin or other service work.
type configurationService struct {
	loader        *catalog.Loader
	configPath    string
	agentsPath    string
	plugins       []Plugin
	pluginIDs     map[string]bool
	capabilityIDs []string

	owner context.Context

	buildMu   sync.Mutex
	published atomic.Pointer[configuration]
}

// newConfigurationService binds the service to the composition's owned
// declarations, the Loader rooted at the home the Runtime resolved once, and
// the Runtime owner context. Neither DataDir nor ConfigPath relocates the
// home-based discovery cache or its locks and TTL records, and the service
// reads no dotenv: that belongs to Runtime startup, not to any build or
// reload.
func newConfigurationService(owner context.Context, c *composition, loader *catalog.Loader, configPath string) *configurationService {
	pluginIDs := make(map[string]bool, len(c.plugins))
	for _, p := range c.plugins {
		pluginIDs[p.ID] = true
	}
	return &configurationService{
		loader:        loader,
		configPath:    configPath,
		agentsPath:    agents.PathForConfig(configPath),
		plugins:       c.plugins,
		pluginIDs:     pluginIDs,
		capabilityIDs: c.capabilityIDs,
		owner:         owner,
	}
}

// current returns the latest published snapshot, or nil before the initial
// publication. Readers never take the build mutex.
func (s *configurationService) current() *configuration {
	return s.published.Load()
}

// publish runs one serialized initial-load or reload build and publishes the
// complete candidate with its next generation as a single atomic Store.
func (s *configurationService) publish(ctx context.Context) (*configuration, error) {
	if err := s.canceled(ctx); err != nil {
		return nil, err
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	// A cancellation observed while waiting is returned after the active
	// builder releases the mutex, without starting another build.
	if err := s.canceled(ctx); err != nil {
		return nil, err
	}
	generation := uint64(1)
	if snapshot := s.published.Load(); snapshot != nil {
		generation = snapshot.generation + 1
		if generation == 0 {
			return nil, fmt.Errorf("configuration publication generation exhausted: %w", ErrConfiguration)
		}
	}
	candidate, err := s.build(ctx, generation)
	if err != nil {
		return nil, err
	}
	// Immediately before the atomic Store, owner and caller cancellation are
	// checked once more. A publication that wins this check may finish despite
	// later cancellation; shutdown joins it.
	if err := s.canceled(ctx); err != nil {
		return nil, err
	}
	s.published.Store(candidate)
	return candidate, nil
}

// canceled applies the owner-first cancellation rule: owner cancellation ends
// admission and reports ErrClosed, and caller cancellation reports its own
// context error.
func (s *configurationService) canceled(ctx context.Context) error {
	if s.owner.Err() != nil {
		return ErrClosed
	}
	return ctx.Err()
}

// build reads and interprets exactly one input set: the main and agents bytes
// are captured once (preserving the first-run skeletons), the captured
// providers layer is delegated to Loader.LoadCaptured so provider assembly
// and every cost protection use that one read, sessions and agent definitions
// are parsed against the ordinary visible export IDs, and the plugin section
// is validated against the owned declarations. Nothing is published until the
// complete candidate survives all of it.
func (s *configurationService) build(ctx context.Context, generation uint64) (*configuration, error) {
	configData, err := captureConfiguredFile(s.configPath, mainConfigSkeleton)
	if err != nil {
		return nil, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	agentsData, err := captureConfiguredFile(s.agentsPath, agentsSkeleton)
	if err != nil {
		return nil, configurationFailure(fmt.Errorf("read agent definitions: %w", err))
	}
	doc, err := decodeCapturedConfig(configData)
	if err != nil {
		return nil, configurationFailure(err)
	}
	built, err := s.loader.LoadCaptured(ctx, doc.Providers)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, configurationFailure(err)
	}
	snapshot, err := newConfiguration(generation, doc, built, agentsData, s.capabilityIDs)
	if err != nil {
		return nil, configurationFailure(err)
	}
	if err := s.validatePlugins(ctx, doc.Plugins); err != nil {
		return nil, err
	}
	return snapshot, nil
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
