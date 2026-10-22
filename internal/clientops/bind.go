package clientops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

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

// descriptor is one native operation in its generated form: the envelope
// member rules and one success status the schema declares, plus the generated
// SDK call that binds the operation's path and query arguments from the
// already-bound envelope and performs exactly that request.
type descriptor struct {
	session memberRule
	field   memberRule
	query   memberRule
	body    memberRule
	status  int
	call    func(context.Context, protocol.ClientInterface, envelope) (*http.Response, error)
}

// invokeGenerated looks up one native operation, binds its declared envelope
// members once, and calls its generated SDK function. The declared success
// status is meaningful only when the call succeeds; a local binding failure or
// a failed SDK call returns its ordinary error, and Invoke discards the
// response tuple on that error.
func invokeGenerated(ctx context.Context, c protocol.ClientInterface, operation string, params json.RawMessage) (*http.Response, int, error) {
	declared, ok := operationDescriptors[operation]
	if !ok {
		return nil, 0, invalidParams("unknown operation %q", operation)
	}
	env, err := bindEnvelope(params, declared.session, declared.field, declared.query, declared.body)
	if err != nil {
		return nil, 0, err
	}
	response, err := declared.call(ctx, c, env)
	return response, declared.status, err
}

// OperationNames returns a fresh copy of the supported invocation names in
// sorted order. It is presentation-only (CLI help and the ACP advertisement);
// admission and dispatch own unknown-name rejection, never this list.
func OperationNames() []string {
	names := make([]string, 0, len(operationDescriptors))
	for name := range operationDescriptors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// bindPath binds one envelope path string to its SDK-declared path type by
// JSON-marshaling the string and decoding it into the typed target. Decoding
// failure is the existing invalid-params class.
func bindPath(value string, target any) error {
	encoded, _ := json.Marshal(value)
	if err := json.Unmarshal(encoded, target); err != nil {
		return invalidParams("path: %v", err)
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
