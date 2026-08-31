package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// cancelThen wraps a scripted return value in a run-context cancellation fired at the exact moment the boundary is inside its own invocation — the blocking-fake stand-in that pins what the next checkpoint must observe.
func cancelThen(cancel context.CancelFunc, set ModelSettlement) (ModelSettlement, error) {
	cancel()
	return set, nil
}

// TestRunCancelledAfterSettledBatchRetainsCompletedOutput pins the loop-top checkpoint reached only after a complete batch: the run's last completed output survives the cancellation intact and carries no unstarted calls — every one of them settled — and no further effect starts.
func TestRunCancelledAfterSettledBatchRetainsCompletedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = readyOutputs(callsOut)
	f.toolRes = func(call int, c model.ToolCall) (model.ToolResult, error) {
		if call == 3 {
			cancel() // the batch's final settlement coincides with cancellation: only the next checkpoint can observe it.
		}
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != callsOut {
		t.Fatalf("interruption dropped the completed output: %p vs %p", res.LastOutput, callsOut)
	}
	if len(res.UnstartedCalls) != 0 {
		t.Fatalf("unstarted = %s, want none after a fully settled batch", joinedIDs(res.UnstartedCalls))
	}
	if f.modelCalls != 1 {
		t.Fatalf("model calls = %d, want no second effect after cancellation", f.modelCalls)
	}
}

// TestRunCancelledBeforeFirstEffect pins the very first checkpoint: an already-done run context ends in a bare interruption with no caller function ever starting.
func TestRunCancelledBeforeFirstEffect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &runFakes{}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != nil || res.UnstartedCalls != nil {
		t.Fatalf("bare interruption expected, got %#v", res)
	}
	if f.sourceCalls != 0 || f.modelCalls != 0 || f.toolCalls != 0 {
		t.Fatalf("cancelled run started caller functions: source=%d model=%d tool=%d", f.sourceCalls, f.modelCalls, f.toolCalls)
	}
}

// TestRunCancelledDuringSnapshot pins the checkpoint between obtaining the fresh context and invoking the model effect: the effect must never start.
func TestRunCancelledDuringSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	f.snapshot = func(int) ([]model.Message, error) { cancel(); return userSnapshot("go"), nil }

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if f.sourceCalls != 1 || f.modelCalls != 0 || f.toolCalls != 0 {
		t.Fatalf("source=%d model=%d tool=%d, want 1/0/0", f.sourceCalls, f.modelCalls, f.toolCalls)
	}
}

// TestRunContextSourceCancellationRouting pins both halves of the source rule: a cancellation report under a genuinely done run context maps to Agent interruption, while the same-shaped report under a live context is an ordinary infrastructure error returned verbatim with no terminal.
func TestRunContextSourceCancellationRouting(t *testing.T) {
	wrapped := func() error { return fmt.Errorf("snapshot read: %w", context.Canceled) }

	t.Run("done-context-interruption", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.snapshot = func(int) ([]model.Message, error) { cancel(); return nil, wrapped() }

		res, err := Run(ctx, f.invocation())
		requireRunInterrupted(t, res, err, "agent interrupted")
		if f.modelCalls != 0 {
			t.Fatalf("model calls = %d, want 0", f.modelCalls)
		}
	})

	t.Run("live-context-infrastructure-error", func(t *testing.T) {
		f := &runFakes{}
		f.snapshot = func(int) ([]model.Message, error) { return nil, wrapped() }

		res, err := Run(context.Background(), f.invocation())
		requireZeroTerminal(t, res)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the verbatim cancellation-shaped infrastructure failure", err)
		}
		if f.modelCalls != 0 || f.toolCalls != 0 {
			t.Fatalf("later effects started: model=%d tool=%d", f.modelCalls, f.toolCalls)
		}
	})

	t.Run("deadline-under-done-context-interruption", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.snapshot = func(int) ([]model.Message, error) {
			cancel()
			return nil, fmt.Errorf("snapshot read: %w", context.DeadlineExceeded)
		}

		res, err := Run(ctx, f.invocation())
		requireRunInterrupted(t, res, err, "agent interrupted")
	})
}

// TestRunCancelledBeforeToolDispatch pins cancellation observed after a completed model output: the interruption carries that output and every one of its not-yet-dispatched calls, in message order.
func TestRunCancelledBeforeToolDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = func(call int, _ model.Request, _ AssemblyCallback) (ModelSettlement, error) {
		return cancelThen(cancel, ModelSettlement{Disposition: DispoReady, Output: callsOut})
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != callsOut {
		t.Fatalf("interruption dropped the completed output: %p vs %p", res.LastOutput, callsOut)
	}
	if got := joinedIDs(res.UnstartedCalls); got != "[a b c]" {
		t.Fatalf("unstarted = %s, want all three calls in order", got)
	}
	if f.toolCalls != 0 {
		t.Fatalf("tool calls = %d, want 0 — no effect starts after cancellation", f.toolCalls)
	}
}

// TestRunCancelledAfterSettledToolPinsLaterUnstarted pins that after a settled tool result only the LATER calls are unstarted: the already-answered call never reappears and the batch stops at the checkpoint.
func TestRunCancelledAfterSettledToolPinsLaterUnstarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = readyOutputs(callsOut)
	f.toolRes = func(call int, c model.ToolCall) (model.ToolResult, error) {
		if call == 1 {
			cancel()
		}
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != callsOut {
		t.Fatalf("interruption dropped the completed output")
	}
	if got := joinedIDs(res.UnstartedCalls); got != "[b c]" {
		t.Fatalf("unstarted = %s, want only calls after the settled one", got)
	}
	if f.toolCalls != 1 {
		t.Fatalf("tool calls = %d, want the batch to stop after call a", f.toolCalls)
	}
}

// TestRunStepSevenCancellationCarriesCompletedOutput pins the last checkpoint on a call-free response: the established completion is never discarded — the interruption carries exactly the output the run would have succeeded with.
func TestRunStepSevenCancellationCarriesCompletedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	final := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
		return cancelThen(cancel, ModelSettlement{Disposition: DispoReady, Output: final})
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != final || res.UnstartedCalls != nil {
		t.Fatalf("terminal = %#v, want the completed output carried verbatim", res)
	}
}

// TestRunInterruptedToolResultStopsBatch pins the interrupted-tool row: the batch stops immediately, the terminal detail is the fixed interruption text, the settled interrupted call is not unstarted, and only later calls are.
func TestRunInterruptedToolResultStopsBatch(t *testing.T) {
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = readyOutputs(callsOut, mkOutput(model.OutputCompleted, "", nil))
	f.toolRes = resScript(
		model.ToolResult{CallID: "a", Status: model.ResultInterrupted, Content: "stopped mid-flight"},
		model.ToolResult{CallID: "b", Status: model.ResultSuccess},
	)

	res, err := Run(context.Background(), f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != callsOut {
		t.Fatalf("interruption dropped the completed output")
	}
	if got := joinedIDs(res.UnstartedCalls); got != "[b c]" {
		t.Fatalf("unstarted = %s, want only calls after the settled interrupted one", got)
	}
	if f.toolCalls != 1 {
		t.Fatalf("tool calls = %d, want no later effect to start", f.toolCalls)
	}
}

// TestRunEffectCancellationErrorsAreProtocolViolations pins that active-effect cancellation must come back as a settlement, never as a Go error: a cancellation-shaped error out of either effect is one typed boundary-protocol error naming that effect's boundary even while the run context is done.
func TestRunEffectCancellationErrorsAreProtocolViolations(t *testing.T) {
	t.Run("model-effect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
			cancel()
			return ModelSettlement{}, fmt.Errorf("attempt died: %w", context.Canceled)
		}

		res, err := Run(ctx, f.invocation())
		requireBoundaryViolation(t, err, "model")
		requireZeroTerminal(t, res)
		if f.toolCalls != 0 {
			t.Fatalf("tool calls = %d, want 0", f.toolCalls)
		}
	})

	t.Run("model-effect-live-context", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
			return ModelSettlement{}, fmt.Errorf("attempt died: %w", context.DeadlineExceeded)
		}

		res, err := Run(context.Background(), f.invocation())
		requireBoundaryViolation(t, err, "model")
		requireZeroTerminal(t, res)
	})

	t.Run("tool-effect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.modelSet = readyOutputs(mkCallsTerminalOutput())
		f.toolRes = func(int, model.ToolCall) (model.ToolResult, error) {
			cancel()
			return model.ToolResult{}, fmt.Errorf("killed: %w", context.DeadlineExceeded)
		}

		res, err := Run(ctx, f.invocation())
		requireBoundaryViolation(t, err, "tool")
		requireZeroTerminal(t, res)
	})

	t.Run("tool-effect-live-context", func(t *testing.T) {
		f := &runFakes{}
		f.modelSet = readyOutputs(mkCallsTerminalOutput())
		f.toolRes = func(int, model.ToolCall) (model.ToolResult, error) {
			return model.ToolResult{}, fmt.Errorf("killed: %w", context.Canceled)
		}

		res, err := Run(context.Background(), f.invocation())
		requireBoundaryViolation(t, err, "tool")
		requireZeroTerminal(t, res)
	})
}

// TestRunInfraErrorsWinOverCancellation pins that a non-nil caller error is never reclassified by concurrent cancellation: the original error comes back verbatim with no terminal, from either effect, even when the same invocation just cancelled the run context.
func TestRunInfraErrorsWinOverCancellation(t *testing.T) {
	t.Run("model-effect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
			cancel()
			return ModelSettlement{}, errInfra
		}

		res, err := Run(ctx, f.invocation())
		requireZeroTerminal(t, res)
		if !errors.Is(err, errInfra) {
			t.Fatalf("error = %v, want the infrastructure error verbatim", err)
		}
	})

	t.Run("tool-effect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &runFakes{}
		f.modelSet = readyOutputs(mkCallsTerminalOutput())
		f.toolRes = func(int, model.ToolCall) (model.ToolResult, error) {
			cancel()
			return model.ToolResult{}, errInfra
		}

		res, err := Run(ctx, f.invocation())
		requireZeroTerminal(t, res)
		if !errors.Is(err, errInfra) {
			t.Fatalf("error = %v, want the infrastructure error verbatim", err)
		}
		if f.toolCalls != 1 {
			t.Fatalf("tool calls = %d, want the batch to stop at the failing call", f.toolCalls)
		}
	})
}

// TestRunCancellationDominatesCap pins the cap-vs-cancellation order: when the run context is done at the exact point the cap would otherwise fire, the result is an interruption, not the cap failure.
func TestRunCancellationDominatesCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	sets := make([]ModelSettlement, maxModelEffects)
	for i := range sets[:maxModelEffects-1] {
		sets[i] = erroredContinue(i)
	}
	f.modelSet = func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if call == maxModelEffects {
			return cancelThen(cancel, erroredContinue(call))
		}
		return setScript(sets...)(call, req, cb)
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	if res.LastOutput != nil { // the last settlement was errored, which no interruption terminal may carry.
		t.Fatalf("interruption carried %#v, want no output", res.LastOutput)
	}
	if f.modelCalls != maxModelEffects || f.sourceCalls != maxModelEffects {
		t.Fatalf("model=%d source=%d, want 25/25", f.modelCalls, f.sourceCalls)
	}
}
