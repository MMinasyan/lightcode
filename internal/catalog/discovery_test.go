package catalog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestFetchDiscoveryParsesModelsAndSendsHeaders(t *testing.T) {
	t.Setenv("TEST_DISCOVERY_API_KEY", "secret-token")
	seenRequest := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequest = true
		if r.URL.Path != "/v1/models" {
			t.Fatalf("discovery path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization header = %q", got)
		}
		if got := r.Header.Get("X-Test-Header"); got != "yes" {
			t.Fatalf("X-Test-Header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "rich",
					"name": "Rich Model",
					"context_length": 128000,
					"max_completion_tokens": 16384,
					"pricing": {
						"prompt": "0.00000015",
						"completion": "0.00000060",
						"input_cache_read": "0.00000003",
						"input_cache_write": "0.00000020"
					}
				},
				{
					"id": "fallback",
					"context_window": 32000,
					"max_output_tokens": 4096
				}
			]
		}`))
	}))
	defer server.Close()

	provider := &Provider{
		ID: "test",
		Transport: Transport{
			BaseURL:   server.URL + "/v1",
			APIKeyEnv: "TEST_DISCOVERY_API_KEY",
			Headers:   map[string]string{"X-Test-Header": "yes"},
		},
	}
	discovered, err := FetchDiscovery(context.Background(), server.Client(), provider)
	if err != nil {
		t.Fatalf("FetchDiscovery returned error: %v", err)
	}
	if !seenRequest {
		t.Fatalf("test server was not called")
	}
	rich := discovered.Models["rich"]
	if rich.Name != "Rich Model" || rich.ContextWindow != 128000 || rich.MaxOutputTokens != 16384 {
		t.Fatalf("rich model = %#v", rich)
	}
	if rich.Cost == nil || rich.Cost.Input == nil || rich.Cost.Output == nil {
		t.Fatalf("rich cost = %#v, want input/output", rich.Cost)
	}
	if rich.Cost.CacheRead == nil || rich.Cost.CacheWrite == nil {
		t.Fatalf("rich cost = %#v, want cache read/write", rich.Cost)
	}
	if !floatNear(*rich.Cost.Input, 0.15) || !floatNear(*rich.Cost.Output, 0.60) || !floatNear(*rich.Cost.CacheRead, 0.03) || !floatNear(*rich.Cost.CacheWrite, 0.20) {
		t.Fatalf("rich cost = %#v, want 0.15/0.60/0.03/0.20 per million", rich.Cost)
	}
	fallback := discovered.Models["fallback"]
	if fallback.ContextWindow != 32000 || fallback.MaxOutputTokens != 4096 {
		t.Fatalf("fallback model = %#v", fallback)
	}
}

func TestFetchDiscoveryReturnsBoundedFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantError  string
	}{
		{name: "auth", statusCode: http.StatusUnauthorized, body: `{}`, wantError: "auth failed for discovery on test"},
		{name: "malformed_json", statusCode: http.StatusOK, body: `{not json`, wantError: "parse discovery response"},
		{name: "empty_data", statusCode: http.StatusOK, body: `{"data": []}`, wantError: "returned no models"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := FetchDiscovery(context.Background(), server.Client(), &Provider{ID: "test", Transport: Transport{BaseURL: server.URL + "/v1"}})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("FetchDiscovery error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestDiscoveryCacheWriteReadRoundTripLoadsAttempt(t *testing.T) {
	home := t.TempDir()
	fetchedAt := time.Now().Add(-48 * time.Hour).UTC()
	transport := discoveryTestTransport()
	if err := WriteDiscoveryCache(home, "local", transport, DiscoveredProvider{Models: map[string]DiscoveredModel{
		"qwen3": {
			Name:            "Qwen 3",
			ContextWindow:   40960,
			MaxOutputTokens: 8192,
			Cost:            &Cost{Input: floatPtr(0.20), Output: floatPtr(0.80)},
			metadata: &discoveryModelMetadata{
				Type:                         "chat",
				ArchitectureOutputModalities: []string{"text"},
				Capabilities:                 map[string]bool{"chat_completion": true, "embeddings": false},
				SupportedParameters:          []string{"tools"},
			},
		},
	}}, fetchedAt); err != nil {
		t.Fatalf("WriteDiscoveryCache returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "local.json")); err != nil {
		t.Fatalf("discovery cache file missing: %v", err)
	}

	cache, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v, want none", warnings)
	}
	if !cache["local"].AttemptedAt.Equal(fetchedAt) {
		t.Fatalf("attempt = %v, want %v", cache["local"].AttemptedAt, fetchedAt)
	}
	model := cache["local"].Models["qwen3"]
	if model.Name != "Qwen 3" || model.ContextWindow != 40960 || model.MaxOutputTokens != 8192 {
		t.Fatalf("round-tripped model = %#v", model)
	}
	if model.Cost == nil || model.Cost.Input == nil || !floatNear(*model.Cost.Input, 0.20) {
		t.Fatalf("round-tripped cost = %#v", model.Cost)
	}
	if model.metadata == nil {
		t.Fatalf("round-tripped metadata = nil")
	}
	if model.metadata.Type != "chat" || !reflect.DeepEqual(model.metadata.ArchitectureOutputModalities, []string{"text"}) {
		t.Fatalf("round-tripped metadata = %#v", model.metadata)
	}
	if got := model.metadata.Capabilities["chat_completion"]; !got {
		t.Fatalf("round-tripped capabilities = %#v, want chat_completion=true", model.metadata.Capabilities)
	}
	if got := model.metadata.Capabilities["embeddings"]; got {
		t.Fatalf("round-tripped capabilities = %#v, want embeddings=false", model.metadata.Capabilities)
	}
}

func TestReadDiscoveryCacheSkipsMalformedFilesWithWarning(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".lightcode", "cache", "discovery", "bad.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cache, warnings := ReadDiscoveryCache(home)
	if len(cache) != 0 {
		t.Fatalf("cache = %#v, want empty", cache)
	}
	if len(warnings) != 1 || warnings[0].Kind != "discovery_failure" || warnings[0].Provider != "bad" {
		t.Fatalf("warnings = %#v, want discovery_failure for bad", warnings)
	}
}

func TestReadDiscoveryCacheOldShapeWithoutMetadataKeepsUnknown(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".lightcode", "cache", "discovery", "local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{
		"fetched_at": "2026-05-30T00:00:00Z",
		"models": {
			"old": {"id": "old", "name": "Old", "context_window": 8192}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cache, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v", warnings)
	}
	if len(cache["local"].Models) != 0 || !cache["local"].AttemptedAt.IsZero() {
		t.Fatalf("legacy cache record = %#v, want unbound empty record", cache["local"])
	}
}

func TestWriteDiscoveryAttemptPreservesCachedModels(t *testing.T) {
	home := t.TempDir()
	fetchedAt := time.Now().Add(-48 * time.Hour).UTC()
	attemptedAt := time.Now().UTC()
	transport := discoveryTestTransport()
	if err := WriteDiscoveryCache(home, "local", transport, DiscoveredProvider{Models: map[string]DiscoveredModel{
		"qwen3": {Name: "Qwen 3", ContextWindow: 40960, MaxOutputTokens: 8192},
	}}, fetchedAt); err != nil {
		t.Fatalf("WriteDiscoveryCache returned error: %v", err)
	}
	if err := WriteDiscoveryAttempt(home, "local", transport, attemptedAt); err != nil {
		t.Fatalf("WriteDiscoveryAttempt returned error: %v", err)
	}
	cache, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v", warnings)
	}
	if _, ok := cache["local"].Models["qwen3"]; !ok {
		t.Fatalf("cached model missing after attempt write: %#v", cache)
	}
	if !cache["local"].AttemptedAt.Equal(attemptedAt) {
		t.Fatalf("attempt = %v, want %v", cache["local"].AttemptedAt, attemptedAt)
	}
}

func TestWriteDiscoveryCacheRejectsUnsafeProviderID(t *testing.T) {
	err := WriteDiscoveryCache(t.TempDir(), "../bad", discoveryTestTransport(), DiscoveredProvider{}, time.Now())
	if err == nil || !errors.Is(err, ErrInvalidModelRef) {
		t.Fatalf("WriteDiscoveryCache error = %v, want ErrInvalidModelRef", err)
	}
}

func floatPtr(v float64) *float64 {
	return &v
}

func discoveryTestTransport() Transport {
	return Transport{BaseURL: "http://127.0.0.1:9/v1"}
}

func TestTransportFingerprintCanonicalIdentity(t *testing.T) {
	base := Transport{
		BaseURL:   "https://example.test/v1",
		APIKeyEnv: "EXAMPLE_KEY",
		Headers: map[string]string{
			"X-B":           "b-value",
			"X-A":           "a-value",
			"Authorization": "Bearer first-secret",
		},
		Options: map[string]any{
			"nested": map[string]any{
				"large":   json.Number("90071992547409931234567890"),
				"decimal": json.Number("1.2300"),
			},
			"array": []any{json.Number("1"), "1", true},
		},
	}
	equivalent := Transport{
		BaseURL:   base.BaseURL,
		APIKeyEnv: base.APIKeyEnv,
		Headers: map[string]string{
			"X-A":           "a-value",
			"Authorization": "Bearer first-secret",
			"X-B":           "b-value",
		},
		Options: map[string]any{
			"array": []any{json.Number("1.0"), "1", true},
			"nested": map[string]any{
				"decimal": json.Number("1.23000"),
				"large":   json.Number("90071992547409931234567890"),
			},
		},
	}
	if !SameTransport(base, equivalent) {
		t.Fatal("equivalent transports did not share a fingerprint")
	}
	changedConfiguredAuthorization := equivalent
	changedConfiguredAuthorization.Headers = map[string]string{
		"X-A":           "a-value",
		"Authorization": "Bearer changed-secret",
		"X-B":           "b-value",
	}
	if SameTransport(base, changedConfiguredAuthorization) {
		t.Fatal("different configured Authorization values shared a fingerprint")
	}
	if SameTransport(base, Transport{BaseURL: base.BaseURL, APIKeyEnv: "OTHER_KEY", Headers: base.Headers, Options: base.Options}) {
		t.Fatal("different API key environment names shared a fingerprint")
	}
	changedHeader := equivalent
	changedHeader.Headers = map[string]string{"X-A": "changed"}
	if SameTransport(base, changedHeader) {
		t.Fatal("different header values shared a fingerprint")
	}
	changedArray := equivalent
	changedArray.Options = map[string]any{"array": []any{true, "1", json.Number("1.0")}}
	if SameTransport(base, changedArray) {
		t.Fatal("different array order shared a fingerprint")
	}
	changedScalar := equivalent
	changedScalar.Options = map[string]any{"array": []any{json.Number("1.0"), json.Number("1"), true}}
	if SameTransport(base, changedScalar) {
		t.Fatal("different scalar types shared a fingerprint")
	}
	if !SameTransport(
		Transport{BaseURL: base.BaseURL, APIKeyEnv: base.APIKeyEnv},
		Transport{BaseURL: base.BaseURL, APIKeyEnv: base.APIKeyEnv, Headers: map[string]string{}, Options: map[string]any{}},
	) {
		t.Fatal("nil and empty transport maps did not share a fingerprint")
	}
	if len(transportFingerprint(base)) != sha256.Size*2 || strings.ToLower(transportFingerprint(base)) != transportFingerprint(base) {
		t.Fatalf("transport fingerprint = %q, want lowercase SHA-256 hex", transportFingerprint(base))
	}
}

func TestTransportFingerprintIgnoresResolvedAPIKey(t *testing.T) {
	const apiKeyEnv = "LIGHTCODE_FINGERPRINT_RESOLVED_SECRET"
	transport := Transport{BaseURL: "https://example.test/v1", APIKeyEnv: apiKeyEnv}
	t.Setenv(apiKeyEnv, "resolved-secret-a")
	firstFingerprint := transportFingerprint(transport)
	firstSame := SameTransport(transport, transport)
	t.Setenv(apiKeyEnv, "resolved-secret-b")
	secondFingerprint := transportFingerprint(transport)
	secondSame := SameTransport(transport, transport)
	if firstFingerprint != secondFingerprint {
		t.Fatalf("resolved secret changed fingerprint: first=%q second=%q", firstFingerprint, secondFingerprint)
	}
	if !firstSame || firstSame != secondSame {
		t.Fatalf("SameTransport changed with resolved secret: before=%v after=%v", firstSame, secondSame)
	}
}

func TestTransportFingerprintDecimalNormalization(t *testing.T) {
	transportWithNumber := func(number string) Transport {
		return Transport{Options: map[string]any{"number": json.Number(number)}}
	}
	for _, pair := range [][2]string{
		{"1e+2", "100"},
		{"123e-2", "1.23"},
		{"-1.20e-2", "-0.012"},
		{"-0e99999", "0"},
		{"90071992547409931234567890e-10", "9007199254740993.123456789"},
		{"1e+200000", "100e+199998"},
		{"1e-200000", "100e-200002"},
	} {
		t.Run(pair[0]+"="+pair[1], func(t *testing.T) {
			if !SameTransport(transportWithNumber(pair[0]), transportWithNumber(pair[1])) {
				t.Fatalf("%s and %s did not normalize equally", pair[0], pair[1])
			}
		})
	}
	for _, pair := range [][2]string{
		{"1.23e2", "0.0123"},
		{"123e-2", "12300"},
		{"-1.20e-2", "-0.120"},
	} {
		t.Run(pair[0]+"!="+pair[1], func(t *testing.T) {
			if SameTransport(transportWithNumber(pair[0]), transportWithNumber(pair[1])) {
				t.Fatalf("%s and %s collided", pair[0], pair[1])
			}
		})
	}
}

func TestTransportFingerprintFloat64AndJSONNumber(t *testing.T) {
	transportWithNumber := func(number any) Transport {
		return Transport{Options: map[string]any{"number": number}}
	}
	if !SameTransport(transportWithNumber(float64(1.25)), transportWithNumber(json.Number("125e-2"))) {
		t.Fatal("float64 and json.Number did not normalize equally")
	}
	if SameTransport(transportWithNumber(float64(1.25)), transportWithNumber(json.Number("126e-2"))) {
		t.Fatal("unequal float64 and json.Number values shared a fingerprint")
	}
}

func TestDiscoveryRecordsAreBoundForModelsAndTTL(t *testing.T) {
	home := t.TempDir()
	transportA := Transport{BaseURL: "http://a.test/v1"}
	transportB := Transport{BaseURL: "http://b.test/v1"}
	fetchedAt := time.Now().Add(-time.Hour).UTC()
	if err := WriteDiscoveryCache(home, "p", transportA, DiscoveredProvider{Models: map[string]DiscoveredModel{"a": {ContextWindow: 4096}}}, fetchedAt); err != nil {
		t.Fatal(err)
	}
	records, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 || !records["p"].BoundTo(transportA) {
		t.Fatalf("records = %#v, warnings = %#v", records, warnings)
	}
	if !records["p"].FetchedAt.Equal(fetchedAt) || records["p"].Models["a"].ContextWindow != 4096 {
		t.Fatalf("matching record = %#v", records["p"])
	}
	if recent, err := DiscoveryAttemptRecent(home, "p", transportA, time.Now().UTC()); err != nil || !recent {
		t.Fatalf("matching transport recent = (%v, %v), want (true, nil)", recent, err)
	}
	if recent, err := DiscoveryAttemptRecent(home, "p", transportB, time.Now().UTC()); err != nil || recent {
		t.Fatalf("foreign transport recent = (%v, %v), want (false, nil)", recent, err)
	}
	if err := WriteDiscoveryAttempt(home, "p", transportA, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	records, _ = ReadDiscoveryCache(home)
	if records["p"].Models["a"].ContextWindow != 4096 || !records["p"].FetchedAt.Equal(fetchedAt) {
		t.Fatalf("matching attempt did not preserve record fields: %#v", records["p"])
	}
	if err := WriteDiscoveryAttempt(home, "p", transportB, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	records, _ = ReadDiscoveryCache(home)
	if !records["p"].BoundTo(transportB) || len(records["p"].Models) != 0 || !records["p"].FetchedAt.IsZero() {
		t.Fatalf("foreign attempt retained stale fields: %#v", records["p"])
	}
	if err := WriteDiscoveryCache(home, "p", transportA, DiscoveredProvider{Models: map[string]DiscoveredModel{"stale": {}}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	records, _ = ReadDiscoveryCache(home)
	cat := &Catalog{Providers: map[string]*Provider{"p": {ID: "p", Discovery: true, Transport: transportB}}}
	candidates := DiscoveryRefreshCandidates(cat, records, time.Now().UTC())
	if !reflect.DeepEqual(candidates, []string{"p"}) {
		t.Fatalf("foreign physical overwrite candidates = %v, want [p]", candidates)
	}
}

func floatNear(a, b float64) bool {
	const epsilon = 0.0000001
	if a > b {
		return a-b < epsilon
	}
	return b-a < epsilon
}

func TestParseDiscoveredModelReadsMaxTokensFromTopProvider(t *testing.T) {
	id, model := parseDiscoveredModel(map[string]any{
		"id":             "test-model",
		"context_length": float64(128000),
		"top_provider": map[string]any{
			"max_completion_tokens": float64(16384),
		},
	})
	if id != "test-model" {
		t.Fatalf("id = %q, want test-model", id)
	}
	if model.ContextWindow != 128000 {
		t.Fatalf("ContextWindow = %d, want 128000", model.ContextWindow)
	}
	if model.MaxOutputTokens != 16384 {
		t.Fatalf("MaxOutputTokens = %d, want 16384", model.MaxOutputTokens)
	}
}

func TestParseDiscoveredModelPreservesGenericMetadata(t *testing.T) {
	id, model := parseDiscoveredModel(map[string]any{
		"id":                "metadata-model",
		"type":              "Chat",
		"task":              "text-generation",
		"input_modalities":  []any{"text", "image"},
		"output_modalities": []any{"text"},
		"modalities":        "text",
		"architecture": map[string]any{
			"input_modalities":  []any{"TEXT", "audio"},
			"output_modalities": []any{"Text"},
			"modality":          "Text + Image -> Text",
		},
		"capabilities": map[string]any{
			"chat_completion": true,
			"embeddings":      false,
		},
		"supported_parameters": []any{"tools", "response-format"},
	})
	if id != "metadata-model" {
		t.Fatalf("id = %q, want metadata-model", id)
	}
	meta := model.metadata
	if meta == nil {
		t.Fatal("Metadata = nil")
	}
	if meta.Type != "chat" || meta.Task != "text_generation" {
		t.Fatalf("Type/Task = %q/%q, want chat/text_generation", meta.Type, meta.Task)
	}
	if !reflect.DeepEqual(meta.InputModalities, []string{"image", "text"}) {
		t.Fatalf("InputModalities = %#v", meta.InputModalities)
	}
	if !reflect.DeepEqual(meta.OutputModalities, []string{"text"}) {
		t.Fatalf("OutputModalities = %#v", meta.OutputModalities)
	}
	if !reflect.DeepEqual(meta.ArchitectureInputModalities, []string{"audio", "text"}) {
		t.Fatalf("ArchitectureInputModalities = %#v", meta.ArchitectureInputModalities)
	}
	if !reflect.DeepEqual(meta.ArchitectureOutputModalities, []string{"text"}) {
		t.Fatalf("ArchitectureOutputModalities = %#v", meta.ArchitectureOutputModalities)
	}
	if meta.ArchitectureModality != "text+image->text" {
		t.Fatalf("ArchitectureModality = %q", meta.ArchitectureModality)
	}
	if !meta.Capabilities["chat_completion"] || meta.Capabilities["embeddings"] {
		t.Fatalf("Capabilities = %#v", meta.Capabilities)
	}
	if !reflect.DeepEqual(meta.SupportedParameters, []string{"response_format", "tools"}) {
		t.Fatalf("SupportedParameters = %#v", meta.SupportedParameters)
	}
}

func TestParseDiscoveredModelPrefersNestedTopProviderOverTopLevel(t *testing.T) {
	id, model := parseDiscoveredModel(map[string]any{
		"id":                    "aggregator-route",
		"context_length":        float64(1000000),
		"max_completion_tokens": float64(1000000),
		"top_provider": map[string]any{
			"max_completion_tokens": float64(128000),
		},
	})
	if id != "aggregator-route" {
		t.Fatalf("id = %q, want aggregator-route", id)
	}
	if model.MaxOutputTokens != 128000 {
		t.Fatalf("MaxOutputTokens = %d, want 128000 (per-route cap from top_provider, not the model-wide aggregate)", model.MaxOutputTokens)
	}
}

func TestParseDiscoveredModelSkipsNullMaxTokensInTopProvider(t *testing.T) {
	id, model := parseDiscoveredModel(map[string]any{
		"id":             "free-model",
		"context_length": float64(200000),
		"top_provider": map[string]any{
			"max_completion_tokens": nil,
		},
	})
	if id != "free-model" {
		t.Fatalf("id = %q, want free-model", id)
	}
	if model.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", model.ContextWindow)
	}
	if model.MaxOutputTokens != 0 {
		t.Fatalf("MaxOutputTokens = %d, want 0 (null in API, fallback happens in merge)", model.MaxOutputTokens)
	}
}

// smallStale returns a small stale discovery cache. The assertions only need
// stale content distinct from the fresh discovery so a clobber is detectable;
// nothing depends on write sizes, so the seed stays minimal.
func smallStale() DiscoveredProvider {
	return DiscoveredProvider{Models: map[string]DiscoveredModel{
		"stale-1": {Name: "Stale 1", ContextWindow: 1000},
		"stale-2": {Name: "Stale 2", ContextWindow: 2000},
	}}
}

// sortedModelKeys returns the sorted model IDs of a discovered provider.
func sortedModelKeys(discovered DiscoveredProvider) []string {
	keys := make([]string, 0, len(discovered.Models))
	for id := range discovered.Models {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	return keys
}

// recordDiscoveryLockAcquisitions installs the discoveryLockAcquiredHook seam
// so every discovery lock acquisition is recorded, and returns the recorded
// paths. It restores the previous hook on test cleanup.
func recordDiscoveryLockAcquisitions(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var acquired []string
	origHook := discoveryLockAcquiredHook
	discoveryLockAcquiredHook = func(lockPath string) {
		mu.Lock()
		acquired = append(acquired, lockPath)
		mu.Unlock()
	}
	t.Cleanup(func() { discoveryLockAcquiredHook = origHook })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), acquired...)
	}
}

// TestConcurrentAttemptWritesLoseNoDiscovery proves two concurrent attempt
// writes cannot lose a discovery published at the same time. The directly
// observable fact is the lock: every discovery cache writer for the provider
// must acquire the same per-provider discovery lock, exactly once per call.
// If either writer's lock is removed the recorded acquisitions fall short on
// every run, with no timing involved. With both locks present the writers
// serialize and the fresh discovery survives.
func TestConcurrentAttemptWritesLoseNoDiscovery(t *testing.T) {
	home := t.TempDir()
	if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()); err != nil {
		t.Fatalf("seed WriteDiscoveryCache: %v", err)
	}
	fresh := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"fresh-a": {Name: "Fresh A", ContextWindow: 4096},
		"fresh-b": {Name: "Fresh B", ContextWindow: 8192},
	}}

	acquisitions := recordDiscoveryLockAcquisitions(t)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // attempt writer 1
		defer wg.Done()
		if err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt 1: %v", err)
		}
	}()
	go func() { // attempt writer 2
		defer wg.Done()
		if err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt 2: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), fresh, time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryCache: %v", err)
		}
	}()
	wg.Wait()

	// Every writer acquires the same per-provider discovery lock exactly once
	// per call: a writer that skips the lock or uses a different path fails
	// this assertion deterministically.
	want := discoveryLockPath(home, "p")
	got := acquisitions()
	if len(got) != 3 {
		t.Fatalf("discovery lock acquisitions = %v, want 3 acquisitions of %s (a writer skipped the lock)", got, want)
	}
	for _, p := range got {
		if p != want {
			t.Fatalf("discovery lock acquisition path = %q, want %q (writers did not share one lock)", p, want)
		}
	}

	// Nothing was lost: the fresh discovery survives the concurrent attempts.
	cache, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v, want none", warnings)
	}
	gotModels := cache["p"].Models
	if len(gotModels) != 2 || gotModels["fresh-a"].Name != "Fresh A" || gotModels["fresh-b"].Name != "Fresh B" {
		t.Fatalf("fresh discovery lost by concurrent attempt writes: got %d models, want [fresh-a fresh-b]", len(gotModels))
	}
}

// TestConcurrentAttemptAndDiscoveryWriteLoseNeither proves a concurrent
// attempt write and whole-file write for the same provider lose neither. The
// directly observable fact is the lock: both write paths must acquire the
// same per-provider discovery lock, exactly once per call. If either writer's
// lock is removed the recorded acquisitions fall short on every run, with no
// timing involved. With both locks present the writers serialize and the
// fresh discovery survives.
func TestConcurrentAttemptAndDiscoveryWriteLoseNeither(t *testing.T) {
	home := t.TempDir()
	if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()); err != nil {
		t.Fatalf("seed WriteDiscoveryCache: %v", err)
	}
	fresh := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"fresh-a": {Name: "Fresh A", ContextWindow: 4096},
		"fresh-b": {Name: "Fresh B", ContextWindow: 8192},
	}}

	acquisitions := recordDiscoveryLockAcquisitions(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // failed refresh: only its attempt write lands
		defer wg.Done()
		if err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), fresh, time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryCache: %v", err)
		}
	}()
	wg.Wait()

	// Both write paths acquire the same per-provider discovery lock exactly
	// once per call: a writer that skips the lock or uses a different path
	// fails this assertion deterministically.
	want := discoveryLockPath(home, "p")
	got := acquisitions()
	if len(got) != 2 {
		t.Fatalf("discovery lock acquisitions = %v, want 2 acquisitions of %s (a writer skipped the lock)", got, want)
	}
	for _, p := range got {
		if p != want {
			t.Fatalf("discovery lock acquisition path = %q, want %q (writers did not share one lock)", p, want)
		}
	}

	// Nothing was lost: the fresh discovery survives the concurrent attempt
	// write, and the published file is complete, parseable JSON.
	cache, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v, want none", warnings)
	}
	gotModels := cache["p"].Models
	if len(gotModels) != 2 || gotModels["fresh-a"].Name != "Fresh A" || gotModels["fresh-b"].Name != "Fresh B" {
		t.Fatalf("fresh discovery lost by concurrent attempt write: got %d models, want [fresh-a fresh-b]", len(gotModels))
	}
	data, err := os.ReadFile(filepath.Join(discoveryCacheDir(home), "p.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	var raw discoveryCacheFile
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("published cache not complete JSON: %v", err)
	}
}

func TestConcurrentStaleTransportWriteAfterFreshTransport(t *testing.T) {
	home := t.TempDir()
	transportA := Transport{BaseURL: "http://a.test/v1", APIKeyEnv: "SHARED_DISCOVERY_KEY"}
	transportB := Transport{BaseURL: "http://b.test/v1", APIKeyEnv: "SHARED_DISCOVERY_KEY"}
	staleA := DiscoveredProvider{Models: map[string]DiscoveredModel{"stale-a": {Name: "Stale A", ContextWindow: 1000}}}
	freshB := DiscoveredProvider{Models: map[string]DiscoveredModel{"fresh-b": {Name: "Fresh B", ContextWindow: 2000}}}

	var acquisitionMu sync.Mutex
	acquisitions := 0
	bAcquired := make(chan struct{})
	allowB := make(chan struct{})
	aAcquired := make(chan struct{})
	originalHook := discoveryLockAcquiredHook
	discoveryLockAcquiredHook = func(lockPath string) {
		if lockPath != discoveryLockPath(home, "p") {
			t.Errorf("lock path = %q, want provider p lock", lockPath)
		}
		acquisitionMu.Lock()
		acquisitions++
		count := acquisitions
		acquisitionMu.Unlock()
		switch count {
		case 1:
			close(bAcquired)
			<-allowB
		case 2:
			close(aAcquired)
		}
	}
	t.Cleanup(func() { discoveryLockAcquiredHook = originalHook })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", transportB, freshB, time.Now().UTC()); err != nil {
			t.Errorf("fresh B WriteDiscoveryCache: %v", err)
		}
	}()
	<-bAcquired
	go func() {
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", transportA, staleA, time.Now().Add(-48*time.Hour).UTC()); err != nil {
			t.Errorf("stale A WriteDiscoveryCache: %v", err)
		}
	}()
	close(allowB)
	select {
	case <-aAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("stale A did not acquire the lock after fresh B released it")
	}
	wg.Wait()

	records, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v", warnings)
	}
	record := records["p"]
	if record.TransportFingerprint != transportFingerprint(transportA) || record.Models["stale-a"].Name != "Stale A" {
		t.Fatalf("final stale A record = %#v, want stale A after fresh B", record)
	}
	if record.BoundTo(transportB) {
		t.Fatal("B reader treated stale A record as bound")
	}
	if recent, err := DiscoveryAttemptRecent(home, "p", transportB, time.Now().UTC()); err != nil || recent {
		t.Fatalf("B attempt recency = (%v, %v), want (false, nil) for due stale record", recent, err)
	}
	candidates := DiscoveryRefreshCandidates(&Catalog{Providers: map[string]*Provider{
		"p": {ID: "p", Transport: transportB, Discovery: true, Models: map[string]*Model{}},
	}}, records, time.Now().UTC())
	if !reflect.DeepEqual(candidates, []string{"p"}) {
		t.Fatalf("B refresh candidates = %#v, want [p]", candidates)
	}
	result := Build(BuildInputs{
		UserRaw: map[string]any{"p": map[string]any{
			"transport": map[string]any{"base_url": transportB.BaseURL, "api_key_env": transportB.APIKeyEnv},
			"discovery": true,
			"models":    map[string]any{},
		}},
		Records: records,
	})
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "p", Model: "stale-a"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("B catalog stale model lookup = %v, want ErrUnknownModel", err)
	}
}

// TestConcurrentDiscoveryWritesDoNotTearFile proves two concurrent whole-file
// writes for the same provider never tear the file: after every round the file
// is complete JSON equal to exactly one of the two payloads.
func TestConcurrentDiscoveryWritesDoNotTearFile(t *testing.T) {
	home := t.TempDir()
	setA := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"tear-a-1": {Name: "A1", ContextWindow: 1000},
		"tear-a-2": {Name: "A2", ContextWindow: 2000},
		"tear-a-3": {Name: "A3", ContextWindow: 3000},
	}}
	setB := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"tear-b-1": {Name: "B1", ContextWindow: 4000},
		"tear-b-2": {Name: "B2", ContextWindow: 5000},
		"tear-b-3": {Name: "B3", ContextWindow: 6000},
	}}
	keysA := sortedModelKeys(setA)
	keysB := sortedModelKeys(setB)

	for round := 0; round < 30; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), setA, time.Now().UTC()); err != nil {
				t.Errorf("WriteDiscoveryCache A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), setB, time.Now().UTC()); err != nil {
				t.Errorf("WriteDiscoveryCache B: %v", err)
			}
		}()
		wg.Wait()

		data, err := os.ReadFile(filepath.Join(discoveryCacheDir(home), "p.json"))
		if err != nil {
			t.Fatalf("round %d: read cache file: %v", round, err)
		}
		var raw discoveryCacheFile
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("round %d: torn cache file, not complete JSON: %v", round, err)
		}
		got := make([]string, 0, len(raw.Models))
		for id := range raw.Models {
			got = append(got, id)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, keysA) && !reflect.DeepEqual(got, keysB) {
			t.Fatalf("round %d: torn cache file mixing payloads: %v", round, got)
		}
	}
}

// TestTryDiscoveryWriterRows covers the one-attempt Try attempt/cache writers:
// success performs the payload, contention returns (false, nil) without any
// payload operation, and an unsafe provider id is refused.
func TestTryDiscoveryWriterRows(t *testing.T) {
	t.Run("attempt_success", func(t *testing.T) {
		home := t.TempDir()
		if err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ok, err := TryWriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
		if !ok || err != nil {
			t.Fatalf("TryWriteDiscoveryAttempt = (%v, %v), want (true, nil)", ok, err)
		}
		records, _ := ReadDiscoveryCache(home)
		if records["p"].AttemptedAt.IsZero() {
			t.Fatal("attempted_at not recorded by TryWriteDiscoveryAttempt")
		}
	})
	t.Run("attempt_contention", func(t *testing.T) {
		home := t.TempDir()
		holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "p"))
		if err != nil || !ok {
			t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
		}
		defer holder.Release()
		got, err := TryWriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
		if got {
			t.Fatal("TryWriteDiscoveryAttempt acquired while the lock was held")
		}
		if err != nil {
			t.Fatalf("contention err = %v, want nil", err)
		}
		if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
			t.Fatalf("contended attempt write created the cache file: %v", statErr)
		}
	})
	t.Run("cache_success", func(t *testing.T) {
		home := t.TempDir()
		discovered := DiscoveredProvider{Models: map[string]DiscoveredModel{"m": {Name: "M", ContextWindow: 4096}}}
		ok, err := TryWriteDiscoveryCache(home, "p", discoveryTestTransport(), discovered, time.Now().UTC())
		if !ok || err != nil {
			t.Fatalf("TryWriteDiscoveryCache = (%v, %v), want (true, nil)", ok, err)
		}
		cache, _ := ReadDiscoveryCache(home)
		if _, ok := cache["p"].Models["m"]; !ok {
			t.Fatalf("cache after TryWriteDiscoveryCache = %#v, want model m", cache["p"].Models)
		}
	})
	t.Run("cache_contention", func(t *testing.T) {
		home := t.TempDir()
		holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "p"))
		if err != nil || !ok {
			t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
		}
		defer holder.Release()
		got, err := TryWriteDiscoveryCache(home, "p", discoveryTestTransport(), DiscoveredProvider{Models: map[string]DiscoveredModel{"m": {}}}, time.Now().UTC())
		if got {
			t.Fatal("TryWriteDiscoveryCache acquired while the lock was held")
		}
		if err != nil {
			t.Fatalf("contention err = %v, want nil", err)
		}
		if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
			t.Fatalf("contended cache write created the cache file: %v", statErr)
		}
	})
	t.Run("unsafe_id", func(t *testing.T) {
		home := t.TempDir()
		if _, err := TryWriteDiscoveryAttempt(home, "a/b", discoveryTestTransport(), time.Now().UTC()); err == nil {
			t.Fatal("TryWriteDiscoveryAttempt accepted an unsafe provider id")
		}
		if _, err := TryWriteDiscoveryCache(home, "a/b", discoveryTestTransport(), DiscoveredProvider{}, time.Now().UTC()); err == nil {
			t.Fatal("TryWriteDiscoveryCache accepted an unsafe provider id")
		}
	})
}

// TestFetchDiscoveryIfDueRows covers the fetch-only core: it returns the
// discovered data only on success, marks attempted once the network begins,
// suppresses a fetch when an attempt is recent, and performs no disk write in
// any outcome.
func TestFetchDiscoveryIfDueRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		home := t.TempDir()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"m","name":"M","context_window":4096}]}`))
		}))
		defer server.Close()
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: server.URL + "/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if !attempted {
			t.Fatal("FetchDiscoveryIfDue did not mark the network attempt")
		}
		if discovered == nil || len(discovered.Models) != 1 {
			t.Fatalf("discovered = %#v, want one model", discovered)
		}
		if len(warnings) != 0 {
			t.Fatalf("warnings = %#v, want none", warnings)
		}
		if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
			t.Fatalf("fetch-only core wrote the cache: %v", statErr)
		}
	})
	t.Run("valid_request_failure_marks_attempt", func(t *testing.T) {
		// Valid-request sibling: the models URL is well-formed, so the network
		// boundary is crossed; the fetch failure stays attempted and preserves
		// TTL suppression.
		home := t.TempDir()
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if !attempted {
			t.Fatal("FetchDiscoveryIfDue did not mark the network attempt on a valid-request failure")
		}
		if discovered != nil {
			t.Fatalf("discovered = %#v, want nil on failure", discovered)
		}
		if len(warnings) != 1 || warnings[0].Kind != "discovery_failure" {
			t.Fatalf("warnings = %#v, want one discovery_failure", warnings)
		}
		if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
			t.Fatalf("fetch-only core wrote the attempt on failure: %v", statErr)
		}
	})
	t.Run("invalid_url_refuses_before_attempt", func(t *testing.T) {
		// Invalid-request sibling: the models URL is rejected before the
		// network-attempt boundary, so attempted stays false, the existing
		// discovery_failure warning is returned, and no attempt/cache marker
		// is created through either wrapper.
		home := t.TempDir()
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "not an absolute url"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if attempted {
			t.Fatal("FetchDiscoveryIfDue marked an attempt for a pre-network URL validation failure")
		}
		if discovered != nil {
			t.Fatalf("discovered = %#v, want nil for an invalid request", discovered)
		}
		if len(warnings) != 1 || warnings[0].Kind != "discovery_failure" {
			t.Fatalf("warnings = %#v, want one discovery_failure", warnings)
		}
		if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
			t.Fatalf("invalid-request core created an attempt/cache marker: %v", statErr)
		}
	})
	t.Run("recent_attempt_suppresses", func(t *testing.T) {
		home := t.TempDir()
		if err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if attempted || discovered != nil || len(warnings) != 0 {
			t.Fatalf("FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) for a recent attempt", attempted, discovered, warnings)
		}
	})
	t.Run("missing_key_suppresses", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("LIGHTCODE_FETCH_DUE_KEY", "")
		_ = os.Unsetenv("LIGHTCODE_FETCH_DUE_KEY")
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "LIGHTCODE_FETCH_DUE_KEY"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if attempted || discovered != nil || len(warnings) != 0 {
			t.Fatalf("FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) without a key", attempted, discovered, warnings)
		}
	})
	t.Run("disabled_provider_suppresses", func(t *testing.T) {
		home := t.TempDir()
		provider := &Provider{ID: "p", Discovery: false, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		if attempted || discovered != nil || len(warnings) != 0 {
			t.Fatalf("FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) for a disabled provider", attempted, discovered, warnings)
		}
	})
}

// TestRefreshProviderDiscoveryTryContention proves the Try refresh wrapper
// returns a warning and publishes nothing when a foreign process holds the
// provider's discovery lock: no attempt, no cache, no catalog merge.
func TestRefreshProviderDiscoveryTryContention(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","name":"M","context_window":4096}]}`))
	}))
	defer server.Close()
	cat := &Catalog{Providers: map[string]*Provider{
		"p": {ID: "p", Builtin: true, Discovery: true, Models: map[string]*Model{}, Transport: Transport{BaseURL: server.URL + "/v1"}},
	}}
	holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "p"))
	if err != nil || !ok {
		t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
	}
	defer holder.Release()

	changed, warnings := RefreshProviderDiscoveryTryWithConfigPath(context.Background(), home, "", cat, "p")
	if changed {
		t.Fatal("Try refresh reported a change under foreign lock contention")
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0].Message, "lock") {
		t.Fatalf("warnings = %#v, want a discovery lock contention warning", warnings)
	}
	if _, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(statErr) {
		t.Fatalf("contended Try refresh wrote the cache: %v", statErr)
	}
	if len(cat.Providers["p"].Models) != 0 {
		t.Fatalf("contended Try refresh merged models into the catalog: %#v", cat.Providers["p"].Models)
	}
}

// discoveryBlockHolderEnv selects the child half of
// TestRefreshProviderDiscoveryBlocksUntilRelease and names the discovery lock
// path the child holds.
const discoveryBlockHolderEnv = "LIGHTCODE_CATALOG_DISCOVERY_BLOCK_HOLDER"

// TestRefreshProviderDiscoveryBlocksUntilRelease proves the blocking refresh
// wrapper stays blocked for startup callers: with a foreign process holding
// the provider's discovery lock, it stays parked and completes once the holder
// releases. It is the positive control beside the one-attempt Try wrapper. The
// holder is a self-exec child of this test binary, so the contention is a real
// cross-process flock, not a same-process open.
func TestRefreshProviderDiscoveryBlocksUntilRelease(t *testing.T) {
	if lockPath := os.Getenv(discoveryBlockHolderEnv); lockPath != "" {
		l, err := atomicfs.Acquire(lockPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := l.Release(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}

	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","name":"M","context_window":4096}]}`))
	}))
	defer server.Close()
	cat := &Catalog{Providers: map[string]*Provider{
		"p": {ID: "p", Builtin: true, Discovery: true, Models: map[string]*Model{}, Transport: Transport{BaseURL: server.URL + "/v1"}},
	}}
	lockPath := discoveryLockPath(home, "p")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRefreshProviderDiscoveryBlocksUntilRelease$")
	cmd.Env = append(os.Environ(), discoveryBlockHolderEnv+"="+lockPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start child: %v", err)
	}
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	reap := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		<-scannerDone
		return err
	}
	fail := func() string {
		cancel()
		_ = reap()
		return stderr.String()
	}
	t.Cleanup(func() { cancel(); _ = reap() })
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("child never held the discovery lock within %v: %v\n%s", 30*time.Second, ctx.Err(), fail())
	}

	started := make(chan struct{})
	done := make(chan struct {
		changed  bool
		warnings []Warning
	}, 1)
	go func() {
		close(started)
		changed, warnings := RefreshProviderDiscoveryWithConfigPath(context.Background(), home, "", cat, "p")
		done <- struct {
			changed  bool
			warnings []Warning
		}{changed, warnings}
	}()
	<-started
	select {
	case <-done:
		t.Fatal("blocking refresh returned while the foreign process held the discovery lock; the startup wrapper must stay blocked")
	case <-time.After(100 * time.Millisecond):
	}
	if err := reap(); err != nil {
		t.Fatalf("child after release: %v\n%s", err, stderr.String())
	}
	select {
	case res := <-done:
		if !res.changed || len(res.warnings) != 0 {
			t.Fatalf("blocking refresh after release = (%v, %#v), want (true, none)", res.changed, res.warnings)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking refresh did not complete after the holder released")
	}
	if len(cat.Providers["p"].Models) != 1 {
		t.Fatalf("merged models = %#v, want the fetched model", cat.Providers["p"].Models)
	}
}

// TestRefreshWrappersDoNotMarkInvalidURLAttempt proves a pre-network URL
// validation failure through either refresh wrapper creates no attempt or
// cache marker: both the blocking and the one-attempt Try wrapper return the
// existing discovery_failure warning with attempted=false, so no
// discovery/<id>.json file is ever created for a request that never reached
// the network.
func TestRefreshWrappersDoNotMarkInvalidURLAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(home string, cat *Catalog) (bool, []Warning)
	}{
		{"blocking", func(home string, cat *Catalog) (bool, []Warning) {
			return RefreshProviderDiscoveryWithConfigPath(context.Background(), home, "", cat, "p")
		}},
		{"try", func(home string, cat *Catalog) (bool, []Warning) {
			return RefreshProviderDiscoveryTryWithConfigPath(context.Background(), home, "", cat, "p")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cat := &Catalog{Providers: map[string]*Provider{
				"p": {ID: "p", Discovery: true, Transport: Transport{BaseURL: "not an absolute url"}},
			}}
			changed, warnings := tc.run(home, cat)
			if changed {
				t.Fatal("invalid-URL refresh reported a change")
			}
			if len(warnings) != 1 || warnings[0].Kind != "discovery_failure" {
				t.Fatalf("warnings = %#v, want one discovery_failure", warnings)
			}
			if _, err := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json")); !os.IsNotExist(err) {
				t.Fatalf("invalid-URL %s wrapper created an attempt/cache marker: %v", tc.name, err)
			}
		})
	}
}
