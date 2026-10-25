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

// TestRunRequestToolsAreFreshCopiesPerEffect pins the per-request ownership rule: every model effect receives its own validated owned copy — tool slice and Parameters bytes, and messages alike — and the round's advertised dispatch names are extracted from that copy before the effect runs, so a model effect mutating the handed request's tool definitions and message text in place cannot alter the names its own round dispatches, any later round's request, or the data the request source itself retains. Different rounds advertise different subsets.
func TestRunRequestToolsAreFreshCopiesPerEffect(t *testing.T) {
	f := &runFakes{}
	roundTools := [][]model.ToolDefinition{
		{
			{Name: "t", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "u", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		{{Name: "t", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	f.tools = func(call int) ([]model.ToolDefinition, error) {
		if call > len(roundTools) {
			return nil, errScriptExhausted
		}
		return roundTools[call-1], nil
	}
	sourceMsgs := userSnapshot("go") // one snapshot shared by every round: an alias from a handed request back into it would reach every later round and this baseline.
	f.snapshot = func(int) ([]model.Message, error) { return sourceMsgs, nil }

	callsOut := mkCallsTerminalOutput() // the first round settles a dispatched batch; the second ends with final text.
	f.modelSet = func(call int, req model.Request, _ AssemblyCallback) (ModelSettlement, error) {
		if call == 1 { // corrupt the handed round-1 request in place: tool name, Parameters bytes, and message text.
			req.Tools[0].Name = "mutated"
			req.Tools[0].Parameters[0] = 'X'
			req.Messages[0].Content[0].Text = "mutated"
			return ModelSettlement{Disposition: DispoReady, Output: callsOut}, nil
		}
		return ModelSettlement{Disposition: DispoReady, Output: mkOutput(model.OutputCompleted, "", nil)}, nil
	}

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, f.sourceCalls == 2 && f.modelCalls == 2 && f.toolCalls == 3, "source=%d model=%d tool=%d, want 2/2/3", f.sourceCalls, f.modelCalls, f.toolCalls)

	// The round-1 mutation landed in the handed request — every pristine assertion below is not vacuous.
	testassert.Require(t, f.reqs[0].Tools[0].Name == "mutated" && f.reqs[0].Tools[0].Parameters[0] == 'X' && f.reqs[0].Messages[0].TextContent() == "mutated", "recorded round-1 request shows no mutation: %#v", f.reqs[0])
	// Round 1's dispatch names are the request's advertisement as extracted before its model effect ran.
	testassert.Require(t, len(f.ads) == 3 && len(f.ads[0]) == 2 && f.ads[0][0] == "t" && f.ads[0][1] == "u", "round-1 dispatch names = %v, want the pristine [t u] advertisement", f.ads[0])
	// Round 2 carries its own pristine single-name subset and an intact snapshot.
	testassert.Require(t, len(f.reqs[1].Tools) == 1 && f.reqs[1].Tools[0].Name == "t" && string(f.reqs[1].Tools[0].Parameters) == `{"type":"object"}`, "round-2 tools = %#v, want the pristine subset", f.reqs[1].Tools)
	testassert.Require(t, f.reqs[1].Messages[0].TextContent() == "go", "round-2 message = %q, want the intact snapshot", f.reqs[1].Messages[0].TextContent())
	// The source's own data keeps exactly its entry values.
	testassert.Require(t, sourceMsgs[0].Content[0].Text == "go", "the source's snapshot was mutated through a handed request: %q", sourceMsgs[0].Content[0].Text)
	for i, want := range []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`)}, {Name: "u", Parameters: json.RawMessage(`{"type":"object"}`)}} {
		if i >= len(roundTools[0]) {
			break
		}
		testassert.Require(t, roundTools[0][i].Name == want.Name && string(roundTools[0][i].Parameters) == string(want.Parameters), "source round-1 tool %d mutated: %#v", i, roundTools[0][i])
	}

	// No alias from a callback into source data or a later request: every round owns independent storage.
	testassert.Require(t, &f.reqs[0].Tools[0].Parameters[0] != &roundTools[0][0].Parameters[0], "%v", "a request gained an alias to the source's tool bytes")
	testassert.Require(t, &f.reqs[0].Tools[1].Parameters[0] != &roundTools[0][1].Parameters[0], "%v", "a request gained an alias to the source's tool bytes")
	testassert.Require(t, &f.reqs[1].Tools[0].Parameters[0] != &roundTools[1][0].Parameters[0], "%v", "a request gained an alias to the source's tool bytes")
	testassert.Require(t, &f.reqs[0].Tools[0].Parameters[0] != &f.reqs[1].Tools[0].Parameters[0], "%v", "successive requests share tool Parameters backing")
	testassert.Require(t, &f.reqs[0].Messages[0].Content[0] != &sourceMsgs[0].Content[0], "%v", "a request gained an alias to the source's message content")
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
