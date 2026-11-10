package melta

import (
	"context"
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
