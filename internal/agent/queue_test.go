package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
	waitUntilFullyDrained(t, a)
	if n := countQueueChanged(cap); n != 0 {
		t.Fatalf("idle direct-start should emit no queue_changed; got %d", n)
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
