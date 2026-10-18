// Command protocolgen generates the native invocation bindings and their
// test-only invocation contract from the protocol schema. It is a private
// build command: it runs during generation, imports the pinned schema loader
// and the standard library, and never enters a shipped Runtime, Harness,
// client or plugin path.
//
//	go run ./internal/protocolgen \
//	  -schema protocol/openapi.yaml \
//	  -out internal/clientops/operations.gen.go \
//	  -native-contract-out frontend/src/generated/native-invocations.json
//
// Both artifacts are resolved, validated and formatted in memory before
// either destination is written, so an unsupported schema shape fails the
// command with both outputs untouched.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/getkin/kin-openapi/openapi3"
)

// The three envelope member modes the schema declares for one operation.
const (
	modeForbidden = "forbidden"
	modeOptional  = "optional"
	modeRequired  = "required"
)

// jsonMediaType and sseMediaType are the two success content types the
// generator understands; every other declared success content is refused.
const (
	jsonMediaType = "application/json"
	sseMediaType  = "text/event-stream"
)

// noContentStatus is the one success status whose response carries no body.
const noContentStatus = 204

// queryMember is one declared query parameter: its name, whether the
// operation requires it, and its declared primitive type.
type queryMember struct {
	name     string
	required bool
	kind     string
}

// operation is one schema-declared native operation in its generated form.
type operation struct {
	id           string
	session      string
	field        string
	fieldType    string
	query        string
	body         string
	queryMembers []queryMember
	status       int
}

// main parses the three explicit paths and runs the generator.
func main() {
	schema := flag.String("schema", "", "path to the protocol OpenAPI schema")
	out := flag.String("out", "", "path of the generated Go operation bindings")
	nativeContractOut := flag.String("native-contract-out", "", "path of the generated native invocation contract")
	flag.Parse()
	if err := run(*schema, *out, *nativeContractOut); err != nil {
		fmt.Fprintln(os.Stderr, "protocolgen:", err)
		os.Exit(1)
	}
}

// run writes the two artifacts generated from one schema. A schema or shape
// failure writes neither destination; an ordinary destination I/O failure
// fails the command after the other destination may already exist.
func run(schemaPath, outPath, contractPath string) error {
	source, contract, err := generateFiles(schemaPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, source, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}
	if err := os.WriteFile(contractPath, contract, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", contractPath, err)
	}
	return nil
}

// generateFiles loads the schema and returns both artifacts, fully formatted,
// without touching either destination.
func generateFiles(schemaPath string) ([]byte, []byte, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(schemaPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading %s: %w", schemaPath, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("validating %s: %w", schemaPath, err)
	}
	operations, err := collectOperations(doc)
	if err != nil {
		return nil, nil, err
	}
	source, err := renderSource(operations)
	if err != nil {
		return nil, nil, err
	}
	contract, err := renderContract(operations)
	if err != nil {
		return nil, nil, err
	}
	return source, contract, nil
}

// collectOperations derives every native operation from the validated schema:
// its addressing, its declared query members, its request body and its one
// success status. The one declared event stream is excluded — exactly one must
// exist — and any shape the envelope cannot address fails the command. Duplicate
// operation IDs are already rejected by the mandatory schema validation before
// this pass runs, so no second identity check is kept here.
func collectOperations(doc *openapi3.T) ([]operation, error) {
	paths := doc.Paths.Map()
	var (
		operations []operation
		streamed   []string
	)
	for _, pathKey := range sortedKeys(paths) {
		item := paths[pathKey]
		methods := item.Operations()
		for _, method := range sortedKeys(methods) {
			declared := methods[method]
			if !generatedName(declared.OperationID) {
				return nil, fmt.Errorf("%s %s: operationId %q is not a generated Go name", strings.ToUpper(method), pathKey, declared.OperationID)
			}
			status, stream, err := successResponse(declared)
			if err != nil {
				return nil, fmt.Errorf("operation %s: %w", declared.OperationID, err)
			}
			if stream {
				streamed = append(streamed, declared.OperationID)
				continue
			}
			op, err := nativeOperation(doc, item, declared, status)
			if err != nil {
				return nil, fmt.Errorf("operation %s: %w", declared.OperationID, err)
			}
			operations = append(operations, op)
		}
	}
	if len(streamed) != 1 {
		return nil, fmt.Errorf("declares %d event streams, want exactly one", len(streamed))
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].id < operations[j].id })
	return operations, nil
}

// successResponse reports one operation's single exact numeric 2xx status and
// whether its success content is the event stream. A declared 2XX range, an
// absent or multiple numeric success, a 204 that declares content, a success
// content set that is not exactly one of the two declared media types, and a
// non-204 success without content are all refused; every non-success
// alternative, including the default error response, is ignored.
func successResponse(declared *openapi3.Operation) (int, bool, error) {
	responses := declared.Responses.Map()
	var statuses []string
	for key := range responses {
		if !strings.HasPrefix(key, "2") {
			continue
		}
		if len(key) != 3 || !isDigits(key) {
			return 0, false, fmt.Errorf("success response %q is not one exact numeric status", key)
		}
		statuses = append(statuses, key)
	}
	sort.Strings(statuses)
	if len(statuses) != 1 {
		return 0, false, fmt.Errorf("declares %d success statuses, want exactly one", len(statuses))
	}
	status, err := strconv.Atoi(statuses[0])
	if err != nil {
		return 0, false, fmt.Errorf("success status %q is not a number", statuses[0])
	}
	response := responses[statuses[0]]
	if response.Value == nil {
		return 0, false, fmt.Errorf("success status %d is unresolved", status)
	}
	content := response.Value.Content
	// A 204 success is the empty success the retained response owner reads as
	// no body at all: it must declare no content, before any media
	// classification.
	if status == noContentStatus {
		if len(content) != 0 {
			return 0, false, fmt.Errorf("success status %d declares media types %v, want no content", status, sortedKeys(content))
		}
		return status, false, nil
	}
	switch {
	case len(content) == 1 && content[jsonMediaType] != nil:
		return status, false, nil
	case len(content) == 1 && content[sseMediaType] != nil:
		return status, true, nil
	case len(content) == 0:
		return 0, false, fmt.Errorf("success status %d declares no content", status)
	default:
		return 0, false, fmt.Errorf("success status %d declares media types %v, want exactly one of %s or %s", status, sortedKeys(content), jsonMediaType, sseMediaType)
	}
}

// isDigits reports whether every byte of one string is an ASCII decimal digit.
func isDigits(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// nativeOperation derives one native operation's addressing from its declared
// path parameters, query parameters and request body.
func nativeOperation(doc *openapi3.T, item *openapi3.PathItem, declared *openapi3.Operation, status int) (operation, error) {
	op := operation{
		id:      declared.OperationID,
		status:  status,
		session: modeForbidden,
		field:   modeForbidden,
		query:   modeForbidden,
		body:    modeForbidden,
	}
	var pathParameters int
	for _, parameter := range append(append(openapi3.Parameters{}, item.Parameters...), declared.Parameters...) {
		value := parameter.Value
		if value == nil {
			return operation{}, fmt.Errorf("parameter is unresolved")
		}
		switch value.In {
		case openapi3.ParameterInPath:
			pathParameters++
			if pathParameters > 1 {
				return operation{}, fmt.Errorf("declares %d path parameters, want at most one", pathParameters)
			}
			if value.Schema == nil || value.Schema.Value == nil {
				return operation{}, fmt.Errorf("path parameter %q has no schema", value.Name)
			}
			if ref := value.Schema.Ref; ref != "" {
				name, err := enumTypeName(doc, ref)
				if err != nil {
					return operation{}, fmt.Errorf("path parameter %q: %w", value.Name, err)
				}
				op.field = modeRequired
				op.fieldType = name
				continue
			}
			if value.Name != "id" || !isStringSchema(value.Schema.Value) {
				return operation{}, fmt.Errorf("path parameter %q has no supported addressing", value.Name)
			}
			op.session = modeRequired
		case openapi3.ParameterInQuery:
			member, err := queryParameter(value)
			if err != nil {
				return operation{}, err
			}
			for _, existing := range op.queryMembers {
				if existing.name == member.name {
					return operation{}, fmt.Errorf("query parameter %q is declared twice", member.name)
				}
			}
			op.queryMembers = append(op.queryMembers, member)
		default:
			return operation{}, fmt.Errorf("parameter %q is declared in unsupported location %q", value.Name, value.In)
		}
	}
	op.query = modeForbidden
	if len(op.queryMembers) > 0 {
		op.query = modeOptional
		for _, member := range op.queryMembers {
			if member.required {
				op.query = modeRequired
			}
		}
	}
	if declared.RequestBody != nil && declared.RequestBody.Value != nil {
		content := declared.RequestBody.Value.Content
		if len(content) != 1 || content[jsonMediaType] == nil {
			return operation{}, fmt.Errorf("request body declares media types %v, want exactly %s", sortedKeys(content), jsonMediaType)
		}
		schema := content[jsonMediaType].Schema
		if schema == nil || schema.Value == nil {
			return operation{}, fmt.Errorf("request body schema is unresolved")
		}
		if schema.Value.Type == nil || !schema.Value.Type.Is("object") {
			return operation{}, fmt.Errorf("request body schema is not one explicit object")
		}
		if !declared.RequestBody.Value.Required {
			return operation{}, fmt.Errorf("request body is optional, want required")
		}
		op.body = modeRequired
	}
	return op, nil
}

// enumTypeName resolves one referenced path-parameter schema to the generated
// protocol type the binder casts the envelope's field member to: an internal
// component reference to a resolved string schema with a nonempty all-string
// enum and a component name the generated client uses unchanged.
func enumTypeName(doc *openapi3.T, ref string) (string, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("reference %q is not an internal #/components/schemas/ reference", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	if !generatedName(name) {
		return "", fmt.Errorf("referenced component name %q is not a generated Go name", name)
	}
	component := doc.Components.Schemas[name]
	if component == nil || component.Value == nil {
		return "", fmt.Errorf("referenced component %q is unresolved", name)
	}
	schema := component.Value
	if !isStringSchema(schema) {
		return "", fmt.Errorf("referenced component %q is not the string type", name)
	}
	if len(schema.Enum) == 0 {
		return "", fmt.Errorf("referenced component %q declares no enum values", name)
	}
	for _, value := range schema.Enum {
		if _, ok := value.(string); !ok {
			return "", fmt.Errorf("referenced component %q declares the non-string enum value %v", name, value)
		}
	}
	return name, nil
}

// generatedName reports whether one schema identifier is already the exact
// generated Go name the pinned generator uses: nonempty, an ASCII uppercase
// initial and only ASCII letters and digits after it. An identifier that would
// need normalization — a lowercase initial, an underscore or other separator,
// a leading digit — is refused explicitly instead of being silently mapped or
// left to fail as generated Go.
func generatedName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9':
		default:
			return false
		}
	}
	return true
}

// queryParameter resolves one declared query parameter's primitive type: only
// the string and boolean primitives the generated Params types bind are
// supported.
func queryParameter(value *openapi3.Parameter) (queryMember, error) {
	if value.Schema == nil || value.Schema.Value == nil {
		return queryMember{}, fmt.Errorf("query parameter %q has no schema", value.Name)
	}
	schema := value.Schema.Value
	switch {
	case isStringSchema(schema):
		return queryMember{name: value.Name, required: value.Required, kind: "string"}, nil
	case schema.Type != nil && schema.Type.Is("boolean"):
		return queryMember{name: value.Name, required: value.Required, kind: "boolean"}, nil
	default:
		return queryMember{}, fmt.Errorf("query parameter %q has an unsupported primitive type", value.Name)
	}
}

// isStringSchema reports whether one schema is the string primitive.
func isStringSchema(schema *openapi3.Schema) bool {
	return schema.Type != nil && schema.Type.Is("string")
}

// sortedKeys returns one string-keyed map's keys in sorted order, so every
// derived artifact is deterministic.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sourceTemplate renders the generated Go bindings. go/format normalizes the
// result, so the template only has to emit valid, complete Go.
const sourceTemplate = `// Code generated by internal/protocolgen from protocol/openapi.yaml; DO NOT EDIT.
//
// The native operation table and its dispatch are derived from the schema:
// operationNames is the sorted set of declared native operations and
// invokeGenerated binds each one's declared addressing before calling the
// matching generated client method.
package clientops

import (
{{- if .NeedsBytes}}
	"bytes"
{{- end}}
	"context"
	"encoding/json"
	"net/http"

	"github.com/MMinasyan/lightcode/client"
{{- if .NeedsProtocol}}
	"github.com/MMinasyan/lightcode/protocol"
{{- end}}
)

// operationNames is the schema's native operation set in sorted order: every
// declared operation whose success response is not the event stream. It is the
// sole authority for the initialize advertisement and for Call's own admission.
var operationNames = []string{
{{- range .Names}}
	{{printf "%q" .}},
{{- end}}
}

// invokeGenerated binds one invocation envelope to the generated client
// method the named operation declares and returns that call's raw response
// beside the operation's declared success status. A local binding failure or a
// failure to obtain an HTTP response returns a nil response and the zero
// status; the caller owns response interpretation.
func invokeGenerated(ctx context.Context, c *client.Client, operation string, params json.RawMessage) (*http.Response, int, error) {
	switch operation {
{{- range .Arms}}
{{template "arm" .}}
{{- end}}
	default:
		return nil, 0, invalidParams("unknown operation %q", operation)
	}
}
{{define "arm"}}	case {{printf "%q" .ID}}:
{{- if .NeedEnv}}
		env, err := bindEnvelope({{.Envelope}})
		if err != nil {
			return nil, 0, err
		}
{{- else}}
		if _, err := bindEnvelope({{.Envelope}}); err != nil {
			return nil, 0, err
		}
{{- end}}
{{- if .QueryType}}
		var query protocol.{{.QueryType}}
{{- if .QueryOptional}}
		if env.query != nil {
			if err := bindQuery(env.query, nil, &query); err != nil {
				return nil, 0, err
			}
		}
{{- else}}
		if err := bindQuery(env.query, {{.QueryRequired}}, &query); err != nil {
			return nil, 0, err
		}
{{- end}}
{{- end}}
		response, callErr := {{.Call}}
		return response, {{.Expected}}, callErr
{{end}}`

// armView is one generated dispatch arm.
type armView struct {
	ID            string
	Envelope      string
	NeedEnv       bool
	QueryType     string
	QueryRequired string
	QueryOptional bool
	Call          string
	Expected      string
}

// sourceData is the whole generated file's template data.
type sourceData struct {
	Names         []string
	Arms          []armView
	NeedsBytes    bool
	NeedsProtocol bool
}

// renderSource renders and formats the generated Go bindings in memory.
func renderSource(operations []operation) ([]byte, error) {
	data := sourceData{}
	for _, op := range operations {
		data.Names = append(data.Names, op.id)
		data.Arms = append(data.Arms, arm(op))
		if op.body != modeForbidden {
			data.NeedsBytes = true
		}
		if op.session != modeForbidden || op.field != modeForbidden || op.query != modeForbidden {
			data.NeedsProtocol = true
		}
	}
	parsed, err := template.New("source").Parse(sourceTemplate)
	if err != nil {
		return nil, err
	}
	var rendered strings.Builder
	if err := parsed.Execute(&rendered, data); err != nil {
		return nil, err
	}
	formatted, err := format.Source([]byte(rendered.String()))
	if err != nil {
		return nil, fmt.Errorf("formatting the generated bindings: %w", err)
	}
	return formatted, nil
}

// arm derives one dispatch arm: the envelope's declared member rules, the
// query binding and the exact generated client call for the operation. The
// declared success status is emitted as the schema's own number, so any single
// numeric 2xx is supported without a named standard-library constant.
func arm(op operation) armView {
	view := armView{
		ID: op.id,
		Envelope: fmt.Sprintf("params, %s, %s, %s, %s",
			goMode(op.session), goMode(op.field), goMode(op.query), goMode(op.body)),
		Expected: strconv.Itoa(op.status),
	}
	view.NeedEnv = op.session != modeForbidden || op.field != modeForbidden || op.query != modeForbidden || op.body != modeForbidden
	if op.query != modeForbidden {
		view.QueryType = op.id + "Params"
		view.QueryOptional = op.query == modeOptional
		var required []string
		for _, member := range op.queryMembers {
			if member.required {
				required = append(required, strconv.Quote(member.name))
			}
		}
		if len(required) == 0 {
			view.QueryRequired = "nil"
		} else {
			view.QueryRequired = "[]string{" + strings.Join(required, ", ") + "}"
		}
	}
	arguments := []string{"ctx"}
	if op.session != modeForbidden {
		arguments = append(arguments, "protocol.SessionID(env.session)")
	}
	if op.fieldType != "" {
		arguments = append(arguments, "protocol."+op.fieldType+"(env.field)")
	}
	if op.query != modeForbidden {
		arguments = append(arguments, "&query")
	}
	name := "c." + op.id
	if op.body != modeForbidden {
		name += "WithBody"
		arguments = append(arguments, "contentTypeJSON", "bytes.NewReader(env.body)")
	}
	view.Call = name + "(" + strings.Join(arguments, ", ") + ")"
	return view
}

// goMode maps one declared envelope mode to the handwritten member rule
// constant the generated bindings use.
func goMode(mode string) string {
	switch mode {
	case modeRequired:
		return "memberRequired"
	case modeOptional:
		return "memberOptional"
	default:
		return "memberForbidden"
	}
}

// contractMember is one query member's published contract.
type contractMember struct {
	Required bool   `json:"required"`
	Type     string `json:"type"`
}

// contractOperation is one operation's published contract.
type contractOperation struct {
	SessionID    string                    `json:"session_id"`
	Field        string                    `json:"field"`
	Query        string                    `json:"query"`
	Body         string                    `json:"body"`
	QueryMembers map[string]contractMember `json:"query_members"`
}

// renderContract renders the test-only native invocation contract in memory:
// one entry per native operation, keyed by operation ID, with sorted keys and
// no stream entry.
func renderContract(operations []operation) ([]byte, error) {
	doc := make(map[string]contractOperation, len(operations))
	for _, op := range operations {
		members := make(map[string]contractMember, len(op.queryMembers))
		for _, member := range op.queryMembers {
			members[member.name] = contractMember{Required: member.required, Type: member.kind}
		}
		doc[op.id] = contractOperation{
			SessionID:    op.session,
			Field:        op.field,
			Query:        op.query,
			Body:         op.body,
			QueryMembers: members,
		}
	}
	rendered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("rendering the native invocation contract: %w", err)
	}
	return append(rendered, '\n'), nil
}
