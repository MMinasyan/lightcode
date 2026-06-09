package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/catalog"
)

const editCfg = `{
  "providers": {
    "custom": {
      "name": "Custom",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "" },
      "discovery": false,
      "models": { "m1": { "context_window": 1000 } }
    }
  },
  "default_model": ""
}`

func modelEntry(t *testing.T, entries []ModelListEntry, ref string) (ModelListEntry, bool) {
	t.Helper()
	for _, e := range entries {
		if e.Ref == ref {
			return e, true
		}
	}
	return ModelListEntry{}, false
}

func readModelConfig(t *testing.T, configPath, provider, model string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	provs, _ := root["providers"].(map[string]any)
	pm, _ := provs[provider].(map[string]any)
	models, _ := pm["models"].(map[string]any)
	m, _ := models[model].(map[string]any)
	return m
}

func TestClassifyModelSource(t *testing.T) {
	bundled := map[string]map[string]struct{}{"p": {"bm": {}}}
	disc := map[string]catalog.DiscoveredProvider{"p": {Models: map[string]catalog.DiscoveredModel{"dm": {}}}}
	builtin := &catalog.Provider{ID: "p", Builtin: true}
	custom := &catalog.Provider{ID: "c", Builtin: false}

	cases := []struct {
		prov *catalog.Provider
		id   string
		want string
	}{
		{builtin, "bm", modelSourceBundled},
		{builtin, "dm", modelSourceDiscovered},
		{builtin, "um", modelSourceUser},
		{custom, "anything", modelSourceUser},
		{nil, "x", modelSourceUser},
	}
	for _, c := range cases {
		if got := classifyModelSource(c.prov, c.id, bundled, disc); got != c.want {
			t.Errorf("classifyModelSource(%v, %q) = %q, want %q", c.prov, c.id, got, c.want)
		}
	}
}

func TestSaveModelEditsAndWritesInts(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SaveModel("custom", "m1", ModelConfigInput{ContextWindow: 5000, MaxOutputTokens: 2048}); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	e, ok := modelEntry(t, a.AllModelList(), "custom/m1")
	if !ok {
		t.Fatal("custom/m1 not listed")
	}
	if e.ContextWindow != 5000 || e.MaxOutputTokens != 2048 {
		t.Fatalf("got ctx=%d out=%d, want 5000/2048", e.ContextWindow, e.MaxOutputTokens)
	}
	// The values must be written as JSON numbers, not strings (a wrong type
	// would drop the whole provider at next build).
	m := readModelConfig(t, a.configPath, "custom", "m1")
	if _, isNum := m["context_window"].(float64); !isNum {
		t.Fatalf("context_window written as %T, want number", m["context_window"])
	}
}

func TestSaveModelAddsModel(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SaveModel("custom", "m2", ModelConfigInput{Name: "Model Two", ContextWindow: 8000}); err != nil {
		t.Fatalf("SaveModel add: %v", err)
	}
	e, ok := modelEntry(t, a.AllModelList(), "custom/m2")
	if !ok {
		t.Fatal("added model custom/m2 not listed")
	}
	if e.DisplayName != "Model Two" || e.ContextWindow != 8000 {
		t.Fatalf("got name=%q ctx=%d", e.DisplayName, e.ContextWindow)
	}
}

func TestDeleteModelUserAddedSucceedsBundledGuarded(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.DeleteModel("custom", "m1"); err != nil {
		t.Fatalf("DeleteModel user-added: %v", err)
	}
	if _, ok := modelEntry(t, a.AllModelList(), "custom/m1"); ok {
		t.Fatal("custom/m1 still listed after delete")
	}
	// Bundled model cannot be deleted.
	err := a.DeleteModel("openai", "gpt-5.5")
	if err == nil || !strings.Contains(err.Error(), "user-added") {
		t.Fatalf("DeleteModel bundled = %v, want user-added guard error", err)
	}
}

func TestResetModelFieldRevertsAndNoOp(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SaveModel("custom", "m1", ModelConfigInput{Name: "Edited", MaxOutputTokens: 2048}); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	if err := a.ResetModelField("custom", "m1", "max_output_tokens"); err != nil {
		t.Fatalf("ResetModelField: %v", err)
	}
	m := readModelConfig(t, a.configPath, "custom", "m1")
	if _, present := m["max_output_tokens"]; present {
		t.Fatal("max_output_tokens override not deleted")
	}
	// No-op reset of an absent field must not error.
	if err := a.ResetModelField("custom", "m1", "max_output_tokens"); err != nil {
		t.Fatalf("no-op ResetModelField errored: %v", err)
	}
	// Unknown field rejected.
	if err := a.ResetModelField("custom", "m1", "bogus"); err == nil {
		t.Fatal("ResetModelField accepted an unknown field")
	}
}

func TestResetModelFieldRejectsUserModelContextWindowWithoutWriting(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	err := a.ResetModelField("custom", "m1", "context_window")
	if err == nil || !strings.Contains(err.Error(), "context_window") || !strings.Contains(err.Error(), "user-added") {
		t.Fatalf("ResetModelField context_window = %v, want user-added context guard", err)
	}
	m := readModelConfig(t, a.configPath, "custom", "m1")
	if got := m["context_window"]; got != float64(1000) {
		t.Fatalf("context_window after rejected reset = %v, want 1000", got)
	}
	if _, ok := modelEntry(t, a.AllModelList(), "custom/m1"); !ok {
		t.Fatal("custom/m1 disappeared after rejected reset")
	}
}

func TestSetProviderConfigEditsTransport(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SetProviderConfig("custom", ProviderConfigInput{BaseURL: "http://changed/v1"}); err != nil {
		t.Fatalf("SetProviderConfig: %v", err)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if st.BaseURL != "http://changed/v1" {
		t.Fatalf("base_url = %q, want http://changed/v1", st.BaseURL)
	}
}

func TestSetProviderConfigRejectsInvalidCustomProviderWithoutWriting(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	err := a.SetProviderConfig("custom", ProviderConfigInput{BaseURL: "not a url"})
	if err == nil || !strings.Contains(err.Error(), "invalid provider config") {
		t.Fatalf("SetProviderConfig invalid base_url = %v, want validation error", err)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if st.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base_url was written as %q", st.BaseURL)
	}
}

func TestSaveModelRejectsInvalidCustomModelWithoutWriting(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	err := a.SaveModel("custom", "m1", ModelConfigInput{SystemRole: "bad-role"})
	if err == nil || !strings.Contains(err.Error(), "invalid provider config") {
		t.Fatalf("SaveModel invalid system_role = %v, want validation error", err)
	}
	m := readModelConfig(t, a.configPath, "custom", "m1")
	if _, present := m["system_role"]; present {
		t.Fatalf("invalid system_role was written: %#v", m["system_role"])
	}
}

func TestResetProviderFieldRejectsInvalidCustomProviderWithoutWriting(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	err := a.ResetProviderField("custom", "base_url")
	if err == nil || !strings.Contains(err.Error(), "invalid provider config") {
		t.Fatalf("ResetProviderField base_url = %v, want validation error", err)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if st.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base_url was reset to %q", st.BaseURL)
	}
}

func TestSetProviderConfigLocksBuiltinIdentity(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SetProviderConfig("openai", ProviderConfigInput{Name: "Renamed"}); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("rename built-in provider = %v, want lock error", err)
	}
	if err := a.SetProviderConfig("openai", ProviderConfigInput{BaseURL: "http://x/v1"}); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("change built-in base URL = %v, want lock error", err)
	}
	// On a built-in, the bundled attribution header is immutable: a write that
	// tries to change it is stripped, while a genuinely new header is kept.
	if err := a.SetProviderConfig("openrouter", ProviderConfigInput{Headers: map[string]string{"X-Title": "codex", "X-Proxy": "corp"}}); err != nil {
		t.Fatalf("set built-in headers = %v, want allowed", err)
	}
	view, err := a.GetProviderConfig("openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := view.UserHeaders["X-Title"]; leaked {
		t.Fatal("bundled X-Title leaked into user headers")
	}
	if view.UserHeaders["X-Proxy"] != "corp" {
		t.Fatalf("user header X-Proxy = %q, want corp", view.UserHeaders["X-Proxy"])
	}
	if view.Headers["X-Title"] != "Lightcode" {
		t.Fatalf("effective X-Title = %q, want bundled Lightcode (not overridable)", view.Headers["X-Title"])
	}
}

func TestSaveModelLocksBuiltinModelIdentity(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SaveModel("openai", "gpt-5.5", ModelConfigInput{Name: "Renamed"}); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("rename built-in model = %v, want lock error", err)
	}
	// Context window IS editable on a built-in model (it's correctable metadata).
	if err := a.SaveModel("openai", "gpt-5.5", ModelConfigInput{ContextWindow: 123456}); err != nil {
		t.Fatalf("set context_window on built-in model = %v, want allowed", err)
	}
}

func TestSetProviderConfigClearsCustomHeaders(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	if err := a.SetProviderConfig("custom", ProviderConfigInput{Headers: map[string]string{"X-A": "1"}}); err != nil {
		t.Fatalf("set header: %v", err)
	}
	if v, _ := a.GetProviderConfig("custom"); v.UserHeaders["X-A"] != "1" {
		t.Fatalf("header not set: %#v", v.UserHeaders)
	}
	// Clearing all headers must take effect (not silently revert).
	if err := a.SetProviderConfig("custom", ProviderConfigInput{Headers: map[string]string{}}); err != nil {
		t.Fatalf("clear headers: %v", err)
	}
	if v, _ := a.GetProviderConfig("custom"); len(v.UserHeaders) != 0 {
		t.Fatalf("headers not cleared: %#v", v.UserHeaders)
	}
}

func TestSetProviderConfigGuards(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)
	// custom is keyless+usable → connected; changing api_key_env must be refused.
	err := a.SetProviderConfig("custom", ProviderConfigInput{APIKeyEnv: "NEW_VAR"})
	if err == nil || !strings.Contains(err.Error(), "disconnect") {
		t.Fatalf("api_key_env change while connected = %v, want disconnect guard", err)
	}
	// Authorization header is rejected.
	err = a.SetProviderConfig("custom", ProviderConfigInput{Headers: map[string]string{"Authorization": "Bearer x"}})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("Authorization header = %v, want rejection", err)
	}
}
