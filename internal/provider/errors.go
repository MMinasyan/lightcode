package provider

import (
	"errors"
	"fmt"
	"strings"
)

// HTTPStatusError represents a non-2xx response from the API endpoint.
type HTTPStatusError struct {
	StatusCode int
	StatusText string
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error %d %s: %s", e.StatusCode, e.StatusText, e.Message)
	}
	return fmt.Sprintf("API error %d %s", e.StatusCode, e.StatusText)
}

// Retryable returns true for 429 (rate limit) and 5xx (server) errors.
func (e *HTTPStatusError) Retryable() bool {
	return e != nil && (e.StatusCode == 429 || e.StatusCode >= 500)
}

// ReservedKeyError indicates a sidecar body contains keys owned by the
// request builder (model, messages, tools, etc.).
type ReservedKeyError struct {
	Keys []string
}

func (e *ReservedKeyError) Error() string {
	return fmt.Sprintf("reserved keys in extra body: %s", strings.Join(e.Keys, ", "))
}

// ErrIncompleteModel is returned when a catalog provider/model is missing.
var ErrIncompleteModel = errors.New("model is incomplete")

// ErrAuthFailed is returned on 401/403 responses.
var ErrAuthFailed = errors.New("authentication failed")
