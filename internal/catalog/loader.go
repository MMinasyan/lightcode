package catalog

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// bundledFS contains Lightcode's hand-curated built-in provider catalog files.
//
//go:embed builtin
var bundledFS embed.FS

// Loader reads catalog inputs from disk and delegates assembly to Build.
type Loader struct {
	home         string
	configPath   string
	bundled      fs.FS
	AllowRefresh func(providerID string, provider *Provider) bool
}

// NewLoader constructs a catalog loader rooted at the user's home directory.
func NewLoader(home string, bundled fs.FS) *Loader {
	if bundled == nil {
		bundled = bundledFS
	}
	return &Loader{home: home, bundled: bundled}
}

// NewLoaderWithConfigPath constructs a catalog loader that reads user provider
// overlays from the same config file the agent loaded at startup.
func NewLoaderWithConfigPath(home string, bundled fs.FS, configPath string) *Loader {
	loader := NewLoader(home, bundled)
	loader.configPath = configPath
	return loader
}

// Load reads the bundled catalog, user config, and discovery cache, then calls
// Build. It is the blocking entry used by pre-owner startup, where discovery
// publication may block on the per-provider discovery lock.
func (l *Loader) Load() (*Catalog, []Warning, error) {
	return l.loadReadInputs(false)
}

// LoadTry is Load with every discovery publication routed through the
// one-attempt Try writers, so a foreign discovery-lock holder yields the
// existing discovery_failure warning instead of hanging the owner shutdown.
func (l *Loader) LoadTry() (*Catalog, []Warning, error) {
	return l.loadReadInputs(true)
}

// LoadCaptured is the captured-input entry: the user provider layer arrives
// as the caller's already-read map and the main configuration is never read.
// It preserves the bundled source and the AllowRefresh filter, routes every
// discovery publication through the one-attempt Try writers, and honors
// cancellation on ctx: a cancellation observed after a provider fetch and
// before the start of a cache writer skips that write and returns the context
// error instead of a discovery warning. Once a cache write has started it
// keeps the existing atomicfs semantics and may finish despite the later
// cancellation. Ordinary discovery and cache failures keep their existing
// warnings and the cached input.
func (l *Loader) LoadCaptured(ctx context.Context, userRaw map[string]any) (BuildResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	catalog, warnings, err := l.loadCaptured(ctx, userRaw)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Catalog: catalog, Warnings: warnings}, nil
}

func (l *Loader) loadReadInputs(try bool) (*Catalog, []Warning, error) {
	home, err := l.resolvedHome()
	if err != nil {
		return nil, nil, err
	}
	bundled, err := readBundledProviders(l.bundled)
	if err != nil {
		return nil, nil, fmt.Errorf("read bundled catalog: %w", err)
	}
	userRaw, warnings := readUserConfigProvidersAt(home, l.configPath)
	return l.assemble(context.Background(), home, bundled, userRaw, try, warnings)
}

func (l *Loader) loadCaptured(ctx context.Context, userRaw map[string]any) (*Catalog, []Warning, error) {
	home, err := l.resolvedHome()
	if err != nil {
		return nil, nil, err
	}
	bundled, err := readBundledProviders(l.bundled)
	if err != nil {
		return nil, nil, fmt.Errorf("read bundled catalog: %w", err)
	}
	return l.assemble(ctx, home, bundled, userRaw, true, nil)
}

// assemble is the shared cache-read/build/refresh/rebuild body behind every
// Loader entry. It builds the effective catalog over the bundled, already-
// read user, and cached discovery inputs, refreshes each filtered due provider
// in sorted order (try selects the one-attempt publications), and rebuilds
// over freshly read records once a publication changed the cache. userRaw is
// the already-read user layer whose declared cost fields the refresh
// publications retain, so no input is reread mid-build.
func (l *Loader) assemble(ctx context.Context, home string, bundled map[string]json.RawMessage, userRaw map[string]any, try bool, warnings []Warning) (*Catalog, []Warning, error) {
	records, cacheWarnings := ReadDiscoveryCache(home)
	warnings = append(warnings, cacheWarnings...)

	result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Records: records})
	candidates := DiscoveryRefreshCandidates(result.Catalog, records, time.Now().UTC())
	candidates = l.filterRefreshCandidates(candidates, result.Catalog)
	discoveryWarnings, discoveryChanged, err := refreshDiscoveryCandidates(ctx, home, candidates, result.Catalog, try, userRaw)
	warnings = append(warnings, discoveryWarnings...)
	if err != nil {
		return nil, warnings, err
	}
	if discoveryChanged {
		records, cacheWarnings = ReadDiscoveryCache(home)
		warnings = append(warnings, cacheWarnings...)
		result = Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Records: records})
	}
	warnings = append(warnings, result.Warnings...)
	return result.Catalog, warnings, nil
}

func (l *Loader) filterRefreshCandidates(candidateIDs []string, cat *Catalog) []string {
	if l == nil || l.AllowRefresh == nil || cat == nil || len(candidateIDs) == 0 {
		return candidateIDs
	}
	filtered := candidateIDs[:0]
	for _, providerID := range candidateIDs {
		if l.AllowRefresh(providerID, cat.Providers[providerID]) {
			filtered = append(filtered, providerID)
		}
	}
	return filtered
}

// refreshDiscoveryCandidates is the loader's sorted refresh loop over the
// filtered candidates: per provider it performs the fetch, checks ctx
// cancellation once the network attempt began and before any cache writer
// starts, and then publishes through the shared per-provider code with the
// already-read userRaw as the cost-protection source. A cancellation observed
// between the fetch and the publication skips that write and returns the
// context error with the warnings collected so far; it is not a discovery
// warning.
func refreshDiscoveryCandidates(ctx context.Context, home string, candidateIDs []string, cat *Catalog, try bool, userRaw map[string]any) ([]Warning, bool, error) {
	var warnings []Warning
	changed := false
	if cat == nil {
		return warnings, changed, nil
	}
	for _, providerID := range candidateIDs {
		provider := catalogProvider(cat, providerID)
		if provider == nil {
			warnings = append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("unknown provider %q", providerID)})
			continue
		}
		attempted, discovered, providerWarnings := FetchDiscoveryIfDue(ctx, home, provider, time.Now().UTC())
		if !attempted {
			warnings = append(warnings, providerWarnings...)
			continue
		}
		if err := ctx.Err(); err != nil {
			return warnings, changed, err
		}
		refreshed, providerWarnings := publishDiscoveryResult(home, provider, providerID, discovered, providerWarnings, try, cat, func() map[string]map[string]bool {
			return userCostProtection(userRaw, providerID)
		})
		if len(providerWarnings) != 0 {
			warnings = append(warnings, providerWarnings...)
			continue
		}
		if refreshed {
			changed = true
		}
	}
	return warnings, changed, nil
}

func (l *Loader) resolvedHome() (string, error) {
	if l != nil && l.home != "" {
		return l.home, nil
	}
	return os.UserHomeDir()
}

func readBundledProviders(fsys fs.FS) (map[string]json.RawMessage, error) {
	providers := map[string]json.RawMessage{}
	err := fs.WalkDir(fsys, "builtin", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path.Ext(file) != ".json" {
			return nil
		}
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		providerID := strings.TrimSuffix(path.Base(file), ".json")
		providers[providerID] = append(json.RawMessage(nil), data...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return providers, nil
}

const catalogEmptyConfigTemplate = `{
  "providers": {}
}
`

func readUserConfigProviders(home string) (map[string]any, []Warning) {
	return readUserConfigProvidersAt(home, "")
}

func readUserConfigProvidersAt(home, configPath string) (map[string]any, []Warning) {
	if configPath == "" {
		configPath = filepath.Join(lightcodeDir(home), "config.json")
	}
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		if writeErr := writeEmptyCatalogConfig(configPath); writeErr != nil {
			return map[string]any{}, []Warning{{Kind: "user_config_skip", Message: fmt.Sprintf("create empty config: %v", writeErr)}}
		}
		// The file now exists — either the empty one just created, or one a
		// concurrent creator won. Re-read so a losing creator still observes
		// the winner's providers instead of an empty map.
		data, err = os.ReadFile(configPath)
	}
	if err != nil {
		return map[string]any{}, []Warning{{Kind: "user_config_skip", Message: fmt.Sprintf("read config: %v", err)}}
	}

	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return map[string]any{}, []Warning{{Kind: "user_config_skip", Message: fmt.Sprintf("parse config: %v", err)}}
	}
	providersValue, exists := root["providers"]
	if !exists || providersValue == nil {
		return map[string]any{}, nil
	}
	providers, ok := providersValue.(map[string]any)
	if !ok {
		return map[string]any{}, []Warning{{Kind: "user_config_skip", Message: "providers must be an object"}}
	}
	return cloneJSONValue(providers).(map[string]any), nil
}
func writeEmptyCatalogConfig(configPath string) error {
	if _, err := atomicfs.CreateExclusive(configPath, []byte(catalogEmptyConfigTemplate), 0o600); err != nil {
		return err
	}
	return nil
}

func lightcodeDir(home string) string {
	return filepath.Join(home, ".lightcode")
}
