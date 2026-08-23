package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
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
	TransportFingerprint string                         `json:"transport_fingerprint,omitempty"`
	FetchedAt            time.Time                      `json:"fetched_at,omitempty"`
	AttemptedAt          time.Time                      `json:"attempted_at,omitempty"`
	Models               map[string]discoveryCacheModel `json:"models"`
}

// DiscoveryRecord is one transport-bound discovery result and its attempt
// timing. A record with an empty fingerprint is unbound and cannot contribute
// models or TTL state to a catalog.
type DiscoveryRecord struct {
	TransportFingerprint string
	FetchedAt            time.Time
	AttemptedAt          time.Time
	Models               map[string]DiscoveredModel
}

// BoundTo reports whether the record belongs to the configured transport.
func (r DiscoveryRecord) BoundTo(transport Transport) bool {
	if r.TransportFingerprint == "" {
		return false
	}
	fingerprint := transportFingerprint(transport)
	return fingerprint != "" && r.TransportFingerprint == fingerprint
}

// SameTransport reports whether two configured transports have the same
// catalog identity. Runtime credentials are not part of that identity; a
// provisional authorization header is excluded because callers pass the
// captured configured transport, not the provisional fetch copy.
func SameTransport(a, b Transport) bool {
	aFingerprint := transportFingerprint(a)
	bFingerprint := transportFingerprint(b)
	return aFingerprint != "" && aFingerprint == bFingerprint
}

// transportFingerprint returns the lowercase SHA-256 identity of a configured
// transport. An empty result means the options could not be represented as
// canonical JSON and is never a valid persisted fingerprint.
func transportFingerprint(transport Transport) string {
	options, err := canonicalJSONValue(transport.Options)
	if err != nil {
		return ""
	}
	if options == nil {
		options = map[string]any{}
	}
	canonicalOptions, err := json.Marshal(options)
	if err != nil {
		return ""
	}
	headers := map[string]string{}
	for name, value := range transport.Headers {
		digest := sha256.Sum256([]byte(value))
		headers[name] = hex.EncodeToString(digest[:])
	}
	payload := struct {
		BaseURL   string            `json:"base_url"`
		APIKeyEnv string            `json:"api_key_env"`
		Headers   map[string]string `json:"headers"`
		Options   json.RawMessage   `json:"options"`
	}{
		BaseURL:   transport.BaseURL,
		APIKeyEnv: transport.APIKeyEnv,
		Headers:   headers,
		Options:   canonicalOptions,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// canonicalJSONValue normalizes JSON-shaped option values without converting
// numbers through float64. encoding/json sorts object keys while preserving
// array order, so the returned value can be serialized deterministically.
func canonicalJSONValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string, bool:
		return typed, nil
	case json.Number:
		return canonicalNumber(string(typed))
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil, fmt.Errorf("non-finite option number")
		}
		return canonicalNumber(strconv.FormatFloat(typed, 'g', -1, 64))
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			canonical, err := canonicalJSONValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = canonical
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			canonical, err := canonicalJSONValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = canonical
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported option type %T", value)
	}
}

func canonicalNumber(value string) (json.Number, error) {
	normalized, err := normalizeDecimal(value)
	if err != nil {
		return "", err
	}
	return json.Number(normalized), nil
}

func normalizeDecimal(value string) (string, error) {
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("invalid option number %q", value)
	}
	start := 0
	negative := false
	if value[0] == '-' {
		negative = true
		start++
	}
	mantissa := value[start:]
	exponent := new(big.Int)
	if index := strings.IndexAny(mantissa, "eE"); index >= 0 {
		exponentText := mantissa[index+1:]
		if strings.HasPrefix(exponentText, "+") {
			exponentText = exponentText[1:]
		}
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return "", fmt.Errorf("invalid option number %q", value)
		}
		mantissa = mantissa[:index]
	}
	parts := strings.Split(mantissa, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return "", fmt.Errorf("invalid option number %q", value)
	}
	digits := parts[0]
	if len(parts) == 2 {
		digits += parts[1]
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("invalid option number %q", value)
		}
	}
	scale := new(big.Int).Set(exponent)
	if len(parts) == 2 {
		scale.Sub(scale, big.NewInt(int64(len(parts[1]))))
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	for strings.HasSuffix(digits, "0") {
		digits = strings.TrimSuffix(digits, "0")
		scale.Add(scale, big.NewInt(1))
	}
	sign := ""
	if negative {
		sign = "-"
	}
	if scale.Sign() == 0 {
		return sign + digits, nil
	}
	return sign + digits + "e" + scale.String(), nil
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

// ReadDiscoveryCache reads all on-disk discovery cache files. Legacy files
// without a transport fingerprint are readable but unbound: they contribute
// neither models nor attempt timing.
func ReadDiscoveryCache(home string) (map[string]DiscoveryRecord, []Warning) {
	records := map[string]DiscoveryRecord{}
	dir := discoveryCacheDir(home)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return records, nil
	}
	if err != nil {
		return records, []Warning{{Kind: "discovery_failure", Message: fmt.Sprintf("read discovery cache dir: %v", err)}}
	}
	var warnings []Warning
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := readDiscoveryCacheFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			warnings = append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("read discovery cache: %v", err)})
			continue
		}
		records[providerID] = record
	}
	return records, warnings
}

// DiscoveryAttemptRecent reports whether provider discovery had a real network
// attempt inside the cache TTL.
func DiscoveryAttemptRecent(home, providerID string, transport Transport, now time.Time) (bool, error) {
	if !safeProviderID(providerID) {
		return false, fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	record, err := readDiscoveryCacheFile(filepath.Join(discoveryCacheDir(home), providerID+".json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !record.BoundTo(transport) {
		return false, nil
	}
	return !discoveryAttemptDue(record.AttemptedAt, now), nil
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

// discoveryLockAcquiredHook fires after the per-provider discovery lock is
// acquired, exactly once per acquisition, with the acquired lock path.
// Production no-op; tests record acquisitions to prove every discovery cache
// writer serializes on the same per-provider lock. Follows the snapshot
// mintPublishHook precedent.
var discoveryLockAcquiredHook = func(lockPath string) {}

// withDiscoveryLock runs fn while holding the per-provider discovery lock for
// providerID. It is the single locking helper for every discovery cache
// writer, so all writers for one provider share one lock path.
func withDiscoveryLock(home, providerID string, fn func() error) error {
	return atomicfs.WithLock(discoveryLockPath(home, providerID), func() error {
		discoveryLockAcquiredHook(discoveryLockPath(home, providerID))
		return fn()
	})
}

// WriteDiscoveryAttempt records a real discovery network attempt without
// changing any cached model metadata. The whole read-modify-write runs under
// the per-provider discovery lock, so a stale attempt can never clobber a
// discovery published concurrently by another refresh.
func WriteDiscoveryAttempt(home, providerID string, transport Transport, attemptedAt time.Time) error {
	if !safeProviderID(providerID) {
		return fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	fingerprint := transportFingerprint(transport)
	if fingerprint == "" {
		return fmt.Errorf("compute discovery transport fingerprint")
	}
	if attemptedAt.IsZero() {
		attemptedAt = time.Now().UTC()
	}
	path := filepath.Join(discoveryCacheDir(home), providerID+".json")
	return withDiscoveryLock(home, providerID, func() error {
		return writeDiscoveryAttemptPayload(path, fingerprint, attemptedAt)
	})
}

// TryWriteDiscoveryAttempt is the one-attempt counterpart of
// WriteDiscoveryAttempt for owner paths that must not block a shutting-down
// owner on a foreign discovery-lock holder. On contention it returns
// (false, nil) without touching the cache file; on success it returns
// (true, the payload result), so a payload failure is distinguishable from
// contention.
func TryWriteDiscoveryAttempt(home, providerID string, transport Transport, attemptedAt time.Time) (bool, error) {
	if !safeProviderID(providerID) {
		return false, fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	fingerprint := transportFingerprint(transport)
	if fingerprint == "" {
		return false, fmt.Errorf("compute discovery transport fingerprint")
	}
	if attemptedAt.IsZero() {
		attemptedAt = time.Now().UTC()
	}
	path := filepath.Join(discoveryCacheDir(home), providerID+".json")
	return atomicfs.TryWithLock(discoveryLockPath(home, providerID), func() error {
		discoveryLockAcquiredHook(discoveryLockPath(home, providerID))
		return writeDiscoveryAttemptPayload(path, fingerprint, attemptedAt)
	})
}

// writeDiscoveryAttemptPayload stamps the attempted timestamp onto one
// provider's cache file, preserving any cached models. It runs under the
// per-provider discovery lock held by the caller (the blocking
// withDiscoveryLock or the one-attempt TryWithLock), so it never reacquires
// the leaf.
func writeDiscoveryAttemptPayload(path, fingerprint string, attemptedAt time.Time) error {
	raw := discoveryCacheFile{TransportFingerprint: fingerprint, Models: map[string]discoveryCacheModel{}}
	data, err := os.ReadFile(path)
	if err == nil {
		var existing discoveryCacheFile
		if err := json.Unmarshal(data, &existing); err != nil {
			return err
		}
		if existing.TransportFingerprint == fingerprint && validTransportFingerprint(existing.TransportFingerprint) {
			raw.FetchedAt = existing.FetchedAt
			raw.Models = existing.Models
			if raw.Models == nil {
				raw.Models = map[string]discoveryCacheModel{}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw.AttemptedAt = attemptedAt.UTC()
	return writeDiscoveryCacheFile(path, raw)
}

// WriteDiscoveryCache writes one provider's discovery cache file. It builds
// its payload fresh from the discovery result and publishes it under the
// per-provider discovery lock, so it never interleaves with the attempt
// writer's read-modify-write or another whole-file write for the same provider.
func WriteDiscoveryCache(home, providerID string, transport Transport, discovered DiscoveredProvider, fetchedAt time.Time) error {
	if !safeProviderID(providerID) {
		return fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	fingerprint := transportFingerprint(transport)
	if fingerprint == "" {
		return fmt.Errorf("compute discovery transport fingerprint")
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	path := filepath.Join(discoveryCacheDir(home), providerID+".json")
	return withDiscoveryLock(home, providerID, func() error {
		return writeDiscoveryCachePayload(path, fingerprint, discovered, fetchedAt)
	})
}

// TryWriteDiscoveryCache is the one-attempt counterpart of
// WriteDiscoveryCache for owner paths that must not block a shutting-down
// owner on a foreign discovery-lock holder. On contention it returns
// (false, nil) without touching the cache file; on success it returns
// (true, the payload result), so a payload failure is distinguishable from
// contention.
func TryWriteDiscoveryCache(home, providerID string, transport Transport, discovered DiscoveredProvider, fetchedAt time.Time) (bool, error) {
	if !safeProviderID(providerID) {
		return false, fmt.Errorf("%w: provider id %q", ErrInvalidModelRef, providerID)
	}
	fingerprint := transportFingerprint(transport)
	if fingerprint == "" {
		return false, fmt.Errorf("compute discovery transport fingerprint")
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	path := filepath.Join(discoveryCacheDir(home), providerID+".json")
	return atomicfs.TryWithLock(discoveryLockPath(home, providerID), func() error {
		discoveryLockAcquiredHook(discoveryLockPath(home, providerID))
		return writeDiscoveryCachePayload(path, fingerprint, discovered, fetchedAt)
	})
}

// writeDiscoveryCachePayload publishes one provider's discovery cache file
// fresh from the discovery result. It runs under the per-provider discovery
// lock held by the caller (the blocking withDiscoveryLock or the one-attempt
// TryWithLock), so it never reacquires the leaf.
func writeDiscoveryCachePayload(path, fingerprint string, discovered DiscoveredProvider, fetchedAt time.Time) error {
	raw := discoveryCacheFile{TransportFingerprint: fingerprint, FetchedAt: fetchedAt.UTC(), AttemptedAt: fetchedAt.UTC(), Models: map[string]discoveryCacheModel{}}
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
	return writeDiscoveryCacheFile(path, raw)
}

func readDiscoveryCacheFile(path string) (DiscoveryRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DiscoveryRecord{}, err
	}
	var raw discoveryCacheFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return DiscoveryRecord{}, err
	}
	if !validTransportFingerprint(raw.TransportFingerprint) {
		return DiscoveryRecord{}, nil
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
	return DiscoveryRecord{
		TransportFingerprint: raw.TransportFingerprint,
		FetchedAt:            raw.FetchedAt,
		AttemptedAt:          attemptedAt,
		Models:               models,
	}, nil
}

func validTransportFingerprint(fingerprint string) bool {
	if len(fingerprint) != sha256.Size*2 {
		return false
	}
	for _, char := range fingerprint {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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

// discoveryLockPath is the per-provider sidecar lock that serializes all
// writers of one provider's discovery cache file. It lives in a .locks
// directory inside the cache dir and is never the provider's own cache file.
func discoveryLockPath(home, providerID string) string {
	return filepath.Join(discoveryCacheDir(home), ".locks", providerID+".lock")
}

func discoveryCacheDir(home string) string {
	return filepath.Join(lightcodeDir(home), "cache", "discovery")
}
