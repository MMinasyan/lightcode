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
	tools          func(call int) ([]model.ToolDefinition, error)
	modelSet       func(call int, req model.Request, cb AssemblyCallback) (ModelSettlement, error)
	toolRes        func(call int, c model.ToolCall) (model.ToolResult, error)
	noCallbackFill bool
	sourceCalls    int
	modelCalls     int
	toolCalls      int
	reqs           []model.Request
	ads            [][]string
	calls          []model.ToolCall
}

func (f *runFakes) request() RequestSource {
	return func(context.Context) (model.Request, error) {
		f.sourceCalls++
		msgs, tools := userSnapshot("go"), advTools()
		var err error
		if f.snapshot != nil {
			if msgs, err = f.snapshot(f.sourceCalls); err != nil {
				return model.Request{}, err
			}
		}
		if f.tools != nil {
			if tools, err = f.tools(f.sourceCalls); err != nil {
				return model.Request{}, err
			}
		}
		return model.Request{Messages: msgs, Tools: tools}, nil
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
	return func(_ context.Context, c model.ToolCall, names []string) (model.ToolResult, error) {
		f.toolCalls++
		f.calls = append(f.calls, c)
		f.ads = append(f.ads, names)
		if f.toolRes == nil {
			return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
		}
		return f.toolRes(f.toolCalls, c)
	}
}

func (f *runFakes) invocation() Invocation {
	return Invocation{ExpectedModel: testRef, Request: f.request(), ModelEffect: f.model(), ToolEffect: f.tool()}
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

// TestRunInvalidInvocation pins every invalid entry value returning the typed boundary-protocol error with boundary "agent" and a zero terminal result before any caller function runs: a nil run context is rejected, never normalized, and the same no-run guarantee holds for every other row. Invalid per-round requests are rejected at the round's single validation point before any model or tool boundary runs.
func TestRunInvalidInvocation(t *testing.T) {
	rows := []struct {
		name string
		ctx  func() context.Context // nil return supplies a literal nil context to Run.
		mod  func(inv *Invocation)
	}{
		{"nil-run-context", func() context.Context { return nil }, nil},
		{"zero-expected-model", nil, func(inv *Invocation) { inv.ExpectedModel = model.ModelRef{} }},
		{"partial-expected-model", nil, func(inv *Invocation) { inv.ExpectedModel = model.ModelRef{Provider: "acme"} }},
		{"nil-request-source", nil, func(inv *Invocation) { inv.Request = nil }},
		{"nil-model-effect", nil, func(inv *Invocation) { inv.ModelEffect = nil }},
		{"nil-tool-effect", nil, func(inv *Invocation) { inv.ToolEffect = nil }},
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

	roundRows := []struct {
		name string
		req  func(f *runFakes)
	}{
		{"invalid-message", func(f *runFakes) {
			f.snapshot = func(int) ([]model.Message, error) {
				return []model.Message{{Role: "bogus", Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}, nil
			}
		}},
		{"empty-tool-name", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "", Parameters: json.RawMessage(`{}`)}}, nil
			}
		}},
		{"duplicate-tool-names", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`{}`)}, {Name: "t", Parameters: json.RawMessage(`{}`)}}, nil
			}
		}},
		{"array-parameters", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`[1,2]`)}}, nil
			}
		}},
		{"primitive-parameters", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`3`)}}, nil
			}
		}},
		{"null-parameters", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "t", Parameters: json.RawMessage(`null`)}}, nil
			}
		}},
		{"absent-parameters", func(f *runFakes) {
			f.tools = func(int) ([]model.ToolDefinition, error) {
				return []model.ToolDefinition{{Name: "t"}}, nil
			}
		}},
	}
	for _, row := range roundRows {
		t.Run(row.name, func(t *testing.T) {
			f := &runFakes{}
			row.req(f)

			res, err := Run(context.Background(), f.invocation())
			requireBoundaryViolation(t, err, "agent")
			requireZeroTerminal(t, res)
			testassert.Require(t, f.modelCalls == 0 && f.toolCalls == 0, "rejected request ran caller effects: model=%d tool=%d", f.modelCalls, f.toolCalls)
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

// TestRunNoAdvertisedToolsSucceeds pins that a request with an absent advertised list is a valid round value — tool advertisement selection is not Agent's to make.
func TestRunNoAdvertisedToolsSucceeds(t *testing.T) {
	f := &runFakes{}
	out := mkOutput(model.OutputCompleted, "", nil)
	f.modelSet = readyOutputs(out)
	f.tools = func(int) ([]model.ToolDefinition, error) { return nil, nil }

	res, err := Run(context.Background(), f.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Require(t, len(f.reqs[0].Tools) == 0, "request carries tools %#v", f.reqs[0].Tools)
	testassert.Require(t, len(f.ads) == 0 && f.toolCalls == 0, "dispatch names = %v tool calls = %d, want none", f.ads, f.toolCalls)
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

// TestRunRawToolArgumentsPassUnchanged pins byte-for-byte argument transit through every JSON shape — object, array, null, malformed, and absent — into the caller-owned tool boundary; Agent never decodes, rejects, or rewrites them, and the whole batch continues to a final text response. The final rows pin the accepting-boundary ownership at dispatch: a tool effect mutating the argument bytes and extra values it received in place cannot rewrite the settled output's own call records — neither a success terminal's last output nor an interruption terminal's retained output and unstarted suffix.
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

	// A tool effect mutating the accepted call's argument bytes and extra values in place leaves the settled output's own records untouched.
	arg := json.RawMessage(`{"keep":1}`)
	mutCalls := []model.ToolCall{
		{ID: "u0", Name: "fn0", Arguments: arg, Extra: model.Extra{"k": json.RawMessage(`1`)}},
		{ID: "u1", Name: "fn1", Arguments: arg, Extra: model.Extra{"k": json.RawMessage(`1`)}},
	}
	mutOut := mkOutput(model.OutputCompleted, "", mutCalls)
	fm := &runFakes{}
	fm.modelSet = readyOutputs(mutOut, mkOutput(model.OutputCompleted, "", nil))
	fm.toolRes = func(_ int, c model.ToolCall) (model.ToolResult, error) {
		if len(c.Arguments) > 0 {
			c.Arguments[0] = 'Z'
		}
		c.Extra["k"] = json.RawMessage(`999`)
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}
	res, err = Run(context.Background(), fm.invocation())
	testassert.Require(t, err == nil && res.Status == TerminalSuccess, "run = %#v, %v; want success", res, err)
	testassert.Equal(t, fm.toolCalls, 2)
	for i, seen := range fm.calls {
		testassert.Require(t, string(seen.Arguments) == `Z"keep":1}` && string(seen.Extra["k"]) == `999`, "dispatched call %d shows no mutation: arguments=%q extra=%q", i, string(seen.Arguments), string(seen.Extra["k"]))
	}
	for i, oc := range mutOut.Message.ToolCalls {
		testassert.Require(t, string(oc.Arguments) == `{"keep":1}`, "settled output call %d arguments = %q, want the unmutated %q", i, string(oc.Arguments), `{"keep":1}`)
		testassert.Require(t, string(oc.Extra["k"]) == `1`, "settled output call %d extra k = %q, want the unmutated 1", i, string(oc.Extra["k"]))
	}

	// The same guarantee for an interruption terminal: the mutated dispatched call settles interrupted, the retained output and the unstarted suffix carry the original bytes.
	intOut := mkOutput(model.OutputCompleted, "", []model.ToolCall{
		{ID: "v0", Name: "fn0", Arguments: arg, Extra: model.Extra{"k": json.RawMessage(`1`)}},
		{ID: "v1", Name: "fn1", Arguments: arg, Extra: model.Extra{"k": json.RawMessage(`1`)}},
		{ID: "v2", Name: "fn2", Arguments: arg, Extra: model.Extra{"k": json.RawMessage(`1`)}},
	})
	fi := &runFakes{}
	fi.modelSet = readyOutputs(intOut)
	fi.toolRes = func(call int, c model.ToolCall) (model.ToolResult, error) {
		if len(c.Arguments) > 0 {
			c.Arguments[0] = 'Z'
		}
		c.Extra["k"] = json.RawMessage(`999`)
		if call == 2 {
			return model.ToolResult{CallID: c.ID, Status: model.ResultInterrupted, Content: "stopped"}, nil
		}
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}
	res, err = Run(context.Background(), fi.invocation())
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == TerminalInterruption && res.LastOutput == intOut, "terminal = %#v, want interruption carrying the settled output", res)
	testassert.Require(t, len(res.UnstartedCalls) == 1 && res.UnstartedCalls[0].ID == "v2", "unstarted calls = %v, want only v2", joinedIDs(res.UnstartedCalls))
	for i, oc := range intOut.Message.ToolCalls {
		testassert.Require(t, string(oc.Arguments) == `{"keep":1}` && string(oc.Extra["k"]) == `1`, "retained output call %d was rewritten: arguments=%q extra=%q", i, string(oc.Arguments), string(oc.Extra["k"]))
	}
	for _, oc := range res.UnstartedCalls {
		testassert.Require(t, string(oc.Arguments) == `{"keep":1}` && string(oc.Extra["k"]) == `1`, "unstarted call %s was rewritten: arguments=%q extra=%q", oc.ID, string(oc.Arguments), string(oc.Extra["k"]))
	}
}
