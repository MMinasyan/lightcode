package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type fakeMemoryHooks struct {
	reconcileCalls int
	indexCalls     int
	deleteCalls    int
	lastSummary    string
}

func (h *fakeMemoryHooks) Reconcile() error {
	h.reconcileCalls++
	return nil
}

func (h *fakeMemoryHooks) IndexSummary(sessionID, projectID, projectName, summary, createdAt, compactionPath string) error {
	h.indexCalls++
	h.lastSummary = summary
	return nil
}

func (h *fakeMemoryHooks) DeleteSessionSummaries(sessionID string) error {
	h.deleteCalls++
	return nil
}

func TestApplyTurnActionRevertCodeUsesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected created file before revert: %v", err)
	}

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn-1 {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn-1)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after revert code; stat err=%v", err)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "create file"}) {
		t.Fatalf("history changed after code-only revert: %q", got)
	}
}

func TestApplyTurnActionRevertHistoryWithCodeUsesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	appendUserTurn(t, a, "after")

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionRevertHistory, true)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn-1 {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn-1)
	}
	if result.Prefill != "create file" {
		t.Fatalf("Prefill = %q, want selected user message", result.Prefill)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after history+code revert; stat err=%v", err)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first"}) {
		t.Fatalf("history after revert = %q, want only first turn", got)
	}
}

func TestApplyTurnActionForkIncludesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurn(t, a, "fork point")
	appendUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionFork, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn)
	}
	if result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("fork session ID = %q, before = %q", result.Session.ID, beforeID)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "fork point"}) {
		t.Fatalf("fork history = %q, want selected turn included", got)
	}
}

func TestRunCompactionPreservesPendingSignalsForMainModel(t *testing.T) {
	var sawSignal bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 2 {
			t.Fatalf("summarizer messages = %#v, want system and user", body["messages"])
		}
		userMsg, ok := messages[1].(map[string]any)
		if !ok {
			t.Fatalf("summarizer user message = %#v", messages[1])
		}
		content, _ := userMsg["content"].(string)
		sawSignal = strings.Contains(content, `<system-signal>idle signal</system-signal>`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"summary"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "first")
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.lp.AddPendingSignal(loop.PendingSignal{Payload: "idle signal", Wake: true})

	if err := a.runCompaction(context.Background(), false); err != nil {
		t.Fatalf("runCompaction returned error: %v", err)
	}
	if sawSignal {
		t.Fatal("summarizer request included pending signal; want main-model delivery only")
	}
	for _, msg := range a.lp.Messages() {
		if strings.Contains(msg.TextContent(), "idle signal") {
			t.Fatalf("pending signal was appended during compaction reload: %#v", msg)
		}
	}
	if !a.lp.HasPendingWakeSignal() {
		t.Fatal("pending wake signal was lost during compaction reload")
	}
}

func TestCompactionMemoryHookRunsOnlyAfterSuccessfulSummary(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"hook summary"},"finish_reason":"stop"}]}`)
		}))
		defer server.Close()

		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "first")
		a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
		hooks := &fakeMemoryHooks{}
		a.memoryHooks = hooks

		if err := a.runCompaction(context.Background(), false); err != nil {
			t.Fatalf("runCompaction returned error: %v", err)
		}
		if hooks.indexCalls != 1 || hooks.lastSummary != "hook summary" {
			t.Fatalf("memory hook calls=%d summary=%q, want one successful summary index", hooks.indexCalls, hooks.lastSummary)
		}
	})

	t.Run("summarizer failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "summary failed", http.StatusInternalServerError)
		}))
		defer server.Close()

		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "first")
		a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
		hooks := &fakeMemoryHooks{}
		a.memoryHooks = hooks

		if err := a.runCompaction(context.Background(), false); err == nil {
			t.Fatal("runCompaction returned nil error for failed summarizer")
		}
		if hooks.indexCalls != 0 {
			t.Fatalf("memory hook index calls=%d, want 0 on failed compaction", hooks.indexCalls)
		}
	})
}

func TestBackgroundExitSignalAfterSessionNewDoesNotDrainIntoNewSession(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "old session prompt")
	oldSessionID := a.store.SessionID()

	id, err := a.procMgr.Start("sleep 0.1; i=0; while [ $i -lt 3000 ]; do printf 'old-output-%04d\n' \"$i\"; i=$((i+1)); done", 0)
	if err != nil {
		t.Fatalf("Start background process: %v", err)
	}
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew returned error: %v", err)
	}
	appendUserTurn(t, a, "new session prompt")

	waitUntilProcessRemovedFromSession(t, a, oldSessionID, id)
	assertNoProcessOutputFiles(t, a)

	if a.lp.HasPendingSignal() {
		a.lp.DrainPendingSignalsForModel(a.store.CurrentTurn())
	}
	for _, msg := range a.lp.Messages() {
		content := msg.TextContent()
		if strings.Contains(content, "old-output") || strings.Contains(content, "Background process") {
			t.Fatalf("old session background signal leaked into new session: %#v", msg)
		}
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"new session prompt"}) {
		t.Fatalf("new session history = %q, want only new session prompt", got)
	}
}

func waitUntilProcessRemovedFromSession(t *testing.T, a *Agent, sessionID, id string) {
	t.Helper()
	a.procMgr.SetSessionProvider(func() string { return sessionID })
	defer a.procMgr.SetSessionProvider(func() string {
		return a.store.SessionID()
	})
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if !strings.Contains(a.procMgr.List(), id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %s did not finalize", id)
}

func assertNoProcessOutputFiles(t *testing.T, a *Agent) {
	t.Helper()
	spillDir := filepath.Dir(a.projects.Root())
	var leftovers []string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		leftovers = nil
		entries, err := os.ReadDir(spillDir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("ReadDir(%q): %v", spillDir, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "proc_output_") {
				leftovers = append(leftovers, entry.Name())
			}
		}
		if len(leftovers) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stale-session background exit kept proc_output files: %v", leftovers)
}

func TestIdleBackgroundTerminalSignalStartsAgentTurn(t *testing.T) {
	requests := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"handled background exit"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	events := make(chan Event, 16)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	appendUserTurn(t, a, "existing turn")
	prov := a.catalog.Providers["test"]
	prov.Transport.BaseURL = server.URL + "/v1"
	a.lp.SetClient(provider.NewAdapter(provider.New(prov, prov.Models["test-model"], "")))

	if _, err := a.procMgr.Start("printf final-output", 0); err != nil {
		t.Fatalf("Start background process: %v", err)
	}

	var sawEnd bool
	var bgEvent *Event
	for deadline := time.After(2 * time.Second); !sawEnd; {
		select {
		case ev := <-events:
			if ev.Kind == EventBackgroundProcessComplete {
				copy := ev
				bgEvent = &copy
			}
			if ev.Kind == EventTurnEnd {
				sawEnd = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for autonomous signal turn")
		}
	}

	var messages []map[string]any
	select {
	case messages = <-requests:
	case <-time.After(time.Second):
		t.Fatal("model request was not sent for idle background exit")
	}
	if len(messages) == 0 {
		t.Fatal("model request had no messages")
	}
	last := messages[len(messages)-1]
	content, _ := last["content"].(string)
	if last["role"] != "user" || !strings.Contains(content, "Background process") || !strings.Contains(content, "final-output") {
		t.Fatalf("last model message = %#v, want background terminal signal with output", last)
	}
	if bgEvent == nil {
		t.Fatal("background completion display event was not emitted")
	}
	if bgEvent.BackgroundProcess == nil {
		t.Fatalf("background event missing payload: %#v", bgEvent)
	}
	if bgEvent.BackgroundProcess.Command != "printf final-output" || bgEvent.BackgroundProcess.Reason != "completed" || bgEvent.BackgroundProcess.ExitCode != 0 || bgEvent.Result != "final-output" || bgEvent.IsError {
		t.Fatalf("background event = %#v", bgEvent)
	}
}

func TestBackgroundTerminalDisplayEventsForErrorAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"handled"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	events := make(chan Event, 32)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	appendUserTurn(t, a, "existing turn")
	prov := a.catalog.Providers["test"]
	prov.Transport.BaseURL = server.URL + "/v1"
	a.lp.SetClient(provider.NewAdapter(provider.New(prov, prov.Models["test-model"], "")))

	if _, err := a.procMgr.Start("printf failed; exit 7", 0); err != nil {
		t.Fatalf("Start error background process: %v", err)
	}
	errEvent := waitBackgroundProcessDisplayEvent(t, events, "printf failed; exit 7")
	if errEvent.BackgroundProcess.Reason != "error" || errEvent.BackgroundProcess.ExitCode != 7 || errEvent.Result != "failed" || !errEvent.IsError {
		t.Fatalf("error background event = %#v", errEvent)
	}
	waitAgentEventKind(t, events, EventTurnEnd)

	if _, err := a.procMgr.Start("printf start; sleep 5", 1); err != nil {
		t.Fatalf("Start timeout background process: %v", err)
	}
	timeoutEvent := waitBackgroundProcessDisplayEvent(t, events, "printf start; sleep 5")
	if timeoutEvent.BackgroundProcess.Reason != "timeout" || timeoutEvent.BackgroundProcess.ExitCode == 0 || !strings.Contains(timeoutEvent.Result, "start") || !timeoutEvent.IsError {
		t.Fatalf("timeout background event = %#v", timeoutEvent)
	}
	waitAgentEventKind(t, events, EventTurnEnd)
}

func waitBackgroundProcessDisplayEvent(t *testing.T, events <-chan Event, command string) Event {
	t.Helper()
	for deadline := time.After(3 * time.Second); ; {
		select {
		case ev := <-events:
			if ev.Kind == EventBackgroundProcessComplete && ev.BackgroundProcess != nil && ev.BackgroundProcess.Command == command {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for background display event for %q", command)
		}
	}
}

func waitAgentEventKind(t *testing.T, events <-chan Event, kind EventKind) Event {
	t.Helper()
	for deadline := time.After(3 * time.Second); ; {
		select {
		case ev := <-events:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event kind %v", kind)
		}
	}
}

func appendUserTurn(t *testing.T, a *Agent, content string) int {
	t.Helper()
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	a.lp.AppendUserMessage(turn, content)
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}
	return turn
}

func appendUserTurnWithSnapshot(t *testing.T, a *Agent, content, path, after string) int {
	t.Helper()
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	if err := a.store.Snapshot(turn, path); err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	a.lp.AppendUserMessage(turn, content)
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}
	return turn
}

func userContents(messages []DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
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

// Agent.RevertCode must repopulate the file tracker from disk after
// restoring snapshots, symmetric with how RevertHistory rebuilds the
// tracker. The test setup includes a read_file tool call in the message
// history so the repopulation path has something to populate from; this
// distinguishes the fix from a plain tracker.Reset() (Reset leaves
// HasRead == false; populate leaves HasRead == true with cleared
// identity).
func TestPR11Closure_RevertCodeRepopulatesTracker(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Turn 1: append a user message and an assistant message whose
	// tool_calls include a read_file for `path`. The repopulation routine
	// scans this for paths to re-track.
	turn1 := a.store.BeginTurn()
	historyMsgs := []message.Message{
		message.NewText(message.RoleUser, "read tracked.txt"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "reading"}},
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"` + path + `"}`},
			}},
		},
		toolResult("call_1", "read_file", "v1"),
	}
	for _, msg := range historyMsgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(turn1, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn1); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	// Turn 2: snapshot the v1 state, then write "v2".
	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")

	// Construct identities for both versions.
	v2Info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v2Data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v2Identity := tool.FileIdentityFromFileInfoAndData(v2Info, v2Data)

	// Simulate the agent's tracker carrying the post-modification state.
	a.fileTracker.TrackIdentity(path, 0, 500, v2Identity)
	if a.fileTracker.WasReadCheckIdentity(path, v2Identity) != nil {
		t.Fatal("setup: tracker should accept v2 identity before revert")
	}

	if err := a.RevertCode(clickedTurn - 1); err != nil {
		t.Fatalf("RevertCode error: %v", err)
	}

	// Disk should now hold v1 again.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after revert: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("on-disk content after revert = %q, want %q", string(got), "v1")
	}

	// Tracker must still record the path (rules out a plain tracker.Reset
	// implementation — that would leave HasRead == false).
	if !a.fileTracker.HasRead(path) {
		t.Fatal("tracker.HasRead = false after RevertCode; populateFileTracker did not re-populate from message history")
	}

	// The stale v2 identity must be gone (rules out the bug where RevertCode
	// leaves the tracker untouched).
	if a.fileTracker.WasReadCheckIdentity(path, v2Identity) == nil {
		t.Fatal("tracker still accepts the stale v2 identity after RevertCode; tracker was not refreshed against post-revert disk state")
	}

	// The post-revert state must still require a real read_file before the
	// next edit, even against the actual current on-disk identity. The
	// stored identity is empty after repopulation, so this check fails
	// (FileChangedError) until read_file observes the current state.
	currentInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	currentData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	currentIdentity := tool.FileIdentityFromFileInfoAndData(currentInfo, currentData)
	if a.fileTracker.WasReadCheckIdentity(path, currentIdentity) == nil {
		t.Fatal("tracker accepted the current on-disk identity without a fresh read_file; revert must force re-read before next edit")
	}
}

// TestPR11Closure_ApplyTurnActionRevertCodeRepopulatesTracker exercises the
// user-facing UI path. Wails (`App.ApplyTurnAction`) and the CLI menu both
// route revert through `ApplyTurnAction(turn, TurnActionRevertCode, ...)`,
// which is a separate branch from direct `Agent.RevertCode(...)`. Without
// the tracker repopulation in that branch a stale identity captured before
// the revert would still authorize the next `edit_file`.
func TestPR11Closure_ApplyTurnActionRevertCodeRepopulatesTracker(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	turn1 := a.store.BeginTurn()
	historyMsgs := []message.Message{
		message.NewText(message.RoleUser, "read tracked.txt"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "reading"}},
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"` + path + `"}`},
			}},
		},
		toolResult("call_1", "read_file", "v1"),
	}
	for _, msg := range historyMsgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(turn1, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn1); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")

	v2Info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v2Data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v2Identity := tool.FileIdentityFromFileInfoAndData(v2Info, v2Data)

	a.fileTracker.TrackIdentity(path, 0, 500, v2Identity)
	if a.fileTracker.WasReadCheckIdentity(path, v2Identity) != nil {
		t.Fatal("setup: tracker should accept v2 identity before revert")
	}

	// Drive the revert through ApplyTurnAction — the UI-facing path.
	if _, err := a.ApplyTurnAction(clickedTurn, TurnActionRevertCode, false); err != nil {
		t.Fatalf("ApplyTurnAction: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after revert: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("on-disk content after revert = %q, want %q", string(got), "v1")
	}

	if !a.fileTracker.HasRead(path) {
		t.Fatal("tracker.HasRead = false after ApplyTurnAction(revert_code); populateFileTracker did not re-populate from message history")
	}

	if a.fileTracker.WasReadCheckIdentity(path, v2Identity) == nil {
		t.Fatal("tracker still accepts the stale v2 identity after ApplyTurnAction(revert_code); tracker was not refreshed against post-revert disk state")
	}

	currentInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	currentData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	currentIdentity := tool.FileIdentityFromFileInfoAndData(currentInfo, currentData)
	if a.fileTracker.WasReadCheckIdentity(path, currentIdentity) == nil {
		t.Fatal("tracker accepted the current on-disk identity without a fresh read_file; revert must force re-read before next edit")
	}
}

// TestPR11Closure_RevertHistoryThenRevertCodeTrackerStaysTruncated exercises
// the sequence: RevertHistory truncates later turns, then a *separate*
// RevertCode is applied. populateFileTracker runs at both steps and must
// rebuild from the truncated store only, so a read recorded in a removed
// turn does not silently authorize a future edit.
func TestPR11Closure_RevertHistoryThenRevertCodeTrackerStaysTruncated(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	pathA := filepath.Join(a.projectRoot, "trackedA.txt")
	pathB := filepath.Join(a.projectRoot, "trackedB.txt")
	if err := os.WriteFile(pathA, []byte("a-v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("b-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Turn 1: read_file pathA in assistant tool_calls.
	turn1 := a.store.BeginTurn()
	appendReadFileTurn(t, a, turn1, "read A", pathA, "a-v1", "call_a")

	// Turn 2: snapshot pathA + write v2, so RevertCode has work to do.
	turn2 := appendUserTurnWithSnapshot(t, a, "modify A", pathA, "a-v2")

	// Turn 3: read_file pathB. This turn will be truncated by RevertHistory.
	turn3 := a.store.BeginTurn()
	appendReadFileTurn(t, a, turn3, "read B", pathB, "b-v1", "call_b")

	// Seed the in-memory tracker as if a real read_file in turn 3 had
	// authorized pathB. The revert sequence must clear this — replaying
	// only on-disk history is not enough; in-memory state for the
	// truncated turn must not silently survive.
	bInfo, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	bData, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	bIdentity := tool.FileIdentityFromFileInfoAndData(bInfo, bData)
	a.fileTracker.TrackIdentity(pathB, 0, 500, bIdentity)
	if a.fileTracker.WasReadCheckIdentity(pathB, bIdentity) != nil {
		t.Fatal("setup: tracker should accept pathB identity before revert")
	}

	// RevertHistory at turn 3 → target=2, keeps turns 1 and 2, drops turn 3.
	if _, err := a.ApplyTurnAction(turn3, TurnActionRevertHistory, false); err != nil {
		t.Fatalf("ApplyTurnAction revert_history: %v", err)
	}
	if !a.fileTracker.HasRead(pathA) {
		t.Fatal("after revert_history: tracker missing pathA (visible read in turn 1)")
	}
	if a.fileTracker.HasRead(pathB) {
		t.Fatal("after revert_history: tracker still has pathB (read was in truncated turn 3)")
	}
	if a.fileTracker.WasReadCheckIdentity(pathB, bIdentity) == nil {
		t.Fatal("after revert_history: tracker still authorizes pathB identity from truncated turn")
	}

	// RevertCode at turn 2 → target=1, restores pathA to v1.
	// populateFileTracker runs again; the truncated read of pathB must not
	// reappear (neither on disk nor in memory).
	if _, err := a.ApplyTurnAction(turn2, TurnActionRevertCode, false); err != nil {
		t.Fatalf("ApplyTurnAction revert_code: %v", err)
	}
	if got, err := os.ReadFile(pathA); err != nil || string(got) != "a-v1" {
		t.Fatalf("pathA after revert_code = %q, %v; want a-v1", got, err)
	}
	if !a.fileTracker.HasRead(pathA) {
		t.Fatal("after revert_code: tracker missing pathA (visible read in turn 1)")
	}
	if a.fileTracker.HasRead(pathB) {
		t.Fatal("after revert_code: tracker has pathB; populateFileTracker reintroduced a truncated read")
	}
	if a.fileTracker.WasReadCheckIdentity(pathB, bIdentity) == nil {
		t.Fatal("after revert_code: tracker still authorizes pathB identity from truncated turn")
	}
}

func appendReadFileTurn(t *testing.T, a *Agent, turn int, userText, path, content, callID string) {
	t.Helper()
	msgs := []message.Message{
		message.NewText(message.RoleUser, userText),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "reading"}},
			ToolCalls: []message.ToolCall{{
				ID:       callID,
				Type:     "function",
				Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"` + path + `"}`},
			}},
		},
		toolResult(callID, "read_file", content),
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}
}

func waitUntilIdle(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !a.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent did not become idle")
}
