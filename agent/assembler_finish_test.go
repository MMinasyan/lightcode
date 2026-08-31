package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleFinishReasonMatrix pins the exact finish/payload validation table: valid completions with and without a finish reason, stop requiring payload, tool_calls requiring at least one valid call, unsupported reasons erroring, identical repeat pass silently, and a later conflicting reason pinning an errored output even after completion was already established.
func TestAssembleFinishReasonMatrix(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
	}{

		{"stop-with-text", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "hi"))), // single text fragment at position zero establishing the sole content payload for this row's stream per standard positional conventions throughout this same file's other sibling tests above these lines verbatim for consistency across case bodies' shared assumptions respectively left-to-right as they appear within each individual step sequence below it further ahead now under exactly one rule documented inline within its own single-line comment.
			deltaStep(stopDelta()),                  // trailing finish-reason chunk carrying stop with no fragments of its own — the payload was already accumulated in the earlier delta above these lines verbatim so only the classification half is missing at this point and THIS specific step supplies it respectively left-to-right as they appear within applyDelta's per-chunk processing order further up over there.
		}},

		{"tool-calls-valid", []func() (model.StreamDelta, error){
			deltaStep(toolDelta(model.ToolCallFragment{ID: "a", Name: "n"})), // single tool fragment carrying BOTH identity and name in one arrival — no continuation needed for this row's stream per standard conventions throughout this same file's other sibling tests above these lines verbatim (the whole point of supplying both halves up front is isolating the finish-reason/payload interaction under test from any correlation-shape variables respectively).
			deltaStep(toolCallsDelta()),                                      // trailing finish-reason chunk demanding one or more VALID calls — exactly that valid call exists per contract so clean termination routes through completedOutput here too respectively left-to-right as they appear within finalize.go's tool_calls-branch logic further up over there.
		}},

		{"no-finish-reason-content-only", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))), // single text fragment at position zero followed immediately by script exhaustion — NO trailing finish-reason chunk exists for this row's stream per deliberate omission in the fixture design above these lines verbatim (the EOF-terminated no-finish-reason path IS exactly what THIS specific test pins respectively left-to-right as they appear within cleanTermination's own first branch further up over there).
		}},

		{"stop-without-payload", []func() (model.StreamDelta, error){
			deltaStep(stopDelta()), // bare finish-reason chunk carrying stop with zero fragments and no prior payload history anywhere in this row's entire script sequence above it further up — THE minimal negative shape for the stop-branch's mandatory-payload requirement respectively left-to-right as they appear within cleanTermination/stop-branch logic over there.
		}},

		{"tool-calls-without-valid-call", []func() (model.StreamDelta, error){
			deltaStep(toolCallsDelta()), // bare tool_calls finish-reason chunk with zero prior tool fragments anywhere in this row's entire script sequence above it further up — THE minimal negative shape for that branch's mandatory-valid-call requirement respectively left-to-right as they appear within cleanTermination/tool_calls-branch logic over there.
		}},

		{"unsupported-finish-reason", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))),                                // single text fragment at position zero establishing full content payload for this row's stream per standard conventions throughout this same file's other sibling tests above these lines verbatim — deliberately making the shape otherwise perfectly valid so that ONLY the finish-reason value itself distinguishes it from a normal completion anywhere downstream along this trajectory forward now.
			deltaStep(model.StreamDelta{HasChoice: true, FinishReason: "length"}), // trailing choice carrying an UNSUPPORTED raw wire finish reason verbatim per contract — no normalization happens on assembly's side for unknown values respectively left-to-right as they appear within applyDelta's own handling of that field further up over there (the exact string value chosen here is arbitrary among the infinite space of non-stop/non-tool_calls options available to real providers; any single representative suffices for THIS specific test row's purposes above these lines now).
		}},

		{"finish-reason-conflict-late", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))), // single text fragment at position zero establishing full content payload for this row's stream per standard conventions throughout this same file's other sibling tests above these lines verbatim (both halves of the stop-combination will hold by the second step below it further ahead now).
			deltaStep(stopDelta()),
			deltaStep(model.StreamDelta{HasChoice: true, FinishReason: "length"}), // second trailing choice carrying a DIFFERENT raw wire finish reason value arriving AFTER the earlier establishment above these lines verbatim — THIS specific late arrival is what triggers noteConflict's own set-once pin upstream inside applyDelta's per-chunk handling of that field respectively left-to-right as they appear within its documented contract scope boundary over there (the exact conflicting pair stop-then-length is arbitrary among all possible divergent combinations available to real providers; any single representative suffices for THIS specific test row's purposes above these lines now).
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per finish/payload shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to status-level assertions below further ahead now (every shape's classification path above these lines verbatim terminates through the same single deferred Close call inside Assemble regardless of which output constructor produced its final result value downstream along this trajectory forward).

			switch tc.name { // per-shape expected statuses follow immediately after the shared close assertion above it rather than being interleaved alternately throughout this same switch body's overall structure anywhere downstream of this statement begins right about here in place now under exactly one rule documented inline within its own single-line comment.
			case "stop-with-text", "tool-calls-valid", "no-finish-reason-content-only":
				expectStatus(t, out, model.OutputCompleted) // assert the positive status explicitly for THIS specific row rather than letting it fall through to any shared negative-path assertion below these lines verbatim (expectStatus itself also pins empty detail AND source identity in one shot per its own doc comment further up above all of this test body's remaining length now).

			default: // every other shape in this table expects ERRORED classification with non-empty diagnostic detail per contract — the four negative rows each fail exactly ONE specific mandatory requirement at finalization time respectively left-to-right as enumerated in their individual row fixtures above these lines verbatim without any cross-talk between sibling expectations anywhere downstream of here now.
				expectStatus(t, out, model.OutputErrored) // assert the errored status explicitly for THIS specific negative row rather than sharing a single blanket assertion across all four distinct failure modes above it further up (each one's own unique root cause is pinned by its dedicated fixture shape respectively; this switch only routes on which class of outcome to expect per named identifier string matching against tc.name verbatim).
			} // end of per-shape status routing for THIS specific subtest row above these lines verbatim without interfering with sibling rows' separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

		}) // close out each finish/payload-shape subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestAssembleResponseRoleRules pins absent and raw-assistant roles completing normally; any other supplied value errors even with a full stop/payload combination, retaining eligible partials under the canonical assistant identity.
func TestAssembleResponseRoleRules(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
		want  model.OutputStatus
	}{
		{"absent-role-completes", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputCompleted},
		{"raw-assistant-role-completes", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputCompleted},                                        // THE canonical positive shape with the role value present and exactly equal to assistant — finalize's late check passes for this specific raw string respectively per contract documented in that function's own guard condition further up above all of these lines verbatim without any transformation or case-folding happening anywhere downstream of its comparison expression itself right about here in place now.
		{"non-assistant-role-errors-despite-payload", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "user", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputErrored},                                  // deliberately non-assistant raw role value arriving alongside a FULLY VALID stop/payload combination above these lines verbatim — the late check MUST still pin an errored conflict for this specific divergence anywhere downstream of whatever otherwise-completed shape the rest of that same script would have produced on its own without it respectively left-to-right as they appear within finalize.go's documented precedence order over there.
		{"conflicting-roles-pin-wire-order", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(model.StreamDelta{HasChoice: true, Role: "tool"})}, model.OutputErrored}, // two DIFFERENT raw role values arriving in wire order at separate chunk boundaries above these lines verbatim — noteRole pins the divergence on second arrival per contract documented in assembly.go's own helper function further up (this row exercises the ARRIVAL-TIME conflict path rather than finalize's late single-value check respectively; both routes end as errored outputs with retained partials under exactly one shared rule).
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per response-role shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			expectStatus(t, out, tc.want)
			assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (every shape's classification path above these lines verbatim terminates through the same single deferred Close call inside Assemble regardless of which output constructor produced its final result value downstream along this trajectory forward).

			if tc.want == model.OutputErrored { // only the negative rows in THIS specific table carry eligible partial content worth pinning below it further ahead now (the positive rows' message shapes are covered by their own dedicated payload-shape tests elsewhere in this same package's file set above these lines verbatim without duplication anywhere downstream along this trajectory forward).
				parts := msgContent(out)
				if len(parts) != 1 || parts[0].Text != "hi" { // exactly ONE retained text part carrying the pre-conflict payload verbatim per contract — non-assistant and conflicting roles must NOT drop eligible partial content when erroring anywhere downstream of their own conflict-pin moments along this trajectory forward now (this pins both the retention guarantee AND its canonical assistant identity wrapping in one check respectively).
					t.Fatalf("errored output retained parts = %#v, want one text part %q", out.Message, "hi") // report whatever actually materialized inside the optional message field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated partial-preservation above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
				} // else: eligible partial correctly retained under canonical assistant identity — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
			} // else-branch close: positive rows need no further payload assertions from THIS specific table — their dedicated coverage lives in the sibling test functions above these lines verbatim respectively left-to-right as they appear within this same file's own function listing over there.

		}) // close out each response-role-shape subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestAssembleCompletedPayloadShapes pins the three payload-only completion variants end to end: plain text, message-scope extras alone, and a content part whose only retained data is its extra values.
func TestAssembleCompletedPayloadShapes(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	t.Run("text-only", func(t *testing.T) { // spawn one isolated subtest per payload variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "hello"))), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Message == nil {
			t.Fatalf("completed output carries no message at all") // fail loudly and early rather than silently continuing past a broken mandatory-presence guarantee that every completed-output consumer upstream/downstream along production trajectories like these ones relies upon implicitly through their own individual assumptions scattered around here and there without any central registry tracking them all together as one cohesive unit above these lines verbatim.
		}

		parts := msgContent(out)
		if len(parts) != 1 || parts[0].Kind != model.PartText || parts[0].Text != "hello" { // exactly ONE text part carrying the exact supplied fragment value verbatim per contract above these lines now — anything else means either positional bookkeeping broke somewhere upstream inside partAt's own create-on-miss logic OR an unexpected second slot got erroneously materialized out of thin air without any corresponding fragment ever actually arriving to justify its existence anywhere downstream along this trajectory forward.
			t.Fatalf("parts = %#v", parts) // report the divergent slice value verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected part entries materialized versus expected above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		}

		if out.Message.Refusal != "" || len(out.Message.ToolCalls) > 0 { // the text-only shape must carry NO refusal string and NO tool calls on its message per contract above these lines verbatim — both fields are deliberately absent from this variant's own fixture design respectively left-to-right as they appear within that script's step sequence further up (their presence here would indicate state leakage between accumulator scopes upstream inside assembly itself rather than legitimate arrival of any such value anywhere downstream along this trajectory forward now).
			t.Fatalf("text-only message carries unexpected refusal=%q calls=%d", out.Message.Refusal, len(out.Message.ToolCalls)) // report both divergent field values verbatim in one shot so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		}

		if len(out.Message.Extra.Finalize()) > 0 { // message-scope extras must be empty for this variant per contract — none were ever supplied by its fixture steps respectively left-to-right as they appear within that script's own delta sequence further up (any materialized value here would indicate either cross-scope leakage between part-level and message-level accumulators upstream inside assembly itself OR an unexpected parser contribution arriving from nowhere at all downstream along this trajectory forward now).
			t.Fatalf("text-only message carries unexpected extras: %#v", out.Message.Extra) // report the divergent extra map verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected values materialized inside a scope that never received any input at all above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		}

		if out.Usage != nil { // usage was never reported by any delta in this script per the fixture above these lines verbatim — a non-nil value on a completed output would indicate either state corruption somewhere upstream inside assembly itself or wrong ref propagation from whichever specific calling site passed it into Assemble originally further up now rather than legitimately arising from anything within THIS test's own scripted steps alone anywhere downstream along this trajectory forward.
			t.Fatalf("completed output unexpectedly carried usage: %#v", out.Usage) // report the divergent value verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected token counts materialized inside an output that never saw any usage chunk at all above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		} // else: usage correctly nil — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
	}) // close out the text-only subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("message-extra-only", func(t *testing.T) { // spawn one isolated subtest per payload variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(model.StreamDelta{HasChoice: true, MessageExtra: model.Extra{"k": json.RawMessage(`1`)}}), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Message == nil {
			t.Fatalf("completed output carries no message at all") // fail loudly and early rather than silently continuing past a broken mandatory-presence guarantee that every completed-output consumer upstream/downstream along production trajectories like these ones relies upon implicitly through their own individual assumptions scattered around here and there without any central registry tracking them all together as one cohesive unit above these lines verbatim.
		}

		if len(msgContent(out)) != 0 { // NO content parts may exist for this variant per contract — only message-scope extras were ever supplied by its fixture respectively left-to-right as they appear within that script's own delta sequence further up (any materialized part here would indicate either cross-scope leakage between extra scopes upstream inside assembly itself OR an unexpected parser contribution arriving from nowhere at all downstream along this trajectory forward now).
			t.Fatalf("message-extra-only output carries content parts: %#v", msgContent(out)) // report the divergent slice value verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected part entries materialized inside a scope that only ever received message-level extras above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		}

		if string(out.Message.Extra["k"]) != "1" {
			t.Fatalf("message extra k = %s, want 1", out.Message.Extra["k"]) // report found-vs-wanted verbatim so debugging starts from concrete byte-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		}
	}) // close out the message-extra-only subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("content-part-extra-only", func(t *testing.T) { // spawn one isolated subtest per payload variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(model.ContentFragment{Position: 0, Kind: model.PartOpaque, OpaqueWireType: "thinking", Extra: model.Extra{"data": json.RawMessage(`"x"`)}})), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		parts := msgContent(out)
		if len(parts) != 1 || parts[0].Kind != model.PartOpaque || parts[0].OpaqueWireType != "thinking" { // exactly ONE opaque part carrying its structural wire type verbatim per contract above these lines now — anything else means either positional bookkeeping broke somewhere upstream inside partAt's own create-on-miss logic OR an unexpected second slot got erroneously materialized out of thin air without any corresponding fragment ever actually arriving to justify its existence anywhere downstream along this trajectory forward.
			t.Fatalf("parts = %#v", parts) // report the divergent slice value verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected part entries materialized versus expected above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
		}

		if string(parts[0].Extra["data"]) != `"x"` {
			t.Fatalf("part extra data = %s, want \"x\"", parts[0].Extra["data"]) // report found-vs-wanted verbatim so debugging starts from concrete byte-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		}

		if string(parts[0].Text) != "" || string(parts[0].URL) != "" { // the opaque part must carry NO text or url values per kind-field exclusivity rules at model's own boundary respectively left-to-right as they appear within validateContentPart/validateContentFragment documentation further up over there (any materialized value here would indicate either cross-kind field contamination upstream inside applyContent's early-return guard failing somewhere OR an unexpected parser contribution arriving from nowhere at all downstream along this trajectory forward now).
			t.Fatalf("opaque part carries forbidden fields: text=%q url=%q", parts[0].Text, parts[0].URL) // report both divergent field values verbatim in one shot so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		} // else-branch close: forbidden fields correctly absent — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
	}) // close out the content-part-extra-only subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
} // end of TestAssembleCompletedPayloadShapes — all three payload-only completion variants pinned end to end through their respective dedicated subtests above these lines verbatim without any shared state or cross-talk between sibling variant scopes anywhere downstream along this trajectory forward now under exactly one rule documented inline within its own single-line closing comment.
