// Package tool contains the prepared file, patch and command execution
// implementation and the real helpers the target tools plugin composes.
package tool

// ExitError carries model-visible output for a tool failure that should be
// rendered as the tool result body instead of a generic Go error string.
type ExitError struct {
	Output   string
	ExitCode int
	// Cancelled marks the runner's delivered-cause cancellation: the parent
	// cause killed the run before it could settle. Own timeouts and signal
	// deaths the runner merely observed stay unflagged.
	Cancelled bool
}

func (e *ExitError) Error() string { return e.Output }
