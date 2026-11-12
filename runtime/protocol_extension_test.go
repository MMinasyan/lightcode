package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	workerfixture "github.com/MMinasyan/lightcode/extension/worker/testdata/fixture/native"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

// extensionOwnerOptions mounts the shared native fixture and one
// zero-capability extension over a value configuration.
func extensionOwnerOptions(t *testing.T) melta.Options {
	t.Helper()
	dataDir := t.TempDir()
	empty := extension.Native{
		Declaration: extension.Declaration{
			ID:           "empty",
			Version:      "1.0.0",
			ConfigSchema: json.RawMessage(`{"type":"object"}`),
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		},
	}
	return melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewValueConfigurationSource([]byte(`{"extensions":{"fixture":{"enabled":true},"empty":{"enabled":true}}}`)),
		Credentials:   melta.NewEnvironmentCredentialSource(dataDir + "/.env"),
		Extensions:    []extension.Native{workerfixture.Native(), empty},
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	testassert.NoError(t, err)
	return string(body)
}

// TestProtocolExtensionCatalogAndInvocation proves the HTTP owner: the
// catalog includes the zero-capability extension and hidden methods, success
// and declared-failure bodies preserve exact raw values, and every invalid
// request refuses before dispatch.
func TestProtocolExtensionCatalogAndInvocation(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, extensionOwnerOptions(t))
	client := mountedClient(t, server)
	catalogResponse, err := client.ListExtensionCapabilitiesWithResponse(ctx)
	testassert.NoError(t, err)
	testassert.Require(t, catalogResponse.JSON200 != nil, "catalog status = %d, body = %s", catalogResponse.StatusCode(), catalogResponse.Body)
	catalog := *catalogResponse.JSON200
	var empty, fixture *protocol.ExtensionCapabilityCatalog
	for i := range catalog {
		switch catalog[i].ExtensionId {
		case "empty":
			empty = &catalog[i]
		case "fixture":
			fixture = &catalog[i]
		}
	}
	testassert.Require(t, empty != nil && len(empty.Capabilities) == 0 && len(empty.Exposed) == 0, "zero-capability catalog entry = %+v", empty)
	testassert.Require(t, fixture != nil, "fixture catalog entry is missing")
	var sawRaw, sawRetain bool
	for _, capability := range fixture.Capabilities {
		for _, method := range capability.Methods {
			sawRetain = sawRetain || method.Name == "retain" && method.Mode == protocol.ExtensionMethodModeServiceIo
			sawRaw = sawRaw || method.Name == "raw"
		}
	}
	testassert.Require(t, sawRaw && sawRetain, "catalog omitted hidden/exposed method detail: %+v", fixture.Capabilities)

	invoke := func(body string) *http.Response {
		return rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/extensions/invoke", server.credential, body)
	}
	target := `{"kind":"harness"}`
	rawArguments := "{\n  \"big\": 9007199254740993,\n  \"text\": \"line\\n\\t<>&\"\n}"
	success := invoke(`{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":` + target + `,"arguments":` + rawArguments + `,"request_id":"req-1"}`)
	testassert.Require(t, success.StatusCode == http.StatusOK, "raw success status = %d", success.StatusCode)
	successBody := readBody(t, success)
	testassert.Require(t, strings.HasPrefix(successBody, `{"kind":"success","result":`) && strings.Contains(successBody, rawArguments), "raw success body lost bytes: %q", successBody)

	nullSuccess := invoke(`{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":` + target + `,"arguments":null,"request_id":"req-null"}`)
	nullBody := readBody(t, nullSuccess)
	testassert.Require(t, nullSuccess.StatusCode == http.StatusOK, "null success status = %d body = %s", nullSuccess.StatusCode, nullBody)
	testassert.Require(t, nullBody == `{"kind":"success","result":null}`, "null success body = %q, want the exact null result", nullBody)

	failure := invoke(`{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"fail","target":` + target + `,"arguments":{},"request_id":"req-fail"}`)
	testassert.Require(t, failure.StatusCode == http.StatusUnprocessableEntity, "declared failure status = %d", failure.StatusCode)
	failureBody := readBody(t, failure)
	testassert.Require(t, strings.Contains(failureBody, `"kind":"failure"`) && strings.Contains(failureBody, `"code":"guest_code"`) && strings.Contains(failureBody, `"message":"guest message"`) && strings.Contains(failureBody, "{\n  \"big\": 9007199254740993\n}") && strings.Contains(failureBody, `"partial_result":["partial", null, 1]`), "declared failure body lost exact payloads: %q", failureBody)

	baseline := invoke(`{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"corrupt","target":` + target + `,"arguments":{},"request_id":"req-corrupt"}`)
	baselineBody := readBody(t, baseline)
	testassert.Require(t, baseline.StatusCode == http.StatusUnprocessableEntity, "baseline 422 status = %d body = %s", baseline.StatusCode, baselineBody)
	var baselineError protocol.Error
	testassert.NoError(t, json.Unmarshal([]byte(baselineBody), &baselineError))
	testassert.Require(t, baselineError.Code == protocol.ErrorCodeCorrupt, "baseline 422 code = %q, want corrupt", baselineError.Code)

	for _, rejection := range []struct {
		name   string
		body   string
		status int
	}{
		{"unknown member", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":` + target + `,"arguments":{},"request_id":"req-x","operation_id":"forged"}`, http.StatusBadRequest},
		{"missing routing field", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":` + target + `,"arguments":{}}`, http.StatusBadRequest},
		{"missing target", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","arguments":{},"request_id":"req-x"}`, http.StatusBadRequest},
		{"target other member", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"raw","target":{"kind":"harness","session_id":"` + strings.Repeat("a", 32) + `"},"arguments":{},"request_id":"req-x"}`, http.StatusBadRequest},
		{"contract mismatch", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"other/v1","contract_version":"1","method":"raw","target":` + target + `,"arguments":{},"request_id":"req-x"}`, http.StatusBadRequest},
		{"arguments outside method schema", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"echo","target":` + target + `,"arguments":null,"request_id":"req-x"}`, http.StatusBadRequest},
		{"unexposed method", `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"assemble","target":` + target + `,"arguments":{},"request_id":"req-x"}`, http.StatusForbidden},
		{"unknown capability", `{"extension_id":"fixture","capability_id":"fixture.unknown","contract_id":"fixture.unknown/v1","contract_version":"1","method":"x","target":` + target + `,"arguments":{},"request_id":"req-x"}`, http.StatusNotFound},
	} {
		response := invoke(rejection.body)
		testassert.Require(t, response.StatusCode == rejection.status, "%s status = %d, want %d; body = %s", rejection.name, response.StatusCode, rejection.status, readBody(t, response))
	}

	// A malformed generated command argument refuses through the fixed
	// service's own generated-value decoder with no state mutation.
	create := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/sessions", server.credential, `{"workspace":"`+t.TempDir()+`"}`)
	createBody := readBody(t, create)
	testassert.Require(t, create.StatusCode == http.StatusCreated, "create session status = %d body = %s", create.StatusCode, createBody)
	var session protocol.Session
	testassert.NoError(t, json.Unmarshal([]byte(createBody), &session))
	malformed := invoke(`{"extension_id":"fixture","capability_id":"fixture.commands","contract_id":"fixture.commands/v1","contract_version":"1","method":"command","target":{"kind":"session","session_id":"` + session.SessionId + `"},"arguments":{"method":"submit","arguments":{"session_id":"forged","operation_id":"op-x","mode":"regular","content":[]}},"request_id":"req-command"}`)
	testassert.Require(t, malformed.StatusCode == http.StatusBadRequest, "malformed command status = %d body = %s", malformed.StatusCode, readBody(t, malformed))
}

// TestProtocolExtensionUnmappedCallErrorRetainsPartial proves a directly
// forwarded CallError keeps the ordinary classified baseline Error and its
// exact produced partial through the public invocation HTTP boundary.
func TestProtocolExtensionUnmappedCallErrorRetainsPartial(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, extensionOwnerOptions(t))
	invoke := func(mode string) (int, string) {
		body := `{"extension_id":"fixture","capability_id":"fixture.service","contract_id":"fixture.service/v1","contract_version":"1","method":"call_error","target":{"kind":"harness"},"arguments":{"mode":"` + mode + `"},"request_id":"req-call-error"}`
		response := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/extensions/invoke", server.credential, body)
		return response.StatusCode, readBody(t, response)
	}
	decode := func(received string) map[string]json.RawMessage {
		var members map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal([]byte(received), &members))
		return members
	}
	status, received := invoke("valid")
	members := decode(received)
	testassert.Require(t, status == http.StatusInternalServerError, "call-error status = %d body = %s", status, received)
	var code string
	testassert.NoError(t, json.Unmarshal(members["code"], &code))
	testassert.Require(t, code == "storage", "call-error code = %q, want storage", code)
	partial, ok := members["partial_result"]
	testassert.Require(t, ok, "call-error body omitted partial_result: %s", received)
	testassert.Require(t, string(partial) == "{\n  \"big\": 9007199254740993,\n  \"text\": \"line\\\\nbreak\"\n}", "partial_result = %q, want the exact produced bytes", partial)

	status, received = invoke("null")
	members = decode(received)
	testassert.Require(t, status == http.StatusInternalServerError && string(members["partial_result"]) == "null", "null partial = (%d, %s)", status, received)
	status, received = invoke("empty")
	members = decode(received)
	_, present := members["partial_result"]
	testassert.Require(t, status == http.StatusInternalServerError && !present, "empty partial = (%d, %s), want the member absent", status, received)
	status, received = invoke("bad")
	testassert.Require(t, status == http.StatusInternalServerError, "malformed partial status = %d body = %s", status, received)
	testassert.Require(t, json.Valid([]byte(received)), "malformed partial produced invalid HTTP JSON: %q", received)
	members = decode(received)
	_, present = members["partial_result"]
	testassert.Require(t, !present, "malformed partial was published: %s", received)
}
