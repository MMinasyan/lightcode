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

// Load reads the bundled catalog, user config, and discovery cache, then calls Build.
func (l *Loader) Load() (*Catalog, []Warning, error) {
	home, err := l.resolvedHome()
	if err != nil {
		return nil, nil, err
	}
	bundled, err := readBundledProviders(l.bundled)
	if err != nil {
		return nil, nil, fmt.Errorf("read bundled catalog: %w", err)
	}
	userRaw, warnings := readUserConfigProvidersAt(home, l.configPath)
	cache, attempts, cacheWarnings := ReadDiscoveryCache(home)
	warnings = append(warnings, cacheWarnings...)

	result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Cache: cache})
	candidates := DiscoveryRefreshCandidates(result.Catalog, attempts, time.Now().UTC())
	candidates = l.filterRefreshCandidates(candidates, result.Catalog)
	discoveryWarnings, discoveryChanged, _ := refreshDiscoveryCandidatesFor(home, l.configPath, candidates, result.Catalog)
	warnings = append(warnings, discoveryWarnings...)
	if discoveryChanged {
		cache, _, cacheWarnings = ReadDiscoveryCache(home)
		warnings = append(warnings, cacheWarnings...)
		result = Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Cache: cache})
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

func refreshDiscoveryCandidatesFor(home, configPath string, candidateIDs []string, cat *Catalog) ([]Warning, bool, []string) {
	var warnings []Warning
	var refreshed []string
	changed := false
	if cat == nil {
		return warnings, changed, refreshed
	}
	for _, providerID := range candidateIDs {
		refreshedProvider, providerWarnings := RefreshProviderDiscoveryWithConfigPath(context.Background(), home, configPath, cat, providerID)
		if len(providerWarnings) != 0 {
			warnings = append(warnings, providerWarnings...)
			continue
		}
		if refreshedProvider {
			changed = true
			refreshed = append(refreshed, providerID)
		}
	}
	return warnings, changed, refreshed
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
  "providers": {},
  "default_model": ""
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
		return map[string]any{}, nil
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
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, []byte(catalogEmptyConfigTemplate), 0o600)
}

func lightcodeDir(home string) string {
	return filepath.Join(home, ".lightcode")
}
