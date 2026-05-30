package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	if err := WriteDiscoveryCache(home, "local", DiscoveredProvider{Models: map[string]DiscoveredModel{
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

	cache, attempts, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v, want none", warnings)
	}
	if !attempts["local"].Equal(fetchedAt) {
		t.Fatalf("attempt = %v, want %v", attempts["local"], fetchedAt)
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

	cache, attempts, warnings := ReadDiscoveryCache(home)
	if len(cache) != 0 {
		t.Fatalf("cache = %#v, want empty", cache)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %#v, want none", attempts)
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

	cache, _, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v", warnings)
	}
	model := cache["local"].Models["old"]
	if model.metadata != nil {
		t.Fatalf("old cache metadata = %#v, want nil", model.metadata)
	}
	if !discoveredModelAllowed(model) {
		t.Fatalf("old cache model without metadata was rejected")
	}
}

func TestWriteDiscoveryAttemptPreservesCachedModels(t *testing.T) {
	home := t.TempDir()
	fetchedAt := time.Now().Add(-48 * time.Hour).UTC()
	attemptedAt := time.Now().UTC()
	if err := WriteDiscoveryCache(home, "local", DiscoveredProvider{Models: map[string]DiscoveredModel{
		"qwen3": {Name: "Qwen 3", ContextWindow: 40960, MaxOutputTokens: 8192},
	}}, fetchedAt); err != nil {
		t.Fatalf("WriteDiscoveryCache returned error: %v", err)
	}
	if err := WriteDiscoveryAttempt(home, "local", attemptedAt); err != nil {
		t.Fatalf("WriteDiscoveryAttempt returned error: %v", err)
	}
	cache, attempts, warnings := ReadDiscoveryCache(home)
	if len(warnings) != 0 {
		t.Fatalf("ReadDiscoveryCache warnings = %#v", warnings)
	}
	if _, ok := cache["local"].Models["qwen3"]; !ok {
		t.Fatalf("cached model missing after attempt write: %#v", cache)
	}
	if !attempts["local"].Equal(attemptedAt) {
		t.Fatalf("attempt = %v, want %v", attempts["local"], attemptedAt)
	}
}

func TestWriteDiscoveryCacheRejectsUnsafeProviderID(t *testing.T) {
	err := WriteDiscoveryCache(t.TempDir(), "../bad", DiscoveredProvider{}, time.Now())
	if err == nil || !errors.Is(err, ErrInvalidModelRef) {
		t.Fatalf("WriteDiscoveryCache error = %v, want ErrInvalidModelRef", err)
	}
}

func floatPtr(v float64) *float64 {
	return &v
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
