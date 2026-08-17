package main

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestWailsErrorFrameOmitsSequenceForSessionlessError verifies the error frame
// built for an error event carries a seq field only when the event actually has
// a sequence. A sessionless error is emitted directly and never sequenced
// (Seq 0); its frame must omit the field, because the frontend gate reads the
// field's absence as "unsequenced" and admits it, while a zero-stamped seq is
// rejected against every snapshot high-water and the error never renders.
func TestWailsErrorFrameOmitsSequenceForSessionlessError(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var payloads []any
	a.emitFn = func(name string, payload any) {
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
	}
	a.startDelivery()
	defer a.closeDelivery()
	a.seedPresented("s")

	// A sessionless error: no transcript, no sequence, and no session tag
	// (an empty session id is a global frame, always delivered).
	a.handleEvent(agent.Event{Kind: agent.EventError, Error: "sessionless"})
	// A session error is sequenced by the transcript coordinator.
	a.handleEvent(agent.Event{Kind: agent.EventError, SessionID: "s", Seq: 7, Error: "sequenced"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(payloads)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drained %d of 2 error frames", n)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	sessionless, ok1 := payloads[0].(map[string]any)
	sequenced, ok2 := payloads[1].(map[string]any)
	if !ok1 || !ok2 {
		t.Fatalf("error frame payloads are %T and %T, want map[string]any", payloads[0], payloads[1])
	}
	if _, present := sessionless["seq"]; present {
		t.Fatalf("sessionless error frame carries seq %#v; the field must be omitted so the frontend gate admits it", sessionless["seq"])
	}
	if sessionless["message"] != "sessionless" {
		t.Fatalf("sessionless error frame message = %#v, want %q", sessionless["message"], "sessionless")
	}
	if sequenced["seq"] != 7 {
		t.Fatalf("sequenced error frame seq = %#v, want 7", sequenced["seq"])
	}
	if sequenced["message"] != "sequenced" {
		t.Fatalf("sequenced error frame message = %#v, want %q", sequenced["message"], "sequenced")
	}
}

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

// TestWailsShutdownDiscardsOwnerResult pins the shutdown hook's explicit
// discard of ShutdownOwner's result: the Wails framework hook is void and has
// no return channel, so nothing can be folded into; the stderr diagnostic
// inside ShutdownOwner is this host's only available signal, and the hook must
// discard the result explicitly rather than silently. Exception, recorded per
// the contract-test rule: making the owner's shutdown actually abandoned needs
// the agent-internal coordinator park that TestOwnerShutdownContractMatrix
// drives (join=timeout), which this package cannot reach, so the discard is
// pinned structurally against that behavioral evidence and the clean-path
// hook test (TestWailsAppOwnsConcreteAgentLifecycle), which drives shutdown
// through the hook to completion.
func TestWailsShutdownDiscardsOwnerResult(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	body, ok := extractAppFunctionBody(string(src), "func (a *App) shutdown(")
	if !ok {
		t.Fatal("shutdown not found")
	}
	if !strings.Contains(body, "_ = a.agent.ShutdownOwner()") {
		t.Fatal("the shutdown hook must discard ShutdownOwner's result explicitly (_ =), since the void framework hook has no return channel")
	}
}

// extractAppFunctionBody returns the brace-delimited body of the first function
// whose definition line starts with prefix. It does not understand strings or
// comments containing braces, so callers should pass production code only.
func extractAppFunctionBody(source, prefix string) (string, bool) {
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

// TestWailsShutdownJoinsInFlightTurn verifies shutdown joins a turn that is still
// running when the window closes. The turn's end-of-turn flush needs the owner's
// internal event drainer alive, and ShutdownOwner keeps it alive through the
// in-flight turn join by construction: the drainer runs on the owner context,
// which is independent of the host context and is cancelled only after the join,
// so the accepted turn's flush and transcript commit complete before shutdown
// returns.
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
		t.Fatal("app.shutdown did not join the in-flight turn: the owner's drainer was not kept alive through the turn flush")
	}

	// The accepted turn's work outlived the host window: the submitted message
	// is durably persisted and the turn's transcript commit ran, so the
	// shutdown did not sever in-flight work. The read is deliberate:
	// SessionMessagesFor's non-live branch resolves the session's project and
	// reads it read-only — the read that survives the clean shutdown's store
	// detach, which makes the live resolution unavailable by design.
	msgs, err := ag.SessionMessagesFor(id)
	if err != nil {
		t.Fatalf("SessionMessagesFor after shutdown: %v", err)
	}
	if !hydratedAppContains(msgs, "hi") {
		t.Fatalf("the in-flight turn's message is missing from durable history after shutdown: %#v", msgs)
	}
}

// hydratedAppContains reports whether a display message list contains content.
func hydratedAppContains(messages []agent.DisplayMessage, content string) bool {
	for _, m := range messages {
		if m.Content == content {
			return true
		}
	}
	return false
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

	a.enqueueFrame(deliveryFrame{name: "navigation", payload: nil, title: "Project B"})
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

// TestWailsDeliveryOrdersModelItemAfterBoundary proves the single drainer keeps a
// root-model item after a preceding navigation boundary, and successive model items
// in order — so a delayed boundary cannot overwrite a later model result and
// concurrent switches cannot invert.
func TestWailsDeliveryOrdersModelItemAfterBoundary(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var order []string
	a.emitFn = func(name string, _ any) { mu.Lock(); order = append(order, name); mu.Unlock() }
	a.startDelivery()
	defer a.closeDelivery()

	a.emitFrame("navigation", nil)
	a.emitFrame("model", nil)
	a.emitFrame("model", nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drained %d of 3: %v", n, order)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"navigation", "model", "model"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v", order, want)
		}
	}
}

// TestWailsDeliveryFiltersAndAdvancesPresentation drives the drain-side filter: an
// A frame delivers while A is presentation-current, a navigation boundary advances
// presentation current to B, a B frame then delivers, and both a premature B frame
// and a late A frame are dropped — proving delivery is decided at drain time against
// a drainer-owned presentation current, in FIFO order, with no owner query.
func TestWailsDeliveryFiltersAndAdvancesPresentation(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var got []string
	a.emitFn = func(name string, _ any) { mu.Lock(); got = append(got, name); mu.Unlock() }
	a.startDelivery()
	defer a.closeDelivery()

	a.seedPresented("A")
	a.emitSessionFrame("A", "tokenA", nil)                                                // delivered: A is current
	a.emitSessionFrame("B", "earlyB", nil)                                                // dropped: B not yet current
	a.enqueueFrame(deliveryFrame{name: "navigation", kind: frameAdvance, sessionID: "B"}) // delivered + advances to B
	a.emitSessionFrame("B", "tokenB", nil)                                                // delivered: B is current
	a.emitSessionFrame("A", "lateA", nil)                                                 // dropped: A no longer current
	a.emitFrame("sentinel", nil)                                                          // delivered: global sentinel (last)

	want := []string{"tokenA", "navigation", "tokenB", "sentinel"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0 && got[len(got)-1] == "sentinel"
		cur := append([]string(nil), got...)
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sentinel not drained; got %v, want %v", cur, want)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("delivered %v, want exactly %v (a dropped frame leaked or a delivered frame is missing)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v", got, want)
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
	a.enqueueFrame(deliveryFrame{name: "navigation", payload: nil, title: "Project B"})
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

// TestHostDeliveryCloseDropPolicy proves the desktop host's close deliberately
// drops frames still queued at close: the framework releases the window and
// webview before invoking the shutdown hook, so nothing emitted from inside the
// hook is ever dispatched — the drop is by drainer design, not because the queued
// frame never got a turn. The protocol host is the opposite: its client process
// is still reading the pipe, so its close drains the backlog instead.
func TestHostDeliveryCloseDropPolicy(t *testing.T) {
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
	a.emitFrame("queued_at_close", nil)
	<-entered // the drainer is blocked inside the first emit; the second frame is queued

	a.closeDelivery() // returns at the join bound; the drainer is still blocked

	close(release) // the drainer resumes, sees close, and drops the queued frame
	select {
	case <-a.deliveryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not exit after close")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 1 || emitted[0] != "blocked" {
		t.Fatalf("emitted = %v, want only the frame in flight: the queued frame is dropped at close by design", emitted)
	}
}

// TestWailsOrderedDeliveryContract is the Wails ordered-delivery contract for
// permission resolution: a resolution publishes its frame before the async turn
// end, so the frontend can clear the prompt instead of showing an answerable
// stale request until turn_end, and answering an already-resolved prompt is a
// benign no-op rather than a surfaced error.
func TestWailsOrderedDeliveryContract(t *testing.T) {
	t.Run("permission_resolved=cancel_clears_prompt_before_turn_end", func(t *testing.T) {
		app, id, reqID, log := wailsPermissionPendingApp(t)

		// Cancel while the prompt is pending: the resolution frame must clear the
		// prompt before the turn end event lands.
		if err := app.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		resolved := waitForWailsFrame(t, log, "permission_resolved")
		if got := wailsFrameString(t, resolved, "id"); got != reqID {
			t.Fatalf("permission_resolved id = %q, want the pending request's id %q", got, reqID)
		}
		if got := wailsFrameString(t, resolved, "sessionId"); got != id {
			t.Fatalf("permission_resolved sessionId = %q, want %q", got, id)
		}
		waitForWailsFrame(t, log, "turn_end")
		if idxRes, idxEnd := wailsFrameIndex(t, log, "permission_resolved"), wailsFrameIndex(t, log, "turn_end"); idxRes > idxEnd {
			t.Fatalf("permission_resolved (index %d) delivered after turn_end (index %d)", idxRes, idxEnd)
		}
	})

	t.Run("permission_resolved=answer_publishes_resolution", func(t *testing.T) {
		app, id, reqID, log := wailsPermissionPendingApp(t)

		// The answer path removes the pending request too: answering must publish
		// the same resolution frame the cancel path does.
		if err := app.RespondPermission(id, reqID, "deny"); err != nil {
			t.Fatalf("RespondPermission: %v", err)
		}

		resolved := waitForWailsFrame(t, log, "permission_resolved")
		if got := wailsFrameString(t, resolved, "id"); got != reqID {
			t.Fatalf("permission_resolved id = %q, want the answered request's id %q", got, reqID)
		}
		if got := wailsFrameString(t, resolved, "sessionId"); got != id {
			t.Fatalf("permission_resolved sessionId = %q, want %q", got, id)
		}
	})

	t.Run("permission_respond=already_resolved_returns_no_error", func(t *testing.T) {
		ag := newAppTestAgentAt(t, "http://127.0.0.1:9/v1")
		app := &App{svc: ag, agent: ag}
		app.emitFn = func(string, any) {}
		app.startup(context.Background())
		id := app.currentSessionID()
		if id == "" {
			t.Fatal("startup did not establish a current session")
		}

		// A stale answer — the id can only have come from a prompt this host was
		// given, so an unknown-request outcome means it was resolved underneath
		// the user — is silent on the desktop.
		if err := app.RespondPermission(id, "p1", "deny"); err != nil {
			t.Fatalf("RespondPermission on an already-resolved request returned an error: %v", err)
		}
		// The allow-for-project route answers through the same gate; it must be a
		// benign no-op for an already-resolved request too.
		if err := app.SaveProjectPermission(id, "p1", []string{"/tmp/*"}); err != nil {
			t.Fatalf("SaveProjectPermission on an already-resolved request returned an error: %v", err)
		}
	})
}

// TestWailsTurnActionFrameCarriesFailedRevertWarning drives a fork whose
// best-effort code revert failed through the desktop adapter and proves the
// turn_action boundary frame it emits carries the warning: the adapter's
// mapping from the boundary callback onto the frame is the link between the
// owner's emit and the frontend's render, and losing it would drop the warning
// on the desktop while the agent and frontend tests still pass. It also pins
// the producer prefill semantics for fork at this layer: a real fork emits no
// composer draft (Prefill == nil), unlike history revert whose ordered frame
// carries its nonnil prepared prefill — direct event injection alone cannot
// prove what the owner actually puts on the wire here.
func TestWailsTurnActionFrameCarriesFailedRevertWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())

	sourceID := app.currentSessionID()
	if sourceID == "" {
		t.Fatal("startup did not establish a current session")
	}
	if _, err := ag.AppendUserMessage("fork point"); err != nil {
		t.Fatalf("seed fork point: %v", err)
	}
	sub := filepath.Join(ag.ProjectRoot(), "sub")
	path := filepath.Join(sub, "created-after-fork.txt")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	turn, err := ag.AppendUserMessage("create after fork")
	if err != nil {
		t.Fatalf("seed snapshot turn: %v", err)
	}
	entryID, _, err := ag.Store().SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatalf("snapshot entry: %v", err)
	}
	if err := os.WriteFile(path, []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ag.Store().RecordSnapshotContent(turn, entryID, []byte("later\n")); err != nil {
		t.Fatalf("record snapshot content: %v", err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(sub, 0o700) }()

	result, err := app.ApplyTurnAction(1, agent.TurnActionFork, true)
	if err != nil {
		t.Fatalf("best-effort code revert must not fail a committed fork: %v", err)
	}
	if result.Warning == "" {
		t.Fatal("fork result must carry the failed code revert warning")
	}

	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, frame)
	}
	if boundary.Warning != result.Warning {
		t.Fatalf("turn_action frame warning = %q, want the result's warning %q", boundary.Warning, result.Warning)
	}
	if !reflect.DeepEqual(boundary.SkippedFiles, result.SkippedFiles) {
		t.Fatalf("turn_action frame skipped files = %#v, want the result's %#v", boundary.SkippedFiles, result.SkippedFiles)
	}
	if boundary.State == nil || boundary.State.Session.ID == "" || boundary.State.Session.ID == sourceID {
		t.Fatalf("turn_action frame must carry the fork destination's state, got %#v", boundary.State)
	}
	if boundary.Prefill != nil {
		t.Fatalf("fork turn_action prefill = %q, want nil: only history revert prepares a composer draft on its ordered frame", *boundary.Prefill)
	}
}

// seedAppCompleteTurns persists n complete turns, one user message each,
// through the owner's public append route: the dead model URL fails the model
// call, but the user turn is durably persisted, so a history revert has turns
// to walk.
func seedAppCompleteTurns(t *testing.T, ag *agent.Agent, n int) string {
	t.Helper()
	id := ag.SessionCurrent().ID
	if id == "" {
		var err error
		id, err = ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	}
	for i := 1; i <= n; i++ {
		if _, err := ag.AppendUserMessageToSession(id, fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatalf("append turn %d: %v", i, err)
		}
	}
	return id
}

// blockAppTurnDir makes one turn directory's removal fail: an unwritable
// directory blocks os.RemoveAll exactly there, so the descending history walk
// stops at it.
func blockAppTurnDir(t *testing.T, ag *agent.Agent, turn int) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	blocked := filepath.Join(ag.Store().Dir(), "turns", strconv.Itoa(turn))
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
}

// TestWailsPartialRevertDisposition proves the ApplyTurnAction route resolves
// a reconciled partial history revert as success: the walk removed some turns,
// published the surviving state as an ordered turn_action frame, and rode the
// failure onto the frame as the warning. The direct method must not also
// reject — the frontend would then render the error twice, once from the frame
// and once from the promise.
func TestWailsPartialRevertDisposition(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())
	id := seedAppCompleteTurns(t, ag, 5)
	// Reverting to turn 4 removes turns 4 and 5; blocking turn 4 makes the
	// walk stop there — a partial failure whose history changed, so the
	// revert reconciles and publishes the boundary.
	blockAppTurnDir(t, ag, 4)

	result, err := app.ApplyTurnAction(4, agent.TurnActionRevertHistory, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction on a reconciled partial revert = %v, want success: the emitted frame owns the error", err)
	}
	if result.Warning == "" || !strings.Contains(result.Warning, "turn 4") {
		t.Fatalf("result.Warning = %q, want the walk error naming turn 4", result.Warning)
	}

	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, frame)
	}
	if boundary.Warning != result.Warning {
		t.Fatalf("frame warning = %q, want the result's %q", boundary.Warning, result.Warning)
	}
	if boundary.State == nil || boundary.State.Session.ID != id {
		t.Fatalf("frame state = %#v, want the surviving session %q", boundary.State, id)
	}
	if c := userContents(boundary.State.Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4"}) {
		t.Fatalf("frame state messages = %q, want turns 1-4 (the blocked turn survives)", c)
	}
}

// TestWailsRevertHistoryPartialDisposition proves the exported RevertHistory
// alias follows the same disposition: a reconciled partial revert returns nil
// because the ordered turn_action frame owns the error, so the direct binding
// call cannot render a second error.
func TestWailsRevertHistoryPartialDisposition(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())
	id := seedAppCompleteTurns(t, ag, 5)
	blockAppTurnDir(t, ag, 4)

	if err := app.RevertHistory(4); err != nil {
		t.Fatalf("RevertHistory on a reconciled partial revert = %v, want nil: the emitted frame owns the error", err)
	}

	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, frame)
	}
	if boundary.State == nil || boundary.State.Session.ID != id {
		t.Fatalf("frame state = %#v, want the surviving session %q", boundary.State, id)
	}
	if !strings.Contains(boundary.Warning, "turn 4") {
		t.Fatalf("frame warning = %q, want the walk error naming turn 4", boundary.Warning)
	}
	if c := userContents(boundary.State.Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4"}) {
		t.Fatalf("frame state messages = %q, want turns 1-4 (the blocked turn survives)", c)
	}
}

// committedHistoryService is a fake AdapterService proving the Wails
// turn-action wrapper classifies a typed committed history failure with
// errors.As: the boundary emits with the prepared state/warning/prefill/
// committed error and the service returns the same wrapped error, so the
// method stays a typed rejection while an ordinary partial error still
// resolves as success because its boundary owns the warning.
type committedHistoryService struct {
	agent.AdapterService
	committed bool
}

func (s *committedHistoryService) ApplyTurnActionForSessionWithBoundary(sessionID string, turn int, action string, alsoRevertCode bool, emit func(agent.HydrationState, []snapshot.SkippedRevert, string, *snapshot.CommittedMutationError, *string)) (agent.TurnActionResult, error) {
	prefill := "draft"
	result := agent.TurnActionResult{
		Action:         action,
		Turn:           turn,
		Prefill:        prefill,
		Warning:        "committed sync failure",
		SessionChanged: true,
		Session:        agent.SessionSummary{ID: sessionID},
	}
	state := agent.HydrationState{Session: result.Session}
	if s.committed {
		committed := &snapshot.CommittedMutationError{Err: errors.New("committed sync failure")}
		emit(state, nil, result.Warning, committed, &prefill)
		return result, fmt.Errorf("wrap: %w", committed)
	}
	emit(state, nil, result.Warning, nil, &prefill)
	return result, errors.New("ordinary partial failure")
}

// TestWailsCommittedHistoryRejectsWhileOrdinaryPartialResolves proves the
// Wails turn-action wrapper distinguishes the typed committed-history failure
// from the ordinary partial one through errors.As: each outcome settles to exactly
// one stateful boundary and no adjacent "error" frame — under either frontend
// schedule (boundary-first or rejection-first) both drain this same FIFO, so the
// settled counts are what every consumer sees. The committed half rejects typed;
// the ordinary partial half resolves as success with its warning on the boundary.
func TestWailsCommittedHistoryRejectsWhileOrdinaryPartialResolves(t *testing.T) {
	svc := &committedHistoryService{committed: true}
	app := &App{svc: svc}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startDelivery()
	defer app.closeDelivery()
	app.setCurrentSessionID("A")

	_, err := app.ApplyTurnAction(1, agent.TurnActionRevertHistory, false)
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("typed committed history = %v, want a wrapped CommittedMutationError rejection", err)
	}
	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, frame)
	}
	if boundary.Prefill == nil || *boundary.Prefill != "draft" {
		t.Fatalf("boundary prefill = %#v, want the prepared nonnil prefill %q", boundary.Prefill, "draft")
	}
	if boundary.Warning != "committed sync failure" {
		t.Fatalf("boundary warning = %q, want the prepared warning", boundary.Warning)
	}
	if boundary.State == nil || boundary.State.Session.ID != "A" {
		t.Fatalf("boundary state = %#v, want the prepared session A", boundary.State)
	}

	counts := settledFrameCounts(t, log)
	if n := counts["turn_action"]; n != 1 {
		t.Fatalf("settled turn_action frames = %d, want exactly one stateful boundary: %#v", n, counts)
	}
	assertNoAdjacentErrorFrame(t, counts)

	// Ordinary partial history still resolves as success: its single boundary owns
	// the warning, and an adjacent direct error would duplicate it.
	ordinary := &committedHistoryService{}
	app2 := &App{svc: ordinary}
	log2 := &wailsFrameLog{}
	app2.emitFn = log2.append
	app2.startDelivery()
	defer app2.closeDelivery()
	app2.setCurrentSessionID("A")
	if _, err := app2.ApplyTurnAction(1, agent.TurnActionRevertHistory, false); err != nil {
		t.Fatalf("ordinary partial history = %v, want success: the emitted frame owns the warning", err)
	}
	frame2 := waitForWailsFrame(t, log2, "turn_action")
	boundary2, ok := frame2.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame2, frame2)
	}
	if boundary2.State == nil || boundary2.State.Session.ID != "A" {
		t.Fatalf("ordinary partial boundary state = %#v, want the prepared session A", boundary2.State)
	}

	counts2 := settledFrameCounts(t, log2)
	if n := counts2["turn_action"]; n != 1 {
		t.Fatalf("settled turn_action frames = %d, want exactly one stateful boundary: %#v", n, counts2)
	}
	assertNoAdjacentErrorFrame(t, counts2)
}

// TestWailsCodeRevertStaysNoticeOnly proves the desktop keeps its notice-only
// code-revert surface: a code-only success publishes no complete state
// boundary — the turn_action frame carries nil state with the skipped files,
// exactly as before the owner started prebuilding the protocol boundary.
func TestWailsCodeRevertStaysNoticeOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())
	seedAppCompleteTurns(t, ag, 1)
	path := filepath.Join(ag.ProjectRoot(), "created.txt")
	entryID, _, err := ag.Store().SnapshotResolvedEntry(1, path, path)
	if err != nil {
		t.Fatalf("snapshot entry: %v", err)
	}
	if err := os.WriteFile(path, []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ag.Store().RecordSnapshotContent(1, entryID, []byte("created\n")); err != nil {
		t.Fatalf("record snapshot content: %v", err)
	}
	// Diverging the file's identity after the snapshot makes the restore skip
	// it, so the notice has skipped files to carry.
	if err := os.WriteFile(path, []byte("diverged\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := app.ApplyTurnAction(1, agent.TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction code revert = %v, want success", err)
	}
	if len(result.SkippedFiles) == 0 {
		t.Fatalf("code revert result = %#v, want at least one skipped file", result)
	}

	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, frame)
	}
	if boundary.State != nil {
		t.Fatalf("code revert boundary state = %#v, want nil (notice-only, no complete state)", boundary.State)
	}
	if boundary.Prefill != nil {
		t.Fatalf("code revert boundary prefill = %#v, want nil", boundary.Prefill)
	}
	if !reflect.DeepEqual(boundary.SkippedFiles, result.SkippedFiles) {
		t.Fatalf("code revert boundary skipped = %#v, want the result's %#v", boundary.SkippedFiles, result.SkippedFiles)
	}
}

// TestWailsHistoryEmptyPrefillIsNonnil proves the producer-side prefill disposition end to end: a history revert whose clicked user message is legitimately empty emits a non-nil EMPTY boundary prefill through the real Agent and Wails delivery — distinct from fork/code-revert's nil (TestWailsCodeRevertStaysNoticeOnly). The frontend event-injection tests only prove consumer application; this exercises the actual producer that builds the pointer.
func TestWailsHistoryEmptyPrefillIsNonnil(t *testing.T) {
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())
	seedAppCompleteTurns(t, ag, 1)
	// The clicked turn's user message is legitimately empty content.
	if _, err := ag.AppendUserMessage(""); err != nil {
		t.Fatalf("append empty user message: %v", err)
	}

	result, err := app.ApplyTurnAction(2, agent.TurnActionRevertHistory, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction history revert = %v, want success (the boundary owns every notice)", err)
	}
	if result.Prefill != "" {
		t.Fatalf("result prefill = %q, want the empty clicked message", result.Prefill)
	}

	frame := waitForWailsFrame(t, log, "turn_action")
	boundary, ok := frame.(turnActionBoundary)
	if !ok {
		t.Fatalf("turn_action frame payload is %T, want turnActionBoundary: %#v", frame, boundary)
	}
	if boundary.State == nil || boundary.State.Session.ID != result.Session.ID {
		t.Fatalf("history revert boundary state = %#v, want the reverted session's complete state", boundary.State)
	}
	if boundary.Prefill == nil {
		t.Fatal("empty user message must emit a nonnil prefill pointer (it still clears the composer)")
	}
	if *boundary.Prefill != "" {
		t.Fatalf("boundary prefill = %q, want empty content for an empty clicked message", *boundary.Prefill)
	}
}

type wailsTestFrame struct {
	name    string
	payload any
}

type wailsFrameLog struct {
	mu     sync.Mutex
	frames []wailsTestFrame
}

func (l *wailsFrameLog) append(name string, payload any) {
	l.mu.Lock()
	l.frames = append(l.frames, wailsTestFrame{name: name, payload: payload})
	l.mu.Unlock()
}

func (l *wailsFrameLog) first(name string) (any, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, f := range l.frames {
		if f.name == name {
			return f.payload, i, true
		}
	}
	return nil, -1, false
}

func waitForWailsFrame(t *testing.T, log *wailsFrameLog, name string) any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if payload, _, ok := log.first(name); ok {
			return payload
		}
		if time.Now().After(deadline) {
			log.mu.Lock()
			frames := append([]wailsTestFrame(nil), log.frames...)
			log.mu.Unlock()
			t.Fatalf("timed out waiting for frame %q; frames: %#v", name, frames)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func wailsFrameIndex(t *testing.T, log *wailsFrameLog, name string) int {
	t.Helper()
	_, idx, ok := log.first(name)
	if !ok {
		t.Fatalf("frame %q not present", name)
	}
	return idx
}

// settledFrameCounts waits until the delivery FIFO stops producing new frames,
// then returns per-name counts of everything drained so far. An "exactly N" or
// "no adjacent error frame" assertion is only sound once no further enqueue can
// still be in flight behind it — both turn-action schedules (boundary-first and
// rejection-first) drain through this same FIFO, so the settled counts are what
// either schedule will see.
func settledFrameCounts(t *testing.T, log *wailsFrameLog) map[string]int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		counts := snapshotFrameCounts(log)
		time.Sleep(20 * time.Millisecond)
		log.mu.Lock()
		n := len(log.frames)
		stable := n == countTotal(counts) && countsEqual(snapshotFrameCountsLocked(log), counts)
		frames := append([]wailsTestFrame(nil), log.frames...)
		log.mu.Unlock()
		if stable {
			return counts
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery FIFO never settled; frames: %#v", frames)
		}
	}
}

func snapshotFrameCounts(log *wailsFrameLog) map[string]int {
	log.mu.Lock()
	defer log.mu.Unlock()
	return countFramesLocked(log)
}

// countsEqual compares two per-name frame count maps.
func countsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// countTotal sums the per-name frame counts.
func countTotal(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// snapshotFrameCountsLocked re-counts under a lock the caller already holds.
func snapshotFrameCountsLocked(log *wailsFrameLog) map[string]int {
	return countFramesLocked(log)
}

// countFramesLocked builds per-name counts; callers must hold log.mu.
func countFramesLocked(log *wailsFrameLog) map[string]int {
	counts := make(map[string]int, len(log.frames))
	for _, f := range log.frames {
		counts[f.name]++
	}
	return counts
}

// assertNoAdjacentErrorFrame fails when the settled frame set carries any Wails
// error frame next to a turn-action boundary: for history and fork outcomes the
// ordered boundary owns every warning, so an adjacent error frame would make
// both frontend schedules render it twice.
func assertNoAdjacentErrorFrame(t *testing.T, counts map[string]int) {
	t.Helper()
	if n := counts["error"]; n != 0 {
		t.Fatalf("settled frames carry %d \"error\" frame(s), want none: the ordered boundary owns its warning %#v", n, counts)
	}
}

// wailsPermissionPendingApp wires a Wails app whose first turn asks a read_file
// permission request, submits a message, and waits until the request is pending.
// It returns the app, the session id, the pending request's id, and the frame
// log the test asserts against.
func wailsPermissionPendingApp(t *testing.T) (*App, string, string, *wailsFrameLog) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeAppSSE(w,
				appToolCallChunk("perm-ask", "test-model", "call_read", "read_file", `{"path":"x.txt"}`),
				appStopChunk("perm-ask", "test-model"),
				"[DONE]")
			return
		}
		// A cancelled turn makes no second model call; serve a plain
		// completion so a stray call cannot hang the test.
		writeAppSSE(w,
			appTextChunk("perm-ask-2", "test-model", "done"),
			appStopChunk("perm-ask-2", "test-model"),
			"[DONE]")
	}))
	t.Cleanup(server.Close)

	ag := newAppTestAgentAt(t, server.URL+"/v1")
	app := &App{svc: ag, agent: ag}
	log := &wailsFrameLog{}
	app.emitFn = log.append
	app.startup(context.Background())

	id := app.currentSessionID()
	if id == "" {
		t.Fatal("startup did not establish a current session")
	}
	if _, err := app.Submit("read the file"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	reqPayload := waitForWailsFrame(t, log, "permission_request")
	reqID := wailsFrameString(t, reqPayload, "id")
	if reqID == "" {
		t.Fatalf("permission_request frame carries no id: %#v", reqPayload)
	}
	return app, id, reqID, log
}

func wailsFrameString(t *testing.T, payload any, key string) string {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("frame payload is %T, want map[string]any: %#v", payload, payload)
	}
	s, _ := m[key].(string)
	return s
}

func writeAppSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, payload := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func appTextChunk(id, model, content string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, id, model, content)
}

func appStopChunk(id, model string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, model)
}

func appToolCallChunk(id, model, callID, name, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, id, model, callID, name, argsJSON)
}
