package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	"github.com/MMinasyan/lightcode/internal/atomicfs"
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

func TestWailsStartupRealCommittedCreationAdoptsWithoutBoundary(t *testing.T) {
	ag := newAppTestAgent(t)
	app := &App{svc: ag, agent: ag}
	var frames []string
	app.emitFn = func(name string, _ any) { frames = append(frames, name) }
	root := ag.Projects().Root()
	atomicfs.SyncDirFunc = func(dir string) error {
		if strings.HasSuffix(dir, string(filepath.Separator)+"sessions") {
			return errors.New("injected startup publication sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	app.startup(context.Background())
	if app.currentSessionID() == "" {
		t.Fatal("startup did not adopt committed destination")
	}
	for _, frame := range frames {
		if frame == "navigation" || frame == "session_changed" {
			t.Fatalf("startup emitted lifecycle frame %q; startup must not emit a startup event", frame)
		}
	}
	if !strings.HasPrefix(root, filepath.Dir(ag.Projects().Root())) {
		t.Fatal("startup test did not use the real project namespace")
	}
}

func TestWailsRealCommittedNamespaceProducers(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		ag := newAppTestAgent(t)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		app := newTestApp(ag)
		app.setCurrentSessionID(id)
		sessionDir := filepath.Join(ag.Projects().SessionsRoot(project.ID), id)
		var syncCalls int
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionDir {
				syncCalls++
				if syncCalls == 2 {
					return errors.New("injected Wails archive sync failure")
				}
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		err = app.SessionArchive(id)
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("Wails archive error = %v, want committed error", err)
		}
		if app.currentSessionID() != "" {
			t.Fatalf("Wails current after committed archive = %q, want detached", app.currentSessionID())
		}
		meta, err := snapshot.LoadSessionMeta(ag.Projects().SessionsRoot(project.ID), id)
		if err != nil {
			t.Fatal(err)
		}
		if meta.State != snapshot.StateArchived {
			t.Fatalf("Wails archive state = %q, want archived", meta.State)
		}
	})

	t.Run("delete", func(t *testing.T) {
		ag := newAppTestAgent(t)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		app := newTestApp(ag)
		app.setCurrentSessionID(id)
		sessionsRoot := ag.Projects().SessionsRoot(project.ID)
		injected := errors.New("injected Wails delete sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		err = app.SessionDelete(id)
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("Wails delete error = %v, want committed error", err)
		}
		if app.currentSessionID() != "" {
			t.Fatalf("Wails current after committed delete = %q, want detached", app.currentSessionID())
		}
		if _, err := os.Stat(filepath.Join(sessionsRoot, id)); !os.IsNotExist(err) {
			t.Fatalf("Wails deleted source = %v, want absent", err)
		}
	})

	t.Run("history", func(t *testing.T) {
		ag := newAppTestAgent(t)
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
		app := newTestApp(ag)
		app.setCurrentSessionID(id)
		turnsDir := filepath.Join(ag.Projects().SessionsRoot(project.ID), id, "turns")
		injected := errors.New("injected Wails history sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == turnsDir {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		err = app.RevertHistory(lastTurn)
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("Wails history error = %v, want committed error", err)
		}
		if app.currentSessionID() != id {
			t.Fatalf("Wails current after committed history = %q, want %q", app.currentSessionID(), id)
		}
		if _, err := os.Stat(filepath.Join(turnsDir, fmt.Sprint(lastTurn))); !os.IsNotExist(err) {
			t.Fatalf("Wails reverted turn = %v, want removed", err)
		}
	})

	t.Run("fork", func(t *testing.T) {
		ag := newAppTestAgent(t)
		sourceID, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatal(err)
		}
		turn, err := ag.AppendUserMessageToSession(sourceID, "fork source")
		if err != nil {
			t.Fatal(err)
		}
		project, err := ag.Projects().Ensure()
		if err != nil {
			t.Fatal(err)
		}
		app := newTestApp(ag)
		app.setCurrentSessionID(sourceID)
		sessionsRoot := ag.Projects().SessionsRoot(project.ID)
		injected := errors.New("injected Wails fork sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		err = app.ForkSession(turn)
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("Wails fork error = %v, want committed error", err)
		}
		destinationID := app.currentSessionID()
		if destinationID == "" || destinationID == sourceID {
			t.Fatalf("Wails current after committed fork = %q, want adopted destination", destinationID)
		}
		if _, err := os.Stat(filepath.Join(sessionsRoot, destinationID, "meta.json")); err != nil {
			t.Fatalf("Wails fork destination metadata: %v", err)
		}
	})
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

// lifecycleMutationSvc is a fake AdapterService whose archive and delete return
// one prepared outcome: nil for success, a plain error, or the same wrapped
// CommittedMutationError both methods share. Every other owner call panics on
// the embedded nil interface, so an unexpected probe fails loudly instead of
// silently succeeding; nothing here activates a real producer — only the
// consumer's classification and frame disposition are under test.
type lifecycleMutationSvc struct {
	agent.AdapterService
	out error // nil = success; plain or wrapped committed failure
}

func (s *lifecycleMutationSvc) SessionArchive(string) error { return s.out }
func (s *lifecycleMutationSvc) SessionDelete(string) error  { return s.out }

// waitWailsFrames returns the first want delivered frames once at least that many
// have been emitted, so an exact-sequence assertion runs against a stable prefix.
func waitWailsFrames(t *testing.T, log *wailsFrameLog, want int) []wailsTestFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		log.mu.Lock()
		if len(log.frames) >= want {
			frames := append([]wailsTestFrame(nil), log.frames[:want]...)
			log.mu.Unlock()
			return frames
		}
		log.mu.Unlock()
		if time.Now().After(deadline) {
			log.mu.Lock()
			got := append([]wailsTestFrame(nil), log.frames...)
			log.mu.Unlock()
			t.Fatalf("timed out waiting for %d delivered frames; got %#v", want, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWailsArchiveDeleteCommittedOutcomeTable proves the Wails archive/delete
// consumer table row by row: success detaches current and removes nothing from a
// noncurrent target's presentation, any plain error enqueues one standalone
// unsequenced error with no advance at all, and a committed failure rejects typed
// while atomically pairing its boundary (empty detach for the removed current, nil
// unchanged-current advance otherwise) with exactly one unsequenced error frame.
// The paired error's presence in delivery proves its tag: after an empty-detach
// adopt, only an empty/global-tagged frame survives; after a noncurrent removal,
// only a frame tagged to the unchanged current does — a removed-session tag would
// be filtered by the drainer and never reach this log.
func TestWailsArchiveDeleteCommittedOutcomeTable(t *testing.T) {
	committed := &snapshot.CommittedMutationError{Err: errors.New("committed sync failure")}

	for _, op := range []struct {
		name string
		run  func(*App, string) error
	}{
		{name: "archive", run: func(app *App, id string) error { return app.SessionArchive(id) }},
		{name: "delete", run: func(app *App, id string) error { return app.SessionDelete(id) }},
	} {
		t.Run(op.name, func(t *testing.T) {
			rows := []struct {
				name        string
				target      string // the session archive/delete is called with
				out         error  // the fake service outcome
				wantCurrent bool   // routing current after the call (false = cleared/unchanged asserted separately)
				wantFrames  int    // delivered frames, exact order below
			}{
				{name: "success_current", target: "A", out: nil},
				{name: "success_noncurrent", target: "B", out: nil},
				{name: "plain_error_current", target: "A", out: errors.New("precommit failure")},
				{name: "plain_error_noncurrent", target: "B", out: errors.New("precommit failure")},
				{name: "committed_current", target: "A", out: fmt.Errorf("wrap: %w", committed)},
				{name: "committed_noncurrent", target: "B", out: fmt.Errorf("wrap: %w", committed)},
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					svc := &lifecycleMutationSvc{out: row.out}
					app := &App{svc: svc}
					log := &wailsFrameLog{}
					app.emitFn = log.append
					app.startDelivery()
					defer app.closeDelivery()
					app.seedPresented("A") // presentation current is A; the target may or may not be it
					app.setCurrentSessionID("A")

					err := op.run(app, row.target)

					var committedErr *snapshot.CommittedMutationError
					switch {
					case row.out == nil:
						if err != nil {
							t.Fatalf("%s = %v, want success", row.name, err)
						}
					case errors.As(row.out, &committedErr):
						// A committed failure must reject with the same wrapped type.
						if !errors.As(err, &committedErr) {
							t.Fatalf("%s = %v, want a wrapped CommittedMutationError rejection", row.name, err)
						}
					default:
						// A plain precommit error passes through unclassified; it must not be reported as committed.
						if err == nil || errors.As(err, &committedErr) {
							t.Fatalf("%s = %v; want the plain failure passed through without a committed classification", row.name, err)
						}
					}

					switch row.name {
					case "success_current":
						frames := waitWailsFrames(t, log, 1)
						if got := classifyLifecycleAdvanceIndex(t, frames[0]); got != "detach" {
							t.Fatalf("delivered frame = %s (%#v), want the empty detach boundary", got, frames[0])
						}
						counts := settledFrameCounts(t, log)
						assertExactSettledFrames(t, counts, map[string]int{"navigation": 1})
						if cur := app.currentSessionID(); cur != "" {
							t.Fatalf("routing current after a successful removal of the current session = %q, want cleared", cur)
						}
					case "success_noncurrent":
						frames := waitWailsFrames(t, log, 1)
						if got := classifyLifecycleAdvanceIndex(t, frames[0]); got != "nil-advance" {
							t.Fatalf("delivered frame = %s (%#v), want the nil unchanged-current advance", got, frames[0])
						}
						counts := settledFrameCounts(t, log)
						assertExactSettledFrames(t, counts, map[string]int{"navigation": 1})
						if cur := app.currentSessionID(); cur != "A" {
							t.Fatalf("routing current after a noncurrent removal = %q, want the unchanged A", cur)
						}
					case "plain_error_current", "plain_error_noncurrent":
						frames := waitWailsFrames(t, log, 1)
						assertUnsequencedErrorFrame(t, frames[0], row.out.Error())
						counts := settledFrameCounts(t, log)
						if n := counts["navigation"]; n != 0 {
							t.Fatalf("plain error enqueued %d navigation frame(s), want no advance: %#v", n, counts)
						}
						if cur := app.currentSessionID(); cur != "A" {
							t.Fatalf("routing current after a plain failure = %q, want the unchanged A (no advance)", cur)
						}
					case "committed_current":
						frames := waitWailsFrames(t, log, 2)
						if got := classifyLifecycleAdvanceIndex(t, frames[0]); got != "detach" {
							t.Fatalf("frame 1 = %s (%#v), want the empty detach boundary", got, frames[0])
						}
						assertUnsequencedErrorFrame(t, frames[1], row.out.Error())
						counts := settledFrameCounts(t, log)
						assertExactSettledFrames(t, counts, map[string]int{"navigation": 1, "error": 1})
						if cur := app.currentSessionID(); cur != "" {
							t.Fatalf("routing current after a committed removal of the current session = %q, want cleared", cur)
						}
					case "committed_noncurrent":
						frames := waitWailsFrames(t, log, 2)
						if got := classifyLifecycleAdvanceIndex(t, frames[0]); got != "nil-advance" {
							t.Fatalf("frame 1 = %s (%#v), want the nil unchanged-current advance", got, frames[0])
						}
						assertUnsequencedErrorFrame(t, frames[1], row.out.Error())
						counts := settledFrameCounts(t, log)
						assertExactSettledFrames(t, counts, map[string]int{"navigation": 1, "error": 1})
						if cur := app.currentSessionID(); cur != "A" {
							t.Fatalf("routing current after a committed noncurrent removal = %q, want the unchanged A", cur)
						}
					}
				})
			}
		})
	}
}

// assertExactSettledFrames fails when the settled per-name counts differ from want,
// printing both maps for exact-pair assertions.
func assertExactSettledFrames(t *testing.T, got, want map[string]int) {
	t.Helper()
	if !countsEqual(got, want) {
		t.Fatalf("settled frames = %#v, want exactly %#v", got, want)
	}
}

// classifyLifecycleAdvanceIndex classifies one delivered lifecycle boundary frame:
// a nil payload is the unchanged-current advance, and an all-empty HydrationState
// (no session id, no messages, not busy) is the current detach. Anything else is a
// stateful replacement that this consumer never enqueues for archive/delete.
func classifyLifecycleAdvanceIndex(t *testing.T, f wailsTestFrame) string {
	t.Helper()
	switch p := f.payload.(type) {
	case nil:
		return "nil-advance"
	case agent.HydrationState:
		if p.Session.ID == "" && len(p.Messages) == 0 && !p.Busy {
			return "detach"
		}
		t.Fatalf("navigation payload is a stateful replacement (%#v); archive/delete enqueues only the empty detach or a nil advance", f.payload)
	default:
		t.Fatalf("navigation payload is %T (%#v); want the empty detach state or a nil advance", f.payload, f.payload)
	}
	return "" // unreachable: every branch above returns or fails; satisfies the compiler after t.Fatalf paths
}

// assertUnsequencedErrorFrame proves one delivered frame is the existing unsequenced
// error shape carrying exactly message (no seq field): a zero-stamped seq would be
// gated as already shown against every snapshot high-water and never render.
func assertUnsequencedErrorFrame(t *testing.T, f wailsTestFrame, wantMsg string) {
	t.Helper()
	if f.name != "error" {
		t.Fatalf("frame = event %q payload %#v; want the unsequenced error frame", f.name, f.payload)
	}
	m, ok := f.payload.(map[string]any)
	if !ok {
		t.Fatalf("error frame payload is %T (%#v), want map[string]any", f.payload, f.payload)
	}
	if _, present := m["seq"]; present {
		t.Fatalf("lifecycle error frame carries seq %#v; it must be unsequenced so the frontend gate admits it", m["seq"])
	}
	if m["message"] != wantMsg {
		t.Fatalf("error frame message = %q, want %q", m["message"], wantMsg)
	}
}

// stagedLifecycleSvc is a fake AdapterService whose lifecycle routes return prepared
// outcomes: the create fallback of open-or-create (staged new), and fork. Each route
// mirrors the owner contract exactly — success emits its prepared state with a nil
// outcome; a committed failure emits that same prepared destination together with its
// wrapped rejection while still returning the destination id; a plain precommit
// failure invokes no callback at all and returns nothing usable. Every other owner
// call panics on the embedded nil interface, so an unexpected probe fails loudly;
// nothing here activates a real producer — only consumer classification, routing
// adoption, and frame disposition are under test.
type stagedLifecycleSvc struct {
	agent.AdapterService // embedded nil: any unoverridden probe panics

	newID   string // create's prepared destination id ("" = none)
	newErr  error  // create outcome: nil / plain / wrapped committed
	forkID  string // fork's prepared destination id
	forkErr error  // fork outcome, same split as newErr
}

func (s *stagedLifecycleSvc) SessionListForProjectPath(_, _ string) ([]agent.SessionSummary, error) {
	return nil, nil
}

// emitPreparedState is the owner contract for a staged route's in-commit callback:
// success emits with nil; committed failure emits prepared destination plus its
// wrapped rejection and returns that id too; plain precommit invokes nothing.
func (s *stagedLifecycleSvc) NewSessionForProjectPathWithBoundary(_, _ string, emit func(agent.HydrationState, error)) (string, error) {
	state := agent.HydrationState{Session: agent.SessionSummary{ID: s.newID}}
	switch {
	case s.newErr == nil && s.newID != "":
		emit(state, nil)
		return s.newID, nil
	default:
		var committed *snapshot.CommittedMutationError
		if s.newID != "" && errors.As(s.newErr, &committed) {
			emit(state, s.newErr)
			return s.newID, s.newErr
		}
		return "", s.newErr // plain precommit (or no destination): no callback at all
	}
}

func (s *stagedLifecycleSvc) ApplyTurnActionForSessionWithBoundary(_ string, _ int, action string, _ bool, emit func(agent.HydrationState, []snapshot.SkippedRevert, string, *snapshot.CommittedMutationError, *string)) (agent.TurnActionResult, error) {
	if action != agent.TurnActionFork {
		panic("staged lifecycle fake received a non-fork turn action; only fork is under test here")
	}
	result := agent.TurnActionResult{SessionChanged: true, Session: agent.SessionSummary{ID: s.forkID}}
	switch {
	case s.forkErr == nil && s.forkID != "":
		emit(agent.HydrationState{Session: result.Session}, nil, "", nil, nil)
		return result, nil
	default:
		var committed *snapshot.CommittedMutationError
		if s.forkID != "" && errors.As(s.forkErr, &committed) {
			emit(agent.HydrationState{Session: result.Session}, nil, "", committed, nil)
			return result, s.forkErr
		}
		return agent.TurnActionResult{}, s.forkErr // plain precommit: no callback at all
	}
}

// TestWailsStagedLifecycleOutcomeTable proves the Wails staged-new and fork consumer
// rows of the lifecycle table: success appends exactly its destination boundary and
// adopts routing; a committed failure adopts the prepared destination first, then
// atomically pairs that same boundary with one unsequenced error frame while still
// rejecting typed — so no stale queued frame can reverse or split them; any plain
// precommit failure invokes nothing: zero frames of every kind, routing untouched.
func TestWailsStagedLifecycleOutcomeTable(t *testing.T) {
	committed := &snapshot.CommittedMutationError{Err: errors.New("committed sync failure")}

	t.Run("op=session_new", func(t *testing.T) {
		rows := []struct {
			name    string
			newID   string
			newErr  error
			wantCur bool // routing current adopted after the call ("" asserted when false)
		}{
			{name: "success", newID: "dest"},
			{name: "committed", newID: "dest", newErr: fmt.Errorf("wrap: %w", committed), wantCur: true},
			{name: "plain_error", newErr: errors.New("precommit failure")},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				svc := &stagedLifecycleSvc{newID: row.newID, newErr: row.newErr}
				app := &App{svc: svc, routeProjectPath: "/proj"}
				log := &wailsFrameLog{}
				app.emitFn = log.append
				app.startDelivery()
				defer app.closeDelivery()

				err := app.SessionNew()

				var committedErr *snapshot.CommittedMutationError
				switch {
				case row.newErr == nil:
					if err != nil {
						t.Fatalf("SessionNew = %v, want success", err)
					}
				case errors.As(row.newErr, &committedErr):
					if !errors.As(err, &committedErr) {
						t.Fatalf("SessionNew = %v, want the wrapped committed rejection passed through typed", err)
					}
				default:
					if err == nil || errors.As(err, &committedErr) {
						t.Fatalf("SessionNew = %v; a plain failure must pass through unclassified and never report as committed", err)
					}
				}

				switch row.name {
				case "success":
					frames := waitWailsFrames(t, log, 1)
					assertStagedDestinationBoundary(t, frames[0], "navigation", "dest")
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"navigation": 1})
				case "committed":
					frames := waitWailsFrames(t, log, 2)
					assertStagedDestinationBoundary(t, frames[0], "navigation", "dest")
					assertUnsequencedErrorFrame(t, frames[1], row.newErr.Error())
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"navigation": 1, "error": 1})
				case "plain_error":
					counts := settledFrameCounts(t, log)
					if len(counts) != 0 {
						t.Fatalf("a plain precommit failure enqueued %#v frames; it must invoke no callback and append nothing", counts)
					}
				}

				wantCur := ""
				if row.wantCur || (row.name == "success" && row.newID != "") {
					wantCur = "dest"
				}
				if cur := app.currentSessionID(); cur != wantCur {
					t.Fatalf("routing current after %s = %q, want %q", row.name, cur, wantCur)
				}
			})
		}
	})

	t.Run("op=project_switch_fallback", func(t *testing.T) {
		rows := []struct {
			name    string
			newID   string
			newErr  error
			wantAbs bool // the destination project path committed after the call (old route when false)
		}{
			{name: "success", newID: "dest"},
			{name: "committed", newID: "dest", newErr: fmt.Errorf("wrap: %w", committed), wantAbs: true},
			{name: "plain_error", newErr: errors.New("precommit failure")},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				target := t.TempDir() // ProjectSwitch stats the target; a real directory passes its prechecks
				svc := &stagedLifecycleSvc{newID: row.newID, newErr: row.newErr}
				app := &App{svc: svc, routeProjectPath: "/old"}
				log := &wailsFrameLog{}
				app.emitFn = log.append
				app.titleFn = func(string) {} // the boundary's project title must not reach a real window in tests
				app.startDelivery()
				defer app.closeDelivery()

				err := app.ProjectSwitch(target)

				var committedErr *snapshot.CommittedMutationError
				switch {
				case row.newErr == nil:
					if err != nil {
						t.Fatalf("ProjectSwitch fallback create = %v, want success", err)
					}
				case errors.As(row.newErr, &committedErr):
					if !errors.As(err, &committedErr) {
						t.Fatalf("ProjectSwitch fallback create = %v, want the wrapped committed rejection passed through typed", err)
					}
				default:
					if err == nil || errors.As(err, &committedErr) {
						t.Fatalf("ProjectSwitch fallback create = %v; a plain failure must pass through unclassified and never report as committed", err)
					}
				}

				switch row.name {
				case "success":
					frames := waitWailsFrames(t, log, 1)
					assertStagedDestinationBoundary(t, frames[0], "navigation", "dest")
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"navigation": 1})
				case "committed":
					frames := waitWailsFrames(t, log, 2)
					assertStagedDestinationBoundary(t, frames[0], "navigation", "dest")
					assertUnsequencedErrorFrame(t, frames[1], row.newErr.Error())
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"navigation": 1, "error": 1})
				case "plain_error":
					counts := settledFrameCounts(t, log)
					if len(counts) != 0 {
						t.Fatalf("a plain precommit failure enqueued %#v frames; it must invoke no callback and append nothing", counts)
					}
				}

				wantPath := "/old" // a plain create error leaves the old route exactly as it was
				if row.wantAbs || (row.name == "success") {
					wantPath = target
				}
				if path := app.routeProjectPath; path != wantPath {
					t.Fatalf("routing project after %s = %q, want %q", row.name, path, wantPath)
				}
				wantCur := ""
				if row.wantAbs || (row.name == "success") {
					wantCur = "dest"
				}
				if cur := app.currentSessionID(); cur != wantCur {
					t.Fatalf("routing current after %s = %q, want %q", row.name, cur, wantCur)
				}
			})
		}
	})

	t.Run("op=fork", func(t *testing.T) {
		rows := []struct {
			name    string
			forkID  string
			forkErr error
			wantCur bool // the prepared destination adopted after the call (source kept when false)
		}{
			{name: "success", forkID: "forkdest"},
			{name: "committed", forkID: "forkdest", forkErr: fmt.Errorf("wrap: %w", committed), wantCur: true},
			{name: "plain_error", forkErr: errors.New("precommit failure")},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				svc := &stagedLifecycleSvc{forkID: row.forkID, forkErr: row.forkErr}
				app := &App{svc: svc}
				app.setCurrentSessionID("src") // the action's source; a plain failure must keep it
				log := &wailsFrameLog{}
				app.emitFn = log.append
				app.startDelivery()
				defer app.closeDelivery()

				err := app.ForkSession(1)

				var committedErr *snapshot.CommittedMutationError
				switch {
				case row.forkErr == nil:
					if err != nil {
						t.Fatalf("ForkSession = %v, want success", err)
					}
				case errors.As(row.forkErr, &committedErr):
					if !errors.As(err, &committedErr) {
						t.Fatalf("ForkSession = %v, want the wrapped committed rejection passed through typed", err)
					}
				default:
					if err == nil || errors.As(err, &committedErr) {
						t.Fatalf("ForkSession = %v; a plain failure must pass through unclassified and never report as committed", err)
					}
				}

				switch row.name {
				case "success":
					frames := waitWailsFrames(t, log, 1)
					assertStagedDestinationBoundary(t, frames[0], "turn_action", "forkdest")
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"turn_action": 1})
				case "committed":
					frames := waitWailsFrames(t, log, 2)
					assertStagedDestinationBoundary(t, frames[0], "turn_action", "forkdest")
					// The callback carries the typed committed pointer; its message renders through that type.
					var c *snapshot.CommittedMutationError
					if !errors.As(row.forkErr, &c) {
						t.Fatalf("test setup: %v is not wrapped as committed", row.forkErr)
					}
					assertUnsequencedErrorFrame(t, frames[1], c.Error())
					counts := settledFrameCounts(t, log)
					assertExactSettledFrames(t, counts, map[string]int{"turn_action": 1, "error": 1})
				case "plain_error":
					counts := settledFrameCounts(t, log)
					if len(counts) != 0 {
						t.Fatalf("a plain precommit failure enqueued %#v frames; it must invoke no callback and append nothing", counts)
					}
				}

				wantCur := "src" // the source is retained unless a destination was prepared
				if row.wantCur || (row.name == "success") {
					wantCur = "forkdest"
				}
				if cur := app.currentSessionID(); cur != wantCur {
					t.Fatalf("routing current after %s fork = %q, want %q", row.name, cur, wantCur)
				}
			})
		}
	})

	// The GUI's fork route is ApplyTurnAction (the message menu), not the ForkSession
	// alias: both entry points must settle a committed failure through one shared
	// disposition — prepared destination adopted, its boundary atomically paired with
	// exactly one unsequenced error frame, and the rejection returned typed. This row
	// drives that route directly so an adapter-only fix to ForkSession cannot pass it.
	t.Run("op=fork_via_apply_turn_action_committed", func(t *testing.T) {
		svc := &stagedLifecycleSvc{forkID: "forkdest", forkErr: fmt.Errorf("wrap: %w", committed)}
		app := &App{svc: svc}
		app.setCurrentSessionID("src") // the action's source; a plain failure must keep it
		log := &wailsFrameLog{}
		app.emitFn = log.append
		app.startDelivery()
		defer app.closeDelivery()

		result, err := app.ApplyTurnAction(1, agent.TurnActionFork, false)

		var committedErr *snapshot.CommittedMutationError
		if !errors.As(err, &committedErr) {
			t.Fatalf("ApplyTurnAction fork = %v, want the wrapped committed rejection passed through typed", err)
		}
		frames := waitWailsFrames(t, log, 2)
		assertStagedDestinationBoundary(t, frames[0], "turn_action", "forkdest")
		var c *snapshot.CommittedMutationError
		if !errors.As(svc.forkErr, &c) {
			t.Fatalf("test setup: %v is not wrapped as committed", svc.forkErr)
		}
		assertUnsequencedErrorFrame(t, frames[1], c.Error()) // the same message text every other consumer surfaces
		counts := settledFrameCounts(t, log)
		assertExactSettledFrames(t, counts, map[string]int{"turn_action": 1, "error": 1})
		if cur := app.currentSessionID(); cur != "forkdest" {
			t.Fatalf("routing current after a committed fork = %q, want the prepared destination adopted", cur)
		}
		if !result.SessionChanged {
			t.Fatal("a committed fork result must report SessionChanged: its boundary owns the new presentation")
		}
	})

	// History's typed committed failure keeps Step 4's warning-only disposition on this very
	// route — one stateful boundary, no adjacent error frame even though a fork now pairs.
	// TestWailsCommittedHistoryRejectsWhileOrdinaryPartialResolves pins the same row through
	// ApplyTurnAction; it must stay green against the shared choke point unchanged for history.
}

// assertStagedDestinationBoundary proves one delivered frame is the staged route's
// destination boundary: a navigation (or turn_action) advance carrying exactly that
// session's prepared complete state.
func assertStagedDestinationBoundary(t *testing.T, f wailsTestFrame, wantName, wantID string) {
	t.Helper()
	if f.name != wantName {
		t.Fatalf("frame = event %q payload %#v; want the %s destination boundary", f.name, f.payload, wantName)
	}
	switch p := f.payload.(type) {
	case agent.HydrationState:
		if p.Session.ID != wantID {
			t.Fatalf("boundary session id = %q, want the prepared destination %q", p.Session.ID, wantID)
		}
	case turnActionBoundary:
		if p.State == nil || p.State.Session.ID != wantID {
			t.Fatalf("turn_action boundary state = %#v, want the prepared destination's complete state (%s)", p.State, wantID)
		}
	default:
		t.Fatalf("%s frame payload is %T; want a HydrationState or turnActionBoundary", wantName, f.payload)
	}
}

// TestWailsLifecycleCommittedPairIsolation proves the committed pair's FIFO position
// end to end through the real drainer: an older queued session frame drains first,
// then boundary and its paired error arrive adjacent — no interleaving between them —
// and a later navigation lands after both. The current-detach row proves only an
// empty/global-tagged error survives (a deleted-session tag would be filtered by the
// adopted-empty presentation); the noncurrent row proves the nil advance re-adopts
// the unchanged current and its current-tagged pair follows it; a late frame tagged to
// either removed session is then replaced/filtered by the later navigation. Unrelated
// busy state never rides these frames, so nothing here clears another session's view.
func TestWailsLifecycleCommittedPairIsolation(t *testing.T) {
	t.Run("atomic_enqueue_source_contract", func(t *testing.T) {
		source, err := os.ReadFile("app.go")
		if err != nil {
			t.Fatalf("read app.go: %v", err)
		}
		body, ok := extractAppFunctionBody(string(source), "func (a *App) enqueueBoundaryWithError(")
		if !ok {
			t.Fatal("enqueueBoundaryWithError body not found")
		}
		if strings.Contains(body, ".enqueueFrame(") || strings.Contains(body, ".enqueueBoundary(") {
			t.Fatal("enqueueBoundaryWithError must append the pair directly; it must not delegate to another enqueue helper")
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "app.go", source, 0)
		if err != nil {
			t.Fatalf("parse app.go: %v", err)
		}
		var fn *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if ok && decl.Name.Name == "enqueueBoundaryWithError" {
				fn = decl
				return false
			}
			return true
		})
		if fn == nil || fn.Body == nil {
			t.Fatal("AST did not find enqueueBoundaryWithError")
		}

		isField := func(expr ast.Expr, field string) bool {
			sel, ok := expr.(*ast.SelectorExpr)
			return ok && sel.Sel.Name == field
		}
		isFieldCall := func(call *ast.CallExpr, field, method string) bool {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != method {
				return false
			}
			return isField(sel.X, field)
		}
		fieldValue := func(lit *ast.CompositeLit, key string) ast.Expr {
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				ident, ok := kv.Key.(*ast.Ident)
				if ok && ident.Name == key {
					return kv.Value
				}
			}
			return nil
		}
		isDeliveryFrameType := func(expr ast.Expr) bool {
			if ident, ok := expr.(*ast.Ident); ok {
				return ident.Name == "deliveryFrame"
			}
			return false
		}
		var locks, unlocks, wakes []token.Pos
		var pairSlice *ast.CompositeLit
		var errorFrame *ast.CompositeLit
		var appendCalls []*ast.CallExpr
		var forbiddenCalls []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if isFieldCall(node, "deliveryMu", "Lock") {
					locks = append(locks, node.Pos())
				}
				if isFieldCall(node, "deliveryMu", "Unlock") {
					unlocks = append(unlocks, node.Pos())
				}
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "enqueueFrame" || sel.Sel.Name == "enqueueBoundary") {
					forbiddenCalls = append(forbiddenCalls, sel.Sel.Name)
				}
				if ident, ok := node.Fun.(*ast.Ident); ok && ident.Name == "append" {
					appendCalls = append(appendCalls, node)
				}
			case *ast.CompositeLit:
				if isDeliveryFrameType(node.Type) {
					if name := fieldValue(node, "name"); name != nil {
						if lit, ok := name.(*ast.BasicLit); ok && lit.Value == `"error"` {
							errorFrame = node
						}
					}
				}
				if array, ok := node.Type.(*ast.ArrayType); ok {
					if ident, ok := array.Elt.(*ast.Ident); ok && ident.Name == "deliveryFrame" {
						pairSlice = node
					}
				}
			case *ast.SendStmt:
				if isField(node.Chan, "deliveryWake") {
					wakes = append(wakes, node.Pos())
				}
			}
			return true
		})
		if pairSlice != nil {
			// The explicit error deliveryFrame is inspected above; the boundary is
			// the elided element of the []deliveryFrame literal.
		}

		if len(forbiddenCalls) != 0 {
			t.Fatalf("forbidden enqueue helper calls in pair body: %v", forbiddenCalls)
		}
		if len(locks) != 1 {
			t.Fatalf("deliveryMu locks = %d, want exactly one lock for the pair append", len(locks))
		}
		if len(wakes) != 1 {
			t.Fatalf("delivery wake sends = %d, want exactly one", len(wakes))
		}
		if pairSlice == nil || len(pairSlice.Elts) != 1 || errorFrame == nil {
			t.Fatalf("deliveryFrame pair construction = slice=%v, elements=%d, error=%v; want one []deliveryFrame literal plus its adjacent error frame", pairSlice != nil, func() int {
				if pairSlice == nil {
					return 0
				}
				return len(pairSlice.Elts)
			}(), errorFrame != nil)
		}
		firstFrame, ok := pairSlice.Elts[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("first pair element = %T, want implicit deliveryFrame literal", pairSlice.Elts[0])
		}
		if kind := fieldValue(firstFrame, "kind"); kind == nil {
			t.Fatal("first deliveryFrame has no kind")
		} else if ident, ok := kind.(*ast.Ident); !ok || ident.Name != "frameAdvance" {
			t.Fatalf("first deliveryFrame kind = %T %#v, want frameAdvance", kind, kind)
		}
		if name := fieldValue(errorFrame, "name"); name == nil {
			t.Fatal("second deliveryFrame has no name")
		} else if lit, ok := name.(*ast.BasicLit); !ok || lit.Value != `"error"` {
			t.Fatalf("second deliveryFrame name = %T %#v, want \"error\"", name, name)
		}
		if errorFrame.Pos() <= pairSlice.Pos() {
			t.Fatalf("error frame construction at %v precedes boundary construction at %v", errorFrame.Pos(), pairSlice.Pos())
		}
		if len(appendCalls) != 2 {
			t.Fatalf("append calls in pair body = %d, want local-frame append plus one deliveryFrames append", len(appendCalls))
		}
		lock := locks[0]
		if !(lock < pairSlice.Pos()) {
			t.Fatalf("pair frame construction must follow the deliveryMu lock: lock=%v pair=%v", lock, pairSlice.Pos())
		}
		for _, call := range appendCalls {
			if !(lock < call.Pos()) {
				t.Fatalf("append at %v occurs before the deliveryMu lock at %v", call.Pos(), lock)
			}
		}
		lastAppend := appendCalls[0].Pos()
		if appendCalls[1].Pos() > lastAppend {
			lastAppend = appendCalls[1].Pos()
		}
		afterPairUnlocks := 0
		var pairUnlock token.Pos
		for _, pos := range unlocks {
			if pos > lastAppend {
				afterPairUnlocks++
				pairUnlock = pos
			}
		}
		if afterPairUnlocks != 1 {
			t.Fatalf("deliveryMu unlocks after the pair append = %d, want exactly one", afterPairUnlocks)
		}
		if pairUnlock <= errorFrame.Pos() {
			t.Fatalf("pair unlock at %v precedes the error frame construction at %v", pairUnlock, errorFrame.Pos())
		}
		if wakes[0] < pairUnlock {
			t.Fatalf("delivery wake at %v occurs before the pair's deliveryMu unlock at %v", wakes[0], pairUnlock)
		}
	})

	committed := &snapshot.CommittedMutationError{Err: errors.New("committed sync failure")}

	t.Run("target=current", func(t *testing.T) {
		svc := &lifecycleMutationSvc{out: fmt.Errorf("wrap: %w", committed)}
		app := &App{svc: svc}
		log := &wailsFrameLog{}
		app.emitFn = log.append
		app.startDelivery()
		defer app.closeDelivery()
		app.seedPresented("A") // presentation current is the removal target A
		app.setCurrentSessionID("A")

		app.emitSessionFrame("A", "token", map[string]any{"seq": 1}) // older queued frame, must drain first
		err := app.SessionArchive("A")                               // committed current removal: detach + global error atomically
		if err == nil {
			t.Fatal("committed archive of the current session = success; want its typed rejection returned")
		}
		app.enqueueBoundary("navigation", agent.HydrationState{Session: agent.SessionSummary{ID: "C"}}, "", "C") // later navigation

		frames := waitWailsFrames(t, log, 4)
		if frames[0].name != "token" {
			t.Fatalf("frame 1 = %q; the older queued frame must drain before the lifecycle pair", frames[0].name)
		}
		if got := classifyLifecycleAdvanceIndex(t, frames[1]); got != "detach" {
			t.Fatalf("frame 2 = %s (%#v), want the empty detach boundary adjacent to its error", got, frames[1])
		}
		assertUnsequencedErrorFrame(t, frames[2], err.Error()) // present ⇒ global/empty tag survived the adopted-empty presentation
		if _, ok := frames[3].payload.(agent.HydrationState); !ok || frames[3].name != "navigation" {
			t.Fatalf("frame 4 = %q %#v; want the later navigation boundary after the pair", frames[3].name, frames[3])
		}

		app.emitSessionFrame("A", "token", map[string]any{"seq": 2}) // late frame for a removed session: replaced/filtered by C's adoption
		counts := settledFrameCounts(t, log)
		assertExactSettledFrames(t, counts, map[string]int{"token": 1, "navigation": 2, "error": 1})
		if cur := app.currentSessionID(); cur != "" {
			t.Fatalf("routing current after a committed removal of the current session = %q, want cleared", cur)
		}
	})

	t.Run("target=noncurrent", func(t *testing.T) {
		svc := &lifecycleMutationSvc{out: fmt.Errorf("wrap: %w", committed)}
		app := &App{svc: svc}
		log := &wailsFrameLog{}
		app.emitFn = log.append
		app.startDelivery()
		defer app.closeDelivery()
		app.seedPresented("A") // presentation current is the unchanged A; target B sits beside it
		app.setCurrentSessionID("A")

		app.emitSessionFrame("A", "token", map[string]any{"seq": 1}) // older queued frame, must drain first
		err := app.SessionDelete("B")                                // committed noncurrent removal: nil advance + current-tag error atomically
		if err == nil {
			t.Fatal("committed delete of a noncurrent session = success; want its typed rejection returned")
		}
		app.enqueueBoundary("navigation", agent.HydrationState{Session: agent.SessionSummary{ID: "C"}}, "", "C") // later navigation

		frames := waitWailsFrames(t, log, 4)
		if frames[0].name != "token" {
			t.Fatalf("frame 1 = %q; the older queued frame must drain before the lifecycle pair", frames[0].name)
		}
		if got := classifyLifecycleAdvanceIndex(t, frames[1]); got != "nil-advance" {
			t.Fatalf("frame 2 = %s (%#v), want the nil unchanged-current advance adjacent to its error", got, frames[1])
		}
		assertUnsequencedErrorFrame(t, frames[2], err.Error()) // present ⇒ current-tagged (a removed-session tag would be filtered)
		if _, ok := frames[3].payload.(agent.HydrationState); !ok || frames[3].name != "navigation" {
			t.Fatalf("frame 4 = %q %#v; want the later navigation boundary after the pair", frames[3].name, frames[3])
		}

		app.emitSessionFrame("A", "token", map[string]any{"seq": 2}) // late frame for the old current: replaced/filtered by C's adoption
		counts := settledFrameCounts(t, log)
		assertExactSettledFrames(t, counts, map[string]int{"token": 1, "navigation": 2, "error": 1})
		if cur := app.currentSessionID(); cur != "A" {
			t.Fatalf("routing current after a committed noncurrent removal = %q, want the unchanged A", cur)
		}
	})

	// The pair must remain adjacent even when another producer races its append while the
	// drainer is stalled in an older frame. These schedules deliberately keep the old frame
	// in flight, retain a competing global frame either before or after the lifecycle pair,
	// and append a later navigation only after the pair. The queue probes use deliveryMu and
	// deliveryFrames only as test barriers: no production hook or timing assumption decides
	// which producer wins.
	for _, tc := range []struct {
		name          string
		competeBefore bool
	}{
		{name: "competing_frame_before_pair", competeBefore: true},
		{name: "competing_frame_after_pair", competeBefore: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &lifecycleMutationSvc{out: fmt.Errorf("wrap: %w", committed)}
			app := &App{svc: svc}
			log := &wailsFrameLog{}
			entered := make(chan struct{})
			release := make(chan struct{})
			var first sync.Once
			app.emitFn = func(name string, payload any) {
				first.Do(func() {
					close(entered) // the old frame is now stalled inside the framework emit
					<-release
				})
				log.append(name, payload)
			}
			app.startDelivery()
			defer app.closeDelivery()
			app.seedPresented("A")
			app.setCurrentSessionID("A")

			app.emitSessionFrame("A", "old", map[string]any{"seq": 1})
			<-entered // deterministic barrier: the old queued frame is in-flight and blocks the drainer

			var competeDone chan struct{}
			if tc.competeBefore {
				competeDone := make(chan struct{})
				go func() {
					app.emitFrame("busy", map[string]any{"state": "still-running"})
					close(competeDone)
				}()
				<-competeDone // the competing producer has appended before the lifecycle producer starts
			} else {
				competeDone = make(chan struct{})
				pairQueued := make(chan struct{})
				go func() {
					<-pairQueued // the pair has been observed adjacent under deliveryMu; this producer cannot precede it
					app.emitFrame("busy", map[string]any{"state": "still-running"})
					close(competeDone)
				}()
				pairDone := make(chan error, 1)
				go func() { pairDone <- app.SessionArchive("A") }()
				waitForQueuedLifecyclePair(t, app)
				close(pairQueued)
				pairErr := <-pairDone
				if pairErr == nil {
					t.Fatal("committed archive returned success; want its typed rejection")
				}
				<-competeDone
				app.enqueueBoundary("navigation", agent.HydrationState{Session: agent.SessionSummary{ID: "C"}}, "", "C")
				close(release)
				frames := waitWailsFrames(t, log, 5)
				if frames[0].name != "old" || frames[1].name != "navigation" || frames[3].name != "busy" || frames[4].name != "navigation" {
					t.Fatalf("after-pair order = %q, %q, %q, %q, %q; want old, lifecycle boundary, paired error, busy, later navigation", frames[0].name, frames[1].name, frames[2].name, frames[3].name, frames[4].name)
				}
				assertUnsequencedErrorFrame(t, frames[2], pairErr.Error())
				return
			}

			pairDone := make(chan error, 1)
			go func() { pairDone <- app.SessionArchive("A") }()
			waitForQueuedLifecyclePair(t, app)
			pairErr := <-pairDone
			if pairErr == nil {
				t.Fatal("committed archive returned success; want its typed rejection")
			}

			if !tc.competeBefore {
				<-competeDone // for the atomic implementation this can only happen after the complete pair is queued
			}

			app.enqueueBoundary("navigation", agent.HydrationState{Session: agent.SessionSummary{ID: "C"}}, "", "C")
			close(release) // release the old frame; the sole drainer now proves the queued order

			frames := waitWailsFrames(t, log, 5)
			if frames[0].name != "old" {
				t.Fatalf("frame 1 = %q, want the stalled old frame before all later producers", frames[0].name)
			}
			pairAt := 1
			if tc.competeBefore {
				if frames[1].name != "busy" || frames[2].name != "navigation" {
					t.Fatalf("frames 2-3 = %q, %q; want competing busy before the lifecycle pair", frames[1].name, frames[2].name)
				}
				pairAt = 2
			} else {
				if frames[1].name != "navigation" || frames[3].name != "busy" {
					t.Fatalf("frames 2/4 = %q, %q; want lifecycle pair before competing busy", frames[1].name, frames[3].name)
				}
			}
			if got := classifyLifecycleAdvanceIndex(t, frames[pairAt]); got != "detach" {
				t.Fatalf("pair boundary at frame %d = %s (%#v), want the current-detach boundary", pairAt+1, got, frames[pairAt])
			}
			assertUnsequencedErrorFrame(t, frames[pairAt+1], pairErr.Error())
			if tc.competeBefore && frames[pairAt+2].name != "navigation" {
				t.Fatalf("frame after lifecycle pair = %q, want the later navigation after the pair", frames[pairAt+2].name)
			}
			if !tc.competeBefore && frames[pairAt+2].name != "busy" {
				t.Fatalf("frame after lifecycle pair = %q, want the competing frame after the pair", frames[pairAt+2].name)
			}
			if frames[4].name != "navigation" {
				t.Fatalf("frame 5 = %q, want later navigation after the lifecycle pair and competitor", frames[4].name)
			}
		})
	}
}

// waitForQueuedLifecyclePair is a deterministic producer barrier. The stalled old frame
// keeps the drainer from consuming the queue, so seeing navigation+error adjacent under
// deliveryMu proves enqueueBoundaryWithError appended the complete unit before the next
// producer is allowed to proceed.
func waitForQueuedLifecyclePair(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.deliveryMu.Lock()
		found := false
		for i := 0; i+1 < len(app.deliveryFrames); i++ {
			if app.deliveryFrames[i].kind == frameAdvance && app.deliveryFrames[i].name == "navigation" && app.deliveryFrames[i+1].name == "error" {
				found = true
				break
			}
		}
		app.deliveryMu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the lifecycle boundary+error pair to be queued")
		}
		time.Sleep(time.Millisecond)
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
