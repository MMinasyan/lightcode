package catalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestLoaderReadsBundledUserConfigAndDiscoveryCache(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
		"providers": {
			"local": {
				"transport": {"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"discovery": true,
				"models": {
					"known": {"context_window": 32768, "max_output_tokens": 8192}
				}
			}
		},
		"default_model": "local/known"
	}`)
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "local.json"), `{
		"fetched_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"attempted_at": "`+time.Now().UTC().Format(time.RFC3339)+`",
		"models": {
			"known": {"id": "known", "name": "Known Remote", "context_window": 40960, "max_output_tokens": 8192, "cost": {"input": 0.2}},
			"qwen3": {"id": "qwen3", "name": "Qwen 3", "context_window": 40960, "max_output_tokens": 8192}
		}
	}`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if hasNonDiscoveryWarning(warnings) {
		t.Fatalf("Load warnings = %#v, want no non-discovery warnings", warnings)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"}); err != nil {
		t.Fatalf("bundled openai model missing after Load: %v", err)
	}
	if _, model, err := catalog.Lookup(ModelRef{Provider: "local", Model: "known"}); err != nil {
		t.Fatalf("user-config model missing after Load: %v", err)
	} else if model.ContextWindow != 32768 || model.MaxOutputTokens != 8192 {
		t.Fatalf("user-config model = %#v", model)
	} else if model.Cost == nil || model.Cost.Input == nil || *model.Cost.Input != 0.2 {
		t.Fatalf("user-config model cost = %#v, want discovery price", model.Cost)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"}); err == nil {
		t.Fatalf("config-only provider gained discovered model")
	}
}

func TestLoaderCreatesEmptyConfigAndFallsBackToBundled(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if hasNonDiscoveryWarning(warnings) {
		t.Fatalf("Load warnings = %#v, want no non-discovery warnings", warnings)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "config.json")); err != nil {
		t.Fatalf("config skeleton was not created: %v", err)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"}); err != nil {
		t.Fatalf("bundled openai model missing after missing config fallback: %v", err)
	}
}

func TestLoaderMalformedConfigFallsBackToBundledWithWarning(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{not json`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !hasWarning(warnings, "user_config_skip") {
		t.Fatalf("Load warnings = %#v, want user_config_skip", warnings)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"}); err != nil {
		t.Fatalf("bundled openai model missing after malformed config fallback: %v", err)
	}
}

func TestLoaderSkipsMalformedDiscoveryCacheWithWarning(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
		"providers": {
			"local": {
				"transport": {"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models": {}
			}
		}
	}`)
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "local.json"), `{not json`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !hasWarning(warnings, "discovery_failure") {
		t.Fatalf("Load warnings = %#v, want discovery_failure", warnings)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Lookup local/qwen3 error = %v, want unknown model/provider", err)
	}
}

func TestLoaderFetchesDiscoveryWhenAttemptDueForBundledProvider(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","name":"Remote Model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)

	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {}
		}`)},
	}

	catalog, warnings, err := NewLoader(home, fsys).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if hasNonDiscoveryWarning(warnings) {
		t.Fatalf("Load warnings = %#v, want no non-discovery warnings", warnings)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls)
	}
	if _, model, err := catalog.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err != nil {
		t.Fatalf("discovered model missing after live Load discovery: %v", err)
	} else if model.Name != "Remote Model" || model.ContextWindow != 16384 || model.MaxOutputTokens != 2048 {
		t.Fatalf("discovered model = %#v", model)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); err != nil {
		t.Fatalf("discovery cache was not written: %v", err)
	}
}

func TestLoaderSkipsDiscoveryWhenAttemptRecent(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"), `{
		"attempted_at": "`+time.Now().UTC().Format(time.RFC3339)+`",
		"models": {}
	}`)
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
		}`)},
	}

	catalog, warnings, err := NewLoader(home, fsys).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if hasNonDiscoveryWarning(warnings) {
		t.Fatalf("Load warnings = %#v, want no non-discovery warnings", warnings)
	}
	if calls != 0 {
		t.Fatalf("discovery calls = %d, want 0", calls)
	}
	if _, _, err := catalog.Lookup(ModelRef{Provider: "remote", Model: "known"}); err != nil {
		t.Fatalf("known model missing: %v", err)
	}
}

func TestLoaderSkipsMissingAPIKeySilently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MISSING_DISCOVERY_KEY", "")
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "https://example.com/v1", "api_key_env": "MISSING_DISCOVERY_KEY"},
			"discovery": true,
			"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
		}`)},
	}

	if _, warnings, err := NewLoader(home, fsys).Load(); err != nil {
		t.Fatalf("Load returned error: %v", err)
	} else if len(warnings) != 0 {
		t.Fatalf("Load warnings = %#v, want none", warnings)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); !os.IsNotExist(err) {
		t.Fatalf("discovery cache stat = %v, want missing cache", err)
	}
}

func TestLoaderDiscoveryFailureWarnsAndDoesNotBlockStartup(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
		"providers": {
			"local": {
				"transport": {"base_url": "`+server.URL+`/v1", "api_key_env": ""},
				"discovery": true,
				"models": {}
			}
		}
	}`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if catalog == nil {
		t.Fatal("Load returned nil catalog after discovery failure")
	}
	if !hasWarning(warnings, "discovery_failure") {
		t.Fatalf("Load warnings = %#v, want discovery_failure", warnings)
	}
	_, err = os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "local.json"))
	if err != nil {
		t.Fatalf("discovery attempt cache was not written: %v", err)
	}
}

func TestLoaderFailedDiscoveryAttemptDoesNotRetryWithinTTL(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
		"providers": {
			"local": {
				"transport": {"base_url": "`+server.URL+`/v1", "api_key_env": ""},
				"discovery": true,
				"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
			}
		}
	}`)

	if _, warnings, err := NewLoader(home, bundledFS).Load(); err != nil {
		t.Fatalf("first Load returned error: %v", err)
	} else if !hasWarning(warnings, "discovery_failure") {
		t.Fatalf("first Load warnings = %#v, want discovery_failure", warnings)
	}
	if _, warnings, err := NewLoader(home, bundledFS).Load(); err != nil {
		t.Fatalf("second Load returned error: %v", err)
	} else if hasWarning(warnings, "discovery_failure") {
		t.Fatalf("second Load warnings = %#v, want no retry failure", warnings)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls)
	}
}

func writeLoaderFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func hasWarning(warnings []Warning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func hasNonDiscoveryWarning(warnings []Warning) bool {
	for _, warning := range warnings {
		if warning.Kind != "discovery_failure" {
			return true
		}
	}
	return false
}

func disableBuiltinDiscoveryKeys(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
}

func TestLoaderAllowRefreshGateExcludesUnconnected(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)

	// Provider is due for discovery (stale cache) but AllowRefresh will reject it.
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"), `{
		"fetched_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"attempted_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"models": {}
	}`)
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
		}`)},
	}

	loader := NewLoader(home, fsys)
	loader.AllowRefresh = func(_ string, _ *Provider) bool { return false }
	cat, _, err := loader.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("discovery calls = %d, want 0 (gate rejected unconnected)", calls)
	}
	if _, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err == nil {
		t.Fatal("discovered model appeared despite AllowRefresh=false")
	}
}

func TestLoaderAllowRefreshGateIncludesConnected(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)

	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"), `{
		"fetched_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"attempted_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"models": {}
	}`)
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
		}`)},
	}

	loader := NewLoader(home, fsys)
	loader.AllowRefresh = func(_ string, _ *Provider) bool { return true }
	cat, _, err := loader.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1 (gate allowed connected)", calls)
	}
	if _, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err != nil {
		t.Fatalf("discovered model missing after allowed refresh: %v", err)
	}
}

func TestLoaderNoAllowRefreshRefreshesAllCandidates(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)

	writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"), `{
		"fetched_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"attempted_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"models": {}
	}`)
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {"known": {"context_window": 1000, "max_output_tokens": 100}}
		}`)},
	}

	// No AllowRefresh set — should behave as before and refresh all candidates.
	cat, _, err := NewLoader(home, fsys).Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1 (no gate = refresh all)", calls)
	}
	if _, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err != nil {
		t.Fatalf("discovered model missing: %v", err)
	}
}

func TestDiscoveryRefreshCandidatesIsMechanical(t *testing.T) {
	// DiscoveryRefreshCandidates must not consult connection state; it only
	// checks the discovery flag and TTL. Connection gating is the loader's job.
	cat := &Catalog{
		Providers: map[string]*Provider{
			"unconnected": {
				ID:        "unconnected",
				Discovery: true,
				Transport: Transport{APIKeyEnv: "DEFINITELY_NOT_SET"},
				Models:    map[string]*Model{"m": {ID: "m", ContextWindow: 1000}},
			},
			"connected": {
				ID:        "connected",
				Discovery: true,
				Transport: Transport{BaseURL: "http://127.0.0.1:9/v1"},
				Models:    map[string]*Model{"m": {ID: "m", ContextWindow: 1000}},
			},
			"no-discovery": {
				ID:        "no-discovery",
				Discovery: false,
				Models:    map[string]*Model{"m": {ID: "m", ContextWindow: 1000}},
			},
		},
	}
	candidates := DiscoveryRefreshCandidates(cat, nil, time.Now().UTC())
	want := map[string]bool{"unconnected": true, "connected": true}
	got := map[string]bool{}
	for _, id := range candidates {
		got[id] = true
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("candidate %q missing; got %v", id, candidates)
		}
	}
	if got["no-discovery"] {
		t.Fatalf("no-discovery provider should not be a candidate; got %v", candidates)
	}
}

// TestLoaderLoadTryContentionWarnsAndDoesNotHang proves the Try Loader routes
// discovery publication through the one-attempt Try writers: with a foreign
// process holding a due provider's discovery lock, LoadTry returns the catalog
// with the existing discovery_failure warning instead of hanging on the lock.
func TestLoaderLoadTryContentionWarnsAndDoesNotHang(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
		"providers": {
			"local": {
				"transport": {"base_url": "http://127.0.0.1:9/v1", "api_key_env": ""},
				"discovery": true,
				"models": {}
			}
		}
	}`)
	holder, ok, err := atomicfs.TryAcquire(discoveryLockPath(home, "local"))
	if err != nil || !ok {
		t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
	}
	defer holder.Release()

	done := make(chan struct {
		cat      *Catalog
		warnings []Warning
		err      error
	}, 1)
	go func() {
		cat, warnings, err := NewLoader(home, bundledFS).LoadTry()
		done <- struct {
			cat      *Catalog
			warnings []Warning
			err      error
		}{cat, warnings, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("LoadTry under discovery contention = %v, want a catalog plus warnings", res.err)
		}
		if res.cat == nil {
			t.Fatal("LoadTry returned a nil catalog under discovery contention")
		}
		if !hasWarning(res.warnings, "discovery_failure") {
			t.Fatalf("LoadTry warnings = %#v, want a discovery_failure warning", res.warnings)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadTry hung on the foreign discovery lock; Try publication must not block shutdown")
	}
}

// TestLoaderLoadTrySucceedsLikeLoad proves the Try Loader publishes through
// the one-attempt writers exactly like the blocking Load when the lock is
// free: the discovery cache is written and the catalog merged.
func TestLoaderLoadTrySucceedsLikeLoad(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","name":"Remote Model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{
		"builtin/remote.json": {Data: []byte(`{
			"id": "remote",
			"name": "Remote",
			"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
			"discovery": true,
			"models": {}
		}`)},
	}
	cat, warnings, err := NewLoader(home, fsys).LoadTry()
	if err != nil {
		t.Fatalf("LoadTry: %v", err)
	}
	if hasWarning(warnings, "discovery_failure") {
		t.Fatalf("LoadTry warnings = %#v, want no discovery_failure", warnings)
	}
	if _, ok := cat.Providers["remote"].Models["remote-model"]; !ok {
		t.Fatalf("LoadTry did not merge the fetched model: %#v", cat.Providers["remote"].Models)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); statErr != nil {
		t.Fatalf("LoadTry did not write the discovery cache: %v", statErr)
	}
}
