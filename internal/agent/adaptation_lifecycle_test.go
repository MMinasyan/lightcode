package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
)

func lifecycleFixture() *adaptation.Adaptation {
	return &adaptation.Adaptation{
		Name:         "fix",
		ExcludeTools: []string{"edit_file"},
		IncludeTools: []string{"apply_patch"},
		Blocks:       []string{"LIFECYCLE_BLOCK_MARKER"},
		LeakPattern:  regexp.MustCompile("LEAK"),
	}
}

// lifecycleResolver maps the bare id "alt-model" to the fixture, everything else baseline.
func lifecycleResolver(modelID string) *adaptation.Adaptation {
	if modelID == "alt-model" {
		return lifecycleFixture()
	}
	return nil
}

// deadProviderURL is an unreachable base URL for tests that never make a model
// request (model resolution builds a client but does not connect).
const deadProviderURL = "http://127.0.0.1:9/v1"

func writeLifecycleConfig(t *testing.T, home, defaultModel string, includeAlt bool, baseURL string) string {
	t.Helper()
	configPath := filepath.Join(home, ".lightcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	models := `"test-model": { "name": "Test Model", "context_window": 8192 }`
	if includeAlt {
		models += `,
        "alt-model": { "name": "Alt Model", "context_window": 4096 }`
	}
	body := `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "` + baseURL + `", "api_key_env": "LIGHTCODE_LIFECYCLE_KEY" },
      "discovery": false,
      "models": { ` + models + ` }
    }
  },
  "default_model": "` + defaultModel + `"
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if defaultModel != "" {
		writeAgentsTestConfig(t, configPath, `{"primary": {"model": "`+defaultModel+`"}}`)
	} else {
		writeAgentsTestConfig(t, configPath, `{}`)
	}
	return configPath
}

// hiddenLifecycleTool is a DefaultHidden tool registered on the agent so the
// fixture's IncludeTools can be observed surfacing it (and being withheld under
// baseline).
type hiddenLifecycleTool struct{ name string }

func (h hiddenLifecycleTool) Name() string        { return h.name }
func (h hiddenLifecycleTool) Description() string { return h.name }
func (h hiddenLifecycleTool) ParametersSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (h hiddenLifecycleTool) Execute(context.Context, map[string]any) (string, error) {
	return "", nil
}
func (h hiddenLifecycleTool) DefaultHidden() bool { return true }

// newLifecycleAgent builds an agent with the default (production Match) resolver;
// callers inject lifecycleResolver where they want the fixture. A hidden apply_patch
// tool is registered on the shared registry so include/exclude advertisement is
// observable.
func newLifecycleAgent(t *testing.T, home, projectRoot, defaultModel string) *Agent {
	t.Helper()
	configPath := writeLifecycleConfig(t, home, defaultModel, true, deadProviderURL)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	a.registry.Register(hiddenLifecycleTool{name: "apply_patch"})
	return a
}

func seedLifecycleSession(t *testing.T, a *Agent, projectRoot, provider, model string) {
	t.Helper()
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := a.store.AttachSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := a.store.BeginNewSession(projectRoot); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := a.store.SetModel(provider, model); err != nil {
		t.Fatalf("set model: %v", err)
	}
	raw, _ := json.Marshal(message.NewText(message.RoleUser, "hello"))
	if err := a.store.AppendMessage(1, raw); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := a.store.MarkTurnComplete(1); err != nil {
		t.Fatalf("mark turn complete: %v", err)
	}
	if _, err := a.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func toolAdvertised(a *Agent, name string) bool {
	for _, tl := range a.registry.AdvertisedTools(a.lp.ActiveAdaptation()) {
		if tl.Function != nil && tl.Function.Name == name {
			return true
		}
	}
	return false
}

func assertFixtureActive(t *testing.T, a *Agent) {
	t.Helper()
	if a.activeAdapt == nil || a.activeAdapt.Name != "fix" {
		t.Fatalf("activeAdapt = %v, want the fixture", a.activeAdapt)
	}
	if a.lp.ActiveAdaptation() != a.activeAdapt {
		t.Fatal("loop did not receive the resolved adaptation")
	}
	if a.lp.ActiveAdaptation().LeakPattern == nil {
		t.Fatal("loop leak pattern not set")
	}
	if toolAdvertised(a, "edit_file") {
		t.Fatal("excluded edit_file still advertised under the fixture")
	}
	if !toolAdvertised(a, "apply_patch") {
		t.Fatal("included hidden apply_patch not advertised under the fixture")
	}
	if !strings.Contains(a.lp.Messages()[0].TextContent(), "LIFECYCLE_BLOCK_MARKER") {
		t.Fatal("system prompt missing the coaching block")
	}
}

func assertBaselineActive(t *testing.T, a *Agent) {
	t.Helper()
	if a.activeAdapt != nil {
		t.Fatalf("activeAdapt = %v, want nil baseline", a.activeAdapt)
	}
	if a.lp.ActiveAdaptation() != nil {
		t.Fatal("loop still carries an adaptation")
	}
	if !toolAdvertised(a, "edit_file") {
		t.Fatal("edit_file not advertised under baseline")
	}
	if toolAdvertised(a, "apply_patch") {
		t.Fatal("hidden apply_patch advertised under baseline (should be withheld)")
	}
	if strings.Contains(a.lp.Messages()[0].TextContent(), "LIFECYCLE_BLOCK_MARKER") {
		t.Fatal("system prompt still carries the coaching block")
	}
}

// SET via SwitchModel + revert to baseline.
func TestLifecycleSwitchModelAppliesAndReverts(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	a := newLifecycleAgent(t, t.TempDir(), t.TempDir(), "test/test-model")
	a.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)

	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("switch to fixture model: %v", err)
	}
	assertFixtureActive(t, a)

	if err := a.SwitchModel("test/test-model"); err != nil {
		t.Fatalf("switch to baseline model: %v", err)
	}
	assertBaselineActive(t, a)
}

// SET via restoreModelFromSession (session resume) — a first-class set site.
func TestLifecycleRestoreAppliesFixture(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	a1 := newLifecycleAgent(t, home, projectRoot, "test/test-model")
	seedLifecycleSession(t, a1, projectRoot, "test", "alt-model")

	a2 := newLifecycleAgent(t, home, projectRoot, "test/test-model")
	a2.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a2.Init(ctx) // resumes -> restoreModelFromSession(alt-model) -> fixture
	assertFixtureActive(t, a2)
}

// SET via ensureActiveModelLocked (lazy resolve of the primary model).
func TestLifecycleEnsureAppliesFixture(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	a := newLifecycleAgent(t, t.TempDir(), t.TempDir(), "test/alt-model")
	a.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	_ = a.CurrentModel() // ensureActiveModelLocked sets primary alt-model -> fixture
	assertFixtureActive(t, a)
}

// CLEAR via reload disconnect (and ensureActiveModelLocked's own clear) — activeAdapt
// reverts to nil, consistent with the now-nil client.
func TestLifecycleReloadDisconnectClears(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	a := newLifecycleAgent(t, home, projectRoot, "test/alt-model")
	a.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	_ = a.CurrentModel()
	assertFixtureActive(t, a)

	// Rewrite the config without alt-model and with no primary model, then reload: the
	// active alt-model is no longer connected, so the inline clear fires and nothing
	// re-sets it.
	writeLifecycleConfig(t, home, "", false, deadProviderURL)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a.activeAdapt != nil {
		t.Fatalf("activeAdapt not cleared on disconnect: %v", a.activeAdapt)
	}
	if a.lp.ActiveAdaptation() != nil {
		t.Fatal("loop adaptation not cleared on disconnect")
	}
	if !a.currentRef.IsZero() {
		t.Fatalf("currentRef not cleared: %v", a.currentRef)
	}
	if strings.Contains(a.lp.Messages()[0].TextContent(), "LIFECYCLE_BLOCK_MARKER") {
		t.Fatal("reload clear did not revert the prompt block (third lever)")
	}
}

// SET via SetDefaultModel (nil -> primary.model -> ensureActiveModelLocked's own branch).
func TestLifecycleSetDefaultModelAppliesFixture(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	a := newLifecycleAgent(t, t.TempDir(), t.TempDir(), "") // no primary model -> no model resolved at Init
	a.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	if err := a.SetDefaultModel("test/alt-model"); err != nil {
		t.Fatalf("set default model: %v", err)
	}
	assertFixtureActive(t, a)
}

// The per-turn preamble (launchTurn -> refreshSystemPrompt with activeAdapt) keeps the
// coaching block across a rules-file change, exercised through a REAL turn so the actual
// preamble call is verified (it would drop the block if it passed nil).
func TestLifecyclePreambleKeepsBlockAcrossTurn(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+`{"id":"x","object":"chat.completion.chunk","model":"alt-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`+"\n\n")
		io.WriteString(w, "data: "+`{"id":"x","object":"chat.completion.chunk","model":"alt-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	home, projectRoot := t.TempDir(), t.TempDir()
	configPath := writeLifecycleConfig(t, home, "test/alt-model", true, srv.URL)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	a.registry.Register(hiddenLifecycleTool{name: "apply_patch"})
	a.resolveAdapt = lifecycleResolver

	turnEnd := make(chan struct{}, 1)
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventTurnEnd {
			select {
			case turnEnd <- struct{}{}:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	_ = a.CurrentModel()
	assertFixtureActive(t, a)

	// Change the rules so the per-turn preamble must rebuild the prompt.
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("NEW_PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(ctx, "hi"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-turnEnd:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not complete")
	}

	prompt := a.lp.Messages()[0].TextContent()
	if !strings.Contains(prompt, "NEW_PROJECT_RULES_MARKER") {
		t.Fatal("turn preamble did not pick up the rules-file change")
	}
	if !strings.Contains(prompt, "LIFECYCLE_BLOCK_MARKER") {
		t.Fatal("turn preamble dropped the coaching block (launchTurn must call refreshSystemPrompt with activeAdapt)")
	}
}

// CLEAR via ensureActiveModelLocked in isolation: a disconnected current model with no
// primary model must revert activeAdapt to nil (would stay the fixture if ensure's clear
// branch were not routed through clearActiveModelLocked).
func TestLifecycleEnsureClearRevertsAdaptation(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	a := newLifecycleAgent(t, t.TempDir(), t.TempDir(), "") // no primary model
	a.resolveAdapt = lifecycleResolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("switch to fixture model: %v", err)
	}
	assertFixtureActive(t, a)
	writeAgentsTestConfig(t, a.configPath, `{}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload after clearing primary model: %v", err)
	}
	assertFixtureActive(t, a)

	rt := a.ensureRuntime()
	rt.mu.Lock()
	a.currentRef = coremodel.ModelRef{Provider: "gone", Model: "gone"} // not in catalog -> disconnected
	a.ensureActiveModelLocked()
	rt.mu.Unlock()

	if a.activeAdapt != nil {
		t.Fatalf("ensure clear did not revert activeAdapt: %v", a.activeAdapt)
	}
	if a.lp.ActiveAdaptation() != nil {
		t.Fatal("loop adaptation not cleared by ensure")
	}
	if strings.Contains(a.lp.Messages()[0].TextContent(), "LIFECYCLE_BLOCK_MARKER") {
		t.Fatal("ensure clear did not revert the prompt block (third lever)")
	}
}

// Master invariant: an unmatched model resolves to baseline across ALL lifecycle
// paths — restore, lazy-resolve, switch, and reload — so tools, prompt, and leak
// are unchanged. No resolver is injected; this uses the production matcher.
func TestLifecycleUnmatchedModelIsBaseline(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	// Seed a session pinned to alt-model so Init exercises the restore path.
	seed := newLifecycleAgent(t, home, projectRoot, "test/alt-model")
	seedLifecycleSession(t, seed, projectRoot, "test", "alt-model")

	a := newLifecycleAgent(t, home, projectRoot, "test/alt-model")
	// Capture the construction-time baseline prompt; it must stay byte-identical
	// across every lifecycle path for an unmatched model.
	baselinePrompt := a.lp.Messages()[0].TextContent()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	assertBaseline := func(path string) {
		t.Helper()
		assertBaselineActive(t, a)
		if got := a.lp.Messages()[0].TextContent(); got != baselinePrompt {
			t.Fatalf("%s: system prompt bytes changed for unmatched model", path)
		}
		// A nil-adaptation refresh must reproduce the installed prompt exactly, so
		// the installed-prompt comparison suppresses any UpdateSystemPrompt churn.
		if got := a.assembleSystemPromptForSessionLocked(a.session); got.Prompt != a.session.installedPrompt {
			t.Fatalf("%s: nil-adaptation assemble differs from the installed prompt", path)
		}
	}

	a.Init(ctx) // restore path: resumes the alt-model session -> still baseline
	assertBaseline("restore")

	_ = a.CurrentModel() // lazy-resolve path
	assertBaseline("lazy-resolve")

	if err := a.SwitchModel("test/alt-model"); err != nil { // switch path
		t.Fatalf("switch: %v", err)
	}
	assertBaseline("switch")

	if err := a.Reload(); err != nil { // reload path (model stays connected)
		t.Fatalf("reload: %v", err)
	}
	assertBaseline("reload")
}
