package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// ackFlush consumes one loop-flush request and acknowledges it, acting as the
// loop event drainer for tests that never start the owner's background
// goroutines.
func ackFlush(rt *runtime) {
	go func() {
		if done, ok := <-rt.loopFlush; ok {
			close(done)
		}
	}()
}

// TestFlushAndCommitTranscript verifies the shared flush-and-commit helper's
// context contract: it takes no caller context, so an unrelated cancelled
// context in the caller's scope cannot skip the commit; it gives up on the
// owner's lifecycle context, so a shutting-down owner cannot block the caller
// forever; and a runtime built without an owner context (a bare newRuntime in a
// test, Init never called) must not panic.
//
// Exception, named per the contract-test rule: flushAndCommitTranscript is an
// unexported runtime method, not adapter-facing — package-level tests, with
// the acting drainer standing in for the owner's drainLoopEvents goroutine.
func TestFlushAndCommitTranscript(t *testing.T) {
	// An already-cancelled context sitting in the caller's scope must not
	// change the outcome: the helper takes no context, so the flush runs and
	// the commit lands.
	t.Run("caller_context_is_ignored", func(t *testing.T) {
		rt := newRuntime(nil, runtimeOptions{})
		rt.ownerCtx, rt.ownerCancel = context.WithCancel(context.Background())
		rt.transcriptState["s1"] = &transcriptCursor{coord: newTranscript()}
		ackFlush(rt)

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_ = cancelled

		rt.flushAndCommitTranscript("s1", 1)
		tr := rt.transcriptState["s1"].coord
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		committedSeq := tr.committedSeq
		tr.seqMu.Unlock()
		if committedTurn != 1 {
			t.Fatalf("committedTurn = %d, want 1 (the caller's cancelled context must not skip the commit)", committedTurn)
		}
		if committedSeq != 0 {
			t.Fatalf("committedSeq = %d, want 0 (nextSeq-1 with an empty tail)", committedSeq)
		}
	})

	// Once the owner's context is done, the wait returns rather than hanging:
	// a shutting-down owner cannot block the caller forever. The commit does
	// not land, because the flush was never acknowledged.
	t.Run("owner_context_done_returns", func(t *testing.T) {
		rt := newRuntime(nil, runtimeOptions{})
		rt.ownerCtx, rt.ownerCancel = context.WithCancel(context.Background())
		rt.transcriptState["s1"] = &transcriptCursor{coord: newTranscript()}
		rt.ownerCancel()

		done := make(chan struct{})
		go func() {
			rt.flushAndCommitTranscript("s1", 1)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("flushAndCommitTranscript hung after the owner context was done")
		}
		tr := rt.transcriptState["s1"].coord
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != 0 {
			t.Fatalf("committedTurn = %d, want 0 (an owner shut down mid-flight must not commit)", committedTurn)
		}
	})

	// A call naming turn 0 names a turn that was never begun: BeginTurn
	// returns 0 only for an inactive session, and committing it would feed
	// EventTurnEnd{Turn: 0} into commitLocked, wiping the tail and regressing
	// the committed markers. The call must return before issuing any flush
	// request, leaving the previous turn's commit and the uncommitted tail
	// untouched.
	t.Run("turn_zero_is_never_committed", func(t *testing.T) {
		rt := newRuntime(nil, runtimeOptions{})
		rt.ownerCtx, rt.ownerCancel = context.WithCancel(context.Background())
		rt.transcriptState["s1"] = &transcriptCursor{coord: newTranscript()}

		tr := rt.transcriptForSessionID("s1")
		// Commit turn 1, then leave a row uncommitted in the tail, so a
		// commit of turn 0 is distinguishable from doing nothing.
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: "s1", Turn: 1})
		feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: "s1", Result: "survivor"})

		// No flush acknowledger is started: a zero-turn call must not issue a
		// flush request at all. Absence is observed only after the call has
		// returned — loopFlush is buffered with capacity one, so a request can
		// sit in the buffer with the call already returned. A call that issued
		// a flush and waited on it would block forever without an acknowledger,
		// so the wait below doubles as the failure for that placement.
		done := make(chan struct{})
		go func() {
			rt.flushAndCommitTranscript("s1", 0)
			close(done)
		}()

		select {
		case <-done:
			// The call returned.
		case <-time.After(5 * time.Second):
			rt.ownerCancel()
			t.Fatal("flushAndCommitTranscript did not return for a turn that was never begun")
		}

		// The call has returned; nothing may be sitting in the flush channel.
		select {
		case req := <-rt.loopFlush:
			t.Fatalf("flushAndCommitTranscript issued a flush request for a turn that was never begun (%v)", req)
		default:
		}

		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tailLen := len(tr.tail)
		tr.seqMu.Unlock()
		if committedTurn != 1 {
			t.Fatalf("committedTurn = %d, want 1 (a turn that was never begun must not commit)", committedTurn)
		}
		if tailLen != 1 {
			t.Fatalf("tail has %d rows, want 1 (an unbegun turn must not wipe the uncommitted tail)", tailLen)
		}
	})

	// A runtime built without an owner context must not panic, and the flush
	// round-trip still completes: the nil context simply never fires.
	t.Run("nil_owner_context_does_not_panic", func(t *testing.T) {
		rt := newRuntime(nil, runtimeOptions{})
		rt.transcriptState["s1"] = &transcriptCursor{coord: newTranscript()}
		ackFlush(rt)

		rt.flushAndCommitTranscript("s1", 1)
		tr := rt.transcriptState["s1"].coord
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != 1 {
			t.Fatalf("committedTurn = %d, want 1 (the flush round-trip completed and committed)", committedTurn)
		}
	})

	// After flushAndCommitTranscript, the turn is durable through the
	// committed bound: a live hydration renders its row exactly once from the
	// durable half, the tail is empty, and the cursor suppresses replay. This
	// is the other half of the coordinator-commit contract — the marker on
	// disk alone must not make the turn visible (TestLiveHydrationUsesCommittedTurn),
	// and the flush round-trip must.
	t.Run("committed_turn_renders_once_after_flush", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		rt := a.ensureRuntime()
		ackFlush(rt)

		turn := unit.store.BeginTurn()
		feedTranscript(a.transcriptForSessionID(id), Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(a.transcriptForSessionID(id), Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "flushed row"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "flushed row"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		rt.flushAndCommitTranscript(id, turn)

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if got := countHydrationContent(hs, "flushed row"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the flush-and-commit made the turn durable through the bound)", "flushed row", got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("hydration has %d tail rows, want none (the commit cleared the tail)", len(hs.Tail))
		}
		if hs.Cursor.CommittedTurn != turn {
			t.Fatalf("cursor committedTurn = %d, want %d", hs.Cursor.CommittedTurn, turn)
		}
		if hs.Cursor.CommittedSeq == 0 {
			t.Fatal("cursor committedSeq = 0, want the committed row's sequence (committedSeq suppresses replay)")
		}
	})
}

// TestRegisterTranscriptReadsStoreBeforeLock is a structural regression for
// the registration lock/IO order: the coordinator's bound read
// (store.HighestCompleteTurn) must precede taking transcriptMu, per the
// plan's lock rule — registration never reads store state while holding
// transcript locks. A structural assertion is used because the store read is
// unobservable from outside the coordinator; the same pattern the preseed-rearm
// lock-order regression uses.
func TestRegisterTranscriptReadsStoreBeforeLock(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	const marker = "func (rt *runtime) registerTranscript"
	start := bytes.Index(src, []byte(marker))
	if start < 0 {
		t.Fatalf("registerTranscript not found in runtime.go")
	}
	body := src[start:]
	readAt := bytes.Index(body, []byte("HighestCompleteTurn()"))
	lockAt := bytes.Index(body, []byte("transcriptMu.Lock()"))
	if readAt < 0 {
		t.Fatalf("no HighestCompleteTurn read in registerTranscript")
	}
	if lockAt < 0 {
		t.Fatalf("no transcriptMu.Lock in registerTranscript")
	}
	if readAt > lockAt {
		t.Fatalf("store read (byte %d) must precede transcriptMu.Lock (byte %d) in registerTranscript", readAt, lockAt)
	}
}

// TestRegisterTranscriptPreservesExistingAndSeedsCompleteTurn covers the two
// registration behaviors behaviorally: re-registering a live id keeps the
// existing coordinator (committed markers and retained tail untouched), and a
// fresh registration over a loaded store seeds the store's highest complete
// turn as the new coordinator's committed bound.
func TestRegisterTranscriptPreservesExistingAndSeedsCompleteTurn(t *testing.T) {
	t.Run("re_registration_preserves_coordinator_state", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		id := sessionIDOf(a.session)
		rt := a.ensureRuntime()
		tr := a.transcriptForSessionID(id)

		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, Turn: 1})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: 1, Result: "keep me"})
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, Turn: 1})
		feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "tail row"})

		rt.registerTranscript(id, a.session.store)

		if got := a.transcriptForSessionID(id); got != tr {
			t.Fatal("re-registration replaced the existing coordinator")
		}
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		committedSeq := tr.committedSeq
		tailLen := len(tr.tail)
		tr.seqMu.Unlock()
		if committedTurn != 1 || committedSeq != 1 {
			t.Fatalf("re-registration regressed the committed markers: turn=%d seq=%d, want 1/1", committedTurn, committedSeq)
		}
		if tailLen != 1 {
			t.Fatalf("re-registration dropped the retained tail: %d rows, want 1", tailLen)
		}
	})

	t.Run("fresh_registration_seeds_highest_complete_turn", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()
		first := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		if _, err := first.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		id := first.SessionCurrent().ID
		for i := 1; i <= 3; i++ {
			appendUserTurn(t, first, fmt.Sprintf("turn %d", i))
		}
		proj, err := first.projects.Ensure()
		if err != nil {
			t.Fatalf("ensure project: %v", err)
		}
		sessionsRoot := first.projects.SessionsRoot(proj.ID)
		if _, err := first.store.Close(); err != nil {
			t.Fatalf("release first owner claim: %v", err)
		}

		// A fresh registration over the loaded store adopts its highest
		// complete turn — never the current (in-flight) turn.
		store, err := snapshot.NewForSessionsRoot(sessionsRoot, first.projects.Root(), proj.ID)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		if err := store.LoadSession(id); err != nil {
			t.Fatalf("LoadSession: %v", err)
		}
		rt := newRuntime(nil, runtimeOptions{})
		rt.registerTranscript("reopened", store)

		tr := rt.transcriptState["reopened"].coord
		tr.seqMu.Lock()
		got := tr.committedTurn
		gotSeq := tr.committedSeq
		gotNext := tr.nextSeq
		tr.seqMu.Unlock()
		if got != 3 {
			t.Fatalf("fresh registration seeded committedTurn = %d, want 3 (the store's highest complete turn)", got)
		}
		if gotSeq != 0 || gotNext != 1 {
			t.Fatalf("fresh registration markers = seq %d next %d, want 0/1 (a fresh coordinator)", gotSeq, gotNext)
		}
		store.Detach()
	})
}
