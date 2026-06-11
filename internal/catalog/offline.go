package catalog

import "fmt"

// BuildOffline assembles the effective catalog without any filesystem write
// or network access: bundled providers come from the embedded FS, the
// discovery cache is read as-is (no refresh), and userRaw is caller-supplied
// — callers must obtain it without auto-creating the config file. Cache-read
// warnings are concatenated into the result's Warnings the way Loader.Load
// surfaces them, so a corrupt cache is visible to read-only callers.
func BuildOffline(home string, userRaw map[string]any) (BuildResult, error) {
	bundled, err := readBundledProviders(bundledFS)
	if err != nil {
		return BuildResult{}, fmt.Errorf("read bundled catalog: %w", err)
	}
	cache, _, cacheWarnings := ReadDiscoveryCache(home)
	result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Cache: cache})
	result.Warnings = append(cacheWarnings, result.Warnings...)
	return result, nil
}

// ProviderConnected reports whether a provider is connected: it has at least
// one usable model (ContextWindow > 0) and its credential resolves — a
// non-empty api_key_env must satisfy envIsSet; a keyless provider needs a
// non-empty base_url. The env lookup is injected so the catalog stays
// env-neutral and callers own the connection decision.
func ProviderConnected(prov *Provider, envIsSet func(string) bool) bool {
	if prov == nil {
		return false
	}
	usable := false
	for _, model := range prov.Models {
		if model != nil && model.ContextWindow > 0 {
			usable = true
			break
		}
	}
	if !usable {
		return false
	}
	if prov.Transport.APIKeyEnv != "" {
		return envIsSet != nil && envIsSet(prov.Transport.APIKeyEnv)
	}
	return prov.Transport.BaseURL != ""
}
