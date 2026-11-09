package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestLoaderAssembleCapturedUsesCapturedLayerAndCache(t *testing.T) {
	cacheDir := t.TempDir()
	fsys := fstest.MapFS{"builtin/base.json": {Data: []byte(`{
		"id": "base",
		"transport": {"base_url": "http://base.test/v1", "api_key_env": ""},
		"discovery": true,
		"models": {"known": {"context_window": 1000}}
	}`)}}
	userRaw := map[string]any{"local": map[string]any{
		"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
		"discovery": false,
		"models":    map[string]any{"known": map[string]any{"context_window": json.Number("9007199254740993")}},
	}}
	testassert.NoError(t, WriteDiscoveryCache(cacheDir, "base", Transport{BaseURL: "http://base.test/v1"}, DiscoveredProvider{
		Models: map[string]DiscoveredModel{"discovered": {ContextWindow: 4096}},
	}, time.Now().Add(-time.Hour).UTC()))

	records, w := ReadDiscoveryCache(cacheDir)
	t.Logf("records=%#v warnings=%#v bound=%v", records, w, records["base"].BoundTo(Transport{BaseURL: "http://base.test/v1"}))
	result, err := NewLoader(cacheDir, fsys).AssembleCaptured(context.Background(), userRaw)
	testassert.NoError(t, err)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "base", Model: "known"})
		testassert.NoError(t, err)
	}
	{
		_, model, err := result.Catalog.Lookup(ModelRef{Provider: "base", Model: "discovered"})
		testassert.NoError(t, err)
		testassert.Require(t, model.ContextWindow == 4096, "cached model = %+v, want the explicit cache input", model)
	}
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"}); err != nil {
		t.Fatalf("captured provider missing: %v", err)
	} else if model.ContextWindow != 9007199254740993 {
		t.Fatalf("captured model = %#v, want the exact number preserved from the captured input", model)
	}
	{
		_, err := os.Stat(filepath.Join(cacheDir, "config.json"))
		testassert.Require(t, os.IsNotExist(err), "the loader read or created a main configuration: %v", err)
	}
}

func TestLoaderEmptyCacheDirDisablesRefresh(t *testing.T) {
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}
	loader := NewLoader("", fsys)
	built, err := loader.AssembleCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	refreshed, err := loader.RefreshCaptured(context.Background(), nil, built, nil)
	testassert.NoError(t, err)
	testassert.Require(t, fetches == 0, "empty cache dir refreshed %d providers", fetches)
	testassert.Require(t, len(refreshed.Catalog.Providers["remote"].Models) == 0, "empty cache dir produced models: %v", refreshed.Catalog.Providers["remote"].Models)
}

func TestLoaderRefreshCapturedUsesCapturedCredentials(t *testing.T) {
	cacheDir := t.TempDir()
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": "REMOTE_CAPTURE_KEY"},
		"discovery": true,
		"models": {}
	}`)}}
	loader := NewLoader(cacheDir, fsys)
	built, err := loader.AssembleCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	refreshed, err := loader.RefreshCaptured(context.Background(), nil, built, map[string]string{"REMOTE_CAPTURE_KEY": "captured-secret"})
	testassert.NoError(t, err)
	testassert.Require(t, auth == "Bearer captured-secret", "discovery Authorization = %q, want the captured credential", auth)
	{
		_, model, err := refreshed.Catalog.Lookup(ModelRef{Provider: "remote", Model: "m"})
		testassert.NoError(t, err)
		testassert.Require(t, model.ContextWindow == 1000, "refreshed model = %+v", model)
	}
	{
		_, err := os.Stat(filepath.Join(cacheDir, "remote.json"))
		testassert.NoError(t, err)
	}
}

func TestLoaderRefreshCapturedMissingCredentialSkipsFetch(t *testing.T) {
	cacheDir := t.TempDir()
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": "REMOTE_MISSING_KEY"},
		"discovery": true,
		"models": {}
	}`)}}
	loader := NewLoader(cacheDir, fsys)
	built, err := loader.AssembleCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	_, err = loader.RefreshCaptured(context.Background(), nil, built, nil)
	testassert.NoError(t, err)
	testassert.Require(t, fetches == 0, "missing captured credential still fetched %d times", fetches)
}

func TestLoaderRefreshCapturedAllowRefreshGate(t *testing.T) {
	cacheDir := t.TempDir()
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	fsys := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}
	loader := NewLoader(cacheDir, fsys)
	loader.AllowRefresh = func(string, *Provider) bool { return false }
	built, err := loader.AssembleCaptured(context.Background(), nil)
	testassert.NoError(t, err)
	_, err = loader.RefreshCaptured(context.Background(), nil, built, nil)
	testassert.NoError(t, err)
	testassert.Require(t, fetches == 0, "AllowRefresh=false still fetched %d times", fetches)
}

func writeLoaderFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	testassert.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// hasWarningForProvider reports one warning of kind for provider.
func hasWarningForProvider(warnings []Warning, kind, provider string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind && warning.Provider == provider {
			return true
		}
	}
	return false
}
