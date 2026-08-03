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
