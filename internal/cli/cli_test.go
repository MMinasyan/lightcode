package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
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

func TestRewriteBoundaryRendersRetainedErrors(t *testing.T) {
	a, _ := newTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	c := New(a)
	c.setCurrentSessionID(a.SessionCurrent().ID)
	var out bytes.Buffer
	c.out = &out
	c.width.Store(int32(80))

	// The producer's rewrite payload carries the committed prefix followed by
	// tail rows and surviving retained errors merged by sequence. An error that
	// survives the compaction disposition must stay on screen after the rewrite
	// in the terminal too, in its sequenced position among the tail rows.
	payload := agent.SessionPayload{
		Messages: []agent.DisplayMessage{
			{Type: "user", Content: "committed turn", Turn: 1},
			{Type: "assistant", Content: "live tail row"},
			{Type: "error", Content: "unattributed failure"},
			{Type: "error", Content: "after boundary failure", Turn: 2},
			{Type: "assistant", Content: "later tail row"},
		},
	}
	c.handleEvent(agent.Event{Kind: agent.EventSessionRewrite, RewritePayload: &payload})

	rendered := out.String()
	if !strings.Contains(rendered, "unattributed failure") {
		t.Fatalf("rewrite boundary dropped the unattributed error row: %q", rendered)
	}
	if !strings.Contains(rendered, "after boundary failure") {
		t.Fatalf("rewrite boundary dropped the above-boundary error row: %q", rendered)
	}
	idx := func(s string) int { return strings.Index(rendered, s) }
	if !(idx("live tail row") < idx("unattributed failure") && idx("unattributed failure") < idx("later tail row")) {
		t.Fatalf("error row not rendered among the tail rows: %q", rendered)
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

// TestCLIResumeTakesIdentityFromDirectory proves the explicit /resume <id>
// stores the directory-derived id as routing current: a session directory
// whose meta declares another id must not route the CLI to the declared id.
func TestCLIResumeTakesIdentityFromDirectory(t *testing.T) {
	a, _ := newTestAgent(t)
	projectPath := t.TempDir()
	proj, err := a.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("ProjectCurrentForPath: %v", err)
	}
	// Plant a session directory whose meta declares another id.
	const dirID = "dirA"
	dir := filepath.Join(a.Projects().SessionsRoot(proj.ID), dirID)
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"dirB","state":"active","project_path":"` + projectPath + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(a)
	var out bytes.Buffer
	c.out = &out
	c.cmdResume([]string{"/resume", dirID})
	if got := c.currentSessionID(); got != dirID {
		t.Fatalf("cli routing current = %q, want the directory's id %q", got, dirID)
	}
	if got := c.currentSessionSummary().ID; got != dirID {
		t.Fatalf("cli current summary id = %q, want %q", got, dirID)
	}
}

// TestCLIResumeSkipsContendedNewestSession proves the no-argument resume opens
// the newest candidate whose claim is acquirable: the newest session is held
// by another owner, so the older one resumes instead of the command failing.
func TestCLIResumeSkipsContendedNewestSession(t *testing.T) {
	first, second := newTestAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := second.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, second, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	c := New(second)
	c.scope = agent.NewAdapterScope(second, projectPath)
	var out bytes.Buffer
	c.out = &out
	c.cmdResume([]string{"/resume"})
	if got := c.currentSessionSummary().ID; got != olderID {
		t.Fatalf("cli current after resume = %q, want the older session %q", got, olderID)
	}
	runtime.KeepAlive(first)
}

// TestCLIResumeReportsNoActiveSessionsWhenEveryCandidateContended proves the
// no-argument resume reports the empty ending when every active candidate is
// held by another owner instead of failing on the first candidate.
func TestCLIResumeReportsNoActiveSessionsWhenEveryCandidateContended(t *testing.T) {
	first, second := newTestAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, first, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	c := New(second)
	c.scope = agent.NewAdapterScope(second, projectPath)
	var out bytes.Buffer
	c.out = &out
	c.cmdResume([]string{"/resume"})
	if !strings.Contains(out.String(), "no active sessions") {
		t.Fatalf("resume output = %q, want %q", out.String(), "no active sessions")
	}
	if got := c.currentSessionID(); got != "" {
		t.Fatalf("cli current after resume = %q, want empty", got)
	}
	runtime.KeepAlive(first)
}

// TestCLIResumeSurfacesListingFailure proves the no-argument resume reports a
// session-listing failure instead of treating it as an empty project.
func TestCLIResumeSurfacesListingFailure(t *testing.T) {
	_, second := newTestAgentPair(t)
	listErr := fmt.Errorf("session listing failed")
	svc := &listingFailSvc{AdapterService: second, listErr: listErr}
	c := New(nil)
	c.agent = svc
	c.scope = agent.NewAdapterScope(svc, second.ProjectRoot())
	var out bytes.Buffer
	c.out = &out
	c.cmdResume([]string{"/resume"})
	if !strings.Contains(out.String(), "session listing failed") {
		t.Fatalf("resume output = %q, want the listing failure", out.String())
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
			// The source is busy in the OWNER (a real claimed turn held open by
			// the provider) while the CLI's own event-derived busy is not set —
			// exactly the batch-race shape. Each command is driven through its
			// real path, and the source reservation must refuse with the
			// owner's mutability error, leaving the selection unchanged.
			_, c, out, source, _ := busySourceCLI(t)
			switch cmd {
			case "/session":
				dest := secondSession(t, c)
				selectSessionByID(t, c, dest)
			case "/project":
				_, otherRoot := sessionInOtherProject(t, c)
				selectProjectByPath(t, c, otherRoot)
			}
			dispatchSelection(t, c, cmd, "x")
			if !strings.Contains(out.String(), "a turn is running") {
				t.Fatalf("%q over a busy source = %q, want the owner mutability error", cmd, out.String())
			}
			if got, _ := c.currentSession(); got != source {
				t.Fatalf("%q over a busy source changed current to %q, want unchanged %q", cmd, got, source)
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
				// The destination is listed but cannot load its history (a
				// corrupt compaction record), so the open surfaces the failure
				// instead of adopting the session.
				dest := corruptSessionHistory(t, a, c.scope.ProjectPath())
				selectSessionByID(t, c, dest)
				dispatchSelection(t, c, cmd, "")
				if !strings.Contains(out.String(), "compaction.json") {
					t.Fatalf("/session over a corrupt destination = %q, want the load failure", out.String())
				}
			case "/project":
				// The destination project's session is listed but cannot
				// load its history (a corrupt compaction record), so the
				// switch surfaces the failure instead of skipping the
				// candidate.
				otherRoot := t.TempDir()
				corruptSessionHistory(t, a, otherRoot)
				selectProjectByPath(t, c, otherRoot)
				dispatchSelection(t, c, cmd, "")
				if !strings.Contains(out.String(), "compaction.json") {
					t.Fatalf("/project over a corrupt destination = %q, want the load failure", out.String())
				}
			}
			if got, _ := c.currentSession(); got != source {
				t.Fatalf("current after failed %s = %q, want unchanged source %q", cmd, got, source)
			}
			if !sourceClaimRetained(c, source) {
				t.Fatalf("source session %q lost its live claim", source)
			}
			// The release ran before the error rendered: the source is no
			// longer transitioning, so it is immediately usable again.
			release2, err := a.ReserveSelectionSource(source)
			if err != nil {
				t.Fatalf("source %q not usable after the failed %s: %v", source, cmd, err)
			}
			release2()
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

	// The no-source and read-only-source rows: no current session means the
	// CLI never calls the owner; a read-only current (another process drives
	// it) reaches the owner but the reservation is a no-op — navigation
	// proceeds in both cases, with no invented live-mutation guard.
	t.Run("no_source=noop_reservation_proceeds", func(t *testing.T) {
		a, c, _, _ := selectionCLI(t)
		rec := &recordingReserveAdapter{AdapterService: a}
		c.agent = rec
		c.sv().SetCurrent("")
		dispatchSelection(t, c, "/new", "")
		if len(rec.reserves) != 0 {
			t.Fatalf("no-source /new reserved: %v", rec.reserves)
		}
		if got, err := c.currentSession(); err != nil || got == "" {
			t.Fatalf("current after no-source /new = %q (err %v), want a fresh session", got, err)
		}
	})

	t.Run("read_only_source=noop_reservation_proceeds", func(t *testing.T) {
		first, second := newTestAgentPair(t)
		c := New(second)
		out := new(bytes.Buffer)
		c.out = out
		rec := &recordingReserveAdapter{AdapterService: second}
		c.agent = rec
		heldID, err := first.NewSessionForProjectPath(c.scope.ProjectPath(), "primary")
		if err != nil {
			t.Fatalf("NewSessionForProjectPath held: %v", err)
		}
		selectSessionByID(t, c, heldID)
		dispatchSelection(t, c, "/session", "")
		if !c.sv().IsReadOnly(heldID) {
			t.Fatal("held session not read-only after the menu open")
		}
		// The read-only source is current; /new calls the owner, whose
		// reservation is a no-op (the source is not live in this owner), and
		// the navigation commits a fresh live session.
		out.Reset()
		dispatchSelection(t, c, "/new", "")
		if len(rec.reserves) != 1 || rec.reserves[0] != heldID {
			t.Fatalf("read-only-source /new reservations = %v, want the no-op call for %q", rec.reserves, heldID)
		}
		if got, _ := c.currentSession(); got == "" || got == heldID {
			t.Fatalf("current after read-only-source /new = %q, want a fresh session", got)
		}
		runtime.KeepAlive(first)
	})

	// The idle-success row: the reservation is acquired for the source and
	// held through the destination commit, then released — a second
	// navigation still succeeds, which a leaked transitioning would refuse.
	t.Run("idle_success=reserves_through_commit_and_releases", func(t *testing.T) {
		a, c, _, source := selectionCLI(t)
		rec := &recordingReserveAdapter{AdapterService: a}
		c.agent = rec
		dispatchSelection(t, c, "/new", "")
		first := c.currentSessionID()
		if first == "" || first == source {
			t.Fatalf("first /new current = %q, want a fresh session distinct from %q", first, source)
		}
		dispatchSelection(t, c, "/new", "")
		if len(rec.reserves) != 2 || rec.reserves[0] != source || rec.reserves[1] != first {
			t.Fatalf("reservations = %v, want [%q, %q]", rec.reserves, source, first)
		}
		if !sourceClaimRetained(c, source) {
			t.Fatalf("source session %q lost its live claim", source)
		}
	})

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

// TestCLINavigationBlockedBySynchronousSubmitClaim proves the same-batch
// contract: a prompt key and a navigation key drained from one key batch
// cannot navigate away from the source, because the synchronous submit claims
// the owner unit before the next key is handled. EventTurnStart is still
// queued undrained when the navigation key runs — the CLI's event-derived
// busy flag is not set — yet the owner's busy state refuses the navigation
// with the mutability error and the selection stays on the source.
func TestCLINavigationBlockedBySynchronousSubmitClaim(t *testing.T) {
	block := make(chan struct{})
	var once sync.Once
	releaseHold := func() { once.Do(func() { close(block) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		http.Error(w, "released", http.StatusOK)
	}))
	t.Cleanup(server.Close)

	a, _ := newTestAgentWithBaseURL(t, server.URL)
	startTestAgent(t, a)
	source, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("source NewSession: %v", err)
	}
	c := New(a)
	c.agent.SetEventHandler(c.enqueueEvent)
	out := new(bytes.Buffer)
	c.out = out
	c.setCurrentSessionID(source)
	rec := &recordingSubmitAdapter{AdapterService: a}
	c.agent = rec
	c.ctx = context.Background()
	t.Cleanup(func() {
		releaseHold()
		_ = a.CancelSession(source)
	})

	// The source starts idle: the prompt in the buffered batch is the only
	// owner claim this test makes.
	if busy, err := a.BusyForSession(source); err != nil || busy {
		t.Fatalf("source busy before the batch = %v (err %v), want idle", busy, err)
	}

	// One real key batch: the prompt "hi" followed by /new. The keys are
	// drained in order exactly as mainLoop drains them; the turn_start the
	// submit emits stays queued behind the batch, the delayed-turn_start
	// shape.
	batch := []keyMsg{
		{Rune: 'h'}, {Rune: 'i'}, {Special: keyEnter},
		{Rune: '/'}, {Rune: 'n'}, {Rune: 'e'}, {Rune: 'w'}, {Special: keyEnter},
	}
	for _, k := range batch {
		c.enqueueKey(k)
	}
	for i, k := range batch {
		if err := c.handleKey(k); err != nil {
			t.Fatalf("handleKey: %v", err)
		}
		if i != 2 {
			continue
		}
		// Right after the prompt's Enter, before any navigation key is
		// handled, the synchronous submit must already own the unit: the
		// claim is the batch's only owner claim.
		if len(rec.submitted) != 1 || rec.submitted[0] != source {
			t.Fatalf("submit target right after the prompt key = %#v, want the source %q claimed synchronously", rec.submitted, source)
		}
		busy, err := a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("source busy right after the prompt key = %v (err %v), want the synchronous claim", busy, err)
		}
		// ...but its turn_start is still queued: the CLI's event-derived busy
		// is not set, so the rejection below cannot have come from the CLI's
		// own presentation state.
		if c.busy {
			t.Fatal("CLI busy flag set before the turn_start event drained; the rejection must not depend on event-derived busy")
		}
	}

	// The /new key refused with the owner's mutability error and the
	// selection is unchanged.
	if !strings.Contains(out.String(), "a turn is running") {
		t.Fatalf("/new after the same-batch submit = %q, want the owner mutability error", out.String())
	}
	if got, _ := c.currentSession(); got != source {
		t.Fatalf("current after same-batch submit+navigation = %q, want unchanged %q", got, source)
	}

	// The delayed turn_start lands only after the batch: draining the
	// delivery FIFO sets the CLI's presentation busy from the owner's claim.
	c.drainEvents()
	if !c.busy {
		t.Fatal("turn_start did not set the CLI busy flag once drained")
	}
}

// TestCLIArchiveDeleteDoNotReserveSelection proves archive/delete stay
// lifecycle operations: the session menu's archive and delete branches run
// the lifecycle path without acquiring a selection reservation, while the new
// branch (a navigation sibling) does reserve the source.
func TestCLIArchiveDeleteDoNotReserveSelection(t *testing.T) {
	t.Run("archive=no_reservation", func(t *testing.T) {
		_, c, _, source := selectionCLI(t)
		rec := &recordingReserveAdapter{AdapterService: c.agent}
		c.agent = rec
		victim := secondSession(t, c)

		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, victim), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		if len(rec.reserves) != 0 {
			t.Fatalf("archive acquired a selection reservation: %v", rec.reserves)
		}
		if _, err := c.agent.SessionSummaryForSession(victim); err == nil {
			t.Fatal("archive did not run the lifecycle path; the victim session still resolves")
		}
		if got, _ := c.currentSession(); got != source {
			t.Fatalf("current after archiving another session = %q, want unchanged %q", got, source)
		}
	})

	t.Run("delete=no_reservation", func(t *testing.T) {
		_, c, _, source := selectionCLI(t)
		rec := &recordingReserveAdapter{AdapterService: c.agent}
		c.agent = rec
		victim := secondSession(t, c)

		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, victim), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		if len(rec.reserves) != 0 {
			t.Fatalf("delete acquired a selection reservation: %v", rec.reserves)
		}
		if _, err := c.agent.SessionSummaryForSession(victim); err == nil {
			t.Fatal("delete did not run the lifecycle path; the victim session still resolves")
		}
		if got, _ := c.currentSession(); got != source {
			t.Fatalf("current after deleting another session = %q, want unchanged %q", got, source)
		}
	})

	t.Run("new=reserves_source", func(t *testing.T) {
		// Positive control: the navigation sibling (new) does acquire the
		// selection reservation.
		_, c, _, source := selectionCLI(t)
		rec := &recordingReserveAdapter{AdapterService: c.agent}
		c.agent = rec
		c.readKeyFn = func() (keyMsg, error) { return keyMsg{Rune: 'n'}, nil }
		dispatchSelection(t, c, "/session", "")

		if len(rec.reserves) != 1 || rec.reserves[0] != source {
			t.Fatalf("new reservations = %v, want exactly the source %q", rec.reserves, source)
		}
		if got, _ := c.currentSession(); got == "" || got == source {
			t.Fatalf("current after menu new = %q, want a fresh session distinct from %q", got, source)
		}
	})
}

// TestCLICompactCompletionGatedBySelection proves the compact/navigation
// ordering: /compact stays async, but the completion mutates and renders only
// while the session it was started for is still selected. A compact from the
// source that completes after a same-batch navigation to the destination must
// not erase the destination's prompt, change its state, or render the source
// errors, and the navigation commit itself must reset the compacting/streaming
// presentation even though a compact sets no stream fields.
func TestCLICompactCompletionGatedBySelection(t *testing.T) {
	// driveBatch runs one real key batch — /compact Enter /new Enter — exactly
	// as mainLoop drains it, then waits for the compact op to enter the
	// blocking adapter and commits the navigation.
	driveBatch := func(t *testing.T, c *CLI, out *bytes.Buffer, adapter *blockingCompactAdapter) (source, dest string, outAfterNav string) {
		t.Helper()
		source = c.currentSessionID()
		for _, k := range []keyMsg{
			{Rune: '/'}, {Rune: 'c'}, {Rune: 'o'}, {Rune: 'm'}, {Rune: 'p'}, {Rune: 'a'}, {Rune: 'c'}, {Rune: 't'}, {Special: keyEnter},
			{Rune: '/'}, {Rune: 'n'}, {Rune: 'e'}, {Rune: 'w'}, {Special: keyEnter},
		} {
			c.enqueueKey(k)
		}
		for {
			k, ok := c.popKey()
			if !ok {
				break
			}
			if err := c.handleKey(k); err != nil {
				t.Fatalf("handleKey: %v", err)
			}
		}
		select {
		case <-adapter.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("compact op did not enter the adapter")
		}
		dest = c.currentSessionID()
		if dest == "" || dest == source {
			t.Fatalf("current after the same-batch /new = %q, want a fresh session distinct from %q", dest, source)
		}
		c.mu.Lock()
		compacting, state, animActive, busy := c.compacting, cliState(c.state.Load()), c.animActive, c.busy
		c.mu.Unlock()
		if compacting || state != stateIdle || animActive || busy {
			t.Fatalf("presentation after the navigation commit = compacting:%v state:%v anim:%v busy:%v, want all reset", compacting, state, animActive, busy)
		}
		return source, dest, out.String()
	}

	t.Run("error_completion_after_navigation_renders_nothing", func(t *testing.T) {
		a, c, out, _ := selectionCLI(t)
		adapter := &blockingCompactAdapter{AdapterService: a, entered: make(chan struct{}), release: make(chan struct{})}
		c.agent = adapter
		_, dest, outAfterNav := driveBatch(t, c, out, adapter)

		// The compact fails only after the navigation committed: the stale
		// completion must render nothing into the destination view.
		adapter.err = errors.New("compact failed")
		close(adapter.release)
		c.opWG.Wait() // the op's enqueuePost landed before the group done
		c.drainEvents()
		if got := out.String(); got != outAfterNav {
			t.Fatalf("stale compact error rendered into the destination view: %q", got)
		}
		c.mu.Lock()
		compacting, state, busy := c.compacting, cliState(c.state.Load()), c.busy
		c.mu.Unlock()
		if compacting || state != stateIdle || busy {
			t.Fatalf("destination state after the stale completion = compacting:%v state:%v busy:%v, want untouched", compacting, state, busy)
		}
		if got, _ := c.currentSession(); got != dest {
			t.Fatalf("current after the stale completion = %q, want %q", got, dest)
		}
	})

	t.Run("success_completion_after_navigation_renders_nothing", func(t *testing.T) {
		a, c, out, _ := selectionCLI(t)
		adapter := &blockingCompactAdapter{AdapterService: a, entered: make(chan struct{}), release: make(chan struct{})}
		c.agent = adapter
		_, _, outAfterNav := driveBatch(t, c, out, adapter)

		close(adapter.release)
		c.opWG.Wait()
		c.drainEvents()
		if got := out.String(); got != outAfterNav {
			t.Fatalf("stale compact success rendered into the destination view: %q", got)
		}
	})

	t.Run("completion_while_still_current_renders", func(t *testing.T) {
		// Positive control: a compact that completes while its own session is
		// still selected finishes exactly as before — the error renders and
		// the idle prompt returns.
		a, c, out, source := selectionCLI(t)
		adapter := &blockingCompactAdapter{AdapterService: a, entered: make(chan struct{}), release: make(chan struct{})}
		c.agent = adapter
		if err := c.dispatchCommand("/compact"); err != nil {
			t.Fatalf("/compact: %v", err)
		}
		select {
		case <-adapter.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("compact op did not enter the adapter")
		}
		adapter.err = errors.New("compact failed")
		close(adapter.release)
		c.opWG.Wait()
		c.drainEvents()
		if !strings.Contains(out.String(), "compact failed") {
			t.Fatalf("current-session compact error not rendered: %q", out.String())
		}
		c.mu.Lock()
		compacting, state := c.compacting, cliState(c.state.Load())
		c.mu.Unlock()
		if compacting || state != stateIdle {
			t.Fatalf("state after the current-session compact = compacting:%v state:%v, want idle", compacting, state)
		}
		if got, _ := c.currentSession(); got != source {
			t.Fatalf("current after the current-session compact = %q, want %q", got, source)
		}
	})
}

// TestCLINavigationReleasesSourceBeforeDestinationRender proves the release
// timing contract on every navigation path: the source reservation ends at
// the selection commit, before the destination render, so a stalled render
// cannot hold the source transitioning. While the destination prompt write is
// blocked, the source must already be usable again — a fresh reservation
// succeeds and a submit starts a turn.
func TestCLINavigationReleasesSourceBeforeDestinationRender(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestResponse(w, "ok")
	}))
	t.Cleanup(server.Close)

	rows := []struct {
		name string
		run  func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error
	}{
		{
			name: "new",
			run: func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error {
				return c.dispatchCommand("/new")
			},
		},
		{
			name: "resume",
			run: func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error {
				dest := secondSession(t, c)
				return c.dispatchCommand("/resume " + dest)
			},
		},
		{
			name: "session_select",
			run: func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error {
				dest := secondSession(t, c)
				selectSessionByID(t, c, dest)
				return c.dispatchCommand("/session")
			},
		},
		{
			name: "session_new",
			run: func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error {
				c.readKeyFn = func() (keyMsg, error) { return keyMsg{Rune: 'n'}, nil }
				return c.dispatchCommand("/session")
			},
		},
		{
			name: "project_select",
			run: func(t *testing.T, c *CLI, a *agent.Agent, projectRoot string) error {
				if _, err := c.agent.NewSessionForProjectPath(projectRoot, "primary"); err != nil {
					t.Fatalf("NewSessionForProjectPath: %v", err)
				}
				otherRoot := projectRoot
				selectProjectByPath(t, c, otherRoot)
				return c.dispatchCommand("/project")
			},
		},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			projectRoot := ""
			if row.name == "project_select" {
				projectRoot = t.TempDir()
			}
			a, _ := newTestAgentWithBaseURL(t, server.URL)
			startTestAgent(t, a)
			source, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("source NewSession: %v", err)
			}
			c := New(a)
			entered := make(chan struct{})
			releaseWrite := make(chan struct{})
			c.out = &markerBlockingWriter{marker: "> ", entered: entered, release: releaseWrite}
			c.setCurrentSessionID(source)

			done := make(chan error, 1)
			go func() { done <- row.run(t, c, a, projectRoot) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("destination prompt write did not stall")
			}

			// The destination render is stalled at the prompt: the source
			// reservation was released before rendering began, so the source
			// is immediately usable again.
			release2, err := a.ReserveSelectionSource(source)
			if err != nil {
				t.Fatalf("source still reserved while the destination render is stalled: %v", err)
			}
			release2()
			res, err := a.SubmitToSession(context.Background(), source, "after commit")
			if err != nil || !res.Started {
				t.Fatalf("submit to the source during the stalled render = started:%v err:%v, want the released source to start a turn", res.Started, err)
			}
			busy, err := a.BusyForSession(source)
			if err != nil || !busy {
				t.Fatalf("source busy after the submit = %v (err %v), want the started turn", busy, err)
			}

			close(releaseWrite)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("navigation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("navigation did not return after the writer unblocked")
			}
			if got, _ := c.currentSession(); got == "" || got == source {
				t.Fatalf("current after the navigation = %q, want the destination", got)
			}
			waitUntilAgentIdle(t, a)
		})
	}
}

// TestCLINavigationFailureReleasesSourceBeforeErrorRender proves the
// destination-failure release timing on every navigation path: the source
// reservation ends before the failure error renders, so a blocked error write
// cannot hold the source transitioning. While the error render is stalled,
// the source must already be usable again.
func TestCLINavigationFailureReleasesSourceBeforeErrorRender(t *testing.T) {
	rows := []struct {
		name string
		run  func(t *testing.T, c *CLI, a *agent.Agent) error
	}{
		{
			name: "resume",
			run: func(t *testing.T, c *CLI, a *agent.Agent) error {
				return c.dispatchCommand("/resume does-not-exist")
			},
		},
		{
			name: "new",
			run: func(t *testing.T, c *CLI, a *agent.Agent) error {
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
				return c.dispatchCommand("/new")
			},
		},
		{
			name: "session_select",
			run: func(t *testing.T, c *CLI, a *agent.Agent) error {
				dest := corruptSessionHistory(t, a, c.scope.ProjectPath())
				selectSessionByID(t, c, dest)
				return c.dispatchCommand("/session")
			},
		},
		{
			name: "project_select",
			run: func(t *testing.T, c *CLI, a *agent.Agent) error {
				otherRoot := t.TempDir()
				corruptSessionHistory(t, a, otherRoot)
				selectProjectByPath(t, c, otherRoot)
				return c.dispatchCommand("/project")
			},
		},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			a, c, _, source := selectionCLI(t)
			entered := make(chan struct{})
			releaseWrite := make(chan struct{})
			c.out = &markerBlockingWriter{marker: "✕", entered: entered, release: releaseWrite}

			done := make(chan error, 1)
			go func() { done <- row.run(t, c, a) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("failure error write did not stall")
			}

			// The error render is stalled: the source reservation was
			// released before it began, so the source is usable again.
			release2, err := a.ReserveSelectionSource(source)
			if err != nil {
				t.Fatalf("source still reserved while the failure error render is stalled: %v", err)
			}
			release2()
			if got, _ := c.currentSession(); got != source {
				t.Fatalf("current while the failure error renders = %q, want unchanged %q", got, source)
			}

			close(releaseWrite)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("navigation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("navigation did not return after the writer unblocked")
			}
		})
	}
}

// TestCLIResumeNoIDListsBeforeReservingSource proves the no-ID /resume lists
// its destination candidates before reserving the source: while the candidate
// list is blocked, the idle source stays claimable — a fresh reservation
// succeeds and a submitted queue drains — and only once the chosen candidate
// opens is the source protected by the reservation.
func TestCLIResumeNoIDListsBeforeReservingSource(t *testing.T) {
	server, firstEntered, firstRelease := blockingProvider(t)

	a, _ := newTestAgentWithBaseURL(t, server.URL)
	startTestAgent(t, a)
	source, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("source NewSession: %v", err)
	}
	dest, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("dest NewSession: %v", err)
	}

	c := New(a)
	out := new(bytes.Buffer)
	c.out = out
	c.setCurrentSessionID(source)
	adapter := &stagedNavigationAdapter{
		AdapterService: a,
		blockList:      true,
		listEntered:    make(chan struct{}),
		listRelease:    make(chan struct{}),
		blockOpen:      true,
		openEntered:    make(chan struct{}),
		openRelease:    make(chan struct{}),
	}
	c.agent = adapter
	c.scope = agent.NewAdapterScope(adapter, c.scope.ProjectRoot())

	done := make(chan error, 1)
	go func() { done <- c.dispatchCommand("/resume") }()
	select {
	case <-adapter.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume candidate list did not block")
	}

	// Phase A: the candidate list runs with NO reservation held. The idle
	// source stays claimable while listing...
	release2, err := a.ReserveSelectionSource(source)
	if err != nil {
		t.Fatalf("source reserved while the candidate list is blocked: %v", err)
	}
	release2()
	// ...and can submit and drain a queue: turn 1 holds on the provider,
	// turn 2 queues and drains when turn 1 ends — all while the list is
	// still blocked.
	if res, err := a.SubmitToSession(context.Background(), source, "first"); err != nil || !res.Started {
		t.Fatalf("submit while listing = started:%v err:%v, want a started turn", res.Started, err)
	}
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn request did not reach the provider")
	}
	if res, err := a.SubmitToSession(context.Background(), source, "second"); err != nil || res.Started {
		t.Fatalf("second submit while the first turn runs = started:%v err:%v, want queued", res.Started, err)
	}
	close(firstRelease)
	waitUntilSourceIdleAndDrained(t, a, source)

	// Phase B: the list releases; the reservation is acquired immediately
	// before the chosen candidate opens, which blocks. The source is now
	// protected during the actual destination operation. The phase-A turns
	// bumped the source's activity, so the destination is stamped newest to
	// keep the candidate order deterministic.
	stampSessionActivity(t, a, c.scope.ProjectPath(), dest, time.Now().Unix()+10)
	close(adapter.listRelease)
	select {
	case <-adapter.openEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("destination open did not block")
	}
	if _, err := a.SubmitToSession(context.Background(), source, "during open"); err == nil ||
		!strings.Contains(err.Error(), "session is changing") {
		t.Fatalf("submit during the destination open = %v, want the reservation refusal", err)
	}

	close(adapter.openRelease)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("/resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("/resume did not return after the open released")
	}
	if got, _ := c.currentSession(); got != dest {
		t.Fatalf("current after the no-ID resume = %q, want %q", got, dest)
	}
}

// TestCLIProjectListsBeforeReservingSource proves /project lists and selects
// its destinations before reserving the source: while the project list is
// blocked, the idle source stays claimable — a fresh reservation succeeds and
// a submitted queue drains — and only during the destination project's
// open/create (its candidate scan) is the source protected by the
// reservation.
func TestCLIProjectListsBeforeReservingSource(t *testing.T) {
	server, firstEntered, firstRelease := blockingProvider(t)

	otherRoot := t.TempDir()
	a, _ := newTestAgentWithBaseURL(t, server.URL)
	startTestAgent(t, a)
	source, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("source NewSession: %v", err)
	}

	c := New(a)
	out := new(bytes.Buffer)
	c.out = out
	c.setCurrentSessionID(source)
	dest, err := a.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	adapter := &stagedNavigationAdapter{
		AdapterService: a,
		blockProject:   true,
		projEntered:    make(chan struct{}),
		projRelease:    make(chan struct{}),
		blockList:      true,
		listEntered:    make(chan struct{}),
		listRelease:    make(chan struct{}),
	}
	c.agent = adapter
	c.scope = agent.NewAdapterScope(adapter, c.scope.ProjectRoot())

	done := make(chan error, 1)
	go func() { done <- c.dispatchCommand("/project") }()
	select {
	case <-adapter.projEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("project list did not block")
	}

	// Phase A: the project list runs with NO reservation held. The idle
	// source stays claimable while listing...
	release2, err := a.ReserveSelectionSource(source)
	if err != nil {
		t.Fatalf("source reserved while the project list is blocked: %v", err)
	}
	release2()
	// ...and can submit and drain a queue while the list is still blocked.
	if res, err := a.SubmitToSession(context.Background(), source, "first"); err != nil || !res.Started {
		t.Fatalf("submit while listing = started:%v err:%v, want a started turn", res.Started, err)
	}
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn request did not reach the provider")
	}
	if res, err := a.SubmitToSession(context.Background(), source, "second"); err != nil || res.Started {
		t.Fatalf("second submit while the first turn runs = started:%v err:%v, want queued", res.Started, err)
	}
	close(firstRelease)
	waitUntilSourceIdleAndDrained(t, a, source)

	// Wire the menu selection only now, from the real agent's current
	// listing: Phase A's source turns advance the source project's
	// activity (second granularity), which can flip the activity-sorted
	// project order between a pre-Phase-A listing and the menu render.
	// Nothing writes project activity between this read and the render
	// (the staged adapter forwards the same listing), so the wired index
	// and the rendered order are identical.
	selectProjectByPathListed(t, c, a, otherRoot)

	// Phase B: the menu selects the destination project; projectSwitch
	// acquires the reservation immediately before OpenOrCreateSession, whose
	// destination candidate scan blocks. The source is now protected during
	// the actual destination operation.
	close(adapter.projRelease)
	select {
	case <-adapter.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("destination candidate scan did not block")
	}
	if _, err := a.SubmitToSession(context.Background(), source, "during open"); err == nil ||
		!strings.Contains(err.Error(), "session is changing") {
		t.Fatalf("submit during the destination scan = %v, want the reservation refusal", err)
	}

	close(adapter.listRelease)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("/project: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("/project did not return after the scan released")
	}
	if got, _ := c.currentSession(); got != dest {
		t.Fatalf("current after the project switch = %q, want %q", got, dest)
	}
	if got := c.scope.ProjectPath(); got != otherRoot {
		t.Fatalf("project path after the switch = %q, want %q", got, otherRoot)
	}
}

// selectIndex returns the 0-based menu index of the given session in the
// CLI's own project-scoped listing order.
func selectIndex(t *testing.T, c *CLI, id string) int {
	t.Helper()
	sessions, err := c.agent.SessionListForProjectPath(c.scope.ProjectPath(), "active")
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	for i, s := range sessions {
		if s.ID == id {
			return i
		}
	}
	t.Fatalf("session %q not listed in the session menu", id)
	return -1
}

// menuKeysWithTail returns a key source that navigates down n items, presses
// Enter to select the n-th item, then yields the tail keys (e.g. the 'a' or
// 'd' menu action).
func menuKeysWithTail(n int, tail ...keyMsg) func() (keyMsg, error) {
	return func() (keyMsg, error) {
		if n > 0 {
			n--
			return keyMsg{Special: keyDown}, nil
		}
		if len(tail) > 0 {
			k := tail[0]
			tail = tail[1:]
			return k, nil
		}
		return keyMsg{Special: keyEnter}, nil
	}
}

// TestCLIProjectSwitchCreatesWhenEveryCandidateHeld proves /project over a
// destination whose every session is driven by another process creates a new
// session in that project and switches to it, with the source keeping its
// live claim: a held destination is not a destination failure, it is the
// multi-interface case the per-session claim exists to support.
func TestCLIProjectSwitchCreatesWhenEveryCandidateHeld(t *testing.T) {
	a, c, _, source := selectionCLI(t)
	otherRoot := t.TempDir()
	heldID := holderSession(t, a, otherRoot)
	selectProjectByPath(t, c, otherRoot)
	dispatchSelection(t, c, "/project", "")

	got, err := c.currentSession()
	if err != nil || got == "" || got == source || got == heldID {
		t.Fatalf("current after /project over a fully held destination = %q (err %v), want a newly created session", got, err)
	}
	if gotPath := c.scope.ProjectPath(); gotPath != otherRoot {
		t.Fatalf("project path after /project = %q, want the destination %q", gotPath, otherRoot)
	}
	summary := c.currentSessionSummary()
	if summary.ID != got || summary.ProjectPath != otherRoot {
		t.Fatalf("current summary after /project = %+v, want session %q in project %q", summary, got, otherRoot)
	}
	if !sourceClaimRetained(c, source) {
		t.Fatalf("source session %q lost its live claim", source)
	}
}

// TestCLIReadOnlyOpenHydrationFailureLeavesSelectionAndProject proves a
// read-only open whose durable view cannot be read commits nothing on either
// terminal path: the session menu and /resume <id> keep the previous session,
// the routing project, and the read-only marker unset, so a failed
// presentation never advances routing.
func TestCLIReadOnlyOpenHydrationFailureLeavesSelectionAndProject(t *testing.T) {
	first, second := newTestAgentPair(t)
	source, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession source: %v", err)
	}
	// The held session lives in its own project, so a failed read-only open of
	// it must leave both the session and the routing project in place.
	otherRoot := t.TempDir()
	heldID, err := first.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath held: %v", err)
	}
	// A held session whose durable history cannot be read: the open still fails
	// as contention (the claim is acquired before any file read), and the
	// read-only hydration then fails on the corrupt compaction record.
	proj, err := first.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("project for held session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Projects().SessionsRoot(proj.ID), heldID, "compaction.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("corrupt held session compaction: %v", err)
	}

	t.Run("session_menu", func(t *testing.T) {
		c := New(second)
		out := new(bytes.Buffer)
		c.out = out
		c.setCurrentSessionID(source)
		// The menu lists the held session, so the CLI routes to its project.
		c.scope.SetProjectPath(otherRoot)

		selectSessionByID(t, c, heldID)
		dispatchSelection(t, c, "/session", "")
		if got, _ := c.currentSession(); got != source {
			t.Fatalf("current after failed read-only open = %q, want unchanged %q", got, source)
		}
		if got := c.scope.ProjectPath(); got != otherRoot {
			t.Fatalf("routing project after failed read-only open = %q, want unchanged %q", got, otherRoot)
		}
		if !strings.Contains(out.String(), "compaction.json") {
			t.Fatalf("failed read-only open output = %q, want the hydration failure", out.String())
		}
		if c.sv().IsReadOnly(heldID) {
			t.Fatal("read-only marker set for an open whose presentation failed")
		}
	})

	t.Run("resume", func(t *testing.T) {
		c := New(second)
		out := new(bytes.Buffer)
		c.out = out
		c.setCurrentSessionID(source)
		// The CLI routes to the source project; a failed read-only open of a
		// session elsewhere must not commit the destination project.
		wantProject := c.scope.ProjectPath()

		dispatchSelection(t, c, "/resume", heldID)
		if got, _ := c.currentSession(); got != source {
			t.Fatalf("current after failed read-only open = %q, want unchanged %q", got, source)
		}
		if got := c.scope.ProjectPath(); got != wantProject {
			t.Fatalf("routing project after failed read-only open = %q, want unchanged %q", got, wantProject)
		}
		if !strings.Contains(out.String(), "compaction.json") {
			t.Fatalf("failed read-only open output = %q, want the hydration failure", out.String())
		}
		if c.sv().IsReadOnly(heldID) {
			t.Fatal("read-only marker set for an open whose presentation failed")
		}
	})
	runtime.KeepAlive(first)
}

// TestCLIExplicitOpenOfContendedSessionIsReadOnly proves an explicit open of a
// session another process drives opens it read-only on both explicit-open
// paths, the session menu and /resume <id>: the session becomes routing
// current with its durable transcript and its own identity, a turn refuses
// with the contention message instead of "unknown session", compaction names
// the contention instead of "no current session", and a switch to a live
// session clears the marker.
func TestCLIExplicitOpenOfContendedSessionIsReadOnly(t *testing.T) {
	first, second := newTestAgentPair(t)
	startTestAgent(t, first)
	startTestAgent(t, second)
	c := New(second)
	out := new(bytes.Buffer)
	c.out = out
	source, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession source: %v", err)
	}
	// The held session lives in the CLI's project, so the session menu lists it.
	heldID, err := first.NewSessionForProjectPath(c.scope.ProjectPath(), "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath held: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(heldID, "durable from the driving owner"); err != nil {
		t.Fatalf("append durable message: %v", err)
	}
	c.setCurrentSessionID(source)

	// The session menu opens the held session read-only.
	selectSessionByID(t, c, heldID)
	dispatchSelection(t, c, "/session", "")
	if got, _ := c.currentSession(); got != heldID {
		t.Fatalf("current after /session over the held session = %q, want it adopted read-only", got)
	}
	if got := cliUserContents(c.sessionMessages()); !equalStringSlices(got, []string{"durable from the driving owner"}) {
		t.Fatalf("read-only transcript = %#v, want the durable messages", got)
	}
	if got := c.liveCurrentSessionID(); got != "" {
		t.Fatalf("read-only session reports live: %q", got)
	}
	if !c.sv().IsReadOnly(heldID) {
		t.Fatal("held session not marked read-only after the menu open")
	}

	// A turn refuses with the contention message, not "unknown session".
	c.submitToBackend("hi")
	c.drainEvents()
	if !strings.Contains(out.String(), "driven by another process") {
		t.Fatalf("submit over the read-only session = %q, want the contention message", out.String())
	}
	// Compaction names the contention instead of "no current session".
	out.Reset()
	c.cmdCompact()
	if !strings.Contains(out.String(), "driven by another process") {
		t.Fatalf("compact over the read-only session = %q, want the contention message", out.String())
	}

	// /resume <id> opens the held session read-only too.
	out.Reset()
	c.setCurrentSessionID(source)
	dispatchSelection(t, c, "/resume", heldID)
	if got, _ := c.currentSession(); got != heldID {
		t.Fatalf("current after /resume over the held session = %q, want it adopted read-only", got)
	}
	if !c.sv().IsReadOnly(heldID) {
		t.Fatal("held session not marked read-only after /resume")
	}

	// The driving process releases the session; /resume reopens it live: the
	// read-only marker does not survive a successful live commit of the same
	// id, so a turn is admitted and no contention is reported.
	if err := first.SessionArchive(heldID); err != nil {
		t.Fatalf("SessionArchive (release): %v", err)
	}
	out.Reset()
	dispatchSelection(t, c, "/resume", heldID)
	if got, _ := c.currentSession(); got != heldID {
		t.Fatalf("current after /resume of the released session = %q, want it live", got)
	}
	if c.sv().IsReadOnly(heldID) {
		t.Fatal("read-only marker survived the live reopen of the same session")
	}
	if got := c.liveCurrentSessionID(); got != heldID {
		t.Fatalf("live current after reopen = %q, want %q", got, heldID)
	}
	c.ctx = context.Background()
	c.submitToBackend("hi")
	c.drainEvents()
	waitUntilSourceIdleAndDrained(t, second, heldID)
	if strings.Contains(out.String(), "driven by another process") {
		t.Fatalf("submit after reopen reported contention: %q", out.String())
	}

	// Switching to a live session clears the marker and restores the live view.
	c.setCurrentSessionID(source)
	if c.sv().IsReadOnly(heldID) {
		t.Fatal("read-only marker survived the switch to a live session")
	}
	if got := c.liveCurrentSessionID(); got != source {
		t.Fatalf("live current after switch = %q, want %q", got, source)
	}
	runtime.KeepAlive(first)
}

// TestCLISubmitResolvesTargetAtEnter proves the submit target is resolved when
// the submit is admitted, not when the op goroutine happens to run: a session
// switch that lands while the op is blocked cannot redirect the text to the new
// session. Built on the read-only fixture: the held session is routing current,
// the submit is blocked in the op-group, the switch commits the source session,
// and the submit must still name the held session.
func TestCLISubmitResolvesTargetAtEnter(t *testing.T) {
	first, second := newTestAgentPair(t)
	c := New(second)
	out := new(bytes.Buffer)
	c.out = out
	rec := &recordingSubmitAdapter{AdapterService: second}
	c.agent = rec
	source, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession source: %v", err)
	}
	heldID, err := first.NewSessionForProjectPath(c.scope.ProjectPath(), "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath held: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(heldID, "durable from the driving owner"); err != nil {
		t.Fatalf("append durable message: %v", err)
	}
	c.setCurrentSessionID(source)

	// The session menu opens the held session read-only.
	selectSessionByID(t, c, heldID)
	dispatchSelection(t, c, "/session", "")
	if !c.sv().IsReadOnly(heldID) {
		t.Fatal("held session not marked read-only after the menu open")
	}

	// The submit is synchronous: the target and its read-only classification
	// are resolved and the owner call lands in one key-processing step, so a
	// session switch can no longer interleave between Enter and the call. The
	// error render is deferred through the delivery FIFO, so a switch after
	// the call cannot redirect the error either: it still names the session
	// that was current at Enter.
	c.ctx = context.Background()
	c.submitToBackend("hi")
	c.setCurrentSessionID(source)
	c.drainEvents()

	if got, _ := c.currentSession(); got != source {
		t.Fatalf("current after the switch = %q, want %q", got, source)
	}
	if len(rec.submitted) != 1 || rec.submitted[0] != heldID {
		t.Fatalf("submit target = %#v, want the held session %q resolved at Enter", rec.submitted, heldID)
	}
	// The read-only classification is captured at Enter with the id, so the
	// failure the user sees still names the contention on the session that
	// was current then, not the owner's "unknown session" refusal.
	if !strings.Contains(out.String(), fmt.Sprintf("session %q is being driven by another process", heldID)) {
		t.Fatalf("submit failure after the switch = %q, want the contention message naming the held session", out.String())
	}
	runtime.KeepAlive(first)
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

// corruptSessionHistory plants a listed session whose history cannot load: a
// valid meta record with a broken compaction record, so opening it fails as a
// corrupt destination rather than as contention.
func corruptSessionHistory(t *testing.T, a *agent.Agent, projectPath string) string {
	t.Helper()
	proj, err := a.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("ProjectCurrentForPath(%q): %v", projectPath, err)
	}
	const id = "corrupt-session"
	sessionDir := filepath.Join(a.Projects().SessionsRoot(proj.ID), id)
	if err := os.MkdirAll(filepath.Join(sessionDir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"id":%q,"state":"active","project_path":%q,"last_activity":%d}`+"\n",
		id, projectPath, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "compaction.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
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
	selectProjectByPathListed(t, c, c.agent, path)
}

// selectProjectByPathListed wires the key source to select the given project
// in the project menu from the listing of the given service, which must be
// the listing the menu will render. Tests whose CLI agent is a staged adapter
// with a blocking ProjectList pass the real agent here.
func selectProjectByPathListed(t *testing.T, c *CLI, lister agent.AdapterService, path string) {
	t.Helper()
	projects, err := lister.ProjectList()
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

		// History reversion is gone from every host; only code rewind and fork remain.
		{"wails", "revert_history", false, []string{"func (a *App) RevertHistory("}},
		{"cli", "revert_history", false, []string{"TurnActionRevertHistory"}},
		{"acp", "revert_history", false, []string{`"session/revert_history"`}},

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

	// stdin EOF through the real reader establishes the exit authority too:
	// the reader observes EOF on the pipe, sets the latch, and a later key
	// read unwinds with the EOF exit even with a key buffered.
	t.Run("reader_eof_sets_latch", func(t *testing.T) {
		c := New(nil)
		c.ctx = context.Background()
		r, w := io.Pipe()
		defer r.Close()
		go c.readKeysFrom(r)
		w.Close()
		select {
		case <-c.exitLatch:
		case <-time.After(5 * time.Second):
			t.Fatal("EOF did not set the exit latch")
		}
		c.enqueueKey(keyMsg{Rune: 'x'})
		_, err := c.nextKey(context.Background())
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != 0 {
			t.Fatalf("nextKey after EOF = %v, want ExitError code 0", err)
		}
	})
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

// TestCLIAsyncOpsUseOpGroup proves compact and copy run through the op-group
// (so they are admission-gated and joined on exit) rather than bare
// goroutines, and that submit is synchronous by contract: the owner's claim
// must land before the next key in the same batch, so it runs inline on the
// key handler's goroutine with no spawnOp and no bare goroutine, posting only
// its error render through the delivery FIFO.
func TestCLIAsyncOpsUseOpGroup(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}
	s := string(src)
	// compact and copy always run through the op-group; cmdCopy runs its
	// clipboard fallback through it (its OSC-52 fast path is synchronous
	// mainLoop output).
	for _, fn := range []string{"func (c *CLI) cmdCompact(", "func (c *CLI) cmdCopy("} {
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
	// submit is synchronous: the owner claim lands before the next key in the
	// same batch, so the op-group (which could schedule the claim arbitrarily
	// late) is not used.
	body, ok := extractFunctionBody(s, "func (c *CLI) submitToBackend(")
	if !ok {
		t.Fatal("submitToBackend not found")
	}
	if strings.Contains(body, "c.spawnOp(") {
		t.Fatal("submitToBackend must be synchronous, not op-group-spawned: the owner claim must land before the next key in the same batch")
	}
	if strings.Contains(body, "go func") {
		t.Fatal("submitToBackend must not start a bare goroutine")
	}
	if !strings.Contains(body, "c.enqueuePost(") {
		t.Fatal("submitToBackend must post its error render through the delivery FIFO")
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

// TestCLISIGINTAdmissionAuthority proves the SIGINT decision is the owner's
// live busy unit, not the CLI's delayed presentation state: an owner-busy
// selected live session cancels and keeps the host open even while turn_start
// is still queued undrained, while idle, empty, missing, and read-only
// selections still exit 130. The signal path never writes the terminal and
// never waits on the CLI render lock.
func TestCLISIGINTAdmissionAuthority(t *testing.T) {
	assertExit130 := func(t *testing.T, c *CLI) {
		t.Helper()
		select {
		case <-c.exitLatch:
		default:
			t.Fatal("SIGINT did not request exit 130")
		}
		c.keyMu.Lock()
		err := c.exitErr
		c.keyMu.Unlock()
		var exit ExitError
		if !errors.As(err, &exit) || exit.Code != 130 {
			t.Fatalf("signal exit error = %v, want ExitError{130}", err)
		}
	}

	t.Run("busy=owner_busy_presentation_idle_cancels_and_stays_open", func(t *testing.T) {
		a, c, out, source, _ := busySourceCLI(t)
		busy, err := a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("source busy after the hold submit = %v (err %v), want the owner claim", busy, err)
		}
		if c.busy {
			t.Fatal("CLI busy flag set before the turn_start event drained; the SIGINT decision must not depend on event-derived busy")
		}
		if cliState(c.state.Load()) != stateIdle {
			t.Fatalf("CLI state = %v, want idle (turn_start undrained)", cliState(c.state.Load()))
		}
		// The signal handler must never wait on the render lock: hold it.
		c.mu.Lock()
		done := make(chan struct{})
		go func() {
			c.handleSignal(syscall.SIGINT)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			c.mu.Unlock()
			t.Fatal("handleSignal blocked behind the CLI render lock")
		}
		c.mu.Unlock()
		select {
		case <-c.exitLatch:
			t.Fatal("busy selected live SIGINT must cancel and stay open, not exit 130")
		default:
		}
		if out.Len() != 0 {
			t.Fatalf("signal path wrote the terminal: %q", out.String())
		}
		// The signal cancelled the owner turn; it ends and the owner returns
		// to idle (busySourceCLI's cleanup releases the blocked provider).
		waitUntilAgentIdle(t, a)
	})

	t.Run("idle=exits_130", func(t *testing.T) {
		a, _ := newTestAgentWithBaseURL(t, "http://127.0.0.1:9/v1")
		startTestAgent(t, a)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		c := New(a)
		out := new(bytes.Buffer)
		c.out = out
		c.setCurrentSessionID(id)
		c.handleSignal(syscall.SIGINT)
		assertExit130(t, c)
		if out.Len() != 0 {
			t.Fatalf("signal path wrote the terminal: %q", out.String())
		}
	})

	t.Run("empty=exits_130", func(t *testing.T) {
		a, _ := newTestAgentWithBaseURL(t, "http://127.0.0.1:9/v1")
		startTestAgent(t, a)
		c := New(a)
		c.out = new(bytes.Buffer)
		c.setCurrentSessionID("")
		c.handleSignal(syscall.SIGINT)
		assertExit130(t, c)
	})

	t.Run("missing=exits_130_unrelated_busy_untouched", func(t *testing.T) {
		// The owner is busy on an unrelated live unit while the CLI's selected
		// attachment is missing: the SIGINT authority is the exact selected
		// session, not the owner-global busy state or the backend current, so
		// the signal exits 130 and leaves the unrelated busy turn uncancelled.
		a, c, out, source, _ := busySourceCLI(t)
		busy, err := a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("source busy before the signal = %v (err %v), want the unrelated busy turn", busy, err)
		}
		c.setCurrentSessionID("does-not-exist")
		c.handleSignal(syscall.SIGINT)
		assertExit130(t, c)
		if out.Len() != 0 {
			t.Fatalf("signal path wrote the terminal: %q", out.String())
		}
		busy, err = a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("unrelated busy turn was cancelled by the missing-selection SIGINT: busy=%v err=%v", busy, err)
		}
	})

	t.Run("readonly=exits_130_unrelated_busy_untouched", func(t *testing.T) {
		// The owner is busy on an unrelated live unit while the CLI's selected
		// attachment is read-only (another process drives it, so it is not
		// live in this owner): the signal exits 130 and leaves the unrelated
		// busy turn uncancelled, rejecting owner-global/backend-current
		// authority.
		a, c, out, source, _ := busySourceCLI(t)
		busy, err := a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("source busy before the signal = %v (err %v), want the unrelated busy turn", busy, err)
		}
		c.setCurrentSessionID("ro-session")
		c.sv().SetReadOnly("ro-session")
		c.handleSignal(syscall.SIGINT)
		assertExit130(t, c)
		if out.Len() != 0 {
			t.Fatalf("signal path wrote the terminal: %q", out.String())
		}
		busy, err = a.BusyForSession(source)
		if err != nil || !busy {
			t.Fatalf("unrelated busy turn was cancelled by the read-only-selection SIGINT: busy=%v err=%v", busy, err)
		}
	})
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
	body, ok := extractFunctionBody(string(src), "func (c *CLI) readKeysFrom(")
	if !ok {
		t.Fatal("readKeysFrom not found")
	}
	req := strings.Index(body, "c.requestExit(")
	clo := strings.Index(body, "c.closeKeys()")
	if req < 0 || clo < 0 || req > clo {
		t.Fatalf("readKeysFrom must set the exit latch (requestExit) before closing key admission (closeKeys); requestExit@%d closeKeys@%d", req, clo)
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

// recordingSubmitAdapter wraps the real agent and records the session each
// submit names, so a test can assert which session a submit targeted without
// inferring it from the agent's state.
type recordingSubmitAdapter struct {
	agent.AdapterService
	submitted []string
}

func (r *recordingSubmitAdapter) SubmitToSession(ctx context.Context, sessionID, content string) (agent.SubmitResult, error) {
	r.submitted = append(r.submitted, sessionID)
	return r.AdapterService.SubmitToSession(ctx, sessionID, content)
}

// recordingReserveAdapter wraps the real agent and records every selection
// reservation the CLI acquires, so a test can assert which navigation paths
// reserve the source and that archive/delete acquire none.
type recordingReserveAdapter struct {
	agent.AdapterService
	reserves []string
}

func (r *recordingReserveAdapter) ReserveSelectionSource(sessionID string) (func(), error) {
	r.reserves = append(r.reserves, sessionID)
	return r.AdapterService.ReserveSelectionSource(sessionID)
}

// blockingCompactAdapter wraps the real agent and blocks every
// CompactNowForSession until release closes, then returns the configured
// error, so a test can hold a compact in flight across a navigation and
// decide its completion outcome deterministically.
type blockingCompactAdapter struct {
	agent.AdapterService
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (b *blockingCompactAdapter) CompactNowForSession(ctx context.Context, sessionID string) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.err
}

// stagedNavigationAdapter wraps the real agent and can hold each navigation
// stage open until the test releases it: the destination candidate list
// (SessionListForProjectPath), the destination open, and the project list.
// While a stage is held, the test drives the owner directly to observe
// whether the source reservation is held.
type stagedNavigationAdapter struct {
	agent.AdapterService
	blockList    bool
	listEntered  chan struct{}
	listRelease  chan struct{}
	listOnce     sync.Once
	blockOpen    bool
	openEntered  chan struct{}
	openRelease  chan struct{}
	openOnce     sync.Once
	blockProject bool
	projEntered  chan struct{}
	projRelease  chan struct{}
	projOnce     sync.Once
}

func (s *stagedNavigationAdapter) SessionListForProjectPath(projectPath, state string) ([]agent.SessionSummary, error) {
	if s.blockList {
		s.listOnce.Do(func() { close(s.listEntered) })
		<-s.listRelease
	}
	return s.AdapterService.SessionListForProjectPath(projectPath, state)
}

func (s *stagedNavigationAdapter) OpenSession(id string) (agent.SessionSummary, error) {
	if s.blockOpen {
		s.openOnce.Do(func() { close(s.openEntered) })
		<-s.openRelease
	}
	return s.AdapterService.OpenSession(id)
}

func (s *stagedNavigationAdapter) ProjectList() ([]agent.ProjectSummary, error) {
	if s.blockProject {
		s.projOnce.Do(func() { close(s.projEntered) })
		<-s.projRelease
	}
	return s.AdapterService.ProjectList()
}

// waitUntilSourceIdleAndDrained waits until the source session is neither
// busy nor carrying queued items, sustained briefly so a pending auto-drain
// cannot still be about to start another turn.
func waitUntilSourceIdleAndDrained(t *testing.T, a *agent.Agent, source string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		busy, err := a.BusyForSession(source)
		if err == nil && !busy {
			if q, err := a.QueueSnapshotForSession(source); err == nil && len(q.Items) == 0 {
				stable++
				if stable >= 3 {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("source did not become idle and drained")
}

// blockingProvider holds the first request open until released and answers
// every later request immediately, so a test can hold turn 1 in flight while
// a second submit queues, then let the queue drain.
func blockingProvider(t *testing.T) (server *httptest.Server, firstEntered, firstRelease chan struct{}) {
	t.Helper()
	firstEntered = make(chan struct{})
	firstRelease = make(chan struct{})
	var reqs atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			close(firstEntered)
			<-firstRelease
		}
		writeTestResponse(w, "ok")
	}))
	t.Cleanup(server.Close)
	return server, firstEntered, firstRelease
}

// writeTestResponse answers a provider request with a plain-text body, the
// shape the CLI test agents' engine loop accepts as a completed turn.
func writeTestResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, content)
}

// markerBlockingWriter passes writes through until one contains the marker,
// then blocks every write until release closes, so a test can stall a render
// at a chosen point — the input prompt ("> ") after a successful commit, or
// the error marker ("✕") of a failure render. The session menu renders never
// contain either marker, so the block lands only on the commit or failure
// render.
type markerBlockingWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	marker  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	blocked bool
}

func (w *markerBlockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.blocked {
		w.buf.Write(p)
		if strings.Contains(string(p), w.marker) {
			w.blocked = true
			w.mu.Unlock()
			w.once.Do(func() { close(w.entered) })
			<-w.release
			return len(p), nil
		}
	}
	w.mu.Unlock()
	return len(p), nil
}

// busySourceCLI builds a CLI over an agent whose provider server holds every
// request open until release, so a submitted turn stays claimed/running in
// the owner deterministically for the whole test. The CLI's event handler is
// wired to the delivery FIFO but nothing drains it unless the test does, so
// the CLI's own event-derived presentation state stays exactly what the test
// drives.
func busySourceCLI(t *testing.T) (*agent.Agent, *CLI, *bytes.Buffer, string, func()) {
	t.Helper()
	block := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		http.Error(w, "released", http.StatusOK)
	}))
	t.Cleanup(server.Close)

	a, _ := newTestAgentWithBaseURL(t, server.URL)
	startTestAgent(t, a)
	source, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("source NewSession: %v", err)
	}
	c := New(a)
	c.agent.SetEventHandler(c.enqueueEvent)
	out := new(bytes.Buffer)
	c.out = out
	c.setCurrentSessionID(source)
	// Submit a real turn: the claim is synchronous, so the source unit is
	// busy as soon as SubmitToSession returns, and the blocked provider holds
	// the turn open until release.
	if _, err := a.SubmitToSession(context.Background(), source, "hold"); err != nil {
		t.Fatalf("submit hold turn: %v", err)
	}
	t.Cleanup(func() {
		release()
		_ = a.CancelSession(source)
	})
	return a, c, out, source, release
}

func newTestAgentWithBaseURL(t *testing.T, baseURL string) (*agent.Agent, string) {
	t.Helper()
	return newTestAgentAtHome(t, baseURL, t.TempDir(), t.TempDir())
}

// newTestAgentAtHome builds an owner over the given home and project root, so
// several owners can share one home for cross-process claim testing.
func newTestAgentAtHome(t *testing.T, baseURL, home, projectRoot string) (*agent.Agent, string) {
	t.Helper()

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

// newTestAgentPair builds two owners over the same home with distinct project
// roots, so one owner's live sessions hold their claims against the other.
func newTestAgentPair(t *testing.T) (*agent.Agent, *agent.Agent) {
	t.Helper()
	home := t.TempDir()
	first, _ := newTestAgentAtHome(t, "http://127.0.0.1:9/v1", home, t.TempDir())
	second, _ := newTestAgentAtHome(t, "http://127.0.0.1:9/v1", home, t.TempDir())
	t.Cleanup(func() {
		runtime.KeepAlive(first)
		runtime.KeepAlive(second)
	})
	return first, second
}

// stampSessionActivity rewrites a session's persisted last activity so the
// active-session listing order is deterministic instead of same-second ties.
func stampSessionActivity(t *testing.T, a *agent.Agent, projectPath, id string, lastActivity int64) {
	t.Helper()
	proj, err := a.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("project for %s: %v", projectPath, err)
	}
	metaPath := filepath.Join(a.Projects().SessionsRoot(proj.ID), id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	meta.LastActivity = lastActivity
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("rewrite meta: %v", err)
	}
}

// listingFailSvc embeds a real owner but fails the active-session listing, so
// a caller that reads the listing result must surface the error instead of
// treating it as an empty project.
type listingFailSvc struct {
	agent.AdapterService
	listErr error
}

func (f *listingFailSvc) SessionListForProjectPath(projectPath, state string) ([]agent.SessionSummary, error) {
	return nil, f.listErr
}

func startTestAgent(t *testing.T, a *agent.Agent) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.Init(ctx)
	t.Cleanup(func() {
		cancel()
		if !a.ShutdownOwner() {
			t.Error("CLI agent shutdown reported abandoned work")
		}
	})
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
		go func() { mainDone <- c.mainLoop(ctx, nil) }()

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
		// runInteractive must not run mainLoop inline: the inline form puts
		// the whole teardown behind a blocked terminal write.
		body, ok := extractFunctionBody(string(src), "func (c *CLI) runInteractive(")
		if !ok {
			t.Fatal("runInteractive not found")
		}
		if strings.Contains(body, "= c.mainLoop(") {
			t.Fatal("runInteractive must not run mainLoop inline; teardown would sit behind a blocked terminal write")
		}
		// runInteractive must wait on the exit latch, so a signal starts
		// teardown while mainLoop is still blocked, and the teardown must
		// follow that wait.
		latchIdx := strings.Index(body, "case <-c.exitLatch:")
		teardownIdx := strings.Index(body, "c.closeKeys()")
		if latchIdx < 0 {
			t.Fatal("runInteractive must wait on the exit latch so teardown does not sit behind mainLoop")
		}
		if teardownIdx < 0 || teardownIdx < latchIdx {
			t.Fatal("runInteractive's teardown must follow the latch wait, not mainLoop's return")
		}
		// Run must not launch mainLoop itself: the startup presentation and
		// the loop launch belong to runInteractive, so the exit authority and
		// the reader exist before the first startup write.
		runBody, ok := extractFunctionBody(string(src), "func (c *CLI) Run(")
		if !ok {
			t.Fatal("Run not found")
		}
		if strings.Contains(runBody, "c.mainLoop(") {
			t.Fatal("Run must not launch mainLoop; runInteractive establishes the exit authority and the reader before the startup write")
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
		body, ok := extractFunctionBody(string(src), "func (c *CLI) runInteractive(")
		if !ok {
			t.Fatal("runInteractive not found")
		}
		signals := []string{"syscall.SIGWINCH", "syscall.SIGINT", "syscall.SIGTERM"}
		for _, sig := range signals {
			idx := strings.Index(body, sig)
			if idx < 0 {
				t.Fatalf("runInteractive no longer registers %s", sig)
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

	t.Run("startup=renders_first_on_main_loop", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so
		// mainLoop is driven directly with the prepared startup closure, the
		// exact shape runInteractive launches. Keys and events are already
		// buffered before mainLoop starts; the startup closure is its first
		// action, so the startup output is the first rendered content and
		// nothing overtakes it.
		c := New(nil)
		var out bytes.Buffer
		c.out = &out
		c.enqueueKey(keyMsg{Rune: 'k'})
		c.enqueueEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Result: "ready-event"})

		started := make(chan struct{})
		startup := func() {
			c.printLine("STARTUP-MARKER")
			close(started)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.mainLoop(ctx, startup) }()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("startup closure did not run as mainLoop's first action")
		}
		c.requestExit(ExitError{Code: 0})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("mainLoop did not unwind after the startup marker")
		}
		// mainLoop has returned; the buffer is quiescent.
		s := out.String()
		if !strings.HasPrefix(s, "STARTUP-MARKER") {
			t.Fatalf("startup did not render first (buffered key/event overtook it): %q", s)
		}
	})

	t.Run("startup=blocked_write_sigterm_unwinds", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be
		// driven in a test (term.MakeRaw on os.Stdin requires a real TTY), so
		// runInteractive is driven directly with a pipe input and a blocked
		// startup write. The startup write stalls inside mainLoop, yet SIGTERM
		// still unwinds to the joined owner cleanup: the exit authority and
		// the reader existed before the first startup write.
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		startTestAgent(t, a)
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(a)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0

		r, w := io.Pipe()
		defer r.Close()
		defer w.Close()

		done := make(chan error, 1)
		go func() { done <- c.runInteractive(context.Background(), func() { c.printLine("startup") }, r) }()

		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("startup write did not enter the blocked output")
		}
		c.handleSignal(syscall.SIGTERM)
		select {
		case err := <-done:
			var exit ExitError
			if !errors.As(err, &exit) || exit.Code != 130 {
				t.Fatalf("runInteractive returned %v, want the clean ExitError{130} (owner cleanup completed)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("runInteractive did not unwind to owner cleanup while the startup write was blocked")
		}
		// The blocked write is still stalled: teardown did not wait for it.
		select {
		case <-release:
			t.Fatal("teardown released the blocked write")
		default:
		}
		close(release)
	})

	t.Run("startup=blocked_write_eof_unwinds", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be
		// driven in a test (term.MakeRaw on os.Stdin requires a real TTY), so
		// runInteractive is driven directly with a pipe input and a blocked
		// startup write. Closing the pipe (stdin EOF) starts owner cleanup
		// independently of the blocked write.
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		startTestAgent(t, a)
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(a)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0

		r, w := io.Pipe()
		defer r.Close()

		done := make(chan error, 1)
		go func() { done <- c.runInteractive(context.Background(), func() { c.printLine("startup") }, r) }()

		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("startup write did not enter the blocked output")
		}
		w.Close() // stdin EOF
		select {
		case err := <-done:
			var exit ExitError
			if !errors.As(err, &exit) || exit.Code != 0 {
				t.Fatalf("runInteractive returned %v, want the clean EOF ExitError{0} (owner cleanup completed)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("runInteractive did not unwind to owner cleanup on EOF while the startup write was blocked")
		}
		select {
		case <-release:
			t.Fatal("teardown released the blocked write")
		default:
		}
		close(release)
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

	t.Run("action_confirm_exit_performs_no_mutation", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the
		// command chain is driven through handleKeyIdle — the key handler
		// mainLoop calls — with an injected key source; the confirmation read
		// failure is the exit error nextKey reports once the latch is set. The
		// error must reach the handler: a confirmation that consumes it lets
		// the loop render another prompt, and the fork proceeds as a "no".
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

		// Select the seeded turn, then "Fork from here"; the confirmation key
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

	t.Run("fork_revert_failure_reports_warning_on_success", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be driven
		// in a test (term.MakeRaw on os.Stdin requires a real TTY), so the
		// command chain is driven through handleKeyIdle — the key handler
		// mainLoop calls — with an injected key source. A fork whose
		// best-effort code revert fails is still a successful fork: the menu
		// switches to the fork and the failure must reach the user as a
		// warning on the success path, not as the operation's error.
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		a, _ := newTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := a.AppendUserMessage("fork point"); err != nil {
			t.Fatalf("seed fork point: %v", err)
		}
		sub := filepath.Join(a.ProjectRoot(), "sub")
		path := filepath.Join(sub, "created-after-fork.txt")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		turn, err := a.AppendUserMessage("create after fork")
		if err != nil {
			t.Fatalf("seed snapshot turn: %v", err)
		}
		entryID, _, err := a.Store().SnapshotResolvedEntry(turn, path, path)
		if err != nil {
			t.Fatalf("snapshot entry: %v", err)
		}
		if err := os.WriteFile(path, []byte("later\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := a.Store().RecordSnapshotContent(turn, entryID, []byte("later\n")); err != nil {
			t.Fatalf("record snapshot content: %v", err)
		}
		sourceID := a.SessionCurrent().ID
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(sub, 0o700) }()

		c := New(a)
		c.setCurrentSessionID(sourceID)
		var out bytes.Buffer
		c.out = &out

		// Select the seeded turn, choose "Fork from here", and answer the
		// code confirmation with Yes.
		keys := []keyMsg{
			{Special: keyEnter},
			{Special: keyDown},
			{Special: keyEnter},
			{Special: keyEnter},
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

		c.input.Set("/fork")
		if err := c.handleKeyIdle(keyMsg{Special: keyEnter}); err != nil {
			t.Fatalf("handleKeyIdle: %v", err)
		}

		// The fork is published and selected; the failed code revert is
		// reported as a warning on that success path.
		forkID := a.SessionCurrent().ID
		if forkID == "" || forkID == sourceID {
			t.Fatalf("fork current = %q, source %q", forkID, sourceID)
		}
		if !strings.Contains(out.String(), "code revert failed") {
			t.Fatalf("failed code revert warning not shown in output:\n%s", out.String())
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

	t.Run("startup_write_stalled=restore_still_runs", func(t *testing.T) {
		// Exception, recorded per the contract-test rule: Run cannot be
		// driven in a test (term.MakeRaw on os.Stdin requires a real TTY), so
		// runInteractive is driven directly. The finding-6 shape: the startup
		// write itself is the blocked one. runInteractive returns (teardown
		// never waits for the abandoned mainLoop) and the deferred
		// restoreTerminal still runs with the write stalled — the mode
		// restore is a control operation on the descriptor and cannot block
		// on the output.
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(nil)
		c.out = &blockingWriter{entered: entered, release: release}
		c.rawFd = 0
		c.oldState = &term.State{}

		r, w := io.Pipe()
		defer r.Close()
		defer w.Close()

		done := make(chan error, 1)
		go func() { done <- c.runInteractive(context.Background(), func() { c.printLine("startup") }, r) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("startup write did not stall")
		}
		c.handleSignal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runInteractive did not return while the startup write was stalled")
		}
		select {
		case <-release:
			t.Fatal("teardown released the blocked write")
		default:
		}
		c.restoreTerminal()
		if c.oldState != nil {
			t.Fatal("restoreTerminal did not restore the terminal mode")
		}
		close(release)
	})
}

// TestFoldAbandonedShutdown pins the exit-code rule for an abandoned
// shutdown: the fold must force a non-zero exit and never lower or
// overwrite a non-zero code already present. The code is resolved the
// same way the exit path resolves it — the first ExitError the joined
// error yields — mirroring the selector the root command layer applies
// to the error Run returns.
func TestFoldAbandonedShutdown(t *testing.T) {
	resolve := func(err error) int {
		var exit ExitError
		if errors.As(err, &exit) {
			return exit.Code
		}
		return -1
	}

	t.Run("abandoned_exit_becomes_1", func(t *testing.T) {
		// /exit supplies ExitError{Code: 0}; the fold must join
		// ExitError{Code: 1} ahead of it so the selector reaches the
		// forced code first.
		if code := resolve(foldAbandonedShutdown(ExitError{Code: 0})); code != 1 {
			t.Fatalf("folded /exit resolves to %d, want 1", code)
		}
	})

	t.Run("abandoned_signal_keeps_130", func(t *testing.T) {
		// A signal supplies ExitError{Code: 130}; the message joins after
		// it, so the non-zero code survives.
		if code := resolve(foldAbandonedShutdown(ExitError{Code: 130})); code != 130 {
			t.Fatalf("folded signal exit resolves to %d, want 130", code)
		}
	})

	t.Run("clean_exit_stays_0", func(t *testing.T) {
		// No abandonment: the error /exit supplies resolves to 0 on its own.
		if code := resolve(ExitError{Code: 0}); code != 0 {
			t.Fatalf("clean /exit resolves to %d, want 0", code)
		}
	})
}

// lifecycleOutcomeAdapter drives committed and plain outcomes through every
// CLI mutation path (inline/menu new, project switch with create fallback,
// fork, archive/delete). It records probe events in order so tests can assert
// release-before-render ordering and reconciliation behavior without touching
// real persistence or snapshots. Every unoverridden probe panics on the nil-
// embedded interface: an unexpected owner call fails loudly instead of
// silently succeeding; nothing here activates a real producer — only consumer
// classification is under test.
type lifecycleOutcomeAdapter struct {
	agent.AdapterService

	newID   string                 // destination id new/create fallback returns for success and committed rows
	outcome error                  // nil = success; plain, or the wrapped committed failure (shared by every mutation path)
	forkRes agent.TurnActionResult // prepared fork result for the revert menu's fork action
	list    []agent.SessionSummary // /session menu content and selectIndex lookup

	events *[]string
}

func (f *lifecycleOutcomeAdapter) log(ev string) {
	if f.events != nil {
		*f.events = append(*f.events, ev)
	}
}

// ReserveSelectionSource records the reservation; its release records where it ends.
func (f *lifecycleOutcomeAdapter) ReserveSelectionSource(sessionID string) (func(), error) {
	f.log("reserve:" + sessionID)
	return func() { f.log("release") }, nil
}

// NewSessionForProjectPath is the create path for inline/menu new and project-switch fallback.
func (f *lifecycleOutcomeAdapter) NewSessionForProjectPath(_, _ string) (string, error) {
	f.log("new:" + f.newID)
	return f.newID, f.outcome
}

func (f *lifecycleOutcomeAdapter) NewSessionForProjectPathWithBoundary(_, _ string, emit func(agent.HydrationState, error)) (string, error) {
	f.log("new:" + f.newID)
	var committed *snapshot.CommittedMutationError
	if emit != nil && f.newID != "" && (f.outcome == nil || errors.As(f.outcome, &committed)) {
		emit(agent.HydrationState{Session: agent.SessionSummary{ID: f.newID}}, f.outcome)
	}
	return f.newID, f.outcome
}

// SessionListForProjectPath feeds menu construction and selectIndex; it returns the prepared list.
func (f *lifecycleOutcomeAdapter) SessionListForProjectPath(_, _ string) ([]agent.SessionSummary, error) {
	f.log("list")
	return f.list, nil
}

// ApplyTurnActionForSession serves only showRevertMenu's fork action: any other
// action panics on the nil embed if it reaches the owner.
func (f *lifecycleOutcomeAdapter) ApplyTurnActionForSession(_ string, _ int, _ string, _ bool) (agent.TurnActionResult, error) {
	f.log("fork")
	return f.forkRes, f.outcome
}

// SessionArchive returns the prepared outcome.
func (f *lifecycleOutcomeAdapter) SessionArchive(id string) error {
	f.log("archive:" + id)
	return f.outcome
}

// SessionDelete returns the prepared outcome.
func (f *lifecycleOutcomeAdapter) SessionDelete(id string) error {
	f.log("delete:" + id)
	return f.outcome
}

// --- Refresh and menu probes: recorded only; their values are irrelevant to these assertions, and any probe not overridden here fails loudly on the nil embed. ---

func (f *lifecycleOutcomeAdapter) CurrentModelForSession(_ string) (agent.ModelInfo, error) {
	f.log("model")
	return agent.ModelInfo{}, errors.New("not available in this test")
}

func (f *lifecycleOutcomeAdapter) SessionSummaryForSessionOrPersisted(id string) (agent.SessionSummary, error) {
	f.log("summary:" + id)
	return agent.SessionSummary{ID: id}, nil
}

func (f *lifecycleOutcomeAdapter) SessionMessagesFor(_ string) ([]agent.DisplayMessage, error) {
	f.log("messages")
	return []agent.DisplayMessage{{Type: "user", Content: "seed turn", Turn: 1}}, nil
}

func (f *lifecycleOutcomeAdapter) QueueSnapshotForSession(_ string) (agent.QueueState, error) {
	f.log("queue")
	return agent.QueueState{}, nil
}

// newLifecycleCLI builds a headless CLI wired to f on both the adapter and scope surfaces.
func newLifecycleCLI(t *testing.T, f *lifecycleOutcomeAdapter) (*CLI, *bytes.Buffer) {
	t.Helper()
	out := new(bytes.Buffer)
	c := &CLI{out: out, mu: &sync.Mutex{}, input: newInputLine(), history: newInputHistory()}
	c.width.Store(80)
	if f.events == nil {
		evs := []string{}
		f.events = &evs
	}
	c.agent = f
	c.scope = agent.NewAdapterScope(f, "/proj")
	return c, out
}

func indexEv(evs []string, want string) int {
	for i, ev := range evs {
		if ev == want {
			return i
		}
	}
	return -1
}

func hasEventPrefix(evs []string, prefix string) bool {
	for _, ev := range evs {
		if strings.HasPrefix(ev, prefix) {
			return true
		}
	}
	return false
}

// keySequence yields the prepared keys in order; a read past the end is an error so any unexpected menu over-read fails loudly.
func keySequence(keys ...keyMsg) func() (keyMsg, error) {
	next := 0
	return func() (keyMsg, error) {
		if next < len(keys) {
			k := keys[next]
			next++
			return k, nil
		}
		return keyMsg{}, errors.New("no more prepared keys")
	}
}

// TestCLILifecycleCommittedOutcomes proves the CLI consumer table row by row: a committed outcome adopts its durable destination (new/fork/project fallback) or runs exactly the stale-safe success reconciliation (archive/delete), releases before rendering, and surfaces the rejection; plain outcomes retain source and routing exactly as they were.
func TestCLILifecycleCommittedOutcomes(t *testing.T) {
	committed := &snapshot.CommittedMutationError{Err: errors.New("commit failed")}
	plain := errors.New("precommit failure")

	t.Run("inline_new_committed_adopts_destination", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: committed}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		dispatchSelection(t, c, "/new", "")

		wantCurrent(t, c, "dest")
		evs := *f.events
		r := indexEv(evs, "release")
		if r < 0 || !hasEventPrefix(evs[:r+1], "reserve:src") {
			t.Fatalf("events = %v, want the source reserved and released", evs)
		}
		if m := indexEv(evs, "model"); m <= r {
			t.Fatalf("events = %v, want release before the destination render", evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("inline_new_plain_retains_source", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: plain}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		dispatchSelection(t, c, "/new", "")

		wantCurrent(t, c, "src")
		evs := *f.events
		if indexEv(evs, "model") >= 0 || indexEv(evs, "summary:dest") >= 0 {
			t.Fatalf("events = %v, want no destination render after a plain failure", evs)
		}
		if r := indexEv(evs, "release"); r != len(evs)-1 {
			t.Fatalf("events = %v, want the release to end the path — nothing renders after it", evs)
		}
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("inline_new_success_releases_before_render", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest"} // nil outcome = success
		c, _ := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		dispatchSelection(t, c, "/new", "")

		wantCurrent(t, c, "dest")
		evs := *f.events
		if r := indexEv(evs, "release"); r < 0 || indexEv(evs, "model") <= r {
			t.Fatalf("events = %v, want release before the destination render", evs)
		}
	})

	t.Run("menu_new_committed_adopts_destination", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: committed, list: []agent.SessionSummary{{ID: "src"}}}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = keySequence(keyMsg{Rune: 'n'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "dest")
		evs := *f.events
		r := indexEv(evs, "release")
		if r < 0 || !hasEventPrefix(evs[:r+1], "reserve:src") {
			t.Fatalf("events = %v, want the source reserved and released", evs)
		}
		if m := indexEv(evs, "model"); m <= r {
			t.Fatalf("events = %v, want release before the destination render", evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("menu_new_plain_retains_source", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: plain, list: []agent.SessionSummary{{ID: "src"}}}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = keySequence(keyMsg{Rune: 'n'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "src")
		evs := *f.events
		if indexEv(evs, "model") >= 0 || indexEv(evs, "summary:dest") >= 0 {
			t.Fatalf("events = %v, want no destination render after a plain failure", evs)
		}
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("project_fallback_committed_adopts_project_and_destination", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: committed}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.projectSwitch("/other")

		wantProjectPath(t, c, "/other")
		wantCurrent(t, c, "dest")
		evs := *f.events
		r := indexEv(evs, "release")
		if r < 0 || !hasEventPrefix(evs[:r+1], "reserve:src") {
			t.Fatalf("events = %v, want the source reserved and released", evs)
		}
		if m := indexEv(evs, "model"); m <= r {
			t.Fatalf("events = %v, want release before the destination render", evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("project_fallback_plain_retains_route_and_source", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest", outcome: plain}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.projectSwitch("/other")

		wantProjectPath(t, c, "/proj")
		wantCurrent(t, c, "src")
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("project_fallback_success_releases_before_render", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{newID: "dest"} // nil outcome = success
		c, _ := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.projectSwitch("/other")

		wantProjectPath(t, c, "/other")
		wantCurrent(t, c, "dest")
		evs := *f.events
		if r := indexEv(evs, "release"); r < 0 || indexEv(evs, "model") <= r {
			t.Fatalf("events = %v, want release before the destination render", evs)
		}
	})

	// forkKeys yields a fresh key source that walks the revert menu to the
	// fork action and confirms it: select the single seeded turn, move from
	// "Revert code" onto "Fork from here", choose it, then accept the
	// also-revert-code question (its Yes/No menu answers with Enter on "Yes"). Each row builds its own copy — a key source is consumed by reads.
	forkKeys := func() func() (keyMsg, error) {
		return keySequence(
			keyMsg{Special: keyEnter}, // select the single seeded turn
			keyMsg{Special: keyDown},  // Revert code -> Fork from here
			keyMsg{Special: keyEnter}, // choose fork
			keyMsg{Special: keyEnter}, // accept "also revert code?" (Yes is the default)
		)
	}

	t.Run("fork_committed_adopts_destination", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, forkRes: agent.TurnActionResult{Session: agent.SessionSummary{ID: "forkdest"}}}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = forkKeys()
		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatalf("dispatchCommand(/revert): %v", err)
		}

		wantCurrent(t, c, "forkdest")
		evs := *f.events
		if hasEventPrefix(evs, "reserve:") {
			t.Fatalf("events = %v, want no selection reservation on the fork path", evs)
		}
		if r := indexEv(evs, "fork"); r < 0 || indexEv(evs, "summary:forkdest") <= r {
			t.Fatalf("events = %v, want the destination render after the committed fork", evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("fork_plain_retains_source", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: plain} // zero forkRes carries no adopted session
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = forkKeys()
		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatalf("dispatchCommand(/revert): %v", err)
		}

		wantCurrent(t, c, "src")
		evs := *f.events
		if r := indexEv(evs, "fork"); r < 0 || r != len(evs)-1 {
			t.Fatalf("events = %v, want the plain fork to end without a destination render", evs)
		}
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("fork_success_adopts_destination", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{forkRes: agent.TurnActionResult{Session: agent.SessionSummary{ID: "forkdest"}}} // nil outcome = success
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = forkKeys()
		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatalf("dispatchCommand(/revert): %v", err)
		}

		wantCurrent(t, c, "forkdest")
		s := out.String()
		if strings.Contains(s, committed.Error()) || strings.Contains(s, plain.Error()) {
			t.Fatalf("output = %q, want no rejection on a successful fork", s)
		}
	})

	// A committed fork's returned result still carries the best-effort code revert's skipped restores:
	// they must stay visible exactly as on success — adopt and refresh the destination first, print the
	// skips from that same returned result, then surface the rejection. The order is what this row pins:
	// no reservation exists on a fork path (the source-retention rows above prove it), so the sequence is
	// adoption → refresh probes → skipped restores → error text in one output stream.
	t.Run("fork_committed_with_skips_prints_them_before_error", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, forkRes: agent.TurnActionResult{Session: agent.SessionSummary{ID: "forkdest"}, SkippedFiles: []snapshot.SkippedRevert{{Path: "/proj/kept.txt", Reason: "diverged"}}}}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("src")
		c.readKeyFn = forkKeys()
		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatalf("dispatchCommand(/revert): %v", err)
		}

		wantCurrent(t, c, "forkdest") // the destination was adopted before its rejection surfaced — same as every committed row here
		evs := *f.events
		if hasEventPrefix(evs, "reserve:") {
			t.Fatalf("events = %v; a fork path never reserves a selection source", evs)
		}
		if r := indexEv(evs, "fork"); r < 0 || indexEv(evs, "summary:forkdest") <= r {
			t.Fatalf("events = %v; the destination refresh must follow the committed fork's adoption", evs)
		}

		s := out.String()
		const skipsHeader = "kept 1 file changed outside this session:"
		if i, e := strings.Index(s, skipsHeader), strings.Index(s, "/proj/kept.txt (diverged)"); i < 0 || e < 0 {
			t.Fatalf("output = %q; the returned fork result's skipped restores must stay visible on a committed failure", s)
		} else if errIdx := strings.Index(s, committed.Error()); errIdx < 0 || i > errIdx || e > errIdx {
			t.Fatalf("skips at index %d/%d, error at %d; want adopt → refresh → skipped restores → rejection in that order: %q", i, e, errIdx, s)
		}
	})

	lifecycleList := []agent.SessionSummary{{ID: "a"}, {ID: "b"}}

	// The committed × plain grid is completed per operation so neither op's reconciliation behavior rests on the other's rows: archive already had its current cells and delete its noncurrent ones; these add each op's inverse committed case plus that case's nearest forbidden plain sibling, and give archive its own success control (delete_success_control_clears_current pins delete's).
	t.Run("archive_committed_noncurrent_preserves_current", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a") // target b is noncurrent; the stale-safe reconciliation must preserve it exactly as a plain failure would leave it — only the rejection differs
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "a") // committed noncurrent removal preserves current — delete_committed_noncurrent_preserves_current is the same cell through the other op; both must hold per operation
		evs := *f.events
		if hasEventPrefix(evs, "reserve:") {
			t.Fatalf("events = %v; an archive path never reserves a selection source", evs)
		}
		if last := evs[len(evs)-1]; last != "archive:b" {
			t.Fatalf("events end at %q; removing a noncurrent session must refresh nothing even when committed: %v", last, evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("delete_committed_current_clears_and_rejects", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("b") // target b IS the current session: exactly the success reconciliation runs before the rejection
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		if got, _ := c.currentSession(); got != "" {
			t.Fatalf("current after committed delete of the current session = %q, want cleared by the stale-safe reconciliation before the rejection", got)
		}
		evs := *f.events
		if last := evs[len(evs)-1]; last != "delete:b" {
			t.Fatalf("events end at %q; reconciling a removed current to empty runs without owner reads: %v", last, evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection after the reconciliation", out.String())
		}
	})

	t.Run("archive_plain_noncurrent_retains_selection_without_refresh", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: plain, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a") // the nearest forbidden sibling of archive_committed_noncurrent_preserves_current above: same op and position; only the outcome class may differ in behavior — a plain failure reconciles nothing at all
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "a") // retained exactly as it was — no reconciliation ran that could have cleared or re-adopted anything
		evs := *f.events
		if last := evs[len(evs)-1]; last != "archive:b" {
			t.Fatalf("events end at %q; a plain archive must refresh nothing: %v", last, evs)
		}
		if !strings.Contains(out.String(), plain.Error()) || strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the plain rejection and no committed classification of it", out.String())
		}
	})

	t.Run("delete_plain_current_retains_selection_without_refresh", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: plain, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("b") // the nearest forbidden sibling of delete_committed_current_clears_and_rejects above: same op and position; a plain failure must NOT run the removed-current reconciliation that its committed twin does — archive_plain_current_retains_selection pins this cell through the other op, so both ops hold it independently
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "b") // still current — the deletion never ran durably and nothing may reconcile it away
		evs := *f.events
		if last := evs[len(evs)-1]; last != "delete:b" {
			t.Fatalf("events end at %q; a plain delete of the current session must clear/reconcile nothing: %v", last, evs)
		}
		if !strings.Contains(out.String(), plain.Error()) || strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the plain rejection and no committed classification of it", out.String())
		}
	})

	t.Run("archive_success_control_clears_current", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{list: lifecycleList} // nil outcome = success; target is current — archive's own success control, mirroring delete_success_control_clears_current through the other op with its own rendered line
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "a"), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		if got, _ := c.currentSession(); got != "" {
			t.Fatalf("current after archiving the current session = %q, want cleared by the success reconciliation", got)
		}
		evs := *f.events
		if last := evs[len(evs)-1]; last != "archive:a" {
			t.Fatalf("events end at %q; a successful archive reconciles to empty without owner reads: %v", last, evs)
		}
		s := out.String()
		if !strings.Contains(s, "session archived") || strings.Contains(s, committed.Error()) || strings.Contains(s, plain.Error()) {
			t.Fatalf("output = %q; want the archive success line and no rejection", s)
		}
	})

	t.Run("archive_committed_current_clears_and_rejects", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "a"), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		if got, _ := c.currentSession(); got != "" {
			t.Fatalf("current after committed archive of the current session = %q, want cleared by the success reconciliation", got)
		}
		evs := *f.events
		if hasEventPrefix(evs, "reserve:") {
			t.Fatalf("events = %v, want no selection reservation on an archive path", evs)
		}
		if last := evs[len(evs)-1]; last != "archive:a" {
			t.Fatalf("events end at %q; the reconcile to empty must run without owner reads: %v", last, evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("delete_committed_noncurrent_preserves_current", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: committed, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "a") // stale-safe reconciliation preserves a current the target is not
		evs := *f.events
		if last := evs[len(evs)-1]; last != "delete:b" {
			t.Fatalf("events end at %q; removing a noncurrent session must refresh nothing: %v", last, evs)
		}
		if !strings.Contains(out.String(), committed.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("delete_plain_noncurrent_retains_current_without_refresh", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: plain, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "a") // a plain precommit failure reconciles nothing — delete_committed_noncurrent_preserves_current is its forbidden sibling (same op and position, the outcome class that must not trigger it)
		evs := *f.events
		if last := evs[len(evs)-1]; last != "delete:b" {
			t.Fatalf("events end at %q; no reconciliation may run after a plain delete of a noncurrent session: %v", last, evs)
		}
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("archive_plain_current_retains_selection", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{outcome: plain, list: lifecycleList}
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("a")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "a"), keyMsg{Rune: 'a'})
		dispatchSelection(t, c, "/session", "")

		wantCurrent(t, c, "a") // a plain precommit failure reconciles nothing — the committed row above is its forbidden sibling
		evs := *f.events
		if last := evs[len(evs)-1]; last != "archive:a" {
			t.Fatalf("events end at %q; no reconciliation may run after a plain archive: %v", last, evs)
		}
		if !strings.Contains(out.String(), plain.Error()) {
			t.Fatalf("output = %q, want the rejection", out.String())
		}
	})

	t.Run("delete_success_control_clears_current", func(t *testing.T) {
		f := &lifecycleOutcomeAdapter{list: lifecycleList} // nil outcome = success; target is current
		c, out := newLifecycleCLI(t, f)
		c.setCurrentSessionID("b")
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, "b"), keyMsg{Rune: 'd'})
		dispatchSelection(t, c, "/session", "")

		if got, _ := c.currentSession(); got != "" {
			t.Fatalf("current after deleting the current session = %q, want cleared", got)
		}
		evs := *f.events
		if last := evs[len(evs)-1]; last != "delete:b" {
			t.Fatalf("events end at %q; success reconciliation to empty reads nothing: %v", last, evs)
		}
		s := out.String()
		if !strings.Contains(s, "session deleted") || strings.Contains(s, committed.Error()) || strings.Contains(s, plain.Error()) {
			t.Fatalf("output = %q, want the success line and no rejection", s)
		}
	})
}

func TestCLIStartupRealCommittedCreationAdoptsDestination(t *testing.T) {
	ag, _ := newTestAgent(t)
	c := New(ag)
	c.out = new(bytes.Buffer)
	root := ag.Projects().Root()
	atomicfs.SyncDirFunc = func(dir string) error {
		if strings.HasSuffix(dir, string(filepath.Separator)+"sessions") {
			return errors.New("injected startup publication sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	err := c.initializeSession(context.Background())
	if err == nil {
		t.Fatal("CLI startup committed publication failure returned nil")
	}
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("CLI startup committed creation error = %v, want committed error", err)
	}
	if c.currentSessionID() == "" {
		t.Fatal("CLI startup did not adopt committed destination")
	}
	if !strings.HasPrefix(root, filepath.Dir(ag.Projects().Root())) {
		t.Fatal("CLI startup test did not use the real project namespace")
	}
}

func TestCLIStartupPlainCreationFailureDoesNotAdopt(t *testing.T) {
	ag, _ := newTestAgent(t)
	if _, err := ag.Projects().Ensure(); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected startup metadata sync failure")
	atomicfs.SyncFileFunc = func(*os.File) error { return injected }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })

	c := New(ag)
	c.out = new(bytes.Buffer)
	err := c.initializeSession(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("CLI startup plain creation error = %v, want injected failure", err)
	}
	var committed *snapshot.CommittedMutationError
	if errors.As(err, &committed) {
		t.Fatalf("CLI startup plain creation error = %v, want no committed classification", err)
	}
	if c.currentSessionID() != "" {
		t.Fatalf("CLI current after plain startup failure = %q, want empty", c.currentSessionID())
	}
}

// TestCLIReturnsOnlyCodeAndForkOptions proves the revert menu's action list offers
// exactly the retained operations: code rewind and fork, with no history option.
// Both positive flows keep their own coverage; this pins the removed entry point at
// the surface a user reaches it on — picking the turn renders the action list, then
// canceling leaves every message intact (the menu is fail-safe as an escape).
func TestCLIReturnsOnlyCodeAndForkOptions(t *testing.T) {
	ag, _ := newTestAgent(t)
	id, err := ag.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.AppendUserMessage("seed message"); err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	c := New(ag)
	c.out = new(bytes.Buffer)
	c.setCurrentSessionID(id)

	// Pick the seeded turn, then cancel the action menu before choosing.
	c.readKeyFn = keySequence(keyMsg{Special: keyEnter}, keyMsg{Special: keyEscape})
	if err := c.dispatchCommand("/revert"); err != nil {
		t.Fatalf("dispatchCommand(/revert): %v", err)
	}

	rendered := c.out.(*bytes.Buffer).String()
	for _, want := range []string{"Revert code", "Fork from here"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("/revert action menu = %q, must offer the retained option %q", rendered, want)
		}
	}
	if strings.Contains(rendered, "Revert history") {
		t.Fatalf("/revert action menu still offers a removed history option: %q", rendered)
	}

	msgs, err := ag.SessionMessagesFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("canceled revert menu left no messages")
	}
}

func TestCLIRealCommittedNamespaceProducers(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		ag, _ := newTestAgent(t)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		c := New(ag)
		c.out = new(bytes.Buffer)
		c.setCurrentSessionID(id)
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, id), keyMsg{Rune: 'a'})
		sessionDir := filepath.Join(ag.Projects().SessionsRoot(project.ID), id)
		var syncCalls int
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionDir {
				syncCalls++
				if syncCalls == 2 {
					return errors.New("injected CLI archive sync failure")
				}
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		dispatchSelection(t, c, "/session", "")
		wantCurrent(t, c, "")
		if !strings.Contains(c.out.(*bytes.Buffer).String(), "CLI archive sync failure") {
			t.Fatalf("CLI archive output = %q, want committed rejection", c.out.(*bytes.Buffer).String())
		}
		meta, err := snapshot.LoadSessionMeta(ag.Projects().SessionsRoot(project.ID), id)
		if err != nil {
			t.Fatal(err)
		}
		if meta.State != snapshot.StateArchived {
			t.Fatalf("CLI archive state = %q, want archived", meta.State)
		}
	})

	t.Run("delete", func(t *testing.T) {
		ag, _ := newTestAgent(t)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		c := New(ag)
		c.out = new(bytes.Buffer)
		c.setCurrentSessionID(id)
		c.readKeyFn = menuKeysWithTail(selectIndex(t, c, id), keyMsg{Rune: 'd'})
		sessionsRoot := ag.Projects().SessionsRoot(project.ID)
		injected := errors.New("injected CLI delete sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		dispatchSelection(t, c, "/session", "")
		wantCurrent(t, c, "")
		if !strings.Contains(c.out.(*bytes.Buffer).String(), "CLI delete sync failure") {
			t.Fatalf("CLI delete output = %q, want committed rejection", c.out.(*bytes.Buffer).String())
		}
		if _, err := os.Stat(filepath.Join(sessionsRoot, id)); !os.IsNotExist(err) {
			t.Fatalf("CLI deleted source = %v, want absent", err)
		}
	})

	seed := func(t *testing.T) (*CLI, string, string, int, *agent.Agent) {
		t.Helper()
		ag, _ := newTestAgent(t)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		lastTurn := 0
		for _, text := range []string{"one", "two", "three"} {
			lastTurn, err = ag.AppendUserMessageToSession(id, text)
			if err != nil {
				t.Fatal(err)
			}
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		c := New(ag)
		c.out = new(bytes.Buffer)
		c.setCurrentSessionID(id)
		return c, id, ag.Projects().SessionsRoot(project.ID), lastTurn, ag
	}

	t.Run("fork", func(t *testing.T) {
		c, sourceID, sessionsRoot, _, _ := seed(t)
		injected := errors.New("injected CLI fork sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
		c.readKeyFn = keySequence(
			keyMsg{Special: keyEnter},
			keyMsg{Special: keyDown},
			keyMsg{Special: keyEnter},
			keyMsg{Special: keyEnter},
		)

		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatal(err)
		}
		destinationID, _ := c.currentSession()
		if destinationID == "" || destinationID == sourceID {
			t.Fatalf("CLI current after committed fork = %q, want adopted destination", destinationID)
		}
		if !strings.Contains(c.out.(*bytes.Buffer).String(), "CLI fork sync failure") {
			t.Fatalf("CLI fork output = %q, want committed rejection", c.out.(*bytes.Buffer).String())
		}
		if _, err := os.Stat(filepath.Join(sessionsRoot, destinationID, "meta.json")); err != nil {
			t.Fatalf("CLI fork destination metadata: %v", err)
		}
	})

	// Positive code-rewind through the retained /revert menu on a real agent:
	// rewinding from a chosen turn restores this session's file changes via its
	// snapshots, reports every externally diverged file it must not touch, and
	// leaves routing and conversation history exactly where they were. The fork
	// subtest above is the retained sibling; TestCLIReturnsOnlyCodeAndForkOptions
	// pins the removed history option at this same menu surface.
	t.Run("revert_code_restores_files_and_keeps_history", func(t *testing.T) {
		c, id, _, _, ag := seed(t)

		createTurn, err := ag.AppendUserMessageToSession(id, "create files")
		if err != nil {
			t.Fatalf("AppendUserMessageToSession: %v", err)
		}
		root := ag.ProjectRoot()
		createdPath := filepath.Join(root, "rewound.txt")
		divergedPath := filepath.Join(root, "kept.txt")
		for _, path := range []string{createdPath, divergedPath} {
			entryID, _, err := ag.Store().SnapshotResolvedEntry(createTurn, path, path)
			if err != nil {
				t.Fatalf("snapshot entry for %s: %v", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			content := []byte("created\n")
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ag.Store().RecordSnapshotContent(createTurn, entryID, content); err != nil {
				t.Fatalf("record snapshot content for %s: %v", path, err)
			}
		}
		diverged := []byte("diverged\n")
		if err := os.WriteFile(divergedPath, diverged, 0o644); err != nil {
			t.Fatal(err)
		}

		c.readKeyFn = keySequence(
			keyMsg{Special: keyDown},  // one -> two
			keyMsg{Special: keyDown},  // two -> three
			keyMsg{Special: keyDown},  // three -> create files (last selectable turn)
			keyMsg{Special: keyEnter}, // pick the seeded create-files turn
			keyMsg{Special: keyEnter}, // choose "Revert code" (first action item; no confirm on this path)
		)

		if err := c.dispatchCommand("/revert"); err != nil {
			t.Fatalf("dispatchCommand(/revert): %v", err)
		}

		s := c.out.(*bytes.Buffer).String()
		wantSuccess := fmt.Sprintf("reverted code to before turn %d", createTurn)
		if !strings.Contains(s, wantSuccess) {
			t.Fatalf("output = %q; must render the rewind success line naming clicked turn %d: %s", s, createTurn, wantSuccess)
		}

		const skipsHeader = "kept 1 file changed outside this session:"
		wantSkipped := fmt.Sprintf("- %s (file content changed since this session last wrote it)", divergedPath)
		if i, e := strings.Index(s, skipsHeader), strings.Index(s, wantSkipped); i < 0 || e < 0 {
			t.Fatalf("output = %q; the kept-files notice must name the diverged file with its reason", s)
		}

		if _, err := os.Stat(createdPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created file still present after rewind (stat err=%v); it was created by this session and must be restored to absent", err)
		}
		got, err := os.ReadFile(divergedPath)
		if err != nil || !bytes.Equal(got, diverged) {
			t.Fatalf("diverged file after rewind = %q (err=%v); an externally changed file must stay byte-identical", got, err)
		}

		wantCurrent(t, c, id) // code rewind keeps the same session; nothing may be adopted or cleared
		msgs, err := ag.SessionMessagesFor(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := cliUserContents(msgs); !equalStringSlices(got, []string{"one", "two", "three", "create files"}) {
			t.Fatalf("history after code rewind = %q; conversation history must stay intact (system prompt + all four user turns)", got)
		}
	})
}
