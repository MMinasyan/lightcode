package extension

import "encoding/json"

// CallError is one named-service call failure whose original class and cause
// remain available through Error and Unwrap. Partial carries the exact
// generated partial payload a command produced before failing; it is present
// only when that payload actually exists.
type CallError struct {
	Err     error
	Partial json.RawMessage
}

// Error returns the original cause's text.
func (e *CallError) Error() string {
	if e == nil || e.Err == nil {
		return "extension: call failed"
	}
	return e.Err.Error()
}

// Unwrap returns the original cause, so errors.Is and errors.As retain its
// class.
func (e *CallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
