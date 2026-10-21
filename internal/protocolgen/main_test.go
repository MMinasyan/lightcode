package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The fixture schema is a minimal valid protocol: one JSON operation, one
// optional-query operation and the one declared event stream. Each case below
// composes the paths it needs, so a supported addition and every unsupported
// shape are exercised against a schema that would otherwise generate.
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
          description: the one event stream
          content:
            text/event-stream:
              schema:
                type: object
`
)

// writeFixture writes one schema text and returns its path.
func writeFixture(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openapi.yaml")
	testassert.NoError(t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

// generateFixture runs the generator over one schema text and returns both
// artifacts.
func generateFixture(t *testing.T, text string) (string, string) {
	t.Helper()
	source, contract, err := generateFiles(writeFixture(t, text))
	testassert.NoError(t, err)
	return string(source), string(contract)
}

// TestSupportedAdditionGenerates proves a supported schema addition generates
// into both artifacts deterministically: the added operation appears in the
// sorted names, in its dispatch arm and in the invocation contract, and two
// runs over the same schema are byte-identical.
func TestSupportedAdditionGenerates(t *testing.T) {
	const addition = `  /v1/sessions/{id}/extra:
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
`
	schema := fixtureHead + healthPath + eventsPath + addition
	source, contract := generateFixture(t, schema)
	testassert.Check(t, strings.Contains(source, `"Extra",`), "generated bindings do not advertise the added operation:\n%s", source)
	testassert.Check(t, strings.Contains(source, `case "Extra":`), "generated bindings have no dispatch arm for the added operation")
	testassert.Check(t, strings.Contains(source, "c.ExtraWithBody(ctx, protocol.SessionID(env.session), contentTypeJSON, bytes.NewReader(env.body))"), "generated arm does not call the added operation's generated method")
	testassert.Check(t, strings.Contains(source, "return response, 201, callErr"), "generated arm does not carry the added operation's declared success status")
	testassert.Check(t, strings.Contains(contract, `"Extra": {`), "invocation contract has no entry for the added operation:\n%s", contract)
	testassert.Check(t, strings.Contains(contract, `"body": "required"`) && strings.Contains(contract, `"session_id": "required"`), "invocation contract does not carry the added operation's declared modes")
	testassert.Check(t, !strings.Contains(contract, "GetEvents"), "invocation contract carries the event stream:\n%s", contract)
	againSource, againContract := generateFixture(t, schema)
	testassert.Check(t, source == againSource && contract == againContract, "two runs over the same schema produced different artifacts")
}

// TestOptionalQueryMembersGenerate proves the optional query envelope modes
// derive from the schema: a query mixing required and optional members binds
// every member and requires the required ones, and an optional-only query
// binds only when present through the nil-required guard. The contract
// publishes both forms and neither operation declares a request body.
func TestOptionalQueryMembersGenerate(t *testing.T) {
	const mixedQuery = `  /v1/optional:
    post:
      operationId: Optional
      parameters:
        - name: cursor
          in: query
          required: false
          schema:
            type: string
        - name: all
          in: query
          required: true
          schema:
            type: boolean
      responses:
        "200":
          description: the mixed query operation
          content:
            application/json:
              schema:
                type: object
`
	const optionalQuery = `  /v1/sessions/{id}/history:
    get:
      operationId: GetSessionHistory
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
        - name: cursor
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: the optional query operation
          content:
            application/json:
              schema:
                type: object
`
	for _, tc := range []struct {
		name     string
		path     string
		source   []string
		contract []string
	}{
		{
			name: "required and optional query members without a body",
			path: mixedQuery,
			source: []string{
				`bindEnvelope(params, memberForbidden, memberForbidden, memberRequired, memberForbidden)`,
				`bindQuery(env.query, []string{"all"}, &query)`,
				`c.Optional(ctx, &query)`,
			},
			contract: []string{
				`"Optional": {
    "session_id": "forbidden",
    "field": "forbidden",
    "query": "required",
    "body": "forbidden"`,
				`"cursor": {
        "required": false,
        "type": "string"
      }`,
				`"all": {
        "required": true,
        "type": "boolean"
      }`,
			},
		},
		{
			name: "an optional-only query binds only when present",
			path: optionalQuery,
			source: []string{
				`bindEnvelope(params, memberRequired, memberForbidden, memberOptional, memberForbidden)`,
				`if env.query != nil {`,
				`bindQuery(env.query, nil, &query)`,
				`c.GetSessionHistory(ctx, protocol.SessionID(env.session), &query)`,
			},
			contract: []string{
				`"GetSessionHistory": {
    "session_id": "required",
    "field": "forbidden",
    "query": "optional",
    "body": "forbidden"`,
				`"cursor": {
        "required": false,
        "type": "string"
      }`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, contract := generateFixture(t, fixtureHead+healthPath+eventsPath+tc.path)
			for _, want := range tc.source {
				testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
			}
			for _, want := range tc.contract {
				testassert.Check(t, strings.Contains(contract, want), "invocation contract is missing %q:\n%s", want, contract)
			}
		})
	}
}

// TestSupportedShapesGenerate pins the accepted shapes the extraction pass
// must keep: a referenced string-enum field path emits its generated protocol
// type and call with the declared metadata, and a single numeric 2xx —
// including one with no named standard-library constant — is emitted as the
// schema's own number, beside the unchanged contentless 204 arm.
func TestSupportedShapesGenerate(t *testing.T) {
	const fieldsPath = `  /v1/fields/{field}:
    delete:
      operationId: ResetField
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
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
`
	const acceptedPath = `  /v1/accepted:
    post:
      operationId: Accepted203
      responses:
        "203":
          description: an unlisted numeric success
          content:
            application/json:
              schema:
                type: object
`
	const extendedPath = `  /v1/extended:
    get:
      operationId: Extended210
      responses:
        "210":
          description: another unlisted numeric success
          content:
            application/json:
              schema:
                type: object
`
	const removedPath = `  /v1/removed:
    delete:
      operationId: Removed204
      responses:
        "204":
          description: a contentless success
`
	const components = `components:
  schemas:
    ProviderField:
      type: string
      enum:
        - name
        - hidden
`
	source, contract := generateFixture(t, fixtureHead+healthPath+eventsPath+fieldsPath+acceptedPath+extendedPath+removedPath+components)
	for _, want := range []string{
		`case "ResetField":`,
		`bindEnvelope(params, memberForbidden, memberRequired, memberRequired, memberForbidden)`,
		`bindQuery(env.query, []string{"provider_id"}, &query)`,
		`c.ResetField(ctx, protocol.ProviderField(env.field), &query)`,
		`return response, 203, callErr`,
		`return response, 210, callErr`,
		`return response, 204, callErr`,
		`"Accepted203",`,
		`"Extended210",`,
		`"Removed204",`,
		`"ResetField",`,
	} {
		testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
	}
	testassert.Check(t, !strings.Contains(source, "http.Status"), "generated bindings carry a named status constant instead of the schema's number:\n%s", source)
	for _, want := range []string{
		`"ResetField": {`,
		`"field": "required"`,
		`"Accepted203": {`,
		`"Extended210": {`,
		`"Removed204": {`,
		`"provider_id": {
        "required": true,
        "type": "string"
      }`,
	} {
		testassert.Check(t, strings.Contains(contract, want), "invocation contract is missing %q:\n%s", want, contract)
	}
}

// TestUnsupportedShapesWriteNothing proves every unsupported schema shape
// fails the command before either destination is touched: both pre-existing
// artifacts keep their exact bytes.
func TestUnsupportedShapesWriteNothing(t *testing.T) {
	base := fixtureHead + healthPath + eventsPath
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
			name:   "no event stream",
			schema: fixtureHead + healthPath,
		},
		{
			name: "a second event stream",
			schema: base + `  /v1/other:
    get:
      operationId: Other
      responses:
        "200":
          description: a second stream
          content:
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
			name: "a referenced non-enum string field path",
			schema: base + `  /v1/fields/{field}:
    delete:
      operationId: ResetField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/FieldName"
      responses:
        "200":
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
components:
  schemas:
    FieldName:
      type: string
`,
		},
		{
			name: "a referenced mixed-enum field path",
			schema: base + `  /v1/fields/{field}:
    delete:
      operationId: ResetField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/FieldName"
      responses:
        "200":
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
components:
  schemas:
    FieldName:
      type: string
      enum:
        - name
        - 1
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
			name: "a noncanonical lowercase operation name",
			schema: base + `  /v1/other:
    get:
      operationId: get_extra
      responses:
        "200":
          description: a normalized name
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a noncanonical separated operation name",
			schema: base + `  /v1/other:
    get:
      operationId: Get_extra
      responses:
        "200":
          description: a normalized name
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a leading-digit operation name",
			schema: base + `  /v1/other:
    get:
      operationId: 9Lives
      responses:
        "200":
          description: a normalized name
          content:
            application/json:
              schema:
                type: object
`,
		},
		{
			name: "a noncanonical referenced type name",
			schema: base + `  /v1/fields/{field}:
    delete:
      operationId: ResetField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/provider_field"
      responses:
        "200":
          description: the reset post-state
          content:
            application/json:
              schema:
                type: object
components:
  schemas:
    provider_field:
      type: string
      enum:
        - name
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			schemaPath := filepath.Join(dir, "openapi.yaml")
			testassert.NoError(t, os.WriteFile(schemaPath, []byte(tc.schema), 0o644))
			outPath := filepath.Join(dir, "operations.gen.go")
			contractPath := filepath.Join(dir, "native-invocations.json")
			const sentinel = "untouched\n"
			for _, path := range []string{outPath, contractPath} {
				testassert.NoError(t, os.WriteFile(path, []byte(sentinel), 0o644))
			}
			if err := run(schemaPath, outPath, contractPath); err == nil {
				t.Fatal("the generator accepted an unsupported schema shape")
			} else {
				t.Logf("rejected: %v", err)
			}
			for _, path := range []string{outPath, contractPath} {
				content, err := os.ReadFile(path)
				testassert.Require(t, err == nil, "reading %s: %v", path, err)
				if !bytes.Equal(content, []byte(sentinel)) {
					t.Fatalf("unsupported input rewrote %s: %q", filepath.Base(path), content)
				}
			}
		})
	}
}
