package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// mkSettlementOutput builds one valid model output of the requested closed status carrying exactly testRef's source identity — a construction failure here means this test fixture itself is broken, not the code under test.
func mkSettlementOutput(status model.OutputStatus, detail string) *model.Output {
	msg := model.Message{Role: model.RoleAssistant, Source: testRef} // one canonical assistant message carrying exactly THIS package's shared source identity

	out := model.Output{Status: status, Source: testRef, Detail: detail}
	switch {
	case status == model.OutputCompleted: // the one mandatory-payload row: a single non-empty text part minimally satisfies the completed-output payload rule.
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	case status == model.OutputInterrupted || detail != "": // optional partial message retained alongside its diagnostic text for the other two shapes — present-but-tool-call-free per their own closed-shape rules
		msg.Content = []model.ContentPart{{Kind: model.PartText, Text: "x"}}

	default: // no message at all
	}

	if out.Message == nil && msg.Content != nil { // only wire up the pointer when we actually built payload content
		out.Message = &msg
	}

	o, err := model.NewOutput(out)
	if err != nil {
		panic(fmt.Sprintf("test fixture failed model-output validation: %v", err))
	}

	return &o
}

// TestValidateSettlement pins the closed disposition table of model settlements end to end: every only-valid combination passes, and each invalid shape returns exactly one typed boundary-protocol error naming the "model" boundary.
func TestValidateSettlement(t *testing.T) {
	completed := mkSettlementOutput(model.OutputCompleted, "") // one canonical valid completed output fixture reused by the valid rows.
	errored := mkSettlementOutput(model.OutputErrored, "boom") // its errored sibling carrying non-empty diagnostic detail.
	interrupted := mkSettlementOutput(model.OutputInterrupted, "stopped")

	refusalMsg := &model.Message{Role: model.RoleAssistant, Source: testRef, Refusal: "declined"}
	refusalErrored, ferr := model.NewOutput(model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom", Message: refusalMsg})
	testassert.NoError(t, ferr)
	extraMsg := &model.Message{Role: model.RoleAssistant, Source: testRef, Extra: model.Extra{"k": json.RawMessage(`1`)}}
	extraErrored, ferr := model.NewOutput(model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom", Message: extraMsg})
	testassert.NoError(t, ferr)

	valid := []struct {
		name string
		set  ModelSettlement
	}{
		{"valid-ready", ModelSettlement{Disposition: DispoReady, Output: completed}},
		{"valid-continue", ModelSettlement{Disposition: DispoContinue, Output: errored}},
		{"valid-continue-completed-no-calls", ModelSettlement{Disposition: DispoContinue, Output: completed}},            // a completed output carrying no tool calls may settle as continue.
		{"valid-continue-errored-refusal-payload", ModelSettlement{Disposition: DispoContinue, Output: &refusalErrored}}, // the refusal arm of the retained-payload rule.
		{"valid-continue-errored-extra-payload", ModelSettlement{Disposition: DispoContinue, Output: &extraErrored}},     // the finalized-extra arm of the retained-payload rule.
		{"valid-failure-no-output", ModelSettlement{Disposition: DispoFailure, Detail: "d"}},
		{"valid-failure-with-errored", ModelSettlement{Disposition: DispoFailure, Output: errored, Detail: "d"}},
		{"valid-interruption-no-output", ModelSettlement{Disposition: DispoInterruption, Detail: "d"}},
		{"valid-interruption-in-flight", ModelSettlement{Disposition: DispoInterruption, Output: interrupted, Detail: "d"}},
		{"valid-interruption-retained-completed", ModelSettlement{Disposition: DispoInterruption, Output: completed, Detail: "d"}},
	}

	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateSettlement(tc.set, testRef, true)
			testassert.NoError(t, err)
		})
	}

	for _, row := range forgedModelSettlements() {
		t.Run(row.name, func(t *testing.T) {
			_, err := validateSettlement(row.set, testRef, true)
			requireBoundaryViolation(t, err, "model")
		})
	}
}

// forgedModelSettlement is one shared invalid-settlement input: the name and
// the forged settlement. Direct validation and Run propagation each build their
// own fresh values from forgedModelSettlements so scripted effects and
// ownership probes cannot share an input.
type forgedModelSettlement struct {
	name string
	set  ModelSettlement
}

func forgedModelSettlements() []forgedModelSettlement {
	completed := &model.Output{Status: model.OutputCompleted, Source: testRef, Message: &model.Message{Role: model.RoleAssistant, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}
	errored := &model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom", Message: &model.Message{Role: model.RoleAssistant, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}
	interrupted := &model.Output{Status: model.OutputInterrupted, Source: testRef, Detail: "stopped", Message: &model.Message{Role: model.RoleAssistant, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}
	callsOut := &model.Output{Status: model.OutputCompleted, Source: testRef, Message: &model.Message{Role: model.RoleAssistant, Source: testRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, ToolCalls: []model.ToolCall{{ID: "a", Name: "fnA"}, {ID: "b", Name: "fnB"}, {ID: "c", Name: "fnC"}}}}
	bare := &model.Output{Status: model.OutputErrored, Source: testRef, Detail: "boom"}
	foreign := &model.Output{Status: model.OutputCompleted, Source: model.ModelRef{Provider: "acme", Model: "m-other"}, Message: &model.Message{Role: model.RoleAssistant, Source: model.ModelRef{Provider: "acme", Model: "m-other"}, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}}}

	return []forgedModelSettlement{
		{"ready-nil-output", ModelSettlement{Disposition: DispoReady}},                      // Ready requires its callback output.
		{"ready-errored-output", ModelSettlement{Disposition: DispoReady, Output: errored}}, // Ready requires completed; an errored output is the wrong direction.
		{"ready-nonempty-detail", ModelSettlement{Disposition: DispoReady, Output: completed, Detail: "x"}},
		{"continue-nil-output", ModelSettlement{Disposition: DispoContinue}},
		{"continue-completed-with-calls", ModelSettlement{Disposition: DispoContinue, Output: callsOut}},  // A continue carrying tool calls would strand them: nothing dispatches after continue.
		{"continue-errored-without-payload", ModelSettlement{Disposition: DispoContinue, Output: bare}},   // Nearest forbidden sibling of the errored-payload continue rows: nothing model-visible was retained.
		{"continue-interrupted-output", ModelSettlement{Disposition: DispoContinue, Output: interrupted}}, // Nearest forbidden sibling of the completed continue row: an interrupted payload settles interruption only.
		{"continue-nonempty-detail", ModelSettlement{Disposition: DispoContinue, Output: errored, Detail: "y"}},
		{"failure-completed-output", ModelSettlement{Disposition: DispoFailure, Output: completed, Detail: "d"}}, // Failure permits no output or its errored one; completed is the nearest forbidden sibling of valid failure-with-errored.
		{"failure-empty-detail", ModelSettlement{Disposition: DispoFailure}},
		{"interruption-errored-output", ModelSettlement{Disposition: DispoInterruption, Output: errored, Detail: "d"}}, // Interruption admits no/interrupted/completed; errored is the nearest forbidden sibling of valid interruption-in-flight.
		{"interruption-empty-detail", ModelSettlement{Disposition: DispoInterruption}},
		{"unknown-disposition", ModelSettlement{Disposition: "bogus"}},                // Unknown dispositions fail before any other field is consulted.
		{"foreign-source", ModelSettlement{Disposition: DispoReady, Output: foreign}}, // A present output must match the expected identity field-for-field.
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
		{
			_, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady})
			testassert.Require(t, isBoundaryViolation(err, "model"), "missing ready output = %v, want the model-boundary violation", err)
		}
	})

	t.Run("owned-copy-on-success", func(t *testing.T) {
		in := mkSettlementOutput(model.OutputCompleted, "")
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady, Output: in})
		testassert.NoError(t, err)
		testassert.Require(t, got.Disposition == DispoReady && got.Detail == "" && (got.Output != nil && got.Output != in), "owned settlement = %#v, want disposition and detail copied as plain values; owned output must be a distinct pointer, got %p", got, got.Output)
		in.Message.Content[0].Text = "input-side" // mutate the input after validation...
		testassert.Equal(t, got.Output.Message.Content[0].Text, "x")
		got.Output.Message.Content[0].Text = "owned-side" // ...then the owned side; neither may reach the other.
		testassert.Equal(t, in.Message.Content[0].Text, "input-side")
		in.Status = model.OutputErrored
		testassert.Equal(t, got.Output.Status, model.OutputCompleted)
	})

	t.Run("outputless-owned", func(t *testing.T) {
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoFailure, Detail: "d"})
		testassert.Require(t, err == nil && got.Disposition == DispoFailure && got.Detail == "d" && got.Output == nil, "got %#v, %v; want the settlement copied with nil output retained", got, err)
	})

	t.Run("owned-copy-nested-fields", func(t *testing.T) {
		in := mkNestedSettlementOutput()
		got, err := ValidateModelSettlement(testRef, ModelSettlement{Disposition: DispoReady, Output: in})
		testassert.NoError(t, err)

		// Input-side mutations after validation never reach the owned copy
		// nested tool-call argument bytes, every Extra map, and usage.
		in.Message.ToolCalls[0].Arguments[0] = 'z'
		in.Message.ToolCalls[0].Extra["c"] = json.RawMessage(`"in"`)
		in.Message.Content[0].Extra["p"] = json.RawMessage(`"in"`)
		in.Message.Extra["m"] = json.RawMessage(`"in"`)
		in.Usage.InputTokens = 99
		testassert.Require(t, string(got.Output.Message.ToolCalls[0].Arguments) == `{"x":1}`, "owned copy aliased the input arguments: %q", got.Output.Message.ToolCalls[0].Arguments)
		testassert.Require(t, string(got.Output.Message.ToolCalls[0].Extra["c"]) == `3` && string(got.Output.Message.Content[0].Extra["p"]) == `1` && string(got.Output.Message.Extra["m"]) == `2`, "owned copy aliased an input Extra map: call=%s part=%s message=%s", got.Output.Message.ToolCalls[0].Extra["c"], got.Output.Message.Content[0].Extra["p"], got.Output.Message.Extra["m"])
		testassert.Require(t, got.Output.Usage.InputTokens == 3, "owned copy aliased the input usage: %+v", got.Output.Usage)

		// Owned-side mutations never reach the input either: the input keeps
		// exactly its own post-mutation values from the block above.
		got.Output.Message.ToolCalls[0].Arguments[1] = 'z'
		got.Output.Message.ToolCalls[0].Extra["c"] = json.RawMessage(`"owned"`)
		got.Output.Message.Content[0].Extra["p"] = json.RawMessage(`"owned"`)
		got.Output.Message.Extra["m"] = json.RawMessage(`"owned"`)
		got.Output.Usage.InputTokens = 1
		testassert.Require(t, string(in.Message.ToolCalls[0].Arguments) == `z"x":1}`, "input arguments aliased the owned copy: %q", in.Message.ToolCalls[0].Arguments)
		testassert.Require(t, string(in.Message.ToolCalls[0].Extra["c"]) == `"in"` && string(in.Message.Content[0].Extra["p"]) == `"in"` && string(in.Message.Extra["m"]) == `"in"`, "input Extra maps aliased the owned copy: call=%s part=%s message=%s", in.Message.ToolCalls[0].Extra["c"], in.Message.Content[0].Extra["p"], in.Message.Extra["m"])
		testassert.Require(t, in.Usage.InputTokens == 99, "input usage aliased the owned copy: %+v", in.Usage)
	})

	t.Run("one-copy-per-validation", func(t *testing.T) {
		set := ModelSettlement{Disposition: DispoReady, Output: mkNestedSettlementOutput()}
		two := testing.AllocsPerRun(200, func() { // the removed two-copy shape: validate (one internal copy, discarded) plus a second full copy
			_, _ = validateSettlement(set, testRef, true)
			_, _ = model.NewOutput(*set.Output)
		})
		one := testing.AllocsPerRun(200, func() {
			_, _ = ValidateModelSettlement(testRef, set)
		})
		testassert.Require(t, one < two, "public validator allocated %v times, not fewer than the removed two-copy shape's %v", one, two)
	})
}

// mkNestedSettlementOutput builds one valid completed output carrying every
// nested reference type the owned copy must isolate: tool-call argument bytes
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
func TestValidateToolResult(t *testing.T) {
	const callID = "call_1" // one stable original-call identity every row answers against.

	cases := []struct {
		name    string
		res     model.ToolResult
		wantErr bool
	}{
		{"valid-success-empty-content", model.ToolResult{CallID: callID, Status: model.ResultSuccess}, false},
		{"valid-error-with-detail", model.ToolResult{CallID: callID, Status: model.ResultError, Content: "detail"}, false},
		{"valid-denied-with-detail", model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: "denied by policy"}, false},              // the denied status mandates non-empty content.
		{"valid-interrupted-with-content", model.ToolResult{CallID: callID, Status: model.ResultInterrupted, Content: "stopped mid-flight"}, false}, // the interrupted status carries the same non-empty content mandate.
		{"invalid-missing-call-id", model.ToolResult{Status: model.ResultSuccess}, true},
		{"invalid-unknown-status", model.ToolResult{CallID: callID, Status: "bogus", Content: "x"}, true}, // any status outside the closed four-value set is one typed violation at this boundary.
		{"invalid-nonempty-required-empty-content", model.ToolResult{CallID: callID, Status: model.ResultInterrupted}, true},
		{"invalid-answered-id-mismatch", model.ToolResult{CallID: "other-call", Status: model.ResultSuccess}, true}, // answering the wrong call id is one typed violation without normalization or fallback.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToolResult(tc.res, callID)
			if tc.wantErr {
				requireBoundaryViolation(t, err, "tool")
			} else if err != nil {
				t.Fatalf("expected valid tool result, got: %v", err)
			}
		})
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

	// badLastOutput is a completed-shaped output that fails re-validation through the read-only model validator: its message carries a foreign source identity.
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
		{"invalid-success-model-invalid-output", TerminalResult{Status: TerminalSuccess, LastOutput: badLastOutput}, true}, // present outputs are re-validated through the read-only model validator.
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
		{"invalid-interruption-unstarted-invalid-call", TerminalResult{Status: TerminalInterruption, LastOutput: callsOut, UnstartedCalls: []model.ToolCall{{ID: "a"}}, Detail: "d"}, true}, // present calls are re-validated through the read-only tool-call validator.
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
