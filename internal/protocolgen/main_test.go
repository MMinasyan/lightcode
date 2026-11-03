package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The fixture schemas are one valid Health/Events baseline with local
// variations. Each case below composes only the paths it needs, so every
// unsupported shape is exercised against a schema that would otherwise
// generate.
const (
	fixtureHead = `openapi: "3.0.3"
info:
  title: protocolgen-fixture
  version: "1"
paths:
`
	healthPath = `  /v1/health:
    get:
      operationId: GetHealth
      responses:
        "200":
          description: readiness
          content:
            application/json:
              schema:
                type: object
`
	eventsPath = `  /v1/events:
    get:
      operationId: GetEvents
      responses:
        "200":
          description: the event stream
          content:
            text/event-stream:
              schema:
                type: object
`
	baselineSchema = fixtureHead + healthPath + eventsPath

	// fixtureConfig is the minimal SDK configuration the fixture module
	// compiles against: the generated protocol package and its client.
	fixtureConfig = `package: protocol
generate:
  models: true
  client: true
compatibility:
  preserve-original-operation-id-casing-in-embedded-spec: true
`
)

// writeFixture writes one schema text and returns its path.
func writeFixture(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openapi.yaml")
	testassert.NoError(t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

// writeConfig writes the fixture SDK configuration and returns its path.
func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oapi-codegen.yaml")
	testassert.NoError(t, os.WriteFile(path, []byte(fixtureConfig), 0o644))
	return path
}

// generateFixture runs the generator over one schema text and returns the SDK,
// native bindings and invocation contract.
func generateFixture(t *testing.T, text string) (string, string, string) {
	t.Helper()
	sdk, source, contract, err := generateFiles(writeFixture(t, text), writeConfig(t))
	testassert.NoError(t, err)
	return string(sdk), string(source), string(contract)
}

// TestStreamCardinality proves stream operations are excluded whole without
// imposing any cardinality: zero streams and multiple streams both generate the
// remaining native operations and publish no stream entry.
func TestStreamCardinality(t *testing.T) {
	const secondEvents = `  /v1/other-events:
    get:
      operationId: OtherEvents
      responses:
        "200":
          description: a second event stream
          content:
            text/event-stream:
              schema:
                type: object
`
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"zero streams", fixtureHead + healthPath},
		{"multiple streams", baselineSchema + secondEvents},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, source, contract := generateFixture(t, tc.schema)
			testassert.Check(t, strings.Contains(source, `"GetHealth": {`), "generated bindings lost the non-stream operation:\n%s", source)
			testassert.Check(t, strings.Contains(contract, `"GetHealth": {`), "invocation contract lost the non-stream operation:\n%s", contract)
			for _, stream := range []string{"GetEvents", "OtherEvents"} {
				testassert.Check(t, !strings.Contains(source, `"`+stream+`": {`), "generated bindings carry stream operation %q:\n%s", stream, source)
				testassert.Check(t, !strings.Contains(contract, `"`+stream+`"`), "invocation contract carries stream operation %q:\n%s", stream, contract)
			}
		})
	}
}

// TestUnsupportedShapesWriteNothing proves every unsupported schema shape and
// every failed SDK generation fails the command before any of the three
// destinations is touched: all three pre-existing artifacts keep their exact
// bytes.
func TestUnsupportedShapesWriteNothing(t *testing.T) {
	base := baselineSchema
	cases := []struct {
		name   string
		schema string
	}{
		{
			name: "duplicate operationId",
			schema: base + `  /v1/other:
    get:
      operationId: GetHealth
      responses:
        "200":
          description: a duplicate id
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "missing operationId",
			schema: base + `  /v1/other:
    get:
      responses:
        "200":
          description: an anonymous operation
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "two success statuses",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: one success
          content:
            application/json:
              schema:
                type: object
        "201":
          description: another success
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a success status without content",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a contentless success
`,
		},
		{
			name: "non-JSON success content",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a text success
          content:
            text/plain:
              schema:
                type: string
`,
		},
		{
			name: "mixed JSON and stream content",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a mixed success
          content:
            application/json:
              schema:
                type: object
            text/event-stream:
              schema:
                type: object
`,
		},
		{
			name: "an unsupported query primitive",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      parameters:
        - name: limit
          in: query
          required: true
          schema:
            type: integer
      responses:
        "200":
          description: a numeric query
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "two path parameters",
			schema: base + `  /v1/other/{a}/{b}:
    get:
      operationId: Other
      parameters:
        - name: a
          in: path
          required: true
          schema:
            type: string
        - name: b
          in: path
          required: true
          schema:
            type: string
      responses:
        "200":
          description: two addresses
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "an unaddressable path parameter",
			schema: base + `  /v1/other/{name}:
    get:
      operationId: Other
      parameters:
        - name: name
          in: path
          required: true
          schema:
            type: string
      responses:
        "200":
          description: an unnamed address
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a header parameter",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      parameters:
        - name: X-Trace
          in: header
          required: false
          schema:
            type: string
      responses:
        "200":
          description: a header argument
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a cookie parameter",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      parameters:
        - name: session
          in: cookie
          required: false
          schema:
            type: string
      responses:
        "200":
          description: a cookie argument
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a request body without JSON content",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          text/plain:
            schema:
              type: string
      responses:
        "200":
          description: a text body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "an optional request body",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: false
        content:
          application/json:
            schema:
              type: object
      responses:
        "200":
          description: an optional body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a 204 success with JSON content",
			schema: base + `  /v1/other:
    delete:
      operationId: Other
      responses:
        "204":
          description: a bodyless success carrying JSON
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a 204 success with SSE content",
			schema: fixtureHead + healthPath + `  /v1/other:
    delete:
      operationId: Other
      responses:
        "204":
          description: a bodyless success carrying a stream
          content:
            text/event-stream:
              schema:
                type: object
`,
		},
		{
			name: "a patterned 2XX success alone",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "2XX":
          description: a patterned success
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a patterned 2XX success beside 200",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a numeric success
          content:
            application/json:
              schema:
                type: object
        "2XX":
          description: a patterned success
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "JSON and plain success media",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a two-media success
          content:
            application/json:
              schema:
                type: object
            text/plain:
              schema:
                type: string
`,
		},
		{
			name: "SSE and plain success media",
			schema: fixtureHead + healthPath + `  /v1/stream:
    get:
      operationId: Streamed
      responses:
        "200":
          description: a two-media stream
          content:
            text/event-stream:
              schema:
                type: object
            text/plain:
              schema:
                type: string
`,
		},
		{
			name: "a referenced integer field path",
			schema: base + `  /v1/fields/{field}:
    delete:
      operationId: ResetField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/FieldCount"
      responses:
        "200":
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
components:
  schemas:
    FieldCount:
      type: integer
`,
		},
		{
			name: "a referenced external field path",
			schema: base + `  /v1/fields/{field}:
    delete:
      operationId: ResetField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "https://example.invalid/field.yaml#/FieldName"
      responses:
        "200":
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a formatted path type outside the admitted set",
			schema: base + `  /v1/sessions/{id}/extra:
    get:
      operationId: Extra
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            x-go-type: "[]string"
      responses:
        "200":
          description: an unsupported formatted path type
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "an array request body",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: array
              items:
                type: string
      responses:
        "200":
          description: an array body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a string request body",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: string
      responses:
        "200":
          description: a string body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "an untyped request body",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          application/json:
            schema:
              properties:
                name:
                  type: string
      responses:
        "200":
          description: an untyped body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a two-media request body",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
          text/plain:
            schema:
              type: string
      responses:
        "200":
          description: a two-media body
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a failed SDK generation",
			schema: base + `  /v1/other:
    post:
      operationId: Other
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                'bad"name':
                  type: string
      responses:
        "200":
          description: a body the SDK cannot emit
          content:
            application/json:
              schema:
                type: object
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			schemaPath := filepath.Join(dir, "openapi.yaml")
			testassert.NoError(t, os.WriteFile(schemaPath, []byte(tc.schema), 0o644))
			configPath := filepath.Join(dir, "oapi-codegen.yaml")
			testassert.NoError(t, os.WriteFile(configPath, []byte(fixtureConfig), 0o644))
			sdkPath := filepath.Join(dir, "protocol.gen.go")
			outPath := filepath.Join(dir, "operations.gen.go")
			contractPath := filepath.Join(dir, "native-invocations.json")
			sentinels := map[string]string{
				sdkPath:      "untouched sdk\n",
				outPath:      "untouched native\n",
				contractPath: "untouched contract\n",
			}
			for path, sentinel := range sentinels {
				testassert.NoError(t, os.WriteFile(path, []byte(sentinel), 0o644))
			}
			if err := run(schemaPath, configPath, sdkPath, outPath, contractPath); err == nil {
				t.Fatal("the generator accepted an unsupported schema shape")
			} else {
				t.Logf("rejected: %v", err)
			}
			for path, sentinel := range sentinels {
				content, err := os.ReadFile(path)
				testassert.Require(t, err == nil, "reading %s: %v", path, err)
				if !bytes.Equal(content, []byte(sentinel)) {
					t.Fatalf("unsupported input rewrote %s: %q", filepath.Base(path), content)
				}
			}
		})
	}
}

// fixtureDispatchSchema is the one compiled owner of every accepted native
// shape: the committed ProviderField/ModelField enum references, an enum-less
// referenced string whose component name the SDK normalizes, a named date
// schema the SDK aliases to a non-string Go representation, inline and
// component-parameter session paths, an operation ID the SDK normalizes,
// path-level query parameters overridden at the operation level in both
// directions, mixed required/optional queries, one required JSON object body,
// and the unlisted 201/203/210/204 success statuses.
const fixtureDispatchSchema = `openapi: "3.0.3"
info:
  title: protocolgen-dispatch-fixture
  version: "1"
paths:
  /v1/health:
    get:
      operationId: GetHealth
      responses:
        "200":
          description: readiness
          content:
            application/json:
              schema:
                type: object
  /v1/events:
    get:
      operationId: GetEvents
      responses:
        "200":
          description: the event stream
          content:
            text/event-stream:
              schema:
                type: object
  /v1/providers/fields/{field}:
    delete:
      operationId: ResetProviderField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/ProviderField"
        - name: provider_id
          in: query
          required: true
          schema:
            type: string
      responses:
        "200":
          description: provider field reset
          content:
            application/json:
              schema:
                type: object
  /v1/providers/models/fields/{field}:
    delete:
      operationId: ResetProviderModelField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/ModelField"
        - name: provider_id
          in: query
          required: true
          schema:
            type: string
        - name: model_id
          in: query
          required: true
          schema:
            type: string
      responses:
        "200":
          description: model field reset
          content:
            application/json:
              schema:
                type: object
  /v1/plain/fields/{field}:
    delete:
      operationId: ResetPlainField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/plain_field"
      responses:
        "200":
          description: plain field reset
          content:
            application/json:
              schema:
                type: object
  /v1/sessions/{id}/plain:
    post:
      operationId: PingSession
      parameters:
        - $ref: "#/components/parameters/SessionID"
      responses:
        "200":
          description: plain session path
          content:
            application/json:
              schema:
                type: object
  /v1/sessions/{id}/extra:
    post:
      operationId: Extra
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
      responses:
        "201":
          description: the added operation
          content:
            application/json:
              schema:
                type: object
  /v1/normalized:
    get:
      operationId: get_extra
      parameters:
        - name: cursor
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: normalized operation ID
          content:
            application/json:
              schema:
                type: object
  /v1/mixed:
    get:
      operationId: MixedQuery
      parameters:
        - name: all
          in: query
          required: true
          schema:
            type: boolean
        - name: cursor
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: a mixed query
          content:
            application/json:
              schema:
                type: object
  /v1/overrides:
    parameters:
      - name: flag
        in: query
        required: false
        schema:
          type: boolean
    get:
      operationId: QueryOverride
      parameters:
        - name: flag
          in: query
          required: true
          schema:
            type: boolean
      responses:
        "200":
          description: required override
          content:
            application/json:
              schema:
                type: object
  /v1/optional-override:
    parameters:
      - name: limit
        in: query
        required: true
        schema:
          type: string
    get:
      operationId: OptionalOverride
      parameters:
        - name: limit
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: optional override
          content:
            application/json:
              schema:
                type: object
  /v1/accepted:
    post:
      operationId: Accepted203
      responses:
        "203":
          description: an unlisted numeric success
          content:
            application/json:
              schema:
                type: object
  /v1/extended:
    get:
      operationId: Extended210
      responses:
        "210":
          description: another unlisted numeric success
          content:
            application/json:
              schema:
                type: object
  /v1/removed:
    delete:
      operationId: Removed204
      responses:
        "204":
          description: a contentless success
  /v1/dated/{day}:
    get:
      operationId: GetDated
      parameters:
        - name: day
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/Day"
      responses:
        "200":
          description: a date path
          content:
            application/json:
              schema:
                type: object
  /v1/sessions:
    post:
      operationId: CreateSession
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
      responses:
        "200":
          description: raw body
          content:
            application/json:
              schema:
                type: object
components:
  parameters:
    SessionID:
      name: id
      in: path
      required: true
      schema:
        type: string
  schemas:
    ProviderField:
      type: string
      enum:
        - name
        - hidden
    ModelField:
      type: string
      enum:
        - name
        - context_window
    plain_field:
      type: string
    Day:
      type: string
      format: date
`

// fixtureDispatchTest is the generated module's own test. It drives the real
// generated SDK through the real stateless binder against an httptest route and
// compares every request's path, query, raw body, declared status and received
// response bytes with independently authored expectations. The date row proves
// the SDK's non-string path type stays compiled and parsed, and a direct string
// cast producer would fail this module's compilation.
const fixtureDispatchTest = `package clientops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/protocol"
)

type recordedCall struct {
	method string
	path   string
	query  string
	body   string
}

func TestGeneratedFixtureDispatch(t *testing.T) {
	var mu sync.Mutex
	var recorded []recordedCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		mu.Lock()
		recorded = append(recorded, recordedCall{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, body: string(body)})
		mu.Unlock()
		status := http.StatusOK
		switch r.URL.Path {
		case "/v1/sessions/s1/extra":
			status = http.StatusCreated
		case "/v1/accepted":
			status = 203
		case "/v1/extended":
			status = 210
		case "/v1/removed":
			status = http.StatusNoContent
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusNoContent {
			return
		}
		_, _ = w.Write([]byte("{\"ok\":true}"))
	}))
	defer server.Close()
	client, err := protocol.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, tc := range []struct {
		name      string
		operation string
		params    string
		method    string
		path      string
		query     string
		body      string
		status    int
		response  string
	}{
		{"health without an envelope", "GetHealth", "", "GET", "/v1/health", "", "", 200, "{\"ok\":true}"},
		{"enum-backed referenced field", "ResetProviderField", "{\"field\":\"hidden\",\"query\":{\"provider_id\":\"prov\"}}", "DELETE", "/v1/providers/fields/hidden", "provider_id=prov", "", 200, "{\"ok\":true}"},
		{"model enum-backed referenced field", "ResetProviderModelField", "{\"field\":\"context_window\",\"query\":{\"provider_id\":\"prov\",\"model_id\":\"m\"}}", "DELETE", "/v1/providers/models/fields/context_window", "provider_id=prov&model_id=m", "", 200, "{\"ok\":true}"},
		{"enum-less normalized referenced field", "ResetPlainField", "{\"field\":\"anything\"}", "DELETE", "/v1/plain/fields/anything", "", "", 200, "{\"ok\":true}"},
		{"component-parameter session path", "PingSession", "{\"session_id\":\"s1\"}", "POST", "/v1/sessions/s1/plain", "", "", 200, "{\"ok\":true}"},
		{"inline string session path with a body", "Extra", "{\"session_id\":\"s1\",\"body\":{\"workspace\":\"/ws\",\"opaque\":9007199254740993}}", "POST", "/v1/sessions/s1/extra", "", "{\"workspace\":\"/ws\",\"opaque\":9007199254740993}", 201, "{\"ok\":true}"},
		{"normalized operation ID", "get_extra", "{\"query\":{\"cursor\":\"abc\"}}", "GET", "/v1/normalized", "cursor=abc", "", 200, "{\"ok\":true}"},
		{"mixed required and optional query", "MixedQuery", "{\"query\":{\"all\":true,\"cursor\":\"abc\"}}", "GET", "/v1/mixed", "all=true&cursor=abc", "", 200, "{\"ok\":true}"},
		{"operation-level required override", "QueryOverride", "{\"query\":{\"flag\":true}}", "GET", "/v1/overrides", "flag=true", "", 200, "{\"ok\":true}"},
		{"operation-level optional override", "OptionalOverride", "{\"query\":{\"limit\":\"x\"}}", "GET", "/v1/optional-override", "limit=x", "", 200, "{\"ok\":true}"},
		{"optional override without a query member", "OptionalOverride", "", "GET", "/v1/optional-override", "", "", 200, "{\"ok\":true}"},
		{"unlisted 203 success", "Accepted203", "", "POST", "/v1/accepted", "", "", 203, "{\"ok\":true}"},
		{"unlisted 210 success", "Extended210", "", "GET", "/v1/extended", "", "", 210, "{\"ok\":true}"},
		{"contentless 204 success", "Removed204", "", "DELETE", "/v1/removed", "", "", 204, ""},
		{"named date path", "GetDated", "{\"field\":\"2024-01-02\"}", "GET", "/v1/dated/2024-01-02", "", "", 200, "{\"ok\":true}"},
		{"raw JSON body", "CreateSession", "{\"body\":{\"workspace\":\"/ws\",\"opaque\":9007199254740993}}", "POST", "/v1/sessions", "", "{\"workspace\":\"/ws\",\"opaque\":9007199254740993}", 200, "{\"ok\":true}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			before := len(recorded)
			mu.Unlock()
			response, status, err := invokeGenerated(context.Background(), client, tc.operation, json.RawMessage(tc.params))
			if err != nil {
				t.Fatalf("invokeGenerated(%s): %v", tc.operation, err)
			}
			if status != tc.status || response.StatusCode != tc.status {
				t.Fatalf("%s status = declared %d received %d, want %d", tc.operation, status, response.StatusCode, tc.status)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != tc.response {
				t.Fatalf("%s result = %q, %v, want %q", tc.operation, body, err, tc.response)
			}
			mu.Lock()
			call := recorded[before]
			mu.Unlock()
			if call.method != tc.method || call.path != tc.path || call.query != tc.query || call.body != tc.body {
				t.Fatalf("%s request = (%s %s ? %s body %q), want (%s %s ? %s body %q)", tc.operation, call.method, call.path, call.query, call.body, tc.method, tc.path, tc.query, tc.body)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		operation string
		params    string
	}{
		{"omitted required query override", "QueryOverride", ""},
		{"mixed query missing its required member", "MixedQuery", "{\"query\":{\"cursor\":\"abc\"}}"},
		{"invalid date path text", "GetDated", "{\"field\":\"not-a-date\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			before := len(recorded)
			mu.Unlock()
			_, _, err := invokeGenerated(context.Background(), client, tc.operation, json.RawMessage(tc.params))
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("%s error = %v, want ErrInvalidParams", tc.operation, err)
			}
			mu.Lock()
			reached := len(recorded) != before
			mu.Unlock()
			if reached {
				t.Fatalf("%s reached the transport after local rejection", tc.operation)
			}
		})
	}
	names := OperationNames()
	want := []string{"Accepted203", "CreateSession", "Extended210", "Extra", "GetDated", "GetHealth", "MixedQuery", "OptionalOverride", "PingSession", "QueryOverride", "Removed204", "ResetPlainField", "ResetProviderField", "ResetProviderModelField", "get_extra"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("OperationNames() = %v, want %v", names, want)
	}
}
`

// TestGeneratedFixtureCompilesAndDispatches generates the fixture SDK and its
// native bindings into a disposable module with the product module path, copies
// the real stateless binder, compiles them, and drives the actual generated
// client through the binder against httptest routes. The fixture is the one
// compiled owner of every accepted native shape; the rows carry independently
// expected paths, queries, raw bodies, declared statuses, response bytes and
// contract metadata.
func TestGeneratedFixtureCompilesAndDispatches(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "openapi.yaml")
	testassert.NoError(t, os.WriteFile(schemaPath, []byte(fixtureDispatchSchema), 0o644))
	configPath := filepath.Join(dir, "oapi-codegen.yaml")
	testassert.NoError(t, os.WriteFile(configPath, []byte(fixtureConfig), 0o644))
	sdkPath := filepath.Join(dir, "protocol", "protocol.gen.go")
	nativePath := filepath.Join(dir, "internal", "clientops", "operations.gen.go")
	contractPath := filepath.Join(dir, "native-invocations.json")
	testassert.NoError(t, os.MkdirAll(filepath.Dir(sdkPath), 0o755))
	testassert.NoError(t, os.MkdirAll(filepath.Dir(nativePath), 0o755))
	if err := run(schemaPath, configPath, sdkPath, nativePath, contractPath); err != nil {
		t.Fatalf("generating the fixture module: %v", err)
	}
	binder, err := os.ReadFile(filepath.Join("..", "clientops", "bind.go"))
	testassert.NoError(t, err)
	testassert.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(nativePath), "bind.go"), binder, 0o644))
	testassert.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(nativePath), "bind_test.go"), []byte(fixtureDispatchTest), 0o644))
	contractData, err := os.ReadFile(contractPath)
	testassert.NoError(t, err)
	var contract map[string]contractOperation
	testassert.NoError(t, json.Unmarshal(contractData, &contract))
	wantContract := map[string]contractOperation{
		"Accepted203":             {SessionID: modeForbidden, Field: modeForbidden, Query: modeForbidden, Body: modeForbidden},
		"CreateSession":           {SessionID: modeForbidden, Field: modeForbidden, Query: modeForbidden, Body: modeRequired},
		"Extended210":             {SessionID: modeForbidden, Field: modeForbidden, Query: modeForbidden, Body: modeForbidden},
		"Extra":                   {SessionID: modeRequired, Field: modeForbidden, Query: modeForbidden, Body: modeRequired},
		"GetDated":                {SessionID: modeForbidden, Field: modeRequired, Query: modeForbidden, Body: modeForbidden},
		"GetHealth":               {SessionID: modeForbidden, Field: modeForbidden, Query: modeForbidden, Body: modeForbidden},
		"MixedQuery":              {SessionID: modeForbidden, Field: modeForbidden, Query: modeRequired, Body: modeForbidden, QueryMembers: map[string]contractMember{"all": {Required: true, Type: "boolean"}, "cursor": {Required: false, Type: "string"}}},
		"OptionalOverride":        {SessionID: modeForbidden, Field: modeForbidden, Query: modeOptional, Body: modeForbidden, QueryMembers: map[string]contractMember{"limit": {Required: false, Type: "string"}}},
		"PingSession":             {SessionID: modeRequired, Field: modeForbidden, Query: modeForbidden, Body: modeForbidden},
		"QueryOverride":           {SessionID: modeForbidden, Field: modeForbidden, Query: modeRequired, Body: modeForbidden, QueryMembers: map[string]contractMember{"flag": {Required: true, Type: "boolean"}}},
		"Removed204":              {SessionID: modeForbidden, Field: modeForbidden, Query: modeForbidden, Body: modeForbidden},
		"ResetPlainField":         {SessionID: modeForbidden, Field: modeRequired, Query: modeForbidden, Body: modeForbidden},
		"ResetProviderField":      {SessionID: modeForbidden, Field: modeRequired, Query: modeRequired, Body: modeForbidden, QueryMembers: map[string]contractMember{"provider_id": {Required: true, Type: "string"}}},
		"ResetProviderModelField": {SessionID: modeForbidden, Field: modeRequired, Query: modeRequired, Body: modeForbidden, QueryMembers: map[string]contractMember{"provider_id": {Required: true, Type: "string"}, "model_id": {Required: true, Type: "string"}}},
		"get_extra":               {SessionID: modeForbidden, Field: modeForbidden, Query: modeOptional, Body: modeForbidden, QueryMembers: map[string]contractMember{"cursor": {Required: false, Type: "string"}}},
	}
	testassert.Require(t, len(contract) == len(wantContract), "fixture contract names %d operations, want %d:\n%s", len(contract), len(wantContract), contractData)
	for name, want := range wantContract {
		got, ok := contract[name]
		testassert.Require(t, ok, "fixture contract is missing %q", name)
		testassert.Require(t, got.SessionID == want.SessionID && got.Field == want.Field && got.Query == want.Query && got.Body == want.Body, "fixture contract %q = %+v, want the declared modes %+v", name, got, want)
		if want.QueryMembers == nil {
			testassert.Require(t, len(got.QueryMembers) == 0, "fixture contract %q carries query members %+v, want none", name, got.QueryMembers)
		} else {
			testassert.Require(t, reflect.DeepEqual(got.QueryMembers, want.QueryMembers), "fixture contract %q query members = %+v, want %+v", name, got.QueryMembers, want.QueryMembers)
		}
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(`module github.com/MMinasyan/lightcode

go 1.26.0

require github.com/oapi-codegen/runtime v1.7.0

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
)
`), 0o644))
	sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum"))
	testassert.NoError(t, err)
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644))
	cmd := exec.Command("go", "test", "-mod=mod", "./internal/clientops")
	cmd.Dir = dir
	{
		out, err := cmd.CombinedOutput()
		testassert.Require(t, err == nil, "compiling/dispatching the generated fixture: %v\n%s", err, out)
	}
}
