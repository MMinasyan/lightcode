package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/message"
)

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
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: "queued user"}}
	a.queueSeq = 1
	a.queueVersion = 1
	a.mu.Unlock()

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

	a.mu.Lock()
	a.transitioning = true
	a.mu.Unlock()

	_, err := a.Submit(context.Background(), "during switch")
	if err == nil {
		t.Fatal("Submit during a transition must return an error (no accept-then-drop)")
	}
	if got := a.QueueSnapshot(); len(got.Items) != 0 {
		t.Fatalf("rejected submit must not enqueue; queue=%#v", got.Items)
	}

	// Clearing transitioning lets submits proceed again.
	a.mu.Lock()
	a.transitioning = false
	a.mu.Unlock()
}

func TestTryDrainQueueCanceledContextDoesNotPersistQueuedDraft(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: "queued draft"}}
	a.queueVersion = 1
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.tryDrainQueue(ctx)

	if got := a.QueueSnapshot(); len(got.Items) != 1 || got.Items[0].Content != "queued draft" {
		t.Fatalf("canceled drain must leave volatile queue untouched; got %#v", got.Items)
	}
	if got := userContents(a.SessionMessages()); contains(got, "queued draft") {
		t.Fatalf("canceled drain persisted queued draft: user turns=%q", got)
	}
}

func TestCloseForProjectSwitchClearsQueueUnderTransition(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: "stale"}}
	a.queueSeq = 1
	a.queueVersion = 4
	a.mu.Unlock()

	if err := a.CloseForProjectSwitch(); err != nil {
		t.Fatalf("CloseForProjectSwitch: %v", err)
	}
	if a.store.Active() {
		t.Fatal("project switch close should detach the active session")
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("queue after project switch = %#v, want empty non-nil slice", got.Items)
	}
	if got.Version <= 4 {
		t.Fatalf("queue version = %d, want > 4", got.Version)
	}
	var sawEmpty bool
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && ev.QueueVersion == got.Version {
			if len(ev.Queue) != 0 || ev.Queue == nil {
				t.Fatalf("project switch event queue = %#v, want empty non-nil slice", ev.Queue)
			}
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Fatalf("missing project switch queue_changed event: %#v", cap.snapshot())
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
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: "stale"}}
	a.queueSeq = 1
	a.queueVersion = 7
	a.mu.Unlock()

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
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !body.Stream {
			startedOnce.Do(func() { close(summaryStarted) })
			select {
			case <-releaseSummary:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"compact summary"},"finish_reason":"stop"}]}`))
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
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !body.Stream {
			summaryOnce.Do(func() { close(summaryRequest) })
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"compact summary"},"finish_reason":"stop"}]}`))
			return
		}
		writeTextResponse(w, "after compaction")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	a.memoryStore = nil
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
}

func TestQueueSnapshotReturnsCopy(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: "a"}}
	a.queueVersion = 3
	a.mu.Unlock()

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
	a.mu.Lock()
	a.queue = []QueuedItem{{ID: "q-1", Content: content}}
	a.queueSeq = 1
	a.queueVersion = version
	a.mu.Unlock()
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
