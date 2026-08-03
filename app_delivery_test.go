package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
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
	// shutdown did not sever in-flight work.
	hs, err := app.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession after shutdown: %v", err)
	}
	if !hydratedAppContains(hs.Messages, "hi") {
		t.Fatalf("the in-flight turn's message is missing from durable history after shutdown: %#v", hs.Messages)
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
