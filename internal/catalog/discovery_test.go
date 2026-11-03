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
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestFetchDiscoveryParsesModelsAndSendsHeaders(t *testing.T) {
	t.Setenv("TEST_DISCOVERY_API_KEY", "secret-token")
	seenRequest := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequest = true
		testassert.Equal(t, r.URL.Path, "/v1/models")
		testassert.Equal(t, r.Header.Get("Authorization"), "Bearer secret-token")
		testassert.Equal(t, r.Header.Get("X-Test-Header"), "yes")
		w.Header().Set("Content-Type", "application/json")
		// One successful shape: the valid document may be followed by JSON
		// whitespace (space, tab, CR, LF). Non-whitespace trailing data is
		// rejected by the bounded-failure rows.
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
		}` + " \t\r\n"))
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
	testassert.NoError(t, err)
	testassert.Require(t, seenRequest, "test server was not called")
	rich := discovered.Models["rich"]
	testassert.Require(t, rich.Name == "Rich Model" && rich.ContextWindow == 128000 && rich.MaxOutputTokens == 16384 && rich.Cost != nil && rich.Cost.Input != nil && rich.Cost.Output != nil, "rich model = %#v; rich cost = %#v, want input/output", rich, rich.Cost)
	testassert.Require(t, rich.Cost.CacheRead != nil && rich.Cost.CacheWrite != nil && floatNear(*rich.Cost.Input, 0.15) && floatNear(*rich.Cost.Output, 0.60) && floatNear(*rich.Cost.CacheRead, 0.03) && floatNear(*rich.Cost.CacheWrite, 0.20), "rich cost = %#v, want cache read/write; rich cost = %#v, want 0.15/0.60/0.03/0.20 per million", rich.Cost, rich.Cost)
	fallback := discovered.Models["fallback"]
	testassert.Require(t, fallback.ContextWindow == 32000 && fallback.MaxOutputTokens == 4096, "fallback model = %#v", fallback)
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
		{name: "empty_body", statusCode: http.StatusOK, body: ``, wantError: "parse discovery response"},
		{name: "trailing_value", statusCode: http.StatusOK, body: `{"data":[{"id":"m","context_length":8}]}{"data":[]}`, wantError: "unexpected trailing data"},
		{name: "trailing_garbage", statusCode: http.StatusOK, body: `{"data":[{"id":"m","context_length":8}]}xx`, wantError: "unexpected trailing data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := FetchDiscovery(context.Background(), server.Client(), &Provider{ID: "test", Transport: Transport{BaseURL: server.URL + "/v1"}})
			testassert.Require(t, err != nil && strings.Contains(err.Error(), tt.wantError), "FetchDiscovery error = %v, want substring %q", err, tt.wantError)
		})
	}
}

// sizedDiscoveryBody returns one valid discovery document padded through an
// ignored member to the exact requested byte length.
func sizedDiscoveryBody(total int) string {
	const prefix = `{"data":[{"id":"padded","context_length":8,"pad":"`
	const suffix = `"}]}`
	pad := total - len(prefix) - len(suffix)
	if pad < 0 {
		panic("sizedDiscoveryBody: requested length below the document skeleton")
	}
	return prefix + strings.Repeat("x", pad) + suffix
}

// discoveryRoundTripFunc is the private transport stub for the response
// framing a real server cannot produce: a body longer than the advertised
// Content-Length, so the leaf's byte limit cannot hide behind the header.
type discoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetchDiscoveryBoundsResponseBytes pins the one bounded response domain:
// a complete valid document at the exact maximum succeeds, and one byte more
// fails whether Content-Length is correct, absent, or misleading. The limit
// counts the bytes the transport delivers; no header is consulted.
func TestFetchDiscoveryBoundsResponseBytes(t *testing.T) {
	exact := sizedDiscoveryBody(maxDiscoveryResponseBytes)
	oversized := sizedDiscoveryBody(maxDiscoveryResponseBytes + 1)
	fetch := func(t *testing.T, client *http.Client, baseURL string) error {
		t.Helper()
		_, err := FetchDiscovery(context.Background(), client, &Provider{ID: "test", Transport: Transport{BaseURL: baseURL}})
		return err
	}

	t.Run("exact_limit_document_succeeds", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, exact)
		}))
		defer server.Close()
		discovered, err := FetchDiscovery(context.Background(), server.Client(), &Provider{ID: "test", Transport: Transport{BaseURL: server.URL + "/v1"}})
		testassert.NoError(t, err)
		entry := discovered.Models["padded"]
		testassert.Require(t, entry.ContextWindow == 8, "padded model = %#v, want the padded document's own projection", entry)
	})

	t.Run("over_limit_known_length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(oversized)))
			_, _ = io.WriteString(w, oversized)
		}))
		defer server.Close()
		{
			err := fetch(t, server.Client(), server.URL+"/v1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "byte maximum"), "FetchDiscovery over the limit = %v, want the byte-maximum refusal", err)
		}
	})

	t.Run("over_limit_chunked", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, oversized)
		}))
		defer server.Close()
		{
			err := fetch(t, server.Client(), server.URL+"/v1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "byte maximum"), "FetchDiscovery over the limit = %v, want the byte-maximum refusal", err)
		}
	})

	t.Run("over_limit_misleading_length", func(t *testing.T) {
		client := &http.Client{Transport: discoveryRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
				ContentLength: 10,
				Body:          io.NopCloser(strings.NewReader(oversized)),
			}, nil
		})}
		{
			err := fetch(t, client, "https://bounds.test/v1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "byte maximum"), "FetchDiscovery over the limit = %v, want the byte-maximum refusal despite the misleading length", err)
		}
	})
}

func TestDiscoveryCacheWriteReadRoundTripLoadsAttempt(t *testing.T) {
	home := t.TempDir()
	fetchedAt := time.Now().Add(-48 * time.Hour).UTC()
	transport := discoveryTestTransport()
	testassert.NoError(t, WriteDiscoveryCache(home, "local", transport, DiscoveredProvider{Models: map[string]DiscoveredModel{
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
	}}, fetchedAt))
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "local.json"))
		testassert.NoError(t, err)
	}

	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v, want none", warnings)
	testassert.Require(t, cache["local"].AttemptedAt.Equal(fetchedAt), "attempt = %v, want %v", cache["local"].AttemptedAt, fetchedAt)
	model := cache["local"].Models["qwen3"]
	testassert.Require(t, model.Name == "Qwen 3" && model.ContextWindow == 40960 && model.MaxOutputTokens == 8192 && model.Cost != nil && model.Cost.Input != nil && floatNear(*model.Cost.Input, 0.20), "round-tripped model = %#v; round-tripped cost = %#v", model, model.Cost)
	testassert.Require(t, model.metadata != nil, "round-tripped metadata = nil")
	testassert.Require(t, model.metadata.Type == "chat" && reflect.DeepEqual(model.metadata.ArchitectureOutputModalities, []string{"text"}), "round-tripped metadata = %#v", model.metadata)
	{
		got := model.metadata.Capabilities["chat_completion"]
		testassert.Require(t, got, "round-tripped capabilities = %#v, want chat_completion=true", model.metadata.Capabilities)
	}
	{
		got := model.metadata.Capabilities["embeddings"]
		testassert.Require(t, !got, "round-tripped capabilities = %#v, want embeddings=false", model.metadata.Capabilities)
	}
}

func TestReadDiscoveryCacheSkipsMalformedFilesWithWarning(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".lightcode", "cache", "discovery", "bad.json")
	testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	testassert.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))

	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(cache) == 0, "cache = %#v, want empty", cache)
	testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "discovery_failure" && warnings[0].Provider == "bad", "warnings = %#v, want discovery_failure for bad", warnings)
}

func TestReadDiscoveryCacheOldShapeWithoutMetadataKeepsUnknown(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".lightcode", "cache", "discovery", "local.json")
	testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	testassert.NoError(t, os.WriteFile(path, []byte(`{
		"fetched_at": "2026-05-30T00:00:00Z",
		"models": {
			"old": {"id": "old", "name": "Old", "context_window": 8192}
		}
	}`), 0o600))

	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v", warnings)
	testassert.Require(t, len(cache["local"].Models) == 0 && cache["local"].AttemptedAt.IsZero(), "legacy cache record = %#v, want unbound empty record", cache["local"])
}

func TestWriteDiscoveryAttemptPreservesCachedModels(t *testing.T) {
	home := t.TempDir()
	fetchedAt := time.Now().Add(-48 * time.Hour).UTC()
	attemptedAt := time.Now().UTC()
	transport := discoveryTestTransport()
	testassert.NoError(t, WriteDiscoveryCache(home, "local", transport, DiscoveredProvider{Models: map[string]DiscoveredModel{
		"qwen3": {Name: "Qwen 3", ContextWindow: 40960, MaxOutputTokens: 8192},
	}}, fetchedAt))
	testassert.NoError(t, WriteDiscoveryAttempt(home, "local", transport, attemptedAt))
	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v", warnings)
	{
		_, ok := cache["local"].Models["qwen3"]
		testassert.Require(t, ok, "cached model missing after attempt write: %#v", cache)
	}
	testassert.Require(t, cache["local"].AttemptedAt.Equal(attemptedAt), "attempt = %v, want %v", cache["local"].AttemptedAt, attemptedAt)
}

func TestWriteDiscoveryCacheRejectsUnsafeProviderID(t *testing.T) {
	err := WriteDiscoveryCache(t.TempDir(), "../bad", discoveryTestTransport(), DiscoveredProvider{}, time.Now())
	testassert.Require(t, err != nil && errors.Is(err, ErrInvalidModelRef), "WriteDiscoveryCache error = %v, want ErrInvalidModelRef", err)
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
	}
	equivalent := Transport{
		BaseURL:   base.BaseURL,
		APIKeyEnv: base.APIKeyEnv,
		Headers: map[string]string{
			"X-A":           "a-value",
			"Authorization": "Bearer first-secret",
			"X-B":           "b-value",
		},
	}
	testassert.Require(t, SameTransport(base, equivalent), "equivalent transports did not share a fingerprint")
	changedConfiguredAuthorization := equivalent
	changedConfiguredAuthorization.Headers = map[string]string{
		"X-A":           "a-value",
		"Authorization": "Bearer changed-secret",
		"X-B":           "b-value",
	}
	testassert.Require(t, !SameTransport(base, changedConfiguredAuthorization), "different configured Authorization values shared a fingerprint")
	testassert.Require(t, !SameTransport(base, Transport{BaseURL: base.BaseURL, APIKeyEnv: "OTHER_KEY", Headers: base.Headers}), "different API key environment names shared a fingerprint")
	testassert.Require(t, !SameTransport(base, Transport{BaseURL: "https://other.test/v1", APIKeyEnv: base.APIKeyEnv, Headers: base.Headers}), "different base URLs shared a fingerprint")
	changedHeader := equivalent
	changedHeader.Headers = map[string]string{"X-A": "changed"}
	testassert.Require(t, !SameTransport(base, changedHeader), "different header values shared a fingerprint")
	testassert.Require(t, SameTransport(
		Transport{BaseURL: base.BaseURL, APIKeyEnv: base.APIKeyEnv},
		Transport{BaseURL: base.BaseURL, APIKeyEnv: base.APIKeyEnv, Headers: map[string]string{}},
	), "nil and empty transport headers did not share a fingerprint")
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
	testassert.Equal(t, firstFingerprint, secondFingerprint)
	testassert.Require(t, firstSame && firstSame == secondSame, "SameTransport changed with resolved secret: before=%v after=%v", firstSame, secondSame)
}

func TestDiscoveryRecordsAreBoundForModelsAndTTL(t *testing.T) {
	home := t.TempDir()
	transportA := Transport{BaseURL: "http://a.test/v1"}
	transportB := Transport{BaseURL: "http://b.test/v1"}
	fetchedAt := time.Now().Add(-time.Hour).UTC()
	testassert.NoError(t, WriteDiscoveryCache(home, "p", transportA, DiscoveredProvider{Models: map[string]DiscoveredModel{"a": {ContextWindow: 4096}}}, fetchedAt))
	records, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0 && records["p"].BoundTo(transportA), "records = %#v, warnings = %#v", records, warnings)
	testassert.Require(t, records["p"].FetchedAt.Equal(fetchedAt) && records["p"].Models["a"].ContextWindow == 4096, "matching record = %#v", records["p"])
	{
		recent, err := DiscoveryAttemptRecent(home, "p", transportA, time.Now().UTC())
		testassert.Require(t, err == nil && recent, "matching transport recent = (%v, %v), want (true, nil)", recent, err)
	}
	{
		recent, err := DiscoveryAttemptRecent(home, "p", transportB, time.Now().UTC())
		testassert.Require(t, err == nil && !recent, "foreign transport recent = (%v, %v), want (false, nil)", recent, err)
	}
	testassert.NoError(t, WriteDiscoveryAttempt(home, "p", transportA, time.Now().UTC()))
	records, _ = ReadDiscoveryCache(home)
	testassert.Require(t, records["p"].Models["a"].ContextWindow == 4096 && records["p"].FetchedAt.Equal(fetchedAt), "matching attempt did not preserve record fields: %#v", records["p"])
	testassert.NoError(t, WriteDiscoveryAttempt(home, "p", transportB, time.Now().UTC()))
	records, _ = ReadDiscoveryCache(home)
	testassert.Require(t, records["p"].BoundTo(transportB) && len(records["p"].Models) == 0 && records["p"].FetchedAt.IsZero(), "foreign attempt retained stale fields: %#v", records["p"])
	testassert.NoError(t, WriteDiscoveryCache(home, "p", transportA, DiscoveredProvider{Models: map[string]DiscoveredModel{"stale": {}}}, time.Now().UTC()))
	records, _ = ReadDiscoveryCache(home)
	cat := &Catalog{Providers: map[string]*Provider{"p": {ID: "p", Discovery: true, Transport: transportB}}}
	candidates := DiscoveryRefreshCandidates(cat, records, time.Now().UTC())
	testassert.Require(t, reflect.DeepEqual(candidates, []string{"p"}), "foreign physical overwrite candidates = %v, want [p]", candidates)
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
	testassert.Equal(t, id, "test-model")
	testassert.Require(t, model.ContextWindow == 128000 && model.MaxOutputTokens == 16384, "got %v, want %v; got %v, want %v", model.ContextWindow, 128000, model.MaxOutputTokens, 16384)
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
	testassert.Equal(t, id, "metadata-model")
	meta := model.metadata
	testassert.Require(t, meta != nil, "Metadata = nil")
	testassert.Require(t, meta.Type == "chat" && meta.Task == "text_generation" && reflect.DeepEqual(meta.InputModalities, []string{"image", "text"}) && reflect.DeepEqual(meta.OutputModalities, []string{"text"}) && reflect.DeepEqual(meta.ArchitectureInputModalities, []string{"audio", "text"}) && reflect.DeepEqual(meta.ArchitectureOutputModalities, []string{"text"}) && meta.ArchitectureModality == "text+image->text" && meta.Capabilities["chat_completion"] && !meta.Capabilities["embeddings"] && reflect.DeepEqual(meta.SupportedParameters, []string{"response_format", "tools"}), "Type/Task = %q/%q, want chat/text_generation; InputModalities = %#v; OutputModalities = %#v; ArchitectureInputModalities = %#v; ArchitectureOutputModalities = %#v; got %v, want %v; Capabilities = %#v; SupportedParameters = %#v", meta.Type, meta.Task, meta.InputModalities, meta.OutputModalities, meta.ArchitectureInputModalities, meta.ArchitectureOutputModalities, meta.ArchitectureModality, "text+image->text", meta.Capabilities, meta.SupportedParameters)
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
	testassert.Equal(t, id, "aggregator-route")
	testassert.Equal(t, model.MaxOutputTokens, 128000)
}

func TestParseDiscoveredModelSkipsNullMaxTokensInTopProvider(t *testing.T) {
	id, model := parseDiscoveredModel(map[string]any{
		"id":             "free-model",
		"context_length": float64(200000),
		"top_provider": map[string]any{
			"max_completion_tokens": nil,
		},
	})
	testassert.Equal(t, id, "free-model")
	testassert.Require(t, model.ContextWindow == 200000 && model.MaxOutputTokens == 0, "got %v, want %v; got %v, want %v", model.ContextWindow, 200000, model.MaxOutputTokens, 0)
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
	testassert.NoError(t, WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()))
	fresh := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"fresh-a": {Name: "Fresh A", ContextWindow: 4096},
		"fresh-b": {Name: "Fresh B", ContextWindow: 8192},
	}}

	acquisitions := recordDiscoveryLockAcquisitions(t)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // attempt writer 1
		defer wg.Done()
		{
			err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
			testassert.Check(t, err == nil, "WriteDiscoveryAttempt 1: %v", err)
		}
	}()
	go func() { // attempt writer 2
		defer wg.Done()
		{
			err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
			testassert.Check(t, err == nil, "WriteDiscoveryAttempt 2: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		{
			err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), fresh, time.Now().UTC())
			testassert.Check(t, err == nil, "WriteDiscoveryCache: %v", err)
		}
	}()
	wg.Wait()

	// Every writer acquires the same per-provider discovery lock exactly once
	// per call: a writer that skips the lock or uses a different path fails
	// this assertion deterministically.
	want := discoveryLockPath(home, "p")
	got := acquisitions()
	testassert.Require(t, len(got) == 3, "discovery lock acquisitions = %v, want 3 acquisitions of %s (a writer skipped the lock)", got, want)
	for _, p := range got {
		testassert.Equal(t, p, want)
	}

	// Nothing was lost: the fresh discovery survives the concurrent attempts.
	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v, want none", warnings)
	gotModels := cache["p"].Models
	testassert.Require(t, len(gotModels) == 2 && gotModels["fresh-a"].Name == "Fresh A" && gotModels["fresh-b"].Name == "Fresh B", "fresh discovery lost by concurrent attempt writes: got %d models, want [fresh-a fresh-b]", len(gotModels))
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
	testassert.NoError(t, WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()))
	fresh := DiscoveredProvider{Models: map[string]DiscoveredModel{
		"fresh-a": {Name: "Fresh A", ContextWindow: 4096},
		"fresh-b": {Name: "Fresh B", ContextWindow: 8192},
	}}

	acquisitions := recordDiscoveryLockAcquisitions(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // failed refresh: only its attempt write lands
		defer wg.Done()
		{
			err := WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
			testassert.Check(t, err == nil, "WriteDiscoveryAttempt: %v", err)
		}
	}()
	go func() { // successful refresh: publishes fresh discovery
		defer wg.Done()
		{
			err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), fresh, time.Now().UTC())
			testassert.Check(t, err == nil, "WriteDiscoveryCache: %v", err)
		}
	}()
	wg.Wait()

	// Both write paths acquire the same per-provider discovery lock exactly
	// once per call: a writer that skips the lock or uses a different path
	// fails this assertion deterministically.
	want := discoveryLockPath(home, "p")
	got := acquisitions()
	testassert.Require(t, len(got) == 2, "discovery lock acquisitions = %v, want 2 acquisitions of %s (a writer skipped the lock)", got, want)
	for _, p := range got {
		testassert.Equal(t, p, want)
	}

	// Nothing was lost: the fresh discovery survives the concurrent attempt
	// write, and the published file is complete, parseable JSON.
	cache, warnings := ReadDiscoveryCache(home)
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v, want none", warnings)
	gotModels := cache["p"].Models
	testassert.Require(t, len(gotModels) == 2 && gotModels["fresh-a"].Name == "Fresh A" && gotModels["fresh-b"].Name == "Fresh B", "fresh discovery lost by concurrent attempt write: got %d models, want [fresh-a fresh-b]", len(gotModels))
	data, err := os.ReadFile(filepath.Join(discoveryCacheDir(home), "p.json"))
	testassert.NoError(t, err)
	var raw discoveryCacheFile
	testassert.NoError(t, json.Unmarshal(data, &raw))
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
		testassert.Check(t, lockPath == discoveryLockPath(home, "p"), "lock path = %q, want provider p lock", lockPath)
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
		{
			err := WriteDiscoveryCache(home, "p", transportB, freshB, time.Now().UTC())
			testassert.Check(t, err == nil, "fresh B WriteDiscoveryCache: %v", err)
		}
	}()
	<-bAcquired
	go func() {
		defer wg.Done()
		{
			err := WriteDiscoveryCache(home, "p", transportA, staleA, time.Now().Add(-48*time.Hour).UTC())
			testassert.Check(t, err == nil, "stale A WriteDiscoveryCache: %v", err)
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
	testassert.Require(t, len(warnings) == 0, "ReadDiscoveryCache warnings = %#v", warnings)
	record := records["p"]
	testassert.Require(t, record.TransportFingerprint == transportFingerprint(transportA) && record.Models["stale-a"].Name == "Stale A", "final stale A record = %#v, want stale A after fresh B", record)
	testassert.Require(t, !record.BoundTo(transportB), "B reader treated stale A record as bound")
	{
		recent, err := DiscoveryAttemptRecent(home, "p", transportB, time.Now().UTC())
		testassert.Require(t, err == nil && !recent, "B attempt recency = (%v, %v), want (false, nil) for due stale record", recent, err)
	}
	candidates := DiscoveryRefreshCandidates(&Catalog{Providers: map[string]*Provider{
		"p": {ID: "p", Transport: transportB, Discovery: true, Models: map[string]*Model{}},
	}}, records, time.Now().UTC())
	testassert.Require(t, reflect.DeepEqual(candidates, []string{"p"}), "B refresh candidates = %#v, want [p]", candidates)
	result := Build(BuildInputs{
		UserRaw: map[string]any{"p": map[string]any{
			"transport": map[string]any{"base_url": transportB.BaseURL, "api_key_env": transportB.APIKeyEnv},
			"discovery": true,
			"models":    map[string]any{},
		}},
		Records: records,
	})
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "p", Model: "stale-a"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "B catalog stale model lookup = %v, want ErrUnknownModel", err)
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
			{
				err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), setA, time.Now().UTC())
				testassert.Check(t, err == nil, "WriteDiscoveryCache A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			{
				err := WriteDiscoveryCache(home, "p", discoveryTestTransport(), setB, time.Now().UTC())
				testassert.Check(t, err == nil, "WriteDiscoveryCache B: %v", err)
			}
		}()
		wg.Wait()

		data, err := os.ReadFile(filepath.Join(discoveryCacheDir(home), "p.json"))
		testassert.Require(t, err == nil, "round %d: read cache file: %v", round, err)
		var raw discoveryCacheFile
		{
			err := json.Unmarshal(data, &raw)
			testassert.Require(t, err == nil, "round %d: torn cache file, not complete JSON: %v", round, err)
		}
		got := make([]string, 0, len(raw.Models))
		for id := range raw.Models {
			got = append(got, id)
		}
		sort.Strings(got)
		testassert.Require(t, reflect.DeepEqual(got, keysA) || reflect.DeepEqual(got, keysB), "round %d: torn cache file mixing payloads: %v", round, got)
	}
}

// TestTryDiscoveryWriterRows covers the one-attempt Try attempt/cache writers:
// success performs the payload, contention returns (false, nil) without any
// payload operation, and an unsafe provider id is refused.
func TestTryDiscoveryWriterRows(t *testing.T) {
	t.Run("attempt_success", func(t *testing.T) {
		home := t.TempDir()
		testassert.NoError(t, WriteDiscoveryCache(home, "p", discoveryTestTransport(), smallStale(), time.Now().Add(-48*time.Hour).UTC()))
		ok, err := TryWriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
		testassert.Require(t, ok && err == nil, "TryWriteDiscoveryAttempt = (%v, %v), want (true, nil)", ok, err)
		records, _ := ReadDiscoveryCache(home)
		testassert.Require(t, !records["p"].AttemptedAt.IsZero(), "attempted_at not recorded by TryWriteDiscoveryAttempt")
	})
	t.Run("attempt_contention", func(t *testing.T) {
		home := t.TempDir()
		holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "p"))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		got, err := TryWriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC())
		testassert.Require(t, !got, "TryWriteDiscoveryAttempt acquired while the lock was held")
		testassert.NoError(t, err)
		{
			_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
			testassert.Require(t, os.IsNotExist(statErr), "contended attempt write created the cache file: %v", statErr)
		}
	})
	t.Run("cache_success", func(t *testing.T) {
		home := t.TempDir()
		discovered := DiscoveredProvider{Models: map[string]DiscoveredModel{"m": {Name: "M", ContextWindow: 4096}}}
		ok, err := TryWriteDiscoveryCache(home, "p", discoveryTestTransport(), discovered, time.Now().UTC())
		testassert.Require(t, ok && err == nil, "TryWriteDiscoveryCache = (%v, %v), want (true, nil)", ok, err)
		cache, _ := ReadDiscoveryCache(home)
		{
			_, ok := cache["p"].Models["m"]
			testassert.Require(t, ok, "cache after TryWriteDiscoveryCache = %#v, want model m", cache["p"].Models)
		}
	})
	t.Run("cache_contention", func(t *testing.T) {
		home := t.TempDir()
		holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "p"))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		got, err := TryWriteDiscoveryCache(home, "p", discoveryTestTransport(), DiscoveredProvider{Models: map[string]DiscoveredModel{"m": {}}}, time.Now().UTC())
		testassert.Require(t, !got, "TryWriteDiscoveryCache acquired while the lock was held")
		testassert.NoError(t, err)
		{
			_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
			testassert.Require(t, os.IsNotExist(statErr), "contended cache write created the cache file: %v", statErr)
		}
	})
	t.Run("unsafe_id", func(t *testing.T) {
		home := t.TempDir()
		{
			_, err := TryWriteDiscoveryAttempt(home, "a/b", discoveryTestTransport(), time.Now().UTC())
			testassert.Require(t, err != nil, "TryWriteDiscoveryAttempt accepted an unsafe provider id")
		}
		{
			_, err := TryWriteDiscoveryCache(home, "a/b", discoveryTestTransport(), DiscoveredProvider{}, time.Now().UTC())
			testassert.Require(t, err != nil, "TryWriteDiscoveryCache accepted an unsafe provider id")
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
		testassert.Require(t, attempted, "FetchDiscoveryIfDue did not mark the network attempt")
		testassert.Require(t, discovered != nil && len(discovered.Models) == 1, "discovered = %#v, want one model", discovered)
		testassert.Require(t, len(warnings) == 0, "warnings = %#v, want none", warnings)
		{
			_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
			testassert.Require(t, os.IsNotExist(statErr), "fetch-only core wrote the cache: %v", statErr)
		}
	})
	t.Run("valid_request_failure_marks_attempt", func(t *testing.T) {
		// Valid-request sibling: the models URL is well-formed, so the network
		// boundary is crossed; the fetch failure stays attempted and preserves
		// TTL suppression.
		home := t.TempDir()
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		testassert.Require(t, attempted, "FetchDiscoveryIfDue did not mark the network attempt on a valid-request failure")
		testassert.Require(t, discovered == nil, "discovered = %#v, want nil on failure", discovered)
		testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "discovery_failure", "warnings = %#v, want one discovery_failure", warnings)
		{
			_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
			testassert.Require(t, os.IsNotExist(statErr), "fetch-only core wrote the attempt on failure: %v", statErr)
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
		testassert.Require(t, !attempted, "FetchDiscoveryIfDue marked an attempt for a pre-network URL validation failure")
		testassert.Require(t, discovered == nil, "discovered = %#v, want nil for an invalid request", discovered)
		testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "discovery_failure", "warnings = %#v, want one discovery_failure", warnings)
		{
			_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
			testassert.Require(t, os.IsNotExist(statErr), "invalid-request core created an attempt/cache marker: %v", statErr)
		}
	})
	t.Run("recent_attempt_suppresses", func(t *testing.T) {
		home := t.TempDir()
		testassert.NoError(t, WriteDiscoveryAttempt(home, "p", discoveryTestTransport(), time.Now().UTC()))
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		testassert.Require(t, !attempted && discovered == nil && len(warnings) == 0, "FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) for a recent attempt", attempted, discovered, warnings)
	})
	t.Run("missing_key_suppresses", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("LIGHTCODE_FETCH_DUE_KEY", "")
		_ = os.Unsetenv("LIGHTCODE_FETCH_DUE_KEY")
		provider := &Provider{ID: "p", Discovery: true, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "LIGHTCODE_FETCH_DUE_KEY"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		testassert.Require(t, !attempted && discovered == nil && len(warnings) == 0, "FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) without a key", attempted, discovered, warnings)
	})
	t.Run("disabled_provider_suppresses", func(t *testing.T) {
		home := t.TempDir()
		provider := &Provider{ID: "p", Discovery: false, Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"}}
		attempted, discovered, warnings := FetchDiscoveryIfDue(context.Background(), home, provider, time.Now().UTC())
		testassert.Require(t, !attempted && discovered == nil && len(warnings) == 0, "FetchDiscoveryIfDue = (%v, %#v, %#v), want (false, nil, none) for a disabled provider", attempted, discovered, warnings)
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
	testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
	defer holder.Release()

	changed, warnings := RefreshProviderDiscoveryTryWithConfigPath(context.Background(), home, "", cat, "p")
	testassert.Require(t, !changed, "Try refresh reported a change under foreign lock contention")
	testassert.Require(t, len(warnings) != 0 && strings.Contains(warnings[0].Message, "lock"), "warnings = %#v, want a discovery lock contention warning", warnings)
	{
		_, statErr := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
		testassert.Require(t, os.IsNotExist(statErr), "contended Try refresh wrote the cache: %v", statErr)
	}
	testassert.Require(t, len(cat.Providers["p"].Models) == 0, "contended Try refresh merged models into the catalog: %#v", cat.Providers["p"].Models)
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
		testassert.Require(t, res.changed && len(res.warnings) == 0, "blocking refresh after release = (%v, %#v), want (true, none)", res.changed, res.warnings)
	case <-time.After(5 * time.Second):
		t.Fatal("blocking refresh did not complete after the holder released")
	}
	testassert.Require(t, len(cat.Providers["p"].Models) == 1, "merged models = %#v, want the fetched model", cat.Providers["p"].Models)
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
			testassert.Require(t, !changed, "invalid-URL refresh reported a change")
			testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "discovery_failure", "warnings = %#v, want one discovery_failure", warnings)
			{
				_, err := os.Stat(filepath.Join(discoveryCacheDir(home), "p.json"))
				testassert.Require(t, os.IsNotExist(err), "invalid-URL %s wrapper created an attempt/cache marker: %v", tc.name, err)
			}
		})
	}
}
