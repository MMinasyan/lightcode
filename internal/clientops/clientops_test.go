package clientops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/protocol"
)

// The capture fixture builds one generated client directly over the
// capturing transport: Call owns only the typed request/response surface, so
// its unit boundary needs no health route, discovery record, authentication,
// or event subscription.

type scriptCall struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	body        string
}

type scriptDoer struct {
	calls   []scriptCall
	respond func(*http.Request) (*http.Response, error)
}

func (d *scriptDoer) Do(request *http.Request) (*http.Response, error) {
	body := ""
	if request.Body != nil {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = string(data)
		_ = request.Body.Close()
	}
	d.calls = append(d.calls, scriptCall{
		method:      request.Method,
		path:        request.URL.EscapedPath(),
		rawQuery:    request.URL.RawQuery,
		contentType: request.Header.Get("Content-Type"),
		body:        body,
	})
	if d.respond != nil {
		return d.respond(request)
	}
	return scriptedResponse(http.StatusNoContent, ""), nil
}

func (d *scriptDoer) one(t *testing.T) scriptCall {
	t.Helper()
	if len(d.calls) != 1 {
		t.Fatalf("transport received %d requests, want exactly one", len(d.calls))
	}
	return d.calls[0]
}

func scriptedResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newScriptFixture wraps the capturing transport in one generated client.
func newScriptFixture(t *testing.T) (*client.Client, *scriptDoer) {
	t.Helper()
	doer := &scriptDoer{}
	generated, err := protocol.NewClientWithResponses("http://runtime.invalid", protocol.WithHTTPClient(doer))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return &client.Client{ClientWithResponses: generated}, doer
}

// dispatchCase is one supported operation as its caller addresses it.
type dispatchCase struct {
	name      string
	operation string
	params    string
	status    int
	response  string
	method    string
	path      string
	rawQuery  string
	body      string
}

var dispatchCases = []dispatchCase{
	{
		name: "GetHealth", operation: "GetHealth",
		status: 200, response: `{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"1"}`,
		method: "GET", path: "/v1/health",
	},
	{
		name: "ListSessions", operation: "ListSessions",
		params: `{"query":{"workspace":"/ws","lifecycle":"open"}}`,
		status: 200, response: `[{"session_id":"s1"}]`,
		method: "GET", path: "/v1/sessions", rawQuery: "workspace=%2Fws&lifecycle=open",
	},
	{
		name: "ListSessions passes the workspace value to the generated serializer", operation: "ListSessions",
		params: `{"query":{"workspace":"/ws dir","lifecycle":"open"}}`,
		status: 200, response: `[]`,
		method: "GET", path: "/v1/sessions", rawQuery: "workspace=%2Fws+dir&lifecycle=open",
	},
	{
		name: "CreateSession", operation: "CreateSession",
		params: `{"body":{"workspace":"/ws","agent_type":"primary","opaque":9007199254740993}}`,
		status: 201, response: `{"session_id":"s1","durable_revision":"9007199254740993"}`,
		method: "POST", path: "/v1/sessions",
		body: `{"workspace":"/ws","agent_type":"primary","opaque":9007199254740993}`,
	},
	{
		name: "DeleteSession", operation: "DeleteSession",
		params: `{"session_id":"s1"}`,
		status: 204, response: "",
		method: "DELETE", path: "/v1/sessions/s1",
	},
	{
		name: "ArchiveSession", operation: "ArchiveSession",
		params: `{"session_id":"s1"}`,
		status: 200, response: `{"session_id":"s1","lifecycle":"archived"}`,
		method: "POST", path: "/v1/sessions/s1/archive",
	},
	{
		name: "ReopenSession", operation: "ReopenSession",
		params: `{"session_id":"s1"}`,
		status: 200, response: `{"session_id":"s1","lifecycle":"open"}`,
		method: "POST", path: "/v1/sessions/s1/reopen",
	},
	{
		name: "SubmitSession", operation: "SubmitSession",
		params: `{"session_id":"s1","body":{"operation_id":"op-submit","mode":"regular","extra":true}}`,
		status: 200, response: `{"operation_id":"op-submit"}`,
		method: "POST", path: "/v1/sessions/s1/submit",
		body: `{"operation_id":"op-submit","mode":"regular","extra":true}`,
	},
	{
		name: "CompactSession", operation: "CompactSession",
		params: `{"session_id":"s1","body":{"operation_id":"op-compact"}}`,
		status: 200, response: `{"operation_id":"op-compact"}`,
		method: "POST", path: "/v1/sessions/s1/compact",
		body: `{"operation_id":"op-compact"}`,
	},
	{
		name: "ForkSession", operation: "ForkSession",
		params: `{"session_id":"s1","body":{"boundary_item_id":"itm","content":[],"operation_id":"op-fork"}}`,
		status: 200, response: `{"session":{"session_id":"s2"},"operation":{"operation_id":"op-fork"}}`,
		method: "POST", path: "/v1/sessions/s1/fork",
		body: `{"boundary_item_id":"itm","content":[],"operation_id":"op-fork"}`,
	},
	{
		name: "SetSessionAgentType", operation: "SetSessionAgentType",
		params: `{"session_id":"s1","body":{"agent_type":"integrated"}}`,
		status: 200, response: `{"session_id":"s1","agent_type":"integrated"}`,
		method: "PUT", path: "/v1/sessions/s1/agent-type",
		body: `{"agent_type":"integrated"}`,
	},
	{
		name: "InterruptSession", operation: "InterruptSession",
		params: `{"session_id":"s1"}`,
		status: 204, response: "",
		method: "POST", path: "/v1/sessions/s1/interrupt",
	},
	{
		name: "StopSession", operation: "StopSession",
		params: `{"session_id":"s1"}`,
		status: 204, response: "",
		method: "POST", path: "/v1/sessions/s1/stop",
	},
	{
		name: "GetSessionHydration", operation: "GetSessionHydration",
		params: `{"session_id":"s1"}`,
		status: 200, response: `{"session":{"session_id":"s1","workspace":"/ws"}}`,
		method: "GET", path: "/v1/sessions/s1/hydration",
	},
	{
		name: "GetSessionHistory without a cursor", operation: "GetSessionHistory",
		params: `{"session_id":"s1","query":{}}`,
		status: 200, response: `{"items":[],"older_cursor":"c1"}`,
		method: "GET", path: "/v1/sessions/s1/history",
	},
	{
		name: "GetSessionHistory without a query member", operation: "GetSessionHistory",
		params: `{"session_id":"s1"}`,
		status: 200, response: `{"items":[],"older_cursor":"c1"}`,
		method: "GET", path: "/v1/sessions/s1/history",
	},
	{
		name: "GetSessionHistory with a cursor", operation: "GetSessionHistory",
		params: `{"session_id":"s1","query":{"cursor":"abc"}}`,
		status: 200, response: `{"items":[],"older_cursor":null}`,
		method: "GET", path: "/v1/sessions/s1/history", rawQuery: "cursor=abc",
	},
	{
		name: "GetSessionCodeSnapshots", operation: "GetSessionCodeSnapshots",
		params: `{"session_id":"s1"}`,
		status: 200, response: `[]`,
		method: "GET", path: "/v1/sessions/s1/code-snapshots",
	},
	{
		name: "RevertSessionCode", operation: "RevertSessionCode",
		params: `{"session_id":"s1","body":{"operation_id":"op-revert"}}`,
		status: 200, response: `{"restored":["a.go"],"skipped":[]}`,
		method: "POST", path: "/v1/sessions/s1/revert-code",
		body: `{"operation_id":"op-revert"}`,
	},
	{
		name: "ListWorkspaces", operation: "ListWorkspaces",
		status: 200, response: `[]`,
		method: "GET", path: "/v1/workspaces",
	},
	{
		name: "ReadWorkspaceFile", operation: "ReadWorkspaceFile",
		params: `{"body":{"workspace":"/ws","path":"a.go"}}`,
		status: 200, response: `{"path":"/ws/a.go","content":"package a"}`,
		method: "POST", path: "/v1/workspaces/files/read",
		body: `{"workspace":"/ws","path":"a.go"}`,
	},
	{
		name: "GetRetainedCodeSnapshots", operation: "GetRetainedCodeSnapshots",
		params: `{"query":{"workspace":"/ws","session_id":"0123456789abcdef0123456789abcdef"}}`,
		status: 200, response: `{"turns":[]}`,
		method: "GET", path: "/v1/retained-code-snapshots",
		rawQuery: "workspace=%2Fws&session_id=0123456789abcdef0123456789abcdef",
	},
	{
		name: "RevertRetainedCode", operation: "RevertRetainedCode",
		params: `{"body":{"session_id":"0123456789abcdef0123456789abcdef","turn":1}}`,
		status: 200, response: `{"restored":["a.go"],"skipped":[]}`,
		method: "POST", path: "/v1/retained-code-snapshots/revert",
		body: `{"session_id":"0123456789abcdef0123456789abcdef","turn":1}`,
	},
	{
		name: "GetConfiguration", operation: "GetConfiguration",
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"1"}}`,
		method: "GET", path: "/v1/configuration",
	},
	{
		name: "ReloadConfiguration", operation: "ReloadConfiguration",
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"2"}}`,
		method: "POST", path: "/v1/configuration/reload",
	},
	{
		name: "UpdateConfigurationSettings", operation: "UpdateConfigurationSettings",
		params: `{"body":{"plugins":{"raw":"{\"n\":9007199254740993}"},"sessions":{}}}`,
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"3"}}`,
		method: "PUT", path: "/v1/configuration/settings",
		body: `{"plugins":{"raw":"{\"n\":9007199254740993}"},"sessions":{}}`,
	},
	{
		name: "SetAgentTypeModel", operation: "SetAgentTypeModel",
		params: `{"query":{"agent_type":"primary"},"body":{"model":"prov/m"}}`,
		status: 200, response: `{"agent_type":"primary","selected_model":"prov/m"}`,
		method: "PUT", path: "/v1/agent-types/model", rawQuery: "agent_type=primary",
		body: `{"model":"prov/m"}`,
	},
	{
		name: "ListModels", operation: "ListModels",
		params: `{"query":{"all":false}}`,
		status: 200, response: `{"models":[]}`,
		method: "GET", path: "/v1/models", rawQuery: "all=false",
	},
	{
		name: "ListModels with all", operation: "ListModels",
		params: `{"query":{"all":true}}`,
		status: 200, response: `{"models":[]}`,
		method: "GET", path: "/v1/models", rawQuery: "all=true",
	},
	{
		name: "ListProviders", operation: "ListProviders",
		status: 200, response: `{"providers":[]}`,
		method: "GET", path: "/v1/providers",
	},
	{
		name: "CreateProvider", operation: "CreateProvider",
		params: `{"body":{"id":"prov","provider":{"name":"P"},"models":{},"api_key":"secret"}}`,
		status: 200, response: `{"provider":{"id":"prov","name":"P"}}`,
		method: "POST", path: "/v1/providers",
		body: `{"id":"prov","provider":{"name":"P"},"models":{},"api_key":"secret"}`,
	},
	{
		name: "GetProviderDetail", operation: "GetProviderDetail",
		params: `{"query":{"provider_id":"prov"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "GET", path: "/v1/providers/detail", rawQuery: "provider_id=prov",
	},
	{
		name: "UpdateProviderDetail", operation: "UpdateProviderDetail",
		params: `{"query":{"provider_id":"prov"},"body":{"options":{"k":1}}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "PUT", path: "/v1/providers/detail", rawQuery: "provider_id=prov",
		body: `{"options":{"k":1}}`,
	},
	{
		name: "DeleteProviderDetail", operation: "DeleteProviderDetail",
		params: `{"query":{"provider_id":"prov"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "DELETE", path: "/v1/providers/detail", rawQuery: "provider_id=prov",
	},
	{
		name: "DiscoverProviderCandidates", operation: "DiscoverProviderCandidates",
		params: `{"body":{"base_url":"http://provider.example","api_key_env":"PROVIDER_KEY"}}`,
		status: 200, response: `[]`,
		method: "POST", path: "/v1/providers/discover",
		body: `{"base_url":"http://provider.example","api_key_env":"PROVIDER_KEY"}`,
	},
	{
		name: "ConnectProvider with an empty body", operation: "ConnectProvider",
		params: `{"query":{"provider_id":"prov"},"body":{}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "POST", path: "/v1/providers/connect", rawQuery: "provider_id=prov",
		body: `{}`,
	},
	{
		name: "DisconnectProvider", operation: "DisconnectProvider",
		params: `{"query":{"provider_id":"prov"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "POST", path: "/v1/providers/disconnect", rawQuery: "provider_id=prov",
	},
	{
		name: "ResetProviderField", operation: "ResetProviderField",
		params: `{"field":"name","query":{"provider_id":"prov"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "DELETE", path: "/v1/providers/fields/name", rawQuery: "provider_id=prov",
	},
	{
		name: "ResetProviderField passes an unknown enum to Runtime", operation: "ResetProviderField",
		params: `{"field":"not_a_field","query":{"provider_id":"prov"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "DELETE", path: "/v1/providers/fields/not_a_field", rawQuery: "provider_id=prov",
	},
	{
		name: "ListProviderModels", operation: "ListProviderModels",
		params: `{"query":{"provider_id":"prov"}}`,
		status: 200, response: `{"models":[]}`,
		method: "GET", path: "/v1/providers/models", rawQuery: "provider_id=prov",
	},
	{
		name: "UpdateProviderModel", operation: "UpdateProviderModel",
		params: `{"query":{"provider_id":"prov","model_id":"m"},"body":{"name":"M"}}`,
		status: 200, response: `{"model":{"id":"m"}}`,
		method: "PUT", path: "/v1/providers/models", rawQuery: "provider_id=prov&model_id=m",
		body: `{"name":"M"}`,
	},
	{
		name: "DeleteProviderModel", operation: "DeleteProviderModel",
		params: `{"query":{"provider_id":"prov","model_id":"m"}}`,
		status: 200, response: `{"model":{"id":"m"}}`,
		method: "DELETE", path: "/v1/providers/models", rawQuery: "provider_id=prov&model_id=m",
	},
	{
		name: "DiscoverProviderModelCandidates", operation: "DiscoverProviderModelCandidates",
		params: `{"query":{"provider_id":"prov"}}`,
		status: 200, response: `[]`,
		method: "POST", path: "/v1/providers/models/discover", rawQuery: "provider_id=prov",
	},
	{
		name: "ResetProviderModelField", operation: "ResetProviderModelField",
		params: `{"field":"name","query":{"provider_id":"prov","model_id":"m"}}`,
		status: 200, response: `{"model":{"id":"m"}}`,
		method: "DELETE", path: "/v1/providers/models/fields/name",
		rawQuery: "provider_id=prov&model_id=m",
	},
	{
		name: "GetWarnings", operation: "GetWarnings",
		status: 200, response: `{"warnings":[],"warnings_revision":{"instance_id":"0123456789abcdef0123456789abcdef","revision":"1"}}`,
		method: "GET", path: "/v1/warnings",
	},
}

// TestCallDispatchesEveryOperation drives every supported invocation name
// through one connected client with a capturing transport and asserts the
// exact generated method, path, query, raw body, and returned response bytes.
func TestCallDispatchesEveryOperation(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range dispatchCases {
		t.Run(tc.name, func(t *testing.T) {
			seen[tc.operation] = true
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(tc.status, tc.response), nil
			}
			result, err := clientops.Call(context.Background(), c, tc.operation, json.RawMessage(tc.params))
			if err != nil {
				t.Fatalf("Call(%s) = %v", tc.operation, err)
			}
			call := doer.one(t)
			if call.method != tc.method {
				t.Errorf("method = %q, want %q", call.method, tc.method)
			}
			if call.path != tc.path {
				t.Errorf("path = %q, want %q", call.path, tc.path)
			}
			if call.rawQuery != tc.rawQuery {
				t.Errorf("query = %q, want %q", call.rawQuery, tc.rawQuery)
			}
			if call.body != tc.body {
				t.Errorf("body = %q, want %q", call.body, tc.body)
			}
			if tc.body != "" && call.contentType != "application/json" {
				t.Errorf("content type = %q, want application/json", call.contentType)
			}
			want := tc.response
			if tc.status == http.StatusNoContent {
				want = "null"
			}
			if string(result) != want {
				t.Errorf("result = %s, want %s", result, want)
			}
		})
	}
	names := clientops.OperationNames()
	if len(names) != len(seen) {
		t.Fatalf("dispatch table covers %d of %d supported operations", len(seen), len(names))
	}
	for _, name := range names {
		if !seen[name] {
			t.Errorf("supported operation %q has no dispatch case", name)
		}
	}
}

// TestCallAcceptsEmptyEnvelopePositives pins the operations whose declared
// envelope is the empty document.
func TestCallAcceptsEmptyEnvelopePositives(t *testing.T) {
	for _, params := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, `{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"1"}`), nil
		}
		result, err := clientops.Call(context.Background(), c, "GetHealth", params)
		if err != nil {
			t.Fatalf("Call(GetHealth, %q) = %v", params, err)
		}
		if string(result) == "" {
			t.Fatal("GetHealth returned no result")
		}
	}
}

// TestCallRejectsForbiddenEnvelope proves every undeclared, missing, null, or
// malformed envelope member, query member, and body is a local invalid-params
// failure that never reaches the transport.
func TestCallRejectsForbiddenEnvelope(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		params    string
	}{
		{"unknown operation", "GetEvents", `{}`},
		{"unknown operation name", "Nope", `{}`},
		{"empty operation name", "", `{}`},
		{"params is an array", "GetHealth", `[]`},
		{"params is a string", "GetHealth", `"x"`},
		{"params is a number", "GetHealth", `5`},
		{"params is null", "GetHealth", `null`},
		{"params carries trailing data", "GetHealth", `{} {}`},
		{"unknown envelope member", "GetHealth", `{"bogus":1}`},
		{"undeclared session_id", "GetHealth", `{"session_id":"s1"}`},
		{"missing session_id", "DeleteSession", `{}`},
		{"null session_id", "DeleteSession", `{"session_id":null}`},
		{"numeric session_id", "DeleteSession", `{"session_id":5}`},
		{"object session_id", "DeleteSession", `{"session_id":{}}`},
		{"undeclared field", "GetHealth", `{"field":"name"}`},
		{"missing field", "ResetProviderField", `{"query":{"provider_id":"prov"}}`},
		{"null field", "ResetProviderField", `{"field":null,"query":{"provider_id":"prov"}}`},
		{"numeric field", "ResetProviderField", `{"field":5,"query":{"provider_id":"prov"}}`},
		{"undeclared query", "GetHealth", `{"query":{}}`},
		{"missing query", "ListSessions", `{}`},
		{"null query", "ListSessions", `{"query":null}`},
		{"array query", "ListSessions", `{"query":[]}`},
		{"numeric query", "ListSessions", `{"query":5}`},
		{"query missing required member", "ListSessions", `{"query":{"workspace":"/ws"}}`},
		{"query required member is null", "ListSessions", `{"query":{"workspace":null,"lifecycle":"open"}}`},
		{"query provided optional member is null", "GetSessionHistory", `{"session_id":"s1","query":{"cursor":null}}`},
		{"optional query is null", "GetSessionHistory", `{"session_id":"s1","query":null}`},
		{"optional query is an array", "GetSessionHistory", `{"session_id":"s1","query":[]}`},
		{"optional query has an unknown member", "GetSessionHistory", `{"session_id":"s1","query":{"bogus":1}}`},
		{"query has an unknown member", "ListSessions", `{"query":{"workspace":"/ws","lifecycle":"open","x":1}}`},
		{"query member has the wrong type", "ListModels", `{"query":{"all":"yes"}}`},
		{"retained session_id is not the outer envelope member", "GetRetainedCodeSnapshots", `{"session_id":"s1","query":{"workspace":"/ws","session_id":"s1"}}`},
		{"undeclared body", "GetHealth", `{"body":{}}`},
		{"missing body", "CreateSession", `{}`},
		{"null body", "CreateSession", `{"body":null}`},
		{"array body", "CreateSession", `{"body":[]}`},
		{"string body", "CreateSession", `{"body":"x"}`},
		{"numeric body", "CreateSession", `{"body":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, doer := newScriptFixture(t)
			result, err := clientops.Call(context.Background(), c, tc.operation, json.RawMessage(tc.params))
			if len(doer.calls) != 0 {
				t.Fatalf("transport received %d requests for a rejected envelope", len(doer.calls))
			}
			if err == nil {
				t.Fatalf("Call(%s, %s) = %s, want a local invalid-params failure", tc.operation, tc.params, result)
			}
			if !errors.Is(err, clientops.ErrInvalidParams) {
				t.Fatalf("error = %v, want clientops.ErrInvalidParams", err)
			}
		})
	}
}

// TestOperationNames proves the one static list is exact and returned fresh.
func TestOperationNames(t *testing.T) {
	want := []string{
		"GetHealth", "ListSessions", "CreateSession", "DeleteSession", "ArchiveSession",
		"ReopenSession", "SubmitSession", "CompactSession", "ForkSession", "SetSessionAgentType",
		"InterruptSession", "StopSession", "GetSessionHydration", "GetSessionHistory",
		"GetSessionCodeSnapshots", "RevertSessionCode", "ListWorkspaces", "ReadWorkspaceFile",
		"GetRetainedCodeSnapshots", "RevertRetainedCode", "GetConfiguration", "ReloadConfiguration",
		"UpdateConfigurationSettings", "SetAgentTypeModel", "ListModels", "ListProviders",
		"CreateProvider", "GetProviderDetail", "UpdateProviderDetail", "DeleteProviderDetail",
		"DiscoverProviderCandidates", "ConnectProvider", "DisconnectProvider", "ResetProviderField",
		"ListProviderModels", "UpdateProviderModel", "DeleteProviderModel",
		"DiscoverProviderModelCandidates", "ResetProviderModelField", "GetWarnings",
	}
	names := clientops.OperationNames()
	if len(names) != len(want) {
		t.Fatalf("OperationNames() returned %d names, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("OperationNames()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	names[0] = "Mutated"
	if again := clientops.OperationNames(); again[0] != "GetHealth" {
		t.Fatalf("OperationNames() returned shared state: %q", again[0])
	}
}

// TestCheckResponseMapsDeclaredErrors covers the one shared response checker:
// every 2xx is success, and a non-2xx body is either the declared Error or
// the partial code-revert result.
func TestCheckResponseMapsDeclaredErrors(t *testing.T) {
	for _, status := range []int{200, 201, 204, 299} {
		if err := clientops.CheckResponse(status, nil); err != nil {
			t.Errorf("CheckResponse(%d) = %v, want success", status, err)
		}
	}
	t.Run("plain error", func(t *testing.T) {
		err := clientops.CheckResponse(http.StatusConflict, []byte(`{"code":"conflict","message":"in use","session_id":"s1"}`))
		var responseErr *clientops.ResponseError
		if !errors.As(err, &responseErr) {
			t.Fatalf("error = %v, want *clientops.ResponseError", err)
		}
		if responseErr.Status != http.StatusConflict {
			t.Errorf("status = %d, want %d", responseErr.Status, http.StatusConflict)
		}
		if responseErr.Payload.Code != protocol.Conflict || responseErr.Payload.Message != "in use" {
			t.Errorf("payload = %+v", responseErr.Payload)
		}
		if responseErr.Partial != nil {
			t.Errorf("partial = %+v, want nil", responseErr.Partial)
		}
		if responseErr.Error() != "in use" {
			t.Errorf("Error() = %q, want the returned message", responseErr.Error())
		}
	})
	t.Run("partial result", func(t *testing.T) {
		body := `{"error":{"code":"conflict","message":"partial restore"},"restored":["a.go"],"skipped":[{"path":"b.go","error":"denied"}]}`
		err := clientops.CheckResponse(http.StatusConflict, []byte(body))
		var responseErr *clientops.ResponseError
		if !errors.As(err, &responseErr) {
			t.Fatalf("error = %v, want *clientops.ResponseError", err)
		}
		if responseErr.Partial == nil {
			t.Fatal("partial = nil, want the complete generated result")
		}
		if len(responseErr.Partial.Restored) != 1 || responseErr.Partial.Restored[0] != "a.go" {
			t.Errorf("restored = %v", responseErr.Partial.Restored)
		}
		if len(responseErr.Partial.Skipped) != 1 || responseErr.Partial.Skipped[0].Path != "b.go" {
			t.Errorf("skipped = %v", responseErr.Partial.Skipped)
		}
		if responseErr.Payload.Message != "partial restore" {
			t.Errorf("payload = %+v", responseErr.Payload)
		}
		if responseErr.Error() != "partial restore" {
			t.Errorf("Error() = %q", responseErr.Error())
		}
	})
	t.Run("partial result with empty arrays", func(t *testing.T) {
		err := clientops.CheckResponse(http.StatusConflict, []byte(`{"error":{"code":"conflict","message":"none"},"restored":[],"skipped":[]}`))
		var responseErr *clientops.ResponseError
		if !errors.As(err, &responseErr) || responseErr.Partial == nil {
			t.Fatalf("error = %v, want a partial result", err)
		}
	})
}

// TestCheckResponseRejectsMalformedBodies proves every malformed body is a
// transport failure, never a forged protocol error.
func TestCheckResponseRejectsMalformedBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"not json", "not json"},
		{"array", `[]`},
		{"string", `"x"`},
		{"null", `null`},
		{"missing code", `{"message":"bad"}`},
		{"missing message", `{"code":"invalid"}`},
		{"null code", `{"code":null,"message":"bad"}`},
		{"null message", `{"code":"invalid","message":null}`},
		{"unknown error code", `{"code":"teapot","message":"bad"}`},
		{"partial missing restored", `{"error":{"code":"conflict","message":"x"},"skipped":[]}`},
		{"partial missing skipped", `{"error":{"code":"conflict","message":"x"},"restored":[]}`},
		{"partial null restored", `{"error":{"code":"conflict","message":"x"},"restored":null,"skipped":[]}`},
		{"partial null skipped", `{"error":{"code":"conflict","message":"x"},"restored":[],"skipped":null}`},
		{"partial missing error", `{"restored":[],"skipped":[]}`},
		{"partial null error", `{"error":null,"restored":[],"skipped":[]}`},
		{"partial error missing code", `{"error":{"message":"x"},"restored":[],"skipped":[]}`},
		{"partial error unknown code", `{"error":{"code":"teapot","message":"x"},"restored":[],"skipped":[]}`},
		{"partial restored is not an array", `{"error":{"code":"conflict","message":"x"},"restored":5,"skipped":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := clientops.CheckResponse(http.StatusConflict, []byte(tc.body))
			if err == nil {
				t.Fatal("CheckResponse accepted a malformed body")
			}
			var responseErr *clientops.ResponseError
			if errors.As(err, &responseErr) {
				t.Fatalf("malformed body mapped to a protocol error: %v", err)
			}
			if errors.Is(err, clientops.ErrInvalidParams) {
				t.Fatalf("malformed body mapped to a local binding failure: %v", err)
			}
		})
	}
}

// TestCallResponseMappingAndTransportFailures pins Call's response mapping:
// exact raw success bytes, the null 204 result, a received error as
// *ResponseError, and every transport or malformed failure as neither a
// protocol error nor a local binding failure.
func TestCallResponseMappingAndTransportFailures(t *testing.T) {
	t.Run("unexpected success status", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusCreated, `{}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertTransportFailure(t, err)
	})
	t.Run("malformed success body", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, "not json"), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertTransportFailure(t, err)
	})
	t.Run("empty success body", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, ""), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertTransportFailure(t, err)
	})
	t.Run("received runtime error", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusNotFound, `{"code":"not_found","message":"no such session"}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetSessionHydration", json.RawMessage(`{"session_id":"gone"}`))
		var responseErr *clientops.ResponseError
		if !errors.As(err, &responseErr) {
			t.Fatalf("error = %v, want *clientops.ResponseError", err)
		}
		if responseErr.Status != http.StatusNotFound || responseErr.Error() != "no such session" {
			t.Fatalf("response error = %+v", responseErr)
		}
	})
	t.Run("received partial result", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusConflict, `{"error":{"code":"conflict","message":"partial"},"restored":["a.go"],"skipped":[]}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "RevertSessionCode", json.RawMessage(`{"session_id":"s1","body":{}}`))
		var responseErr *clientops.ResponseError
		if !errors.As(err, &responseErr) || responseErr.Partial == nil {
			t.Fatalf("error = %v, want a partial response error", err)
		}
		if len(responseErr.Partial.Restored) != 1 {
			t.Fatalf("partial = %+v", responseErr.Partial)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		want := errors.New("dial failed")
		doer.respond = func(*http.Request) (*http.Response, error) { return nil, want }
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want the transport error", err)
		}
		assertTransportFailure(t, err)
	})
	t.Run("raw success bytes are returned unremarshaled", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		body := `{"a":9007199254740993,"b":"\\u003c","c":null}`
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, body), nil
		}
		result, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if string(result) != body {
			t.Fatalf("result = %s, want the exact response bytes %s", result, body)
		}
	})
}

func assertTransportFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a failure")
	}
	var responseErr *clientops.ResponseError
	if errors.As(err, &responseErr) {
		t.Fatalf("transport failure mapped to a protocol error: %v", err)
	}
	if errors.Is(err, clientops.ErrInvalidParams) {
		t.Fatalf("transport failure mapped to a local binding failure: %v", err)
	}
}
