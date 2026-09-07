package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
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
	if err != nil {
		t.Fatalf("LoadCaptured: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none: the captured layer must not report config problems", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "base", Model: "known"}); err != nil {
		t.Fatalf("bundled provider missing from the captured build: %v", err)
	}
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"}); err != nil {
		t.Fatalf("captured provider missing: %v", err)
	} else if model.ContextWindow != 9007199254740993 {
		t.Fatalf("captured model = %#v, want the exact number preserved from the captured input", model)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("LoadCaptured read or created the main configuration: %v", err)
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
	if err != nil {
		t.Fatalf("LoadCaptured: %v", err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want the due provider to be fetched", calls)
	}
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "m"}); err != nil {
		t.Fatalf("discovered model missing after the one-attempt publication: %v", err)
	} else if model.Cost == nil || model.Cost.Input == nil || *model.Cost.Input != 0.5 || model.Cost.Output == nil || *model.Cost.Output != 1.2 {
		t.Fatalf("discovered model cost = %#v, want the captured-declared input protected and the discovered output filled", model.Cost)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); err != nil {
		t.Fatalf("discovery cache was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("cost protection reread the main configuration: %v", err)
	}

	// The retained 24h attempted-at TTL suppresses a duplicate fetch.
	if _, err := loader.LoadCaptured(context.Background(), userRaw); err != nil {
		t.Fatalf("second LoadCaptured: %v", err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want the recent attempt TTL retained", calls)
	}
}

func TestLoaderLoadCapturedCancellationSkipsPublicationWithoutUndoingWrites(t *testing.T) {
	home := t.TempDir()
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a-model","context_window":1000}]}`))
	}))
	t.Cleanup(okServer.Close)

	// The stale candidate's fetch runs against a test-only listener that
	// accepts and never answers, so the caller cancellation lands exactly
	// while that fetch is in flight: the fetch aborts, and the observed
	// cancellation is reported before any cache writer starts.
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadCaptured error = %v, want the caller's context error unwrapped", err)
	}
	if result.Catalog != nil || result.Warnings != nil {
		t.Fatalf("canceled LoadCaptured published result %#v, want the zero BuildResult", result)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "aa.json")); statErr != nil {
		t.Fatalf("the cache write that had started was undone: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "bb.json")); !os.IsNotExist(statErr) {
		t.Fatalf("the canceled fetch's publication was not skipped: %v", statErr)
	}
}

// stallTransport starts a test-only listener that accepts connections and
// never answers them. It returns the listener's base URL and a channel that
// receives exactly one signal when the first request connection arrives.
func stallTransport(t *testing.T) (string, chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stall listener: %v", err)
	}
	stalled := make(chan struct{}, 1)
	accepted := make(chan net.Conn, 64)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- conn:
			default:
				_ = conn.Close()
			}
			select {
			case stalled <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for {
			select {
			case conn := <-accepted:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	return "http://" + listener.Addr().String(), stalled
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
	if err != nil {
		t.Fatalf("a failed fetch must stay a warning, not an error: %v", err)
	}
	if !hasWarningForProvider(result.Warnings, "discovery_failure", "remote") {
		t.Fatalf("warnings = %#v, want the discovery_failure warning", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "known"}); err != nil {
		t.Fatalf("cached input lost after a failed fetch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); statErr != nil {
		t.Fatalf("the failed fetch's attempt marker was not written: %v", statErr)
	}
}

func TestLoaderLoadCapturedContentionWarnsAndKeepsCachedInput(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	transport := Transport{BaseURL: server.URL + "/v1"}
	if err := WriteDiscoveryCache(home, "remote", transport, DiscoveredProvider{
		Models: map[string]DiscoveredModel{"cached-model": {ContextWindow: 4096}},
	}, time.Now().Add(-48*time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "remote"))
	if err != nil || !ok {
		t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
	}
	defer holder.Release()

	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}
	result, err := NewLoader(home, fsys).LoadCaptured(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadCaptured under discovery contention = %v, want a result plus warnings", err)
	}
	if !hasWarningForProvider(result.Warnings, "discovery_failure", "remote") {
		t.Fatalf("warnings = %#v, want the retained contention warning", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "cached-model"}); err != nil {
		t.Fatalf("cached input lost under contention: %v", err)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err == nil {
		t.Fatal("the contended publication merged anyway")
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
	if err := WriteDiscoveryCache(home, "remote", Transport{BaseURL: server.URL + "/v1"}, DiscoveredProvider{
		Models: map[string]DiscoveredModel{"cached-model": {ContextWindow: 4096}},
	}, time.Now().Add(-48*time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}

	loader := NewLoader(home, fsys)
	loader.AllowRefresh = func(_ string, _ *Provider) bool { return false }
	result, err := loader.LoadCaptured(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadCaptured: %v", err)
	}
	if calls != 0 {
		t.Fatalf("discovery calls = %d, want the AllowRefresh filter retained", calls)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "remote", Model: "cached-model"}); err != nil {
		t.Fatalf("cached input lost under the filter: %v", err)
	}
}
