package agent

import (
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// validateSettlement checks one settlement returned by a model effect against the closed disposition table and the invocation's expected identity: ready is exactly its completed callback output with empty detail; continue is its errored output that retains an assistant payload or its completed output carrying no tool calls, with empty detail (the caller has already settled any continuation facts); failure non-empty detail plus no accepted stream or that same errored output — a completed output never settles as failure; interruption non-empty detail plus one of three states, nothing before acceptance, the interrupted callback output while generation was in flight over it, or a retained completed output when cancellation followed successful completion. A present output must itself satisfy every ordinary model-output invariant and carry exactly the expected source identity field-for-field (String() rendering is lossy on first-slash splits, so both fields are compared separately) and its completed tool calls must carry pairwise-unique IDs. Every violation returns one typed boundary-protocol error naming the "model" boundary; nothing here coerces a malformed settlement into another shape.
func validateSettlement(set ModelSettlement, expected model.ModelRef) error {
	switch set.Disposition {
	case DispoReady: // completed callback output only; empty detail checked below.
		if err := requireDispositionOutput("ready", set.Output, model.OutputCompleted); err != nil {
			return err
		}
	case DispoContinue: // errored output retaining an assistant payload, or a completed one carrying no tool calls; empty detail checked below.
		if set.Output == nil {
			return newBoundaryViolation("model", "continue disposition requires its callback output, got none")
		}
		switch set.Output.Status {
		case model.OutputErrored:
			if !hasAssistantPayload(set.Output.Message) {
				return newBoundaryViolation("model", "continue disposition requires an errored output retaining an assistant payload (content part, refusal, or finalized extra)")
			}
		case model.OutputCompleted:
			if set.Output.Message != nil && len(set.Output.Message.ToolCalls) > 0 {
				return newBoundaryViolation("model", "continue disposition requires a completed output with no tool calls")
			}
		default:
			return newBoundaryViolation("model", fmt.Sprintf("continue disposition requires an errored or completed callback output, got %s", string(set.Output.Status)))
		}
	case DispoFailure:
		if set.Detail == "" { // failure always carries diagnostic text.
			return newBoundaryViolation("model", "failure disposition requires non-empty detail")
		}
		if set.Output != nil && set.Output.Status != model.OutputErrored { // no output or the errored callback output; completed and interrupted are both invalid here.
			return newBoundaryViolation("model", fmt.Sprintf("failure disposition requires no output or an errored one, got %s", string(set.Output.Status)))
		}
	case DispoInterruption:
		if set.Detail == "" { // interruption always carries diagnostic text.
			return newBoundaryViolation("model", "interruption disposition requires non-empty detail")
		}
		switch { // three payload states are valid; errored never rides an interruption (that shape belongs to the continue and failure rows only).
		case set.Output == nil: // nothing was accepted before cancellation.
		case set.Output.Status != model.OutputInterrupted && set.Output.Status != model.OutputCompleted:
			return newBoundaryViolation("model", fmt.Sprintf("interruption disposition requires no output, an interrupted one, or a completed one retained after successful completion, got %s", string(set.Output.Status)))
		}
	default: // unknown or empty dispositions fail before any other field is consulted.
		return newBoundaryViolation("model", fmt.Sprintf("unknown disposition %q (closed set: ready, continue, failure, interruption)", string(set.Disposition)))
	}

	if (set.Disposition == DispoReady || set.Disposition == DispoContinue) && set.Detail != "" { // the two empty-detail rows; non-emptiness was already enforced above for the other two.
		return newBoundaryViolation("model", fmt.Sprintf("%s disposition requires an empty detail", string(set.Disposition)))
	}

	if set.Output == nil { // no output to validate on this row.
		return nil
	}

	out := *set.Output                                                                  // local copy so field reads below never alias the caller's pointer during validation.
	if out.Source.Provider != expected.Provider || out.Source.Model != expected.Model { // field-based identity check (String() rendering is lossy); a partial or zero source cannot match either way.
		return newBoundaryViolation("model", fmt.Sprintf("settlement output source %q does not equal invocation expected model identity %q", out.Source.String(), expected.String()))
	}

	if _, err := model.NewOutput(out); err != nil {
		return fmt.Errorf("%w: %v", newBoundaryViolation("model", "settlement output violates model-output invariants"), err)
	}
	if out.Message != nil {
		if err := requireUniqueCallIDs("model", out.Message.ToolCalls); err != nil {
			return err
		}
	}
	return nil // well-formed.
}

// hasAssistantPayload reports whether an assistant message carries model-visible payload under the finalization view — a non-empty finalized content part, a non-empty refusal, or at least one finalized non-null extra — written against exported fields only as the agent-side mirror of model's private predicate (tool calls are impossible on errored outputs and are governed by their own row rule).
func hasAssistantPayload(m *model.Message) bool {
	if m == nil {
		return false
	}
	if m.Refusal != "" {
		return true
	}
	for _, part := range m.Content {
		if part.Text != "" || part.URL != "" || part.OpaqueWireType != "" || len(part.Extra.Finalize()) > 0 {
			return true
		}
	}
	return len(m.Extra.Finalize()) > 0
}

// ValidateModelSettlement validates one model settlement against the closed disposition table and the expected identity exactly like the run's internal validator, rejecting an incomplete expected identity before any settlement row is consulted. On success it returns an independent owned copy of the settlement: a present output is the validated deep copy from the public model constructor, while disposition and detail are plain value copies.
func ValidateModelSettlement(expected model.ModelRef, set ModelSettlement) (ModelSettlement, error) {
	if !nonzeroSource(expected) {
		return ModelSettlement{}, newBoundaryViolation("model", "settlement validation requires a nonzero expected model identity")
	}
	if err := validateSettlement(set, expected); err != nil {
		return ModelSettlement{}, err
	}
	owned := set // disposition and detail are plain value copies.
	if set.Output != nil {
		out, err := model.NewOutput(*set.Output) // the ownership copy; validateSettlement already accepted this exact value through the same constructor.
		if err != nil {
			return ModelSettlement{}, fmt.Errorf("%w: %v", newBoundaryViolation("model", "settlement output violates model-output invariants"), err)
		}
		owned.Output = &out
	}
	return owned, nil
}

// requireUniqueCallIDs enforces the completed-call identity invariant shared by settlements and terminal results: at most one call may carry any given ID, so a repeated ID never reaches dispatch, unstarted-call matching stays unambiguous, and a validated caller cannot drive the loop's internal terminal invariant route.
func requireUniqueCallIDs(boundary string, calls []model.ToolCall) error {
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if seen[c.ID] {
			return newBoundaryViolation(boundary, fmt.Sprintf("completed output repeats tool call id %q", c.ID))
		}
		seen[c.ID] = true
	}
	return nil
}

// requireDispositionOutput enforces presence and exact status for ready (completed) and continue (errored): a nil output or any other closed status is an invalid combination settled as one typed error naming what was found.
func requireDispositionOutput(disposition string, out *model.Output, want model.OutputStatus) error {
	if out == nil { // required callback output missing entirely.
		return newBoundaryViolation("model", disposition+" disposition requires its callback output, got none")
	}
	if out.Status != want { // wrong status for this disposition in either direction.
		return newBoundaryViolation("model", fmt.Sprintf("%s disposition requires %s callback output, got %s", disposition, string(want), string(out.Status)))
	}
	return nil // status matches.
}

// validateToolResult checks one settled tool result returned by a tool effect against the started call it answers: non-empty original call ID matching exactly (no normalization or fallback), closed status set with its content requirements re-validated through the landed public constructor at this trust boundary. Any breach is one typed boundary-protocol error naming the "tool" boundary; nothing here coerces a malformed settlement into another shape.
func validateToolResult(res model.ToolResult, callID string) error {
	if _, err := model.NewToolResult(res); err != nil { // closed status set and per-status content requirements at this trust boundary through the public constructor.
		return fmt.Errorf("%w: %v", newBoundaryViolation("tool", "settled tool result violates the tool-result contract"), err)
	}
	if res.CallID != callID { // mismatch between the answered identity and the started call is a protocol violation without normalization or fallback of any kind.
		return newBoundaryViolation("tool", fmt.Sprintf("settled tool result answers %q but was invoked for call %q", res.CallID, callID))
	}
	return nil // well-formed settlement answering exactly its own call.
}

// validateTerminalResult checks one Agent terminal result against the closed status table: success is a valid completed last output with no tool calls, no unstarted calls and empty detail; failure is non-empty detail, no unstarted calls, and an optional valid completed or errored output; interruption is non-empty detail, an optional valid completed or interrupted output, and unstarted calls that must each identify a distinct call of that completed output in increasing original-call order. Every other shape or status is one typed boundary-protocol error naming the "agent" boundary; present outputs and unstarted calls are re-validated through the landed public model constructors first, and a present completed output must carry pairwise-unique call IDs.
func validateTerminalResult(res TerminalResult) error {
	out := res.LastOutput
	if out != nil {
		validated, err := model.NewOutput(*out) // a present last output must satisfy the ordinary model-output invariants before its status participates in any row check.
		if err != nil {
			return fmt.Errorf("%w: %v", newBoundaryViolation("agent", "terminal last output violates model-output invariants"), err)
		}
		out = &validated
		if out.Message != nil {
			if err := requireUniqueCallIDs("agent", out.Message.ToolCalls); err != nil {
				return err
			}
		}
	}

	switch res.Status {
	case TerminalSuccess:
		if res.Detail != "" { // success is exactly the clean end of a run and never carries diagnostic text.
			return newBoundaryViolation("agent", "success status requires an empty detail")
		}
		if len(res.UnstartedCalls) > 0 { // nothing was left undischarged on a successful run.
			return newBoundaryViolation("agent", "success status allows no unstarted calls")
		}
		if out == nil || out.Status != model.OutputCompleted { // the mandatory completed last output, already re-validated above.
			return newBoundaryViolation("agent", "success status requires a completed last output")
		}
		if len(out.Message.ToolCalls) > 0 { // a success-ending model response carries no tool work at all.
			return newBoundaryViolation("agent", "success status requires a last output with no tool calls")
		}
	case TerminalFailure:
		if res.Detail == "" { // failure always carries diagnostic text.
			return newBoundaryViolation("agent", "failure status requires non-empty detail")
		}
		if len(res.UnstartedCalls) > 0 { // unstarted calls ride interruptions only.
			return newBoundaryViolation("agent", "failure status allows no unstarted calls")
		}
		if out != nil && out.Status != model.OutputCompleted && out.Status != model.OutputErrored { // optional last settled output, completed or errored; an interrupted one belongs to the interruption row.
			return newBoundaryViolation("agent", fmt.Sprintf("failure status allows no output or a completed/errored one, got %s", string(out.Status)))
		}
	case TerminalInterruption:
		if res.Detail == "" { // interruption always carries diagnostic text.
			return newBoundaryViolation("agent", "interruption status requires non-empty detail")
		}
		if out != nil && out.Status != model.OutputCompleted && out.Status != model.OutputInterrupted { // optional last settled output, completed or interrupted; an errored one belongs to the failure row.
			return newBoundaryViolation("agent", fmt.Sprintf("interruption status allows no output or a completed/interrupted one, got %s", string(out.Status)))
		}
		if err := validateUnstartedCalls(res.UnstartedCalls, out); err != nil {
			return err
		}
	default: // unknown or empty statuses fail before any payload row is consulted.
		return newBoundaryViolation("agent", fmt.Sprintf("unknown terminal status %q (closed set: success, failure, interruption)", string(res.Status)))
	}

	return nil
}

// validateUnstartedCalls enforces the interruption row's correlation rule: unstarted calls exist only against a completed last output, and each must be a valid tool call identifying one distinct call of that output through the public constructor and ID matching, in strictly increasing original-call order.
func validateUnstartedCalls(calls []model.ToolCall, out *model.Output) error {
	if len(calls) == 0 { // nothing to correlate.
		return nil
	}
	if out == nil || out.Status != model.OutputCompleted { // the calls must come from a completed output; an interrupted partial carries none.
		return newBoundaryViolation("agent", "unstarted calls require a completed last output to identify their calls")
	}

	prev := -1
	for _, call := range calls {
		checked, err := model.NewToolCall(call) // identity and shape come back from the public constructor first.
		if err != nil {
			return fmt.Errorf("%w: %v", newBoundaryViolation("agent", "unstarted call violates the tool-call contract"), err)
		}
		idx := -1
		for i, produced := range out.Message.ToolCalls { // identification is exact call-ID equality against the output's own ordered calls.
			if produced.ID == checked.ID {
				idx = i
				break
			}
		}
		if idx < 0 || idx <= prev { // not from this output, already consumed by an earlier unstarted call, or out of the output's original order.
			return newBoundaryViolation("agent", fmt.Sprintf("unstarted call %q does not identify a later call of the completed last output", checked.ID))
		}
		prev = idx
	}
	return nil
}
