package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestLoaderLoadCapturedUsesInputWithoutReadingConfig(t *testing.T) {
	home := t.TempDir()
	fsys := fstest.MapFS{"builtin/base.json": {Data: []byte(`{
		"id": "base",
		"transport": {"base_url": "http://base.test/v1", "api_key_env": ""},
		"discovery": false,
		"models": {"known": {"context_window": 1000}}
	}`)}}
	userRaw := map[string]any{"local": map[string]any{
		"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
		"discovery": false,
		"models":    map[string]any{"known": map[string]any{"context_window": json.Number("9007199254740993")}},
	}}

	result, err := NewLoader(home, fsys).LoadCaptured(context.Background(), userRaw)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.Warnings) == 0, "warnings = %#v, want none: the captured layer must not report config problems", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "base", Model: "known"})
		testassert.NoError(t, err)
	}
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"}); err != nil {
		t.Fatalf("captured provider missing: %v", err)
	} else if model.ContextWindow != 9007199254740993 {
		t.Fatalf("captured model = %#v, want the exact number preserved from the captured input", model)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "config.json"))
		testassert.Require(t, os.IsNotExist(err), "LoadCaptured read or created the main configuration: %v", err)
	}
}

func TestLoaderLoadCapturedCostProtectionUsesCapturedInput(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000,"cost":{"input":0.9,"output":1.2}}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}
	userRaw := map[string]any{"remote": map[string]any{
		"models": map[string]any{"m": map[string]any{"cost": map[string]any{"input": json.Number("0.5")}}},
	}}
	loader := NewLoader(home, fsys)

	result, err := loader.LoadCaptured(context.Background(), userRaw)
	testassert.NoError(t, err)
	testassert.Equal(t, calls, 1)
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "m"}); err != nil {
		t.Fatalf("discovered model missing after the one-attempt publication: %v", err)
	} else if model.Cost == nil || model.Cost.Input == nil || *model.Cost.Input != 0.5 || model.Cost.Output == nil || *model.Cost.Output != 1.2 {
		t.Fatalf("discovered model cost = %#v, want the captured-declared input protected and the discovered output filled", model.Cost)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.NoError(t, err)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "config.json"))
		testassert.Require(t, os.IsNotExist(err), "cost protection reread the main configuration: %v", err)
	}

	// The retained 24h attempted-at TTL suppresses a duplicate fetch.
	{
		_, err := loader.LoadCaptured(context.Background(), userRaw)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, calls, 1)
}

func TestLoaderLoadCapturedCancellationSkipsPublicationWithoutUndoingWrites(t *testing.T) {
	home := t.TempDir()
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a-model","context_window":1000}]}`))
	}))
	t.Cleanup(okServer.Close)

	// The stale candidate's fetch runs against a test-only server that
	// receives the request and never answers, so the caller cancellation
	// lands exactly while that fetch is in flight: the fetch aborts, and the
	// observed cancellation is reported before any cache writer starts.
	staleBaseURL, stalled := stallTransport(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stalled:
		case <-time.After(30 * time.Second):
		}
		cancel()
	}()

	fsys := fstest.MapFS{
		"builtin/aa.json": {Data: []byte(`{
			"id": "aa",
			"transport": {"base_url": "` + okServer.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {}
		}`)},
		"builtin/bb.json": {Data: []byte(`{
			"id": "bb",
			"transport": {"base_url": "` + staleBaseURL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {}
		}`)},
	}

	result, err := NewLoader(home, fsys).LoadCaptured(ctx, nil)
	testassert.Require(t, errors.Is(err, context.Canceled), "LoadCaptured error = %v, want the caller's context error unwrapped", err)
	testassert.Require(t, result.Catalog == nil && result.Warnings == nil, "canceled LoadCaptured published result %#v, want the zero BuildResult", result)
	{
		_, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "aa.json"))
		testassert.Require(t, statErr == nil, "the cache write that had started was undone: %v", statErr)
	}
	{
		_, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "bb.json"))
		testassert.Require(t, os.IsNotExist(statErr), "the canceled fetch's publication was not skipped: %v", statErr)
	}
}

// stallTransport starts a test-only HTTP server whose handler signals the
// first request's arrival and then holds the request open until the client
// cancels it or the fixture's cleanup releases it. Cleanup releases before
// Server.Close, so a failed assertion cannot leave Close waiting on a live
// handler. It returns the server's base URL and a channel that receives
// exactly one signal when the first request arrives.
func stallTransport(t *testing.T) (string, chan struct{}) {
	t.Helper()
	stalled := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case stalled <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server.URL, stalled
}

func TestLoaderLoadCapturedFailedFetchKeepsWarningAndAttempt(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {"known": {"context_window": 1000}}
	}`)}}

	result, err := NewLoader(home, fsys).LoadCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	testassert.Require(t, hasWarningForProvider(result.Warnings, "discovery_failure", "remote"), "warnings = %#v, want the discovery_failure warning", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "known"})
		testassert.NoError(t, err)
	}
	{
		_, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.Require(t, statErr == nil, "the failed fetch's attempt marker was not written: %v", statErr)
	}
}

func TestLoaderLoadCapturedContentionWarnsAndKeepsCachedInput(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	transport := Transport{BaseURL: server.URL + "/v1"}
	testassert.NoError(t, WriteDiscoveryCache(home, "remote", transport, DiscoveredProvider{
		Models: map[string]DiscoveredModel{"cached-model": {ContextWindow: 4096}},
	}, time.Now().Add(-48*time.Hour).UTC()))
	holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "remote"))
	testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
	defer holder.Release()

	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}
	result, err := NewLoader(home, fsys).LoadCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	testassert.Require(t, hasWarningForProvider(result.Warnings, "discovery_failure", "remote"), "warnings = %#v, want the retained contention warning", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "cached-model"})
		testassert.NoError(t, err)
	}
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "remote-model"})
		testassert.Require(t, err != nil, "the contended publication merged anyway")
	}
}

func TestLoaderLoadCapturedAllowRefreshFilterRetained(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	testassert.NoError(t, WriteDiscoveryCache(home, "remote", Transport{BaseURL: server.URL + "/v1"}, DiscoveredProvider{
		Models: map[string]DiscoveredModel{"cached-model": {ContextWindow: 4096}},
	}, time.Now().Add(-48*time.Hour).UTC()))
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}

	loader := NewLoader(home, fsys)
	loader.AllowRefresh = func(_ string, _ *Provider) bool { return false }
	result, err := loader.LoadCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	testassert.Equal(t, calls, 0)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "cached-model"})
		testassert.NoError(t, err)
	}
}

// TestLoaderLoadCapturedConnectionOverlaysOneProviderWithoutRefresh proves
// the connection-candidate entry: the same captured bundled FS, discovery
// cache read, and Build producer — no network fetch, no cache write, no
// refresh — with a nonnil fetched result overlaying only that provider's
// in-memory record (fingerprint over the original configured transport,
// fetched model values carried verbatim) and a nil result leaving the
// ordinary cached inputs.
func TestLoaderLoadCapturedConnectionOverlaysOneProviderWithoutRefresh(t *testing.T) {
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{
		"builtin/base.json": {Data: []byte(`{
			"id": "base",
			"transport": {"base_url": "http://base.test/v1", "api_key_env": ""},
			"discovery": false,
			"models": {"known": {"context_window": 1000}}
		}`)},
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": "REMOTE_KEY"},
			"discovery": true,
			"models": {}
		}`)},
	}
	userRaw := map[string]any{"local": map[string]any{
		"transport": map[string]any{"base_url": "http://local.test/v1", "api_key_env": ""},
		"models":    map[string]any{},
	}}
	home := t.TempDir()
	loader := NewLoader(home, fsys)

	// The fetched discovery for the connection candidate is carried verbatim
	// with its unexported metadata intact: the explicitly filtered metadata
	// type stays absent, while the chat and nil-metadata siblings reach the
	// catalog.
	fetched := map[string]DiscoveredModel{
		"fresh":   {Name: "Fresh", ContextWindow: 4096, metadata: &discoveryModelMetadata{Type: "chat"}},
		"blocked": {Name: "Blocked", ContextWindow: 4096, metadata: &discoveryModelMetadata{Type: "embedding"}},
		"plain":   {Name: "Plain", ContextWindow: 4096},
	}
	transport := Transport{BaseURL: server.URL + "/v1", APIKeyEnv: "REMOTE_KEY"}

	result, err := loader.LoadCapturedConnection(context.Background(), userRaw, "remote", transport, &DiscoveredProvider{Models: fetched})
	testassert.NoError(t, err)
	testassert.Equal(t, fetches, 0)
	prov := result.Catalog.Providers["remote"]
	testassert.Require(t, prov != nil, "remote provider missing from the connection candidate")
	{
		entry := prov.Models["fresh"]
		testassert.Require(t, entry != nil && entry.ContextWindow == 4096, "overlay record missing the fetched model: %#v", prov.Models)
	}
	if entry := prov.Models["fresh"]; entry != nil && entry.Source != SourceDiscovered {
		t.Fatalf("overlay model source = %q, want discovered", entry.Source)
	}
	{
		entry := prov.Models["plain"]
		testassert.Require(t, entry != nil && entry.ContextWindow == 4096 && entry.Source == SourceDiscovered, "nil-metadata overlay sibling = %#v, want the allowed discovered model", prov.Models["plain"])
	}
	{
		entry, ok := prov.Models["blocked"]
		testassert.Require(t, !ok, "filtered metadata model reached the catalog: %#v", entry)
	}
	{
		_, ok := result.Catalog.Providers["local"]
		testassert.Require(t, ok, "the non-connection sibling disappeared from the candidate")
	}

	// Nil discovery: the ordinary cached inputs, still no fetch and no
	// cache write.
	result, err = loader.LoadCapturedConnection(context.Background(), userRaw, "remote", transport, nil)
	testassert.NoError(t, err)
	testassert.Equal(t, fetches, 0)
	if prov = result.Catalog.Providers["remote"]; prov == nil || len(prov.Models) != 0 {
		t.Fatalf("nil-discovery candidate carried %v, want the empty cached model set", prov.Models)
	}
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "base", Model: "known"})
		testassert.NoError(t, err)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.Require(t, os.IsNotExist(err), "the connection candidate wrote a cache file: %v", err)
	}

	// A fingerprint over a different transport never binds the overlay: the
	// same Build producer computes it inside catalog.
	other := Transport{BaseURL: "http://other.test/v1", APIKeyEnv: "REMOTE_KEY"}
	result, err = loader.LoadCapturedConnection(context.Background(), userRaw, "remote", other, &DiscoveredProvider{Models: fetched})
	testassert.NoError(t, err)
	if prov = result.Catalog.Providers["remote"]; prov != nil && len(prov.Models) != 0 {
		t.Fatalf("misbound overlay contributed %v, want no models", prov.Models)
	}
}
