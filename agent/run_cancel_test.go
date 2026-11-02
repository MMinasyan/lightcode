package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// cancelThen wraps a scripted return value in a run-context cancellation fired at the exact moment the boundary is inside its own invocation — the blocking-fake stand-in that pins what the next checkpoint must observe.
func cancelThen(cancel context.CancelFunc, set ModelSettlement) (ModelSettlement, error) {
	cancel()
	return set, nil
}

func TestRunCancellationShapedCallbackErrorIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cause := fmt.Errorf("model failure: %w", context.Canceled)
	f := &runFakes{}
	f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
		cancel()
		return ModelSettlement{}, cause
	}
	res, err := Run(ctx, f.invocation())
	requireZeroTerminal(t, res)
	if err != cause || f.sourceCalls != 1 || f.modelCalls != 1 || f.toolCalls != 0 {
		t.Fatalf("error=%v calls=%d/%d/%d, want original error and 1/1/0", err, f.sourceCalls, f.modelCalls, f.toolCalls)
	}
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
	testassert.Equal(t, res.LastOutput, callsOut)
	if len(res.UnstartedCalls) != 0 {
		t.Fatalf("unstarted = %s, want none after a fully settled batch", joinedIDs(res.UnstartedCalls))
	}
	testassert.Equal(t, f.modelCalls, 1)
}

// TestRunCancelledBeforeFirstEffect pins the very first checkpoint: an already-done run context ends in a bare interruption with no caller function ever starting.
func TestRunCancelledBeforeFirstEffect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &runFakes{}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	testassert.Require(t, res.LastOutput == nil && res.UnstartedCalls == nil, "bare interruption expected, got %#v", res)
	testassert.Require(t, f.sourceCalls == 0 && f.modelCalls == 0 && f.toolCalls == 0, "cancelled run started caller functions: source=%d model=%d tool=%d", f.sourceCalls, f.modelCalls, f.toolCalls)
}

// TestRunCancelledDuringSnapshot pins the checkpoint between obtaining the fresh context and invoking the model effect: the effect must never start.
func TestRunCancelledDuringSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &runFakes{}
	f.snapshot = func(int) ([]model.Message, error) { cancel(); return userSnapshot("go"), nil }

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	testassert.Require(t, f.sourceCalls == 1 && f.modelCalls == 0 && f.toolCalls == 0, "source=%d model=%d tool=%d, want 1/0/0", f.sourceCalls, f.modelCalls, f.toolCalls)
}

func TestRunCallbackErrorIdentity(t *testing.T) {
	for _, boundary := range []string{"source", "model", "tool"} {
		for _, done := range []bool{false, true} {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("wrapped: %w", context.Canceled), fmt.Errorf("wrapped: %w", context.DeadlineExceeded), errInfra} {
				t.Run(fmt.Sprintf("%s/done=%t/%s", boundary, done, cause), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					f := &runFakes{modelSet: readyOutputs(mkCallsTerminalOutput())}
					fail := func() {
						if done {
							cancel()
						}
					}
					switch boundary {
					case "source":
						f.snapshot = func(int) ([]model.Message, error) { fail(); return nil, cause }
					case "model":
						f.modelSet = func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
							fail()
							return ModelSettlement{}, cause
						}
					case "tool":
						f.toolRes = func(int, model.ToolCall) (model.ToolResult, error) { fail(); return model.ToolResult{}, cause }
					}
					res, err := Run(ctx, f.invocation())
					requireZeroTerminal(t, res)
					wantModel, wantTool := 0, 0
					if boundary != "source" {
						wantModel = 1
					}
					if boundary == "tool" {
						wantTool = 1
					}
					if err != cause || f.sourceCalls != 1 || f.modelCalls != wantModel || f.toolCalls != wantTool {
						t.Fatalf("error=%v calls=%d/%d/%d, want original error and 1/%d/%d", err, f.sourceCalls, f.modelCalls, f.toolCalls, wantModel, wantTool)
					}
				})
			}
		}
	}
}

func TestRunRequestSourceOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     RequestResult
		err        error
		wantError  error
		wantStatus TerminalStatus
	}{
		{"populated", RequestResult{Request: &model.Request{Messages: userSnapshot("go")}}, nil, nil, TerminalSuccess},
		{"empty", RequestResult{Request: &model.Request{}}, nil, nil, TerminalSuccess},
		{"interrupted", RequestResult{Interrupted: true}, nil, nil, TerminalInterruption},
		{"neither", RequestResult{}, nil, ErrBoundaryProtocol, ""},
		{"both", RequestResult{Request: &model.Request{}, Interrupted: true}, nil, ErrBoundaryProtocol, ""},
		{"error wins invalid", RequestResult{Request: &model.Request{}, Interrupted: true}, errInfra, errInfra, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &runFakes{modelSet: readyOutputs(mkOutput(model.OutputCompleted, "", nil))}
			inv := f.invocation()
			inv.Request = func(context.Context) (RequestResult, error) { f.sourceCalls++; return tc.result, tc.err }
			res, err := Run(context.Background(), inv)
			if tc.wantError != nil {
				requireZeroTerminal(t, res)
				if !errors.Is(err, tc.wantError) || f.modelCalls != 0 || f.toolCalls != 0 {
					t.Fatalf("result=%+v error=%v calls=%d/%d", res, err, f.modelCalls, f.toolCalls)
				}
				if tc.err != nil && err != tc.err {
					t.Fatalf("error identity = %v", err)
				}
				return
			}
			if err != nil || res.Status != tc.wantStatus || f.sourceCalls != 1 || f.toolCalls != 0 {
				t.Fatalf("result=%+v error=%v calls=%d/%d/%d", res, err, f.sourceCalls, f.modelCalls, f.toolCalls)
			}
			wantModel := 1
			if tc.wantStatus == TerminalInterruption {
				wantModel = 0
			}
			if f.modelCalls != wantModel {
				t.Fatalf("model calls=%d want %d", f.modelCalls, wantModel)
			}
		})
	}
	t.Run("invalid ready before cancellation checkpoint", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &runFakes{}
		inv := f.invocation()
		inv.Request = func(context.Context) (RequestResult, error) {
			cancel()
			return RequestResult{Request: &model.Request{Messages: []model.Message{{Role: "invalid"}}}}, nil
		}
		res, err := Run(ctx, inv)
		requireZeroTerminal(t, res)
		if !errors.Is(err, ErrBoundaryProtocol) || f.modelCalls != 0 {
			t.Fatalf("result=%+v error=%v", res, err)
		}
	})
}

func TestRunLaterRequestSourceOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result RequestResult
		err    error
		status TerminalStatus
	}{
		{"populated", RequestResult{Request: &model.Request{Messages: userSnapshot("later")}}, nil, TerminalSuccess},
		{"empty", RequestResult{Request: &model.Request{}}, nil, TerminalSuccess},
		{"interrupted", RequestResult{Interrupted: true}, nil, TerminalInterruption},
		{"both", RequestResult{Request: &model.Request{}, Interrupted: true}, nil, ""},
		{"neither", RequestResult{}, nil, ""},
		{"error wins invalid", RequestResult{Interrupted: true, Request: &model.Request{}}, errInfra, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := mkCallsTerminalOutput()
			f := &runFakes{modelSet: readyOutputs(first, mkOutput(model.OutputCompleted, "", nil))}
			inv := f.invocation()
			source := inv.Request
			inv.Request = func(ctx context.Context) (RequestResult, error) {
				if f.sourceCalls == 0 {
					return source(ctx)
				}
				f.sourceCalls++
				return tc.result, tc.err
			}
			res, err := Run(context.Background(), inv)
			if tc.status == "" {
				requireZeroTerminal(t, res)
				if tc.err != nil && err != tc.err {
					t.Fatalf("error=%v want %v", err, tc.err)
				}
				if tc.err == nil && !errors.Is(err, ErrBoundaryProtocol) {
					t.Fatalf("error=%v want protocol error", err)
				}
			} else if err != nil || res.Status != tc.status {
				t.Fatalf("result=%+v error=%v", res, err)
			}
			if tc.status == TerminalInterruption && (res.LastOutput != first || len(res.UnstartedCalls) != 0) {
				t.Fatalf("interruption=%+v", res)
			}
			wantModels := 1
			if tc.status == TerminalSuccess {
				wantModels = 2
			}
			if f.sourceCalls != 2 || f.modelCalls != wantModels || f.toolCalls != 3 {
				t.Fatalf("calls=%d/%d/%d", f.sourceCalls, f.modelCalls, f.toolCalls)
			}
		})
	}
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
	testassert.Equal(t, res.LastOutput, callsOut)
	{
		got := joinedIDs(res.UnstartedCalls)
		testassert.Equal(t, got, "[a b c]")
	}
	testassert.Equal(t, f.toolCalls, 0)
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
	testassert.Equal(t, res.LastOutput, callsOut)
	{
		got := joinedIDs(res.UnstartedCalls)
		testassert.Equal(t, got, "[b c]")
	}
	testassert.Equal(t, f.toolCalls, 1)
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
	testassert.Require(t, res.LastOutput == final && res.UnstartedCalls == nil, "terminal = %#v, want the completed output carried verbatim", res)
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
	testassert.Equal(t, res.LastOutput, callsOut)
	{
		got := joinedIDs(res.UnstartedCalls)
		testassert.Equal(t, got, "[b c]")
	}
	testassert.Equal(t, f.toolCalls, 1)
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
		testassert.Require(t, errors.Is(err, errInfra), "error = %v, want the infrastructure error verbatim", err)
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
		testassert.Require(t, errors.Is(err, errInfra), "error = %v, want the infrastructure error verbatim", err)
		testassert.Equal(t, f.toolCalls, 1)
	})
}

// TestRunCancelledAfterManyContinuations proves a long run still stops before the next effect.
func TestRunCancelledAfterManyContinuations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &runFakes{}
	f.modelSet = func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if call == 30 {
			cancel()
		}
		return erroredContinue(call), nil
	}

	res, err := Run(ctx, f.invocation())
	requireRunInterrupted(t, res, err, "agent interrupted")
	// the last settlement was errored, which no interruption terminal may carry.
	testassert.Require(t, res.LastOutput == nil, "interruption carried %#v, want no output", res.LastOutput)
	testassert.Require(t, f.modelCalls == 30 && f.sourceCalls == 30, "model=%d source=%d, want 30/30", f.modelCalls, f.sourceCalls)
}
