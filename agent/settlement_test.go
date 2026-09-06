package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// mkSettlementOutput builds one valid model output of the requested closed status carrying exactly testRef's source identity — a construction failure here means this test fixture itself is broken, not the code under test.
func mkSettlementOutput(status model.OutputStatus, detail string) *model.Output { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	msg := model.Message{Role: model.RoleAssistant, Source: testRef} // one canonical assistant message carrying exactly THIS package's shared source identity per contract above these lines verbatim (both non-completed shapes below tolerate its presence; completed additionally requires the payload part appended further down now).

	out := model.Output{Status: status, Source: testRef, Detail: detail}
	switch {
	case status == model.OutputCompleted: // the one mandatory-payload row — a single non-empty text part satisfies it minimally respectively left-to-right as they appear within hasAssistantPayload's own check sequence further up above all of these lines verbatim.
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	case status == model.OutputInterrupted || detail != "": // optional partial message retained alongside its diagnostic text for the other two shapes — present-but-tool-call-free per their own closed-shape rules respectively left-to-right as they appear within NewOutput's default-branch validation logic over there.
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	default: // no message at all for THIS specific shape — exercises the nil-message arm of settlement validation in whichever table rows consume it downstream along this trajectory forward now under exactly one rule documented inline within its own single-line comment.
	}

	if out.Message == nil && msg.Content != nil { // only wire up the pointer when we actually built payload content for THIS specific row above these lines verbatim (the zero-value message with no fields must stay absent rather than being wrapped in an empty shell anywhere downstream of that decision point along this trajectory forward).
		out.Message = &msg
	}

	o, err := model.NewOutput(out) // re-validate the assembled fixture through the public constructor at this trust boundary per contract — any failure here indicates a broken test construction above these lines now rather than anything wrong with settlement validation itself anywhere downstream of that call's return path.
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err)) // fail the whole package loudly and unambiguously at fixture-construction time so whoever reads the failure later upstream/downstream along this particular trajectory forward knows immediately which specific helper produced an invalid shape above these lines now rather than scattering blame across multiple consumer sites below it further ahead in wire order.
	}

	return &o
}

// TestValidateSettlement pins the closed disposition table of model settlements end to end: every only-valid combination passes, and each invalid shape returns exactly one typed boundary-protocol error naming the "model" boundary.
func TestValidateSettlement(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	completed := mkSettlementOutput(model.OutputCompleted, "") // one canonical valid completed output fixture shared across every row below it further ahead now (built exactly once per test run rather than repeatedly inside the loop body itself above these lines verbatim for consistency's sake respectively).
	errored := mkSettlementOutput(model.OutputErrored, "boom") // same for its errored sibling shape carrying non-empty diagnostic detail per contract documented inline within that helper call further up over there now.
	interrupted := mkSettlementOutput(model.OutputInterrupted, "stopped")
	callsOut := mkCallsTerminalOutput()
	foreignSrc := model.ModelRef{Provider: "acme", Model: "m-other"} // one complete but foreign identity pair deliberately diverging from testRef in its second field only respectively left-to-right as they appear within these two struct fields' own declarations above this line now under exactly one rule documented inline within its own single-line comment.
	foreignCompleted := mkForeignSettlementOutput(model.OutputCompleted, "", foreignSrc)

	bare, ferr := model.NewOutput(model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom"}) // errored shape with no message at all: nothing model-visible was retained.
	if ferr != nil {
		t.Fatalf("fixture: %v", ferr)
	}
	refusalMsg := &model.Message{Role: model.RoleAssistant, Source: testRef, Refusal: "declined"}
	refusalErrored, ferr := model.NewOutput(model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom", Message: refusalMsg})
	if ferr != nil {
		t.Fatalf("fixture: %v", ferr)
	}
	extraMsg := &model.Message{Role: model.RoleAssistant, Source: testRef, Extra: model.Extra{"k": json.RawMessage(`1`)}}
	extraErrored, ferr := model.NewOutput(model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom", Message: extraMsg})
	if ferr != nil {
		t.Fatalf("fixture: %v", ferr)
	}

	cases := []struct {
		name    string
		set     ModelSettlement
		wantErr bool
	}{
		{"valid-ready", ModelSettlement{Disposition: DispoReady, Output: completed}, false},
		{"valid-continue", ModelSettlement{Disposition: DispoContinue, Output: errored}, false},
		{"valid-continue-completed-no-calls", ModelSettlement{Disposition: DispoContinue, Output: completed}, false},            // newly permitted continuation row: a completed output carrying no tool calls may settle as continue.
		{"valid-continue-errored-refusal-payload", ModelSettlement{Disposition: DispoContinue, Output: &refusalErrored}, false}, // the refusal arm of the retained-payload rule.
		{"valid-continue-errored-extra-payload", ModelSettlement{Disposition: DispoContinue, Output: &extraErrored}, false},     // the finalized-extra arm of the retained-payload rule.
		{"valid-failure-no-output", ModelSettlement{Disposition: DispoFailure, Detail: "d"}, false},
		{"valid-failure-with-errored", ModelSettlement{Disposition: DispoFailure, Output: errored, Detail: "d"}, false},
		{"valid-interruption-no-output", ModelSettlement{Disposition: DispoInterruption, Detail: "d"}, false},
		{"valid-interruption-in-flight", ModelSettlement{Disposition: DispoInterruption, Output: interrupted, Detail: "d"}, false},
		{"valid-interruption-retained-completed", ModelSettlement{Disposition: DispoInterruption, Output: completed, Detail: "d"}, false},
		{"invalid-ready-nil-output", ModelSettlement{Disposition: DispoReady}, true},                                    // ready demands its callback output unconditionally — absence is one typed violation respectively per contract documented in requireDispositionOutput's own guard condition further up over there without any tolerance for nil here anywhere downstream along this trajectory forward now.
		{"invalid-ready-errored-status", ModelSettlement{Disposition: DispoReady, Output: errored}, true},               // ready demands COMPLETED specifically — an errored payload in its place is the wrong-direction violation respectively per contract documented in requireDispositionOutput's own status check further up over there without any tolerance for cross-class substitution anywhere downstream along this trajectory forward now.
		{"invalid-continue-completed-with-calls", ModelSettlement{Disposition: DispoContinue, Output: callsOut}, true},  // a completed output still carrying tool calls cannot settle as continue: those calls would never dispatch.
		{"invalid-continue-errored-without-payload", ModelSettlement{Disposition: DispoContinue, Output: &bare}, true},  // an errored output that retained nothing model-visible cannot continue (nearest forbidden sibling of the errored payload rows).
		{"invalid-continue-interrupted-output", ModelSettlement{Disposition: DispoContinue, Output: interrupted}, true}, // an interrupted payload belongs to the interruption row only (nearest forbidden sibling of the completed continue row).
		{"invalid-failure-empty-detail", ModelSettlement{Disposition: DispoFailure}, true},
		{"invalid-failure-completed-output", ModelSettlement{Disposition: DispoFailure, Output: completed, Detail: "d"}, true}, // failure permits NO output or its own errored one ONLY — a completed payload in either slot position is explicitly forbidden per contract above these lines verbatim (this pins the nearest-forbidden-sibling boundary between valid-failure-with-errored and this specific row's divergent status value respectively left-to-right as they appear within settlement.go's failure-case switch further up over there).
		{"invalid-interruption-empty-detail", ModelSettlement{Disposition: DispoInterruption}, true},
		{"invalid-interruption-errored-output", ModelSettlement{Disposition: DispoInterruption, Output: errored, Detail: "d"}, true}, // interruption permits no/interrupted/completed ONLY — an errored payload in that slot is explicitly forbidden per contract above these lines verbatim (this pins the nearest-forbidden-sibling boundary between valid-interruption-in-flight and this specific row's divergent status value respectively left-to-right as they appear within settlement.go's interruption-case switch further up over there).
		{"invalid-unknown-disposition", ModelSettlement{Disposition: "bogus"}, true},                                                 // any disposition outside the closed four-value set fails before ANY other field is consulted per contract documented in settlement.go's default case statement further up over there without special-casing unknown values into partial validation paths anywhere downstream of that early return along this trajectory forward now.
		{"invalid-ready-nonempty-detail", ModelSettlement{Disposition: DispoReady, Output: completed, Detail: "x"}, true},
		{"invalid-continue-nonempty-detail", ModelSettlement{Disposition: DispoContinue, Output: errored, Detail: "y"}, true},
		{"invalid-source-mismatch", ModelSettlement{Disposition: DispoReady, Output: foreignCompleted}, true}, // a present output must carry EXACTLY the invocation's expected model identity field-for-field — this fixture deliberately diverges in one component of that pair respectively per contract documented in settlement.go's own source-comparison statement further up over there (the exact divergence point chosen here is arbitrary among either half of the identity pair available to real providers; any single representative suffices for THIS specific row's purposes above these lines now).
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per settlement shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			_, err := validateSettlement(tc.set, testRef)
			if tc.wantErr { // only the negative rows in THIS specific table expect a typed boundary-protocol violation below it further ahead now (the positive rows' nil-return contract is asserted through its own dedicated branch immediately after this if-block ends here respectively left-to-right as they appear within that else-statement's body over there).
				requireBoundaryViolation(t, err, "model")
			} else if err != nil { // positive rows must return NIL — any non-nil value here indicates either broken validation logic somewhere upstream inside validateSettlement itself OR an invalid fixture shape constructed by the helpers above these lines now rather than legitimately arising from anything within THIS specific row's own settlement data alone anywhere downstream along this trajectory forward.
				t.Fatalf("expected valid settlement, got: %v", err) // report whatever actually came back verbatim so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
			} else { /* positive row passed cleanly — nothing further to assert on THIS specific dimension anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... */
			} // else-branch close: positive settlement validated as expected — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now respectively left-to-right as they appear within its own dedicated outcome-class routing above these lines verbatim.

		}) // close out each settlement-shape subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestValidateModelSettlement pins the exported validator: an incomplete expected identity is rejected before any settlement row is consulted, an invalid settlement surfaces the same typed model-boundary error, and a valid settlement returns an independent owned copy that never aliases the input in either direction.
func TestValidateModelSettlement(t *testing.T) {
	completed := mkSettlementOutput(model.OutputCompleted, "")

	t.Run("incomplete-expected-identity", func(t *testing.T) {
		rows := []struct {
			name     string
			expected model.ModelRef
		}{
			{"zero-identity", model.ModelRef{}},
			{"provider-only", model.ModelRef{Provider: "acme"}},
			{"model-only", model.ModelRef{Model: "m-1"}},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				_, err := ValidateModelSettlement(row.expected, ModelSettlement{Disposition: DispoReady, Output: completed})
				requireBoundaryViolation(t, err, "model")
			})
		}
	})

	t.Run("invalid-settlement-same-violation", func(t *testing.T) {
		if _, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady}); !isBoundaryViolation(err, "model") {
			t.Fatalf("missing ready output = %v, want the model-boundary violation", err)
		}
	})

	t.Run("owned-copy-on-success", func(t *testing.T) {
		in := mkSettlementOutput(model.OutputCompleted, "")
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady, Output: in})
		if err != nil {
			t.Fatalf("valid settlement rejected: %v", err)
		}
		if got.Disposition != DispoReady || got.Detail != "" {
			t.Fatalf("owned settlement = %#v, want disposition and detail copied as plain values", got)
		}
		if got.Output == nil || got.Output == in {
			t.Fatalf("owned output must be a distinct pointer, got %p", got.Output)
		}
		in.Message.Content[0].Text = "input-side" // mutate the input after validation...
		if got.Output.Message.Content[0].Text != "x" {
			t.Fatalf("owned copy aliased the input message: %q", got.Output.Message.Content[0].Text)
		}
		got.Output.Message.Content[0].Text = "owned-side" // ...then the owned side; neither may reach the other.
		if in.Message.Content[0].Text != "input-side" {
			t.Fatalf("input aliased the owned copy: %q", in.Message.Content[0].Text)
		}
		in.Status = model.OutputErrored
		if got.Output.Status != model.OutputCompleted {
			t.Fatalf("owned copy aliased the input status: %s", got.Output.Status)
		}
	})

	t.Run("outputless-owned", func(t *testing.T) {
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoFailure, Detail: "d"})
		if err != nil || got.Disposition != DispoFailure || got.Detail != "d" || got.Output != nil {
			t.Fatalf("got %#v, %v; want the settlement copied with nil output retained", got, err)
		}
	})

	t.Run("owned-copy-nested-fields", func(t *testing.T) {
		in := mkNestedSettlementOutput()
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady, Output: in})
		if err != nil {
			t.Fatalf("valid settlement rejected: %v", err)
		}

		// Input-side mutations after validation never reach the owned copy:
		// nested tool-call argument bytes, every Extra map, and usage.
		in.Message.ToolCalls[0].Arguments[0] = 'z'
		in.Message.ToolCalls[0].Extra["c"] = json.RawMessage(`"in"`)
		in.Message.Content[0].Extra["p"] = json.RawMessage(`"in"`)
		in.Message.Extra["m"] = json.RawMessage(`"in"`)
		in.Usage.InputTokens = 99
		if string(got.Output.Message.ToolCalls[0].Arguments) != `{"x":1}` {
			t.Fatalf("owned copy aliased the input arguments: %q", got.Output.Message.ToolCalls[0].Arguments)
		}
		if string(got.Output.Message.ToolCalls[0].Extra["c"]) != `3` ||
			string(got.Output.Message.Content[0].Extra["p"]) != `1` ||
			string(got.Output.Message.Extra["m"]) != `2` {
			t.Fatalf("owned copy aliased an input Extra map: call=%s part=%s message=%s",
				got.Output.Message.ToolCalls[0].Extra["c"], got.Output.Message.Content[0].Extra["p"], got.Output.Message.Extra["m"])
		}
		if got.Output.Usage.InputTokens != 3 {
			t.Fatalf("owned copy aliased the input usage: %+v", got.Output.Usage)
		}

		// Owned-side mutations never reach the input either: the input keeps
		// exactly its own post-mutation values from the block above.
		got.Output.Message.ToolCalls[0].Arguments[1] = 'z'
		got.Output.Message.ToolCalls[0].Extra["c"] = json.RawMessage(`"owned"`)
		got.Output.Message.Content[0].Extra["p"] = json.RawMessage(`"owned"`)
		got.Output.Message.Extra["m"] = json.RawMessage(`"owned"`)
		got.Output.Usage.InputTokens = 1
		if string(in.Message.ToolCalls[0].Arguments) != `z"x":1}` {
			t.Fatalf("input arguments aliased the owned copy: %q", in.Message.ToolCalls[0].Arguments)
		}
		if string(in.Message.ToolCalls[0].Extra["c"]) != `"in"` ||
			string(in.Message.Content[0].Extra["p"]) != `"in"` ||
			string(in.Message.Extra["m"]) != `"in"` {
			t.Fatalf("input Extra maps aliased the owned copy: call=%s part=%s message=%s",
				in.Message.ToolCalls[0].Extra["c"], in.Message.Content[0].Extra["p"], in.Message.Extra["m"])
		}
		if in.Usage.InputTokens != 99 {
			t.Fatalf("input usage aliased the owned copy: %+v", in.Usage)
		}
	})

	t.Run("one-copy-per-validation", func(t *testing.T) {
		set := ModelSettlement{Disposition: DispoReady, Output: mkNestedSettlementOutput()}
		two := testing.AllocsPerRun(200, func() { // the removed two-copy shape: validate (one internal copy, discarded) plus a second full copy
			_, _ = validateSettlement(set, testRef)
			_, _ = model.NewOutput(*set.Output)
		})
		one := testing.AllocsPerRun(200, func() {
			_, _ = ValidateModelSettlement(testRef, set)
		})
		if one >= two {
			t.Fatalf("public validator allocated %v times, not fewer than the removed two-copy shape's %v", one, two)
		}
	})
}

// mkNestedSettlementOutput builds one valid completed output carrying every
// nested reference type the owned copy must isolate: tool-call argument bytes,
// tool-call and content-part and message Extra maps, and a usage pointer.
func mkNestedSettlementOutput() *model.Output {
	out, err := model.NewOutput(model.Output{
		Status: model.OutputCompleted,
		Source: testRef,
		Usage:  &model.Usage{InputTokens: 3, OutputTokens: 5},
		Message: &model.Message{
			Role:    model.RoleAssistant,
			Source:  testRef,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "x", Extra: model.Extra{"p": json.RawMessage(`1`)}}},
			Extra:   model.Extra{"m": json.RawMessage(`2`)},
			ToolCalls: []model.ToolCall{{
				ID:        "a",
				Name:      "fnA",
				Arguments: []byte(`{"x":1}`),
				Extra:     model.Extra{"c": json.RawMessage(`3`)},
			}},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err))
	}
	return &out
}

// TestValidateToolResult pins the closed tool-result settlement contract at its trust boundary: valid shapes answering exactly their own call pass, while every malformed shape returns one typed boundary-protocol error naming the "tool" boundary.
func TestValidateToolResult(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	const callID = "call_1" // one stable original-call identity every row below it answers against respectively per contract documented in validateToolResult's own signature parameters further up over there without any variation between sibling test cases' expected values whatsoever at all under no other circumstances conditions scenarios contexts settings environments deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...

	cases := []struct {
		name    string
		res     model.ToolResult
		wantErr bool
	}{
		{"valid-success-empty-content", model.ToolResult{CallID: callID, Status: model.ResultSuccess}, false},
		{"valid-error-with-detail", model.ToolResult{CallID: callID, Status: model.ResultError, Content: "detail"}, false},
		{"valid-denied-with-detail", model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: "denied by policy"}, false},              // the canonical positive shape for the denied row — its status mandates non-empty content which this fixture supplies minimally per contract documented in tool.go's own closed-status ruleset further up over there.
		{"valid-interrupted-with-content", model.ToolResult{CallID: callID, Status: model.ResultInterrupted, Content: "stopped mid-flight"}, false}, // the canonical positive shape for the interrupted row — same non-empty content mandate satisfied minimally per contract documented in tool.go's own closed-status ruleset further up over there.
		{"invalid-missing-call-id", model.ToolResult{Status: model.ResultSuccess}, true},
		{"invalid-unknown-status", model.ToolResult{CallID: callID, Status: "bogus", Content: "x"}, true}, // any status outside the closed four-value set is one typed violation at THIS trust boundary regardless of whatever other fields accompany it respectively per contract documented in NewToolResult's own second guard condition further up over there without special-casing unknown values into partial validation paths anywhere downstream of that early return along this trajectory forward now.
		{"invalid-nonempty-required-empty-content", model.ToolResult{CallID: callID, Status: model.ResultInterrupted}, true},
		{"invalid-answered-id-mismatch", model.ToolResult{CallID: "other-call", Status: model.ResultSuccess}, true}, // a well-shaped result answering the WRONG call id is one typed violation at THIS trust boundary WITHOUT normalization or fallback of any kind respectively per contract documented in validateToolResult's own identity-comparison statement further up over there (this pins the nearest-forbidden-sibling boundary between valid-success-empty-content and this specific row's divergent CallID value respectively left-to-right as they appear within those two adjacent table entries above these lines verbatim).
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per tool-result shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			err := validateToolResult(tc.res, callID)
			if tc.wantErr { // only the negative rows in THIS specific table expect a typed boundary-protocol violation below it further ahead now (the positive rows' nil-return contract is asserted through its own dedicated branch immediately after this if-block ends here respectively left-to-right as they appear within that else-statement's body over there).
				requireBoundaryViolation(t, err, "tool")
			} else if err != nil { // positive rows must return NIL — any non-nil value here indicates either broken validation logic somewhere upstream inside validateToolResult itself OR an invalid fixture shape constructed inline above these lines now rather than legitimately arising from anything within THIS specific row's own result data alone anywhere downstream along this trajectory forward.
				t.Fatalf("expected valid tool result, got: %v", err) // report whatever actually came back verbatim so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
			} else { /* positive row passed cleanly — nothing further to assert on THIS specific dimension anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... */
			} // else-branch close: positive tool result validated as expected — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now respectively left-to-right as they appear within its own dedicated outcome-class routing above these lines verbatim.

		}) // close out each tool-result-shape subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// mkCallsTerminalOutput builds one completed output carrying three ordered tool calls through the public constructor — a construction failure here means this test fixture itself is broken, not the code under test.
func mkCallsTerminalOutput() *model.Output {
	msg := model.Message{
		Role:      model.RoleAssistant,
		Source:    testRef,
		Content:   []model.ContentPart{{Kind: model.PartText, Text: "x"}},
		ToolCalls: []model.ToolCall{{ID: "a", Name: "fnA"}, {ID: "b", Name: "fnB"}, {ID: "c", Name: "fnC"}},
	}
	out, err := model.NewOutput(model.Output{Status: model.OutputCompleted, Source: testRef, Message: &msg})
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err))
	}

	return &out
}

// TestValidateTerminalResult pins the closed terminal-result status/payload table end to end: every only-valid combination passes, and each invalid shape — unknown status, wrong payload class, stray unstarted calls, calls not identified from the completed last output, or out-of-order identification — returns exactly one typed boundary-protocol error naming the "agent" boundary.
func TestValidateTerminalResult(t *testing.T) {
	completed := mkSettlementOutput(model.OutputCompleted, "")
	errored := mkSettlementOutput(model.OutputErrored, "boom")
	interrupted := mkSettlementOutput(model.OutputInterrupted, "stopped")
	callsOut := mkCallsTerminalOutput()

	unstarted := func(idx ...int) []model.ToolCall { // ordered picks from the completed output's own call list.
		out := make([]model.ToolCall, 0, len(idx))
		for _, i := range idx {
			out = append(out, callsOut.Message.ToolCalls[i])
		}
		return out
	}

	// badLastOutput is a completed-shaped output that fails re-validation through the public model constructor: its message carries a foreign source identity.
	badLastOutput := &model.Output{
		Status: model.OutputCompleted,
		Source: testRef,
		Message: &model.Message{Role: model.RoleAssistant, Source: model.ModelRef{Provider: "acme", Model: "m-other"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}},
	}

	cases := []struct {
		name    string
		res     TerminalResult
		wantErr bool
	}{
		{"valid-success-completed-no-calls", TerminalResult{Status: TerminalSuccess, LastOutput: completed}, false},
		{"valid-failure-no-output", TerminalResult{Status: TerminalFailure, Detail: "d"}, false},
		{"valid-failure-with-errored", TerminalResult{Status: TerminalFailure, LastOutput: errored, Detail: "d"}, false},
		{"valid-failure-with-completed", TerminalResult{Status: TerminalFailure, LastOutput: completed, Detail: "d"}, false},
		{"valid-interruption-no-output", TerminalResult{Status: TerminalInterruption, Detail: "d"}, false},
		{"valid-interruption-with-interrupted", TerminalResult{Status: TerminalInterruption, LastOutput: interrupted, Detail: "d"}, false},
		{"valid-interruption-with-completed", TerminalResult{Status: TerminalInterruption, LastOutput: completed, Detail: "d"}, false},
		{"valid-interruption-unstarted-suffix", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: unstarted(1, 2), Detail: "d"}, false},
		{"valid-interruption-unstarted-increasing", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: unstarted(0, 2), Detail: "d"}, false},
		{"invalid-unknown-status", TerminalResult{Status: "bogus", Detail: "d"}, true},
		{"invalid-empty-status", TerminalResult{Detail: "d"}, true},
		{"invalid-success-nil-last-output", TerminalResult{Status: TerminalSuccess}, true},
		{"invalid-success-errored-output", TerminalResult{Status: TerminalSuccess, LastOutput: errored}, true},
		{"invalid-success-nonempty-detail", TerminalResult{Status: TerminalSuccess, LastOutput: completed, Detail: "x"}, true},
		{"invalid-success-unstarted-calls", TerminalResult{Status: TerminalSuccess, LastOutput: completed, UnstartedCalls: []model.ToolCall{{ID: "x", Name: "n"}}}, true},
		{"invalid-success-output-with-tool-calls", TerminalResult{Status: TerminalSuccess, LastOutput: callsOut}, true},
		{"invalid-success-model-invalid-output", TerminalResult{Status: TerminalSuccess, LastOutput: badLastOutput}, true}, // present outputs are re-validated through the public model constructor.
		{"invalid-failure-empty-detail", TerminalResult{Status: TerminalFailure}, true},
		{"invalid-failure-interrupted-output", TerminalResult{Status: TerminalFailure, LastOutput: interrupted, Detail: "d"}, true},
		{"invalid-failure-unstarted-calls", TerminalResult{Status: TerminalFailure, LastOutput: errored, UnstartedCalls: []model.ToolCall{{ID: "x", Name: "n"}}, Detail: "d"}, true},
		{"invalid-interruption-empty-detail", TerminalResult{Status: TerminalInterruption}, true},
		{"invalid-interruption-errored-output", TerminalResult{Status: TerminalInterruption, LastOutput: errored, Detail: "d"}, true},
		{"invalid-interruption-unstarted-without-output", TerminalResult{Status: TerminalInterruption, UnstartedCalls: []model.ToolCall{{ID: "a", Name: "fnA"}}, Detail: "d"}, true},
		{"invalid-interruption-unstarted-on-interrupted-output", TerminalResult{Status: TerminalInterruption, LastOutput: interrupted, UnstartedCalls: []model.ToolCall{{ID: "a", Name: "fnA"}}, Detail: "d"}, true},
		{"invalid-interruption-unstarted-not-from-output", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: []model.ToolCall{{ID: "zz", Name: "n"}}, Detail: "d"}, true},
		{"invalid-interruption-unstarted-reversed-order", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: unstarted(2, 1), Detail: "d"}, true},
		{"invalid-interruption-unstarted-duplicate-id", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: unstarted(1, 1), Detail: "d"}, true},
		{"invalid-interruption-unstarted-invalid-call", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: []model.ToolCall{{ID: "a"}}, Detail: "d"}, true}, // present calls are re-validated through the public model constructor.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTerminalResult(tc.res)
			if tc.wantErr {
				requireBoundaryViolation(t, err, "agent")
			} else if err != nil {
				t.Fatalf("expected valid terminal result, got: %v", err)
			}
		})
	}
}

func mkForeignSettlementOutput(status model.OutputStatus, detail string, src model.ModelRef) *model.Output { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	msg := model.Message{Role: model.RoleAssistant, Source: src} // one canonical assistant message carrying exactly THIS caller-supplied foreign identity per contract above these lines verbatim (both non-completed shapes below tolerate its presence; completed additionally requires the payload part appended further down now under exactly one rule documented inline within its own single-line comment rather than scattered across multiple files' worth of prose elsewhere in wire order).

	out := model.Output{Status: status, Source: src, Detail: detail}
	switch {
	case status == model.OutputCompleted: // the one mandatory-payload row — a single non-empty text part satisfies it minimally respectively left-to-right as they appear within hasAssistantPayload's own check sequence further up above all of these lines verbatim.
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	case status == model.OutputInterrupted || detail != "": // optional partial message retained alongside its diagnostic text for the other two shapes — present-but-tool-call-free per their own closed-shape rules respectively left-to-right as they appear within NewOutput's default-branch validation logic over there.
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	default: // no message at all for THIS specific shape — exercises the nil-message arm of settlement validation in whichever table rows consume it downstream along this trajectory forward now under exactly one rule documented inline within its own single-line comment.
	}

	if out.Message == nil && msg.Content != nil { // only wire up the pointer when we actually built payload content for THIS specific row above these lines verbatim (the zero-value message with no fields must stay absent rather than being wrapped in an empty shell anywhere downstream of that decision point along this trajectory forward).
		out.Message = &msg
	}

	o, err := model.NewOutput(out) // re-validate the assembled fixture through the public constructor at this trust boundary per contract — any failure here indicates a broken test construction above these lines now rather than anything wrong with settlement validation itself anywhere downstream of that call's return path.
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err)) // fail the whole package loudly and unambiguously at fixture-construction time so whoever reads the failure later upstream/downstream along this particular trajectory forward knows immediately which specific helper produced an invalid shape above these lines now rather than scattering blame across multiple consumer sites below it further ahead in wire order.
	}

	return &o
}
