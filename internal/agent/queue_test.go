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
	"sync"
	"testing"
	"time"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type compactionCheckpointTool struct{}

func (compactionCheckpointTool) Name() string { return "checkpoint_tool" }

func (compactionCheckpointTool) Description() string { return "checkpoint tool" }

func (compactionCheckpointTool) ParametersSchema() map[string]any {
	return map[string]any{"type": "object"}
}

func (compactionCheckpointTool) Execute(context.Context, map[string]any) (string, error) {
	return "tool result", nil
}

// waitUntilBusy waits until the agent reports busy.
func waitUntilBusy(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.Busy() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("agent did not become busy")
}

func countQueueChanged(cap *eventCapture) int {
	n := 0
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged {
			n++
		}
	}
	return n
}

func TestSubmitIdleStartsTurnNoQueueEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hi")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	res, err := a.Submit(ctx, "hello")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !res.Started || res.Turn == 0 {
		t.Fatalf("idle submit should start a turn: %#v", res)
	}
	if res.Queue == nil {
		t.Fatalf("idle direct-start queue snapshot is nil, want empty slice")
	}
	waitUntilFullyDrained(t, a)
	if n := countQueueChanged(cap); n != 0 {
		t.Fatalf("idle direct-start should emit no queue_changed; got %d", n)
	}
	if got := a.QueueSnapshot(); got.Items == nil {
		t.Fatalf("empty QueueSnapshot items is nil, want empty slice")
	}
}

func TestSubmitWhileBusyEnqueuesAndEmits(t *testing.T) {
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHangingResponse(w, srvCtx)
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	waitUntilBusy(t, a)

	res, err := a.Submit(ctx, "second")
	if err != nil {
		t.Fatalf("Submit second: %v", err)
	}
	if res.Started {
		t.Fatalf("submit while busy must enqueue, not start: %#v", res)
	}
	if len(res.Queue) != 1 || res.Queue[0].Content != "second" {
		t.Fatalf("queue snapshot = %#v, want one item 'second'", res.Queue)
	}
	if res.Version == 0 {
		t.Fatal("queue version should be > 0 after enqueue")
	}
	if countQueueChanged(cap) < 1 {
		t.Fatal("enqueue should emit an EventQueueChanged")
	}
	cancelSrv()
	_ = a.Cancel()
	waitUntilFullyDrained(t, a)
}

func TestQueueDrainEmitsEmptyArraySnapshot(t *testing.T) {
	var reqs int
	var mu sync.Mutex
	hold := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := reqs
		reqs++
		mu.Unlock()
		if n == 0 {
			<-hold
		}
		writeTextResponse(w, "ok")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "start"); err != nil {
		t.Fatalf("Submit start: %v", err)
	}
	waitUntilBusy(t, a)
	if _, err := a.Submit(ctx, "queued"); err != nil {
		t.Fatalf("Submit queued: %v", err)
	}
	close(hold)
	waitUntilFullyDrained(t, a)

	var sawEmpty bool
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && ev.QueueVersion > 0 && len(ev.Queue) == 0 {
			if ev.Queue == nil {
				t.Fatalf("empty queue_changed payload is nil, want empty slice")
			}
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Fatalf("did not observe empty queue_changed event: %#v", cap.snapshot())
	}
}

func TestQueuedInputWinsOverPendingWakeSignal(t *testing.T) {
	type requestBody struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}

	var mu sync.Mutex
	var requests []requestBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		writeTextResponse(w, "ok")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	a.lp.AddPendingSignal(loop.PendingSignal{Payload: "wake signal", Persist: true, Wake: true})
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().queue = []QueuedItem{{ID: "q-1", Content: "queued user"}}
	a.ensureRuntime().sessionLocked().queueSeq = 1
	a.ensureRuntime().sessionLocked().queueVersion = 1
	a.ensureRuntime().mu.Unlock()

	a.nudgeSignalScheduler()
	a.nudgeQueueDrainer()
	waitUntilFullyDrained(t, a)

	mu.Lock()
	gotRequests := append([]requestBody(nil), requests...)
	mu.Unlock()
	if got := len(gotRequests); got != 1 {
		t.Fatalf("model request count = %d, want 1 queued turn and no separate signal-only turn", got)
	}

	var secondUsers []string
	for _, msg := range gotRequests[0].Messages {
		if msg.Role != "user" {
			continue
		}
		if content, ok := msg.Content.(string); ok {
			secondUsers = append(secondUsers, content)
		}
	}
	if !contains(secondUsers, `<system-signal>wake signal</system-signal>`) {
		t.Fatalf("request user messages = %#v, want pending wake signal drained into queued turn", secondUsers)
	}
	if !contains(secondUsers, "queued user") {
		t.Fatalf("request user messages = %#v, want queued user input", secondUsers)
	}
	assertProjectionsMatch(t, a, cap)
}

func TestSubmitRejectsDuringTransition(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)

	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().transitioning = true
	a.ensureRuntime().mu.Unlock()

	_, err := a.Submit(context.Background(), "during switch")
	if err == nil {
		t.Fatal("Submit during a transition must return an error (no accept-then-drop)")
	}
	if got := a.QueueSnapshot(); len(got.Items) != 0 {
		t.Fatalf("rejected submit must not enqueue; queue=%#v", got.Items)
	}

	// Clearing transitioning lets submits proceed again.
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().transitioning = false
	a.ensureRuntime().mu.Unlock()
}

func TestTryDrainQueueCanceledContextDoesNotPersistQueuedDraft(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().queue = []QueuedItem{{ID: "q-1", Content: "queued draft"}}
	a.ensureRuntime().sessionLocked().queueVersion = 1
	a.ensureRuntime().mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.ensureRuntime().tryDrainQueue(ctx)

	if got := a.QueueSnapshot(); len(got.Items) != 1 || got.Items[0].Content != "queued draft" {
		t.Fatalf("canceled drain must leave volatile queue untouched; got %#v", got.Items)
	}
	if got := userContents(a.SessionMessages()); contains(got, "queued draft") {
		t.Fatalf("canceled drain persisted queued draft: user turns=%q", got)
	}
}

func TestSessionNewClearsQueueAndBumpsVersionMonotonically(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	// White-box: seed a non-empty queue at a known version.
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().queue = []QueuedItem{{ID: "q-1", Content: "stale"}}
	a.ensureRuntime().sessionLocked().queueSeq = 1
	a.ensureRuntime().sessionLocked().queueVersion = 7
	a.ensureRuntime().mu.Unlock()

	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 0 {
		t.Fatalf("SessionNew must clear the queue; got %#v", got.Items)
	}
	if got.Version <= 7 {
		t.Fatalf("queueVersion must be monotonic (bumped), got %d want > 7", got.Version)
	}
}

func TestSessionSwitchClearsQueueAndBumpsVersionMonotonically(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	appendUserTurn(t, a, "first session turn")
	firstID := a.store.SessionID()
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensure second session: %v", err)
	}
	appendUserTurn(t, a, "second session turn")
	secondID := a.store.SessionID()
	if secondID == "" || secondID == firstID {
		t.Fatalf("second session id = %q, first = %q", secondID, firstID)
	}

	seedQueue(t, a, 12, "stale for switched session")
	if err := a.SessionSwitch(firstID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}
	if got := a.SessionCurrent().ID; got != firstID {
		t.Fatalf("current session = %q, want switched session %q", got, firstID)
	}
	assertQueueClearedAfterVersion(t, a, cap, 12)
}

func TestApplyTurnActionSessionChangesClearQueueAndBumpVersionMonotonically(t *testing.T) {
	t.Run("revert_history", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		cap := &eventCapture{}
		_ = startEventOrderAgent(t, a, cap)
		appendUserTurn(t, a, "turn one")
		appendUserTurn(t, a, "turn two")

		seedQueue(t, a, 20, "stale after revert")
		res, err := a.ApplyTurnAction(2, TurnActionRevertHistory, false)
		if err != nil {
			t.Fatalf("ApplyTurnAction revert_history: %v", err)
		}
		if !res.SessionChanged {
			t.Fatalf("revert_history result = %#v, want SessionChanged", res)
		}
		assertQueueClearedAfterVersion(t, a, cap, 20)
	})

	t.Run("fork", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		cap := &eventCapture{}
		_ = startEventOrderAgent(t, a, cap)
		appendUserTurn(t, a, "turn one")
		appendUserTurn(t, a, "turn two")
		before := a.SessionCurrent().ID

		seedQueue(t, a, 30, "stale after fork")
		res, err := a.ApplyTurnAction(2, TurnActionFork, false)
		if err != nil {
			t.Fatalf("ApplyTurnAction fork: %v", err)
		}
		if !res.SessionChanged || res.Session.ID == "" || res.Session.ID == before {
			t.Fatalf("fork result = %#v, before session %q", res, before)
		}
		assertQueueClearedAfterVersion(t, a, cap, 30)
	})
}

func TestDirectRevertAndForkClearQueueAndBumpVersionMonotonically(t *testing.T) {
	t.Run("revert_history", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		cap := &eventCapture{}
		_ = startEventOrderAgent(t, a, cap)
		appendUserTurn(t, a, "turn one")
		appendUserTurn(t, a, "turn two")

		seedQueue(t, a, 40, "stale after direct revert")
		if err := a.RevertHistory(1); err != nil {
			t.Fatalf("RevertHistory: %v", err)
		}
		assertQueueClearedAfterVersion(t, a, cap, 40)
	})

	t.Run("fork", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		cap := &eventCapture{}
		_ = startEventOrderAgent(t, a, cap)
		appendUserTurn(t, a, "turn one")
		appendUserTurn(t, a, "turn two")
		before := a.SessionCurrent().ID

		seedQueue(t, a, 50, "stale after direct fork")
		if err := a.ForkSession(2); err != nil {
			t.Fatalf("ForkSession: %v", err)
		}
		if got := a.SessionCurrent().ID; got == "" || got == before {
			t.Fatalf("current session after ForkSession = %q, before %q", got, before)
		}
		assertQueueClearedAfterVersion(t, a, cap, 50)
	})
}

func TestCancelPreservesQueueThenDrains(t *testing.T) {
	var reqs int
	var mu sync.Mutex
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := reqs
		reqs++
		mu.Unlock()
		if n == 0 {
			writeHangingResponse(w, srvCtx) // first turn hangs until cancel
			return
		}
		writeTextResponse(w, "drained")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	waitUntilBusy(t, a)
	if _, err := a.Submit(ctx, "queued"); err != nil {
		t.Fatalf("Submit queued: %v", err)
	}
	if got := a.QueueSnapshot(); len(got.Items) != 1 {
		t.Fatalf("queued item should be present before cancel; got %#v", got.Items)
	}

	// Cancel must NOT clear the queue.
	cancelSrv()
	_ = a.Cancel()

	// After cancel, the backend auto-drains the queued item into a new turn.
	waitUntilFullyDrained(t, a)
	if got := userContents(a.SessionMessages()); !contains(got, "queued") {
		t.Fatalf("queued message should have drained after cancel; user turns=%q", got)
	}
}

func TestCompactNowNudgesQueueDrainer(t *testing.T) {
	summaryStarted := make(chan struct{})
	releaseSummary := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseSummary) })
	}
	t.Cleanup(release)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.Tools) == 0 {
			startedOnce.Do(func() { close(summaryStarted) })
			select {
			case <-releaseSummary:
			case <-r.Context().Done():
				return
			}
			writeTextResponse(w, "compact summary")
			return
		}
		writeTextResponse(w, "drained")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	a.memoryStore = nil
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	appendUserTurn(t, a, "before compaction")

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.CompactNow(ctx)
	}()

	select {
	case <-summaryStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("manual compaction did not start")
	}

	res, err := a.Submit(ctx, "queued during compaction")
	if err != nil {
		t.Fatalf("Submit while compacting: %v", err)
	}
	if res.Started {
		t.Fatalf("Submit while compacting should enqueue, not start: %#v", res)
	}
	if got := a.QueueSnapshot(); len(got.Items) != 1 {
		t.Fatalf("queued item should be present during compaction; got %#v", got.Items)
	}

	release()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("CompactNow returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CompactNow did not finish")
	}

	waitUntilFullyDrained(t, a)
	if got := a.QueueSnapshot(); len(got.Items) != 0 {
		t.Fatalf("queue should drain after compaction; got %#v", got.Items)
	}
	if got := userContents(a.SessionMessages()); !contains(got, "queued during compaction") {
		t.Fatalf("queued message should have drained after compaction; user turns=%q", got)
	}
}

func TestAutoCompactionEventOrderInsideTurn(t *testing.T) {
	summaryRequest := make(chan struct{})
	var summaryOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.Tools) == 0 {
			summaryOnce.Do(func() { close(summaryRequest) })
			writeTextResponse(w, "compact summary")
			return
		}
		writeTextResponse(w, "after compaction")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	a.memoryHooks = nil
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.SetEventHandler(func(Event) {})
	a.Init(ctx)

	appendUserTurn(t, a, "completed prior turn")
	a.cfg.Compaction.Enabled = true
	a.cfg.Compaction.ThresholdPct = 0.50
	a.tokensMu.Lock()
	a.lastContextUsed = 5000
	a.contextWindowSize = 8192
	a.tokensMu.Unlock()
	if !a.shouldAutoCompact() {
		t.Fatal("test setup did not cross auto-compaction threshold")
	}

	cap := &eventCapture{}
	a.SetEventHandler(cap.handler)
	if _, err := a.Submit(ctx, "turn that compacts first"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)
	select {
	case <-summaryRequest:
	default:
		t.Fatal("auto-compaction did not call summarizer")
	}

	events := cap.snapshot()
	index := func(match func(Event) bool) int {
		for i, ev := range events {
			if match(ev) {
				return i
			}
		}
		return -1
	}
	turnStart := index(func(ev Event) bool { return ev.Kind == EventTurnStart && ev.Turn == 2 })
	compactStart := index(func(ev Event) bool { return ev.Kind == EventCompactionStart })
	compactEnd := index(func(ev Event) bool { return ev.Kind == EventCompactionEnd })
	userDisplay := index(func(ev Event) bool { return ev.Kind == EventUserMessageDisplay && ev.Turn == 2 })
	textDelta := index(func(ev Event) bool { return ev.Kind == EventTextDelta })
	turnEnd := index(func(ev Event) bool { return ev.Kind == EventTurnEnd && ev.Turn == 2 })
	for name, idx := range map[string]int{
		"turn start":       turnStart,
		"compaction start": compactStart,
		"compaction end":   compactEnd,
		"user display":     userDisplay,
		"text delta":       textDelta,
		"turn end":         turnEnd,
	} {
		if idx < 0 {
			t.Fatalf("missing %s event in %#v", name, events)
		}
	}
	if !(turnStart < compactStart && compactStart < compactEnd && compactEnd < userDisplay && userDisplay < textDelta && textDelta < turnEnd) {
		t.Fatalf("unexpected auto-compaction event order: turnStart=%d compactStart=%d compactEnd=%d userDisplay=%d textDelta=%d turnEnd=%d events=%#v", turnStart, compactStart, compactEnd, userDisplay, textDelta, turnEnd, events)
	}
	if events[compactEnd].RefreshSession {
		t.Fatalf("active-turn compaction_end must not request immediate history refresh: %#v", events[compactEnd])
	}
	if !events[turnEnd].RefreshSession {
		t.Fatalf("active-turn compaction must request deferred history refresh on turn_end: %#v", events[turnEnd])
	}
}

func TestAutoCompactionBeforeFollowUpPreservesActiveToolTail(t *testing.T) {
	var (
		streamCalls       int
		summaryInput      string
		secondReqMessages []any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if tools, _ := body["tools"].([]any); len(tools) == 0 {
			messages, _ := body["messages"].([]any)
			if len(messages) > 1 {
				if msg, ok := messages[1].(map[string]any); ok {
					summaryInput, _ = msg["content"].(string)
				}
			}
			writeTextResponse(w, "summary after tool")
			return
		}
		streamCalls++
		if streamCalls == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"checkpoint_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":8000,"completion_tokens":1}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		secondReqMessages, _ = body["messages"].([]any)
		writeTextResponse(w, "done")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	a.registry.Register(compactionCheckpointTool{})
	a.memoryHooks = nil
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	appendUserTurn(t, a, "completed prior turn")
	a.cfg.Compaction.Enabled = true
	a.cfg.Compaction.ThresholdPct = 0.50

	if _, err := a.Submit(ctx, "current request"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	if streamCalls != 2 {
		t.Fatalf("stream calls = %d, want 2", streamCalls)
	}
	if !strings.Contains(summaryInput, "completed prior turn") {
		t.Fatalf("summary input did not include completed prior turn:\n%s", summaryInput)
	}
	for _, active := range []string{"current request", "checkpoint_tool", "tool result"} {
		if strings.Contains(summaryInput, active) {
			t.Fatalf("summary input included active turn %q:\n%s", active, summaryInput)
		}
	}

	userIdx := -1
	for i, raw := range secondReqMessages {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "user" && strings.Contains(fmt.Sprint(msg["content"]), "current request") {
			userIdx = i
			break
		}
	}
	if userIdx < 0 {
		t.Fatalf("follow-up request missing current user message: %#v", secondReqMessages)
	}
	if userIdx+2 >= len(secondReqMessages) {
		t.Fatalf("follow-up request active tail too short from user index %d: %#v", userIdx, secondReqMessages)
	}
	assistantMsg, _ := secondReqMessages[userIdx+1].(map[string]any)
	toolMsg, _ := secondReqMessages[userIdx+2].(map[string]any)
	if assistantMsg["role"] != "assistant" || !strings.Contains(fmt.Sprint(assistantMsg["tool_calls"]), "checkpoint_tool") {
		t.Fatalf("assistant tool-call message after current user = %#v", assistantMsg)
	}
	if toolMsg["role"] != "tool" || !strings.Contains(fmt.Sprint(toolMsg["content"]), "tool result") {
		t.Fatalf("tool result message after assistant call = %#v", toolMsg)
	}
}

func TestAutoCompactionBeforeFollowUpPreservesActiveReadTracker(t *testing.T) {
	filePath := ""
	streamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if tools, _ := body["tools"].([]any); len(tools) == 0 {
			writeTextResponse(w, "summary after read")
			return
		}
		streamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		switch streamCalls {
		case 1:
			args := fmt.Sprintf(`{"path":%q}`, filePath)
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read_file","arguments":%q}}]},"finish_reason":"tool_calls"}]}`+"\n\n", args)
			fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":8000,"completion_tokens":1}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			args := fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"goodbye"}`, filePath)
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_edit","type":"function","function":{"name":"edit_file","arguments":%q}}]},"finish_reason":"tool_calls"}]}`+"\n\n", args)
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	filePath = filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(filePath, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	rulePath := "//" + strings.TrimPrefix(filePath, "/")
	a.cfg.Permissions.Allow = []string{"read_file(" + rulePath + ")", "edit_file(" + rulePath + ")"}
	a.memoryHooks = nil
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	appendUserTurn(t, a, "completed prior turn")
	a.cfg.Compaction.Enabled = true
	a.cfg.Compaction.ThresholdPct = 0.50

	if _, err := a.Submit(ctx, "read then edit"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilEventOrderTurnEndCount(t, cap, 1)

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if got := string(data); got != "goodbye\n" {
		t.Fatalf("file content after read -> compaction -> edit = %q, want goodbye", got)
	}
}

func TestActiveTailReadRecordsKeepsOnlyNewestMatchingRead(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "tracked.txt")
	if err := os.WriteFile(filePath, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	resolved, err := pathutil.ResolveFilePath(filePath)
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	oldRead := tool.ReadRecord{Path: resolved.CanonicalPath, Offset: 1, Limit: 500, Identity: tool.FileIdentity{Mtime: time.Unix(1, 0)}}
	activeRead := tool.ReadRecord{Path: resolved.CanonicalPath, Offset: 1, Limit: 500, Identity: tool.FileIdentity{Mtime: time.Unix(2, 0)}}
	tail := []message.Message{{
		Role: message.RoleAssistant,
		ToolCalls: []message.ToolCall{{
			ID:       "call_read",
			Type:     "function",
			Function: message.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, filePath)},
		}},
	}}

	got := activeTailReadRecords(tail, []tool.ReadRecord{oldRead, activeRead}, 500, "")
	if len(got) != 1 {
		t.Fatalf("active read records len = %d, want 1: %#v", len(got), got)
	}
	if !got[0].Identity.Mtime.Equal(activeRead.Identity.Mtime) {
		t.Fatalf("restored read mtime = %v, want newest active read %v", got[0].Identity.Mtime, activeRead.Identity.Mtime)
	}
}

func TestActiveTailReadRecordsResolveRelativePathFromWorkspaceRoot(t *testing.T) {
	projectRoot := t.TempDir()
	otherCWD := t.TempDir()
	t.Chdir(otherCWD)

	filePath := filepath.Join(projectRoot, "tracked.txt")
	if err := os.WriteFile(filePath, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	resolved, err := pathutil.ResolveFilePathFrom(projectRoot, "tracked.txt")
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	activeRead := tool.ReadRecord{Path: resolved.CanonicalPath, Offset: 1, Limit: 500, Identity: tool.FileIdentity{Mtime: time.Unix(2, 0)}}
	tail := []message.Message{{
		Role: message.RoleAssistant,
		ToolCalls: []message.ToolCall{{
			ID:       "call_read",
			Type:     "function",
			Function: message.FunctionCall{Name: "read_file", Arguments: `{"path":"tracked.txt"}`},
		}},
	}}

	got := activeTailReadRecords(tail, []tool.ReadRecord{activeRead}, 500, projectRoot)
	if len(got) != 1 {
		t.Fatalf("active read records len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Path != resolved.CanonicalPath {
		t.Fatalf("restored path = %q, want %q", got[0].Path, resolved.CanonicalPath)
	}
}

func TestQueueSnapshotReturnsCopy(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().queue = []QueuedItem{{ID: "q-1", Content: "a"}}
	a.ensureRuntime().sessionLocked().queueVersion = 3
	a.ensureRuntime().mu.Unlock()

	snap := a.QueueSnapshot()
	if len(snap.Items) != 1 || snap.Version != 3 {
		t.Fatalf("snapshot = %#v", snap)
	}
	snap.Items[0].Content = "mutated"
	if a.QueueSnapshot().Items[0].Content != "a" {
		t.Fatal("QueueSnapshot must return a copy; internal state was mutated")
	}
}

func TestMultiItemDrainAllButLastAreUserOnlyTurns(t *testing.T) {
	var reqs int
	var mu sync.Mutex
	hold := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := reqs
		reqs++
		mu.Unlock()
		if n == 0 {
			<-hold // hold turn 1 open so we can enqueue two items
		}
		writeTextResponse(w, "ok")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "start"); err != nil {
		t.Fatalf("Submit start: %v", err)
	}
	waitUntilBusy(t, a)
	// Enqueue TWO items while the first turn is held open.
	for _, m := range []string{"alpha", "beta"} {
		res, err := a.Submit(ctx, m)
		if err != nil {
			t.Fatalf("Submit %s: %v", m, err)
		}
		if res.Started {
			t.Fatalf("%s should enqueue while busy: %#v", m, res)
		}
	}
	if got := a.QueueSnapshot(); len(got.Items) != 2 {
		t.Fatalf("queue should hold 2 items before drain; got %#v", got.Items)
	}
	close(hold) // release turn 1; backend auto-drains [alpha, beta]
	waitUntilFullyDrained(t, a)

	// Drain semantics: all-but-last ("alpha") becomes a user-only turn, the last
	// ("beta") starts a model turn. In loop history that means user "alpha" is
	// immediately followed by user "beta" (no assistant in between), and an
	// assistant reply follows "beta".
	msgs := a.lp.Messages()
	ai, bi := -1, -1
	for i, m := range msgs {
		if m.Role == message.RoleUser && m.TextContent() == "alpha" {
			ai = i
		}
		if m.Role == message.RoleUser && m.TextContent() == "beta" {
			bi = i
		}
	}
	if ai < 0 || bi < 0 {
		t.Fatalf("both queued messages must be in history; alpha=%d beta=%d msgs=%#v", ai, bi, msgs)
	}
	if bi != ai+1 {
		t.Fatalf("alpha must be a user-only turn immediately before beta (no assistant between); alpha=%d beta=%d", ai, bi)
	}
	if bi+1 >= len(msgs) || msgs[bi+1].Role != message.RoleAssistant {
		t.Fatalf("beta must start a model turn (assistant reply follows); msgs=%#v", msgs)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func seedQueue(t *testing.T, a *Agent, version int, content string) {
	t.Helper()
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().queue = []QueuedItem{{ID: "q-1", Content: content}}
	a.ensureRuntime().sessionLocked().queueSeq = 1
	a.ensureRuntime().sessionLocked().queueVersion = version
	a.ensureRuntime().mu.Unlock()
}

func assertQueueClearedAfterVersion(t *testing.T, a *Agent, cap *eventCapture, previousVersion int) {
	t.Helper()
	got := a.QueueSnapshot()
	if len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("queue snapshot = %#v, want empty non-nil slice", got.Items)
	}
	if got.Version <= previousVersion {
		t.Fatalf("queue version = %d, want > %d", got.Version, previousVersion)
	}
	var sawEvent bool
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && ev.QueueVersion == got.Version {
			if len(ev.Queue) != 0 || ev.Queue == nil {
				t.Fatalf("queue_changed payload = %#v, want empty non-nil slice", ev.Queue)
			}
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Fatalf("missing queue_changed event for version %d: %#v", got.Version, cap.snapshot())
	}
}
