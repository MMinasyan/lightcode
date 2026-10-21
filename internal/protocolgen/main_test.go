package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The fixture schemas are one valid Health/Events baseline with local
// variations. Each case below composes only the paths it needs, so a supported
// addition and every unsupported shape are exercised against a schema that
// would otherwise generate.
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

// TestSupportedAdditionGenerates proves a supported schema addition generates
// into the SDK and both native artifacts deterministically: the added
// operation appears in the SDK client, the descriptor table and the invocation
// contract, and two runs over the same schema are byte-identical.
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
	schema := baselineSchema + addition
	sdk, source, contract := generateFixture(t, schema)
	testassert.Check(t, strings.Contains(sdk, "func (c *Client) ExtraWithBody("), "generated SDK has no added operation method:\n%s", sdk)
	testassert.Check(t, strings.Contains(source, `"Extra": {`), "generated bindings have no descriptor for the added operation:\n%s", source)
	testassert.Check(t, strings.Contains(source, "session: memberRequired,"), "generated descriptor does not declare the added path member")
	testassert.Check(t, strings.Contains(source, "body:    memberRequired,"), "generated descriptor does not declare the added body member")
	testassert.Check(t, strings.Contains(source, "status:  201,"), "generated descriptor does not carry the added operation's declared success status")
	testassert.Check(t, strings.Contains(source, "var pathValue string"), "generated descriptor does not bind the plain-string path")
	testassert.Check(t, strings.Contains(source, "c.ExtraWithBody(ctx, pathValue, contentTypeJSON, bytes.NewReader(env.body))"), "generated descriptor does not call the added operation's generated method")
	testassert.Check(t, strings.Contains(contract, `"Extra": {`), "invocation contract has no entry for the added operation:\n%s", contract)
	testassert.Check(t, strings.Contains(contract, `"body": "required"`) && strings.Contains(contract, `"session_id": "required"`), "invocation contract does not carry the added operation's declared modes")
	testassert.Check(t, !strings.Contains(contract, "GetEvents"), "invocation contract carries the event stream:\n%s", contract)
	againSDK, againSource, againContract := generateFixture(t, schema)
	testassert.Check(t, sdk == againSDK && source == againSource && contract == againContract, "two runs over the same schema produced different artifacts")
}

// TestOptionalQueryMembersGenerate proves the optional query envelope modes
// derive from the SDK's combined parameter records: a query mixing required and
// optional members binds every member and requires the required ones, and an
// optional-only query binds only when present through the nil-required guard.
// The contract publishes both forms and neither operation declares a request
// body.
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
				`"Optional": {`,
				`query:  memberRequired,`,
				`var query protocol.OptionalParams`,
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
				`"GetSessionHistory": {`,
				`session: memberRequired,`,
				`query:   memberOptional,`,
				`var pathValue string`,
				`if env.query != nil {`,
				`bindQuery(env.query, nil, &query)`,
				`c.GetSessionHistory(ctx, pathValue, &query)`,
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
			_, source, contract := generateFixture(t, baselineSchema+tc.path)
			for _, want := range tc.source {
				testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
			}
			for _, want := range tc.contract {
				testassert.Check(t, strings.Contains(contract, want), "invocation contract is missing %q:\n%s", want, contract)
			}
		})
	}
}

// TestSupportedShapesGenerate pins the accepted shapes the projection must
// keep: a referenced string-enum field path emits its SDK-declared protocol
// type, and a single numeric 2xx — including one with no named standard-library
// constant — is emitted as the schema's own number, beside the unchanged
// contentless 204 descriptor.
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
	_, source, contract := generateFixture(t, baselineSchema+fieldsPath+acceptedPath+extendedPath+removedPath+components)
	for _, want := range []string{
		`"ResetField": {`,
		`field:  memberRequired,`,
		`query:  memberRequired,`,
		`var pathValue protocol.ProviderField`,
		`bindPath(env.field, &pathValue)`,
		`bindQuery(env.query, []string{"provider_id"}, &query)`,
		`c.ResetField(ctx, pathValue, &query)`,
		`status: 203,`,
		`status: 210,`,
		`status: 204,`,
		`"Accepted203": {`,
		`"Extended210": {`,
		`"Removed204": {`,
		`"ResetField": {`,
	} {
		testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
	}
	testassert.Check(t, !strings.Contains(source, "http.Status"), "generated bindings carry a named status constant instead of the schema's number:\n%s", source)
	testassert.Check(t, !strings.Contains(source, "protocol.ProviderField(env.field)"), "generated bindings still cast the field instead of binding its SDK-declared type:\n%s", source)
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

// TestReferencedStringsAndNormalizedNames proves the projection uses the SDK's
// resolved type and name records: referenced strings bind their declared type
// with or without an enum, a normalized component name binds the SDK-generated
// type, a plain inline string binds the builtin type, and a normalized
// operation ID keeps its original native name while the SDK call uses the
// normalized generated method.
func TestReferencedStringsAndNormalizedNames(t *testing.T) {
	const paths = `  /v1/providers/fields/{field}:
    delete:
      operationId: ResetProviderField
      parameters:
        - name: field
          in: path
          required: true
          schema:
            $ref: "#/components/schemas/ProviderField"
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
        - name: id
          in: path
          required: true
          schema:
            type: string
      responses:
        "200":
          description: plain session path
          content:
            application/json:
              schema:
                type: object
  /v1/normalized:
    get:
      operationId: get_extra
      responses:
        "200":
          description: normalized operation ID
          content:
            application/json:
              schema:
                type: object
`
	const components = `components:
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
`
	sdk, source, contract := generateFixture(t, baselineSchema+paths+components)
	for _, want := range []string{
		`"ResetProviderField": {`,
		`var pathValue protocol.ProviderField`,
		`bindPath(env.field, &pathValue)`,
		`c.ResetProviderField(ctx, pathValue)`,
		`"ResetProviderModelField": {`,
		`var pathValue protocol.ModelField`,
		`c.ResetProviderModelField(ctx, pathValue)`,
		`"ResetPlainField": {`,
		`var pathValue protocol.PlainField`,
		`c.ResetPlainField(ctx, pathValue)`,
		`"PingSession": {`,
		`var pathValue string`,
		`c.PingSession(ctx, pathValue)`,
		`"get_extra": {`,
		`c.GetExtra(ctx)`,
	} {
		testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
	}
	testassert.Check(t, strings.Contains(sdk, "func (c *Client) GetExtra("), "generated SDK has no normalized method for the source operation ID:\n%s", sdk)
	testassert.Check(t, strings.Contains(sdk, "type PlainField = string"), "generated SDK did not normalize the referenced schema name:\n%s", sdk)
	for _, want := range []string{`"get_extra": {`, `"ResetPlainField": {`, `"PingSession": {`} {
		testassert.Check(t, strings.Contains(contract, want), "invocation contract is missing %q:\n%s", want, contract)
	}
	testassert.Check(t, !strings.Contains(contract, `"GetExtra"`), "invocation contract uses the normalized SDK name instead of the source operation ID:\n%s", contract)
}

// TestLocalParameterOverrides proves operation-level parameters override their
// path-level declarations with local-wins requiredness, and that both the
// native contract and the generated SDK Params type carry the resulting
// requiredness.
func TestLocalParameterOverrides(t *testing.T) {
	const paths = `  /v1/overrides:
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
`
	sdk, source, contract := generateFixture(t, baselineSchema+paths)
	testassert.Check(t, strings.Contains(sdk, "type QueryOverrideParams struct"), "generated SDK has no Params type for the overridden query:\n%s", sdk)
	for _, want := range []string{
		`"QueryOverride": {`,
		`query:  memberRequired,`,
		`bindQuery(env.query, []string{"flag"}, &query)`,
		`"OptionalOverride": {`,
		`query:  memberOptional,`,
		`bindQuery(env.query, nil, &query)`,
	} {
		testassert.Check(t, strings.Contains(source, want), "generated bindings are missing %q:\n%s", want, source)
	}
	for _, want := range []string{
		`"flag": {
        "required": true,
        "type": "boolean"
      }`,
		`"limit": {
        "required": false,
        "type": "string"
      }`,
	} {
		testassert.Check(t, strings.Contains(contract, want), "invocation contract is missing %q:\n%s", want, contract)
	}
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

// fixtureDispatchSchema exercises the accepted native shapes together: the
// committed ProviderField/ModelField enum references, an enum-less referenced
// string whose component name the SDK normalizes, the plain inline string path,
// an operation ID the SDK normalizes, path-level query parameters overridden at
// the operation level in both directions, and one required JSON object body.
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
`

// fixtureDispatchTest is the generated module's own test. It drives the real
// generated SDK through the real stateless binder against an httptest route and
// compares every request's path, query and raw body with independently
// authored expectations.
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
		w.Header().Set("Content-Type", "application/json")
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
	}{
		{"enum-backed referenced field", "ResetProviderField", "{\"field\":\"hidden\",\"query\":{\"provider_id\":\"prov\"}}", "DELETE", "/v1/providers/fields/hidden", "provider_id=prov", ""},
		{"model enum-backed referenced field", "ResetProviderModelField", "{\"field\":\"context_window\",\"query\":{\"provider_id\":\"prov\",\"model_id\":\"m\"}}", "DELETE", "/v1/providers/models/fields/context_window", "provider_id=prov&model_id=m", ""},
		{"enum-less normalized referenced field", "ResetPlainField", "{\"field\":\"anything\"}", "DELETE", "/v1/plain/fields/anything", "", ""},
		{"plain string session path", "PingSession", "{\"session_id\":\"s1\"}", "POST", "/v1/sessions/s1/plain", "", ""},
		{"normalized operation ID", "get_extra", "{\"query\":{\"cursor\":\"abc\"}}", "GET", "/v1/normalized", "cursor=abc", ""},
		{"operation-level required override", "QueryOverride", "{\"query\":{\"flag\":true}}", "GET", "/v1/overrides", "flag=true", ""},
		{"operation-level optional override", "OptionalOverride", "{\"query\":{\"limit\":\"x\"}}", "GET", "/v1/optional-override", "limit=x", ""},
		{"optional override without a query member", "OptionalOverride", "", "GET", "/v1/optional-override", "", ""},
		{"raw JSON body", "CreateSession", "{\"body\":{\"workspace\":\"/ws\",\"opaque\":9007199254740993}}", "POST", "/v1/sessions", "", "{\"workspace\":\"/ws\",\"opaque\":9007199254740993}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			before := len(recorded)
			mu.Unlock()
			response, status, err := invokeGenerated(context.Background(), client, tc.operation, json.RawMessage(tc.params))
			if err != nil {
				t.Fatalf("invokeGenerated(%s): %v", tc.operation, err)
			}
			if status != http.StatusOK {
				t.Fatalf("%s status = %d, want %d", tc.operation, status, http.StatusOK)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "{\"ok\":true}" {
				t.Fatalf("%s result = %q, %v, want the exact response bytes", tc.operation, body, err)
			}
			mu.Lock()
			call := recorded[before]
			mu.Unlock()
			if call.method != tc.method || call.path != tc.path || call.query != tc.query || call.body != tc.body {
				t.Fatalf("%s request = (%s %s ? %s body %q), want (%s %s ? %s body %q)", tc.operation, call.method, call.path, call.query, call.body, tc.method, tc.path, tc.query, tc.body)
			}
		})
	}
	{
		mu.Lock()
		before := len(recorded)
		mu.Unlock()
		_, _, err := invokeGenerated(context.Background(), client, "QueryOverride", nil)
		if !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("omitted required override error = %v, want ErrInvalidParams", err)
		}
		mu.Lock()
		reached := len(recorded) != before
		mu.Unlock()
		if reached {
			t.Fatal("a locally rejected query override reached the transport")
		}
	}
	names := OperationNames()
	want := []string{"CreateSession", "GetHealth", "OptionalOverride", "PingSession", "QueryOverride", "ResetPlainField", "ResetProviderField", "ResetProviderModelField", "get_extra"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("OperationNames() = %v, want %v", names, want)
	}
}
`

// TestGeneratedFixtureCompilesAndDispatches generates a fixture SDK and its
// native bindings into a disposable module with the product module path, copies
// the real stateless binder, compiles them, and drives the actual generated
// client through the binder against httptest routes.
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
	contract, err := os.ReadFile(contractPath)
	testassert.NoError(t, err)
	testassert.Check(t, strings.Contains(string(contract), `"get_extra": {`), "fixture contract does not advertise the source operation ID:\n%s", contract)
	testassert.Check(t, !strings.Contains(string(contract), "GetEvents"), "fixture contract carries the event stream:\n%s", contract)
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
	cmd := exec.Command("go", "test", "./internal/clientops")
	cmd.Dir = dir
	cmd.Env = fixtureEnv()
	{
		out, err := cmd.CombinedOutput()
		testassert.Require(t, err == nil, "compiling/dispatching the generated fixture: %v\n%s", err, out)
	}
}

// fixtureEnv returns the process environment with the disposable module's build
// flags pinned: no network, the local module cache and the copied go.sum.
func fixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GOFLAGS=") || strings.HasPrefix(entry, "GOPROXY=") || strings.HasPrefix(entry, "GOSUMDB=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off")
}
