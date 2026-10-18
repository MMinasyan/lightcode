// Package clientops owns the target clients' typed non-UI invocation: it
// binds one invocation envelope to the generated Runtime operation it names
// and returns that operation's exact raw response JSON. It owns no
// connection, Session selection, projection, or work lifetime.
package clientops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/protocol"
)

// contentTypeJSON is the content type every declared request body uses.
const contentTypeJSON = "application/json"

// ErrInvalidParams marks one local invocation-binding failure: the operation
// name is unknown or the supplied envelope does not match the operation's
// declared addressing. Callers distinguish these from transport failures.
var ErrInvalidParams = errors.New("invalid params")

// invalidParams wraps one local binding failure with the caller's context.
func invalidParams(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ErrInvalidParams)...)
}

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

// OperationNames returns a fresh copy of the supported invocation names in
// sorted order. It is presentation-only (CLI help and the ACP advertisement);
// admission and dispatch own unknown-name rejection, never this list.
func OperationNames() []string {
	return append([]string(nil), operationNames...)
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
	response, expected, err := invokeGenerated(ctx, c, operation, params)
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

// envelope is one bound invocation envelope: only the members the addressed
// operation declares are populated.
type envelope struct {
	session string
	field   string
	query   json.RawMessage
	body    json.RawMessage
}

// memberRule declares how one envelope member must appear for one operation:
// forbidden (the operation does not declare it), required, or declared but
// optional. An optional member is absent or present and non-null.
type memberRule int

const (
	memberForbidden memberRule = iota
	memberRequired
	memberOptional
)

// envelopeDoc is the envelope's raw declaration before membership checks.
type envelopeDoc struct {
	SessionID json.RawMessage `json:"session_id"`
	Field     json.RawMessage `json:"field"`
	Query     json.RawMessage `json:"query"`
	Body      json.RawMessage `json:"body"`
}

// bindEnvelope decodes one invocation envelope as a single document with
// unknown members rejected, then applies the declared-member rules: declared
// required members must be present and non-null, declared optional members
// are absent or present and non-null, and members the operation does not
// declare must be absent. Missing params is the empty envelope; a supplied
// null is invalid params like every other non-object form.
func bindEnvelope(params json.RawMessage, session, field, query, body memberRule) (envelope, error) {
	if len(params) == 0 {
		params = []byte("{}")
	}
	if isJSONNull(params) {
		return envelope{}, invalidParams("invocation envelope must not be null")
	}
	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var doc envelopeDoc
	if err := decoder.Decode(&doc); err != nil {
		return envelope{}, invalidParams("invocation envelope: %v", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return envelope{}, invalidParams("invocation envelope carries trailing data")
	}
	var env envelope
	rawSession, err := takeMember(doc.SessionID, "session_id", session)
	if err != nil {
		return envelope{}, err
	}
	if rawSession != nil {
		if err := json.Unmarshal(rawSession, &env.session); err != nil {
			return envelope{}, invalidParams("session_id: %v", err)
		}
	}
	rawField, err := takeMember(doc.Field, "field", field)
	if err != nil {
		return envelope{}, err
	}
	if rawField != nil {
		if err := json.Unmarshal(rawField, &env.field); err != nil {
			return envelope{}, invalidParams("field: %v", err)
		}
	}
	env.query, err = takeMember(doc.Query, "query", query)
	if err != nil {
		return envelope{}, err
	}
	if env.query != nil && !isJSONObject(env.query) {
		return envelope{}, invalidParams("query must be one JSON object")
	}
	env.body, err = takeMember(doc.Body, "body", body)
	if err != nil {
		return envelope{}, err
	}
	if env.body != nil && !isJSONObject(env.body) {
		return envelope{}, invalidParams("body must be one JSON object")
	}
	return env, nil
}

// takeMember applies one declared-member rule to one envelope member.
func takeMember(raw json.RawMessage, name string, rule memberRule) (json.RawMessage, error) {
	if rule == memberForbidden {
		if len(raw) != 0 {
			return nil, invalidParams("%s is not accepted for this operation", name)
		}
		return nil, nil
	}
	if len(raw) == 0 {
		if rule == memberOptional {
			return nil, nil
		}
		return nil, invalidParams("%s is required for this operation", name)
	}
	if isJSONNull(raw) {
		return nil, invalidParams("%s must not be null", name)
	}
	return raw, nil
}

// bindQuery binds one declared query object into its exact generated Params
// type: every required member must be present and non-null, every provided
// member must be non-null, and unknown members are rejected before typed
// decoding. Enum values pass through as their generated type and Runtime
// decides their semantic validity.
func bindQuery(raw json.RawMessage, required []string, target any) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return invalidParams("query: %v", err)
	}
	if members == nil {
		return invalidParams("query must be one JSON object")
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return invalidParams("query is missing required member %q", name)
		}
	}
	for name, value := range members {
		if isJSONNull(value) {
			return invalidParams("query member %q must not be null", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return invalidParams("query: %v", err)
	}
	return nil
}

// isJSONNull reports whether one raw member is the null literal.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// isJSONObject reports whether one raw value is a JSON object document.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && trimmed[0] == '{'
}
