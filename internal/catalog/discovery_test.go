package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
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
	if err := WriteDiscoveryCache(home, "p", smallStale(), time.Now().Add(-48*time.Hour).UTC()); err != nil {
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
		if err := WriteDiscoveryAttempt(home, "p", time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt 1: %v", err)
		}
	}()
	go func() { // attempt writer 2
		defer wg.Done()
		if err := WriteDiscoveryAttempt(home, "p", time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt 2: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", fresh, time.Now().UTC()); err != nil {
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
	cache, _, warnings := ReadDiscoveryCache(home)
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
	if err := WriteDiscoveryCache(home, "p", smallStale(), time.Now().Add(-48*time.Hour).UTC()); err != nil {
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
		if err := WriteDiscoveryAttempt(home, "p", time.Now().UTC()); err != nil {
			t.Errorf("WriteDiscoveryAttempt: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		if err := WriteDiscoveryCache(home, "p", fresh, time.Now().UTC()); err != nil {
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
	cache, _, warnings := ReadDiscoveryCache(home)
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
			if err := WriteDiscoveryCache(home, "p", setA, time.Now().UTC()); err != nil {
				t.Errorf("WriteDiscoveryCache A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := WriteDiscoveryCache(home, "p", setB, time.Now().UTC()); err != nil {
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
