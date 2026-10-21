package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleFinishReasonMatrix pins the exact finish/payload validation table: valid completions with and without a finish reason, stop requiring payload, tool_calls requiring at least one valid call, unsupported reasons erroring, identical repeat pass silently, and a later conflicting reason pinning an errored output even after completion was already established.
func TestAssembleFinishReasonMatrix(t *testing.T) {
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
	}{

		{"stop-with-text", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "hi"))),
			deltaStep(stopDelta()),
		}},

		{"tool-calls-valid", []func() (model.StreamDelta, error){
			deltaStep(toolDelta(model.ToolCallFragment{ID: "a", Name: "n"})),
			deltaStep(toolCallsDelta()),
		}},

		{"no-finish-reason-content-only", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))), // Script exhaustion reports EOF; no finish-reason chunk exists.
		}},

		{"stop-without-payload", []func() (model.StreamDelta, error){
			deltaStep(stopDelta()),
		}},

		{"tool-calls-without-valid-call", []func() (model.StreamDelta, error){
			deltaStep(toolCallsDelta()),
		}},

		{"unsupported-finish-reason", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))),
			deltaStep(model.StreamDelta{HasChoice: true, FinishReason: "length"}), // Unknown finish reasons are not normalized.
		}},

		{"finish-reason-conflict-late", []func() (model.StreamDelta, error){
			deltaStep(choiceDelta(txtPos(0, "x"))),
			deltaStep(stopDelta()),
			deltaStep(model.StreamDelta{HasChoice: true, FinishReason: "length"}), // A different finish reason after establishment is a conflict.
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			assertSingleClose(t, s)

			switch tc.name {
			case "stop-with-text", "tool-calls-valid", "no-finish-reason-content-only":
				expectStatus(t, out, model.OutputCompleted)
			default: // Each negative row fails one mandatory finalization requirement.
				expectStatus(t, out, model.OutputErrored)
			}
		})
	}
}

// TestAssembleCleanEmptyResponseErrors pins the truly empty clean termination: a stream ending at EOF with no payload, no calls and no finish reason is an errored response, not a vacuous completion.
func TestAssembleCleanEmptyResponseErrors(t *testing.T) {
	out, s := assemble(t, context.Background(), testRef) // no scripted steps at all — the first Recv reports EOF.
	expectStatus(t, out, model.OutputErrored)
	assertSingleClose(t, s)

	testassert.Require(t, out.Message == nil, "errored empty output unexpectedly carried a message: %#v", out.Message)
}

// TestAssembleRepeatedAgreeingRoleAndFinishReason pins the repeat side of the finalization agreement rules: the same assistant role and the same finish reason arriving across multiple chunks remain one valid completion, with no conflict recorded for identical repeats.
func TestAssembleRepeatedAgreeingRoleAndFinishReason(t *testing.T) {
	roleText := model.StreamDelta{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{txtPos(0, "hi")}}
	repeatRole := model.StreamDelta{HasChoice: true, Role: "assistant"}

	out, s := assemble(t, context.Background(), testRef,
		deltaStep(roleText),
		deltaStep(repeatRole),  // identical non-empty role repeats silently.
		deltaStep(stopDelta()), // first finish reason is accepted.
		deltaStep(stopDelta()), // identical finish reason repeats silently, even after establishment.
	)

	expectStatus(t, out, model.OutputCompleted)
	assertSingleClose(t, s)

	parts := msgContent(out)
	testassert.Require(t, len(parts) == 1 && parts[0].Text == "hi", "completed parts = %#v, want one text part %q", out.Message, "hi")
}

// TestAssembleResponseRoleRules pins absent and raw-assistant roles completing normally; any other supplied value errors even with a full stop/payload combination, retaining eligible partials under the canonical assistant identity.
func TestAssembleResponseRoleRules(t *testing.T) {
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
		want  model.OutputStatus
	}{
		{"absent-role-completes", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputCompleted},
		{"raw-assistant-role-completes", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputCompleted},
		{"non-assistant-role-errors-despite-payload", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "user", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(stopDelta())}, model.OutputErrored},                                  // A non-assistant role errors even with a full payload.
		{"conflicting-roles-pin-wire-order", []func() (model.StreamDelta, error){deltaStep(model.StreamDelta{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "hi"}}}), deltaStep(model.StreamDelta{HasChoice: true, Role: "tool"})}, model.OutputErrored}, // Two different raw role values conflict.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, s := assemble(t, context.Background(), testRef, tc.steps...)
			expectStatus(t, out, tc.want)
			assertSingleClose(t, s)

			if tc.want == model.OutputErrored {
				parts := msgContent(out)
				// Erroring must not drop eligible partial content.
				testassert.Require(t, len(parts) == 1 && parts[0].Text == "hi", "errored output retained parts = %#v, want one text part %q", out.Message, "hi")
			}
		})
	}
}

// TestAssembleCompletedPayloadShapes pins the three payload-only completion variants end to end: plain text, message-scope extras alone, and a content part whose only retained data is its extra values.
func TestAssembleCompletedPayloadShapes(t *testing.T) {
	t.Run("text-only", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "hello"))), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s)

		testassert.Require(t, out.Message != nil, "completed output carries no message at all")

		parts := msgContent(out)
		// exactly ONE text part carrying the exact supplied fragment value verbatim

		testassert.Require(t, len(parts) == 1 && parts[0].Kind == model.PartText && parts[0].Text == "hello", "parts = %#v", parts)

		// the text-only shape must carry NO refusal string and NO tool calls on its message

		testassert.Require(t, out.Message.Refusal == "" && len(out.Message.ToolCalls) <= 0, "text-only message carries unexpected refusal=%q calls=%d", out.Message.Refusal, len(out.Message.ToolCalls))

		// message-scope extras must be empty for this variant per contract — none were ever supplied by its fixture steps

		// usage was never reported by any delta in this script per the fixture

		testassert.Require(t, len(out.Message.Extra.Finalize()) <= 0 && out.Usage == nil, "text-only message carries unexpected extras: %#v; completed output unexpectedly carried usage: %#v", out.Message.Extra, out.Usage)
	})

	t.Run("message-extra-only", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(model.StreamDelta{HasChoice: true, MessageExtra: model.Extra{"k": json.RawMessage(`1`)}}), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s)

		testassert.Require(t, out.Message != nil, "completed output carries no message at all")

		if len(msgContent(out)) != 0 { // NO content parts may exist for this variant per contract — only message-scope extras were ever supplied by its fixture
			t.Fatalf("message-extra-only output carries content parts: %#v", msgContent(out))
		}

		testassert.Require(t, string(out.Message.Extra["k"]) == "1", "message extra k = %s, want 1", out.Message.Extra["k"])
	})

	t.Run("content-part-extra-only", func(t *testing.T) {
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(model.ContentFragment{Position: 0, Kind: model.PartOpaque, OpaqueWireType: "thinking", Extra: model.Extra{"data": json.RawMessage(`"x"`)}})), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s)

		parts := msgContent(out)
		// exactly ONE opaque part carrying its structural wire type verbatim

		testassert.Require(t, len(parts) == 1 && parts[0].Kind == model.PartOpaque && parts[0].OpaqueWireType == "thinking", "parts = %#v", parts)

		testassert.Require(t, string(parts[0].Extra["data"]) == `"x"`, "part extra data = %s, want \"x\"", parts[0].Extra["data"])

		// the opaque part must carry NO text or url values per kind-field exclusivity rules at model's own boundary

		testassert.Require(t, string(parts[0].Text) == "" && string(parts[0].URL) == "", "opaque part carries forbidden fields: text=%q url=%q", parts[0].Text, parts[0].URL)
	})
}
