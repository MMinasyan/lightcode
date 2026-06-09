package catalog

import "encoding/json"

// BundledProviderHeaders returns the transport headers a bundled provider ships
// with (e.g. OpenRouter's attribution HTTP-Referer / X-Title). These are part of
// the built-in's identity and must not be user-editable.
func BundledProviderHeaders(providerID string) map[string]string {
	bundled, err := readBundledProviders(bundledFS)
	if err != nil {
		return nil
	}
	raw, ok := bundled[providerID]
	if !ok {
		return nil
	}
	var decoded struct {
		Transport struct {
			Headers map[string]string `json:"headers"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded.Transport.Headers
}

// BundledModelIDs returns, for each bundled built-in provider, the set of model
// IDs declared in its embedded catalog file. It lets callers classify a model's
// provenance (bundled vs discovered vs user-added) without re-reading the merged
// catalog, which has already collapsed the layers together.
func BundledModelIDs() map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	bundled, err := readBundledProviders(bundledFS)
	if err != nil {
		return out
	}
	for providerID, raw := range bundled {
		var decoded struct {
			Models map[string]json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		ids := make(map[string]struct{}, len(decoded.Models))
		for modelID := range decoded.Models {
			ids[modelID] = struct{}{}
		}
		out[providerID] = ids
	}
	return out
}
