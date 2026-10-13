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

// CheckResponse maps one received generated response status/body pair for a
// direct typed caller: every 2xx is success, and a non-2xx body is the
// declared Runtime Error or non-2xx CodeRevertResult. A malformed body is a
// transport failure, never a forged protocol error.
func CheckResponse(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	payload, partial, err := decodeRuntimeError(body)
	if err != nil {
		return err
	}
	return &ResponseError{Status: status, Payload: payload, Partial: partial}
}

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

// operationNames is the one static list of supported invocation names in
// table order; it is the sole authority for the initialize advertisement and
// for Call's own admission.
var operationNames = []string{
	"GetHealth",
	"ListSessions",
	"CreateSession",
	"DeleteSession",
	"ArchiveSession",
	"ReopenSession",
	"SubmitSession",
	"CompactSession",
	"ForkSession",
	"SetSessionAgentType",
	"InterruptSession",
	"StopSession",
	"GetSessionHydration",
	"GetSessionHistory",
	"GetSessionCodeSnapshots",
	"RevertSessionCode",
	"ListWorkspaces",
	"ReadWorkspaceFile",
	"GetRetainedCodeSnapshots",
	"RevertRetainedCode",
	"GetConfiguration",
	"ReloadConfiguration",
	"UpdateConfigurationSettings",
	"SetAgentTypeModel",
	"ListModels",
	"ListProviders",
	"CreateProvider",
	"GetProviderDetail",
	"UpdateProviderDetail",
	"DeleteProviderDetail",
	"DiscoverProviderCandidates",
	"ConnectProvider",
	"DisconnectProvider",
	"ResetProviderField",
	"ListProviderModels",
	"UpdateProviderModel",
	"DeleteProviderModel",
	"DiscoverProviderModelCandidates",
	"ResetProviderModelField",
	"GetWarnings",
}

// OperationNames returns a fresh copy of the supported invocation names.
func OperationNames() []string {
	return append([]string(nil), operationNames...)
}

// Call invokes one generated Runtime operation through the supplied
// authenticated client. The envelope carries only the addressing the
// operation declares: session_id for a schema Session path, field for the two
// reset path enums, and query/body where the operation declares them. The
// returned document is the generated response's exact raw JSON — the JSON
// null for a 204 — after the expected-status and body checks.
func Call(ctx context.Context, c *client.Client, operation string, params json.RawMessage) (json.RawMessage, error) {
	var (
		response wireResponse
		expected int
		callErr  error
	)
	switch operation {
	case "GetHealth":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.GetHealthWithResponse(ctx)
		expected = http.StatusOK
	case "ListSessions":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.ListSessionsParams
		if err := bindQuery(env.query, []string{"workspace", "lifecycle"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ListSessionsWithResponse(ctx, &query)
		expected = http.StatusOK
	case "CreateSession":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.CreateSessionWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusCreated
	case "DeleteSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.DeleteSessionWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusNoContent
	case "ArchiveSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.ArchiveSessionWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusOK
	case "ReopenSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.ReopenSessionWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusOK
	case "SubmitSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.SubmitSessionWithBodyWithResponse(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "CompactSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.CompactSessionWithBodyWithResponse(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "ForkSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.ForkSessionWithBodyWithResponse(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "SetSessionAgentType":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.SetSessionAgentTypeWithBodyWithResponse(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "InterruptSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.InterruptSessionWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusNoContent
	case "StopSession":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.StopSessionWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusNoContent
	case "GetSessionHydration":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.GetSessionHydrationWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusOK
	case "GetSessionHistory":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberOptional, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.GetSessionHistoryParams
		if env.query != nil {
			if err := bindQuery(env.query, nil, &query); err != nil {
				return nil, err
			}
		}
		response, callErr = c.GetSessionHistoryWithResponse(ctx, protocol.SessionID(env.session), &query)
		expected = http.StatusOK
	case "GetSessionCodeSnapshots":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberForbidden)
		if err != nil {
			return nil, err
		}
		response, callErr = c.GetSessionCodeSnapshotsWithResponse(ctx, protocol.SessionID(env.session))
		expected = http.StatusOK
	case "RevertSessionCode":
		env, err := bindEnvelope(params, memberRequired, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.RevertSessionCodeWithBodyWithResponse(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "ListWorkspaces":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.ListWorkspacesWithResponse(ctx)
		expected = http.StatusOK
	case "ReadWorkspaceFile":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.ReadWorkspaceFileWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "GetRetainedCodeSnapshots":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.GetRetainedCodeSnapshotsParams
		if err := bindQuery(env.query, []string{"workspace", "session_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.GetRetainedCodeSnapshotsWithResponse(ctx, &query)
		expected = http.StatusOK
	case "RevertRetainedCode":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.RevertRetainedCodeWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "GetConfiguration":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.GetConfigurationWithResponse(ctx)
		expected = http.StatusOK
	case "ReloadConfiguration":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.ReloadConfigurationWithResponse(ctx)
		expected = http.StatusOK
	case "UpdateConfigurationSettings":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.UpdateConfigurationSettingsWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "SetAgentTypeModel":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberRequired)
		if err != nil {
			return nil, err
		}
		var query protocol.SetAgentTypeModelParams
		if err := bindQuery(env.query, []string{"agent_type"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.SetAgentTypeModelWithBodyWithResponse(ctx, &query, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "ListModels":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.ListModelsParams
		if err := bindQuery(env.query, []string{"all"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ListModelsWithResponse(ctx, &query)
		expected = http.StatusOK
	case "ListProviders":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.ListProvidersWithResponse(ctx)
		expected = http.StatusOK
	case "CreateProvider":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.CreateProviderWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "GetProviderDetail":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.GetProviderDetailParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.GetProviderDetailWithResponse(ctx, &query)
		expected = http.StatusOK
	case "UpdateProviderDetail":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberRequired)
		if err != nil {
			return nil, err
		}
		var query protocol.UpdateProviderDetailParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.UpdateProviderDetailWithBodyWithResponse(ctx, &query, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "DeleteProviderDetail":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.DeleteProviderDetailParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.DeleteProviderDetailWithResponse(ctx, &query)
		expected = http.StatusOK
	case "DiscoverProviderCandidates":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberRequired)
		if err != nil {
			return nil, err
		}
		response, callErr = c.DiscoverProviderCandidatesWithBodyWithResponse(ctx, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "ConnectProvider":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberRequired)
		if err != nil {
			return nil, err
		}
		var query protocol.ConnectProviderParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ConnectProviderWithBodyWithResponse(ctx, &query, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "DisconnectProvider":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.DisconnectProviderParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.DisconnectProviderWithResponse(ctx, &query)
		expected = http.StatusOK
	case "ResetProviderField":
		env, err := bindEnvelope(params, memberForbidden, memberRequired, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.ResetProviderFieldParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ResetProviderFieldWithResponse(ctx, protocol.ProviderField(env.field), &query)
		expected = http.StatusOK
	case "ListProviderModels":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.ListProviderModelsParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ListProviderModelsWithResponse(ctx, &query)
		expected = http.StatusOK
	case "UpdateProviderModel":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberRequired)
		if err != nil {
			return nil, err
		}
		var query protocol.UpdateProviderModelParams
		if err := bindQuery(env.query, []string{"provider_id", "model_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.UpdateProviderModelWithBodyWithResponse(ctx, &query, contentTypeJSON, bytes.NewReader(env.body))
		expected = http.StatusOK
	case "DeleteProviderModel":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.DeleteProviderModelParams
		if err := bindQuery(env.query, []string{"provider_id", "model_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.DeleteProviderModelWithResponse(ctx, &query)
		expected = http.StatusOK
	case "DiscoverProviderModelCandidates":
		env, err := bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.DiscoverProviderModelCandidatesParams
		if err := bindQuery(env.query, []string{"provider_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.DiscoverProviderModelCandidatesWithResponse(ctx, &query)
		expected = http.StatusOK
	case "ResetProviderModelField":
		env, err := bindEnvelope(params, memberForbidden, memberRequired, memberRequired, memberForbidden)
		if err != nil {
			return nil, err
		}
		var query protocol.ResetProviderModelFieldParams
		if err := bindQuery(env.query, []string{"provider_id", "model_id"}, &query); err != nil {
			return nil, err
		}
		response, callErr = c.ResetProviderModelFieldWithResponse(ctx, protocol.ModelField(env.field), &query)
		expected = http.StatusOK
	case "GetWarnings":
		if _, err := bindEnvelope(params, memberForbidden, memberForbidden, memberForbidden, memberForbidden); err != nil {
			return nil, err
		}
		response, callErr = c.GetWarningsWithResponse(ctx)
		expected = http.StatusOK
	default:
		return nil, invalidParams("unknown operation %q", operation)
	}
	if callErr != nil {
		return nil, callErr
	}
	return mappedResponse(response.StatusCode(), response.GetBody(), expected)
}

// wireResponse is the local structural shape every generated response type
// implements: the raw body and received status one operation call maps.
type wireResponse interface {
	StatusCode() int
	GetBody() []byte
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

// mappedResponse returns one generated response's exact raw JSON after the
// expected-status and required-body checks: a 204 is the JSON null, and a
// typed success must carry one complete JSON document. A received non-2xx
// body is mapped through the shared checker.
func mappedResponse(status int, body []byte, expected int) (json.RawMessage, error) {
	if status == expected {
		if expected == http.StatusNoContent {
			return json.RawMessage("null"), nil
		}
		if !json.Valid(body) {
			return nil, fmt.Errorf("runtime returned an empty or malformed success body for status %d", status)
		}
		return json.RawMessage(body), nil
	}
	if err := CheckResponse(status, body); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("runtime returned status %d for an operation expecting %d", status, expected)
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
