package worker

import (
	"context"
	"errors"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
)

// Class is the closed public infrastructure error class.
type Class string

// The public error classes.
const (
	ClassInvalid       Class = "invalid"
	ClassDenied        Class = "denied"
	ClassNotFound      Class = "not_found"
	ClassConflict      Class = "conflict"
	ClassClosed        Class = "closed"
	ClassCancelled     Class = "cancelled"
	ClassUnavailable   Class = "unavailable"
	ClassConfiguration Class = "configuration"
	ClassIncompatible  Class = "incompatible"
	ClassCorrupt       Class = "corrupt"
	ClassStorage       Class = "storage"
	ClassInternal      Class = "internal"
)

// Valid reports whether the class is one of the closed public classes.
func (c Class) Valid() bool { return c.valid() }

func (c Class) valid() bool {
	switch c {
	case ClassInvalid, ClassDenied, ClassNotFound, ClassConflict, ClassClosed,
		ClassCancelled, ClassUnavailable, ClassConfiguration, ClassIncompatible,
		ClassCorrupt, ClassStorage, ClassInternal:
		return true
	}
	return false
}

// Error is one author or infrastructure failure carried as the closed public
// class plus a non-secret message. Unwrap exposes the matching Harness
// sentinel where the class has one, so decoded failures classify with
// errors.Is. Interrupted and Evidence carry the author's declared effect
// knowledge when the author error is a *harness.EffectError.
type Error struct {
	Class       Class                     `json:"class"`
	Message     string                    `json:"message"`
	Interrupted bool                      `json:"interrupted,omitempty"`
	Evidence    harness.ExecutionEvidence `json:"evidence,omitempty"`
	Partial     JSONBytes                 `json:"partial,omitempty"`
}

// Error returns the class and message.
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Class)
	}
	return string(e.Class) + ": " + e.Message
}

// Unwrap returns the matching sentinel for the class, when one exists.
func (e *Error) Unwrap() error {
	switch e.Class {
	case ClassClosed:
		return extension.ErrClosed
	case ClassCancelled:
		return context.Canceled
	case ClassInvalid:
		return harness.ErrInvalid
	case ClassNotFound:
		return harness.ErrNotFound
	case ClassConflict:
		return harness.ErrConflict
	case ClassCorrupt:
		return harness.ErrCorrupt
	case ClassIncompatible:
		return harness.ErrIncompatible
	case ClassStorage:
		return harness.ErrStorage
	}
	return nil
}

// Classify renders one Go failure as the closed wire error: an Error's own
// class, extension.ErrClosed, a Harness sentinel, a context cancellation or
// deadline, otherwise internal.
func Classify(err error) *Error { return classifyError(err) }

// classifyError renders one Go failure as the closed wire error: an Error's own
// class, extension.ErrClosed, a Harness sentinel, a context cancellation or
// deadline, otherwise internal.
func classifyError(err error) *Error {
	if err == nil {
		return nil
	}
	failure := classOfError(err)
	var typed *Error
	if errors.As(err, &typed) {
		failure.Interrupted = typed.Interrupted
		failure.Evidence = typed.Evidence
		if len(typed.Partial) != 0 {
			failure.Partial = append(JSONBytes(nil), typed.Partial...)
		}
	}
	var effect *harness.EffectError
	if errors.As(err, &effect) {
		failure.Interrupted = harness.EffectInterrupted(err)
		failure.Evidence = harness.DeclaredExecutionKnowledge(err)
	}
	var callErr *extension.CallError
	if errors.As(err, &callErr) && len(callErr.Partial) != 0 {
		failure.Partial = append(JSONBytes(nil), callErr.Partial...)
	}
	return failure
}

func classOfError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		class := typed.Class
		if !class.valid() {
			class = ClassInternal
		}
		message := typed.Message
		if message == "" {
			message = err.Error()
		}
		return &Error{Class: class, Message: message}
	}
	message := err.Error()
	switch {
	case errors.Is(err, extension.ErrClosed):
		return &Error{Class: ClassClosed, Message: message}
	case errors.Is(err, harness.ErrInvalid):
		return &Error{Class: ClassInvalid, Message: message}
	case errors.Is(err, harness.ErrNotFound):
		return &Error{Class: ClassNotFound, Message: message}
	case errors.Is(err, harness.ErrConflict):
		return &Error{Class: ClassConflict, Message: message}
	case errors.Is(err, harness.ErrIncompatible):
		return &Error{Class: ClassIncompatible, Message: message}
	case errors.Is(err, harness.ErrCorrupt):
		return &Error{Class: ClassCorrupt, Message: message}
	case errors.Is(err, harness.ErrStorage):
		return &Error{Class: ClassStorage, Message: message}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &Error{Class: ClassCancelled, Message: message}
	}
	return &Error{Class: ClassInternal, Message: message}
}
