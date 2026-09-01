package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// erroredContinue builds one valid continue settlement carrying a distinct errored output so pointer identity pins which settlement the terminal retained.
func erroredContinue(n int) ModelSettlement {
	return ModelSettlement{Disposition: DispoContinue, Output: mkOutput(model.OutputErrored, fmt.Sprintf("e%d", n), nil)}
}

// TestRunContinuationUsesFreshSnapshotPerEffect pins that every logical model effect — a continue disposition and, separately, a settled tool batch — repeats from a freshly obtained context snapshot carried into its own request.
func TestRunContinuationUsesFreshSnapshotPerEffect(t *testing.T) {
	f := &runFakes{}
	snaps := [][]model.Message{userSnapshot("a"), userSnapshot("b"), userSnapshot("c")}
	f.snapshot = func(call int) ([]model.Message, error) {
		if call > len(snaps) {
			return nil, errScriptExhausted
		}
		return snaps[call-1], nil
	}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = setScript(
		ModelSettlement{Disposition: DispoReady, Output: callsOut},
		ModelSettlement{Disposition: DispoContinue, Output: mkOutput(model.OutputErrored, "boom", nil)},
		ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)},
	)

	res, err := Run(context.Background(), f.invocation())
	if err != nil || res.Status != TerminalSuccess {
		t.Fatalf("run = %#v, %v; want success", res, err)
	}
	if f.sourceCalls != 3 || f.modelCalls != 3 {
		t.Fatalf("source=%d model=%d, want 3/3", f.sourceCalls, f.modelCalls)
	}
	for i, want := range []string{"a", "b", "c"} { // one fresh snapshot per logical effect: after the settled batch and after the continue disposition alike.
		if got := f.reqs[i].Messages[0].TextContent(); got != want {
			t.Fatalf("request %d snapshot = %q, want %q", i, got, want)
		}
	}
	if res.LastOutput == nil || res.LastOutput.Status != model.OutputCompleted {
		t.Fatalf("last output = %#v, want the final completed settlement", res.LastOutput)
	}
}

// TestRunToolBatchSettlesInOrderThenContinues pins sequential in-order dispatch with a settled complete non-interrupted batch feeding the next fresh-context effect, which then ends the run.
func TestRunToolBatchSettlesInOrderThenContinues(t *testing.T) {
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput() // ids a, b, c
	final := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = readyOutputs(callsOut, final)
	f.toolRes = resScript(
		model.ToolResult{CallID: "a", Status: model.ResultSuccess},
		model.ToolResult{CallID: "b", Status: model.ResultError, Content: "failed"},
		model.ToolResult{CallID: "c", Status: model.ResultDenied, Content: "denied by policy"},
	)

	res, err := Run(context.Background(), f.invocation())
	if err != nil || res.Status != TerminalSuccess {
		t.Fatalf("run = %#v, %v; want success", res, err)
	}
	if f.modelCalls != 2 || f.sourceCalls != 2 || f.toolCalls != 3 {
		t.Fatalf("model=%d source=%d tool=%d, want 2/2/3", f.modelCalls, f.sourceCalls, f.toolCalls)
	}
	if got := joinedIDs(f.calls); got != "[a b c]" {
		t.Fatalf("dispatch order = %s, want assembled order", got)
	}
	if res.LastOutput != final {
		t.Fatalf("terminal carries %p, want the second settlement %p", res.LastOutput, final)
	}
}

// TestRunDeniedAndErrorResultsNeverStopTheBatch pins that per-call settlements other than interrupted settle only their own call: the surrounding batch completes and the run continues.
func TestRunDeniedAndErrorResultsNeverStopTheBatch(t *testing.T) {
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = readyOutputs(callsOut, mkOutput(model.OutputCompleted, "", nil))

	res, err := Run(context.Background(), f.invocation())
	if err != nil || res.Status != TerminalSuccess {
		t.Fatalf("run = %#v, %v; want success via default all-success tool script", res, err)
	}
	if f.toolCalls != 3 || f.modelCalls != 2 {
		t.Fatalf("tool=%d model=%d, want 3/2", f.toolCalls, f.modelCalls)
	}
}

// TestRunCapExhaustedAfterContinue pins the cap failure shape: after 25 continues, Agent fails with the last errored output and the fixed detail before a 26th request — and before another snapshot fetch.
func TestRunCapExhaustedAfterContinue(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, maxModelEffects)
	for i := range sets {
		sets[i] = erroredContinue(i)
	}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != TerminalFailure || res.Detail != "agent exceeded 25 model effects" {
		t.Fatalf("terminal = %s/%q, want failure with the cap detail", res.Status, res.Detail)
	}
	if res.LastOutput != sets[maxModelEffects-1].Output {
		t.Fatalf("cap failure carries %p, want the 25th settlement output %p", res.LastOutput, sets[maxModelEffects-1].Output)
	}
	if f.modelCalls != maxModelEffects || f.sourceCalls != maxModelEffects {
		t.Fatalf("model=%d source=%d, want 25/25 — the cap ends the run before a 26th request", f.modelCalls, f.sourceCalls)
	}
}

// TestRunFinalTextFromEffect25Succeeds pins that the 25th logical model effect may still finish the run cleanly: its call-free completed output succeeds instead of tripping the cap.
func TestRunFinalTextFromEffect25Succeeds(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, maxModelEffects)
	for i := range sets[:maxModelEffects-1] {
		sets[i] = erroredContinue(i)
	}
	sets[maxModelEffects-1] = ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	if err != nil || res.Status != TerminalSuccess {
		t.Fatalf("run = %#v, %v; want success from the 25th effect", res, err)
	}
	if f.modelCalls != maxModelEffects {
		t.Fatalf("model=%d, want 25", f.modelCalls)
	}
}

// TestRunEffect25SettlesBatchBeforeCapFailure pins that a completed tool batch from the 25th effect fully settles — every call dispatched — and only then fails with that last completed output under the cap detail.
func TestRunEffect25SettlesBatchBeforeCapFailure(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, maxModelEffects)
	for i := range sets[:maxModelEffects-1] {
		sets[i] = erroredContinue(i)
	}
	callsOut := mkCallsTerminalOutput()
	sets[maxModelEffects-1] = ModelSettlement{Disposition: DispoReady, Output: callsOut}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != TerminalFailure || res.Detail != "agent exceeded 25 model effects" {
		t.Fatalf("terminal = %s/%q, want cap failure", res.Status, res.Detail)
	}
	if res.LastOutput != callsOut {
		t.Fatalf("cap failure carries %p, want the 25th completed output %p", res.LastOutput, callsOut)
	}
	if f.toolCalls != 3 || f.modelCalls != maxModelEffects || f.sourceCalls != maxModelEffects {
		t.Fatalf("tool=%d model=%d source=%d, want 3/25/25 — the batch settles before the cap ends the run", f.toolCalls, f.modelCalls, f.sourceCalls)
	}
}

// TestRunToolEffectsDoNotConsumeModelCap pins that tool calls beyond the numeric model cap are irrelevant to it: one batch of 30 calls followed by a final text response still succeeds.
func TestRunToolEffectsDoNotConsumeModelCap(t *testing.T) {
	f := &runFakes{}
	bigOut := mkOutput(model.OutputCompleted, "", callArgs(maxModelEffects+5, json.RawMessage(`{}`)))
	f.modelSet = readyOutputs(bigOut, mkOutput(model.OutputCompleted, "", nil))

	res, err := Run(context.Background(), f.invocation())
	if err != nil || res.Status != TerminalSuccess {
		t.Fatalf("run = %#v, %v; want success", res, err)
	}
	if f.toolCalls != maxModelEffects+5 || f.modelCalls != 2 {
		t.Fatalf("tool=%d model=%d, want 30/2", f.toolCalls, f.modelCalls)
	}
}
