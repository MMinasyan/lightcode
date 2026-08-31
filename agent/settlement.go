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

// validateToolResult checks one settled tool result returned by a tool effect against the started call it answers: non-empty original call ID matching exactly (no normalization or fallback), closed status set with its content requirements re-validated through the landed public constructor at this trust boundary. Any breach is one typed boundary-protocol error naming the "tool" boundary; nothing here coerces a malformed result into another shape.
func validateToolResult(res model.ToolResult, callID string) error {
	if _, err := model.NewToolResult(res); err != nil { // closed status set and per-status content requirements at this trust boundary through the public constructor.
		return fmt.Errorf("%w: %v", newBoundaryViolation("tool", "settled tool result violates the tool-result contract"), err)
	}
	if res.CallID != callID { // mismatch between the answered identity and the started call is a protocol violation without normalization or fallback of any kind.
		return newBoundaryViolation("tool", fmt.Sprintf("settled tool result answers %q but was invoked for call %q", res.CallID, callID))
	}
	return nil // well-formed settlement answering exactly its own call.
}
