// Package clientops owns the target clients' typed non-UI invocation: it
// binds one invocation envelope to the generated Runtime operation it names
// and returns that operation's exact raw response JSON. It owns no
// connection, Session selection, projection, or work lifetime.
package clientops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/protocol"
)

// ResponseError is one received Runtime error or partial code-revert result.
// Payload is always the generated Error; Partial carries the complete
// generated result when the Runtime answered a code-revert call with one.
type ResponseError struct {
	Status  int
	Payload protocol.Error
	Partial *protocol.CodeRevertResult
}

// Error returns the received Runtime error's message.
func (e *ResponseError) Error() string { return e.Payload.Message }

// decodeRuntimeError decodes one declared non-2xx Runtime body from its
// preserved raw members: a partial code-revert result requires its non-null
// restored and skipped arrays and its non-null Error, which supplies the
// payload while the complete result supplies Partial; otherwise the body is
// one plain Error with a non-null message and a closed generated ErrorCode.
func decodeRuntimeError(body []byte) (protocol.Error, *protocol.CodeRevertResult, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		return protocol.Error{}, nil, fmt.Errorf("malformed runtime error body: %v", err)
	}
	if members == nil {
		return protocol.Error{}, nil, fmt.Errorf("malformed runtime error body: not one JSON object")
	}
	_, hasRestored := members["restored"]
	_, hasSkipped := members["skipped"]
	if hasRestored || hasSkipped {
		for _, name := range []string{"restored", "skipped", "error"} {
			value, ok := members[name]
			if !ok || isJSONNull(value) {
				return protocol.Error{}, nil, fmt.Errorf("partial runtime result is missing non-null %q", name)
			}
		}
		payload, err := decodeError(members["error"])
		if err != nil {
			return protocol.Error{}, nil, err
		}
		var result protocol.CodeRevertResult
		if err := json.Unmarshal(body, &result); err != nil {
			return protocol.Error{}, nil, fmt.Errorf("malformed partial runtime result: %v", err)
		}
		if result.Error == nil {
			return protocol.Error{}, nil, fmt.Errorf("partial runtime result carries no error")
		}
		return payload, &result, nil
	}
	payload, err := decodeError(body)
	if err != nil {
		return protocol.Error{}, nil, err
	}
	return payload, nil, nil
}

// decodeError decodes one generated Error document: code and message must be
// present and non-null and the code must be a closed ErrorCode member.
func decodeError(raw json.RawMessage) (protocol.Error, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return protocol.Error{}, fmt.Errorf("malformed runtime error: %v", err)
	}
	if members == nil {
		return protocol.Error{}, fmt.Errorf("malformed runtime error: not one JSON object")
	}
	for _, name := range []string{"code", "message"} {
		value, ok := members[name]
		if !ok || isJSONNull(value) {
			return protocol.Error{}, fmt.Errorf("runtime error is missing non-null %q", name)
		}
	}
	var payload protocol.Error
	if err := json.Unmarshal(raw, &payload); err != nil {
		return protocol.Error{}, fmt.Errorf("malformed runtime error: %v", err)
	}
	if !payload.Code.Valid() {
		return protocol.Error{}, fmt.Errorf("runtime error code %q is outside the closed set", payload.Code)
	}
	return payload, nil
}

// ErrInvalidResponse marks one received unusable response: a failed body
// read, a malformed response, or an unexpected success status. This is the
// invalid-delivery meaning, not a Runtime error code — receiving an unusable
// response is not absence of a response — so it is never a ResponseError.
var ErrInvalidResponse = errors.New("invalid runtime response")

// Invoke performs one generated Runtime operation through the supplied
// authenticated client and returns its lossless result: the received HTTP
// status and the exact body bytes. The schema-generated dispatch in
// operations.gen.go binds each operation's declared addressing and calls its
// generated client method; the envelope carries only that addressing:
// session_id for a schema Session path, field for the two reset path enums,
// and query/body where the operation declares them. A received valid
// response returns its actual status and exact body bytes; a
// valid 204 returns its status and the empty HTTP body. A declared non-2xx
// rejection returns those same status/body bytes plus the ResponseError. A
// local binding failure or a failure to obtain an HTTP response returns zero
// status, no body and its ordinary error; once an HTTP response exists, its
// status and available body bytes are retained even on failure.
func Invoke(ctx context.Context, c *client.Client, operation string, params json.RawMessage) (int, json.RawMessage, error) {
	response, expected, err := invokeGenerated(ctx, c.ClientWithResponses.ClientInterface, operation, params)
	if err != nil {
		return 0, nil, err
	}
	return receive(response, expected)
}

// Call is the thin typed consumer of Invoke: it returns the raw successful
// JSON — the JSON null for a 204 — or the same error. It never decodes a
// success DTO.
func Call(ctx context.Context, c *client.Client, operation string, params json.RawMessage) (json.RawMessage, error) {
	status, body, err := Invoke(ctx, c, operation, params)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return json.RawMessage("null"), nil
	}
	return body, nil
}

// receive is the one common response owner: it reads and closes the HTTP body
// once and applies the expected-status, JSON-document, 204 and
// Error/partial-restore rules. A body-read, malformed-response or
// unexpected-success-status failure wraps ErrInvalidResponse and retains the
// received status and available body bytes; a declared non-2xx rejection is
// the ResponseError over those same bytes.
func receive(response *http.Response, expected int) (int, json.RawMessage, error) {
	status := response.StatusCode
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return status, body, fmt.Errorf("read runtime response body: %w", ErrInvalidResponse)
	}
	if status == expected {
		if expected == http.StatusNoContent {
			return status, nil, nil
		}
		if !json.Valid(body) {
			return status, body, fmt.Errorf("runtime returned an empty or malformed success body for status %d: %w", status, ErrInvalidResponse)
		}
		return status, body, nil
	}
	if status < 200 || status >= 300 {
		payload, partial, err := decodeRuntimeError(body)
		if err != nil {
			return status, body, fmt.Errorf("%v: %w", err, ErrInvalidResponse)
		}
		return status, body, &ResponseError{Status: status, Payload: payload, Partial: partial}
	}
	return status, body, fmt.Errorf("runtime returned status %d for an operation expecting %d: %w", status, expected, ErrInvalidResponse)
}
