package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
)

func newProviderManagementAgent(t *testing.T, cfg string) *Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	dotenvPath := filepath.Join(lightcodeDir, ".env")
	if err := os.WriteFile(dotenvPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: loaded, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home, Env: config.NewManagedEnvForTest(dotenvPath)})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func providerStatusByID(t *testing.T, statuses []ProviderStatus, id string) ProviderStatus {
	t.Helper()
	for _, st := range statuses {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("provider %s not found in %#v", id, statuses)
	return ProviderStatus{}
}

func TestProviderListReportsConnectionAndKeySource(t *testing.T) {
	t.Setenv("LIGHTCODE_PROVIDER_EXTERNAL", "external-key")
	a := newProviderManagementAgent(t, `{
  "providers": {
    "managed": {
      "name": "Managed",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_PROVIDER_MANAGED" },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    },
    "external": {
      "name": "External",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_PROVIDER_EXTERNAL" },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    },
    "keyless": {
      "name": "Keyless",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "" },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    },
    "empty": {
      "name": "Empty",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_PROVIDER_EMPTY" },
      "discovery": true,
      "models": {}
    }
  },
  "default_model": ""
}`)
	if err := a.ConnectProvider("managed", "managed-key"); err != nil {
		t.Fatalf("ConnectProvider managed: %v", err)
	}
	managed := providerStatusByID(t, a.ProviderList(), "managed")
	if !managed.Connected || managed.KeySource != ProviderKeySourceManaged || !managed.Disconnectable {
		t.Fatalf("managed status = %#v", managed)
	}
	external := providerStatusByID(t, a.ProviderList(), "external")
	if !external.Connected || external.KeySource != ProviderKeySourceExternal || external.Disconnectable {
		t.Fatalf("external status = %#v", external)
	}
	keyless := providerStatusByID(t, a.ProviderList(), "keyless")
	if !keyless.Connected || keyless.KeySource != ProviderKeySourceKeyless || keyless.Disconnectable {
		t.Fatalf("keyless status = %#v", keyless)
	}
	empty := providerStatusByID(t, a.ProviderList(), "groq")
	if empty.Connected || empty.UsableModels != 0 {
		t.Fatalf("empty status = %#v", empty)
	}
}

func TestConnectBundledEmptyRunsDiscoveryBeforePersistingKey(t *testing.T) {
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","name":"Remote Model","context_window":16384,"max_output_tokens":2048}]}`))
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "groq": {
      "transport": {
        "base_url": "`+server.URL+`/v1",
        "api_key_env": "LIGHTCODE_PROVIDER_EMPTY_DISC",
        "headers": {"Authorization": "Bearer configured-secret"}
      }
    }
  },
  "default_model": ""
}`)
	if err := a.ConnectProvider("groq", "secret"); err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}
	if sawAuth != "Bearer secret" {
		t.Fatalf("discovery Authorization = %q", sawAuth)
	}
	if got := os.Getenv("LIGHTCODE_PROVIDER_EMPTY_DISC"); got != "secret" {
		t.Fatalf("env after connect = %q", got)
	}
	st := providerStatusByID(t, a.ProviderList(), "groq")
	if !st.Connected || st.KeySource != ProviderKeySourceManaged || st.UsableModels != 1 {
		t.Fatalf("status after discovery = %#v", st)
	}
	foundRemote := false
	for _, entry := range a.ModelList() {
		if entry.Ref == "groq/remote-model" {
			foundRemote = true
		}
	}
	if !foundRemote {
		t.Fatalf("ModelList missing groq/remote-model: %#v", a.ModelList())
	}
	cachePath := filepath.Join(a.home, ".lightcode", "cache", "discovery", "groq.json")
	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cacheData), "secret") || strings.Contains(string(cacheData), "Authorization") {
		t.Fatalf("discovery cache persisted provisional credentials: %q", cacheData)
	}
	records, warnings := catalog.ReadDiscoveryCache(a.home)
	if len(warnings) != 0 {
		t.Fatalf("discovery cache warnings = %#v", warnings)
	}
	record, ok := records["groq"]
	if !ok {
		t.Fatalf("discovery cache missing groq record: %#v", records)
	}
	configured := a.catalog.Providers["groq"].Transport
	if !record.BoundTo(configured) {
		t.Fatal("discovery cache was not bound to the captured configured Authorization")
	}
	provisional := configured
	provisional.Headers = map[string]string{"Authorization": "Bearer secret"}
	if record.BoundTo(provisional) {
		t.Fatal("provisional Authorization injection changed the captured transport identity")
	}
}

func TestConnectProviderRejectsConfiguredTransportChange(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	releaseGate := func() { release.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":4096}]}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseGate)
	keyEnv := "LIGHTCODE_TRANSPORT_CHANGE_KEY"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, `{
  "providers": {
    "disc": {
      "transport": { "base_url": "`+server.URL+`/v1", "api_key_env": "`+keyEnv+`" },
      "discovery": true,
      "models": {}
    }
  }
}`)
	done := make(chan error, 1)
	go func() { done <- a.ConnectProvider("disc", "secret") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectProvider did not reach the gated discovery request")
	}
	changedConfig := `{
  "providers": {
    "disc": {
      "transport": { "base_url": "http://changed.test/v1", "api_key_env": "` + keyEnv + `" },
      "discovery": true,
      "models": {}
    }
  }
}`
	if err := os.WriteFile(a.configPath, []byte(changedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("reload changed transport: %v", err)
	}
	catalogAfterChange := a.catalog
	releaseGate()
	select {
	case err := <-done:
		if err == nil || err.Error() != "provider disc changed while connecting; retry" {
			t.Fatalf("ConnectProvider after transport change = %v, want exact retry error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectProvider did not finish after releasing discovery")
	}
	if authorization != "Bearer secret" {
		t.Fatalf("provisional Authorization = %q, want Bearer secret", authorization)
	}
	if a.catalog != catalogAfterChange {
		t.Fatal("transport-mismatch rejection performed a final reload")
	}
	if _, ok := os.LookupEnv(keyEnv); ok {
		t.Fatal("transport-mismatch rejection persisted the API key")
	}
	if records, _ := catalog.ReadDiscoveryCache(a.home); len(records) != 0 {
		t.Fatalf("transport-mismatch rejection wrote discovery records: %#v", records)
	}
}

func TestConnectProviderRevalidatesNonDiscoveryTransportBeforePublication(t *testing.T) {
	const keyEnv = "LIGHTCODE_NON_DISCOVERY_REVALIDATION_KEY"
	for _, tc := range []struct {
		name       string
		remove     bool
		wantErr    string
		mutateLive func(*catalog.Provider)
	}{
		{name: "changed transport", wantErr: "provider managed changed while connecting; retry", mutateLive: func(prov *catalog.Provider) {
			prov.Transport.BaseURL = "http://changed.test/v1"
		}},
		{name: "provider removed", remove: true, wantErr: `provider "managed" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Unsetenv(keyEnv)
			t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
			a := newProviderManagementAgent(t, `{
  "providers": {
    "managed": {
      "transport": {"base_url": "http://configured.test/v1", "api_key_env": "`+keyEnv+`"},
      "discovery": false,
      "models": {"m": {"context_window": 1000}}
    }
  }
}`)
			catalogBefore := a.catalog
			cfgBefore := a.cfg
			a.connectProviderBeforeFinalLock = func() {
				if tc.remove {
					delete(a.catalog.Providers, "managed")
					return
				}
				tc.mutateLive(a.catalog.Providers["managed"])
			}
			err := a.ConnectProvider("managed", "secret")
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("ConnectProvider error = %v, want %q", err, tc.wantErr)
			}
			if _, ok := os.LookupEnv(keyEnv); ok {
				t.Fatal("final revalidation failure persisted API key")
			}
			if records, warnings := catalog.ReadDiscoveryCache(a.home); len(records) != 0 || len(warnings) != 0 {
				t.Fatalf("final revalidation failure wrote discovery state: records=%#v warnings=%#v", records, warnings)
			}
			if a.catalog != catalogBefore || a.cfg != cfgBefore {
				t.Fatal("final revalidation failure reloaded catalog/config")
			}
		})
	}
}

func TestConnectProviderRejectsRemovedDiscoveryProviderBeforePublication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":4096}]}`))
	}))
	t.Cleanup(server.Close)
	const keyEnv = "LIGHTCODE_DISCOVERY_REVALIDATION_KEY"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, `{
  "providers": {
    "disc": {
      "transport": {"base_url": "`+server.URL+`/v1", "api_key_env": "`+keyEnv+`"},
      "discovery": true,
      "models": {}
    }
  }
}`)
	catalogBefore := a.catalog
	a.connectProviderBeforeFinalLock = func() { delete(a.catalog.Providers, "disc") }
	err := a.ConnectProvider("disc", "secret")
	if err == nil || err.Error() != `provider "disc" not found` {
		t.Fatalf("ConnectProvider error = %v, want provider-not-found", err)
	}
	if _, ok := os.LookupEnv(keyEnv); ok {
		t.Fatal("discovery provider removal failure persisted API key")
	}
	if records, warnings := catalog.ReadDiscoveryCache(a.home); len(records) != 0 || len(warnings) != 0 {
		t.Fatalf("discovery provider removal failure wrote state: records=%#v warnings=%#v", records, warnings)
	}
	if a.catalog != catalogBefore {
		t.Fatal("discovery provider removal failure reloaded catalog")
	}
}

func TestConnectBundledEmptyFailurePersistsNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "groq": {
      "transport": { "base_url": "`+server.URL+`/v1", "api_key_env": "LIGHTCODE_PROVIDER_EMPTY_FAIL" }
    }
  },
  "default_model": ""
}`)
	if err := a.ConnectProvider("groq", "secret"); err == nil {
		t.Fatal("ConnectProvider returned nil for failed discovery")
	}
	if got := os.Getenv("LIGHTCODE_PROVIDER_EMPTY_FAIL"); got != "" {
		t.Fatalf("env persisted on failure: %q", got)
	}
	st := providerStatusByID(t, a.ProviderList(), "groq")
	if st.Connected || st.KeySource != ProviderKeySourceNone {
		t.Fatalf("status after failed discovery = %#v", st)
	}
}

func TestConnectBundledEmptyRejectsUnusableDiscovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"incomplete"}]}`))
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "groq": {
      "transport": { "base_url": "`+server.URL+`/v1", "api_key_env": "LIGHTCODE_PROVIDER_EMPTY_UNUSABLE" }
    }
  },
  "default_model": ""
}`)
	err := a.ConnectProvider("groq", "secret")
	if err == nil || !strings.Contains(err.Error(), "no usable models") {
		t.Fatalf("ConnectProvider err = %v, want unusable discovery error", err)
	}
	if got := os.Getenv("LIGHTCODE_PROVIDER_EMPTY_UNUSABLE"); got != "" {
		t.Fatalf("env persisted on unusable discovery: %q", got)
	}
	st := providerStatusByID(t, a.ProviderList(), "groq")
	if st.Connected || st.KeySource != ProviderKeySourceNone {
		t.Fatalf("status after unusable discovery = %#v", st)
	}
}

func TestConnectBundledEmptyExternalKeyWritesOnlyCache(t *testing.T) {
	t.Setenv("LIGHTCODE_PROVIDER_EMPTY_EXTERNAL", "external-secret")
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","context_window":16384}]}`))
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "groq": {
      "transport": { "base_url": "`+server.URL+`/v1", "api_key_env": "LIGHTCODE_PROVIDER_EMPTY_EXTERNAL" }
    }
  },
  "default_model": ""
}`)
	if err := a.ConnectProvider("groq", "ignored"); err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}
	if sawAuth != "Bearer external-secret" {
		t.Fatalf("discovery Authorization = %q", sawAuth)
	}
	if a.ManagedEnv().IsManaged("LIGHTCODE_PROVIDER_EMPTY_EXTERNAL") {
		t.Fatal("external key marked managed")
	}
	st := providerStatusByID(t, a.ProviderList(), "groq")
	if !st.Connected || st.KeySource != ProviderKeySourceExternal || st.Disconnectable {
		t.Fatalf("external status = %#v", st)
	}
}

func TestDiscoverAndAddCustomProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"custom-model","name":"Custom Model","context_window":32000,"max_output_tokens":4096}]}`))
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	usage := false
	candidates, err := a.DiscoverCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: server.URL + "/v1", APIKey: "transient"})
	if err != nil {
		t.Fatalf("DiscoverCustomProvider: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != "custom-model" || !candidates[0].Usable {
		t.Fatalf("candidates = %#v", candidates)
	}
	if _, ok := a.catalog.Providers["custom"]; ok {
		t.Fatalf("discovery persisted custom provider")
	}
	if err := a.AddCustomProvider(CustomProviderRequest{
		ID:             "custom",
		Name:           "Custom",
		BaseURL:        server.URL + "/v1",
		APIKeyEnv:      "LIGHTCODE_CUSTOM_KEY",
		APIKey:         "custom-secret",
		Headers:        map[string]string{"HTTP-Referer": "https://lightcode.dev"},
		Options:        map[string]any{"region": "local"},
		SystemRole:     "developer",
		UsageInStream:  &usage,
		MaxTokensField: "max_completion_tokens",
		ExtraBody:      map[string]any{"reasoning_effort": "low"},
		Hidden:         true,
		Discovery:      true,
		Models: []CustomProviderModelInput{{
			ID:              "custom-model",
			Name:            "Custom Model",
			ContextWindow:   32000,
			MaxOutputTokens: 4096,
			InputModalities: []catalog.Modality{catalog.ModalityText},
			SystemRole:      "developer",
			UsageInStream:   &usage,
			ExtraBody:       map[string]any{"temperature": 0},
		}},
	}); err != nil {
		t.Fatalf("AddCustomProvider: %v", err)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if !st.Connected || st.KeySource != ProviderKeySourceManaged || st.Removable {
		t.Fatalf("custom status = %#v", st)
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "custom-secret") {
		t.Fatalf("secret leaked into config: %s", string(data))
	}
	providers := root["providers"].(map[string]any)
	custom := providers["custom"].(map[string]any)
	transport := custom["transport"].(map[string]any)
	if transport["headers"] == nil || transport["options"] == nil || custom["system_role"] != "developer" || custom["max_tokens_field"] != "max_completion_tokens" || custom["extra_body"] == nil || custom["hidden"] != true {
		t.Fatalf("custom provider fields not preserved: %#v", custom)
	}
	models := custom["models"].(map[string]any)
	model := models["custom-model"].(map[string]any)
	if model["input_modalities"] == nil || model["system_role"] != "developer" || model["extra_body"] == nil {
		t.Fatalf("custom model fields not preserved: %#v", model)
	}
}

func TestAddCustomProviderExternalKeyDoesNotPersistSecret(t *testing.T) {
	t.Setenv("LIGHTCODE_CUSTOM_EXTERNAL_KEY", "external-secret")
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	if err := a.AddCustomProvider(CustomProviderRequest{
		ID:        "custom",
		Name:      "Custom",
		BaseURL:   "http://127.0.0.1:9/v1",
		APIKeyEnv: "LIGHTCODE_CUSTOM_EXTERNAL_KEY",
		APIKey:    "ignored",
		Models:    []CustomProviderModelInput{{ID: "m", ContextWindow: 1000}},
	}); err != nil {
		t.Fatalf("AddCustomProvider: %v", err)
	}
	if a.ManagedEnv().IsManaged("LIGHTCODE_CUSTOM_EXTERNAL_KEY") {
		t.Fatal("external key marked managed")
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if !st.Connected || st.KeySource != ProviderKeySourceExternal {
		t.Fatalf("custom external status = %#v", st)
	}
}

func TestDiscoverCustomProviderPrefersExternalKey(t *testing.T) {
	t.Setenv("LIGHTCODE_DISCOVER_EXTERNAL_KEY", "external-secret")
	var sawAuth, sawReferer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawReferer = r.Header.Get("HTTP-Referer")
		_, _ = w.Write([]byte(`{"data":[{"id":"custom-model","context_window":32000}]}`))
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	if _, err := a.DiscoverCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: server.URL + "/v1", APIKeyEnv: "LIGHTCODE_DISCOVER_EXTERNAL_KEY", APIKey: "typed-secret", Headers: map[string]string{"HTTP-Referer": "https://lightcode.dev"}}); err != nil {
		t.Fatalf("DiscoverCustomProvider: %v", err)
	}
	if sawAuth != "Bearer external-secret" {
		t.Fatalf("discovery Authorization = %q, want external key", sawAuth)
	}
	if sawReferer != "https://lightcode.dev" {
		t.Fatalf("discovery HTTP-Referer = %q", sawReferer)
	}
}

func TestAddCustomProviderRejectsDuplicateAPIKeyEnv(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "existing": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_DUPLICATE_KEY" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	err := a.AddCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "LIGHTCODE_DUPLICATE_KEY", APIKey: "secret", Models: []CustomProviderModelInput{{ID: "m", ContextWindow: 1000}}})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("AddCustomProvider err = %v, want duplicate api_key_env error", err)
	}
}

func TestCustomProviderRejectsAuthorizationHeader(t *testing.T) {
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	req := CustomProviderRequest{ID: "custom", BaseURL: "http://127.0.0.1:9/v1", Headers: map[string]string{"Authorization": "Bearer secret"}, APIKey: "safe-secret", Models: []CustomProviderModelInput{{ID: "m", ContextWindow: 1000}}}
	if _, err := a.DiscoverCustomProvider(req); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("DiscoverCustomProvider err = %v, want forbidden header error", err)
	}
	if err := a.AddCustomProvider(req); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("AddCustomProvider err = %v, want forbidden header error", err)
	}
}

func TestCustomProviderExternalEmptyKeyErrors(t *testing.T) {
	t.Setenv("LIGHTCODE_CUSTOM_EMPTY_EXTERNAL", "")
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	err := a.AddCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "LIGHTCODE_CUSTOM_EMPTY_EXTERNAL", APIKey: "typed-secret", Models: []CustomProviderModelInput{{ID: "m", ContextWindow: 1000}}})
	if err == nil || !strings.Contains(err.Error(), "externally set but empty") {
		t.Fatalf("AddCustomProvider err = %v, want empty external key error", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"custom-model","context_window":32000}]}`))
	}))
	t.Cleanup(server.Close)
	_, err = a.DiscoverCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: server.URL + "/v1", APIKeyEnv: "LIGHTCODE_CUSTOM_EMPTY_EXTERNAL", APIKey: "typed-secret"})
	if err == nil || !strings.Contains(err.Error(), "externally set but empty") {
		t.Fatalf("DiscoverCustomProvider err = %v, want empty external key error", err)
	}
}

func TestAddCustomProviderRequiresUsableModel(t *testing.T) {
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	err := a.AddCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: "http://127.0.0.1:9/v1", Models: []CustomProviderModelInput{{ID: "bad"}}})
	if err == nil || !strings.Contains(err.Error(), "usable model") {
		t.Fatalf("AddCustomProvider err = %v, want usable model error", err)
	}
}

func TestAddCustomProviderRejectsDuplicateModelID(t *testing.T) {
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	err := a.AddCustomProvider(CustomProviderRequest{ID: "custom", BaseURL: "http://127.0.0.1:9/v1", Models: []CustomProviderModelInput{{ID: "dup", ContextWindow: 1000}, {ID: "dup"}}})
	if err == nil || !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("AddCustomProvider err = %v, want duplicate model id error", err)
	}
}

func TestDisconnectAndRemoveProvider(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "custom": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_REMOVE_KEY" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": "custom/m"
}`)
	if err := a.ConnectProvider("custom", "secret"); err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}
	if err := a.SwitchModel("custom/m"); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if err := a.DisconnectProvider("custom"); err != nil {
		t.Fatalf("DisconnectProvider: %v", err)
	}
	if got := os.Getenv("LIGHTCODE_REMOVE_KEY"); got != "" {
		t.Fatalf("env after disconnect = %q", got)
	}
	if cur := a.CurrentModel(); cur.Ref != "" {
		t.Fatalf("current model after disconnect = %#v", cur)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if st.Connected || st.KeySource != ProviderKeySourceNone {
		t.Fatalf("status after disconnect = %#v", st)
	}
	if err := a.RemoveProvider("custom"); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	for _, st := range a.ProviderList() {
		if st.ID == "custom" {
			t.Fatalf("custom provider still listed: %#v", a.ProviderList())
		}
	}
}

func TestDisconnectExternalKeyRefusedAndBundledRemoveRefused(t *testing.T) {
	t.Setenv("LIGHTCODE_EXTERNAL_REFUSE", "external")
	a := newProviderManagementAgent(t, `{
  "providers": {
    "external": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_EXTERNAL_REFUSE" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	err := a.DisconnectProvider("external")
	if err == nil || !strings.Contains(err.Error(), "connected via environment") {
		t.Fatalf("DisconnectProvider err = %v", err)
	}
	if err := a.RemoveProvider("openai"); err == nil {
		t.Fatal("RemoveProvider(openai) returned nil")
	}
}

func TestGenerateAPIKeyEnvNameUnique(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "foo": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_NEW_PROVIDER_API_KEY" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	got := a.GenerateAPIKeyEnvName("new-provider")
	if got != "LIGHTCODE_NEW_PROVIDER_API_KEY_2" {
		t.Fatalf("GenerateAPIKeyEnvName = %q", got)
	}
}

func TestConnectProviderExternalDoesNotOverwrite(t *testing.T) {
	t.Setenv("LIGHTCODE_EXISTING_EXTERNAL", "external")
	a := newProviderManagementAgent(t, `{
  "providers": {
    "external": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_EXISTING_EXTERNAL" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	if err := a.ConnectProvider("external", "new-secret"); err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}
	if got := os.Getenv("LIGHTCODE_EXISTING_EXTERNAL"); got != "external" {
		t.Fatalf("external key overwritten: %q", got)
	}
	if a.ManagedEnv().IsManaged("LIGHTCODE_EXISTING_EXTERNAL") {
		t.Fatal("external key marked managed")
	}
}

func TestConnectProviderMissingKeyErrors(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "missing": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_MISSING_CONNECT_KEY" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	err := a.ConnectProvider("missing", "")
	if err == nil || !strings.Contains(err.Error(), "LIGHTCODE_MISSING_CONNECT_KEY") {
		t.Fatalf("ConnectProvider err = %v, want missing key error", err)
	}
}

func TestConnectProviderExternalEmptyKeyErrors(t *testing.T) {
	t.Setenv("LIGHTCODE_EMPTY_EXTERNAL_KEY", "")
	a := newProviderManagementAgent(t, `{
  "providers": {
    "external-empty": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_EMPTY_EXTERNAL_KEY" },
      "models": { "m": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`)
	err := a.ConnectProvider("external-empty", "new-secret")
	if err == nil || !strings.Contains(err.Error(), "externally set but empty") {
		t.Fatalf("ConnectProvider err = %v, want empty external key error", err)
	}
	if got := os.Getenv("LIGHTCODE_EMPTY_EXTERNAL_KEY"); got != "" {
		t.Fatalf("external env overwritten: %q", got)
	}
	if a.ManagedEnv().IsManaged("LIGHTCODE_EMPTY_EXTERNAL_KEY") {
		t.Fatal("empty external key marked managed")
	}
}

func TestManagedEnvSetErrorStillExternalFriendly(t *testing.T) {
	// Regression guard for the error type used by ConnectProvider's external path.
	if !errors.Is(config.ErrExternalKey, config.ErrExternalKey) {
		t.Fatal("ErrExternalKey should compare with errors.Is")
	}
}

func TestAddCustomProviderAdvancedModelFieldsPersisted(t *testing.T) {
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)
	usageFalse := false
	if err := a.AddCustomProvider(CustomProviderRequest{
		ID:        "adv",
		Name:      "Advanced",
		BaseURL:   "http://127.0.0.1:9/v1",
		APIKeyEnv: "LIGHTCODE_ADV_KEY",
		APIKey:    "secret",
		Models: []CustomProviderModelInput{{
			ID:            "m1",
			ContextWindow: 2000,
			SystemRole:    "developer",
			UsageInStream: &usageFalse,
			Hidden:        true,
		}},
	}); err != nil {
		t.Fatalf("AddCustomProvider: %v", err)
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	providers := root["providers"].(map[string]any)
	adv := providers["adv"].(map[string]any)
	models := adv["models"].(map[string]any)
	m1 := models["m1"].(map[string]any)
	if m1["system_role"] != "developer" {
		t.Fatalf("model system_role = %v, want developer", m1["system_role"])
	}
	if m1["usage_in_stream"] != false {
		t.Fatalf("model usage_in_stream = %v, want false", m1["usage_in_stream"])
	}
	if m1["hidden"] != true {
		t.Fatalf("model hidden = %v, want true", m1["hidden"])
	}
}

func TestConnectProviderDiscoveryTimeoutReturnsPromptly(t *testing.T) {
	// A stalled discovery endpoint must return within the bounded timeout, not
	// hang indefinitely. We override discoveryTimeout via a short-lived server
	// that never responds and verify the call fails fast.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond — simulate a stalled endpoint.
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "stalled": {
      "transport": { "base_url": "`+server.URL+`/v1", "api_key_env": "LIGHTCODE_STALLED_KEY" }
    }
  },
  "default_model": ""
}`)
	// Temporarily shorten the timeout for this test.
	origClient := discoveryHTTPClient
	discoveryHTTPClient = &http.Client{Timeout: 500 * time.Millisecond}
	t.Cleanup(func() { discoveryHTTPClient = origClient })

	start := time.Now()
	err := a.ConnectProvider("stalled", "secret")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ConnectProvider returned nil for stalled discovery")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("ConnectProvider took %v, want bounded timeout", elapsed)
	}
}

func TestDiscoverCustomProviderDoesNotHoldRuntimeLockDuringFetch(t *testing.T) {
	// Prove the runtime lock is released during the network fetch: a slow
	// discovery server must not block a concurrent ProviderList call.
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(gate) })
	a := newProviderManagementAgent(t, `{"providers": {}, "default_model": ""}`)

	// Start discovery in a goroutine; it will block on the server gate.
	done := make(chan error, 1)
	go func() {
		_, err := a.DiscoverCustomProvider(CustomProviderRequest{ID: "slow", BaseURL: server.URL + "/v1", APIKey: "k"})
		done <- err
	}()

	// Give the goroutine time to enter the network call.
	time.Sleep(50 * time.Millisecond)

	// ProviderList must return promptly even while discovery is in flight.
	listDone := make(chan struct{})
	go func() {
		_ = a.ProviderList()
		close(listDone)
	}()
	select {
	case <-listDone:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("ProviderList blocked while DiscoverCustomProvider held the lock")
	}

	// Unblock the discovery server.
	gate <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DiscoverCustomProvider: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DiscoverCustomProvider did not return after unblocking server")
	}
}
