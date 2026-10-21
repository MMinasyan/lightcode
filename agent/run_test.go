package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// errInfra is one distinguishable infrastructure failure the fakes return through a caller boundary; it must always surface verbatim as Run's Go error with no terminal result.
var errInfra = errors.New("infrastructure failure")

// errScriptExhausted marks a boundary invocation past what the test scripted — evidence of an extra or late effect the surrounding assertions should already have caught.
var errScriptExhausted = errors.New("caller fake script exhausted")

// runFakes scripts and records the three caller boundaries of one Invocation: every call is counted, requests and started calls are recorded in order, and a script function receives the 1-based invocation number. Exhausting a script fails the run through the recorded counters rather than silently extending it. A script that settles an output-bearing disposition without touching the callback receives one protocol-plain callback invocation (testRef over a minimal valid stream) unless noCallbackFill is set — scripts that exercise the callback protocol itself manage the callback explicitly.
type runFakes struct {
	snapshot       func(call int) ([]model.Message, error)
	modelSet       func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error)
	toolRes        func(call int, c model.ToolCall) (model.ToolResult, error)
	noCallbackFill bool
	sourceCalls    int
	modelCalls     int
	toolCalls      int
	reqs           []model.Request
	calls          []model.ToolCall
}

func (f *runFakes) context() ContextSource {
	return func(context.Context) ([]model.Message, error) {
		f.sourceCalls++
		if f.snapshot == nil {
			return userSnapshot("go"), nil
		}
		return f.snapshot(f.sourceCalls)
	}
}

func (f *runFakes) model() ModelEffect {
	return func(_ context.Context, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		f.modelCalls++
		f.reqs = append(f.reqs, req)
		if f.modelSet == nil {
			return ModelSettlement{}, nil // an unscripted invocation returns a deliberately invalid settlement: the run ends in a typed violation the assertions catch.
		}
		used := 0
		tracked := func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			used++
			return cb(source, stream)
		}
		set, err := f.modelSet(f.modelCalls, req, tracked)
		if err != nil || f.noCallbackFill || used != 0 || set.Output == nil {
			return set, err
		}
		if _, ferr := tracked(testRef, newFakeStream(deltaStep(choiceDelta(txtPos(0, "fill"))))); ferr != nil {
			return set, ferr
		}
		return set, nil
	}
}

func (f *runFakes) tool() ToolEffect {
	return func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		f.toolCalls++
		f.calls = append(f.calls, c)
		if f.toolRes == nil {
			return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
		}
		return f.toolRes(f.toolCalls, c)
	}
}

func (f *runFakes) invocation() Invocation {
	return Invocation{ExpectedModel: testRef, Tools: advTools(), Context: f.context(), ModelEffect: f.model(), ToolEffect: f.tool()}
}

func userSnapshot(text string) []model.Message {
	return []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: text}}}}
}

func advTools() []model.ToolDefinition {
	return []model.ToolDefinition{{Name: "t", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

// setScript replays one settlement per model effect call in order.
func setScript(sets ...ModelSettlement) func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
	return func(call int, _ model.Request, _ AssemblyCallback) (ModelSettlement, error) {
		if call > len(sets) {
			return ModelSettlement{}, errScriptExhausted
		}
		return sets[call-1], nil
	}
}

// resScript replays one tool result per tool effect call in order.
func resScript(results ...model.ToolResult) func(int, model.ToolCall) (model.ToolResult, error) {
	return func(call int, _ model.ToolCall) (model.ToolResult, error) {
		if call > len(results) {
			return model.ToolResult{}, errScriptExhausted
		}
		return results[call-1], nil
	}
}

// readyOutputs builds a script of ready settlements carrying one completed output per entry, each with distinct text so per-effect outputs are individually identifiable by pointer.
func readyOutputs(outs ...*model.Output) func(int, model.Request, AssemblyCallback) (ModelSettlement, error) {
	sets := make([]ModelSettlement, len(outs))
	for i, o := range outs {
		sets[i] = ModelSettlement{Disposition: DispoReady, Output: o}
	}
	return setScript(sets...)
}

func mkOutput(status model.OutputStatus, detail string, calls []model.ToolCall) *model.Output {
	msg := &model.Message{Role: model.RoleAssistant, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, ToolCalls: calls}
	if status != model.OutputCompleted {
		msg.ToolCalls = nil // non-completed outputs never carry tool calls; the retained partial payload keeps continue rows valid under the assistant-payload rule.
	}
	out, err := model.NewOutput(model.Output{Status: status, Source: testRef, Message: msg, Detail: detail})
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err))
	}
	return &out
}

func callArgs(n int, arg json.RawMessage) []model.ToolCall {
	calls := make([]model.ToolCall, 0, n)
	for i := range n {
		calls = append(calls, model.ToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("fn%d", i), Arguments: arg})
	}
	return calls
}

func joinedIDs(calls []model.ToolCall) string {
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		ids = append(ids, c.ID)
	}
	return fmt.Sprint(ids)
}

func requireZeroTerminal(t *testing.T, res TerminalResult) {
	t.Helper()
	testassert.Require(t, res.Status == "" && res.LastOutput == nil && res.UnstartedCalls == nil && res.Detail == "", "expected zero terminal result, got %#v", res)
}

func requireRunInterrupted(t *testing.T, res TerminalResult, err error, wantDetail string) {
	t.Helper()
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalInterruption && res.Detail == wantDetail, "interruption = %s/%q, want interruption/%q", res.Status, res.Detail, wantDetail)
}

// TestRunInvalidInvocation pins every invalid entry value returning the typed boundary-protocol error with boundary "agent" and a zero terminal result before any caller function runs: a nil run context is rejected, never normalized, and the same no-run guarantee holds for every other row.
func TestRunInvalidInvocation(t *testing.T) {
	rows := []struct {
		name string
		ctx  func() context.Context // nil return supplies a literal nil context to Run.
		mod  func(inv *Invocation)
	}{
		{"nil-run-context", func() context.Context { return nil }, nil},
		{"zero-expected-model", nil, func(inv *Invocation) { inv.ExpectedModel = model.ModelRef{} }},
		{"partial-expected-model", nil, func(inv *Invocation) { inv.ExpectedModel = model.ModelRef{Provider: "acme"} }},
		{"nil-context-source", nil, func(inv *Invocation) { inv.Context = nil }},
		{"nil-model-effect", nil, func(inv *Invocation) { inv.ModelEffect = nil }},
		{"nil-tool-effect", nil, func(inv *Invocation) { inv.ToolEffect = nil }},
		{"empty-tool-name", nil, func(inv *Invocation) {
			inv.Tools = []model.ToolDefinition{{Name: "", Parameters: json.RawMessage(`{}`)}}
		}},
		{"duplicate-tool-names", nil, func(inv *Invocation) {
			inv.Tools = []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`{}`)}, {Name: "t", Parameters: json.RawMessage(`{}`)}}
		}},
		{"array-parameters", nil, func(inv *Invocation) {
			inv.Tools = []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`[1,2]`)}}
		}},
		{"primitive-parameters", nil, func(inv *Invocation) {
			inv.Tools = []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`3`)}}
		}},
		{"null-parameters", nil, func(inv *Invocation) {
			inv.Tools = []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`null`)}}
		}},
		{"absent-parameters", nil, func(inv *Invocation) { inv.Tools = []model.ToolDefinition{{Name: "t"}} }},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			inv := f.invocation()
			ctx := context.Background()
			if row.ctx != nil {
				ctx = row.ctx()
			}
			if row.mod != nil {
				row.mod(&inv)
			}

			res, err := Run(ctx, inv)
			requireBoundaryViolation(t, err, "agent")
			requireZeroTerminal(t, res)
			testassert.Require(t, f.sourceCalls == 0 && f.modelCalls == 0 && f.toolCalls == 0, "rejected invocation ran caller functions: source=%d model=%d tool=%d", f.sourceCalls, f.modelCalls, f.toolCalls)
		})
	}
}

// TestRunSuccessSingleEffect pins the clean end of a minimal run: one fresh snapshot, one counted model effect whose request carries the validated advertised tools, and a success terminal whose last output is the settlement's own transferred pointer — no second copy.
func TestRunSuccessSingleEffect(t *testing.T) {
	f := &runFakes{}
	out := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = readyOutputs(out)

	res, err := Run(context.Background(), f.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalSuccess && res.Detail == "" && res.UnstartedCalls == nil && res.LastOutput == out, "terminal = %#v, want success with empty detail and no unstarted calls; got %v, want %v", res, res.LastOutput, out)
	testassert.Require(t, f.sourceCalls == 1 && f.modelCalls == 1 && f.toolCalls == 0, "calls source=%d model=%d tool=%d, want 1/1/0", f.sourceCalls, f.modelCalls, f.toolCalls)
	testassert.Require(t, len(f.reqs[0].Tools) == 1 && f.reqs[0].Tools[0].Name == "t", "request tools = %#v, want the one advertised definition", f.reqs[0].Tools)
	testassert.Require(t, len(f.reqs[0].Messages) == 1 && f.reqs[0].Messages[0].TextContent() == "go", "request messages = %#v, want the fresh snapshot", f.reqs[0].Messages)
}

// TestRunNoAdvertisedToolsSucceeds pins that an absent advertised list is a valid entry value — tool advertisement selection is not Agent's to make.
func TestRunNoAdvertisedToolsSucceeds(t *testing.T) {
	f := &runFakes{}
	out := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = readyOutputs(out)
	inv := f.invocation()
	inv.Tools = nil

	res, err := Run(context.Background(), inv)
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, len(f.reqs[0].Tools) == 0, "request carries tools %#v", f.reqs[0].Tools)
}

// TestRunAssemblyCallbackProducesSettlement pins the callback wiring: the model effect receives a working assembler bound to the run context, its returned output settles as a ready disposition verbatim, and the handed-over stream is released exactly once.
func TestRunAssemblyCallbackProducesSettlement(t *testing.T) {
	f := &runFakes{}
	stream := newFakeStream(deltaStep(choiceDelta(txtPos(0, "hi"))))
	var cbCalls int
	f.modelSet = func(_ int, _ model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		cbCalls++
		out, err := cb(testRef, stream) // the callback Run supplies must be the shipped assembler: it owns and closes the handed-over stream exactly once.
		if err != nil {
			return ModelSettlement{}, err
		}
		return ModelSettlement{Disposition: DispoReady, Output: &out}, nil
	}

	res, err := Run(context.Background(), f.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, cbCalls == 1 && f.modelCalls == 1, "callback calls=%d model calls=%d, want 1/1", cbCalls, f.modelCalls)
	testassert.Require(t, res.Status == TerminalSuccess && res.LastOutput.Message.TextContent() == "hi", "terminal = %#v, want success carrying the assembled output", res)
	testassert.Require(t, stream.closeCount == 1 && stream.recvsAfterClose == 0, "stream close=%d recvs-after-close=%d, want 1/0", stream.closeCount, stream.recvsAfterClose)
}

// TestRunSubstitutedValidSettlementOutputAccepted pins the documented non-rule: Agent compares the callback's invocation discipline (count, source, error handling) but never compares the callback and settlement outputs for deep or byte equality, so a different otherwise-valid substitution stays a trusted-caller matter, not another protocol path.
func TestRunSubstitutedValidSettlementOutputAccepted(t *testing.T) {
	f := &runFakes{}
	stream := newFakeStream(deltaStep(choiceDelta(txtPos(0, "assembled"))))
	substitute := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = func(_ int, _ model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if _, err := cb(testRef, stream); err != nil {
			return ModelSettlement{}, err
		}
		return ModelSettlement{Disposition: DispoReady, Output: substitute}, nil
	}

	res, err := Run(context.Background(), f.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalSuccess && res.LastOutput == substitute, "terminal = %#v, want success carrying the substituted pointer", res)
	testassert.Equal(t, stream.closeCount, 1)
}

// TestRunRawToolArgumentsPassUnchanged pins byte-for-byte argument transit through every JSON shape — object, array, null, malformed, and absent — into the caller-owned tool boundary; Agent never decodes, rejects, or rewrites them, and the whole batch continues to a final text response.
func TestRunRawToolArgumentsPassUnchanged(t *testing.T) {
	args := []json.RawMessage{json.RawMessage(`{"a":[1,{"b":2}]}`), json.RawMessage(`[1,2]`), json.RawMessage(`null`), json.RawMessage(`{oops`), nil}
	calls := make([]model.ToolCall, 0, len(args))
	for i, a := range args {
		calls = append(calls, model.ToolCall{ID: fmt.Sprintf("k%d", i), Name: fmt.Sprintf("fn%d", i), Arguments: a})
	}
	callsOut := mkOutput(model.OutputCompleted, "", calls)
	f := &runFakes{}
	f.modelSet = readyOutputs(callsOut, mkOutput(model.OutputCompleted, "", nil))

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Equal(t, f.toolCalls, len(args))
	for i, seen := range f.calls {
		want := args[i]
		testassert.Require(t, string(seen.Arguments) == string(want) && len(seen.Arguments) == len(want), "call %d arguments = %q, want verbatim %q", i, string(seen.Arguments), string(want))
		testassert.Require(t, seen.ID == calls[i].ID && seen.Name == calls[i].Name, "call %d identity = %q/%q, want %q/%q", i, seen.ID, seen.Name, calls[i].ID, calls[i].Name)
	}
}
