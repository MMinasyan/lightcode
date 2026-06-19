package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/subagent"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func subFixture(name string) *adaptation.Adaptation {
	return &adaptation.Adaptation{
		Name:         name,
		ExcludeTools: []string{"read_file"},
		Blocks:       []string{"SUBAGENT_BLOCK_" + name},
		LeakPattern:  regexp.MustCompile("LEAK"),
	}
}

// subResolver maps "alt-model" -> child fixture, "parent-model" -> a distinct fixture,
// everything else -> baseline.
func subResolver(modelID string) *adaptation.Adaptation {
	switch modelID {
	case "alt-model":
		return subFixture("child")
	case "parent-model":
		return subFixture("parent")
	}
	return nil
}

func newSubagentTaskTool(t *testing.T, resolver adaptation.Resolver) *taskTool {
	t.Helper()
	return newTaskTool(taskToolConfig{
		Loader:       subagent.NewLoader(t.TempDir(), t.TempDir()),
		ResolveAdapt: resolver,
	})
}

func dummyChildClient() *provider.Client {
	return provider.New(
		&catalog.Provider{ID: "test", Transport: catalog.Transport{BaseURL: "http://127.0.0.1:9/v1"}},
		&catalog.Model{ID: "alt-model"},
		"",
	)
}

func advertisedToolNames(registry *tool.Registry, adapt *adaptation.Adaptation) []string {
	var names []string
	for _, tl := range registry.AdvertisedTools(adapt) {
		if tl.Function != nil {
			names = append(names, tl.Function.Name)
		}
	}
	return names
}

// Bullet 1: a fixture excluding read_file removes it from the child's advertised set,
// the dispatch gate blocks it, the coaching block is seeded into the child prompt, and
// the leak pattern is installed on the child loop.
func TestSubagentFixtureExcludesToolSeedsBlockAndLeak(t *testing.T) {
	task := newSubagentTaskTool(t, subResolver)
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file", "write_file"}, Prompt: "explore base prompt"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	ref := coremodel.ModelRef{Provider: "test", Model: "alt-model"}

	lp := task.buildChildLoop(at, dummyChildClient(), registry, ref)

	adapt := lp.ActiveAdaptation()
	if adapt == nil || adapt.Name != "child" {
		t.Fatalf("child adaptation = %v, want the child fixture (resolved from the child model)", adapt)
	}
	// (a) read_file absent from the advertised set.
	if slices.Contains(advertisedToolNames(registry, adapt), "read_file") {
		t.Fatal("excluded read_file still in the child's advertised set")
	}
	// (b) the dispatch gate (registry.Advertises) blocks it.
	if registry.Advertises("read_file", adapt) {
		t.Fatal("excluded read_file would not be gate-blocked")
	}
	// non-excluded tools survive.
	if !registry.Advertises("write_file", adapt) {
		t.Fatal("non-excluded write_file dropped from the child")
	}
	// (c) the coaching block is seeded into the child prompt.
	if !strings.Contains(lp.Messages()[0].TextContent(), "SUBAGENT_BLOCK_child") {
		t.Fatal("coaching block not seeded into the child prompt")
	}
	// (d) the leak pattern is on the child loop.
	if adapt.LeakPattern == nil {
		t.Fatal("leak pattern not installed on the child loop")
	}
}

// Bullet 2: a child on a baseline model is unchanged — advertised set, prompt, no leak.
func TestSubagentBaselineModelUnchanged(t *testing.T) {
	task := newSubagentTaskTool(t, subResolver)
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file", "write_file"}, Prompt: "explore base prompt"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	ref := coremodel.ModelRef{Provider: "test", Model: "baseline-model"} // not mapped -> nil

	lp := task.buildChildLoop(at, dummyChildClient(), registry, ref)

	if lp.ActiveAdaptation() != nil {
		t.Fatalf("baseline child carries an adaptation: %v", lp.ActiveAdaptation())
	}
	// The advertised set is the registered tools minus DefaultHidden ones.
	// A mutation-capable baseline child now also has apply_patch registered
	// (DefaultHidden) so the GPT-5 adaptation can reveal it; the baseline
	// filter withholds apply_patch from the advertisement.
	baseline := advertisedToolNames(registry, nil)
	if slices.Contains(baseline, "apply_patch") {
		t.Fatalf("apply_patch leaked into baseline child advertised set: %v", baseline)
	}
	// An IncludeTools adaptation that names apply_patch reveals it.
	full := advertisedToolNames(registry, &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}})
	if !slices.Contains(full, "apply_patch") {
		t.Fatalf("apply_patch absent from full set under IncludeTools: %v", full)
	}
	if got := lp.Messages()[0].TextContent(); got != "explore base prompt" {
		t.Fatalf("baseline child prompt changed: %q", got)
	}
}

// Bullet 3: the child resolves its OWN model's adaptation (the model from resolveClient,
// which may be the cfg.Subagents.Model override), never the parent's.
func TestSubagentResolvesOwnModelNotParent(t *testing.T) {
	task := newSubagentTaskTool(t, subResolver)
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file"}, Prompt: "p"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)

	child := task.buildChildLoop(at, dummyChildClient(), registry, coremodel.ModelRef{Provider: "test", Model: "alt-model"})
	if a := child.ActiveAdaptation(); a == nil || a.Name != "child" {
		t.Fatalf("child-model adaptation = %v, want the child fixture", a)
	}
	other := task.buildChildLoop(at, dummyChildClient(), registry, coremodel.ModelRef{Provider: "test", Model: "parent-model"})
	if a := other.ActiveAdaptation(); a == nil || a.Name != "parent" {
		t.Fatalf("parent-model adaptation = %v, want the parent fixture", a)
	}
	if child.ActiveAdaptation() == other.ActiveAdaptation() {
		t.Fatal("two child loops on different models share an adaptation")
	}
}

// Bullet 3 (mechanism): the cfg.Subagents.Model override (subModel) makes the child's
// model — the one resolveClient hands buildChildLoop — differ from the parent's.
func TestSubagentSubModelSelectsChildModel(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	a := newLifecycleAgent(t, t.TempDir(), t.TempDir(), "test/test-model")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)

	// Parent runs test-model; point the subagent override at alt-model.
	a.taskToolInst.setCatalog(a.catalog)
	a.taskToolInst.setSubModel("test/alt-model")
	_, ref, err := a.taskToolInst.resolveClient()
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if ref.Model != "alt-model" {
		t.Fatalf("child model = %q, want alt-model (the subModel override, not the parent's test-model)", ref.Model)
	}
}

// Bullet 4: include of an unregistered child name is a no-op, exclude of an absent name
// is harmless, and the recursive task tool is never advertised to a subagent.
func TestSubagentIncludeExcludeEdgeCases(t *testing.T) {
	task := newSubagentTaskTool(t, func(string) *adaptation.Adaptation {
		return &adaptation.Adaptation{
			Name:         "edge",
			IncludeTools: []string{"apply_patch"},    // not a child built-in -> no-op
			ExcludeTools: []string{"does_not_exist"}, // absent -> harmless
		}
	})
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file", "task"}, Prompt: "p"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)

	lp := task.buildChildLoop(at, dummyChildClient(), registry, coremodel.ModelRef{Provider: "test", Model: "x"})
	names := advertisedToolNames(registry, lp.ActiveAdaptation())
	if slices.Contains(names, "apply_patch") {
		t.Fatalf("include of an unregistered child tool produced a phantom advertisement: %v", names)
	}
	if !slices.Contains(names, "read_file") {
		t.Fatalf("exclude of an absent tool harmed an unrelated tool: %v", names)
	}
	if slices.Contains(names, "task") {
		t.Fatalf("recursive task tool advertised to a subagent: %v", names)
	}
}

// Bullet 5: concurrent subagents each hold their own adaptation (pure resolver, no
// shared state).
func TestSubagentConcurrentIndependentAdaptations(t *testing.T) {
	task := newSubagentTaskTool(t, subResolver)
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file"}, Prompt: "p"}
	models := []string{"alt-model", "parent-model", "baseline-x"}
	want := []string{"child", "parent", ""}

	results := make([]*adaptation.Adaptation, len(models))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			registry := task.buildRegistry(at, parentMutationScope{}, nil)
			lp := task.buildChildLoop(at, dummyChildClient(), registry, coremodel.ModelRef{Provider: "test", Model: m})
			results[i] = lp.ActiveAdaptation()
		}(i, m)
	}
	wg.Wait()

	for i, w := range want {
		switch {
		case w == "" && results[i] != nil:
			t.Fatalf("model %q: adaptation = %v, want baseline nil", models[i], results[i])
		case w != "" && (results[i] == nil || results[i].Name != w):
			t.Fatalf("model %q: adaptation = %v, want %q", models[i], results[i], w)
		}
	}
}

// newSubModelEventAgent builds a full agent whose subagents run a DIFFERENT model than
// the parent (subagents.model = test/alt-model vs the parent's test-model), pointed at
// the given SSE base URL.
func newSubModelEventAgent(t *testing.T, baseURL string) *Agent {
	t.Helper()
	home, projectRoot := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test")
	configPath := filepath.Join(home, ".lightcode", "config.json")
	body := fmt.Sprintf(`{
  "providers": { "test": {
    "name": "Test Provider",
    "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_TEST_KEY" },
    "discovery": false,
    "models": {
      "test-model": { "name": "TM", "context_window": 8192, "max_output_tokens": 1024 },
      "alt-model": { "name": "AM", "context_window": 8192, "max_output_tokens": 1024 }
    }
  } },
  "default_model": "test/test-model",
  "subagents": { "model": "test/alt-model" }
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func requestToolNames(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("parse request tools: %v", err)
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}
	return names
}

// End-to-end: with the parent on test-model (fixture A) and the subagent overridden to
// alt-model (fixture B), an actual subagent run shows B — its tools and prompt block —
// NOT the parent's A; and the child's excluded tool is gate-blocked at dispatch.
func TestSubagentChildShowsOwnAdaptationAndGateBlocks(t *testing.T) {
	resolver := func(modelID string) *adaptation.Adaptation {
		switch modelID {
		case "test-model":
			return &adaptation.Adaptation{Name: "A", ExcludeTools: []string{"write_file"}, Blocks: []string{"BLOCK_A"}}
		case "alt-model":
			return &adaptation.Adaptation{Name: "B", ExcludeTools: []string{"read_file"}, Blocks: []string{"BLOCK_B"}}
		}
		return nil
	}
	var calls atomic.Int32
	var childReq1, childReq2 atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"go","subagent_type":"explore"}]}`)
		case 2:
			body, _ := io.ReadAll(r.Body)
			childReq1.Store(string(body))
			// The child tries the tool B excludes; the dispatch gate must block it.
			writeTaskToolCallResponse(w, "call_rf", "read_file", `{"path":"x"}`)
		case 3:
			body, _ := io.ReadAll(r.Body)
			childReq2.Store(string(body))
			writeTextResponse(w, "CHILD_DONE")
		default:
			writeTextResponse(w, "PARENT_DONE")
		}
	}))
	defer server.Close()

	a := newSubModelEventAgent(t, server.URL+"/v1")
	a.resolveAdapt = resolver              // parent -> A (test-model)
	a.taskToolInst.resolveAdapt = resolver // child -> B (alt-model)
	writeProjectSubagentType(t, a.projectRoot, "explore", []string{"read_file", "write_file"})

	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	if _, err := a.Submit(ctx, "delegate"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	req1, _ := childReq1.Load().(string)
	req2, _ := childReq2.Load().(string)
	if req1 == "" || req2 == "" {
		t.Fatalf("child requests not captured (req1=%d req2=%d bytes)", len(req1), len(req2))
	}

	names := requestToolNames(t, req1)
	// The child is B (alt-model), not the parent's A (test-model): B excludes read_file
	// and keeps write_file (only A excludes write_file).
	if slices.Contains(names, "read_file") {
		t.Fatalf("child advertised read_file that fixture B excludes: %v", names)
	}
	if !slices.Contains(names, "write_file") {
		t.Fatalf("child dropped write_file — it wrongly took the parent's fixture A: %v", names)
	}
	if !strings.Contains(req1, "BLOCK_B") {
		t.Fatal("child prompt missing fixture B's block")
	}
	if strings.Contains(req1, "BLOCK_A") {
		t.Fatal("child prompt carries the parent's fixture A block")
	}
	// The excluded read_file call was gate-blocked; the error rides into the next request.
	if !strings.Contains(req2, "is not available") {
		t.Fatal("child's excluded read_file call was not gate-blocked at dispatch")
	}
}

// A subagent whose adaptation reveals apply_patch (the IncludeTools /
// ExcludeTools shape the production GPT-5 binding will produce in
// commit 7) advertises apply_patch and not edit_file / write_file. The
// key invariant — Decision 18 — is that the agent does NOT have to
// list apply_patch in at.Tools to receive it; it only has to be
// mutation-capable (edit_file or write_file present), because the
// registry always registers apply_patch as a hidden core tool for
// any mutation-capable agent instance, and the adaptation reveals it.
func TestSubagentAppliesApplyPatchOnGPT5Family(t *testing.T) {
	gptCodex := &adaptation.Adaptation{
		Name:         "gpt-codex",
		ExcludeTools: []string{"edit_file", "write_file"},
		IncludeTools: []string{"apply_patch"},
	}
	task := newSubagentTaskTool(t, func(string) *adaptation.Adaptation { return gptCodex })
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file", "edit_file", "write_file"}, Prompt: "p"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	ref := coremodel.ModelRef{Provider: "test", Model: "gpt-5.4"}

	lp := task.buildChildLoop(at, dummyChildClient(), registry, ref)
	adapt := lp.ActiveAdaptation()
	if adapt == nil || adapt.Name != "gpt-codex" {
		t.Fatalf("child adaptation = %v, want the gpt-codex fixture", adapt)
	}

	names := advertisedToolNames(registry, adapt)
	if !slices.Contains(names, "apply_patch") {
		t.Fatalf("apply_patch missing from child's advertised set: %v", names)
	}
	if slices.Contains(names, "edit_file") {
		t.Fatalf("edit_file leaked into the gpt-5.4 child's advertised set: %v", names)
	}
	if slices.Contains(names, "write_file") {
		t.Fatalf("write_file leaked into the gpt-5.4 child's advertised set: %v", names)
	}
}

// A read-only subagent (no edit_file / write_file in at.Tools) does NOT
// receive apply_patch even if the adaptation names it in IncludeTools,
// because apply_patch is not registered on a read-only child registry.
// Include of an unregistered name is a silent no-op (advertise.go:31).
func TestReadOnlySubagentDoesNotReceiveApplyPatch(t *testing.T) {
	gptCodex := &adaptation.Adaptation{
		IncludeTools: []string{"apply_patch"},
	}
	task := newSubagentTaskTool(t, func(string) *adaptation.Adaptation { return gptCodex })
	at := subagent.AgentType{Name: "explore", Tools: []string{"read_file"}, Prompt: "p"}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	ref := coremodel.ModelRef{Provider: "test", Model: "gpt-5.4"}

	lp := task.buildChildLoop(at, dummyChildClient(), registry, ref)
	_ = lp.ActiveAdaptation()
	names := advertisedToolNames(registry, gptCodex)
	if slices.Contains(names, "apply_patch") {
		t.Fatalf("apply_patch leaked into a read-only child's advertised set: %v", names)
	}
}

func TestReadOnlySubagentDoesNotReceiveExecutePending(t *testing.T) {
	task := newSubagentTaskTool(t, nil)
	at := subagent.AgentType{
		Name:   "explore",
		Tools:  []string{"read_file", "run_command", "diagnostics", "workspace_symbol"},
		Prompt: "p",
	}
	registry := task.buildRegistry(at, parentMutationScope{}, nil)
	names := advertisedToolNames(registry, nil)
	if slices.Contains(names, "execute_pending") {
		t.Fatalf("execute_pending leaked into a read-only child's advertised set: %v", names)
	}
	if _, ok := registry.Get("execute_pending"); ok {
		t.Fatal("execute_pending registered on a read-only child registry")
	}
}
