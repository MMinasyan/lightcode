package catalog

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
		"transport_fingerprint": "`+transportFingerprint(Transport{BaseURL: "http://localhost:11434/v1"})+`",
		"fetched_at": "`+time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)+`",
		"attempted_at": "`+time.Now().UTC().Format(time.RFC3339)+`",
		"models": {
			"known": {"id": "known", "name": "Known Remote", "context_window": 40960, "max_output_tokens": 8192, "cost": {"input": 0.2}},
			"qwen3": {"id": "qwen3", "name": "Qwen 3", "context_window": 40960, "max_output_tokens": 8192}
		}
	}`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	testassert.NoError(t, err)
	testassert.Require(t, !hasNonDiscoveryWarning(warnings), "Load warnings = %#v, want no non-discovery warnings", warnings)
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"})
		testassert.NoError(t, err)
	}
	if _, model, err := catalog.Lookup(ModelRef{Provider: "local", Model: "known"}); err != nil {
		t.Fatalf("user-config model missing after Load: %v", err)
	} else if model.ContextWindow != 32768 || model.MaxOutputTokens != 8192 {
		t.Fatalf("user-config model = %#v", model)
	} else if model.Cost == nil || model.Cost.Input == nil || *model.Cost.Input != 0.2 {
		t.Fatalf("user-config model cost = %#v, want discovery price", model.Cost)
	}
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"})
		testassert.Require(t, err != nil, "config-only provider gained discovered model")
	}
}

func TestLoaderCreatesEmptyConfigAndFallsBackToBundled(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	testassert.NoError(t, err)
	testassert.Require(t, !hasNonDiscoveryWarning(warnings), "Load warnings = %#v, want no non-discovery warnings", warnings)
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "config.json"))
		testassert.NoError(t, err)
	}
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"})
		testassert.NoError(t, err)
	}
}

func TestLoaderMalformedConfigFallsBackToBundledWithWarning(t *testing.T) {
	disableBuiltinDiscoveryKeys(t)
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{not json`)

	catalog, warnings, err := NewLoader(home, bundledFS).Load()
	testassert.NoError(t, err)
	testassert.Require(t, hasWarning(warnings, "user_config_skip"), "Load warnings = %#v, want user_config_skip", warnings)
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5-mini"})
		testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	testassert.Require(t, hasWarning(warnings, "discovery_failure"), "Load warnings = %#v, want discovery_failure", warnings)
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"})
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "unknown"), "Lookup local/qwen3 error = %v, want unknown model/provider", err)
	}
}

func TestLoaderMalformedTransportKeepsValidSibling(t *testing.T) {
	home := t.TempDir()
	writeLoaderFile(t, filepath.Join(home, ".lightcode", "config.json"), `{
  "providers": {
    "invalid": {"transport": "not an object", "discovery": false, "models": {}},
    "valid": {"transport": {"base_url": "http://valid.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"context_window": 1}}}
  }
}`)
	cat, warnings, err := NewLoader(home, fstest.MapFS{"builtin/base.json": {Data: []byte(`{
      "id": "base",
      "transport": {"base_url": "http://base.test/v1", "api_key_env": ""},
      "discovery": false,
      "models": {}
    }`)}}).Load()
	testassert.NoError(t, err)
	testassert.Require(t, hasWarningForProvider(warnings, "user_config_skip", "invalid"), "warnings = %#v, want user_config_skip for invalid malformed transport", warnings)
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "invalid", Model: "m"})
		testassert.Require(t, err != nil, "malformed transport provider was retained")
	}
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "valid", Model: "m"})
		testassert.NoError(t, err)
	}
}

func TestLoaderTransportBoundRecentFailedAndLegacyRows(t *testing.T) {
	newServer := func(t *testing.T, modelID string) (*httptest.Server, *int) {
		t.Helper()
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"context_window":4096}]}`, modelID)
		}))
		t.Cleanup(server.Close)
		return server, &calls
	}
	providerFS := func(baseURL string) fstest.MapFS {
		return fstest.MapFS{"builtin/remote.json": {Data: []byte(fmt.Sprintf(`{
  "id": "remote",
  "transport": {"base_url": %q, "api_key_env": ""},
  "discovery": true,
  "models": {"known": {"context_window": 1}}
}`, baseURL))}}
	}
	t.Run("recent A then B", func(t *testing.T) {
		home := t.TempDir()
		serverA, callsA := newServer(t, "a-model")
		serverB, callsB := newServer(t, "b-model")
		transportA := Transport{BaseURL: serverA.URL + "/v1"}
		testassert.NoError(t, WriteDiscoveryCache(home, "remote", transportA, DiscoveredProvider{Models: map[string]DiscoveredModel{"a-cached": {ContextWindow: 4096}}}, time.Now().UTC()))
		catA, _, err := NewLoader(home, providerFS(transportA.BaseURL)).Load()
		testassert.NoError(t, err)
		if *callsA != 0 {
			t.Fatalf("recent A discovery calls = %d, want 0", *callsA)
		}
		{
			_, _, err := catA.Lookup(ModelRef{Provider: "remote", Model: "a-cached"})
			testassert.NoError(t, err)
		}
		catB, _, err := NewLoader(home, providerFS(serverB.URL+"/v1")).Load()
		testassert.NoError(t, err)
		if *callsB != 1 {
			t.Fatalf("B discovery calls = %d, want 1 after foreign recent A", *callsB)
		}
		{
			_, _, err := catB.Lookup(ModelRef{Provider: "remote", Model: "b-model"})
			testassert.NoError(t, err)
		}
		{
			_, _, err := catB.Lookup(ModelRef{Provider: "remote", Model: "a-cached"})
			testassert.Require(t, err != nil, "foreign A model survived the B rebuild")
		}
	})
	t.Run("failed A then B", func(t *testing.T) {
		home := t.TempDir()
		serverA, _ := newServer(t, "unused-a")
		serverB, callsB := newServer(t, "b-model")
		transportA := Transport{BaseURL: serverA.URL + "/v1"}
		testassert.NoError(t, WriteDiscoveryAttempt(home, "remote", transportA, time.Now().UTC()))
		{
			_, _, err := NewLoader(home, providerFS(serverB.URL+"/v1")).Load()
			testassert.NoError(t, err)
		}
		if *callsB != 1 {
			t.Fatalf("B discovery calls after failed A = %d, want 1", *callsB)
		}
	})
	t.Run("legacy is unbound", func(t *testing.T) {
		home := t.TempDir()
		server, calls := newServer(t, "b-model")
		writeLoaderFile(t, filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"), `{
  "attempted_at": "`+time.Now().UTC().Format(time.RFC3339)+`",
  "models": {"legacy": {"context_window": 4096}}
}`)
		cat, _, err := NewLoader(home, providerFS(server.URL+"/v1")).Load()
		testassert.NoError(t, err)
		if *calls != 1 {
			t.Fatalf("legacy cache discovery calls = %d, want 1", *calls)
		}
		{
			_, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "legacy"})
			testassert.Require(t, err != nil, "legacy model was used as bound discovery")
		}
	})
}

func TestLoaderFetchesDiscoveryWhenAttemptDueForBundledProvider(t *testing.T) {
	home := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		testassert.Equal(t, r.URL.Path, "/v1/models")
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
	testassert.NoError(t, err)
	testassert.Require(t, !hasNonDiscoveryWarning(warnings), "Load warnings = %#v, want no non-discovery warnings", warnings)
	testassert.Equal(t, calls, 1)
	if _, model, err := catalog.Lookup(ModelRef{Provider: "remote", Model: "remote-model"}); err != nil {
		t.Fatalf("discovered model missing after live Load discovery: %v", err)
	} else if model.Name != "Remote Model" || model.ContextWindow != 16384 || model.MaxOutputTokens != 2048 {
		t.Fatalf("discovered model = %#v", model)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.NoError(t, err)
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
		"transport_fingerprint": "`+transportFingerprint(Transport{BaseURL: server.URL + "/v1"})+`",
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
	testassert.NoError(t, err)
	testassert.Require(t, !hasNonDiscoveryWarning(warnings), "Load warnings = %#v, want no non-discovery warnings", warnings)
	testassert.Equal(t, calls, 0)
	{
		_, _, err := catalog.Lookup(ModelRef{Provider: "remote", Model: "known"})
		testassert.NoError(t, err)
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
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.Require(t, os.IsNotExist(err), "discovery cache stat = %v, want missing cache", err)
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
	testassert.NoError(t, err)
	testassert.Require(t, catalog != nil, "Load returned nil catalog after discovery failure")
	testassert.Require(t, hasWarning(warnings, "discovery_failure"), "Load warnings = %#v, want discovery_failure", warnings)
	_, err = os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "local.json"))
	testassert.NoError(t, err)
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
	testassert.Equal(t, calls, 1)
}

func writeLoaderFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	{
		err := os.WriteFile(path, []byte(content), 0o600)
		testassert.Require(t, err == nil, "WriteFile(%s): %v", path, err)
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

func hasWarningForProvider(warnings []Warning, kind, provider string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind && warning.Provider == provider {
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
	testassert.NoError(t, err)
	testassert.Equal(t, calls, 0)
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"})
		testassert.Require(t, err != nil, "discovered model appeared despite AllowRefresh=false")
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
	testassert.NoError(t, err)
	testassert.Equal(t, calls, 1)
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"})
		testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	testassert.Equal(t, calls, 1)
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "remote", Model: "remote-model"})
		testassert.NoError(t, err)
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
		testassert.Require(t, got[id], "candidate %q missing; got %v", id, candidates)
	}
	testassert.Require(t, !got["no-discovery"], "no-discovery provider should not be a candidate; got %v", candidates)
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
	testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
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
		testassert.Require(t, res.err == nil, "LoadTry under discovery contention = %v, want a catalog plus warnings", res.err)
		testassert.Require(t, res.cat != nil, "LoadTry returned a nil catalog under discovery contention")
		testassert.Require(t, hasWarning(res.warnings, "discovery_failure"), "LoadTry warnings = %#v, want a discovery_failure warning", res.warnings)
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
	testassert.NoError(t, err)
	testassert.Require(t, !hasWarning(warnings, "discovery_failure"), "LoadTry warnings = %#v, want no discovery_failure", warnings)
	{
		_, ok := cat.Providers["remote"].Models["remote-model"]
		testassert.Require(t, ok, "LoadTry did not merge the fetched model: %#v", cat.Providers["remote"].Models)
	}
	{
		_, statErr := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.Require(t, statErr == nil, "LoadTry did not write the discovery cache: %v", statErr)
	}
}
