package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

const discoveryCacheTTL = 24 * time.Hour

// RawDiscoveredModel is one provider-supplied model object from /v1/models.
type RawDiscoveredModel map[string]any

type discoveryResponse struct {
	Data []RawDiscoveredModel `json:"data"`
}

type discoveryCacheFile struct {
	FetchedAt   time.Time                      `json:"fetched_at,omitempty"`
	AttemptedAt time.Time                      `json:"attempted_at,omitempty"`
	Models      map[string]discoveryCacheModel `json:"models"`
}

type discoveryCacheModel struct {
	ID              string                  `json:"id,omitempty"`
	Name            string                  `json:"name,omitempty"`
	ContextWindow   int                     `json:"context_window,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
	Cost            *Cost                   `json:"cost,omitempty"`
	Metadata        *discoveryModelMetadata `json:"metadata,omitempty"`
}

// FetchDiscovery fetches and parses a provider's OpenAI-compatible /models list.
func FetchDiscovery(ctx context.Context, client *http.Client, provider *Provider) (DiscoveredProvider, error) {
	if provider == nil {
		return DiscoveredProvider{}, fmt.Errorf("discovery provider is nil")
	}
	modelsURL, err := discoveryModelsURL(provider.Transport.BaseURL)
	if err != nil {
		return DiscoveredProvider{}, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return DiscoveredProvider{}, err
	}
	for key, value := range provider.Transport.Headers {
		req.Header.Set(key, value)
	}
	if provider.Transport.APIKeyEnv != "" {
		if apiKey := os.Getenv(provider.Transport.APIKeyEnv); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return DiscoveredProvider{}, fmt.Errorf("fetch discovery for %s: %w", provider.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return DiscoveredProvider{}, fmt.Errorf("auth failed for discovery on %s: HTTP %d", provider.ID, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DiscoveredProvider{}, fmt.Errorf("discovery for %s returned HTTP %d", provider.ID, resp.StatusCode)
	}

	var parsed discoveryResponse
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return DiscoveredProvider{}, fmt.Errorf("parse discovery response for %s: %w", provider.ID, err)
	}
	if len(parsed.Data) == 0 {
		return DiscoveredProvider{}, fmt.Errorf("discovery for %s returned no models", provider.ID)
	}

	models := map[string]DiscoveredModel{}
	for _, raw := range parsed.Data {
		modelID, model := parseDiscoveredModel(raw)
		if modelID == "" {
			continue
		}
		models[modelID] = model
	}
	if len(models) == 0 {
		return DiscoveredProvider{}, fmt.Errorf("discovery for %s returned no usable model IDs", provider.ID)
	}
	return DiscoveredProvider{Models: models}, nil
}

func discoveryModelsURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("discovery base_url must be an absolute URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/models"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func parseDiscoveredModel(raw RawDiscoveredModel) (string, DiscoveredModel) {
	modelID, _ := raw["id"].(string)
	tp, _ := rawObject(raw["top_provider"])
	model := DiscoveredModel{
		Name:          stringField(raw, "name"),
		ContextWindow: firstPositiveInt(raw, "context_length", "context_window", "max_context_length"),
		MaxOutputTokens: firstPositiveIntMulti(
			[]map[string]any{tp, raw},
			"max_completion_tokens", "max_output_tokens",
		),
		Cost:     extractCostIfPresent(raw),
		metadata: extractDiscoveryMetadata(raw),
	}
	return modelID, model
}

func extractDiscoveryMetadata(raw RawDiscoveredModel) *discoveryModelMetadata {
	arch, _ := rawObject(raw["architecture"])
	meta := &discoveryModelMetadata{
		Type:                         normalizeDiscoveryToken(stringField(raw, "type")),
		Task:                         normalizeDiscoveryToken(stringField(raw, "task")),
		InputModalities:              normalizedDiscoveryStringList(raw["input_modalities"]),
		OutputModalities:             normalizedDiscoveryStringList(raw["output_modalities"]),
		Modalities:                   normalizedDiscoveryStringList(raw["modalities"]),
		ArchitectureInputModalities:  normalizedDiscoveryStringList(arch["input_modalities"]),
		ArchitectureOutputModalities: normalizedDiscoveryStringList(arch["output_modalities"]),
		ArchitectureModality:         normalizeDiscoveryModality(stringField(arch, "modality")),
		Capabilities:                 normalizedDiscoveryCapabilities(raw["capabilities"]),
		SupportedParameters:          normalizedDiscoveryStringList(raw["supported_parameters"]),
	}
	if discoveryMetadataEmpty(meta) {
		return nil
	}
	return meta
}

func normalizedDiscoveryStringList(value any) []string {
	var raw []string
	switch v := value.(type) {
	case string:
		raw = append(raw, v)
	case []string:
		raw = append(raw, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	return normalizeDiscoveryTokens(raw)
}

func normalizedDiscoveryCapabilities(value any) map[string]bool {
	out := map[string]bool{}
	switch v := value.(type) {
	case string:
		if key := normalizeDiscoveryToken(v); key != "" {
			out[key] = true
		}
	case []string:
		for _, item := range v {
			if key := normalizeDiscoveryToken(item); key != "" {
				out[key] = true
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if key := normalizeDiscoveryToken(s); key != "" {
					out[key] = true
				}
			}
		}
	case map[string]bool:
		for key, enabled := range v {
			if normalized := normalizeDiscoveryToken(key); normalized != "" {
				out[normalized] = enabled
			}
		}
	case map[string]any:
		for key, rawEnabled := range v {
			enabled, ok := rawEnabled.(bool)
			if !ok {
				continue
			}
			if normalized := normalizeDiscoveryToken(key); normalized != "" {
				out[normalized] = enabled
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func discoveryMetadataEmpty(meta *discoveryModelMetadata) bool {
	if meta == nil {
		return true
	}
	return meta.Type == "" &&
		meta.Task == "" &&
		len(meta.InputModalities) == 0 &&
		len(meta.OutputModalities) == 0 &&
		len(meta.Modalities) == 0 &&
		len(meta.ArchitectureInputModalities) == 0 &&
		len(meta.ArchitectureOutputModalities) == 0 &&
		meta.ArchitectureModality == "" &&
		len(meta.Capabilities) == 0 &&
		len(meta.SupportedParameters) == 0
}

func stringField(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}

func firstPositiveInt(raw map[string]any, keys ...string) int {
	return firstPositiveIntMulti([]map[string]any{raw}, keys...)
}

func firstPositiveIntMulti(raws []map[string]any, keys ...string) int {
	for _, raw := range raws {
		if raw == nil {
			continue
		}
		for _, key := range keys {
			if value, ok := positiveInt(raw[key]); ok {
				return value
			}
		}
	}
	return 0
}

func positiveInt(value any) (int, bool) {
	if i, ok := validationInt(value); ok && i > 0 {
		return i, true
	}
	if s, ok := value.(string); ok {
		i, err := strconv.Atoi(s)
		if err == nil && i > 0 {
			return i, true
		}
	}
	return 0, false
}

func extractCostIfPresent(raw RawDiscoveredModel) *Cost {
	if costRaw, ok := rawObject(raw["cost"]); ok {
		return costFromRaw(costRaw, 1)
	}
	if pricingRaw, ok := rawObject(raw["pricing"]); ok {
		return costFromPricing(pricingRaw)
	}
	return nil
}

func costFromPricing(raw map[string]any) *Cost {
	cost := &Cost{}
	seen := false
	if value, ok := discoveryNumber(raw["prompt"]); ok {
		v := value * 1_000_000
		cost.Input = &v
		seen = true
	} else if value, ok := discoveryNumber(raw["input"]); ok {
		v := value * 1_000_000
		cost.Input = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["completion"]); ok {
		v := value * 1_000_000
		cost.Output = &v
		seen = true
	} else if value, ok := discoveryNumber(raw["output"]); ok {
		v := value * 1_000_000
		cost.Output = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["input_cache_read"]); ok {
		v := value * 1_000_000
		cost.CacheRead = &v
		seen = true
	} else if value, ok := discoveryNumber(raw["cache_read"]); ok {
		v := value * 1_000_000
		cost.CacheRead = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["input_cache_write"]); ok {
		v := value * 1_000_000
		cost.CacheWrite = &v
		seen = true
	} else if value, ok := discoveryNumber(raw["cache_write"]); ok {
		v := value * 1_000_000
		cost.CacheWrite = &v
		seen = true
	}
	if !seen {
		return nil
	}
	return cost
}

func costFromRaw(raw map[string]any, multiplier float64) *Cost {
	cost := &Cost{}
	seen := false
	if value, ok := discoveryNumber(raw["input"]); ok {
		v := value * multiplier
		cost.Input = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["output"]); ok {
		v := value * multiplier
		cost.Output = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["cache_read"]); ok {
		v := value * multiplier
		cost.CacheRead = &v
		seen = true
	}
	if value, ok := discoveryNumber(raw["cache_write"]); ok {
		v := value * multiplier
		cost.CacheWrite = &v
		seen = true
	}
	if !seen {
		return nil
	}
	return cost
}

func discoveryNumber(value any) (float64, bool) {
	if f, ok := validationNumber(value); ok {
		return f, true
	}
	if s, ok := value.(string); ok {
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func rawObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// ReadDiscoveryCache reads all on-disk discovery cache files and last-attempt
// timestamps. Old cache files without attempted_at use fetched_at as the
// attempt time for backwards compatibility.
func ReadDiscoveryCache(home string) (map[string]DiscoveredProvider, map[string]time.Time, []Warning) {
	cache := map[string]DiscoveredProvider{}
	attempts := map[string]time.Time{}
	dir := discoveryCacheDir(home)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return cache, attempts, nil
	}
	if err != nil {
		return cache, attempts, []Warning{{Kind: "discovery_failure", Message: fmt.Sprintf("read discovery cache dir: %v", err)}}
	}
	var warnings []Warning
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		fileCache, attemptedAt, err := readDiscoveryCacheFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			warnings = append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("read discovery cache: %v", err)})
			continue
		}
		cache[providerID] = fileCache
		if !attemptedAt.IsZero() {
			attempts[providerID] = attemptedAt
		}
	}
	return cache, attempts, warnings
}

// DiscoveryAttemptRecent reports whether provider discovery had a real network
// attempt inside the cache TTL.
func DiscoveryAttemptRecent(home, providerID string, now time.Time) (bool, error) {
	if !safeProviderID(providerID) {
		return false, fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, attemptedAt, err := readDiscoveryCacheFile(filepath.Join(discoveryCacheDir(home), providerID+".json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !discoveryAttemptDue(attemptedAt, now), nil
}

func discoveryAttemptDue(attemptedAt, now time.Time) bool {
	if attemptedAt.IsZero() {
		return true
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(attemptedAt) >= discoveryCacheTTL
}

// WriteDiscoveryAttempt records a real discovery network attempt without
// changing any cached model metadata.
func WriteDiscoveryAttempt(home, providerID string, attemptedAt time.Time) error {
	if !safeProviderID(providerID) {
		return fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	if attemptedAt.IsZero() {
		attemptedAt = time.Now().UTC()
	}
	path := filepath.Join(discoveryCacheDir(home), providerID+".json")
	raw := discoveryCacheFile{Models: map[string]discoveryCacheModel{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		if raw.Models == nil {
			raw.Models = map[string]discoveryCacheModel{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw.AttemptedAt = attemptedAt.UTC()
	return writeDiscoveryCacheFile(path, raw)
}

// WriteDiscoveryCache writes one provider's discovery cache file.
func WriteDiscoveryCache(home, providerID string, discovered DiscoveredProvider, fetchedAt time.Time) error {
	if !safeProviderID(providerID) {
		return fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	raw := discoveryCacheFile{FetchedAt: fetchedAt.UTC(), AttemptedAt: fetchedAt.UTC(), Models: map[string]discoveryCacheModel{}}
	for modelID, model := range discovered.Models {
		raw.Models[modelID] = discoveryCacheModel{
			ID:              modelID,
			Name:            model.Name,
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
			Cost:            model.Cost,
			Metadata:        model.metadata,
		}
	}
	return writeDiscoveryCacheFile(filepath.Join(discoveryCacheDir(home), providerID+".json"), raw)
}

func readDiscoveryCacheFile(path string) (DiscoveredProvider, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DiscoveredProvider{}, time.Time{}, err
	}
	var raw discoveryCacheFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return DiscoveredProvider{}, time.Time{}, err
	}
	attemptedAt := raw.AttemptedAt
	if attemptedAt.IsZero() {
		attemptedAt = raw.FetchedAt
	}
	models := map[string]DiscoveredModel{}
	for modelID, model := range raw.Models {
		models[modelID] = DiscoveredModel{
			Name:            model.Name,
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
			Cost:            model.Cost,
			metadata:        model.Metadata,
		}
	}
	return DiscoveredProvider{Models: models}, attemptedAt, nil
}

func writeDiscoveryCacheFile(path string, raw discoveryCacheFile) error {
	if raw.Models == nil {
		raw.Models = map[string]discoveryCacheModel{}
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfs.Write(path, data, 0o600)
}

func safeProviderID(providerID string) bool {
	return providerID != "" && !strings.Contains(providerID, "/") && !strings.Contains(providerID, `\`)
}

func discoveryCacheDir(home string) string {
	return filepath.Join(lightcodeDir(home), "cache", "discovery")
}
