package melta

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/protocol"
)

// ErrorCode classifies one producer error into its public class by errors.Is
// over the existing typed sentinels, never rendered text. Caller-input
// invalidity outranks the complete-candidate configuration failure that may
// wrap it; Caller cancellation/deadline is its own cancelled class, and every
// unclassified error is internal. Producer-owned partial results use the same
// mapping, so HTTP never uniquely supplies classification of a private error.
// ErrorValue projects one producer error onto the generated public error value:
// its public class, message, optional corrupt Session attribution, and the
// owned partial produced beside a typed call failure. It is the public owner
// boundary the Runtime consumes; callers never reclassify the error.
func ErrorValue(err error) protocol.Error {
	value := protocol.Error{Code: ErrorCode(err), Message: err.Error()}
	var corrupt *harness.CorruptionError
	if errors.As(err, &corrupt) && corrupt.SessionID != "" {
		session := corrupt.SessionID
		value.SessionId = &session
	}
	var callErr *extension.CallError
	if errors.As(err, &callErr) && len(callErr.Partial) != 0 {
		partial := append(json.RawMessage(nil), callErr.Partial...)
		value.PartialResult = &partial
	}
	return value
}

func ErrorCode(err error) protocol.ErrorCode {
	var workerErr *worker.Error
	if errors.As(err, &workerErr) {
		code := protocol.ErrorCode(workerErr.Class)
		if code.Valid() {
			return code
		}
		return protocol.ErrorCodeInternal
	}
	switch {
	case errors.Is(err, ErrDenied):
		return protocol.ErrorCodeDenied
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return protocol.ErrorCodeCancelled
	case errors.Is(err, harness.ErrInvalid):
		return protocol.ErrorCodeInvalid
	case errors.Is(err, harness.ErrNotFound):
		return protocol.ErrorCodeNotFound
	case errors.Is(err, harness.ErrConflict):
		return protocol.ErrorCodeConflict
	case errors.Is(err, harness.ErrIncompatible):
		return protocol.ErrorCodeIncompatible
	case errors.Is(err, ErrClosed), errors.Is(err, extension.ErrClosed):
		return protocol.ErrorCodeClosed
	case errors.Is(err, harness.ErrCorrupt):
		return protocol.ErrorCodeCorrupt
	case errors.Is(err, ErrConfiguration):
		return protocol.ErrorCodeConfiguration
	case errors.Is(err, harness.ErrStorage):
		return protocol.ErrorCodeStorage
	default:
		return protocol.ErrorCodeInternal
	}
}
