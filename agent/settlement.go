package agent

import (
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// validateSettlement checks one settlement returned by a model effect against the closed disposition table and the invocation's expected identity: ready is exactly its completed callback output with empty detail; continue exactly its errored output with empty detail (the caller has already settled any continuation facts); failure non-empty detail plus no accepted stream or that same errored output — a completed output never settles as failure; interruption non-empty detail plus one of three states, nothing before acceptance, the interrupted callback output while generation was in flight over it, or a retained completed output when cancellation followed successful completion. A present output must itself satisfy every ordinary model-output invariant and carry exactly the expected source identity field-for-field (String() rendering is lossy on first-slash splits, so both fields are compared separately). Every violation returns one typed boundary-protocol error naming the "model" boundary; nothing here coerces a malformed settlement into another shape.
func validateSettlement(set ModelSettlement, expected model.ModelRef) error {
	switch set.Disposition {
	case DispoReady: // completed callback output only; empty detail checked below.
		if err := requireDispositionOutput("ready", set.Output, model.OutputCompleted); err != nil {
			return err
		}
	case DispoContinue: // errored callback output only; empty detail checked below.
		if err := requireDispositionOutput("continue", set.Output, model.OutputErrored); err != nil {
			return err
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
	return nil // well-formed.
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

// validateTerminalResult checks one Agent terminal result against the closed status table: success is a valid completed last output with no tool calls, no unstarted calls and empty detail; failure is non-empty detail, no unstarted calls, and an optional valid completed or errored output; interruption is non-empty detail, an optional valid completed or interrupted output, and unstarted calls that must each identify a distinct call of that completed output in increasing original-call order. Every other shape or status is one typed boundary-protocol error naming the "agent" boundary; present outputs and unstarted calls are re-validated through the landed public model constructors first.
func validateTerminalResult(res TerminalResult) error {
	out := res.LastOutput
	if out != nil {
		validated, err := model.NewOutput(*out) // a present last output must satisfy the ordinary model-output invariants before its status participates in any row check.
		if err != nil {
			return fmt.Errorf("%w: %v", newBoundaryViolation("agent", "terminal last output violates model-output invariants"), err)
		}
		out = &validated
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
