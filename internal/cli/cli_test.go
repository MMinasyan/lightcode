package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
)

func TestCmdClearClearsTerminalRedrawsHeaderAndKeepsMessages(t *testing.T) {
	a, projectName := newTestAgent(t)
	c := New(a)
	c.refreshState()

	var out bytes.Buffer
	c.out = &out
	c.messages = []displayEntry{{typ: "assistant", content: "keep me"}}
	c.promptLines = 2
	c.streamStarted = true
	c.streamDisplayActive = true
	c.streamBuf.WriteString("full response")
	c.streamVisibleBuf.WriteString("visible response")
	c.streamNeedsNL = true

	c.cmdClear()

	rendered := out.String()
	if !strings.Contains(rendered, "\x1b[2J\x1b[3J\x1b[H") {
		t.Fatalf("clear output missing screen/scrollback clear sequence: %q", rendered)
	}
	if !strings.Contains(rendered, projectName) || !strings.Contains(rendered, "test/test-model") {
		t.Fatalf("clear output did not redraw header: %q", rendered)
	}
	if got := len(c.messages); got != 1 || c.messages[0].content != "keep me" {
		t.Fatalf("cmdClear mutated display messages: %#v", c.messages)
	}
	if got := c.streamBuf.String(); got != "full response" {
		t.Fatalf("cmdClear cleared semantic stream buffer: %q", got)
	}
	if c.streamVisibleBuf.Len() != 0 {
		t.Fatalf("cmdClear left stale visible stream buffer: %q", c.streamVisibleBuf.String())
	}
}

func TestWarningSnapshotPrintsOnlyNewWarnings(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out
	c.state = stateStreaming

	w1 := agent.PromptWarning{Kind: "rules_not_found", Message: "No AGENTS.md found"}
	w2 := agent.PromptWarning{Kind: "lsp_install_failed", Message: "Failed to install gopls"}
	c.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{w1}})
	c.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{w1, w2}})
	c.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{w1, w2}})

	rendered := out.String()
	if got := strings.Count(rendered, "rules_not_found: No AGENTS.md found"); got != 1 {
		t.Fatalf("rules warning rendered %d times, want 1: %q", got, rendered)
	}
	if got := strings.Count(rendered, "lsp_install_failed: Failed to install gopls"); got != 1 {
		t.Fatalf("lsp warning rendered %d times, want 1: %q", got, rendered)
	}
}

func TestHandleGenericSystemSignalIdleRendersAndAppends(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out

	c.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Result: "LSP gopls ready"})

	rendered := out.String()
	if !strings.Contains(rendered, "System: LSP gopls ready") {
		t.Fatalf("system signal not rendered: %q", rendered)
	}
	if len(c.messages) != 1 || c.messages[0].typ != "system" || c.messages[0].content != "System: LSP gopls ready" {
		t.Fatalf("display entry = %#v", c.messages)
	}
}

func TestHandleGenericSystemSignalBusyRestartsThinking(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out
	c.busy = true

	c.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Result: "Model switched to openai/x"})

	rendered := out.String()
	if !strings.Contains(rendered, "System: Model switched to openai/x") {
		t.Fatalf("system signal not rendered: %q", rendered)
	}
	if len(c.messages) != 1 || c.messages[0].typ != "system" {
		t.Fatalf("display entry = %#v", c.messages)
	}
}

func TestTurnEndCancelledRendersSystemPrefixedInterrupted(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out
	c.busy = true
	c.state = stateStreaming

	c.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Cancelled: true})

	rendered := out.String()
	if !strings.Contains(rendered, "System: Request interrupted by user") {
		t.Fatalf("interrupted message not rendered with System prefix: %q", rendered)
	}
	if strings.Contains(rendered, "  interrupted") {
		t.Fatalf("legacy `  interrupted` text still present: %q", rendered)
	}
}

func TestToolDisplayEntryEndsWithBlankLineWithoutResult(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out

	c.printDisplayEntry(displayEntry{typ: "tool", name: "read_file", args: `{"path":"x.go"}`, done: true, success: true})

	if rendered := out.String(); !strings.HasSuffix(rendered, nl+nl) {
		t.Fatalf("tool without result should end with a blank line: %q", rendered)
	}
}

func TestToolDisplayEntryEndsWithBlankLineWithResult(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out

	c.printDisplayEntry(displayEntry{typ: "tool", name: "write_file", args: `{"path":"x.go","content":"hello"}`, done: true, success: true})

	if rendered := out.String(); !strings.HasSuffix(rendered, nl+nl) {
		t.Fatalf("tool with result should end with a blank line: %q", rendered)
	}
}

func TestFlushQueueKeepsQueuedMessagesWhenBackendBusy(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBackend := func() {
		releaseOnce.Do(func() { close(release) })
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defer releaseBackend()

	a, _ := newTestAgentWithBaseURL(t, server.URL+"/v1")
	agentCtx := startTestAgent(t, a)
	if _, err := a.SendPrompt(agentCtx, "hold backend busy"); err != nil {
		t.Fatalf("SendPrompt returned error: %v", err)
	}
	waitUntilAgentBusy(t, a)

	c := New(a)
	c.ctx = context.Background()
	c.out = io.Discard
	c.msgQueue = []string{"queued while busy"}

	c.mu.Lock()
	c.flushQueueLocked()
	c.mu.Unlock()

	waitUntilCLIAnimationStopped(t, c)
	c.mu.Lock()
	got := append([]string(nil), c.msgQueue...)
	c.mu.Unlock()
	if !equalStringSlices(got, []string{"queued while busy"}) {
		t.Fatalf("queue after busy backend response = %q, want retained queued text", got)
	}
	releaseBackend()
	waitUntilAgentIdle(t, a)
}

func TestFlushQueueRemovesQueuedMessagesAfterBackendAccepts(t *testing.T) {
	a, _ := newTestAgent(t)
	agentCtx := startTestAgent(t, a)
	c := New(a)
	c.ctx = agentCtx
	c.out = io.Discard
	c.msgQueue = []string{"first queued", "second queued"}

	c.mu.Lock()
	c.flushQueueLocked()
	c.mu.Unlock()

	waitUntilCLIQueueLen(t, c, 0)
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()
	waitUntilAgentIdle(t, a)
}

func newTestAgent(t *testing.T) (*agent.Agent, string) {
	return newTestAgentWithBaseURL(t, "http://127.0.0.1:9/v1")
}

func newTestAgentWithBaseURL(t *testing.T, baseURL string) (*agent.Agent, string) {
	t.Helper()

	home := t.TempDir()
	projectRoot := t.TempDir()
	projectName := filepath.Base(projectRoot)
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test")
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	a, err := agent.New(agent.Config{
		Cfg:         cfg,
		ProjectRoot: projectRoot,
		Home:        home,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, projectName
}

func startTestAgent(t *testing.T, a *agent.Agent) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.Init(ctx)
	t.Cleanup(cancel)
	return ctx
}

func waitUntilAgentBusy(t *testing.T, a *agent.Agent) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent did not become busy")
}

func waitUntilAgentIdle(t *testing.T, a *agent.Agent) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !a.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent did not become idle")
}

func waitUntilCLIQueueLen(t *testing.T, c *CLI, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.msgQueue)
		c.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	got := len(c.msgQueue)
	c.mu.Unlock()
	t.Fatalf("CLI queue len = %d, want %d", got, want)
}

func waitUntilCLIAnimationStopped(t *testing.T, c *CLI) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		stopped := c.animStop == nil
		c.mu.Unlock()
		if stopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CLI animation did not stop")
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHandleKeyIdleInputEditing(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:     &buf,
		mu:      &sync.Mutex{},
		input:   newInputLine(),
		history: newInputHistory(),
	}

	// Rune insertion redraws prompt in idle
	c.handleKeyIdle(keyMsg{Rune: 'a'})
	if got := c.input.String(); got != "a" {
		t.Fatalf("input = %q, want %q", got, "a")
	}
	if !strings.Contains(buf.String(), "> a") {
		t.Fatal("idle rune should redraw prompt")
	}

	// Backspace
	buf.Reset()
	c.handleKeyIdle(keyMsg{Special: keyBackspace})
	if got := c.input.String(); got != "" {
		t.Fatalf("input = %q, want empty after backspace", got)
	}

	// Navigation: insert "xy", move left, move home, move end
	buf.Reset()
	c.handleKeyIdle(keyMsg{Rune: 'x'})
	c.handleKeyIdle(keyMsg{Rune: 'y'})
	buf.Reset()
	c.handleKeyIdle(keyMsg{Special: keyLeft})
	if c.input.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after left", c.input.cursor)
	}
	c.handleKeyIdle(keyMsg{Special: keyHome})
	if c.input.cursor != 0 {
		t.Fatalf("cursor = %d, want 0 after home", c.input.cursor)
	}
	c.handleKeyIdle(keyMsg{Special: keyEnd})
	if c.input.cursor != 2 {
		t.Fatalf("cursor = %d, want 2 after end", c.input.cursor)
	}

	// Delete: clear, insert "xy", move home, delete forward
	buf.Reset()
	c.input.Clear()
	c.handleKeyIdle(keyMsg{Rune: 'x'})
	c.handleKeyIdle(keyMsg{Rune: 'y'})
	c.handleKeyIdle(keyMsg{Special: keyHome})
	c.handleKeyIdle(keyMsg{Special: keyDelete})
	if got := c.input.String(); got != "y" {
		t.Fatalf("input = %q, want %q after delete", got, "y")
	}

	// Right: cursor at 0 (from Home above), move right
	c.handleKeyIdle(keyMsg{Special: keyHome})
	c.handleKeyIdle(keyMsg{Special: keyRight})
	if c.input.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after right", c.input.cursor)
	}

	// History: clear input first so Prev stores empty draft,
	// then Next restores empty
	c.input.Clear()
	c.history.Add("prev")
	c.handleKeyIdle(keyMsg{Special: keyUp})
	if got := c.input.String(); got != "prev" {
		t.Fatalf("input = %q, want %q after up", got, "prev")
	}
	c.handleKeyIdle(keyMsg{Special: keyDown})
	if got := c.input.String(); got != "" {
		t.Fatalf("input = %q, want empty after down", got)
	}
}

func TestHandleKeyStreamingInputEditing(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:     &buf,
		mu:      &sync.Mutex{},
		input:   newInputLine(),
		history: newInputHistory(),
	}

	// Rune insertion does NOT redraw prompt in streaming
	c.handleKeyStreaming(keyMsg{Rune: 'a'})
	if got := c.input.String(); got != "a" {
		t.Fatalf("input = %q, want %q", got, "a")
	}
	if strings.Contains(buf.String(), "> a") {
		t.Fatal("streaming rune should not redraw prompt")
	}

	// Backspace
	c.handleKeyStreaming(keyMsg{Special: keyBackspace})
	if got := c.input.String(); got != "" {
		t.Fatalf("input = %q, want empty after backspace", got)
	}

	// Navigation
	c.handleKeyStreaming(keyMsg{Rune: 'x'})
	c.handleKeyStreaming(keyMsg{Rune: 'y'})
	c.handleKeyStreaming(keyMsg{Special: keyLeft})
	if c.input.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after left", c.input.cursor)
	}
	c.handleKeyStreaming(keyMsg{Special: keyRight})
	if c.input.cursor != 2 {
		t.Fatalf("cursor = %d, want 2 after right", c.input.cursor)
	}
	c.handleKeyStreaming(keyMsg{Special: keyHome})
	if c.input.cursor != 0 {
		t.Fatalf("cursor = %d, want 0 after home", c.input.cursor)
	}
	c.handleKeyStreaming(keyMsg{Special: keyDelete})
	if got := c.input.String(); got != "y" {
		t.Fatalf("input = %q, want %q after delete", got, "y")
	}
	c.handleKeyStreaming(keyMsg{Special: keyEnd})
	if c.input.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after end", c.input.cursor)
	}

	// History: clear input first so Prev stores empty draft,
	// then Next restores empty
	c.input.Clear()
	c.history.Add("prev")
	c.handleKeyStreaming(keyMsg{Special: keyUp})
	if got := c.input.String(); got != "prev" {
		t.Fatalf("input = %q, want %q after up", got, "prev")
	}
	c.handleKeyStreaming(keyMsg{Special: keyDown})
	if got := c.input.String(); got != "" {
		t.Fatalf("input = %q, want empty after down", got)
	}
}
