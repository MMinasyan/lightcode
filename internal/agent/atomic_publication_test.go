package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/prompt"
)

// TestTurnEndEmitsUnderRuntimeLockWithEnqueueOnlyCallback proves a real turn's
// terminal event, now enqueued while runtime.mu is held, does not stall the turn
// when the callback is enqueue-only. A callback that re-entered the owner under
// that lock would deadlock; the adapters instead apply the refresh payload the
// producer carried on the event, so this callback only records the delivery.
func TestTurnEndEmitsUnderRuntimeLockWithEnqueueOnlyCallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hi")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); a.ShutdownOwner() })

	var mu sync.Mutex
	sawTurnEnd := false
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventTurnEnd {
			mu.Lock()
			sawTurnEnd = true
			mu.Unlock()
		}
	})
	a.Init(ctx)

	if _, err := a.Submit(ctx, "hello"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("turn never completed: the terminal emit under runtime.mu deadlocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawTurnEnd {
		t.Fatal("turn_end was never delivered")
	}
}

// TestAtomicLifecyclePublication proves each captured class publishes its
// event inside the state lock the navigation capture holds across its boundary
// append. A producer that runs during the capture blocks on that lock, so its
// event is absent from the snapshot and delivered after the boundary; a producer
// that runs before the capture is in the snapshot and delivered before the
// boundary. The snapshot is built before the boundary callback runs, so a producer
// started inside the callback mutates only after the snapshot is fixed.
func TestAtomicLifecyclePublication(t *testing.T) {
	ref := coremodel.ModelRef{Provider: "p", Model: "m"}

	// usage(tokensMu): a usage event produced during the capture is delivered after
	// the boundary and is absent from the snapshot's cumulative report.
	t.Run("usage=blocked_producer_after_boundary_absent_from_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventUsage {
				mu.Lock()
				order = append(order, "usage")
				mu.Unlock()
			}
		})

		snapshotInput := -1
		done := make(chan struct{})
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotInput = st.tokens.Total.Input
			go func() {
				a.recordUsageForSession(unit, loop.Event{Model: "m", ModelRef: ref, UsageKnown: true, Input: 7})
				close(done)
			}()
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		<-done

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "usage" {
			t.Fatalf("delivery order = %v, want [boundary usage]", got)
		}
		if snapshotInput != 0 {
			t.Fatalf("snapshot tokens.Total.Input = %d, want 0 (the usage produced during the capture is not in the snapshot)", snapshotInput)
		}
	})

	// usage: a usage event produced before the capture is in the snapshot and
	// delivered before the boundary.
	t.Run("usage=producer_before_capture_in_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventUsage {
				mu.Lock()
				order = append(order, "usage")
				mu.Unlock()
			}
		})
		a.recordUsageForSession(unit, loop.Event{Model: "m", ModelRef: ref, UsageKnown: true, Input: 9})

		snapshotInput := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotInput = st.tokens.Total.Input
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "usage" || got[1] != "boundary" {
			t.Fatalf("delivery order = %v, want [usage boundary]", got)
		}
		if snapshotInput != 9 {
			t.Fatalf("snapshot tokens.Total.Input = %d, want 9", snapshotInput)
		}
	})

	// warnings(warningsMu): a warning mutation during the capture is delivered after
	// the boundary and absent from the snapshot.
	t.Run("warning=blocked_producer_after_boundary_absent_from_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventWarning {
				mu.Lock()
				order = append(order, "warning")
				mu.Unlock()
			}
		})

		snapshotWarnings := -1
		done := make(chan struct{})
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotWarnings = len(st.warnings)
			go func() {
				a.setWarningGroup("protocol", []prompt.Warning{{Kind: "k", Message: "blocked"}})
				close(done)
			}()
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		<-done

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "warning" {
			t.Fatalf("delivery order = %v, want [boundary warning]", got)
		}
		if snapshotWarnings != 0 {
			t.Fatalf("snapshot warnings = %d, want 0", snapshotWarnings)
		}
	})

	// permission(gateMu): registration inserts the request and enqueues its event in
	// one gate-mutex section, so a request that starts during the capture blocks on
	// the gate lock the capture holds — absent from the snapshot and its event
	// delivered after the boundary, never split. If the capture released the gate
	// lock before the boundary, the registration could land during the capture,
	// failing the pending-during-capture and delivery-order assertions.
	t.Run("permission=blocked_during_capture_absent_from_snapshot_delivered_after", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		sessionID := sessionIDOf(unit)
		permCtx, permCancel := context.WithCancel(context.Background())
		defer permCancel()
		defer a.gate.CancelAll()

		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventPermissionRequest {
				mu.Lock()
				order = append(order, "permission")
				mu.Unlock()
			}
		})

		// The registering goroutine pauses at the barrier — before it takes the gate
		// mutex — until the capture holds gateMu, so its registration provably blocks
		// on the held lock instead of racing the capture's read.
		atBarrier := make(chan struct{})
		proceed := make(chan struct{})
		a.gate.RegisterBarrier = func() {
			close(atBarrier)
			<-proceed
		}
		go func() {
			a.gate.AskRequest(permCtx, permission.Request{SessionID: sessionID, ToolName: "write_file", Arg: "x"})
		}()
		<-atBarrier

		snapshotPerms := -1
		pendingDuringCapture := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			snapshotPerms = len(st.permissions)
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			// Release the registering goroutine: its gate lock now blocks on the one
			// this capture holds. Yield so it parks in Lock, then confirm it cannot
			// have registered while the boundary is appended.
			close(proceed)
			for i := 0; i < 100; i++ {
				goruntime.Gosched()
			}
			pendingDuringCapture = len(a.gate.PendingForSessionLocked(sessionID))
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for len(a.gate.PendingForSession(sessionID)) == 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if snapshotPerms != 0 {
			t.Fatalf("snapshot permissions = %d, want 0", snapshotPerms)
		}
		if pendingDuringCapture != 0 {
			t.Fatalf("pending during the capture = %d, want 0 (the registration is blocked on the held gate lock)", pendingDuringCapture)
		}
		if n := len(a.gate.PendingForSession(sessionID)); n != 1 {
			t.Fatalf("pending after the capture = %d, want 1 (registered after the boundary, not lost)", n)
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "permission" {
			t.Fatalf("delivery order = %v, want [boundary permission] (the request event is enqueued under gateMu after the boundary)", got)
		}
	})

	// transcript(seqMu): a transcript row fed during the capture blocks on seqMu the
	// capture holds, so it is absent from the tail snapshot and delivered after the
	// boundary.
	t.Run("transcript=blocked_producer_after_boundary_absent_from_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventTextDelta && ev.Result == "blocked-row" {
				mu.Lock()
				order = append(order, "row")
				mu.Unlock()
			}
		})

		snapshotTail := -1
		done := make(chan struct{})
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotTail = len(st.transcript.tail)
			go func() {
				a.feedAndEmit(tr, Event{Kind: EventTextDelta, SessionID: sessionIDOf(unit), Result: "blocked-row"})
				close(done)
			}()
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		<-done

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "row" {
			t.Fatalf("delivery order = %v, want [boundary row]", got)
		}
		if snapshotTail != 0 {
			t.Fatalf("snapshot tail = %d, want 0 (the row fed during the capture is not in the snapshot)", snapshotTail)
		}
	})

	// transcript: a row fed before the capture is in the tail snapshot and delivered
	// before the boundary.
	t.Run("transcript=producer_before_capture_in_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventTextDelta && ev.Result == "before-row" {
				mu.Lock()
				order = append(order, "row")
				mu.Unlock()
			}
		})
		a.feedAndEmit(tr, Event{Kind: EventTextDelta, SessionID: sessionIDOf(unit), Result: "before-row"})

		snapshotTail := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotTail = len(st.transcript.tail)
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "row" || got[1] != "boundary" {
			t.Fatalf("delivery order = %v, want [row boundary]", got)
		}
		if snapshotTail == 0 {
			t.Fatal("snapshot tail = 0, want the row fed before the capture")
		}
	})

	// session error(seqMu): an error fed during the capture blocks on seqMu the
	// capture holds, so it is absent from the retained-error snapshot and delivered
	// after the boundary.
	t.Run("session_error=blocked_producer_after_boundary_absent_from_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventError && ev.Error == "blocked-error" {
				mu.Lock()
				order = append(order, "error")
				mu.Unlock()
			}
		})

		snapshotErrors := -1
		done := make(chan struct{})
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotErrors = len(st.transcript.errors)
			go func() {
				a.feedAndEmit(tr, Event{Kind: EventError, SessionID: "err-session", Error: "blocked-error", Turn: 1})
				close(done)
			}()
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		<-done

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "error" {
			t.Fatalf("delivery order = %v, want [boundary error]", got)
		}
		if snapshotErrors != 0 {
			t.Fatalf("snapshot errors = %d, want 0 (the error fed during the capture is not in the snapshot)", snapshotErrors)
		}
	})

	// session error: an error fed before the capture is in the retained-error
	// snapshot and delivered before the boundary.
	t.Run("session_error=producer_before_capture_in_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventError && ev.Error == "before-error" {
				mu.Lock()
				order = append(order, "error")
				mu.Unlock()
			}
		})
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: "err-session", Error: "before-error", Turn: 1})

		snapshotErrors := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotErrors = len(st.transcript.errors)
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "error" || got[1] != "boundary" {
			t.Fatalf("delivery order = %v, want [error boundary]", got)
		}
		if snapshotErrors == 0 {
			t.Fatal("snapshot errors = 0, want the error fed before the capture")
		}
	})

	// warning: a warning mutated before the capture is in the snapshot and delivered
	// before the boundary.
	t.Run("warning=producer_before_capture_in_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventWarning {
				mu.Lock()
				order = append(order, "warning")
				mu.Unlock()
			}
		})
		a.setWarningGroup("protocol", []prompt.Warning{{Kind: "k", Message: "before"}})

		snapshotWarnings := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotWarnings = len(st.warnings)
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "warning" || got[1] != "boundary" {
			t.Fatalf("delivery order = %v, want [warning boundary]", got)
		}
		if snapshotWarnings == 0 {
			t.Fatal("snapshot warnings = 0, want the warning mutated before the capture")
		}
	})

	// permission: a request registered before the capture is in the snapshot.
	t.Run("permission=registered_before_capture_in_snapshot", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		sessionID := sessionIDOf(unit)
		permCtx, permCancel := context.WithCancel(context.Background())
		defer permCancel()
		defer a.gate.CancelAll()

		go a.gate.AskRequest(permCtx, permission.Request{SessionID: sessionID, ToolName: "write_file", Arg: "x"})
		deadline := time.Now().Add(2 * time.Second)
		for len(a.gate.PendingForSession(sessionID)) == 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}

		snapshotPerms := -1
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			snapshotPerms = len(st.permissions)
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		if snapshotPerms != 1 {
			t.Fatalf("snapshot permissions = %d, want 1 (registered before the capture)", snapshotPerms)
		}
	})

	// busy/turn(runtime.mu): the capture reads busy under runtime.mu, so a producer
	// clearing busy during the capture is serialized after the boundary. The
	// snapshot reflects the pre-clear busy state.
	t.Run("busy=capture_reads_busy_before_concurrent_clear", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit.busy = true
		rt.mu.Unlock()

		var mu sync.Mutex
		var order []string
		a.SetEventHandler(func(ev Event) {
			if ev.Kind == EventTurnEnd {
				mu.Lock()
				order = append(order, "turn_end")
				mu.Unlock()
			}
		})

		snapshotBusy := false
		done := make(chan struct{})
		if _, err := a.captureStateForSelection(unit, func(st completeState) {
			mu.Lock()
			order = append(order, "boundary")
			mu.Unlock()
			snapshotBusy = st.busy
			go func() {
				// Mirror the turn-end atomic section: clear busy and enqueue turn_end
				// in one runtime.mu section. It blocks on runtime.mu the capture holds.
				rt.mu.Lock()
				unit.busy = false
				a.emitEvent(Event{Kind: EventTurnEnd, SessionID: sessionIDOf(unit), Turn: 1})
				rt.mu.Unlock()
				close(done)
			}()
		}); err != nil {
			t.Fatalf("captureStateForSelection: %v", err)
		}
		<-done

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != "boundary" || got[1] != "turn_end" {
			t.Fatalf("delivery order = %v, want [boundary turn_end]", got)
		}
		if !snapshotBusy {
			t.Fatal("snapshot busy = false, want true (the busy clear is serialized after the boundary)")
		}
	})
}
