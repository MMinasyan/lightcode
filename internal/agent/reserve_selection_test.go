package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	loop "github.com/MMinasyan/lightcode/internal/engine"
)

// TestReserveSelectionSourceNoopRows proves the no-op reservation rows: an
// empty id, a non-live id, and a read-only session (one another process
// drives, so it is not live in this owner) all return a no-op release with
// no error, and navigation proceeds without any invented live-mutation guard.
func TestReserveSelectionSourceNoopRows(t *testing.T) {
	t.Run("empty=noop", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		release, err := a.ReserveSelectionSource("")
		if err != nil {
			t.Fatalf("empty reserve = %v, want no-op", err)
		}
		if release == nil {
			t.Fatal("empty reserve returned a nil release")
		}
		release() // must not panic
	})

	t.Run("non_live=noop", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		release, err := a.ReserveSelectionSource("no-such-session")
		if err != nil {
			t.Fatalf("non-live reserve = %v, want no-op", err)
		}
		release()
	})

	t.Run("read_only=noop", func(t *testing.T) {
		first, second := newSharedHomeAgentPair(t)
		held, err := first.NewSession("", "primary")
		if err != nil {
			t.Fatalf("held NewSession: %v", err)
		}
		// The held session is persisted-only for the second owner: reserving
		// it there is a no-op (it is not live in this owner).
		release, err := second.ReserveSelectionSource(held)
		if err != nil {
			t.Fatalf("read-only reserve = %v, want no-op", err)
		}
		release()
	})
}

// TestReserveSelectionSourceRefusals proves the refusal rows: a busy live
// source and an already-transitioning live source both return the existing
// mutability error and no release, leaving the owner state untouched.
func TestReserveSelectionSourceRefusals(t *testing.T) {
	t.Run("busy=refused", func(t *testing.T) {
		block := make(chan struct{})
		var once sync.Once
		releaseHold := func() { once.Do(func() { close(block) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
			writeTextResponse(w, "released")
		}))
		t.Cleanup(server.Close)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := a.SubmitToSession(ctx, id, "hold"); err != nil {
			t.Fatalf("submit hold: %v", err)
		}
		t.Cleanup(func() {
			releaseHold()
			_ = a.CancelSession(id)
		})

		release, err := a.ReserveSelectionSource(id)
		if err == nil || !strings.Contains(err.Error(), "a turn is running") {
			t.Fatalf("busy reserve = %v, want the mutability error", err)
		}
		if release != nil {
			t.Fatal("busy refusal returned a release")
		}
	})

	t.Run("already_transitioning=refused", func(t *testing.T) {
		a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		release, err := a.ReserveSelectionSource(id)
		if err != nil {
			t.Fatalf("first reserve of an idle source: %v", err)
		}
		defer release()
		second, err := a.ReserveSelectionSource(id)
		if err == nil || !strings.Contains(err.Error(), "session is closing or switching") {
			t.Fatalf("second reserve while transitioning = %v, want the mutability error", err)
		}
		if second != nil {
			t.Fatal("transitioning refusal returned a release")
		}
	})
}

// TestReserveSelectionSourceBlocksOwnerProgressUntilRelease proves the idle
// reservation's owner coverage: while a live idle source is reserved, submit
// is refused, the queue drain cannot claim it, and the signal scheduler
// cannot claim it; the release then rearms the retained queue and the pending
// wake signal without any manual nudge — endLiveTransition's own nudges drive
// the drainer and the scheduler.
func TestReserveSelectionSourceBlocksOwnerProgressUntilRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "ok")
	}))
	t.Cleanup(server.Close)

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// A queued item and a pending wake signal sit on the idle source. The
	// drain and the scheduler both require a non-transitioning unit, so the
	// reservation must hold both, and the release must rearm both.
	a.ensureRuntime().mu.Lock()
	unit := a.sessions[id]
	if unit == nil {
		a.ensureRuntime().mu.Unlock()
		t.Fatal("session not in the live map")
	}
	unit.queue = []QueuedItem{{ID: "q-1", Content: "queued while idle"}}
	unit.queueSeq = 1
	unit.queueVersion = 1
	unit.lp.AddPendingSignal(loop.PendingSignal{Wake: true, Persist: true, Payload: "wake after queue"})
	a.ensureRuntime().mu.Unlock()

	release, err := a.ReserveSelectionSource(id)
	if err != nil {
		t.Fatalf("reserve the idle source: %v", err)
	}

	// Submit is refused while the source is reserved.
	if _, err := a.SubmitToSession(ctx, id, "during reservation"); err == nil ||
		!strings.Contains(err.Error(), "session is changing") {
		t.Fatalf("submit during the reservation = %v, want the changing refusal", err)
	}

	// Neither the queue drain nor the signal scheduler can claim the source
	// while it is reserved, even when nudged.
	a.ensureRuntime().tryDrainQueue(ctx)
	a.ensureRuntime().tryStartSignalTurn(ctx)
	time.Sleep(50 * time.Millisecond)
	if got := countTurnStartsForSession(cap.snapshot(), id); got != 0 {
		t.Fatalf("reserved source started %d turns under manual nudges", got)
	}
	a.ensureRuntime().mu.Lock()
	retained := len(a.sessions[id].queue) == 1 && a.sessions[id].lp.HasPendingWakeSignal()
	a.ensureRuntime().mu.Unlock()
	if !retained {
		t.Fatal("reserved source lost its queued item or its pending wake signal")
	}

	// The release rearms both without manual nudges: endLiveTransition wakes
	// the drainer, the queued turn starts and drains, and the pending wake
	// signal is consumed.
	release()
	waitUntilTurnStartsForSession(t, cap, id, 1)
	waitUntilSessionQueueEmpty(t, a, id)
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.ensureRuntime().mu.Lock()
		pending := a.sessions[id].lp.HasPendingWakeSignal()
		a.ensureRuntime().mu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wake signal still pending after the release rearmed the scheduler")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
