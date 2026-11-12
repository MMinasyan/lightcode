package clientops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.Equal(t, len(d.calls), 1)
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
	testassert.NoError(t, err)
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
		status: 200, response: `{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"9"}`,
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
		params: `{"body":{"workspace":"/ws","agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"}},"opaque":9007199254740993}}`,
		status: 201, response: `{"session_id":"s1","durable_revision":"9007199254740993"}`,
		method: "POST", path: "/v1/sessions",
		body: `{"workspace":"/ws","agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"}},"opaque":9007199254740993}`,
	},
	{
		name: "DeleteSession", operation: "DeleteSession",
		params: `{"session_id":"s1"}`,
		status: 200, response: `{"cleanup_failures":[]}`,
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
		params: `{"session_id":"s1","body":{"agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"integrated"}}}}`,
		status: 200, response: `{"session_id":"s1"}`,
		method: "PUT", path: "/v1/sessions/s1/agent-type",
		body: `{"agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"integrated"}}}`,
	},
	{
		name: "SetSessionModel", operation: "SetSessionModel",
		params: `{"session_id":"s1","body":{"model":"prov/m"}}`,
		status: 200, response: `{"session_id":"s1"}`,
		method: "PUT", path: "/v1/sessions/s1/model",
		body: `{"model":"prov/m"}`,
	},
	{
		name: "InterruptSession", operation: "InterruptSession",
		params: `{"session_id":"s1"}`,
		status: 204, response: "",
		method: "POST", path: "/v1/sessions/s1/interrupt",
	},
	{
		name: "ListExtensionPackages", operation: "ListExtensionPackages",
		status: 200, response: `[{"id":"fixture","loaded_version":"1.0.0","selected_version":"2.0.0"}]`,
		method: "GET", path: "/v1/extensions/packages",
	},
	{
		name: "InstallExtensionPackage", operation: "InstallExtensionPackage",
		params: `{"body":{"source_dir":"/tmp/package"}}`,
		status: 200, response: `{"id":"fixture","loaded_version":"","selected_version":"1.0.0"}`,
		method: "POST", path: "/v1/extensions/packages", body: `{"source_dir":"/tmp/package"}`,
	},
	{
		name: "RemoveExtensionPackage", operation: "RemoveExtensionPackage",
		params: `{"query":{"id":"fixture"}}`,
		status: 204, response: "",
		method: "DELETE", path: "/v1/extensions/packages", rawQuery: "id=fixture",
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
		name: "GetConfiguration with opaque overflow extra_body numbers", operation: "GetConfiguration",
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"1"},"providers":[{"id":"prov","connected":true,"extra_body":{"n":9007199254740993,"big":1e400},"models":[{"id":"m","context_window":8192,"extra_body":{"n":9007199254740993},"source":"user"}]}]}`,
		method: "GET", path: "/v1/configuration",
	},
	{
		name: "ReloadConfiguration", operation: "ReloadConfiguration",
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"2"}}`,
		method: "POST", path: "/v1/configuration/reload",
	},
	{
		name: "UpdateConfigurationSettings", operation: "UpdateConfigurationSettings",
		params: `{"body":{"sessions":{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":0},"tools":{},"jobs":{},"compaction":{},"extensions":{"raw":"{\"n\":9007199254740993}"}}}`,
		status: 200, response: `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"3"},"result":{"sessions":{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":0},"tools":{},"jobs":{},"compaction":{},"extensions":{"raw":"{\"n\":9007199254740993}"}}}`,
		method: "PUT", path: "/v1/configuration/settings",
		body: `{"sessions":{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":0},"tools":{},"jobs":{},"compaction":{},"extensions":{"raw":"{\"n\":9007199254740993}"}}`,
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
		params: `{"query":{"provider_id":"prov"},"body":{"name":"Edited"}}`,
		status: 200, response: `{"provider":{"id":"prov"}}`,
		method: "PUT", path: "/v1/providers/detail", rawQuery: "provider_id=prov",
		body: `{"name":"Edited"}`,
	},
	{
		name: "UpdateProviderDetail with opaque overflow extra_body", operation: "UpdateProviderDetail",
		params: `{"query":{"provider_id":"prov"},"body":{"extra_body":{"n":9007199254740993,"big":1e400}}}`,
		status: 200, response: `{"provider":{"id":"prov","extra_body":{"n":9007199254740993,"big":1e400}}}`,
		method: "PUT", path: "/v1/providers/detail", rawQuery: "provider_id=prov",
		body: `{"extra_body":{"n":9007199254740993,"big":1e400}}`,
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
	{
		name: "ListExtensionCapabilities", operation: "ListExtensionCapabilities",
		status: 200, response: `[]`,
		method: "GET", path: "/v1/extensions/capabilities",
	},
	{
		name: "InvokeExtension", operation: "InvokeExtension",
		params: `{"body":{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":{"kind":"harness"},"arguments":{"big":9007199254740993},"request_id":"req-1"}}`,
		status: 200, response: `{"kind":"success","result":{"big":9007199254740993}}`,
		method: "POST", path: "/v1/extensions/invoke",
		body: `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":{"kind":"harness"},"arguments":{"big":9007199254740993},"request_id":"req-1"}`,
	},
}

func TestAgentRosterValidation(t *testing.T) {
	valid := `{"name":"primary","definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"},"description":"","subagent":false,"model":"","readonly":false,"write_dir":"","tools":[],"capabilities":[]}`
	duplicateName := `{"name":"primary","definition":{"extension_id":"guest","capability_id":"guest.definitions","definition_id":"primary"},"description":"","subagent":false,"model":"","readonly":false,"write_dir":"","tools":[],"capabilities":[]}`
	for _, row := range []struct {
		name  string
		raw   string
		want  int
		valid bool
	}{
		{"empty", `{"agents":[]}`, 0, true},
		{"supplied", `{"agents":[` + valid + `]}`, 1, true},
		{"duplicate display names", `{"agents":[` + valid + `,` + duplicateName + `]}`, 2, true},
		{"foreign-only primary", `{"agents":[` + duplicateName + `]}`, 1, true},
		{"absent", `{}`, 0, false},
		{"null", `{"agents":null}`, 0, false},
		{"incomplete", `{"agents":[{"name":"primary"}]}`, 0, false},
		{"null tool", `{"agents":[{"name":"primary","definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"},"description":"","subagent":false,"model":"","readonly":false,"write_dir":"","tools":[null],"capabilities":[]}]}`, 0, false},
		{"duplicate", `{"agents":[` + valid + `,` + valid + `]}`, 0, false},
		{"unknown", `{"agents":[{"name":"primary","definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"},"description":"","subagent":false,"model":"","readonly":false,"write_dir":"","tools":[],"capabilities":[],"unknown":true}]}`, 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := clientops.AgentRoster(json.RawMessage(row.raw))
			testassert.Require(t, (err == nil) == row.valid && (!row.valid || len(got) == row.want), "roster = %+v, %v", got, err)
		})
	}
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
			testassert.Require(t, err == nil, "Call(%s) = %v", tc.operation, err)
			call := doer.one(t)
			testassert.Check(t, call.method == tc.method, "method = %q, want %q", call.method, tc.method)
			testassert.Check(t, call.path == tc.path, "path = %q, want %q", call.path, tc.path)
			testassert.Check(t, call.rawQuery == tc.rawQuery, "query = %q, want %q", call.rawQuery, tc.rawQuery)
			testassert.Check(t, call.body == tc.body, "body = %q, want %q", call.body, tc.body)
			testassert.Check(t, tc.body == "" || call.contentType == "application/json", "content type = %q, want application/json", call.contentType)
			want := tc.response
			if tc.status == http.StatusNoContent {
				want = "null"
			}
			testassert.Check(t, string(result) == want, "result = %s, want %s", result, want)
		})
	}
	names := clientops.OperationNames()
	testassert.Equal(t, len(names), len(seen))
	for _, name := range names {
		testassert.Check(t, seen[name], "supported operation %q has no dispatch case", name)
	}
}

// TestCallDispatchesSettingsExtensionEdit drives the second arm of the
// settings/extension-edit request union through the same generated operation:
// the edit's document string is carried as exact text and only the declared
// request is sent.
func TestCallDispatchesSettingsExtensionEdit(t *testing.T) {
	c, doer := newScriptFixture(t)
	response := `{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"3"},"result":{"sessions":{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":0},"tools":{},"jobs":{},"compaction":{},"extensions":{}}}`
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(200, response), nil
	}
	params := json.RawMessage(`{"body":{"extension_edit":{"extension_id":"agents","edit_id":"set_model","arguments":"{\"definition_id\":\"primary\",\"model\":\"prov/m\"}"}}}`)
	result, err := clientops.Call(context.Background(), c, "UpdateConfigurationSettings", params)
	testassert.Require(t, err == nil, "Call(UpdateConfigurationSettings extension edit) = %v", err)
	call := doer.one(t)
	testassert.Equal(t, call.method, "PUT")
	testassert.Equal(t, call.path, "/v1/configuration/settings")
	testassert.Equal(t, call.body, `{"extension_edit":{"extension_id":"agents","edit_id":"set_model","arguments":"{\"definition_id\":\"primary\",\"model\":\"prov/m\"}"}}`)
	testassert.Equal(t, string(result), response)
}

// TestCallAcceptsEmptyEnvelopePositives pins the operations whose declared
// envelope is the empty document.
func TestCallAcceptsEmptyEnvelopePositives(t *testing.T) {
	for _, params := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, `{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"9"}`), nil
		}
		result, err := clientops.Call(context.Background(), c, "GetHealth", params)
		testassert.Require(t, err == nil, "Call(GetHealth, %q) = %v", params, err)
		testassert.Require(t, string(result) != "", "GetHealth returned no result")
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
			testassert.Equal(t, len(doer.calls), 0)
			testassert.Require(t, err != nil, "Call(%s, %s) = %s, want a local invalid-params failure", tc.operation, tc.params, result)
			testassert.Require(t, errors.Is(err, clientops.ErrInvalidParams), "error = %v, want clientops.ErrInvalidParams", err)
		})
	}
}

// TestOperationNames proves the list is returned fresh; exact name/dispatch
// coverage lives in the invocation-table test.
func TestOperationNames(t *testing.T) {
	names := clientops.OperationNames()
	testassert.Require(t, len(names) != 0, "OperationNames() is empty")
	first := names[0]
	names[0] = "Mutated"
	{
		again := clientops.OperationNames()
		testassert.Equal(t, again[0], first)
	}
}

// TestInvocationSurfaceMatchesSchema derives the native operation set from the
// committed schema itself — every declared operation whose success content is
// not the event stream — and requires the advertisement, the captured dispatch
// table and that derived set to agree in both directions. A schema addition
// without a dispatch case, or a dispatch case for an operation the schema does
// not declare, fails here independently of the generated table.
func TestInvocationSurfaceMatchesSchema(t *testing.T) {
	doc := schemaDocument(t)
	declared := map[string]bool{}
	streamed := map[string]bool{}
	for _, path := range sortedKeys(doc.Paths.Map()) {
		item := doc.Paths.Map()[path]
		methods := item.Operations()
		for _, method := range sortedKeys(methods) {
			operation := methods[method]
			testassert.Require(t, operation.OperationID != "", "%s %s: operation has no operationId", method, path)
			testassert.Require(t, !declared[operation.OperationID] && !streamed[operation.OperationID], "operation %q is declared twice", operation.OperationID)
			if operationDeclaresStream(t, operation) {
				streamed[operation.OperationID] = true
				continue
			}
			declared[operation.OperationID] = true
		}
	}
	testassert.Require(t, len(declared) != 0 && len(streamed) != 0, "schema declares %d native operations and %d event streams; both sets must be non-empty", len(declared), len(streamed))
	names := clientops.OperationNames()
	testassert.Check(t, sort.StringsAreSorted(names), "OperationNames() is not sorted: %v", names)
	advertised := map[string]bool{}
	for _, name := range names {
		testassert.Check(t, declared[name], "advertised operation %q is not a declared native operation", name)
		testassert.Check(t, !advertised[name], "advertised operation %q appears twice", name)
		advertised[name] = true
	}
	dispatched := map[string]bool{}
	for _, tc := range dispatchCases {
		testassert.Check(t, declared[tc.operation], "dispatch case %q names %q, which is not a declared native operation", tc.name, tc.operation)
		dispatched[tc.operation] = true
	}
	for name := range declared {
		testassert.Check(t, advertised[name], "declared native operation %q is not advertised", name)
		testassert.Check(t, dispatched[name], "declared native operation %q has no dispatch case", name)
	}
	for name := range streamed {
		testassert.Check(t, !advertised[name], "declared event stream %q is advertised as a native operation", name)
	}
	// The test-only native invocation contract is the second generated
	// artifact: its root must name exactly the schema's native operations and
	// carry no stream entry.
	contractData, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "generated", "native-invocations.json"))
	testassert.NoError(t, err)
	var contract map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(contractData, &contract))
	for name := range declared {
		{
			_, ok := contract[name]
			testassert.Check(t, ok, "declared native operation %q is missing from the native invocation contract", name)
		}
	}
	for name := range contract {
		testassert.Check(t, declared[name], "the native invocation contract names %q, which is not a declared native operation", name)
	}
}

// operationDeclaresStream reports whether one declared operation's success
// content is the event stream.
func operationDeclaresStream(t *testing.T, operation *openapi3.Operation) bool {
	t.Helper()
	for status, response := range operation.Responses.Map() {
		if len(status) != 3 || status[0] != '2' {
			continue
		}
		testassert.Require(t, response.Value != nil, "operation %s success status %s is unresolved", operation.OperationID, status)
		if response.Value.Content["text/event-stream"] != nil {
			return true
		}
	}
	return false
}

// schemaDocument loads and validates the committed protocol schema for the
// calling test.
func schemaDocument(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join("..", "..", "protocol", "openapi.yaml"))
	testassert.NoError(t, err)
	testassert.NoError(t, doc.Validate(context.Background()))
	return doc
}

// sortedKeys returns one string-keyed map's keys in sorted order.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestInvokeMapsDeclaredErrors drives the shared response owner over the
// accepting fixture: a declared non-2xx body is either the Error or the
// partial code-revert result, each returned beside the retained received
// status and exact body bytes.
func TestInvokeMapsDeclaredErrors(t *testing.T) {
	invokeRejection := func(t *testing.T, operation, body string) (int, json.RawMessage, error) {
		t.Helper()
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusConflict, body), nil
		}
		params := json.RawMessage(nil)
		if operation == "RevertSessionCode" {
			params = json.RawMessage(`{"session_id":"s1","body":{"boundary_operation_id":"op"}}`)
		}
		return clientops.Invoke(context.Background(), c, operation, params)
	}
	t.Run("plain error", func(t *testing.T) {
		const payload = `{"code":"conflict","message":"in use","session_id":"s1"}`
		status, raw, err := invokeRejection(t, "GetHealth", payload)
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr), "error = %v, want *clientops.ResponseError", err)
		testassert.Require(t, status == http.StatusConflict && string(raw) == payload, "Invoke = (%d, %q), want the received status and body", status, raw)
		testassert.Check(t, responseErr.Status == http.StatusConflict, "status = %d, want %d", responseErr.Status, http.StatusConflict)
		testassert.Check(t, responseErr.Payload.Code == protocol.ErrorCodeConflict && responseErr.Payload.Message == "in use", "payload = %+v", responseErr.Payload)
		testassert.Check(t, responseErr.Partial == nil, "partial = %+v, want nil", responseErr.Partial)
		testassert.Check(t, string(responseErr.Body) == payload, "body = %q, want the exact received body", responseErr.Body)
		if responseErr.Error() != "in use" {
			t.Errorf("Error() = %q, want the returned message", responseErr.Error())
		}
		// Declared and malformed remain disjoint outcomes.
		testassert.Require(t, !errors.Is(err, clientops.ErrInvalidResponse), "declared rejection wrapped ErrInvalidResponse: %v", err)
	})
	t.Run("partial result", func(t *testing.T) {
		const payload = `{"error":{"code":"conflict","message":"partial restore"},"restored":["a.go"],"skipped":[{"path":"b.go","error":"denied"}]}`
		status, raw, err := invokeRejection(t, "RevertSessionCode", payload)
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr), "error = %v, want *clientops.ResponseError", err)
		testassert.Require(t, status == http.StatusConflict && string(raw) == payload, "Invoke = (%d, %q), want the received status and body", status, raw)
		testassert.Require(t, responseErr.Partial != nil, "partial = nil, want the complete generated result")
		testassert.Check(t, string(responseErr.Body) == payload, "body = %q, want the exact received body", responseErr.Body)
		testassert.Check(t, len(responseErr.Partial.Restored) == 1 && responseErr.Partial.Restored[0] == "a.go", "restored = %v", responseErr.Partial.Restored)
		testassert.Check(t, len(responseErr.Partial.Skipped) == 1 && responseErr.Partial.Skipped[0].Path == "b.go", "skipped = %v", responseErr.Partial.Skipped)
		testassert.Check(t, responseErr.Payload.Message == "partial restore", "payload = %+v", responseErr.Payload)
		if responseErr.Error() != "partial restore" {
			t.Errorf("Error() = %q", responseErr.Error())
		}
	})
	t.Run("partial result with empty arrays", func(t *testing.T) {
		_, _, err := invokeRejection(t, "RevertSessionCode", `{"error":{"code":"conflict","message":"none"},"restored":[],"skipped":[]}`)
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr) && responseErr.Partial != nil, "error = %v, want a partial result", err)
	})
	t.Run("baseline partial", func(t *testing.T) {
		partial := "{\n  \"big\": 9007199254740993,\n  \"text\": \"line\\\\nbreak\"\n}"
		payload := `{"code":"conflict","message":"baseline partial","partial_result":` + partial + `}`
		status, raw, err := invokeRejection(t, "GetHealth", payload)
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr), "error = %v, want a baseline ResponseError", err)
		testassert.Require(t, responseErr.Partial == nil, "baseline partial was labelled a code-revert partial")
		testassert.Require(t, responseErr.Payload.PartialResult != nil, "payload omitted partial_result")
		testassert.Require(t, string(*responseErr.Payload.PartialResult) == partial, "payload partial = %q, want %q", *responseErr.Payload.PartialResult, partial)
		testassert.Require(t, string(responseErr.Body) == payload && string(raw) == payload && status == http.StatusConflict, "baseline partial retained = (%d, %q, %q)", status, raw, responseErr.Body)
	})
	t.Run("baseline explicit null partial", func(t *testing.T) {
		const payload = `{"code":"conflict","message":"null partial","partial_result":null}`
		_, _, err := invokeRejection(t, "GetHealth", payload)
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr), "error = %v, want a baseline ResponseError", err)
		testassert.Require(t, responseErr.Payload.PartialResult != nil && string(*responseErr.Payload.PartialResult) == "null", "explicit null partial = %+v", responseErr.Payload.PartialResult)
		testassert.Require(t, string(responseErr.Body) == payload, "explicit null body = %q", responseErr.Body)
	})
}

// TestInvokeRejectsMalformedRejectionBodies proves every malformed declared
// non-2xx body is invalid delivery beside its retained status/body, never a
// forged protocol error and never a local binding failure.
func TestInvokeRejectsMalformedRejectionBodies(t *testing.T) {
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
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(http.StatusConflict, tc.body), nil
			}
			status, raw, err := clientops.Invoke(context.Background(), c, "GetHealth", nil)
			testassert.Require(t, err != nil, "Invoke accepted a malformed body")
			testassert.Require(t, status == http.StatusConflict && string(raw) == tc.body, "Invoke = (%d, %q), want the received status and body", status, raw)
			assertInvalidDelivery(t, err)
		})
	}
}

// TestCallResponseMappingAndTransportFailures pins Call's response mapping:
// exact raw success bytes, the null 204 result, a received error as
// *ResponseError, and every transport or malformed failure as neither a
// protocol error nor a local binding failure.
func TestCallResponseMappingAndTransportFailures(t *testing.T) {
	t.Run("unexpected success status is invalid delivery", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusCreated, `{}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertInvalidDelivery(t, err)
	})
	t.Run("malformed success body is invalid delivery", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, "not json"), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertInvalidDelivery(t, err)
	})
	t.Run("empty success body is invalid delivery", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, ""), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		assertInvalidDelivery(t, err)
	})
	t.Run("received runtime error", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusNotFound, `{"code":"not_found","message":"no such session"}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "GetSessionHydration", json.RawMessage(`{"session_id":"gone"}`))
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr), "error = %v, want *clientops.ResponseError", err)
		testassert.Require(t, responseErr.Status == http.StatusNotFound && responseErr.Error() == "no such session", "response error = %+v", responseErr)
	})
	t.Run("received partial result", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusConflict, `{"error":{"code":"conflict","message":"partial"},"restored":["a.go"],"skipped":[]}`), nil
		}
		_, err := clientops.Call(context.Background(), c, "RevertSessionCode", json.RawMessage(`{"session_id":"s1","body":{}}`))
		var responseErr *clientops.ResponseError
		testassert.Require(t, errors.As(err, &responseErr) && responseErr.Partial != nil, "error = %v, want a partial response error", err)
		testassert.Require(t, len(responseErr.Partial.Restored) == 1, "partial = %+v", responseErr.Partial)
	})
	t.Run("transport failure", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		want := errors.New("dial failed")
		doer.respond = func(*http.Request) (*http.Response, error) { return nil, want }
		_, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		testassert.Require(t, errors.Is(err, want), "error = %v, want the transport error", err)
		assertTransportFailure(t, err)
	})
	t.Run("raw success bytes are returned unremarshaled", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		body := `{"a":9007199254740993,"b":"\\u003c","c":null}`
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, body), nil
		}
		result, err := clientops.Call(context.Background(), c, "GetHealth", nil)
		testassert.NoError(t, err)
		testassert.Require(t, string(result) == body, "result = %s, want the exact response bytes %s", result, body)
	})
}

// TestInvokeRetainsStatusAndBodyOnFailure proves the lossless response owner:
// a body-read failure, a malformed success or rejection, and an unexpected
// success status all retain the received status and available body bytes,
// wrap ErrInvalidResponse, and never become a ResponseError. A declared
// non-2xx rejection returns the same status/body beside its ResponseError,
// and a valid 204 returns its status with the empty HTTP body.
func TestInvokeRetainsStatusAndBodyOnFailure(t *testing.T) {
	t.Run("body read failure retains the received status", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(&failingReader{data: []byte(`{"partial":`)}),
			}, nil
		}
		status, body, err := clientops.Invoke(context.Background(), c, "GetHealth", nil)
		testassert.Require(t, status == http.StatusOK && string(body) == `{"partial":`, "Invoke = (%d, %q), want the received status and partial bytes", status, body)
		assertInvalidDelivery(t, err)
	})
	t.Run("malformed success body", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, "not json"), nil
		}
		status, body, err := clientops.Invoke(context.Background(), c, "GetHealth", nil)
		testassert.Require(t, status == http.StatusOK && string(body) == "not json", "Invoke = (%d, %q), want the received status and body", status, body)
		assertInvalidDelivery(t, err)
	})
	t.Run("unexpected success status", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusCreated, `{}`), nil
		}
		status, body, err := clientops.Invoke(context.Background(), c, "GetHealth", nil)
		testassert.Require(t, status == http.StatusCreated && string(body) == `{}`, "Invoke = (%d, %q), want the received status and body", status, body)
		assertInvalidDelivery(t, err)
	})
	t.Run("valid 204 returns its status and empty body", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusNoContent, ""), nil
		}
		status, body, err := clientops.Invoke(context.Background(), c, "InterruptSession", json.RawMessage(`{"session_id":"s1"}`))
		testassert.NoError(t, err)
		testassert.Require(t, status == http.StatusNoContent && len(body) == 0, "Invoke = (%d, %q), want the 204 status and empty body", status, body)
	})
	t.Run("valid 200 deletion result retains its cleanup failures", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		body := `{"cleanup_failures":[{"extension_id":"guest","code":"unavailable","message":"extension data cleanup is unavailable"}]}`
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, body), nil
		}
		status, received, err := clientops.Invoke(context.Background(), c, "DeleteSession", json.RawMessage(`{"session_id":"s1"}`))
		testassert.NoError(t, err)
		testassert.Require(t, status == http.StatusOK && string(received) == body, "Invoke = (%d, %q), want the 200 status and exact cleanup result", status, received)
	})
	t.Run("local binding failure returns zero status and no body", func(t *testing.T) {
		c, _ := newScriptFixture(t)
		status, body, err := clientops.Invoke(context.Background(), c, "Nope", nil)
		testassert.Require(t, status == 0 && body == nil && errors.Is(err, clientops.ErrInvalidParams), "Invoke = (%d, %q, %v), want the zero-status local binding failure", status, body, err)
	})
	t.Run("transport failure returns zero status and no body", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		want := errors.New("dial failed")
		doer.respond = func(*http.Request) (*http.Response, error) { return nil, want }
		status, body, err := clientops.Invoke(context.Background(), c, "GetHealth", nil)
		testassert.Require(t, status == 0 && body == nil && errors.Is(err, want), "Invoke = (%d, %q, %v), want the zero-status transport failure", status, body, err)
	})
}

// failingReader delivers its data once, then fails the read, so a body-read
// failure still yields the received prefix bytes and status.
type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("connection reset")
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

func (r *failingReader) Close() error { return nil }

// assertInvalidDelivery proves one failure is ErrInvalidResponse and never a
// ResponseError or a local binding failure.
func assertInvalidDelivery(t *testing.T, err error) {
	t.Helper()
	testassert.Require(t, errors.Is(err, clientops.ErrInvalidResponse), "error = %v, want ErrInvalidResponse", err)
	var responseErr *clientops.ResponseError
	testassert.Require(t, !errors.As(err, &responseErr) && !errors.Is(err, clientops.ErrInvalidParams), "invalid delivery mapped to a protocol error: %v; invalid delivery mapped to a local binding failure: %v", err, err)
}

func assertTransportFailure(t *testing.T, err error) {
	t.Helper()
	testassert.Require(t, err != nil, "want a failure")
	var responseErr *clientops.ResponseError
	testassert.Require(t, !errors.As(err, &responseErr) && !errors.Is(err, clientops.ErrInvalidParams) && !errors.Is(err, clientops.ErrInvalidResponse), "transport failure mapped to a protocol error: %v; transport failure mapped to a local binding failure: %v; transport failure mapped to an invalid delivery: %v", err, err, err)
}

func TestInvokeExtensionDeclaredFailureAndDeliveryClasses(t *testing.T) {
	const declared = `{"kind":"failure","error":{"code":"guest_code","message":"guest message","data":{"big":9007199254740993}},"partial_result":["partial",null,1]}`
	c, doer := newScriptFixture(t)
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusUnprocessableEntity, declared), nil
	}
	_, _, err := clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	var extensionErr *clientops.ExtensionResponseError
	testassert.Require(t, errors.As(err, &extensionErr), "error = %v, want ExtensionResponseError", err)
	testassert.Require(t, extensionErr.Status == http.StatusUnprocessableEntity && extensionErr.Failure.Error.Code == "guest_code" && extensionErr.Failure.Error.Message == "guest message" && extensionErr.Failure.PartialResult != nil, "extension failure = %+v", extensionErr)
	testassert.Require(t, string(extensionErr.Body) == declared, "extension failure body = %q, want exact bytes", extensionErr.Body)
	var responseErr *clientops.ResponseError
	testassert.Require(t, !errors.As(err, &responseErr), "declared guest failure was misclassified as a baseline ResponseError")

	// A restore-shaped guest partial stays a guest failure, never the current
	// operation's restore partial.
	restoreShaped := `{"kind":"failure","error":{"code":"guest_code","message":"guest"},"partial_result":{"restored":[],"skipped":[],"error":{"code":"conflict","message":"guest"}}}`
	c, doer = newScriptFixture(t)
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusUnprocessableEntity, restoreShaped), nil
	}
	_, _, err = clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	testassert.Require(t, errors.As(err, &extensionErr) && extensionErr.Failure.PartialResult != nil, "restore-shaped guest failure = %v", err)
	testassert.Require(t, !errors.As(err, &responseErr) || responseErr.Partial != nil, "restore-shaped guest payload was misclassified")

	// The 422 schema's baseline Error alternative keeps its existing class.
	c, doer = newScriptFixture(t)
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusUnprocessableEntity, `{"code":"corrupt","message":"baseline"}`), nil
	}
	_, _, err = clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	testassert.Require(t, errors.As(err, &responseErr) && responseErr.Payload.Code == protocol.ErrorCodeCorrupt, "baseline 422 = %v", err)

	// Malformed wrappers retain their status and exact body as invalid
	// delivery, never a fabricated baseline error.
	for _, malformed := range []string{
		`{"kind":"failure","error":null}`,
		`{"kind":"failure","error":{"code":"","message":"x"}}`,
		`{"kind":"failure","error":{"code":"x","message":"y"},"unknown":true}`,
		`{"kind":"success","result":null}`,
		`{"kind":"success"}`,
	} {
		c, doer = newScriptFixture(t)
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusUnprocessableEntity, malformed), nil
		}
		status, body, invokeErr := clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
		testassert.Require(t, errors.Is(invokeErr, clientops.ErrInvalidResponse), "malformed %q error = %v, want invalid delivery", malformed, invokeErr)
		testassert.Require(t, status == http.StatusUnprocessableEntity && string(body) == malformed, "malformed %q retained = (%d, %q)", malformed, status, body)
	}

	// Success null is a present result value, not a missing one.
	c, doer = newScriptFixture(t)
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusOK, `{"kind":"success","result":null}`), nil
	}
	result, err := clientops.Call(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	testassert.Require(t, err == nil && string(result) == `{"kind":"success","result":null}`, "null success = (%s, %v)", result, err)
}

// TestRestoreShapeSelectsPartialOnlyForRevertSessionCode proves the
// restored/skipped member sniff is the current code-revert operation's own
// partial interpretation: every other operation refuses that body as invalid
// delivery with its status and exact bytes retained, never a fabricated
// ResponseError.Partial.
func TestRestoreShapeSelectsPartialOnlyForRevertSessionCode(t *testing.T) {
	const restoreShaped = `{"restored":["a.go"],"skipped":[],"error":{"code":"conflict","message":"restore-shaped"}}`
	const restoreParams = `{"session_id":"s1","body":{"boundary_operation_id":"op"}}`
	cases := []struct {
		name      string
		operation string
		params    string
	}{
		{"revert code", "RevertSessionCode", restoreParams},
		{"invoke extension", "InvokeExtension", `{"body":{}}`},
		{"unrelated ordinary operation", "SubmitSession", `{"session_id":"s1","body":{"operation_id":"op-submit","mode":"regular","content":[]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(http.StatusConflict, restoreShaped), nil
			}
			status, body, err := clientops.Invoke(context.Background(), c, tc.operation, json.RawMessage(tc.params))
			if tc.operation == "RevertSessionCode" {
				var responseErr *clientops.ResponseError
				testassert.Require(t, errors.As(err, &responseErr) && responseErr.Partial != nil, "RevertSessionCode error = %v, want the genuine partial", err)
				testassert.Require(t, status == http.StatusConflict && string(body) == restoreShaped, "RevertSessionCode retained = (%d, %q)", status, body)
				return
			}
			var responseErr *clientops.ResponseError
			testassert.Require(t, errors.Is(err, clientops.ErrInvalidResponse), "%s error = %v, want invalid delivery", tc.operation, err)
			testassert.Require(t, !errors.As(err, &responseErr) || responseErr.Partial != nil, "%s fabricated a ResponseError.Partial", tc.operation)
			testassert.Require(t, status == http.StatusConflict && string(body) == restoreShaped, "%s retained = (%d, %q)", tc.operation, status, body)
		})
	}
}

// TestExtensionFailureEnvelopeOnlyAt422 proves the declared failure arm is
// selected only for InvokeExtension's declared 422. The same envelope at any
// other status is invalid delivery, while baseline Error bodies keep their
// ordinary classification at 422 and 500.
func TestExtensionFailureEnvelopeOnlyAt422(t *testing.T) {
	const declared = `{"kind":"failure","error":{"code":"guest_code","message":"guest message"}}`
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(status, declared), nil
			}
			gotStatus, body, err := clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
			var extensionErr *clientops.ExtensionResponseError
			var responseErr *clientops.ResponseError
			testassert.Require(t, errors.Is(err, clientops.ErrInvalidResponse), "status %d error = %v, want invalid delivery", status, err)
			testassert.Require(t, !errors.As(err, &extensionErr) && !errors.As(err, &responseErr), "status %d fabricated a declared or baseline error", status)
			testassert.Require(t, gotStatus == status && string(body) == declared, "status %d retained = (%d, %q)", status, gotStatus, body)
		})
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"declared 422", http.StatusUnprocessableEntity, declared},
		{"baseline 422", http.StatusUnprocessableEntity, `{"code":"corrupt","message":"baseline"}`},
		{"baseline 500", http.StatusInternalServerError, `{"code":"storage","message":"baseline"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(tc.status, tc.body), nil
			}
			status, body, err := clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
			if tc.name == "declared 422" {
				var extensionErr *clientops.ExtensionResponseError
				testassert.Require(t, errors.As(err, &extensionErr) && extensionErr.Failure.Error.Code == "guest_code", "declared 422 = %v", err)
			} else {
				var responseErr *clientops.ResponseError
				testassert.Require(t, errors.As(err, &responseErr), "%s = %v, want the baseline ResponseError", tc.name, err)
			}
			testassert.Require(t, status == tc.status && string(body) == tc.body, "%s retained = (%d, %q)", tc.name, status, body)
		})
	}
}

// TestInvokeExtensionExactEnvelopeMembers proves the shared generated-value
// decoder rejects case-variant envelope members that encoding/json would
// otherwise bind case-insensitively, while opaque result/data objects keep
// their own independent member names.
func TestInvokeExtensionExactEnvelopeMembers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"declared failure Code sibling", http.StatusUnprocessableEntity, `{"kind":"failure","error":{"code":"","Code":"guest","message":"failure"}}`},
		{"success RESULT sibling", http.StatusOK, `{"kind":"success","result":{"ok":true},"RESULT":{"ok":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(tc.status, tc.body), nil
			}
			status, body, err := clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
			var extensionErr *clientops.ExtensionResponseError
			testassert.Require(t, errors.Is(err, clientops.ErrInvalidResponse) && !errors.As(err, &extensionErr), "error = %v, want invalid delivery without a declared failure", err)
			testassert.Require(t, status == tc.status && string(body) == tc.body, "retained = (%d, %q), want the exact received response", status, body)
		})
	}

	// Opaque result and failure-data objects are not envelope members: their
	// own case-varied keys stay untouched.
	c, doer := newScriptFixture(t)
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusOK, `{"kind":"success","result":{"Code":"guest","RESULT":9007199254740993}}`), nil
	}
	result, err := clientops.Call(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	testassert.Require(t, err == nil && strings.Contains(string(result), `"RESULT":9007199254740993`), "opaque result = (%s, %v)", result, err)

	c, doer = newScriptFixture(t)
	declared := `{"kind":"failure","error":{"code":"guest_code","message":"failure","data":{"Code":"guest","RESULT":9007199254740993}}}`
	doer.respond = func(*http.Request) (*http.Response, error) {
		return scriptedResponse(http.StatusUnprocessableEntity, declared), nil
	}
	_, _, err = clientops.Invoke(context.Background(), c, "InvokeExtension", json.RawMessage(`{"body":{}}`))
	var extensionErr *clientops.ExtensionResponseError
	testassert.Require(t, errors.As(err, &extensionErr) && extensionErr.Failure.Error.Data != nil && string(*extensionErr.Failure.Error.Data) == `{"Code":"guest","RESULT":9007199254740993}`, "opaque failure data = %v", err)
}
