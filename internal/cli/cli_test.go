package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"golang.org/x/term"
)

func TestCmdClearClearsTerminalRedrawsHeaderAndKeepsMessages(t *testing.T) {
	a, projectName := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
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
	c.state.Store(int32(stateStreaming))

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
	c.width.Store(int32(80))

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
	c.width.Store(int32(80))

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
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
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
	c.state.Store(int32(stateStreaming))

	c.handleEvent(agent.Event{Kind: agent.EventCompactionEnd})

	if c.compacting {
		t.Fatal("compaction_end should clear compacting state")
	}
	if got := len(c.messages); got != 1 || c.messages[0].content != "System: live signal before compaction" {
		t.Fatalf("compaction_end alone rebuilt active live rows: %#v", c.messages)
	}
	if strings.Contains(out.String(), "persisted before compaction") {
		t.Fatalf("compaction_end alone refreshed persisted history: %q", out.String())
	}

	// The replacement transcript arrives as the rewrite boundary carrying the payload.
	out.Reset()
	payload, err := a.SessionPayloadForSession(a.SessionCurrent().ID)
	if err != nil {
		t.Fatalf("SessionPayloadForSession: %v", err)
	}
	c.handleEvent(agent.Event{Kind: agent.EventSessionRewrite, RewritePayload: &payload})
	if got := len(c.messages); got != 1 || c.messages[0].typ != "user" || c.messages[0].content != "persisted before compaction" {
		t.Fatalf("rewrite boundary should rebuild from the carried payload: %#v", c.messages)
	}
	if !strings.Contains(out.String(), "persisted before compaction") {
		t.Fatalf("rewrite boundary did not render the replacement: %q", out.String())
	}

	c.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 2})
	if c.busy || cliState(c.state.Load()) != stateIdle {
		t.Fatalf("turn_end should leave CLI idle: busy=%v state=%v", c.busy, cliState(c.state.Load()))
	}
}

func TestCLIStaleCurrent(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := a.AppendUserMessage("gone"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if err := a.SessionDelete(id); err != nil {
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
		run  func(*agent.Agent, string) error
	}{
		{name: "archive", run: func(a *agent.Agent, id string) error { return a.SessionArchive(id) }},
		{name: "delete", run: func(a *agent.Agent, id string) error { return a.SessionDelete(id) }},
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
			if err := tc.run(a, firstID); err != nil {
				t.Fatalf("%s first: %v", tc.name, err)
			}
			c.clearRemovedCurrent(firstID)
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
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
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
	if _, err := a.NewSession("", "primary"); err != nil {
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
	}
	c.state.Store(int32(stateStreaming))

	c.mu.Lock()
	idle := c.finishCompactLocked()
	c.mu.Unlock()

	if idle {
		t.Fatal("finishCompactLocked reported idle while a queued turn was running")
	}
	if !c.busy || cliState(c.state.Load()) != stateStreaming {
		t.Fatalf("finishCompactLocked overwrote queued turn state: busy=%v state=%v", c.busy, cliState(c.state.Load()))
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
	}
	c.state.Store(int32(stateStreaming))

	c.mu.Lock()
	idle := c.finishCompactLocked()
	c.mu.Unlock()

	if !idle || c.busy || cliState(c.state.Load()) != stateIdle {
		t.Fatalf("finishCompactLocked should restore idle state: idle=%v busy=%v state=%v", idle, c.busy, cliState(c.state.Load()))
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
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
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

func TestCLIExitReturnsExitError(t *testing.T) {
	a, _ := newTestAgent(t)
	c := New(a)
	err := c.dispatchCommand("/exit")
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 0 {
		t.Fatalf("/exit err = %v, want exit code 0", err)
	}
}

// TestCLISelectionOnlyContract proves /session, /resume, /new, and /project
// change only the current selection. For every command: an idle success
// retains the source session's live claim with no teardown, a busy CLI
// rejects the command unchanged, a destination failure leaves the source
// selection unchanged, and an idle success replaces the source's queue
// display with the destination's lower-version snapshot instead of dropping
// it as stale against the source's cursor.
func TestCLISelectionOnlyContract(t *testing.T) {
	for _, cmd := range []string{"/session", "/resume", "/new", "/project"} {
		cmd := cmd
		t.Run(cmd+"/idle_success=source_live_claim_retained_no_teardown", func(t *testing.T) {
			a, c, _, source := selectionCLI(t)
			switch cmd {
			case "/resume":
				// Cross-project destination: the selection commits the
				// destination project so later project-scoped routes target
				// it, and the project-scoped list then shows the destination.
				dest, otherRoot := sessionInOtherProject(t, c)
				dispatchSelection(t, c, cmd, dest)
				wantCurrent(t, c, dest)
				wantProjectCommitted(t, c, otherRoot, dest)
				// A destination whose metadata is unreadable still routes
				// there: the summary carries the resolved project.
				dest2, otherRoot2 := sessionInOtherProject(t, c)
				corruptSessionMeta(t, a, otherRoot2, dest2)
				dispatchSelection(t, c, cmd, dest2)
				wantCurrent(t, c, dest2)
				wantProjectPath(t, c, otherRoot2)
			case "/new":
				dispatchSelection(t, c, cmd, "")
				got, err := c.currentSession()
				if err != nil || got == "" || got == source {
					t.Fatalf("current after /new = %q (err %v), want a new session distinct from source", got, err)
				}
			case "/session":
				dest := secondSession(t, c)
				selectSessionByID(t, c, dest)
				dispatchSelection(t, c, cmd, "")
				wantCurrent(t, c, dest)
			case "/project":
				dest, otherRoot := sessionInOtherProject(t, c)
				selectProjectByPath(t, c, otherRoot)
				dispatchSelection(t, c, cmd, "")
				wantCurrent(t, c, dest)
				wantProjectCommitted(t, c, otherRoot, dest)
			}
			if !sourceClaimRetained(c, source) {
				t.Fatalf("source session %q lost its live claim", source)
			}
		})

		t.Run(cmd+"/busy=rejected_unchanged", func(t *testing.T) {
			_, c, out, source := selectionCLI(t)
			busyCmd := cmd
			if cmd == "/resume" {
				busyCmd = "/resume x"
			}
			c.handleSlashWhileBusy(busyCmd)
			if !strings.Contains(out.String(), "cannot run this command while a turn is running") {
				t.Fatalf("%q while busy = %q, want rejection", cmd, out.String())
			}
			if got, _ := c.currentSession(); got != source {
				t.Fatalf("%q while busy changed current to %q, want unchanged %q", cmd, got, source)
			}
		})

		t.Run(cmd+"/destination_failure=selection_source_claim_unchanged", func(t *testing.T) {
			a, c, out, source := selectionCLI(t)
			switch cmd {
			case "/resume":
				dispatchSelection(t, c, cmd, "does-not-exist")
			case "/new":
				// The destination project is unreadable: its meta record is
				// corrupt, so the new-session create cannot proceed.
				badPath := filepath.Join(t.TempDir(), "blocked")
				proj, err := project.EnsureForPath(a.Projects().Root(), badPath)
				if err != nil {
					t.Fatalf("EnsureForPath(%q): %v", badPath, err)
				}
				if err := os.WriteFile(filepath.Join(a.Projects().Root(), proj.ID, "meta.json"), []byte("{not json"), 0o600); err != nil {
					t.Fatalf("corrupt destination project meta: %v", err)
				}
				c.scope.SetProjectPath(badPath)
				dispatchSelection(t, c, cmd, "")
			case "/session":
				// The destination is listed but driven by another process
				// (its claim is held), so the open fails.
				dest := holderSession(t, a, c.scope.ProjectPath())
				selectSessionByID(t, c, dest)
				dispatchSelection(t, c, cmd, "")
				if !strings.Contains(out.String(), "driven by another process") {
					t.Fatalf("/session over a contended destination = %q, want the contention error", out.String())
				}
			case "/project":
				// The destination project's session is driven by another
				// process (its claim is held), so the switch fails.
				otherRoot := t.TempDir()
				holderSession(t, a, otherRoot)
				selectProjectByPath(t, c, otherRoot)
				dispatchSelection(t, c, cmd, "")
				if !strings.Contains(out.String(), "driven by another process") {
					t.Fatalf("/project over a contended destination = %q, want the contention error", out.String())
				}
			}
			if got, _ := c.currentSession(); got != source {
				t.Fatalf("current after failed %s = %q, want unchanged source %q", cmd, got, source)
			}
			if !sourceClaimRetained(c, source) {
				t.Fatalf("source session %q lost its live claim", source)
			}
		})

		t.Run(cmd+"/queue_version=destination_lower_than_source_replaces", func(t *testing.T) {
			_, c, _, _ := selectionCLI(t)
			// A high version cursor left over from the source session: the
			// destination's lower-version snapshot must replace it, not be
			// dropped as stale against the source sibling's cursor.
			c.lastQueueVersion = 100
			c.queueDisplay = []agent.QueuedItem{{Content: "stale source queue"}}
			switch cmd {
			case "/resume":
				dest := secondSession(t, c)
				dispatchSelection(t, c, cmd, dest)
			case "/new":
				dispatchSelection(t, c, cmd, "")
			case "/session":
				dest := secondSession(t, c)
				selectSessionByID(t, c, dest)
				dispatchSelection(t, c, cmd, "")
			case "/project":
				_, otherRoot := sessionInOtherProject(t, c)
				selectProjectByPath(t, c, otherRoot)
				dispatchSelection(t, c, cmd, "")
			}
			want := c.queueSnapshot()
			if c.lastQueueVersion != want.Version {
				t.Fatalf("queue cursor after %s = %d, want the destination's snapshot version %d", cmd, c.lastQueueVersion, want.Version)
			}
			if !reflect.DeepEqual(c.queueDisplay, want.Items) {
				t.Fatalf("queue display after %s = %#v, want the destination's %#v", cmd, c.queueDisplay, want.Items)
			}
		})
	}

	// The no-teardown barrier: no selection path calls a teardown operation,
	// so the source session is never stopped, cancelled, closed, detached,
	// evicted, or shut down by a selection.
	t.Run("no_teardown=selection_paths", func(t *testing.T) {
		for _, file := range []string{"cli.go", "menu.go"} {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			s := string(src)
			for _, fn := range selectionFuncsIn(file) {
				body, ok := extractFunctionBody(s, fn)
				if !ok {
					t.Fatalf("selection function %q not found in %s; the teardown scan must cover it", fn, file)
				}
				for _, bad := range []string{"CancelSession", "ShutdownOwner", "DetachAdapter", "closeEvents", "closeKeys"} {
					if strings.Contains(body, bad) {
						t.Fatalf("%s must not tear down the source session; found %s", fn, bad)
					}
				}
			}
		}
	})
}

// selectionCLI builds a CLI over a fresh agent with one source session
// selected and its output captured.
func selectionCLI(t *testing.T) (*agent.Agent, *CLI, *bytes.Buffer, string) {
	t.Helper()
	a, _ := newTestAgent(t)
	source, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("source NewSession: %v", err)
	}
	c := New(a)
	out := new(bytes.Buffer)
	c.out = out
	c.setCurrentSessionID(source)
	return a, c, out, source
}

// secondSession creates one more session in the CLI's project.
func secondSession(t *testing.T, c *CLI) string {
	t.Helper()
	id, err := c.agent.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return id
}

// sessionInOtherProject creates a session in a fresh project and returns the
// session id and the project path.
func sessionInOtherProject(t *testing.T, c *CLI) (string, string) {
	t.Helper()
	otherRoot := t.TempDir()
	dest, err := c.agent.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	return dest, otherRoot
}

// corruptSessionMeta makes the session's persisted metadata unreadable.
func corruptSessionMeta(t *testing.T, a *agent.Agent, projectPath, sessionID string) {
	t.Helper()
	proj, err := a.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("ProjectCurrentForPath(%q): %v", projectPath, err)
	}
	if err := os.WriteFile(filepath.Join(a.Projects().SessionsRoot(proj.ID), sessionID, "meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt destination meta: %v", err)
	}
}

// holderSession creates a session on disk through a bare snapshot store that
// keeps the session claim, standing in for a destination driven by another
// process: it is listed but cannot be opened here.
func holderSession(t *testing.T, a *agent.Agent, projectPath string) string {
	t.Helper()
	proj, err := a.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("ProjectCurrentForPath(%q): %v", projectPath, err)
	}
	holder, err := snapshot.NewForSessionsRoot(a.Projects().SessionsRoot(proj.ID), a.Projects().Root(), proj.ID)
	if err != nil {
		t.Fatalf("holder store: %v", err)
	}
	if err := holder.BeginNewSession(projectPath); err != nil {
		t.Fatalf("holder BeginNewSession: %v", err)
	}
	t.Cleanup(func() { holder.Detach() })
	return holder.SessionID()
}

// menuKeys returns a key source that navigates down n items and presses
// enter, selecting the n-th item (0-based) of a menu.
func menuKeys(n int) func() (keyMsg, error) {
	return func() (keyMsg, error) {
		if n > 0 {
			n--
			return keyMsg{Special: keyDown}, nil
		}
		return keyMsg{Special: keyEnter}, nil
	}
}

// selectSessionByID wires the key source to select the given session in the
// session menu, using the CLI's own project-scoped listing order.
func selectSessionByID(t *testing.T, c *CLI, id string) {
	t.Helper()
	sessions, err := c.agent.SessionListForProjectPath(c.scope.ProjectPath(), "active")
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	idx := -1
	for i, s := range sessions {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("destination %q not listed in the session menu", id)
	}
	c.readKeyFn = menuKeys(idx)
}

// selectProjectByPath wires the key source to select the given project in the
// project menu, using the host's own listing order.
func selectProjectByPath(t *testing.T, c *CLI, path string) {
	t.Helper()
	projects, err := c.agent.ProjectList()
	if err != nil {
		t.Fatalf("ProjectList: %v", err)
	}
	idx := -1
	for i, p := range projects {
		if p.Path == path {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("project %q not listed in the project menu", path)
	}
	c.readKeyFn = menuKeys(idx)
}

// dispatchSelection runs the given selection command against the wired CLI.
func dispatchSelection(t *testing.T, c *CLI, cmd, dest string) {
	t.Helper()
	var err error
	switch cmd {
	case "/resume":
		err = c.dispatchCommand("/resume " + dest)
	default:
		err = c.dispatchCommand(cmd)
	}
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
}

func wantCurrent(t *testing.T, c *CLI, want string) {
	t.Helper()
	if got, _ := c.currentSession(); got != want {
		t.Fatalf("current = %q, want %q", got, want)
	}
}

func wantProjectPath(t *testing.T, c *CLI, want string) {
	t.Helper()
	if got := c.scope.ProjectPath(); got != want {
		t.Fatalf("project path = %q, want %q", got, want)
	}
}

// wantProjectCommitted asserts the destination project is committed: the
// scope routes there and its project-scoped session list shows the
// destination.
func wantProjectCommitted(t *testing.T, c *CLI, otherRoot, dest string) {
	t.Helper()
	wantProjectPath(t, c, otherRoot)
	sessions, err := c.scope.SessionList("active")
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != dest {
		t.Fatalf("project-scoped session list after selection = %#v, want only %q", sessions, dest)
	}
}

// sourceClaimRetained reports whether the source session is still live and
// claimable after a selection.
func sourceClaimRetained(c *CLI, source string) bool {
	_, err := c.agent.SessionSummaryForSession(source)
	return err == nil
}

// TestAdapterOperationParity proves the contract route set is reachable on
// every host — the Wails bindings, the terminal host, and the ACP method
// table — and that the routes the contract excludes stay absent. The absent
// half is the point: it is what stops a later change quietly adding a route
// the contract excludes. The checks are structural: each cell asserts the
// named entry points exist (or do not exist) in the host's own source, not
// that they are reachable from the process entry.
func TestAdapterOperationParity(t *testing.T) {
	readHost := func(files ...string) string {
		t.Helper()
		var sb strings.Builder
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			sb.Write(b)
		}
		return sb.String()
	}
	wails := readHost("../../app.go", "../../main.go", "../../frontend/src/App.svelte")
	cli := readHost("cli.go", "menu.go", "input.go")
	acp := readHost("../acp/acp.go")

	type cell struct {
		adapter string
		route   string
		present bool
		needle  []string // present: every needle must exist; absent: none may
	}
	cells := []cell{
		// Session list / open-resume / new / switch / fork / archive / delete.
		{"wails", "session_list", true, []string{"func (a *App) SessionList("}},
		{"cli", "session_list", true, []string{`case "/session":`, "showSessionMenu("}},
		{"acp", "session_list", true, []string{`"session/list"`}},

		{"wails", "session_open_resume", true, []string{"func (a *App) SessionSwitch("}},
		{"cli", "session_open_resume", true, []string{`case "/resume":`, "func (c *CLI) cmdResume("}},
		{"acp", "session_open_resume", true, []string{`"session/switch"`}},

		{"wails", "session_new", true, []string{"func (a *App) SessionNew("}},
		{"cli", "session_new", true, []string{`case "/new":`, "NewSession("}},
		{"acp", "session_new", true, []string{`"session/new"`}},

		{"wails", "session_switch", true, []string{"func (a *App) SessionSwitch("}},
		{"cli", "session_switch", true, []string{"showSessionMenuWithActions(", "OpenSession("}},
		{"acp", "session_switch", true, []string{`"session/switch"`}},

		{"wails", "session_fork", true, []string{"func (a *App) ForkSession(", "func (a *App) ApplyTurnAction("}},
		{"cli", "session_fork", true, []string{`case "/fork":`, "showRevertMenu("}},
		{"acp", "session_fork", true, []string{`"session/fork"`}},

		{"wails", "session_archive", true, []string{"func (a *App) SessionArchive("}},
		{"cli", "session_archive", true, []string{"SessionArchive("}},
		{"acp", "session_archive", true, []string{`"session/archive"`}},

		{"wails", "session_delete", true, []string{"func (a *App) SessionDelete("}},
		{"cli", "session_delete", true, []string{"SessionDelete("}},
		{"acp", "session_delete", true, []string{`"session/delete"`}},

		// Submit / cancel.
		{"wails", "submit", true, []string{"func (a *App) Submit("}},
		{"cli", "submit", true, []string{"submitToBackend("}},
		{"acp", "submit", true, []string{`"session/prompt"`}},

		{"wails", "cancel", true, []string{"func (a *App) Cancel("}},
		{"cli", "cancel", true, []string{"cancelCurrent(", "case keyCtrlC, keyEscape:"}},
		{"acp", "cancel", true, []string{`"session/cancel"`}},

		// Queue; permission response.
		{"wails", "queue", true, []string{"func (a *App) QueueSnapshot("}},
		{"cli", "queue", true, []string{"queueSnapshot("}},
		{"acp", "queue", true, []string{`"queue/list"`}},

		{"wails", "permission_respond", true, []string{"func (a *App) RespondPermission("}},
		{"cli", "permission_respond", true, []string{"popAndRespondAction("}},
		{"acp", "permission_respond", true, []string{`"permission/respond"`}},

		{"wails", "permission_suggest", true, []string{"func (a *App) PermissionSuggest("}},
		{"cli", "permission_suggest", true, []string{"showPermissionSuggestions("}},
		{"acp", "permission_suggest", true, []string{`"permission/suggest"`}},

		{"wails", "permission_save", true, []string{"func (a *App) SaveProjectPermission("}},
		{"cli", "permission_save", true, []string{"SaveProjectPermissionForSession("}},
		{"acp", "permission_save", true, []string{`"permission/save"`}},

		// Selected-root model current / switch.
		{"wails", "model_current", true, []string{"func (a *App) CurrentModel("}},
		{"cli", "model_current", true, []string{`case "/model":`, "showModelMenu("}},
		{"acp", "model_current", true, []string{`"model/current"`}},

		{"wails", "model_switch", true, []string{"func (a *App) SwitchModel("}},
		{"cli", "model_switch", true, []string{"SwitchModelForSession("}},
		{"acp", "model_switch", true, []string{`"model/switch"`}},

		// Complete-state hydration / ordered events.
		{"wails", "session_current", true, []string{"func (a *App) SessionCurrent("}},
		{"cli", "session_current", true, []string{"refreshSessionLocked("}},
		{"acp", "session_current", true, []string{`"session/current"`}},

		{"wails", "session_messages", true, []string{"func (a *App) SessionMessages(", "func (a *App) SessionMessagesFor("}},
		{"cli", "session_messages", true, []string{"SessionMessagesFor("}},
		{"acp", "session_messages", true, []string{`"session/messages"`}},

		{"wails", "state_snapshots", true, []string{"func (a *App) HydrateSession(", "EventsOn('navigation'", "EventsOn('resync'"}},
		{"cli", "state_snapshots", true, []string{"refreshState("}},
		{"acp", "state_snapshots", true, []string{"pushResyncForEvent("}},

		{"wails", "events", true, []string{"EventsOn('turn_start'"}},
		{"cli", "events", true, []string{"func (c *CLI) handleEvent("}},
		{"acp", "events", true, []string{`"agent/turn_end"`}},

		// Project access.
		{"wails", "project_list", true, []string{"func (a *App) ProjectList("}},
		{"cli", "project_list", true, []string{`case "/project":`, "showProjectMenu("}},
		{"acp", "project_list", true, []string{`"project/list"`}},

		{"wails", "project_current", true, []string{"func (a *App) ProjectCurrent("}},
		{"cli", "project_current", true, []string{"ProjectCurrent("}},
		{"acp", "project_current", true, []string{`"project/current"`}},

		{"wails", "project_switch", true, []string{"func (a *App) ProjectSwitch(", "func (a *App) ProjectPickAndSwitch("}},
		{"cli", "project_switch", true, []string{"projectSwitch("}},
		{"acp", "project_switch", false, []string{`"project/switch"`}},

		// Snapshot / revert.
		{"wails", "snapshot_list", true, []string{"func (a *App) SnapshotList("}},
		{"cli", "snapshot_list", false, []string{`"/snapshot"`, "SnapshotList"}},
		{"acp", "snapshot_list", true, []string{`"snapshot/list"`}},

		{"wails", "revert_code", true, []string{"func (a *App) RevertCode("}},
		{"cli", "revert_code", true, []string{`case "/revert":`, "TurnActionRevertCode"}},
		{"acp", "revert_code", true, []string{`"session/revert_code"`}},

		{"wails", "revert_history", true, []string{"func (a *App) RevertHistory("}},
		{"cli", "revert_history", true, []string{"TurnActionRevertHistory"}},
		{"acp", "revert_history", true, []string{`"session/revert_history"`}},

		// Host shutdown trigger — and no shutdown RPC anywhere.
		{"wails", "shutdown_trigger", true, []string{"OnShutdown: app.shutdown"}},
		{"cli", "shutdown_trigger", true, []string{`case "/exit":`, "signal.Notify(intCh, syscall.SIGINT)", "signal.Notify(termCh, syscall.SIGTERM)"}},
		{"acp", "shutdown_trigger", true, []string{"signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)", "!scanner.Scan()"}},

		// A shutdown RPC is absent in every shape it could take on each host:
		// a Wails-bound shutdown/stop/exit-style method, a terminal slash
		// route naming the action, and an ACP method that is the bare
		// "shutdown" or any namespace ending in "/shutdown".
		{"wails", "shutdown_rpc", false, []string{"func (a *App) Shutdown", "func (a *App) Stop", "func (a *App) Exit", "func (a *App) Quit", "func (a *App) Terminate"}},
		{"cli", "shutdown_rpc", false, []string{`"/shutdown"`, `"/stop"`, `"/quit"`}},
		{"acp", "shutdown_rpc", false, []string{`"shutdown`, `/shutdown"`}},

		// The excluded provider/config routes (a Wails-only surface) and the
		// removed public process-management surface stay off the other hosts.
		{"cli", "provider_management", false, []string{"ConnectProvider(", "ProviderList("}},
		{"acp", "provider_management", false, []string{`"provider/`}},
		{"cli", "config_editing", false, []string{"SetRuntimeConfig(", "GetRuntimeConfig("}},
		{"acp", "config_editing", false, []string{`"runtime_config"`, `"config/set"`}},
		{"wails", "public_process_management", false, []string{"AttachAdapter", "WaitOwner", "runServe", "runStop"}},
		{"cli", "public_process_management", false, []string{`"/serve"`, `"/attach"`}},
		{"acp", "public_process_management", false, []string{`"owner/`, `"attach"`, `"serve"`}},
	}

	for _, cell := range cells {
		cell := cell
		status := "present"
		if !cell.present {
			status = "absent"
		}
		t.Run(fmt.Sprintf("adapter=%s/route=%s/%s", cell.adapter, cell.route, status), func(t *testing.T) {
			var host string
			switch cell.adapter {
			case "wails":
				host = wails
			case "cli":
				host = cli
			case "acp":
				host = acp
			default:
				t.Fatalf("unknown adapter %q", cell.adapter)
			}
			for _, needle := range cell.needle {
				ok := strings.Contains(host, needle)
				if cell.present && !ok {
					t.Fatalf("%s must expose route %q (%s); missing %q", cell.adapter, cell.route, status, needle)
				}
				if !cell.present && ok {
					t.Fatalf("%s must not expose route %q (%s); found %q", cell.adapter, cell.route, status, needle)
				}
			}
		})
	}
}

func selectionFuncsIn(file string) []string {
	switch file {
	case "cli.go":
		// dispatchCommand routes /new (inline), /session, /project, /resume; scanning
		// it covers the inline /new selection path that has no named handler.
		return []string{"func (c *CLI) dispatchCommand(", "func (c *CLI) cmdResume(", "func (c *CLI) projectSwitch("}
	case "menu.go":
		return []string{"func (c *CLI) showSessionMenuInner("}
	}
	return nil
}

// TestCLIQueueCursorDropsSameSessionStale proves a stale queue snapshot (older
// version) for the current session is dropped.
func TestCLIQueueCursorDropsSameSessionStale(t *testing.T) {
	a, _ := newTestAgent(t)
	c := New(a)
	c.out = new(bytes.Buffer)
	id, _ := a.NewSession("", "primary")
	c.setCurrentSessionID(id)

	c.lastQueueVersion = 10
	c.queueDisplay = []agent.QueuedItem{{Content: "keep"}}
	c.handleEvent(agent.Event{Kind: agent.EventQueueChanged, SessionID: id, QueueVersion: 5, Queue: []agent.QueuedItem{{Content: "stale"}}})
	if c.lastQueueVersion != 10 || len(c.queueDisplay) != 1 || c.queueDisplay[0].Content != "keep" {
		t.Fatalf("stale queue snapshot applied: version=%d display=%#v", c.lastQueueVersion, c.queueDisplay)
	}
	// A newer version for the same session applies.
	c.handleEvent(agent.Event{Kind: agent.EventQueueChanged, SessionID: id, QueueVersion: 11, Queue: []agent.QueuedItem{{Content: "fresh"}}})
	if c.lastQueueVersion != 11 || c.queueDisplay[0].Content != "fresh" {
		t.Fatalf("newer same-session queue not applied: version=%d display=%#v", c.lastQueueVersion, c.queueDisplay)
	}
}

// TestCLIRefreshResetsQueueCursor proves a destination replacement resets the one
// version cursor from a fresh snapshot, so the destination's lower queue version is
// not dropped against a higher source version.
func TestCLIRefreshResetsQueueCursor(t *testing.T) {
	a, _ := newTestAgent(t)
	c := New(a)
	c.out = new(bytes.Buffer)
	id, _ := a.NewSession("", "primary")
	c.setCurrentSessionID(id)

	c.lastQueueVersion = 100 // a high version left over from a prior session
	c.mu.Lock()
	c.refreshSessionLocked()
	c.mu.Unlock()
	if c.lastQueueVersion != c.queueSnapshot().Version {
		t.Fatalf("refresh did not reset the queue cursor: cursor=%d snapshot=%d", c.lastQueueVersion, c.queueSnapshot().Version)
	}
}

// TestCLIEventCallbackAbsorbsBurstWithoutBlocking proves the owner event callback
// never blocks, even when a synchronous owner call emits far more events than the
// former bounded channel held while mainLoop is stalled.
func TestCLIEventCallbackAbsorbsBurstWithoutBlocking(t *testing.T) {
	c := New(nil)
	const n = 1000 // well above the former 256 channel capacity
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			c.enqueueEvent(agent.Event{Kind: agent.EventTextDelta, Result: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event callback blocked emitting above the former channel capacity")
	}
	c.eventMu.Lock()
	got := len(c.eventFrames)
	c.eventMu.Unlock()
	if got != n {
		t.Fatalf("buffered events = %d, want %d (none dropped or blocked)", got, n)
	}
}

// TestCLIClosedEventCallbackDropsEvents proves that once event admission is closed
// the callback drops further events, and that a frame already queued is discarded
// at close, so shutdown joins the owner without a late event slipping into an
// abandoned queue and nothing queued at close is ever rendered.
func TestCLIClosedEventCallbackDropsEvents(t *testing.T) {
	c := New(nil)
	c.enqueueEvent(agent.Event{Kind: agent.EventTextDelta, Result: "queued before close"})
	c.closeEvents()
	c.enqueueEvent(agent.Event{Kind: agent.EventTextDelta, Result: "after close"})
	c.eventMu.Lock()
	got := len(c.eventFrames)
	c.eventMu.Unlock()
	if got != 0 {
		t.Fatalf("closed event admission kept %d frames (queued backlog or late admission), want 0", got)
	}
}

// TestHostDeliveryCloseDropPolicy proves the terminal host's close deliberately
// discards frames already queued: mainLoop has returned by teardown and nothing
// renders after it, so the backlog is dropped rather than drained — the protocol
// host drains its backlog because its client process is still reading the pipe,
// while the terminal has no consumer left at close.
func TestHostDeliveryCloseDropPolicy(t *testing.T) {
	c := New(nil)
	c.enqueueEvent(agent.Event{Kind: agent.EventTextDelta, Result: "queued"})
	c.closeEvents()
	c.eventMu.Lock()
	got := len(c.eventFrames)
	c.eventMu.Unlock()
	if got != 0 {
		t.Fatalf("close left %d queued frames to be rendered later; the terminal host drops the backlog at close", got)
	}
}

// TestCLIKeyReaderAbsorbsBurstWithoutBlocking proves the key reader never blocks on
// a bounded channel, so a stalled mainLoop cannot wedge it mid-send and keep it from
// its next stdin read.
func TestCLIKeyReaderAbsorbsBurstWithoutBlocking(t *testing.T) {
	c := New(nil)
	const n = 500 // well above the former 64 channel capacity
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			c.enqueueKey(keyMsg{Rune: 'x'})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("key reader blocked appending above the former channel capacity")
	}
	c.keyMu.Lock()
	got := len(c.keyFrames)
	c.keyMu.Unlock()
	if got != n {
		t.Fatalf("buffered keys = %d, want %d", got, n)
	}
}

// TestCLINextKeyReturnsBufferedKey proves the shared key source pops keys in order.
func TestCLINextKeyReturnsBufferedKey(t *testing.T) {
	c := New(nil)
	c.enqueueKey(keyMsg{Rune: 'a'})
	k, err := c.nextKey(context.Background())
	if err != nil || k.Rune != 'a' {
		t.Fatalf("nextKey = (%v, %v), want rune 'a'", k, err)
	}
}

// TestCLIExitLatchUnwindsKeyRead proves the exit latch takes priority over a
// buffered key, so a signal or EOF unwinds a nested menu read rather than consuming
// another key.
func TestCLIExitLatchUnwindsKeyRead(t *testing.T) {
	c := New(nil)
	c.enqueueKey(keyMsg{Rune: 'x'}) // a key is buffered
	c.requestExit(ExitError{Code: 7})
	_, err := c.nextKey(context.Background())
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("nextKey after exit = %v, want ExitError code 7 (latch priority over buffered key)", err)
	}
}

// TestCLITickAnimationRendersWhenActiveOnly proves mainLoop's ticker renders a
// spinner frame only while animation is active, so there is no free-running spinner.
func TestCLITickAnimationRendersWhenActiveOnly(t *testing.T) {
	var buf bytes.Buffer
	c := &CLI{out: &buf, mu: &sync.Mutex{}}

	c.tickAnimation()
	if buf.Len() != 0 {
		t.Fatalf("inactive tick wrote %q, want nothing", buf.String())
	}

	c.mu.Lock()
	c.startAnimationLocked("Thinking")
	c.mu.Unlock()
	buf.Reset()
	c.tickAnimation()
	if !strings.Contains(buf.String(), "Thinking") {
		t.Fatalf("active tick = %q, want a frame carrying the label", buf.String())
	}

	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()
	buf.Reset()
	c.tickAnimation()
	if buf.Len() != 0 {
		t.Fatalf("stopped tick wrote %q, want nothing", buf.String())
	}
}

// TestCLIOpGroupRejectsAfterClose proves the op-group admits members while open and
// rejects them (without running) once admission is closed.
func TestCLIOpGroupRejectsAfterClose(t *testing.T) {
	c := New(nil)
	if !c.spawnOp(func() {}) {
		t.Fatal("spawnOp should admit while open")
	}
	c.opWG.Wait()
	c.closeOpAdmission()
	ran := make(chan struct{}, 1)
	if c.spawnOp(func() { ran <- struct{}{} }) {
		t.Fatal("spawnOp must reject after closeOpAdmission")
	}
	select {
	case <-ran:
		t.Fatal("a rejected op must not run")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestCLIOpGroupJoinsMembers proves opWG.Wait joins an in-flight member, so host exit
// does not abandon a running submit/compact.
func TestCLIOpGroupJoinsMembers(t *testing.T) {
	c := New(nil)
	release := make(chan struct{})
	done := make(chan struct{})
	if !c.spawnOp(func() { <-release; close(done) }) {
		t.Fatal("spawnOp should admit")
	}
	c.closeOpAdmission()

	joined := make(chan struct{})
	go func() { c.opWG.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("opWG.Wait returned before the member finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("member did not run")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("opWG.Wait did not return after the member finished")
	}
}

// TestCLIAsyncOpsUseOpGroup proves submit and compact run through the op-group (so
// they are admission-gated and joined on exit) rather than bare goroutines.
func TestCLIAsyncOpsUseOpGroup(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	s := string(src)
	// submit and compact always run through the op-group; cmdCopy runs its clipboard
	// fallback through it (its OSC-52 fast path is synchronous mainLoop output).
	for _, fn := range []string{"func (c *CLI) submitToBackend(", "func (c *CLI) cmdCompact(", "func (c *CLI) cmdCopy("} {
		body, ok := extractFunctionBody(s, fn)
		if !ok {
			t.Fatalf("%s not found", fn)
		}
		if strings.Contains(body, "go func") {
			t.Fatalf("%s must run through spawnOp, not a bare goroutine", fn)
		}
		if !strings.Contains(body, "c.spawnOp(") {
			t.Fatalf("%s must run through the op-group (spawnOp)", fn)
		}
	}
}

// TestCLISignalPathDoesNotWriteTerminal proves the signal handler does not
// write os.Stdout: signals only requestExit or cancel the current turn, and the
// deferred restoreTerminal shows the cursor. Keeps the terminal writer single.
func TestCLISignalPathDoesNotWriteTerminal(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	body, ok := extractFunctionBody(string(src), "func (c *CLI) handleSignal(")
	if !ok {
		t.Fatal("handleSignal not found")
	}
	if strings.Contains(body, "os.Stdout") {
		t.Fatal("handleSignal must not write os.Stdout; the signal path only requestExit or cancels, and restoreTerminal shows the cursor")
	}
}

// TestCLIAnimationHasNoSpinnerGoroutine proves the spinner runs through mainLoop's
// ticker rather than a separate goroutine, keeping mainLoop the sole terminal writer.
func TestCLIAnimationHasNoSpinnerGoroutine(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	body, ok := extractFunctionBody(string(src), "func (c *CLI) startAnimationLocked(")
	if !ok {
		t.Fatal("startAnimationLocked not found")
	}
	if strings.Contains(body, "go func") {
		t.Fatal("startAnimationLocked must not spawn a goroutine; animation renders via mainLoop's ticker")
	}
}

// TestCLIPopKeyReportsEmptyAfterExit proves latch priority is enforced atomically at
// the single pop point: once exit is requested, popKey reports empty even with keys
// buffered, so no key read (mainLoop or a nested menu) can pop and act on a queued
// key — e.g. a buffered Enter cannot fire a revert during signal shutdown.
func TestCLIPopKeyReportsEmptyAfterExit(t *testing.T) {
	c := New(nil)
	c.enqueueKey(keyMsg{Rune: 'a'})
	c.enqueueKey(keyMsg{Rune: 'b'})
	c.requestExit(ExitError{Code: 0})
	if _, ok := c.popKey(); ok {
		t.Fatal("popKey returned a buffered key after exit was requested; latch priority must gate popping")
	}
}

// TestCLINextKeyObservesContextCancel proves a cancelled context releases a blocked
// key read.
func TestCLINextKeyObservesContextCancel(t *testing.T) {
	c := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.nextKey(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("nextKey with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestCLIReadKeysSetsExitLatchBeforeClosingKeys proves the stdin-EOF path establishes
// the exit latch before closing key admission. closeKeys wakes a reader, so if it ran
// first a reader could observe an open latch and pop and act on a buffered key (a
// full key backlog after EOF) instead of unwinding to shutdown.
func TestCLIReadKeysSetsExitLatchBeforeClosingKeys(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	body, ok := extractFunctionBody(string(src), "func (c *CLI) readKeys(")
	if !ok {
		t.Fatal("readKeys not found")
	}
	req := strings.Index(body, "c.requestExit(")
	clo := strings.Index(body, "c.closeKeys()")
	if req < 0 || clo < 0 || req > clo {
		t.Fatalf("readKeys must set the exit latch (requestExit) before closing key admission (closeKeys); requestExit@%d closeKeys@%d", req, clo)
	}
}

func TestCLIDoesNotCallOSExit(t *testing.T) {
	data, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	if strings.Contains(string(data), "os.Exit(") {
		t.Fatal("CLI must return through Run so lifecycle defers execute, not call os.Exit")
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

// recordingPermissionAdapter wraps the real agent and records permission
// answers, so a test can assert that a terminal exit performs no answer
// instead of inferring it from the agent's gate.
type recordingPermissionAdapter struct {
	agent.AdapterService
	answers []string
}

func (r *recordingPermissionAdapter) RespondPermissionActionForSession(sessionID, id, action string) error {
	r.answers = append(r.answers, action)
	return r.AdapterService.RespondPermissionActionForSession(sessionID, id, action)
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

// TestCLIPermissionStaleAnswerIsVoid proves an answer for a request that was
// resolved underneath the user (removed from the displayed queue) is dropped
// without answering the next request or surfacing an error.
func TestCLIPermissionStaleAnswerIsVoid(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	c := New(a)
	sid := a.SessionCurrent().ID
	c.setCurrentSessionID(sid)

	req1 := &agent.PermissionRequest{ID: "p1", SessionID: sid, ToolName: "read_file", Arg: "/tmp/a.txt"}
	req2 := &agent.PermissionRequest{ID: "p2", SessionID: sid, ToolName: "read_file", Arg: "/tmp/b.txt"}
	var out bytes.Buffer
	c.out = &out
	c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req1})
	c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req2})
	// The resolution for the displayed request lands while the user's answer for
	// it is in flight.
	c.handleEvent(agent.Event{Kind: agent.EventPermissionResolved, SessionID: sid, PermReq: &agent.PermissionRequest{ID: "p1", SessionID: sid}})

	out.Reset()
	c.popAndRespondAction(req1, "deny")

	c.mu.Lock()
	queue := append([]*agent.PermissionRequest(nil), c.permQueue...)
	c.mu.Unlock()
	if len(queue) != 1 || queue[0].ID != "p2" {
		t.Fatalf("queue after stale answer = %#v, want [p2] untouched", queue)
	}
	if got := out.String(); strings.Contains(got, "no pending permission request") {
		t.Fatalf("stale answer surfaced the unknown-request error: %q", got)
	}
}

// TestCLIPermissionNeverPendingAnswerIsReported proves an answer for an id this
// host never issued is surfaced rather than swallowed.
// TestCLIPermissionStaleSaveIsSilent proves the allow-for-project save for a
// request resolved underneath the user is a void no-op, not a printed error.
func TestCLIPermissionStaleSaveIsSilent(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	c := New(a)
	sid := a.SessionCurrent().ID
	c.setCurrentSessionID(sid)

	req := &agent.PermissionRequest{ID: "p1", SessionID: sid, ToolName: "read_file", Arg: "/tmp/a.txt"}
	var out bytes.Buffer
	c.out = &out
	c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req})
	// The resolution lands while the suggestion menu is open.
	c.handleEvent(agent.Event{Kind: agent.EventPermissionResolved, SessionID: sid, PermReq: &agent.PermissionRequest{ID: "p1", SessionID: sid}})

	out.Reset()
	c.readKeyFn = func() (keyMsg, error) { return keyMsg{Special: keyEnter}, nil }
	c.showPermissionSuggestions(req)

	if got := out.String(); strings.Contains(got, "no pending permission request") {
		t.Fatalf("stale save surfaced the unknown-request error: %q", got)
	}
}

// TestCLIPermissionNeverPendingSaveIsReported proves the allow-for-project save
// for an id this host never issued is surfaced rather than swallowed.
// TestCLIPermissionCancellationBetweenCheckAndAnswerIsSilent proves a
// resolution landing after the display decision but before the agent call does
// not surface the unknown-request error: the decision is on whether this host
// ever issued the id, which a later cancellation does not retract.
// TestCLIPermissionCancellationBetweenCheckAndSaveIsSilent proves a resolution
// landing after the display decision but before the save call does not surface
// the unknown-request error.
// TestCLIPermissionPreviousTurnIdReportedAfterTurnEnd proves an answer or save
// naming an id from a previous turn is reported once the turn has ended: the
// record is cleared at turn end, so a recycled id is not silently swallowed.
// TestCLIPermissionTurnEndBetweenCheckAndCallIsSilent proves a turn end landing
// after the membership capture but before the agent call does not surface the
// unknown-request error: the decision is made on the captured membership, which
// the clear at turn end does not retract. Once the turn has ended, the same id
// is reported.
// TestCLIPermissionOtherSessionIdReported proves an id issued for one session
// does not authorise suppression for an answer naming a different session.

// TestCLIPermissionResolvedBehindHeadLeavesDisplayAlone proves a resolution for
// a queued request behind the displayed head removes it from the queue without
// touching the displayed prompt: the head and the permission state stay put.
func TestCLIPermissionResolvedBehindHeadLeavesDisplayAlone(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	c := New(a)
	sid := a.SessionCurrent().ID
	c.setCurrentSessionID(sid)

	req1 := &agent.PermissionRequest{ID: "p1", SessionID: sid, ToolName: "read_file", Arg: "/tmp/a.txt"}
	req2 := &agent.PermissionRequest{ID: "p2", SessionID: sid, ToolName: "read_file", Arg: "/tmp/b.txt"}
	var out bytes.Buffer
	c.out = &out
	c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req1})
	c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req2})

	// The resolution lands for the queued request behind the displayed head.
	c.handleEvent(agent.Event{Kind: agent.EventPermissionResolved, SessionID: sid, PermReq: &agent.PermissionRequest{ID: "p2", SessionID: sid}})

	c.mu.Lock()
	queue := append([]*agent.PermissionRequest(nil), c.permQueue...)
	state := cliState(c.state.Load())
	c.mu.Unlock()
	if len(queue) != 1 || queue[0].ID != "p1" {
		t.Fatalf("queue after behind-head resolution = %#v, want [p1]", queue)
	}
	if state != statePermission {
		t.Fatalf("state = %v, want statePermission (the displayed prompt is untouched)", state)
	}
}

// blockingWriter signals the first Write and blocks every Write until release
// closes, standing in for a stdout that stopped being read: mainLoop stalls
// inside writeRaw holding c.mu.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return len(p), nil
}

// TestCLIRunShutdownContract proves the exit is reachable while a terminal
// write is stalled, and that the host sequence does not sit behind mainLoop.
// The trigger is the point: a test that starts from an already-requested exit
// bypasses the defect, which is that mainLoop holds c.mu inside a blocked
// writeRaw and the SIGINT branch used to take c.mu to read state.
// Exception, recorded per the contract-test rule: Run cannot be driven in a
// test (term.MakeRaw on os.Stdin requires a real TTY), so the trigger is
// driven through handleSignal, the extracted signal branch, against mainLoop
// run directly, and the teardown decoupling is pinned structurally against
// the Run body.
func TestCLIRunShutdownContract(t *testing.T) {
	t.Run("trigger=blocked_write", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(nil)
		c.out = &blockingWriter{entered: entered, release: release}
		c.readKeyFn = func() (keyMsg, error) { return keyMsg{}, nil }

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mainDone := make(chan error, 1)
		go func() { mainDone <- c.mainLoop(ctx) }()

		// Pump one event so mainLoop enters handleEvent -> writeRaw and stalls
		// inside the write, holding c.mu.
		c.enqueueEvent(agent.Event{Kind: agent.EventTextDelta, Result: "blocked"})
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("mainLoop did not enter the blocked write")
		}

		// The signal trigger while the write is stalled: the SIGINT branch's
		// state read must not block on c.mu, or the branch never reaches
		// requestExit and the exit is never triggered at all.
		triggered := make(chan struct{})
		go func() { c.handleSignal(syscall.SIGINT); close(triggered) }()
		select {
		case <-triggered:
		case <-time.After(5 * time.Second):
			t.Fatal("SIGINT branch blocked behind the stalled write's c.mu; the exit is never triggered")
		}
		select {
		case <-c.exitLatch:
		default:
			t.Fatal("signal did not trigger the exit latch while the write was blocked")
		}

		// mainLoop is still abandoned in the blocked write: the host sequence
		// must not sit behind it.
		select {
		case <-mainDone:
			t.Fatal("mainLoop returned while its write was still blocked")
		default:
		}

		// Unblock the write: mainLoop unwinds to the latch and the exit
		// completes with the signal's code.
		close(release)
		select {
		case err := <-mainDone:
			var exit ExitError
			if !errors.As(err, &exit) || exit.Code != 130 {
				t.Fatalf("mainLoop returned %v, want ExitError{130}", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("mainLoop did not unwind to the exit after the write unblocked")
		}
	})

	t.Run("teardown=independent_of_mainLoop", func(t *testing.T) {
		src, err := os.ReadFile("cli.go")
		if err != nil {
			t.Fatalf("read cli.go: %v", err)
		}
		body, ok := extractFunctionBody(string(src), "func (c *CLI) Run(")
		if !ok {
			t.Fatal("Run not found")
		}
		// mainLoop must not run inline in Run: the inline form puts the whole
		// teardown behind a blocked terminal write.
		if strings.Contains(body, "err = c.mainLoop(ctx)") {
			t.Fatal("Run must not run mainLoop inline; teardown would sit behind a blocked terminal write")
		}
		// Run must wait on the exit latch, so a signal starts teardown while
		// mainLoop is still blocked, and the teardown must follow that wait.
		latchIdx := strings.Index(body, "case <-c.exitLatch:")
		teardownIdx := strings.Index(body, "c.closeKeys()")
		if latchIdx < 0 {
			t.Fatal("Run must wait on the exit latch so teardown does not sit behind mainLoop")
		}
		if teardownIdx < 0 || teardownIdx < latchIdx {
			t.Fatal("Run's teardown must follow the latch wait, not mainLoop's return")
		}
	})

	t.Run("watcher=resize_then_terminate_lock_held", func(t *testing.T) {
		// The signal watcher is a single goroutine servicing every signal
		// from one channel, so no branch of it may wait on the render lock:
		// a stalled write holds c.mu, and any branch that waits on it strands
		// every later signal, including the one that would trigger the exit.
		// The rule is swept structurally across every branch. Exception,
		// recorded per the contract-test rule: the resize branch only takes
		// the lock after term.GetSize succeeds, which needs a real terminal,
		// so the no-lock sweep is structural and the ordering is behavioral.
		src, err := os.ReadFile("cli.go")
		if err != nil {
			t.Fatalf("read cli.go: %v", err)
		}
		body, ok := extractFunctionBody(string(src), "func (c *CLI) handleSignal(")
		if !ok {
			t.Fatal("handleSignal not found")
		}
		if strings.Contains(body, "c.mu.") {
			t.Fatal("a signal watcher branch takes the render lock; a stalled write holds it and strands every later signal")
		}

		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(nil)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0

		// A stalled write holds the render lock.
		c.mu.Lock()
		defer c.mu.Unlock()

		// A resize is serviced promptly even while the lock is held...
		resized := make(chan struct{})
		go func() { c.handleSignal(syscall.SIGWINCH); close(resized) }()
		select {
		case <-resized:
		case <-time.After(5 * time.Second):
			t.Fatal("SIGWINCH branch did not return with the render lock held")
		}
		// ...and the terminate that follows it on the same watcher still
		// reaches the exit latch.
		c.handleSignal(syscall.SIGTERM)
		select {
		case <-c.exitLatch:
		default:
			t.Fatal("terminate did not reach the exit latch after a resize")
		}
	})

	t.Run("watcher=signals_split_by_purpose", func(t *testing.T) {
		// The runtime delivers signals non-blockingly and drops one whose
		// channel is full, so every signal that can independently require its
		// own action must be registered alone on its own channel: a shared
		// channel lets one kind's burst discard a different kind (a resize
		// burst or two cancelling interrupts can fill it and drop the
		// terminate behind them), and with the render stalled nothing sets
		// the exit latch.
		src, err := os.ReadFile("cli.go")
		if err != nil {
			t.Fatalf("read cli.go: %v", err)
		}
		body, ok := extractFunctionBody(string(src), "func (c *CLI) Run(")
		if !ok {
			t.Fatal("Run not found")
		}
		signals := []string{"syscall.SIGWINCH", "syscall.SIGINT", "syscall.SIGTERM"}
		for _, sig := range signals {
			idx := strings.Index(body, sig)
			if idx < 0 {
				t.Fatalf("Run no longer registers %s", sig)
			}
			notifyStart := strings.LastIndex(body[:idx], "signal.Notify(")
			if notifyStart < 0 {
				t.Fatalf("%s is not registered via signal.Notify", sig)
			}
			notifyEnd := strings.Index(body[notifyStart:], ")")
			reg := body[notifyStart : notifyStart+notifyEnd+1]
			for _, other := range signals {
				if other != sig && strings.Contains(reg, other) {
					t.Fatalf("%s must be registered alone on its own channel, not with %s", sig, other)
				}
			}
		}
		wb, ok := extractFunctionBody(string(src), "func (c *CLI) watchSignals(")
		if !ok {
			t.Fatal("watchSignals not found")
		}
		if strings.Count(wb, "case sig := <-") != 3 {
			t.Fatal("watchSignals must select on the resize, interrupt and terminate channels")
		}
	})

	t.Run("watcher=two_interrupts_then_terminate", func(t *testing.T) {
		// deliver simulates the runtime's signal delivery: a non-blocking
		// send that drops the signal when the channel is full.
		deliver := func(ch chan os.Signal, sig os.Signal) bool {
			select {
			case ch <- sig:
				return true
			default:
				return false
			}
		}

		// Two interrupts arrive during a turn, so neither exits. They ride
		// their own channel and the terminate rides its own, so the queued
		// cancels can never occupy the slot a terminate needs.
		resizeCh := make(chan os.Signal, 1)
		intCh := make(chan os.Signal, 2)
		termCh := make(chan os.Signal, 1)
		deliver(intCh, syscall.SIGINT)
		deliver(intCh, syscall.SIGINT)
		terminateDelivered := deliver(termCh, syscall.SIGTERM)
		if !terminateDelivered {
			t.Fatal("terminate was dropped despite its own channel")
		}

		c := New(nil)
		c.state.Store(int32(stateStreaming)) // a turn is running: SIGINT cancels, not exits
		c.mu.Lock()                          // a stalled write holds the render lock
		defer c.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go c.watchSignals(termCh, intCh, resizeCh, ctx)

		select {
		case <-c.exitLatch:
		case <-time.After(5 * time.Second):
			t.Fatalf("terminate did not reach the exit latch after two cancelling interrupts (delivered=%v)", terminateDelivered)
		}
	})

	t.Run("watcher=resize_burst_then_terminate", func(t *testing.T) {
		// deliver simulates the runtime's signal delivery: a non-blocking
		// send that drops the signal when the channel is full.
		deliver := func(ch chan os.Signal, sig os.Signal) bool {
			select {
			case ch <- sig:
				return true
			default:
				return false
			}
		}

		// A resize burst arrives before the watcher drains the channel: the
		// first resize occupies the one slot, the rest are dropped (resizes
		// coalesce, so that is harmless). The terminate rides its own
		// channel, so the burst cannot displace it.
		resizeCh := make(chan os.Signal, 1)
		intCh := make(chan os.Signal, 2)
		termCh := make(chan os.Signal, 1)
		deliver(resizeCh, syscall.SIGWINCH)
		deliver(resizeCh, syscall.SIGWINCH)
		deliver(resizeCh, syscall.SIGWINCH)
		terminateDelivered := deliver(termCh, syscall.SIGTERM)
		if !terminateDelivered {
			t.Fatal("terminate was dropped despite its own channel")
		}

		c := New(nil)
		c.mu.Lock() // a stalled write holds the render lock
		defer c.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go c.watchSignals(termCh, intCh, resizeCh, ctx)

		select {
		case <-c.exitLatch:
		case <-time.After(5 * time.Second):
			t.Fatalf("terminate did not reach the exit latch after a resize burst (delivered=%v)", terminateDelivered)
		}
	})

	t.Run("permission_prompt=exit_performs_no_answer", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the key
		// source is injected and the permission key path is driven through
		// handleKey, mainLoop's own entry.
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		c := New(a)
		sid := a.SessionCurrent().ID
		c.setCurrentSessionID(sid)
		var out bytes.Buffer
		c.out = &out

		// Record any permission answer the flow would dispatch, so "no answer"
		// is asserted on the agent call, not inferred from the gate.
		rec := &recordingPermissionAdapter{AdapterService: a}
		c.agent = rec

		req := &agent.PermissionRequest{ID: "p1", SessionID: sid, ToolName: "read_file", Arg: "/tmp/a.txt"}
		c.handleEvent(agent.Event{Kind: agent.EventPermissionRequest, SessionID: sid, PermReq: req})

		// The terminal exits while the suggestion menu is open: the key read
		// reports the exit error, exactly as nextKey does once the latch is set.
		c.requestExit(ExitError{Code: 130})
		c.readKeyFn = func() (keyMsg, error) { return keyMsg{}, c.exitErr }

		err := c.handleKey(keyMsg{Rune: 'p'})
		var exit ExitError
		if !errors.As(err, &exit) || exit.Code != 130 {
			t.Fatalf("handleKey returned %v, want the exit error", err)
		}
		c.mu.Lock()
		queue := append([]*agent.PermissionRequest(nil), c.permQueue...)
		c.mu.Unlock()
		if len(queue) != 1 || queue[0].ID != req.ID {
			t.Fatalf("permission queue after exit = %#v, want [p1] unanswered", queue)
		}
		if len(rec.answers) != 0 {
			t.Fatalf("exit recorded an answer the user never made: %v", rec.answers)
		}
	})

	t.Run("revert_confirm=exit_performs_no_revert", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the
		// command chain is driven through handleKeyIdle — the key handler
		// mainLoop calls — with an injected key source; the confirmation read
		// failure is the exit error nextKey reports once the latch is set. The
		// error must reach the handler: a confirmation that consumes it lets
		// the loop render another prompt, and the revert proceeds as a "no".
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := a.AppendUserMessage("seed message"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		c := New(a)
		sid := a.SessionCurrent().ID
		c.setCurrentSessionID(sid)
		var out bytes.Buffer
		c.out = &out

		before, err := a.SessionMessagesFor(sid)
		if err != nil {
			t.Fatalf("SessionMessagesFor before: %v", err)
		}

		// Select the seeded turn, then "Revert history"; the confirmation key
		// read then reports the exit error.
		keys := []keyMsg{
			{Special: keyEnter},
			{Special: keyDown},
			{Special: keyEnter},
		}
		next := 0
		c.requestExit(ExitError{Code: 130})
		c.readKeyFn = func() (keyMsg, error) {
			if next < len(keys) {
				k := keys[next]
				next++
				return k, nil
			}
			return keyMsg{}, c.exitErr
		}

		c.input.Set("/revert")
		err = c.handleKeyIdle(keyMsg{Special: keyEnter})
		var exit ExitError
		if !errors.As(err, &exit) || exit.Code != 130 {
			t.Fatalf("handleKeyIdle returned %v, want the confirmation's exit error", err)
		}

		after, err := a.SessionMessagesFor(sid)
		if err != nil {
			t.Fatalf("SessionMessagesFor after: %v", err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("exit during the revert confirmation performed a revert: before=%v after=%v", before, after)
		}
	})

	t.Run("revert_error_refreshes_reconciled_transcript", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the
		// command chain is driven through handleKeyIdle with an injected key
		// source. A history revert that stops partway still removed every turn
		// above the one it stopped at, so the loop is reconciled to disk and
		// the error branch must refresh the view: the user must not keep
		// looking at turns that are gone from both loop and disk.
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		lightcodeDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LIGHTCODE_TEST_KEY", "test")
		configPath := filepath.Join(lightcodeDir, "config.json")
		if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
		if err != nil {
			t.Fatalf("new agent: %v", err)
		}
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		for i := 1; i <= 10; i++ {
			if _, err := a.AppendUserMessage(fmt.Sprintf("turn %d", i)); err != nil {
				t.Fatalf("seed turn %d: %v", i, err)
			}
		}
		sid := a.SessionCurrent().ID
		matches, err := filepath.Glob(filepath.Join(lightcodeDir, "projects", "*", "sessions", sid))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 1 {
			t.Fatalf("session dirs matching %q = %v, want exactly one", sid, matches)
		}
		blocked := filepath.Join(matches[0], "turns", "7")
		if err := os.Chmod(blocked, 0o555); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(blocked, 0o700) }()

		c := New(a)
		c.setCurrentSessionID(sid)
		var out bytes.Buffer
		c.out = &out
		c.refreshSession()
		if len(c.messages) != 10 {
			t.Fatalf("initial transcript has %d entries, want 10", len(c.messages))
		}

		// Pick the first user turn, choose "Revert history", and answer the
		// code confirmation with Escape (No).
		keys := []keyMsg{
			{Special: keyEnter},
			{Special: keyDown},
			{Special: keyEnter},
			{Special: keyEscape},
		}
		next := 0
		c.readKeyFn = func() (keyMsg, error) {
			if next < len(keys) {
				k := keys[next]
				next++
				return k, nil
			}
			return keyMsg{}, fmt.Errorf("unexpected key read")
		}

		c.input.Set("/revert")
		if err := c.handleKeyIdle(keyMsg{Special: keyEnter}); err != nil {
			t.Fatalf("handleKeyIdle: %v", err)
		}
		// The error branch re-renders the transcript over the reconciled
		// loop: turns 8-10 are gone from both loop and disk, so they must be
		// gone from the display too.
		if len(c.messages) != 7 {
			t.Fatalf("transcript after partial revert = %d entries, want 7 (turns 1-7): the error branch must refresh the view", len(c.messages))
		}
		for i, m := range c.messages {
			if m.turn != i+1 {
				t.Fatalf("display entry %d = turn %d, want turn %d", i, m.turn, i+1)
			}
		}
		if !strings.Contains(out.String(), "turn 7") {
			t.Fatalf("revert error not shown in output:\n%s", out.String())
		}
	})

	t.Run("fork_confirm=exit_performs_no_fork", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the
		// command chain is driven through handleKeyIdle — the key handler
		// mainLoop calls — with an injected key source; the confirmation read
		// failure is the exit error nextKey reports once the latch is set. A
		// guard that lets the fork through publishes a new session and the
		// menu switches the selection to it, so both are asserted: the error
		// reaches the handler and the selection stays on the source session.
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := a.AppendUserMessage("seed message"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		c := New(a)
		sid := a.SessionCurrent().ID
		c.setCurrentSessionID(sid)
		var out bytes.Buffer
		c.out = &out

		// Select the seeded turn, then "Fork from here"; the confirmation key
		// read then reports the exit error.
		keys := []keyMsg{
			{Special: keyEnter},
			{Special: keyDown},
			{Special: keyDown},
			{Special: keyEnter},
		}
		next := 0
		c.requestExit(ExitError{Code: 130})
		c.readKeyFn = func() (keyMsg, error) {
			if next < len(keys) {
				k := keys[next]
				next++
				return k, nil
			}
			return keyMsg{}, c.exitErr
		}

		c.input.Set("/fork")
		err := c.handleKeyIdle(keyMsg{Special: keyEnter})
		var exit ExitError
		if !errors.As(err, &exit) || exit.Code != 130 {
			t.Fatalf("handleKeyIdle returned %v, want the confirmation's exit error", err)
		}

		// A fork publishes a new session and the menu switches the selection
		// to it; neither may happen on a failed confirmation read.
		if got := c.currentSessionID(); got != sid {
			t.Fatalf("fork switched the selection to %q on a failed confirmation read", got)
		}
		after, err := a.SessionMessagesFor(sid)
		if err != nil {
			t.Fatalf("SessionMessagesFor after: %v", err)
		}
		if len(after) != 1 || after[0].Content != "seed message" {
			t.Fatalf("source session changed on a failed confirmation read: %v", after)
		}
	})
}

// TestCLIRestoreTerminalUnblockedByStalledWrite proves restoreTerminal only
// restores the terminal mode and never writes the output, whatever the output
// is doing and whoever holds the render lock: no exit path may depend on the
// output draining, and no fact available at exit — the lock's state or the
// loop having finished — establishes that the output is writable. A filled
// pipe with a dead reader hangs any write, so the cursor escape was removed
// outright. What is given up: exiting mid-spinner leaves the cursor hidden
// until the next prompt redraws it (the spinner shows the cursor again when
// it stops, so the exit-time write only ever covered the mid-run exit).
func TestCLIRestoreTerminalUnblockedByStalledWrite(t *testing.T) {
	t.Run("output_normal_nothing_written", func(t *testing.T) {
		var out bytes.Buffer
		c := New(nil)
		c.out = &out
		c.rawFd = 0
		c.oldState = &term.State{}

		c.restoreTerminal()

		if c.oldState != nil {
			t.Fatal("restoreTerminal did not restore the terminal mode")
		}
		if out.Len() != 0 {
			t.Fatalf("restoreTerminal wrote to the output: %q", out.String())
		}
	})

	t.Run("stalled_output_lock_free_writes_nothing", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(nil)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0
		c.oldState = &term.State{}

		// No render lock is held — the shape that exposed the last guard: a
		// stalled write from the lock-free clipboard path coexists with a
		// free lock.
		done := make(chan struct{})
		go func() {
			c.restoreTerminal()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("restoreTerminal did not return with the output stalled and the lock free")
		}
		if c.oldState != nil {
			t.Fatal("restoreTerminal did not restore the terminal mode")
		}
		select {
		case <-entered:
			t.Fatal("restoreTerminal wrote to a stalled output")
		default:
		}
	})

	t.Run("stalled_output_lock_held_writes_nothing", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(nil)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0
		c.oldState = &term.State{}

		// The render lock is held, as by mainLoop inside a blocked writeRaw.
		c.mu.Lock()
		defer c.mu.Unlock()

		done := make(chan struct{})
		go func() {
			c.restoreTerminal()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("restoreTerminal did not return with the render lock held")
		}
		if c.oldState != nil {
			t.Fatal("restoreTerminal did not restore the terminal mode")
		}
		select {
		case <-entered:
			t.Fatal("restoreTerminal wrote to a stalled output")
		default:
		}
	})
}

// TestCLIShutdownAbandonedReturnsError pins Run's teardown fold: when the
// owner reports that shutdown abandoned in-flight work, Run must fold a
// non-nil error into its returned error so a script driving this process
// detects the abandonment from the exit code. Exception, recorded per the
// contract-test rule: Run cannot be driven against a stub owner in a test.
// The owner field is a concrete *agent.Agent — typed for the concrete-only
// surface (ShutdownOwner), not the AdapterService interface — so a stub
// cannot be substituted without changing the field's type to an interface, a
// production change this test must not force. Run additionally requires a
// real TTY (term.MakeRaw on os.Stdin) to reach its teardown at all, and an
// abandoned shutdown needs the agent-internal coordinator park that
// TestOwnerShutdownContractMatrix drives (join=timeout). The fold is
// therefore pinned structurally against that behavioral evidence.
func TestCLIShutdownAbandonedReturnsError(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	body, ok := extractFunctionBody(string(src), "func (c *CLI) Run(")
	if !ok {
		t.Fatal("Run not found")
	}
	// The whole shape is one structure, not separate facts: the fold is
	// guarded by the abandoned outcome, the joined error is assigned into
	// err, and the same err is returned later in the body. An inverted guard,
	// an unconditional fold, or a fold that builds the joined error and
	// discards it would each fail this one pattern while still containing the
	// guard, the join call, the message and the return somewhere in the
	// function.
	guardedFoldIntoReturned := regexp.MustCompile(`!c\.owner\.ShutdownOwner\(\)\s*\{\s*err\s*=\s*errors\.Join\(\s*err\s*,\s*fmt\.Errorf\(\s*"owner shutdown abandoned in-flight work"\s*\)\s*\)\s*\}[\s\S]*?return\s+err\b`)
	if !guardedFoldIntoReturned.MatchString(body) {
		t.Fatal("Run must fold the abandoned outcome into the error it returns as one guarded structure: `if !c.owner.ShutdownOwner() { err = errors.Join(err, fmt.Errorf(\"owner shutdown abandoned in-flight work\")) }` and later `return err`")
	}
}
