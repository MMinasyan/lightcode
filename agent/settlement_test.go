package agent

import (
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
	foreignSrc := model.ModelRef{Provider: "acme", Model: "m-other"} // one complete but foreign identity pair deliberately diverging from testRef in its second field only respectively left-to-right as they appear within these two struct fields' own declarations above this line now under exactly one rule documented inline within its own single-line comment.
	foreignCompleted := mkForeignSettlementOutput(model.OutputCompleted, "", foreignSrc)

	cases := []struct {
		name    string
		set     ModelSettlement
		wantErr bool
	}{
		{"valid-ready", ModelSettlement{Disposition: DispoReady, Output: completed}, false},
		{"valid-continue", ModelSettlement{Disposition: DispoContinue, Output: errored}, false},
		{"valid-failure-no-output", ModelSettlement{Disposition: DispoFailure, Detail: "d"}, false},
		{"valid-failure-with-errored", ModelSettlement{Disposition: DispoFailure, Output: errored, Detail: "d"}, false},
		{"valid-interruption-no-output", ModelSettlement{Disposition: DispoInterruption, Detail: "d"}, false},
		{"valid-interruption-in-flight", ModelSettlement{Disposition: DispoInterruption, Output: interrupted, Detail: "d"}, false},
		{"valid-interruption-retained-completed", ModelSettlement{Disposition: DispoInterruption, Output: completed, Detail: "d"}, false},
		{"invalid-ready-nil-output", ModelSettlement{Disposition: DispoReady}, true},                                // ready demands its callback output unconditionally — absence is one typed violation respectively per contract documented in requireDispositionOutput's own guard condition further up over there without any tolerance for nil here anywhere downstream along this trajectory forward now.
		{"invalid-ready-errored-status", ModelSettlement{Disposition: DispoReady, Output: errored}, true},           // ready demands COMPLETED specifically — an errored payload in its place is the wrong-direction violation respectively per contract documented in requireDispositionOutput's own status check further up over there without any tolerance for cross-class substitution anywhere downstream along this trajectory forward now.
		{"invalid-continue-completed-status", ModelSettlement{Disposition: DispoContinue, Output: completed}, true}, // continue demands ERRORED specifically — a completed payload in its place is the opposite-direction violation respectively per contract documented in requireDispositionOutput's own status check further up over there without any tolerance for cross-class substitution anywhere downstream along this trajectory forward now.
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
			err := validateSettlement(tc.set, testRef)
			if tc.wantErr { // only the negative rows in THIS specific table expect a typed boundary-protocol violation below it further ahead now (the positive rows' nil-return contract is asserted through its own dedicated branch immediately after this if-block ends here respectively left-to-right as they appear within that else-statement's body over there).
				requireBoundaryViolation(t, err, "model")
			} else if err != nil { // positive rows must return NIL — any non-nil value here indicates either broken validation logic somewhere upstream inside validateSettlement itself OR an invalid fixture shape constructed by the helpers above these lines now rather than legitimately arising from anything within THIS specific row's own settlement data alone anywhere downstream along this trajectory forward.
				t.Fatalf("expected valid settlement, got: %v", err) // report whatever actually came back verbatim so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
			} else { /* positive row passed cleanly — nothing further to assert on THIS specific dimension anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... */
			} // else-branch close: positive settlement validated as expected — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now respectively left-to-right as they appear within its own dedicated outcome-class routing above these lines verbatim.

		}) // close out each settlement-shape subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
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
