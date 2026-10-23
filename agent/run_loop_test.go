package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, f.sourceCalls == 3 && f.modelCalls == 3, "source=%d model=%d, want 3/3", f.sourceCalls, f.modelCalls)
	for i, want := range []string{"a", "b", "c"} { // one fresh snapshot per logical effect: after the settled batch and after the continue disposition alike.
		{
			got := f.reqs[i].Messages[0].TextContent()
			testassert.Require(t, got == want, "request %d snapshot = %q, want %q", i, got, want)
		}
	}
	testassert.Require(t, res.LastOutput != nil && res.LastOutput.Status == model.OutputCompleted, "last output = %#v, want the final completed settlement", res.LastOutput)
}

// TestRunRequestToolsAreFreshCopiesPerEffect pins the per-request tool ownership rule: every model effect receives its own deep copy of the entry-validated advertised list — slice and Parameters bytes — so a model-effect callback mutating the handed request's tool slice and bytes in place cannot reach the immutable advertised baseline, any later request, or the invocation input.
func TestRunRequestToolsAreFreshCopiesPerEffect(t *testing.T) {
	f := &runFakes{}
	advertised := advTools()
	inv := f.invocation()
	inv.Tools = advertised
	var handed []model.ToolDefinition // what each request carried before the callback corrupted it
	f.modelSet = func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		handed = append(handed, model.ToolDefinition{Name: req.Tools[0].Name, Parameters: append([]byte(nil), req.Tools[0].Parameters...)})
		if len(req.Tools) > 0 { // corrupt the handed request's tool entry in place: element fields and Parameters bytes.
			req.Tools[0].Name = "mutated"
			if len(req.Tools[0].Parameters) > 0 {
				req.Tools[0].Parameters[0] = 'X'
			}
		}
		return setScript(
			ModelSettlement{Disposition: DispoContinue, Output: mkOutput(model.OutputErrored, "boom", nil)},
			ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)},
		)(call, req, cb)
	}

	res, err := Run(context.Background(), inv)
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Equal(t, f.modelCalls, 2)
	// Every request started from the pristine advertised values.
	for i, got := range handed {
		testassert.Require(t, got.Name == "t" && string(got.Parameters) == `{"type":"object"}`, "request %d carried a mutated tool: %#v", i+1, got)
	}
	// The mutation actually landed in the recorded requests — the check above is not vacuous.
	testassert.Require(t, f.reqs[0].Tools[0].Name == "mutated" && f.reqs[1].Tools[0].Name == "mutated", "recorded requests show %q/%q, want the callback's in-place mutations", f.reqs[0].Tools[0].Name, f.reqs[1].Tools[0].Name)
	// The invocation input keeps exactly its own entry values.
	testassert.Require(t, advertised[0].Name == "t" && string(advertised[0].Parameters) == `{"type":"object"}`, "advertised baseline mutated through a request alias: %#v", advertised[0])
	// Each request owns independent storage: no request aliases the advertised bytes, and successive requests share nothing.
	testassert.Require(t, &f.reqs[0].Tools[0].Parameters[0] != &advertised[0].Parameters[0], "%v", "a request gained an alias to the advertised baseline bytes")
	testassert.Require(t, &f.reqs[0].Tools[0].Parameters[0] != &f.reqs[1].Tools[0].Parameters[0], "%v", "successive requests share tool Parameters backing")
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
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, f.modelCalls == 2 && f.sourceCalls == 2 && f.toolCalls == 3, "model=%d source=%d tool=%d, want 2/2/3", f.modelCalls, f.sourceCalls, f.toolCalls)
	{
		got := joinedIDs(f.calls)
		testassert.Equal(t, got, "[a b c]")
	}
	testassert.Equal(t, res.LastOutput, final)
}

// TestRunDeniedAndErrorResultsNeverStopTheBatch pins that per-call settlements other than interrupted settle only their own call: the surrounding batch completes and the run continues.
func TestRunDeniedAndErrorResultsNeverStopTheBatch(t *testing.T) {
	f := &runFakes{}
	callsOut := mkCallsTerminalOutput()
	f.modelSet = readyOutputs(callsOut, mkOutput(model.OutputCompleted, "", nil))

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success via default all-success tool script", res, err)
	testassert.Require(t, f.toolCalls == 3 && f.modelCalls == 2, "tool=%d model=%d, want 3/2", f.toolCalls, f.modelCalls)
}

// TestRunContinuesPast25ModelEffects proves continuation length cannot fail a run.
func TestRunContinuesPast25ModelEffects(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, 51)
	for i := range sets[:50] {
		sets[i] = erroredContinue(i)
	}
	sets[50] = ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalSuccess, "terminal = %s/%q, want success after 50 continuations", res.Status, res.Detail)
	testassert.Equal(t, res.LastOutput, sets[50].Output)
	testassert.Require(t, f.modelCalls == 51 && f.sourceCalls == 51, "model=%d source=%d, want 51/51", f.modelCalls, f.sourceCalls)
}

// TestRunFinalTextFromEffect25Succeeds pins success at the former request boundary.
func TestRunFinalTextFromEffect25Succeeds(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, 25)
	for i := range sets[:24] {
		sets[i] = erroredContinue(i)
	}
	sets[24] = ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success from the 25th effect", res, err)
	testassert.Equal(t, f.modelCalls, 25)
}

// TestRunToolBatchesContinuePast25ModelEffects proves ordered tool dispatch continues until final text.
func TestRunToolBatchesContinuePast25ModelEffects(t *testing.T) {
	f := &runFakes{}
	sets := make([]ModelSettlement, 31)
	callsOut := mkCallsTerminalOutput()
	for i := range sets[:30] {
		sets[i] = ModelSettlement{Disposition: DispoReady, Output: callsOut}
	}
	final := mkOutput(model.OutputCompleted, "", nil)
	sets[30] = ModelSettlement{Disposition: DispoReady, Output: final}
	f.modelSet = setScript(sets...)

	res, err := Run(context.Background(), f.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalSuccess && res.LastOutput == final, "terminal = %s/%q, want success with final text", res.Status, res.Detail)
	testassert.Require(t, f.toolCalls == 90 && f.modelCalls == 31 && f.sourceCalls == 31, "tool=%d model=%d source=%d, want 90/31/31", f.toolCalls, f.modelCalls, f.sourceCalls)
	for i, call := range f.calls {
		testassert.Equal(t, call.ID, callsOut.Message.ToolCalls[i%3].ID)
	}
}

// TestRunLargeToolBatchSucceeds pins that every call in a large batch settles.
func TestRunLargeToolBatchSucceeds(t *testing.T) {
	f := &runFakes{}
	bigOut := mkOutput(model.OutputCompleted, "", callArgs(30, json.RawMessage(`{}`)))
	f.modelSet = readyOutputs(bigOut, mkOutput(model.OutputCompleted, "", nil))

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, f.toolCalls == 30 && f.modelCalls == 2, "tool=%d model=%d, want 30/2", f.toolCalls, f.modelCalls)
}
