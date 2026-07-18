package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
)

// TestWailsHydrateSessionUnavailableWithoutAgent verifies the concrete-only
// hydration surface fails cleanly when no concrete agent is owned.
func TestWailsHydrateSessionUnavailableWithoutAgent(t *testing.T) {
	app := &App{}
	if _, err := app.HydrateSession("x"); err == nil {
		t.Fatal("HydrateSession without a concrete agent = nil error, want unavailable")
	}
}

// TestWailsAppOwnsConcreteAgentLifecycle verifies startup initializes and marks the
// owner, hydration reaches the concrete agent, and shutdown joins the owner
// without hanging.
func TestWailsAppOwnsConcreteAgentLifecycle(t *testing.T) {
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	app.emitFn = func(string, any) {}

	app.startup(context.Background())
	if !app.started {
		t.Fatal("startup did not mark the owner started")
	}
	id := app.currentSessionID()
	if id == "" {
		t.Fatal("startup did not establish a current session")
	}
	hs, err := app.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession: %v", err)
	}
	if hs.Session.ID != id {
		t.Fatalf("hydrated session = %q, want %q", hs.Session.ID, id)
	}

	done := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("app.shutdown hung joining the owner")
	}
}

// TestWailsShutdownBeforeStartupIsSafe verifies a close that wins the race against
// an asynchronous startup neither joins an uninitialized owner nor lets startup
// initialize one afterwards.
func TestWailsShutdownBeforeStartupIsSafe(t *testing.T) {
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	app.emitFn = func(string, any) {}
	app.startDelivery()

	app.shutdown(context.Background())
	if app.started {
		t.Fatal("shutdown before startup must not mark started")
	}

	app.startup(context.Background())
	if app.started {
		t.Fatal("startup after shutdown must short-circuit without initializing an owner")
	}
}

// TestWailsShutdownJoinsInFlightTurn verifies shutdown joins a turn that is still
// running when the window closes. The turn's end-of-turn flush needs the owner's
// internal event drainer alive, so shutdown must join the owner before cancelling
// the host context (whose cancellation would otherwise stop that drainer first and
// strand the flush).
func TestWailsShutdownJoinsInFlightTurn(t *testing.T) {
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-srvCtx.Done() // hold the model call open until the turn is cancelled
	}))
	// Release the hanging handler before closing the server: Close waits for
	// outstanding handlers, and the handler only returns once srvCtx is cancelled.
	defer func() {
		cancelSrv()
		server.Close()
	}()

	ag := newAppTestAgentAt(t, server.URL+"/v1")
	app := &App{svc: ag, agent: ag}
	app.emitFn = func(string, any) {}
	app.startup(context.Background())

	id := app.currentSessionID()
	if id == "" {
		t.Fatal("startup did not establish a current session")
	}
	if _, err := ag.SubmitToSession(context.Background(), id, "hi"); err != nil {
		t.Fatalf("SubmitToSession: %v", err)
	}
	waitUntilBusyApp(t, ag)

	done := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("app.shutdown did not join the in-flight turn: the internal drainer was stopped before the turn flushed")
	}
}

// waitUntilBusyApp blocks until the owner reports an in-flight turn.
func waitUntilBusyApp(t *testing.T, ag *agent.Agent) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ag.Busy() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("turn did not become in-flight")
}

// TestWailsDeliveryFIFOEmitsFramesInOrder verifies every frame reaches the emit
// choke point through the single drainer, in append order.
func TestWailsDeliveryFIFOEmitsFramesInOrder(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var got []string
	a.emitFn = func(name string, _ any) {
		mu.Lock()
		got = append(got, name)
		mu.Unlock()
	}
	a.startDelivery()
	defer a.closeDelivery()

	const n = 100
	for i := 0; i < n; i++ {
		a.emitFrame(fmt.Sprintf("e%d", i), nil)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		c := len(got)
		mu.Unlock()
		if c == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drained %d of %d frames", c, n)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("e%d", i); got[i] != want {
			t.Fatalf("frame %d = %q, want %q (out of order)", i, got[i], want)
		}
	}
}

// TestWailsDeliveryAppliesTitleAfterFrame proves the drainer applies a frame's
// window title in the same ordered step, right after emitting the frame and before
// the next, so a stalled boundary withholds its title too.
func TestWailsDeliveryAppliesTitleAfterFrame(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var order []string
	a.emitFn = func(name string, _ any) { mu.Lock(); order = append(order, "emit:"+name); mu.Unlock() }
	a.titleFn = func(title string) { mu.Lock(); order = append(order, "title:"+title); mu.Unlock() }
	a.startDelivery()
	defer a.closeDelivery()

	a.emitFrameTitled("navigation", nil, "Project B")
	a.emitFrame("token", nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drained %d of 3 items: %v", n, order)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"emit:navigation", "title:Project B", "emit:token"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v", order, want)
		}
	}
}

// TestWailsDeliveryDropsTitleAfterClose proves a boundary's window title is not
// applied after close: if the boundary's emit blocks and close abandons the
// drainer, the title must not change once shutdown has proceeded.
func TestWailsDeliveryDropsTitleAfterClose(t *testing.T) {
	old := deliveryJoinTimeout
	deliveryJoinTimeout = 50 * time.Millisecond
	defer func() { deliveryJoinTimeout = old }()

	a := &App{ctx: context.Background()}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var titles []string
	a.emitFn = func(string, any) { close(entered); <-release } // block the boundary's emit
	a.titleFn = func(title string) { mu.Lock(); titles = append(titles, title); mu.Unlock() }
	a.startDelivery()
	a.emitFrameTitled("navigation", nil, "Project B")
	<-entered // the drainer is blocked inside the boundary's emit

	done := make(chan struct{})
	go func() { a.closeDelivery(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeDelivery did not return")
	}

	close(release)                     // unblock the emit; the drainer reaches the title step
	time.Sleep(100 * time.Millisecond) // give it a chance to (wrongly) apply the title

	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 0 {
		t.Fatalf("title applied after close: %v", titles)
	}
}

// TestWailsDeliveryCloseJoinsAndRejects verifies close drains and joins the
// drainer, then rejects further frames.
func TestWailsDeliveryCloseJoinsAndRejects(t *testing.T) {
	a := &App{ctx: context.Background()}
	a.emitFn = func(string, any) {}
	a.startDelivery()
	a.emitFrame("x", nil)
	a.closeDelivery()

	select {
	case <-a.deliveryDone:
	default:
		t.Fatal("drainer did not exit after close")
	}

	// The drainer has exited, so the queue is stable. A frame appended after
	// close is rejected: it does not change the queue length.
	a.deliveryMu.Lock()
	before := len(a.deliveryFrames)
	a.deliveryMu.Unlock()
	a.emitFrame("after", nil)
	a.deliveryMu.Lock()
	after := len(a.deliveryFrames)
	a.deliveryMu.Unlock()
	if after != before {
		t.Fatalf("post-close emitFrame not rejected: queue length %d -> %d", before, after)
	}
}

// TestWailsDeliveryCloseAbandonsBlockedEmit verifies close returns even when the
// drainer is blocked inside one framework emit, and that a frame queued behind
// the blocked one is not emitted after shutdown has proceeded.
func TestWailsDeliveryCloseAbandonsBlockedEmit(t *testing.T) {
	old := deliveryJoinTimeout
	deliveryJoinTimeout = 50 * time.Millisecond
	defer func() { deliveryJoinTimeout = old }()

	a := &App{ctx: context.Background()}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var emitted []string
	a.emitFn = func(name string, _ any) {
		mu.Lock()
		first := len(emitted) == 0
		emitted = append(emitted, name)
		mu.Unlock()
		if first {
			close(entered)
			<-release // block only the first emit
		}
	}
	a.startDelivery()
	a.emitFrame("blocked", nil)
	a.emitFrame("queued_behind", nil)
	<-entered // the drainer is now blocked inside the first emit

	done := make(chan struct{})
	go func() {
		a.closeDelivery()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeDelivery did not return; blocked drainer not abandoned")
	}

	close(release)                     // unblock the drainer
	time.Sleep(100 * time.Millisecond) // give it a chance to (wrongly) emit the queued frame

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 1 || emitted[0] != "blocked" {
		t.Fatalf("emitted = %v, want only [blocked]: no emission after close", emitted)
	}
}
