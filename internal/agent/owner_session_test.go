package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func TestUnknownSessionIDsAreRejected(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx := context.Background()

	checks := []struct {
		name string
		run  func() error
	}{
		{
			name: "submit",
			run: func() error {
				_, err := a.SubmitToSession(ctx, "missing-session", "hello")
				return err
			},
		},
		{
			name: "queue",
			run: func() error {
				_, err := a.QueueSnapshotForSession("missing-session")
				return err
			},
		},
		{
			name: "append",
			run: func() error {
				_, err := a.AppendUserMessageToSession("missing-session", "hello")
				return err
			},
		},
		{
			name: "messages",
			run: func() error {
				_, err := a.SessionMessagesByID("missing-session")
				return err
			},
		},
		{
			name: "current model",
			run: func() error {
				_, err := a.CurrentModelForSession("missing-session")
				return err
			},
		},
		{
			name: "revert code",
			run: func() error {
				_, err := a.RevertCodeForSession("missing-session", 0)
				return err
			},
		},
		{
			name: "cancel",
			run: func() error {
				return a.CancelSession("missing-session")
			},
		},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), `unknown session "missing-session"`) {
				t.Fatalf("error = %v, want unknown session", err)
			}
		})
	}
}

func TestLiveSessionsHaveSeparateHistoryAndQueues(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx := context.Background()

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("session ids first=%q second=%q, want distinct non-empty ids", firstID, secondID)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	if first == nil || second == nil {
		t.Fatalf("live session map missing first=%v second=%v", first != nil, second != nil)
	}
	if first.store == nil || second.store == nil || first.store == second.store {
		t.Fatalf("stores are not distinct: first=%p second=%p", first.store, second.store)
	}
	if first.lp == nil || second.lp == nil || first.lp == second.lp {
		t.Fatalf("loops are not distinct: first=%p second=%p", first.lp, second.lp)
	}

	if _, err := a.AppendUserMessageToSession(firstID, "first only"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second only"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	firstMessages, err := a.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	secondMessages, err := a.SessionMessagesByID(secondID)
	if err != nil {
		t.Fatalf("messages second: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"first only"}) {
		t.Fatalf("first messages = %#v, want first only", got)
	}
	if got := userContents(secondMessages); !equalStrings(got, []string{"second only"}) {
		t.Fatalf("second messages = %#v, want second only", got)
	}

	a.ensureRuntime().mu.Lock()
	first.busy = true
	second.busy = true
	a.ensureRuntime().mu.Unlock()
	t.Cleanup(func() {
		a.ensureRuntime().mu.Lock()
		first.busy = false
		second.busy = false
		a.ensureRuntime().mu.Unlock()
	})

	if _, err := a.SubmitToSession(ctx, firstID, "queued first"); err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if _, err := a.SubmitToSession(ctx, secondID, "queued second"); err != nil {
		t.Fatalf("submit second: %v", err)
	}
	firstQueue, err := a.QueueSnapshotForSession(firstID)
	if err != nil {
		t.Fatalf("queue first: %v", err)
	}
	secondQueue, err := a.QueueSnapshotForSession(secondID)
	if err != nil {
		t.Fatalf("queue second: %v", err)
	}
	if len(firstQueue.Items) != 1 || firstQueue.Items[0].Content != "queued first" {
		t.Fatalf("first queue = %#v, want queued first", firstQueue.Items)
	}
	if len(secondQueue.Items) != 1 || secondQueue.Items[0].Content != "queued second" {
		t.Fatalf("second queue = %#v, want queued second", secondQueue.Items)
	}
}

func TestQueuedInputDrainsFromInactiveSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "drained")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	ctx := startEventOrderAgent(t, a, &eventCapture{})

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if firstID == secondID {
		t.Fatalf("session ids both %q, want distinct", firstID)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	first.busy = true
	a.ensureRuntime().mu.Unlock()
	if _, err := a.SubmitToSession(ctx, firstID, "queued first"); err != nil {
		t.Fatalf("SubmitToSession first: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first.busy = false
	a.ensureRuntime().mu.Unlock()
	a.ensureRuntime().tryDrainQueue(ctx)

	waitUntilSessionQueueEmpty(t, a, firstID)
	firstMessages, err := a.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"queued first"}) {
		t.Fatalf("first messages after drain = %#v, want queued first", got)
	}
	secondQueue, err := a.QueueSnapshotForSession(secondID)
	if err != nil {
		t.Fatalf("queue second: %v", err)
	}
	if len(secondQueue.Items) != 0 {
		t.Fatalf("second queue = %#v, want empty", secondQueue.Items)
	}
}

func TestTokenUsageLoadsIntoSelectedSession(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	writeTokenFile(t, first, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
	writeTokenFile(t, second, TokenEntry{Provider: "test", Model: "alt-model", Input: 3, Output: 2, Known: true})

	a.loadTokensFromDiskForSession(first)
	report, err := a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first: %v", err)
	}
	if report.Total.Input != 11 || report.Total.Output != 7 {
		t.Fatalf("first token report = %#v, want 11/7", report.Total)
	}
	secondReport, err := a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second: %v", err)
	}
	if secondReport.Total.Input != 0 || secondReport.Total.Output != 0 {
		t.Fatalf("second token report after first load = %#v, want zero", secondReport.Total)
	}

	a.loadTokensFromDiskForSession(second)
	secondReport, err = a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second after load: %v", err)
	}
	if secondReport.Total.Input != 3 || secondReport.Total.Output != 2 {
		t.Fatalf("second token report = %#v, want 3/2", secondReport.Total)
	}
	report, err = a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first after second load: %v", err)
	}
	if report.Total.Input != 11 || report.Total.Output != 7 {
		t.Fatalf("first token report after second load = %#v, want 11/7", report.Total)
	}
}

func TestMessagesComeFromSelectedSessionProject(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	firstID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(firstID, "from first project"); err != nil {
		t.Fatalf("append first: %v", err)
	}

	second := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "from second project"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	second.ensureRuntime().mu.Lock()
	second.sessions[firstID] = first.sessions[firstID]
	second.ensureRuntime().mu.Unlock()
	firstMessages, err := second.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first via second owner: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"from first project"}) {
		t.Fatalf("first messages through resolved store = %#v, want first project", got)
	}
	secondMessages, err := second.SessionMessagesByID(secondID)
	if err != nil {
		t.Fatalf("messages second: %v", err)
	}
	if got := userContents(secondMessages); !equalStrings(got, []string{"from second project"}) {
		t.Fatalf("second messages = %#v, want second project", got)
	}
}

func TestSessionsUseTheirProjectRoot(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstRoot, "target.txt"), []byte("first-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "target.txt"), []byte("second-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstRoot, "AGENTS.md"), []byte("first project rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "AGENTS.md"), []byte("second project rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	secondTarget := filepath.Join(secondRoot, "target.txt")
	first.cfg.Permissions.Allow = []string{
		"read_file(//" + strings.TrimPrefix(secondTarget, "/") + ")",
		"run_command(pwd)",
	}
	if _, err := first.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondProjectAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondProjectID, err := secondProjectAgent.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	secondPlanTarget := filepath.Join(first.projects.Root(), secondProjectID.ID, "plans", "plan.md")
	first.cfg.Permissions.Allow = append(first.cfg.Permissions.Allow, "write_file(//"+strings.TrimPrefix(secondPlanTarget, "/")+")")

	secondID, err := first.NewSession(secondProjectID.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession second project: %v", err)
	}
	first.ensureRuntime().mu.Lock()
	second := first.sessions[secondID]
	readTool, ok := second.registry.Get("read_file")
	runTool, runOK := second.registry.Get("run_command")
	systemPrompt := second.lp.Messages()[0].TextContent()
	first.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("read_file not registered on second project session")
	}
	if !runOK {
		t.Fatal("run_command not registered on second project session")
	}
	if !strings.Contains(systemPrompt, "second project rules") || strings.Contains(systemPrompt, "first project rules") {
		t.Fatalf("system prompt uses wrong project rules: %q", systemPrompt)
	}
	out, err := readTool.Execute(context.Background(), map[string]any{"path": "target.txt"})
	if err != nil {
		t.Fatalf("read_file second project: %v", err)
	}
	if !strings.Contains(out, "second-root") || strings.Contains(out, "first-root") {
		t.Fatalf("read_file output = %q, want second project content only", out)
	}
	out, err = runTool.Execute(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("run_command pwd second project: %v", err)
	}
	if !strings.Contains(out, secondRoot) || strings.Contains(out, firstRoot) {
		t.Fatalf("run_command output = %q, want second project root only", out)
	}

	planID, err := first.NewSession(secondProjectID.ID, "plan")
	if err != nil {
		t.Fatalf("NewSession plan second project: %v", err)
	}
	first.ensureRuntime().mu.Lock()
	plan := first.sessions[planID]
	writeTool, ok := plan.registry.Get("write_file")
	first.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("write_file not registered on second project plan session")
	}
	turn := plan.store.BeginTurn()
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": secondPlanTarget, "content": "second project plan"}); err != nil {
		t.Fatalf("write_file second project plan: %v", err)
	}
	if err := plan.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("complete plan write turn: %v", err)
	}
	data, err := os.ReadFile(secondPlanTarget)
	if err != nil {
		t.Fatalf("read second project plan: %v", err)
	}
	if got := string(data); got != "second project plan" {
		t.Fatalf("second project plan content = %q, want written content", got)
	}
}

func TestTurnActionsUseSelectedSessionHistory(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	firstID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(firstID, "first project only"); err != nil {
		t.Fatalf("append first: %v", err)
	}

	second := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "keep"); err != nil {
		t.Fatalf("append second keep: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "selected"); err != nil {
		t.Fatalf("append second selected: %v", err)
	}

	first.ensureRuntime().mu.Lock()
	first.sessions[secondID] = second.sessions[secondID]
	first.ensureRuntime().mu.Unlock()
	result, err := first.ApplyTurnActionForSession(secondID, 2, TurnActionRevertHistory, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession second: %v", err)
	}
	if result.Prefill != "selected" {
		t.Fatalf("Prefill = %q, want selected", result.Prefill)
	}
	if got := userContents(result.Messages); !equalStrings(got, []string{"keep"}) {
		t.Fatalf("result messages = %#v, want keep", got)
	}
	firstMessages, err := first.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"first project only"}) {
		t.Fatalf("first messages = %#v, want unchanged first project", got)
	}
}

func TestCompactionIndexesSelectedSessionProject(t *testing.T) {
	const summary = "## Goal\nremember second project detail"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, summary)
	}))
	defer server.Close()

	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	first.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	firstProject, err := first.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	firstID, err := first.NewSession(firstProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}

	secondProjectAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondProject, err := secondProjectAgent.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	secondID, err := first.NewSession(secondProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(secondID, "second project context"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	memStore := memory.NewStoreWithEmbedder(deterministicMemoryEmbedder{}, first.projects.Root(), first.home)
	hooks := &recordingMemoryHooks{store: memStore}
	first.memoryHooks = hooks

	first.ensureRuntime().mu.Lock()
	second := first.sessions[secondID]
	first.setCurrentSessionLocked(first.sessions[firstID])
	first.ensureRuntime().mu.Unlock()
	if err := first.runCompactionForSession(context.Background(), second, false); err != nil {
		t.Fatalf("runCompactionForSession second: %v", err)
	}

	if hooks.sessionID != secondID {
		t.Fatalf("indexed session id = %q, want %q", hooks.sessionID, secondID)
	}
	if hooks.projectID != secondProject.ID || hooks.projectName != secondProject.Name {
		t.Fatalf("indexed project = %q/%q, want %q/%q", hooks.projectID, hooks.projectName, secondProject.ID, secondProject.Name)
	}

	secondSearch := tool.NewSearchHistory(memStore, secondProject.ID)
	result, err := secondSearch.Execute(context.Background(), map[string]any{"query": "second project detail"})
	if err != nil {
		t.Fatalf("search_history second: %v", err)
	}
	if !strings.Contains(result, secondID) || !strings.Contains(result, "remember second project detail") {
		t.Fatalf("second project search result = %q, want indexed second session summary", result)
	}
	firstSearch := tool.NewSearchHistory(memStore, firstProject.ID)
	result, err = firstSearch.Execute(context.Background(), map[string]any{"query": "second project detail"})
	if err != nil {
		t.Fatalf("search_history first: %v", err)
	}
	if strings.Contains(result, secondID) {
		t.Fatalf("first project search result = %q, should not include second project session %s", result, secondID)
	}
}

func TestCompactTypeCannotBeStartedByUser(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession primary: %v", err)
	}
	_, err := a.NewSession("", "compact")
	if err == nil || !strings.Contains(err.Error(), `agent type "compact" cannot be started as a session`) {
		t.Fatalf("NewSession compact error = %v, want code-only rejection", err)
	}
}

func TestTaskTokensStayWithLaunchingSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`+"\n\n", "child done")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":13,"completion_tokens":5}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	ctx := startEventOrderAgent(t, a, &eventCapture{})
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	if first == nil || second == nil {
		t.Fatalf("test setup missing sessions first=%v second=%v", first != nil, second != nil)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session = %q, want second %q", current, secondID)
	}

	turn := first.store.BeginTurn()
	if turn == 0 {
		t.Fatal("first session turn = 0")
	}
	_, err = first.taskToolInst.Execute(ctx, map[string]any{
		"tasks": []any{
			map[string]any{"prompt": "child usage", "subagent_type": "secondary"},
		},
	})
	if err != nil {
		t.Fatalf("task Execute: %v", err)
	}
	if err := first.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("complete first turn: %v", err)
	}

	firstReport, err := a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first: %v", err)
	}
	if firstReport.Total.Input != 13 || firstReport.Total.Output != 5 {
		t.Fatalf("first token report = %#v, want input 13 output 5", firstReport.Total)
	}
	secondReport, err := a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second: %v", err)
	}
	if secondReport.Total.Input != 0 || secondReport.Total.Output != 0 {
		t.Fatalf("second token report = %#v, want zero", secondReport.Total)
	}
}

func waitUntilSessionQueueEmpty(t *testing.T, a *Agent, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		queue, err := a.QueueSnapshotForSession(sessionID)
		if err == nil && len(queue.Items) == 0 {
			busy, err := a.BusyForSession(sessionID)
			if err == nil && !busy {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s did not drain", sessionID)
}

func writeTokenFile(t *testing.T, unit *session, entries ...TokenEntry) {
	t.Helper()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(unit.store.Dir(), tokensFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newCatalogBackedTestAgentForRoot(t *testing.T, home, projectRoot string) *Agent {
	t.Helper()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "alt-model": { "name": "Alt Model", "context_window": 4096, "max_output_tokens": 512 }
      }
    }
  }
}`), 0o600); err != nil {
			t.Fatal(err)
		}
		writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}
