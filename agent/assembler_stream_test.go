package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleReadErrorLiveContext pins a non-EOF read failure on an accepted stream under a live run context: the output is errored with non-empty detail, eligible partial content accumulated before the failure is retained, and close still lands exactly once.
func TestAssembleReadErrorLiveContext(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(errors.New("boom")))
	expectStatus(t, out, model.OutputErrored)
	assertSingleClose(t, s)

	parts := msgContent(out)
	// A read error must not discard content accumulated before it fired.
	testassert.Require(t, len(parts) == 1 && parts[0].Text == "x", "read-error output retained parts = %#v, want one text part %q", out.Message, "x")
}

// TestAssembleReadErrorCancelledContext pins the same read failure observed while the run context was already cancelled: classification flips to interrupted (not errored) with non-empty detail, and partial retention follows the ordinary tool-call-free rules in both payload-less and payload-carrying shapes.
func TestAssembleReadErrorCancelledContext(t *testing.T) {
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
	}{
		{"no-payload", []func() (model.StreamDelta, error){errStep(errors.New("boom"))}},
		{"with-payload", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), errStep(errors.New("boom"))}}, // second row: full text payload accumulated BEFORE the read failure fires — finalization must retain that eligible partial content alongside its interrupted classification
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background()) // build a fresh cancellable run context
			cancel()

			out, s := assemble(t, ctx, testRef, tc.steps...)
			expectStatus(t, out, model.OutputInterrupted) // A cancelled read failure classifies as interrupted, not errored.
			assertSingleClose(t, s)

			if tc.name == "no-payload" {
				testassert.Require(t, out.Message == nil, "no-payload interrupted output unexpectedly carried a message: %#v", out.Message)
			} else {
				parts := msgContent(out)
				// Interrupted classification must not discard the pre-failure payload.
				testassert.Require(t, len(parts) == 1 && parts[0].Text == "x", "interrupted output retained parts = %#v, want one text part %q", out.Message, "x")
			}
		})
	}
}

// TestEstablishmentProtection pins the establishment rule end to end: once a successful explicit finish/payload combination was observed, later read errors and cancellation no longer downgrade completion — semantic conflicts are the only things that still can.
func TestEstablishmentProtection(t *testing.T) {
	cases := []struct {
		name   string
		steps  []func() (model.StreamDelta, error)
		cancel bool
	}{
		{"live-ctx", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), errStep(errors.New("late-boom"))}, false},     // The run context stays live through the stream.
		{"cancelled-ctx", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), errStep(errors.New("late-boom"))}, true}, // Cancellation still loses to an established completion.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			out, s := assemble(t, ctx, testRef, tc.steps...)
			expectStatus(t, out, model.OutputCompleted)
			assertSingleClose(t, s)

			parts := msgContent(out)
			// Protected completion retains its full accumulated content.
			testassert.Require(t, len(parts) == 1 && parts[0].Text == "x", "protected completion retained parts = %#v, want one text part %q", out.Message, "x")
		})
	}
}

// TestAssembleUsagePropagation pins usage value semantics end to end: last reported chunk wins by value replacement (even when it arrives after the finish reason as a choice-less trailing delta), and streams that never report any usage yield nil on their outputs.
func TestAssembleUsagePropagation(t *testing.T) {
	t.Run("last-reported-wins", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), usageStep(model.Usage{InputTokens: 1, OutputTokens: 2}), deltaStep(stopDelta()), usageStep(model.Usage{InputTokens: 3, CachedInputTokens: 4, OutputTokens: 5}))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s)

		testassert.Require(t, out.Usage != nil, "completed output carries nil usage despite reported chunks")
		want := model.Usage{InputTokens: 3, CachedInputTokens: 4, OutputTokens: 5}
		testassert.Equal(t, *out.Usage, want)
	})

	t.Run("never-reported-stays-nil", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s)

		if out.Usage != nil { // a NIL usage pointer MUST hold
			t.Fatalf("completed output unexpectedly carried usage: %#v", *out.Usage)
		}
	})
}

// TestAssembleCancelledReadErrorDropsIncompleteCalls pins interruption classification ahead of the incomplete-tool-call final validation: when no completed output was established and a non-EOF read fails while the run context is done, the output is interrupted, eligible assistant content is retained, and every tool-call block is discarded.
func TestAssembleCancelledReadErrorDropsIncompleteCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so the cancellation observation at finalization is fully deterministic.

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "half"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})),                   // identity-only arrival: its name never completes.
		deltaStep(toolDelta(model.ToolCallFragment{ArgumentFragment: `{"x":`})), // anonymous continuation keeps that same slot structurally incomplete.
		errStep(errors.New("boom")),                                             // non-EOF read failure observed while the run context is already done.
	)

	expectStatus(t, out, model.OutputInterrupted) // the incomplete call block must not outrank the interrupted classification; discarding it is the contract, erroring is not.
	assertSingleClose(t, s)

	parts := msgContent(out)
	testassert.Require(t, len(parts) == 1 && parts[0].Text == "half", "interrupted output retained parts = %#v, want one text part %q", out.Message, "half")

	if len(msgCalls(out)) != 0 {
		t.Fatalf("interrupted output unexpectedly retained tool calls: %#v", out.Message.ToolCalls)
	}
}

// TestAssembleCleanEOFBeatsPreCancellation pins normal-EOF precedence over the cancellation check: a valid no-finish-reason payload terminated by EOF completes even when the run context was already done by finalization time — only a non-EOF read failure may classify as interrupted.
func TestAssembleCleanEOFBeatsPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled BEFORE assembly begins, so EOF processing is the only thing standing between this stream and an interruption.

	out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x")))) // no finish-reason chunk; script exhaustion reports io.EOF.

	expectStatus(t, out, model.OutputCompleted) // EOF wins over the cancellation observation; no finish reason is needed for this completion shape.
	assertSingleClose(t, s)
}

// TestAssembleStopWithIncompleteCallStaysUnestablished pins that an observed incomplete tool slot blocks stop establishment: completion may only be established from a fully valid finish/payload state over every observed slot, so a cancelled non-EOF read failure still classifies as interrupted, retaining eligible partial content and discarding the unfinished call block.
func TestAssembleStopWithIncompleteCallStaysUnestablished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so the cancellation observation at finalization is fully deterministic.

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "half"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})), // identity only: the slot is observed but structurally incomplete.
		deltaStep(stopDelta()),                                // stop beside an observed slot is not yet a valid completion state.
		errStep(errors.New("boom")),
	)

	expectStatus(t, out, model.OutputInterrupted) // a bogus establishment must not route the cancelled failure through the final-state conflict instead of the interruption's call-discarding partial.
	assertSingleClose(t, s)

	parts := msgContent(out)
	testassert.Require(t, len(parts) == 1 && parts[0].Text == "half", "interrupted output retained parts = %#v, want one text part %q", out.Message, "half")

	if len(msgCalls(out)) != 0 {
		t.Fatalf("interrupted output unexpectedly retained tool calls: %#v", out.Message.ToolCalls)
	}
}

// TestAssembleEstablishmentResumesAfterSlotCompletes pins the positive sibling: a slot that completes on a later delta passes the same shared establishment check, and the established tool_calls completion is then protected from a cancelled read failure.
func TestAssembleEstablishmentResumesAfterSlotCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "x"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})),            // incomplete at this instant: no establishment.
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a", Name: "n"})), // same identity correlates back to the slot and completes it.
		deltaStep(toolCallsDelta()),                                      // the shared check now sees every observed slot complete with one valid call.
		errStep(errors.New("late-boom")),
	)

	expectStatus(t, out, model.OutputCompleted) // establishment protects the now-valid completion from the cancelled read failure.
	assertSingleClose(t, s)

	calls := msgCalls(out)
	testassert.Require(t, len(calls) == 1 && calls[0].ID == "a" && calls[0].Name == "n", "calls = %#v, want the single completed call retained", calls)
}

func TestAssembleCleanupCloseError(t *testing.T) {
	s := newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta())).withCloseError(errors.New("close-fail")) // Close deliberately fails; the receive script is complete.
	out, err := Assemble(context.Background(), testRef, s)

	testassert.NoError(t, err)

	expectStatus(t, out, model.OutputCompleted) // A close failure must not alter the classification derived from the consumed wire data.
	assertSingleClose(t, s)
}

// TestAssembleRefusalAccumulationAndRetention pins refusal fragments concatenating in arrival order without separators: a stop-completed response carries the full joined string on its message, and an interrupted stream with no other payload retains it as eligible partial content.
func TestAssembleRefusalAccumulationAndRetention(t *testing.T) {
	t.Run("completed-carries-joined-refusal", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(model.StreamDelta{HasChoice: true, RefusalFragment: "I cannot"}), deltaStep(choiceDelta(txtPos(0, ""))), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted) // A refusal alone satisfies the completed-output payload rule.
		assertSingleClose(t, s)

		testassert.Require(t, out.Message != nil && msgRefusal(out) == "I cannot", "completed output refusal = %q, want %q", msgRefusal(out), "I cannot")
	})

	t.Run("interrupted-retains-refusal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		out, s := assemble(t, ctx, testRef, deltaStep(model.StreamDelta{HasChoice: true, RefusalFragment: "nope"}), errStep(errors.New("boom")))
		expectStatus(t, out, model.OutputInterrupted) // A cancelled read failure classifies as interrupted.
		assertSingleClose(t, s)

		testassert.Require(t, out.Message != nil && msgRefusal(out) == "nope", "interrupted output retained refusal = %q, want %q", msgRefusal(out), "nope")
	})
}

// TestAssembleAcceptedProtocolReadError pins that accepted-stream protocol failures (read errors wrapping model.ErrProtocol) flow through the same read-error classification as every other non-EOF failure: live context classifies errored, cancelled context interrupted — no special-casing of this error class anywhere in assembly itself.
func TestAssembleAcceptedProtocolReadError(t *testing.T) {
	protoErr := fmt.Errorf("%w: malformed sse framing on event 3", model.ErrProtocol)
	t.Run("live-context-errored", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(protoErr))
		expectStatus(t, out, model.OutputErrored) // A protocol read failure under a live context is an ordinary stream error.
		assertSingleClose(t, s)
	})

	t.Run("cancelled-context-interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(protoErr))
		expectStatus(t, out, model.OutputInterrupted) // The same failure under an already-cancelled context is an interruption.
		assertSingleClose(t, s)
	})
}

// TestEstablishedCompletionRevalidatedAtFinalState pins the final-state re-validation rule for established outputs: a later read error or cancellation preserves completion ONLY while the finally accumulated semantic state still matches the finish reason that established it — tool calls landing after a stop establishment are exactly such a mismatch, so both protection variants must flip to errored instead of trusting the earlier flag.
func TestEstablishedCompletionRevalidatedAtFinalState(t *testing.T) {
	cases := []struct {
		name   string
		cancel bool // true pre-cancels the run context before assembly begins — establishing protection must survive cancellation ONLY when final state still agrees with the established finish reason
	}{
		{"read-error-after-stop-with-late-call", false},  // The run context stays live through the stream.
		{"cancellation-after-stop-with-late-call", true}, // Cancellation does not rescue a final state that contradicts its own finish reason.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), deltaStep(toolDelta(toolFrag("a", "fnA"))), errStep(errors.New("late-boom"))) // A tool call lands after completion was established by stop.
			expectStatus(t, out, model.OutputErrored)                                                                                                                                         // The final state contradicts its finish reason, so completion must not be preserved.
			assertSingleClose(t, s)

			parts := msgContent(out)
			testassert.Require(t, len(parts) == 1 && parts[0].Text == "x", "errored output retained parts = %#v, want one text part %q", out.Message, "x")
			testassert.Require(t, len(msgCalls(out)) == 0, "errored output unexpectedly retained tool calls: %#v", out.Message)
		})
	}
}
