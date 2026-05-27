package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

func newCatalogBackedTestAgent(t *testing.T) *Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "alt-model": { "name": "Alt Model", "context_window": 4096, "max_output_tokens": 512 },
        "hidden-model": { "name": "Hidden Model", "context_window": 2048, "max_output_tokens": 256, "hidden": true }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func TestAgentCurrentModelUsesCatalogDefaultRef(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	cur := a.CurrentModel()

	if cur.Ref != "test/test-model" || cur.Provider != "test" || cur.Model != "test-model" {
		t.Fatalf("CurrentModel identity = %#v, want test/test-model", cur)
	}
	if cur.DisplayName != "Test Model" || cur.ContextWindow != 8192 {
		t.Fatalf("CurrentModel metadata = %#v, want display/context from catalog", cur)
	}
}

func TestAgentSwitchModelTakesPrefixRef(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("SwitchModel returned error: %v", err)
	}
	cur := a.CurrentModel()
	if cur.Ref != "test/alt-model" || cur.Model != "alt-model" || cur.DisplayName != "Alt Model" || cur.ContextWindow != 4096 {
		t.Fatalf("CurrentModel after switch = %#v, want alt-model metadata", cur)
	}
}

func TestAgentModelListReturnsVisibleFlatCatalogEntries(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	entries := a.ModelList()

	byRef := map[string]ModelListEntry{}
	for _, entry := range entries {
		byRef[entry.Ref] = entry
	}
	if _, ok := byRef["test/hidden-model"]; ok {
		t.Fatalf("hidden model appeared in ModelList: %#v", byRef["test/hidden-model"])
	}
	entry, ok := byRef["test/test-model"]
	if !ok {
		t.Fatalf("ModelList missing test/test-model; entries=%#v", entries)
	}
	if entry.Provider != "test" || entry.ProviderName != "Test Provider" || entry.Model != "test-model" || entry.DisplayName != "Test Model" {
		t.Fatalf("ModelList entry identity/display = %#v", entry)
	}
	if entry.ContextWindow != 8192 || entry.Incomplete {
		t.Fatalf("ModelList entry completeness = %#v, want complete 8192 context", entry)
	}
}

func TestAgentSwitchModelUnknownModelDoesNotRunDiscovery(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"remote-model","name":"Remote Model","context_window":12288,"max_output_tokens":3072}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	delete(a.catalog.Providers["test"].Models, "remote-model")

	if err := a.SwitchModel("test/remote-model"); err == nil {
		t.Fatal("SwitchModel returned nil for unknown model")
	}
	if calls != 0 {
		t.Fatalf("discovery calls = %d, want 0", calls)
	}
}

func TestAgentRefreshDiscoveryUpdatesCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fresh-model","name":"Fresh Model","context_window":24576,"max_output_tokens":4096}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	a.catalog.Providers["test"].Builtin = true

	if err := a.RefreshDiscovery("test"); err != nil {
		t.Fatalf("RefreshDiscovery returned error: %v", err)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/fresh-model" {
			if entry.DisplayName != "Fresh Model" || entry.ContextWindow != 24576 || entry.Incomplete {
				t.Fatalf("fresh model entry = %#v", entry)
			}
			return
		}
	}
	t.Fatalf("ModelList missing test/fresh-model after RefreshDiscovery; entries=%#v", entries)
}

func TestAgentRefreshDiscoveryPreservesUserCostFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"test-model","name":"Discovered Test Model","context_window":24576,"max_output_tokens":4096,"cost":{"input":1,"output":2}}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": {
          "name": "Test Model",
          "context_window": 8192,
          "max_output_tokens": 1024,
          "cost": {"input": 99}
        }
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	a.catalog.Providers["test"].Builtin = true

	if err := a.RefreshDiscovery("test"); err != nil {
		t.Fatalf("RefreshDiscovery returned error: %v", err)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref != "test/test-model" {
			continue
		}
		if entry.Cost == nil {
			t.Fatalf("test-model cost = nil")
		}
		if entry.Cost.Output == nil || *entry.Cost.Output != 2 {
			t.Fatalf("test-model output cost = %v, want discovered value 2", entry.Cost.Output)
		}
		if entry.Cost.Input == nil || *entry.Cost.Input != 99 {
			t.Fatalf("test-model input cost = %v, want user value 99", entry.Cost.Input)
		}
		return
	}
	t.Fatalf("ModelList missing test/test-model after RefreshDiscovery; entries=%#v", entries)
}

func TestAgentRefreshDiscoveryFailureEmitsWarningEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	var warningEvent *Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventWarning {
			copy := ev
			warningEvent = &copy
		}
	})

	if err := a.RefreshDiscovery("test"); err == nil {
		t.Fatal("RefreshDiscovery returned nil error for HTTP 500")
	}
	if warningEvent == nil || len(warningEvent.Warnings) == 0 {
		t.Fatalf("RefreshDiscovery did not emit EventWarning; event=%#v", warningEvent)
	}
	if warningEvent.Warnings[0].Kind != "catalog_discovery_failure" {
		t.Fatalf("warning = %#v, want catalog_discovery_failure", warningEvent.Warnings[0])
	}
}

func TestAgentModelListIsSafeDuringRefreshDiscovery(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	configureChangingDiscoveryServer(t, a)

	runConcurrentDiscoveryReaders(t, a, func() {
		_ = a.ModelList()
	})
}

func TestAgentCurrentModelIsSafeDuringRefreshDiscovery(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	configureChangingDiscoveryServer(t, a)

	runConcurrentDiscoveryReaders(t, a, func() {
		_ = a.CurrentModel()
	})
}

func TestAgentReloadRebuildsCatalogAndFallsBackToDefault(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("SwitchModel returned error: %v", err)
	}
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider Reloaded",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model Reloaded", "context_window": 16384, "max_output_tokens": 2048 },
        "new-model": { "name": "New Model", "context_window": 32768, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)

	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}

	cur := a.CurrentModel()
	if cur.Ref != "test/test-model" || cur.DisplayName != "Test Model Reloaded" || cur.ContextWindow != 16384 {
		t.Fatalf("CurrentModel after Reload = %#v, want fallback to reloaded default", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/new-model" && entry.DisplayName == "New Model" && entry.ContextWindow == 32768 {
			return
		}
	}
	t.Fatalf("ModelList missing test/new-model after Reload; entries=%#v", entries)
}

func TestAgentReloadRefusesWhileBusy(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()

	if err := a.Reload(); err == nil {
		t.Fatal("Reload returned nil while busy")
	}
}

func TestAgentCompleteModelEntryWritesConfigAndReloadsCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "incomplete-model": { "name": "Incomplete Model" }
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload before completion returned error: %v", err)
	}

	if err := a.CompleteModelEntry("test/incomplete-model", ModelCompletion{ContextWindow: 65536, MaxOutputTokens: 8192}); err != nil {
		t.Fatalf("CompleteModelEntry returned error: %v", err)
	}

	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/incomplete-model" {
			if entry.Incomplete || entry.ContextWindow != 65536 {
				t.Fatalf("completed model entry = %#v, want complete 65536 context", entry)
			}
			return
		}
	}
	t.Fatalf("ModelList missing test/incomplete-model after CompleteModelEntry; entries=%#v", entries)
}

func TestAgentEnsureSessionReloadsExternalConfigEdit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Edited Model", "context_window": 24576, "max_output_tokens": 2048 },
        "new-session-model": { "name": "New Session Model", "context_window": 49152, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)

	if _, err := a.AppendUserMessage("hello"); err != nil {
		t.Fatalf("AppendUserMessage returned error: %v", err)
	}

	cur := a.CurrentModel()
	if cur.DisplayName != "Edited Model" || cur.ContextWindow != 24576 {
		t.Fatalf("CurrentModel after session-start reload = %#v, want edited model", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/new-session-model" {
			return
		}
	}
	t.Fatalf("ModelList missing test/new-session-model after session-start reload; entries=%#v", entries)
}

func TestAgentSessionSwitchReloadsExternalConfigEdit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.AppendUserMessage("first"); err != nil {
		t.Fatalf("AppendUserMessage first returned error: %v", err)
	}
	firstID := a.SessionCurrent().ID
	if firstID == "" {
		t.Fatal("first session id is empty")
	}
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew returned error: %v", err)
	}
	if _, err := a.AppendUserMessage("second"); err != nil {
		t.Fatalf("AppendUserMessage second returned error: %v", err)
	}
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Switched Model", "context_window": 28672, "max_output_tokens": 2048 },
        "switch-model": { "name": "Switch Model", "context_window": 57344, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)

	if err := a.SessionSwitch(firstID); err != nil {
		t.Fatalf("SessionSwitch returned error: %v", err)
	}

	cur := a.CurrentModel()
	if cur.DisplayName != "Switched Model" || cur.ContextWindow != 28672 {
		t.Fatalf("CurrentModel after SessionSwitch reload = %#v, want switched model", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/switch-model" {
			return
		}
	}
	t.Fatalf("ModelList missing test/switch-model after SessionSwitch reload; entries=%#v", entries)
}

func TestAgentAllModelListIncludesHiddenModels(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	entries := a.AllModelList()

	byRef := map[string]ModelListEntry{}
	for _, entry := range entries {
		byRef[entry.Ref] = entry
	}
	hiddenEntry, ok := byRef["test/hidden-model"]
	if !ok {
		t.Fatalf("AllModelList missing test/hidden-model; entries=%#v", entries)
	}
	if !hiddenEntry.Hidden {
		t.Fatalf("hidden-model entry Hidden = %v, want true", hiddenEntry.Hidden)
	}
	testEntry, ok := byRef["test/test-model"]
	if !ok {
		t.Fatalf("AllModelList missing test/test-model; entries=%#v", entries)
	}
	if testEntry.Hidden {
		t.Fatalf("test-model entry Hidden = %v, want false", testEntry.Hidden)
	}
	if testEntry.Provider != "test" || testEntry.ProviderName != "Test Provider" || testEntry.Model != "test-model" || testEntry.DisplayName != "Test Model" {
		t.Fatalf("test-model entry identity/display = %#v", testEntry)
	}
	if testEntry.ContextWindow != 8192 {
		t.Fatalf("test-model entry ContextWindow = %d, want 8192", testEntry.ContextWindow)
	}
}

func TestAgentSetModelHiddenWritesConfigAndUpdatesCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	if err := a.SetModelHidden("test/test-model", true); err != nil {
		t.Fatalf("SetModelHidden returned error: %v", err)
	}
	if !a.catalog.Providers["test"].Models["test-model"].Hidden {
		t.Fatalf("in-memory catalog not updated: test-model.Hidden = %v, want true", a.catalog.Providers["test"].Models["test-model"].Hidden)
	}

	data, err := os.ReadFile(agentConfigPath(a.home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	providers := root["providers"].(map[string]any)
	testProv := providers["test"].(map[string]any)
	models := testProv["models"].(map[string]any)
	testModel := models["test-model"].(map[string]any)
	if hidden, ok := testModel["hidden"].(bool); !ok || !hidden {
		t.Fatalf("config test-model.hidden = %v, want true", testModel["hidden"])
	}

	visibleEntries := a.ModelList()
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			t.Fatalf("test/test-model appeared in ModelList after SetModelHidden(true)")
		}
	}

	allEntries := a.AllModelList()
	found := false
	for _, entry := range allEntries {
		if entry.Ref == "test/test-model" {
			found = true
			if !entry.Hidden {
				t.Fatalf("AllModelList test/test-model Hidden = %v, want true", entry.Hidden)
			}
			break
		}
	}
	if !found {
		t.Fatalf("AllModelList missing test/test-model after SetModelHidden(true)")
	}

	if err := a.SetModelHidden("test/test-model", false); err != nil {
		t.Fatalf("SetModelHidden(false) returned error: %v", err)
	}
	visibleEntries = a.ModelList()
	found = false
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("test/test-model did not reappear in ModelList after SetModelHidden(false)")
	}
}

func TestAgentSetProviderHiddenWritesConfigAndUpdatesCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	if err := a.SetProviderHidden("test", true); err != nil {
		t.Fatalf("SetProviderHidden returned error: %v", err)
	}
	if !a.catalog.Providers["test"].Hidden {
		t.Fatalf("in-memory catalog not updated: test.Hidden = %v, want true", a.catalog.Providers["test"].Hidden)
	}

	data, err := os.ReadFile(agentConfigPath(a.home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	providers := root["providers"].(map[string]any)
	testProv := providers["test"].(map[string]any)
	if hidden, ok := testProv["hidden"].(bool); !ok || !hidden {
		t.Fatalf("config test.hidden = %v, want true", testProv["hidden"])
	}

	visibleEntries := a.ModelList()
	for _, entry := range visibleEntries {
		if entry.Provider == "test" {
			t.Fatalf("provider test models appeared in ModelList after SetProviderHidden(true)")
		}
	}

	allEntries := a.AllModelList()
	for _, entry := range allEntries {
		if entry.Provider == "test" {
			if !entry.ProviderHidden {
				t.Fatalf("AllModelList test entry ProviderHidden = %v, want true", entry.ProviderHidden)
			}
			if !entry.Hidden {
				t.Fatalf("AllModelList test entry Hidden = %v, want true", entry.Hidden)
			}
		}
	}

	if err := a.SetProviderHidden("test", false); err != nil {
		t.Fatalf("SetProviderHidden(false) returned error: %v", err)
	}
	visibleEntries = a.ModelList()
	found := false
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("test/test-model did not reappear in ModelList after SetProviderHidden(false)")
	}
	for _, entry := range visibleEntries {
		if entry.Ref == "test/hidden-model" {
			t.Fatalf("test/hidden-model appeared in ModelList after SetProviderHidden(false) (model-level hidden still true)")
		}
	}
}

func TestAgentMutateConfigRejectsMalformedShapes(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": "not-an-object"
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetProviderHidden("test", true); err == nil {
		t.Fatal("SetProviderHidden returned nil for malformed provider")
	} else if !strings.Contains(err.Error(), "providers.test must be an object") {
		t.Fatalf("SetProviderHidden error = %v, want providers.test must be an object", err)
	}

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": "not-an-object"
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetModelHidden("test/test-model", true); err == nil {
		t.Fatal("SetModelHidden returned nil for malformed models")
	} else if !strings.Contains(err.Error(), "providers.test.models must be an object") {
		t.Fatalf("SetModelHidden error = %v, want providers.test.models must be an object", err)
	}

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": "not-an-object"
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetModelHidden("test/test-model", true); err == nil {
		t.Fatal("SetModelHidden returned nil for malformed model")
	} else if !strings.Contains(err.Error(), "providers.test.models.test-model must be an object") {
		t.Fatalf("SetModelHidden error = %v, want providers.test.models.test-model must be an object", err)
	}
}

func writeCatalogTestConfig(t *testing.T, home, content string) {
	t.Helper()
	path := filepath.Join(home, ".lightcode", "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func configureChangingDiscoveryServer(t *testing.T, a *Agent) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		call := calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"data":[
			{"id":"test-model","name":"Test Model %d","context_window":%d,"max_output_tokens":1024},
			{"id":"fresh-model-%d","name":"Fresh Model %d","context_window":12288,"max_output_tokens":2048}
		]}`, call, 8192+call, call, call)
	}))
	t.Cleanup(server.Close)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
}

func runConcurrentDiscoveryReaders(t *testing.T, a *Agent, read func()) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, 200)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := a.RefreshDiscovery("test"); err != nil {
					errCh <- err
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				read()
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RefreshDiscovery returned error: %v", err)
		}
	}
}
