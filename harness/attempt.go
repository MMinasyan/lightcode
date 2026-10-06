package harness

import (
	"context"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// attemptKind is the closed producer state of one shared physical attempt
// loop run. The loop is the state's sole producer; no result validator
// exists.
type attemptKind int

const (
	attemptAccepted    attemptKind = iota // nonnil stream, nil error
	attemptFailed                         // nil stream, the original attempt error
	attemptInterrupted                    // nil stream and nil error; the caller settles its existing interruption
	attemptInvalid                        // nil stream, the boundary model ProtocolError
)

// attemptResult is the one shared physical-attempt loop's result: the kind,
// the accepted stream when accepted, and the attempt or boundary error
// otherwise.
type attemptResult struct {
	kind   attemptKind
	stream model.Stream
	err    error
}

// runModelAttempts runs the one physical model-attempt loop shared by the
// conversation and compact model effects: check cancellation before every
// attempt, invoke the physical callback once per iteration, accept exactly
// one nonnil stream with a nil error without reclassifying a late
// cancellation, close any supplied stream once and report the boundary
// ProtocolError for an outcome that is both or neither — before the
// post-failure cancellation check — let observed cancellation win over retry
// classification, select the standard policy for a nil Retry, stop on a
// false classification or a negative delay, and run the backoff behind the
// cancellation check. An accepted stream never re-enters retry: the caller
// owns assembly and closure exactly once after acceptance.
func runModelAttempts(ctx context.Context, req model.Request, attempt func(context.Context, model.Request) (model.Stream, error), retry RetryPolicy) attemptResult {
	if retry == nil {
		retry = standardRetryPolicy
	}
	for failed := 1; ; failed++ {
		if ctx.Err() != nil { // observed cancellation before an attempt interrupts; no output and no assembly call
			return attemptResult{kind: attemptInterrupted}
		}
		stream, attemptErr := attempt(ctx, req)
		if attemptErr == nil && stream != nil {
			return attemptResult{kind: attemptAccepted, stream: stream}
		}
		if attemptErr == nil || stream != nil { // exactly one stream or one error must be returned; a supplied stream closes before the boundary failure
			if stream != nil {
				_ = stream.Close()
			}
			return attemptResult{kind: attemptInvalid, err: &agent.ProtocolError{Boundary: "model", Detail: "physical model request returned neither exactly one stream nor one error"}}
		}
		// An attempt failure observed under a done execution context
		// interrupts before any classification: regardless of the attempt
		// error's shape or the retry policy's answer, pre-acceptance
		// cancellation is an interruption with no output and no assembly
		// call.
		if ctx.Err() != nil {
			return attemptResult{kind: attemptInterrupted}
		}
		delay, again := retry(attemptErr, failed)
		if !again || delay < 0 {
			return attemptResult{kind: attemptFailed, err: attemptErr}
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done(): // cancellation during backoff interrupts; no attempt starts
			return attemptResult{kind: attemptInterrupted}
		}
	}
}
