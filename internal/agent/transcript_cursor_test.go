package agent

import (
	"context"
	"testing"
	"time"
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
}
