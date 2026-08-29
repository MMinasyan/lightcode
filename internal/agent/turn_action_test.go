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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/compact"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func TestApplyTurnActionRevertCodeUsesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected created file before revert: %v", err)
	}

	result, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, clickedTurn, TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession returned error: %v", err)
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

// TestApplyTurnActionRejectedActionsPerformNoMutation proves a turn action the
// owner does not know performs no queue, transcript, compaction, or durable
// mutation: history reversion has no retained route under its old name, and an
// arbitrary forged string is refused at the same point. The rejection returns
// before any reservation, reload, or store call, so a seeded turn with a live
// snapshot file stays byte-identical on disk and in memory.
func TestApplyTurnActionRejectedActionsPerformNoMutation(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected created file before the forged actions: %v", err)
	}
	sessionID := a.SessionCurrent().ID
	seedUnitQueue(t, a, sessionID, "queued draft")
	queueBefore, err := a.QueueSnapshotForSession(sessionID)
	if err != nil {
		t.Fatalf("QueueSnapshotForSession before forged actions: %v", err)
	}
	events := &eventCapture{}
	a.SetEventHandler(events.handler)

	for _, action := range []string{"revert_history", "bogus_action"} {
		result, err := a.ApplyTurnActionForSession(sessionID, clickedTurn, action, true)
		if err == nil || !strings.Contains(err.Error(), `unknown turn action`) {
			t.Fatalf("ApplyTurnActionForSession(%q) = %#v %v, want an unknown-action error with no mutation", action, result, err)
		}
		if result.SessionChanged {
			t.Fatalf("%s forged action reported a session change: %#v", action, result)
		}
		queueAfter, err := a.QueueSnapshotForSession(sessionID)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession after %q: %v", action, err)
		}
		if !reflect.DeepEqual(queueAfter, queueBefore) {
			t.Fatalf("queue after %q = %#v, want unchanged %#v", action, queueAfter, queueBefore)
		}
		if got := countQueueChanged(events); got != 0 {
			t.Fatalf("%q entered the transition reservation and emitted %d queue_changed event(s)", action, got)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("created file missing after rejected actions: %v, want it untouched", err)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "create file"}) {
		t.Fatalf("history = %q after rejected actions, want both turns intact", got)
	}
}

func TestApplyTurnActionForkIncludesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurn(t, a, "fork point")
	appendUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	result, err := a.ApplyTurnActionForSession(beforeID, clickedTurn, TurnActionFork, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession returned error: %v", err)
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
		systemMsg, ok := messages[0].(map[string]any)
		if !ok {
			t.Fatalf("summarizer system message = %#v", messages[0])
		}
		if got, _ := systemMsg["content"].(string); got != compact.DefaultSummarizerPrompt {
			t.Fatalf("summarizer system prompt = %q, want compact prompt body", got)
		}
		userMsg, ok := messages[1].(map[string]any)
		if !ok {
			t.Fatalf("summarizer user message = %#v", messages[1])
		}
		content, _ := userMsg["content"].(string)
		sawSignal = strings.Contains(content, `<system-signal>idle signal</system-signal>`)
		writeTextResponse(w, "summary")
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

func TestCompactionWritesCompactTranscript(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeTextResponse(w, fmt.Sprintf("summary-%d", calls))
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, strings.Repeat("alpha ", 1600))
	appendUserTurn(t, a, strings.Repeat("beta ", 1600))
	sessionID := a.store.SessionID()
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"

	if err := a.runCompaction(context.Background(), false); err != nil {
		t.Fatalf("runCompaction returned error: %v", err)
	}
	if calls < 2 {
		t.Fatalf("summarizer calls = %d, want iterative compaction to make at least 2 calls", calls)
	}

	infos, err := snapshot.List(a.store.Root(), "", snapshot.StateActive)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var childID string
	for _, info := range infos {
		if info.ParentSessionID == sessionID {
			if childID != "" {
				t.Fatalf("multiple compact child sessions found: %q and %q", childID, info.ID)
			}
			childID = info.ID
		}
	}
	if childID == "" {
		t.Fatal("compact child session was not created")
	}
	if childID == sessionID {
		t.Fatalf("compact child session reused conversation id %q", sessionID)
	}
	if _, err := os.Stat(filepath.Join(a.store.Root(), childID, "compaction.json")); !os.IsNotExist(err) {
		t.Fatalf("compact child compaction.json stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(a.store.Dir(), "compaction.json")); err != nil {
		t.Fatalf("conversation compaction.json stat: %v", err)
	}
	meta, err := snapshot.LoadSessionMeta(a.store.Root(), childID)
	if err != nil {
		t.Fatalf("load compact child meta: %v", err)
	}
	if meta.ActiveAgentType != "compact" {
		t.Fatalf("compact child active agent type = %q, want compact", meta.ActiveAgentType)
	}
	if meta.Provider != "test" || meta.Model != "test-model" {
		t.Fatalf("compact child model metadata = %s/%s, want test/test-model", meta.Provider, meta.Model)
	}
	sessions, err := a.SessionList(snapshot.StateActive)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	for _, session := range sessions {
		if session.ID == childID {
			t.Fatalf("compact child session leaked into SessionList: %#v", sessions)
		}
	}
	projectSessions, err := a.SessionListForProjectPath(a.projectRoot, snapshot.StateActive)
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	for _, session := range projectSessions {
		if session.ID == childID {
			t.Fatalf("compact child session leaked into project SessionList: %#v", projectSessions)
		}
	}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "open", run: func() error { _, err := a.OpenSession(childID); return err }},
		{name: "switch", run: func() error { return a.SessionSwitch(childID) }},
		{name: "archive", run: func() error { return a.SessionArchive(childID) }},
		{name: "delete", run: func() error { return a.SessionDelete(childID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "internal transcript session") {
				t.Fatalf("%s compact child error = %v, want internal transcript rejection", tc.name, err)
			}
		})
	}
	if got := a.SessionCurrent().ID; got != sessionID {
		t.Fatalf("current session after rejected compact management = %q, want %q", got, sessionID)
	}

	childStore, err := snapshot.NewForSessionsRoot(a.store.Root(), "", "")
	if err != nil {
		t.Fatalf("new child store: %v", err)
	}
	if err := childStore.LoadSession(childID); err != nil {
		t.Fatalf("load compact child session: %v", err)
	}
	turns, err := childStore.LoadCompleteTurnsReadOnly()
	if err != nil {
		t.Fatalf("load compact child turns: %v", err)
	}
	if len(turns) != calls {
		t.Fatalf("compact transcript turns = %d, want %d", len(turns), calls)
	}
	for i, turn := range turns {
		if len(turn.Messages) != 2 {
			t.Fatalf("compact turn %d messages = %d, want user and assistant", turn.Turn, len(turn.Messages))
		}
		var userMsg, assistantMsg message.Message
		if err := json.Unmarshal(turn.Messages[0], &userMsg); err != nil {
			t.Fatalf("decode compact user turn %d: %v", turn.Turn, err)
		}
		if err := json.Unmarshal(turn.Messages[1], &assistantMsg); err != nil {
			t.Fatalf("decode compact assistant turn %d: %v", turn.Turn, err)
		}
		if userMsg.Role != message.RoleUser {
			t.Fatalf("compact turn %d first role = %q, want user", turn.Turn, userMsg.Role)
		}
		if assistantMsg.Role != message.RoleAssistant || assistantMsg.TextContent() != fmt.Sprintf("summary-%d", i+1) {
			t.Fatalf("compact turn %d assistant = role:%q text:%q", turn.Turn, assistantMsg.Role, assistantMsg.TextContent())
		}
	}
	displayMessages, err := a.SessionMessagesFor(childID)
	if err != nil {
		t.Fatalf("SessionMessagesFor compact child: %v", err)
	}
	if len(displayMessages) == 0 {
		t.Fatal("SessionMessagesFor compact child returned no transcript rows")
	}
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

// TestSignalTurnRefusalBalancesClaim proves the signal scheduler's turn start
// is the same commitment point as direct submit: a claimed start refused at
// the receiving end is suppressed — no retry of the same wake, no event, no
// model request — and its claim's Add is balanced exactly; a live owner
// starts a nonzero signal turn whose claim is released when the turn ends.
func TestSignalTurnRefusalBalancesClaim(t *testing.T) {
	t.Run("refused=claimed_start_suppressed_add_balanced", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()
		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		a.lp.AddPendingSignal(loop.PendingSignal{Payload: "wake", Persist: true, Wake: true})
		rt := a.ensureRuntime()
		// Cancel the owner context without publishing closed: the scheduler's
		// claim still succeeds (its turn context derives from the cancelled
		// owner context), so the receiving end refuses the claimed start —
		// a claimed signal turn with its Add, refused at launch.
		rt.ownerCancel()
		done := make(chan struct{})
		go func() {
			rt.tryStartSignalTurn(ctx)
			close(done)
		}()
		// The refused start is suppressed: the pass returns instead of
		// immediately retrying the same wakeable unit.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refused signal start was immediately retried while the same wake remains")
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("refused signal start emitted a turn: %#v", ev)
			}
		}
		if got := reqs.Load(); got != 0 {
			t.Fatalf("refused signal start made %d model requests", got)
		}
		// The claim's Add was released exactly once by the receiving-end
		// refusal.
		wgDone := make(chan struct{})
		go func() {
			rt.turnWG.Wait()
			close(wgDone)
		}()
		select {
		case <-wgDone:
		case <-time.After(2 * time.Second):
			t.Fatal("refused signal start leaked a wait-group count")
		}
	})

	t.Run("live=nonzero_signal_turn", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "ok")
		}))
		defer server.Close()
		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		a.lp.AddPendingSignal(loop.PendingSignal{Payload: "wake", Persist: true, Wake: true})
		rt := a.ensureRuntime()
		rt.tryStartSignalTurn(ctx)
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		var started Event
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart && ev.Turn > 0 {
				started = ev
			}
		}
		if started.Turn == 0 {
			t.Fatalf("signal turn start missing or zero in %#v", cap.snapshot())
		}
		wgDone := make(chan struct{})
		go func() {
			rt.turnWG.Wait()
			close(wgDone)
		}()
		select {
		case <-wgDone:
		case <-time.After(5 * time.Second):
			t.Fatal("signal turn never released its wait-group count")
		}
	})
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
	// ensureSession no longer creates a session; bootstrap through the real
	// creation entry when the agent has none yet (fresh agent or after
	// SessionNew).
	if !a.store.Active() {
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	}
	// ensureSession registers the live session in a.sessions; production callers
	// hold the runtime mutex around it, so this helper must too.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	err := a.ensureSession()
	rt.mu.Unlock()
	if err != nil {
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
	if !a.store.Active() {
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	}
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	entryID, _, err := a.store.SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	if err := a.store.RecordSnapshotContent(turn, entryID, []byte(after)); err != nil {
		t.Fatalf("RecordSnapshotContent returned error: %v", err)
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

func TestRevertCodeClearsFileTrackerState(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	a.fileTracker.TrackIdentity(path, 0, 500, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, path) {
		t.Fatal("setup: tracker missing read state")
	}

	result, err := a.RevertCode(clickedTurn)
	if err != nil {
		t.Fatalf("RevertCode error: %v", err)
	}
	if len(result.Restored) != 1 || result.Restored[0] != path {
		t.Fatalf("RevertCode result = %+v, want restored %s", result, path)
	}

	// Disk should now hold v1 again.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after revert: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("on-disk content after revert = %q, want %q", string(got), "v1")
	}
	if agentTrackerHasRead(a, path) {
		t.Fatal("tracker retained read state after RevertCode")
	}
}

// TestDirectRevertCodeUsesFirstRestoredTurn pins the direct code-revert
// convention: RevertCode(N) and RevertCodeForSession(N) take N as the first
// restored turn, matching the shared revert_code turn action — the store keeps
// turns up to N-1, so a file created in turn N is removed.
func TestDirectRevertCodeUsesFirstRestoredTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	path := filepath.Join(a.projectRoot, "created.txt")
	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	id := a.SessionCurrent().ID
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected created file before revert: %v", err)
	}

	if _, err := a.RevertCodeForSession(id, clickedTurn); err != nil {
		t.Fatalf("RevertCodeForSession: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created in the first restored turn still exists; stat err=%v", err)
	}
}

func TestRevertCodeReturnsSkippedFiles(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := a.RevertCode(clickedTurn)
	if err != nil {
		t.Fatalf("RevertCode: %v", err)
	}
	if len(result.Restored) != 0 {
		t.Fatalf("Restored = %v, want none", result.Restored)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Path != path {
		t.Fatalf("Skipped = %+v, want skipped %s", result.Skipped, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "external" {
		t.Fatalf("path after skipped revert = %q, %v; want external", got, err)
	}
}

func TestApplyTurnActionRevertCodeReportsRestoredFiles(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")

	result, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, clickedTurn, TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession: %v", err)
	}
	if len(result.RestoredFiles) != 1 || result.RestoredFiles[0] != path {
		t.Fatalf("RestoredFiles = %v, want [%s]", result.RestoredFiles, path)
	}
	if len(result.SkippedFiles) != 0 {
		t.Fatalf("SkippedFiles = %+v, want none", result.SkippedFiles)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after revert: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("on-disk content after revert = %q, want %q", string(got), "v1")
	}
}

func TestApplyTurnActionRevertCodeReportsSkippedFiles(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	clickedTurn := appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, clickedTurn, TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession: %v", err)
	}
	if len(result.RestoredFiles) != 0 {
		t.Fatalf("RestoredFiles = %v, want none", result.RestoredFiles)
	}
	if len(result.SkippedFiles) != 1 || result.SkippedFiles[0].Path != path {
		t.Fatalf("SkippedFiles = %+v, want skipped %s", result.SkippedFiles, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "external" {
		t.Fatalf("path after skipped revert = %q, %v; want external", got, err)
	}
}

func agentTrackerHasRead(a *Agent, path string) bool {
	for _, record := range a.fileTracker.Snapshot() {
		if record.Path == path {
			return true
		}
	}
	return false
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

// TestCodeRevertPartialErrorKeepsBareResult proves a partial code-only revert
// returns the exact restored/skipped outcome with the error and no hydrated
// replacement payload: a code-only revert emits no boundary, and the adapters
// discard the result (Wails, ACP) or consume only the skips (CLI). The
// fixture makes the higher turn restore successfully and the lower turn fail:
// the lower turn's created-file snapshot is mutated to the legacy
// Existed:false shape (its canonical witness — the canonical_path field —
// removed), so its delete refuses with "no canonical proof".
func TestCodeRevertPartialErrorKeepsBareResult(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// Turn 1 (lower): a created file whose snapshot is mutated to the legacy
	// Existed:false shape.
	legacyPath := filepath.Join(a.ProjectRoot(), "legacy.txt")
	appendUserTurnWithSnapshot(t, a, "create legacy", legacyPath, "legacy\n")
	entryDir := filepath.Join(a.store.Dir(), "snapshots", "1")
	entries, err := os.ReadDir(entryDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("turn 1 snapshot entries = %v, %v; want exactly one", entries, err)
	}
	legacyEntry := filepath.Join(entryDir, entries[0].Name())
	var meta map[string]any
	data, err := os.ReadFile(filepath.Join(legacyEntry, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	delete(meta, "canonical_path")
	mutated, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyEntry, "meta.json"), mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	// Turn 2 (higher): a valid created-file snapshot.
	laterPath := filepath.Join(a.ProjectRoot(), "later.txt")
	appendUserTurnWithSnapshot(t, a, "create later", laterPath, "later\n")

	result, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, 1, TurnActionRevertCode, false)
	if err == nil {
		t.Fatal("partial code revert reported success")
	}
	if !strings.Contains(err.Error(), "canonical proof") {
		t.Fatalf("revert error = %q, want the legacy delete refusal naming the missing canonical proof", err.Error())
	}
	// The higher turn restored exactly; the lower turn produced no skip (its
	// failure is an error, not a skip).
	if len(result.RestoredFiles) != 1 || result.RestoredFiles[0] != laterPath {
		t.Fatalf("RestoredFiles = %v, want exactly [%s]", result.RestoredFiles, laterPath)
	}
	if len(result.SkippedFiles) != 0 {
		t.Fatalf("SkippedFiles = %+v, want none", result.SkippedFiles)
	}
	// The result is bare: no hydrated Session/Messages/Tokens replacement.
	if result.Session.ID != "" || len(result.Messages) != 0 || len(result.Tokens.PerModel) != 0 || result.Tokens.Total.Input != 0 || result.Tokens.Total.Output != 0 {
		t.Fatalf("partial code revert result carries a hydrated replacement: session=%q messages=%d tokens=%+v", result.Session.ID, len(result.Messages), result.Tokens)
	}
	// Durable outcome: the higher turn's file is removed, the lower turn's
	// failed restore leaves its file in place.
	if _, err := os.Stat(laterPath); !os.IsNotExist(err) {
		t.Fatalf("later file still exists after the successful higher-turn restore: %v", err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file removed despite the failed lower-turn restore: %v", err)
	}
}
