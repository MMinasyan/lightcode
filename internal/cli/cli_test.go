package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

func TestCmdClearClearsTerminalRedrawsHeaderAndKeepsMessages(t *testing.T) {
	a, projectName := newTestAgent(t)
	if _, err := a.AppendUserMessage("seed"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	c := New(a)
	c.setCurrentSessionID(a.SessionCurrent().ID)
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

func TestHandleUserMessageDisplayAppendsAndRenders(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out
	c.width = 80

	c.handleEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Result: "hello world", Turn: 4})

	if len(c.messages) != 1 || c.messages[0].typ != "user" || c.messages[0].content != "hello world" || c.messages[0].turn != 4 {
		t.Fatalf("messages = %#v, want one user entry with content + turn", c.messages)
	}
	if rendered := out.String(); !strings.Contains(rendered, "hello world") {
		t.Fatalf("user render missing content: %q", rendered)
	}
}

func TestHandleGenericSystemSignalAppendsAndRenders(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out
	c.width = 80

	c.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Result: "Model switched to test/test-model", Turn: 2})

	if len(c.messages) != 1 || c.messages[0].typ != "system" || c.messages[0].content != "System: Model switched to test/test-model" {
		t.Fatalf("messages = %#v, want one system entry with System: prefix", c.messages)
	}
	if rendered := out.String(); !strings.Contains(rendered, "System: Model switched") {
		t.Fatalf("system render missing prefixed payload: %q", rendered)
	}
}

func TestHandleTurnEndCancelledDoesNotAppendTranscriptEntry(t *testing.T) {
	c := New(nil)
	var out bytes.Buffer
	c.out = &out

	c.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 3, Cancelled: true})

	for _, m := range c.messages {
		if m.typ == "system" {
			t.Fatalf("turn_end cancelled appended a system entry: %#v", c.messages)
		}
	}
	if strings.Contains(out.String(), "interrupted") {
		t.Fatalf("turn_end cancelled rendered interrupted text directly: %q", out.String())
	}
}

func TestActiveCompactionRefreshDeferredUntilTurnEnd(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.AppendUserMessage("persisted before compaction"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	c := New(a)
	c.setCurrentSessionID(a.SessionCurrent().ID)
	var out bytes.Buffer
	c.out = &out
	c.messages = []displayEntry{{typ: "system", content: "System: live signal before compaction"}}
	c.compacting = true
	c.busy = true
	c.state = stateStreaming

	c.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, RefreshSession: false})

	if c.compacting {
		t.Fatal("compaction_end should clear compacting state")
	}
	if got := len(c.messages); got != 1 || c.messages[0].content != "System: live signal before compaction" {
		t.Fatalf("compaction_end without RefreshSession rebuilt active live rows: %#v", c.messages)
	}
	if strings.Contains(out.String(), "persisted before compaction") {
		t.Fatalf("compaction_end without RefreshSession refreshed persisted history: %q", out.String())
	}

	out.Reset()
	c.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 2, RefreshSession: true})

	if c.busy || c.state != stateIdle {
		t.Fatalf("turn_end should leave CLI idle: busy=%v state=%v", c.busy, c.state)
	}
	if got := len(c.messages); got != 1 || c.messages[0].typ != "user" || c.messages[0].content != "persisted before compaction" {
		t.Fatalf("turn_end with RefreshSession should rebuild from persisted history: %#v", c.messages)
	}
	if !strings.Contains(out.String(), "persisted before compaction") {
		t.Fatalf("turn_end with RefreshSession did not render refreshed history: %q", out.String())
	}
}

func TestCLIStaleCurrent(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.AppendUserMessage("gone"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if _, err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	c := New(a)
	c.setCurrentSessionID(id)
	if got := c.currentSessionSummary().ID; got != "" {
		t.Fatalf("stale current id = %q, want empty", got)
	}
	c.setCurrentSessionID(id)
	if got := c.sessionMessages(); len(got) != 0 {
		t.Fatalf("stale messages = %#v, want empty", got)
	}
}

func TestCLIClearRemovedCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*agent.Agent, string) (bool, error)
	}{
		{name: "archive", run: func(a *agent.Agent, id string) (bool, error) { return a.SessionArchive(id) }},
		{name: "delete", run: func(a *agent.Agent, id string) (bool, error) { return a.SessionDelete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestAgent(t)
			firstID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			secondID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}

			c := New(a)
			var out bytes.Buffer
			c.out = &out
			c.setCurrentSessionID(firstID)
			closedCurrent, err := tc.run(a, firstID)
			if err != nil {
				t.Fatalf("%s first: %v", tc.name, err)
			}
			c.clearRemovedCurrent(firstID, closedCurrent)
			if got := c.currentSessionSummary().ID; got != "" {
				t.Fatalf("current after %s = %q, want empty", tc.name, got)
			}
			out.Reset()
			c.cmdCompact()
			if !strings.Contains(out.String(), "no current session") {
				t.Fatalf("%s compact output = %q, want no current session", tc.name, out.String())
			}
			if c.acceptsEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: firstID, Result: "skip"}) {
				t.Fatalf("%s left old session event visible", tc.name)
			}
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("backend current after %s = %q, want %q", tc.name, current, secondID)
			}
		})
	}
}

func TestCLINewSetsCurrent(t *testing.T) {
	a, _ := newTestAgent(t)
	c := New(a)
	var out bytes.Buffer
	c.out = &out

	c.dispatchCommand("/new")
	if got := c.currentSessionSummary().ID; got == "" {
		t.Fatal("new session did not set current")
	}
}

func TestCLISwitchKeepsCurrent(t *testing.T) {
	a, _ := newTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	c := New(a)
	var out bytes.Buffer
	c.out = &out
	c.setCurrentSessionID(secondID)
	c.cmdResume([]string{"/resume", firstID})
	if got := c.currentSessionSummary().ID; got != firstID {
		t.Fatalf("cli current = %q, want %q", got, firstID)
	}
	if got := a.SessionCurrent().ID; got != secondID {
		t.Fatalf("backend current = %q, want %q", got, secondID)
	}
	if got := cliUserContents(c.sessionMessages()); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("cli messages = %#v, want first", got)
	}
}

func TestCLISubagentFilter(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.AppendUserMessage("root"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	root := a.SessionCurrent().ID
	if root == "" {
		t.Fatal("missing root session")
	}
	c := New(a)

	c.setCurrentSessionID("")
	if c.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: root}) {
		t.Fatal("empty current accepted child event")
	}

	c.setCurrentSessionID(root)
	if c.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: "other"}) {
		t.Fatal("wrong parent accepted child event")
	}
	if !c.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: root}) {
		t.Fatal("matching parent child start rejected")
	}
	if !c.acceptsEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "child", SubagentSessionID: "child", Result: "ok"}) {
		t.Fatal("subscribed child event rejected")
	}

	oldRoot := c.liveCurrentSessionID()
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if _, err := a.AppendUserMessage("next"); err != nil {
		t.Fatalf("seed next session: %v", err)
	}
	next := a.SessionCurrent().ID
	if next == "" || next == oldRoot {
		t.Fatalf("new session id = %q, old = %q", next, oldRoot)
	}
	c.setCurrentSessionID(next)
	if c.acceptsSubagentEventForCurrent(oldRoot, agent.Event{Kind: agent.EventSubagentStart, SessionID: "old-child", SubagentSessionID: "old-child", ParentSessionID: oldRoot}) {
		t.Fatal("stale current snapshot accepted child event")
	}
}

func TestSubmitAndFlushDoNotRenderUserMessagesLocally(t *testing.T) {
	source, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	body, ok := extractFunctionBody(string(source), "func (c *CLI) submitInputLocked(")
	if !ok {
		t.Fatal("submitInputLocked not found in cli.go")
	}
	if strings.Contains(body, "renderUserMsg(") {
		t.Fatalf("submitInputLocked must not call renderUserMsg; user transcript entries arrive via EventUserMessageDisplay")
	}
	if strings.Contains(body, "typ:  \"user\"") || strings.Contains(body, "typ: \"user\"") {
		t.Fatalf("submitInputLocked must not append a user displayEntry locally")
	}

	body, ok = extractFunctionBody(string(source), "func (c *CLI) submitToBackend(")
	if !ok {
		t.Fatal("submitToBackend not found in cli.go")
	}
	if strings.Contains(body, "renderUserMsg(") {
		t.Fatalf("submitToBackend must not call renderUserMsg; user transcript entries arrive via EventUserMessageDisplay")
	}
	if strings.Contains(body, "typ:  \"user\"") || strings.Contains(body, "typ: \"user\"") {
		t.Fatalf("submitToBackend must not append a user displayEntry locally")
	}
	// The CLI must route input through the explicit-session backend submit entry point.
	if !strings.Contains(string(source), "c.agent.SubmitToSession(c.ctx") {
		t.Fatalf("CLI must submit input through agent.SubmitToSession")
	}
}

func TestSubmitErrorRestoresDraftWhenInputEmpty(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:     &buf,
		mu:      &sync.Mutex{},
		input:   newInputLine(),
		history: newInputHistory(),
	}

	c.mu.Lock()
	c.handleSubmitErrorLocked("retry this", errors.New("session is changing; retry"))
	c.mu.Unlock()

	if got := c.input.String(); got != "retry this" {
		t.Fatalf("input = %q, want rejected text restored", got)
	}
}

func TestSubmitErrorDoesNotOverwriteNewDraft(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:     &buf,
		mu:      &sync.Mutex{},
		input:   newInputLine(),
		history: newInputHistory(),
	}
	c.input.Set("new draft")

	c.mu.Lock()
	c.handleSubmitErrorLocked("old rejected text", errors.New("session is changing; retry"))
	c.mu.Unlock()

	if got := c.input.String(); got != "new draft" {
		t.Fatalf("input = %q, want newer draft preserved", got)
	}
}

func TestFinishCompactDoesNotOverwriteRunningQueuedTurn(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:        &buf,
		mu:         &sync.Mutex{},
		input:      newInputLine(),
		history:    newInputHistory(),
		busy:       true,
		compacting: true,
		state:      stateStreaming,
	}

	c.mu.Lock()
	idle := c.finishCompactLocked()
	c.mu.Unlock()

	if idle {
		t.Fatal("finishCompactLocked reported idle while a queued turn was running")
	}
	if !c.busy || c.state != stateStreaming {
		t.Fatalf("finishCompactLocked overwrote queued turn state: busy=%v state=%v", c.busy, c.state)
	}
	if c.compacting {
		t.Fatal("finishCompactLocked should clear compacting")
	}
}

func TestFinishCompactReturnsIdleWhenNoTurnRunning(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{
		out:        &buf,
		mu:         &sync.Mutex{},
		input:      newInputLine(),
		history:    newInputHistory(),
		compacting: true,
		state:      stateStreaming,
	}

	c.mu.Lock()
	idle := c.finishCompactLocked()
	c.mu.Unlock()

	if !idle || c.busy || c.state != stateIdle {
		t.Fatalf("finishCompactLocked should restore idle state: idle=%v busy=%v state=%v", idle, c.busy, c.state)
	}
	if c.compacting {
		t.Fatal("finishCompactLocked should clear compacting")
	}
}

func TestStagedFlushTerminalLateEndNoCursorUpAndLastEndWins(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{out: &buf, mu: &sync.Mutex{}}
	args := `{"path":"x.go","old_string":"a","new_string":"b"}`

	// State as if ToolCallStart already rendered the staged tool's header and
	// its stage-time "Staged." end already completed it in the model — i.e. the
	// row is far above and activeToolID was consumed (a later, non-matching end
	// is the staged-flush "late" case).
	c.messages = []displayEntry{
		{typ: "assistant", content: "intro"},
		{typ: "tool", id: "call_1", name: "edit_file", args: args, done: true, success: true, result: "Staged."},
		{typ: "assistant", content: "more text below the tool row"},
	}
	c.activeToolID = ""

	meta := editpreview.MetadataFromArgs(args, "Edited x.go (1 replacement, lines 1-2).")
	if meta == nil {
		t.Fatal("test setup: expected non-nil edit_preview metadata")
	}
	// Production: the staged-flush ToolCallEnd carries NO Args (flushPendingQueue
	// emits only id/name/result/metadata). The CLI must still render the path
	// from the model row, matching reload.
	c.handleEvent(agent.Event{
		Kind:       agent.EventToolCallEnd,
		ToolCallID: "call_1",
		ToolName:   "edit_file",
		Result:     "Edited x.go (1 replacement, lines 1-2).",
		Metadata:   meta,
	})

	// Late end must NOT emit a cursor-up against the (far-below) current line.
	if strings.Contains(buf.String(), "\x1b[1A") {
		t.Fatalf("late staged-flush end performed a cursor-up against the wrong line: %q", buf.String())
	}
	// The rendered header must still show the path even though the event had no
	// Args (rendered from the model row's retained args) — no live/reload drift.
	if !strings.Contains(buf.String(), "x.go") {
		t.Fatalf("late staged-flush header dropped the path: %q", buf.String())
	}
	// Model is last-end-wins: the row now holds the real result, not "Staged.".
	var row *displayEntry
	for i := range c.messages {
		if c.messages[i].typ == "tool" && c.messages[i].id == "call_1" {
			row = &c.messages[i]
		}
	}
	if row == nil || row.result != "Edited x.go (1 replacement, lines 1-2)." {
		t.Fatalf("tool row not overwritten with real result: %#v", row)
	}
}

func TestInlineToolEndStillRewritesInPlace(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{out: &buf, mu: &sync.Mutex{}}
	args := `{"path":"x.go","old_string":"a","new_string":"b"}`

	// Inline completion: the tool's header is the most-recently-rendered line.
	c.messages = []displayEntry{{typ: "tool", id: "call_1", name: "edit_file", args: args, done: false}}
	c.activeToolID = "call_1"

	meta := editpreview.MetadataFromArgs(args, "Edited x.go (1 replacement, lines 1-2).")
	if meta == nil {
		t.Fatal("test setup: expected non-nil edit_preview metadata")
	}
	c.handleEvent(agent.Event{
		Kind:       agent.EventToolCallEnd,
		ToolCallID: "call_1",
		ToolName:   "edit_file",
		Args:       args,
		Result:     "Edited x.go (1 replacement, lines 1-2).",
		Metadata:   meta,
	})

	// Inline edit_file with a summary upgrades the header in place via cursor-up.
	if !strings.Contains(buf.String(), "\x1b[1A") {
		t.Fatalf("inline edit_file end should rewrite the header in place (cursor-up): %q", buf.String())
	}
	if c.activeToolID != "" {
		t.Fatalf("activeToolID should be consumed by its end, got %q", c.activeToolID)
	}
}

func TestSubagentBackgroundProcessCompletionRenders(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.AppendUserMessage("root"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	root := a.SessionCurrent().ID
	var buf bytes.Buffer
	c := New(a)
	c.out = &buf
	c.setCurrentSessionID(root)
	c.handleEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child-1", ParentSessionID: root, SubagentSessionID: "child-1", TaskIndex: 2})
	buf.Reset()

	c.handleEvent(agent.Event{
		Kind:              agent.EventBackgroundProcessComplete,
		SessionID:         "child-1",
		SubagentSessionID: "child-1",
		TaskIndex:         2,
		Result:            "background done",
		BackgroundProcess: &agent.BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf done",
			Reason:   "completed",
			ExitCode: 0,
			Output:   "background done",
		},
	})

	rendered := buf.String()
	for _, want := range []string{"[subagent:task2]", "background_process", "bg-1", "completed exit 0", "background done"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("subagent background render missing %q: %q", want, rendered)
		}
	}
}

// extractFunctionBody returns the brace-delimited body of the first function
// whose definition line starts with prefix. It does not understand strings or
// comments containing braces, so callers should pass production code only.
func extractFunctionBody(source, prefix string) (string, bool) {
	idx := strings.Index(source, prefix)
	if idx < 0 {
		return "", false
	}
	rest := source[idx:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 1
	for i := open + 1; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open+1 : i], true
			}
		}
	}
	return "", false
}

func newTestAgent(t *testing.T) (*agent.Agent, string) {
	return newTestAgentWithBaseURL(t, "http://127.0.0.1:9/v1")
}

func cliUserContents(messages []agent.DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
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
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
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
