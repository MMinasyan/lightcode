// Command protocolgen generates the SDK, the native invocation bindings and
// their test-only invocation contract from one protocol schema. It is a
// private build command: it imports the pinned SDK generator and schema loader
// and never enters a shipped Runtime, Harness, client or plugin path.
//
//	go run ./internal/protocolgen \
//	  -schema protocol/openapi.yaml \
//	  -config protocol/oapi-codegen.yaml \
//	  -sdk-out protocol/protocol.gen.go \
//	  -out internal/clientops/operations.gen.go \
//	  -native-contract-out frontend/src/generated/native-invocations.json
//
// The schema is loaded and validated once. The SDK generator owns every
// schema/reference/type/name derivation; this command only projects its
// resolved operation records into the native envelope limits before writing.
// All three artifacts are resolved and formatted in memory before any
// destination is written, so an unsupported schema shape or a failed SDK
// generation leaves all three untouched.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
	"gopkg.in/yaml.v3"
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

// operation is one SDK-resolved native operation in its projected form.
type operation struct {
	id            string
	method        string
	session       string
	sessionType   string
	field         string
	fieldType     string
	query         string
	queryRequired []string
	queryMembers  []queryMember
	body          string
	status        int
}

// main parses the explicit paths and runs the generator.
func main() {
	schema := flag.String("schema", "", "path to the protocol OpenAPI schema")
	config := flag.String("config", "", "path to the SDK generator configuration")
	sdkOut := flag.String("sdk-out", "", "path of the generated SDK")
	out := flag.String("out", "", "path of the generated Go operation bindings")
	nativeContractOut := flag.String("native-contract-out", "", "path of the generated native invocation contract")
	flag.Parse()
	if err := run(*schema, *config, *sdkOut, *out, *nativeContractOut); err != nil {
		fmt.Fprintln(os.Stderr, "protocolgen:", err)
		os.Exit(1)
	}
}

// run writes the three artifacts generated from one schema. A schema, SDK or
// shape failure writes none of the destinations; an ordinary destination I/O
// failure fails the command after the earlier destinations may already exist.
func run(schemaPath, configPath, sdkPath, outPath, contractPath string) error {
	sdk, source, contract, err := generateFiles(schemaPath, configPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sdkPath, sdk, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", sdkPath, err)
	}
	if err := os.WriteFile(outPath, source, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}
	if err := os.WriteFile(contractPath, contract, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", contractPath, err)
	}
	return nil
}

// loadConfiguration reads one SDK generator configuration with unknown members
// rejected, applies its defaults and validates it.
func loadConfiguration(path string) (codegen.Configuration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codegen.Configuration{}, fmt.Errorf("reading %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var configuration codegen.Configuration
	if err := decoder.Decode(&configuration); err != nil {
		return codegen.Configuration{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	configuration = configuration.UpdateDefaults()
	if err := configuration.Validate(); err != nil {
		return codegen.Configuration{}, fmt.Errorf("validating %s: %w", path, err)
	}
	return configuration, nil
}

// generateFiles loads the schema, generates its SDK and projects the SDK's
// resolved operation records into both native artifacts. Nothing is written.
// Generate runs first because OperationDefinitions' schema/type mapping
// depends on the global state Generate initializes; the two calls are
// sequential over the same document.
func generateFiles(schemaPath, configPath string) ([]byte, []byte, []byte, error) {
	configuration, err := loadConfiguration(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	loader := openapi3.NewLoader()
	// Source origins feed OperationDefinition.SpecOrder; without them the SDK
	// producer would lose the schema's declaration order.
	loader.IncludeOrigin = true
	doc, err := loader.LoadFromFile(schemaPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading %s: %w", schemaPath, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, nil, nil, fmt.Errorf("validating %s: %w", schemaPath, err)
	}
	if err := validatePathSchemas(doc); err != nil {
		return nil, nil, nil, err
	}
	sdk, err := codegen.Generate(doc, configuration)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating the SDK: %w", err)
	}
	definitions, err := codegen.OperationDefinitions(doc)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolving the SDK operation records: %w", err)
	}
	operations, err := projectOperations(definitions)
	if err != nil {
		return nil, nil, nil, err
	}
	source, err := renderSource(operations)
	if err != nil {
		return nil, nil, nil, err
	}
	contract, err := renderContract(operations)
	if err != nil {
		return nil, nil, nil, err
	}
	return []byte(sdk), source, contract, nil
}

// validatePathSchemas inspects every operation path parameter's effective
// local reference chain before any output is rendered. The supported domain is
// one unformatted string, string alias or string enum with no x-go-type
// override at the parameter, reference site or any intermediate allOf
// definition; harmless naming/import metadata by itself is not rejected.
func validatePathSchemas(doc *openapi3.T) error {
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			// Operation-level parameters override path-level parameters with the
			// same name and location; validate the effective set only.
			effective := make(map[string]*openapi3.ParameterRef, len(item.Parameters)+len(operation.Parameters))
			for _, parameter := range item.Parameters {
				if parameter.Value != nil {
					effective[parameter.Value.In+"\x00"+parameter.Value.Name] = parameter
				}
			}
			for _, parameter := range operation.Parameters {
				if parameter.Value != nil {
					effective[parameter.Value.In+"\x00"+parameter.Value.Name] = parameter
				}
			}
			for _, parameter := range effective {
				if parameter.Value.In != "path" {
					continue
				}
				if err := validatePathSchema(doc, parameter.Value.Schema, fmt.Sprintf("%s %s path parameter %q", method, path, parameter.Value.Name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validatePathSchema(doc *openapi3.T, ref *openapi3.SchemaRef, label string) error {
	return validatePathSchemaNode(doc, ref, label, make(map[string]bool))
}

// validatePathSchemaNode checks one schema reference's own metadata, then
// follows the explicit local component-reference chain from the loaded
// document before checking the resolved value. A plain alias collapses
// SchemaRef.Value to its target while keeping its own Ref and Extensions only
// on the component declaration, so .Value alone is not the effective chain.
func validatePathSchemaNode(doc *openapi3.T, ref *openapi3.SchemaRef, label string, seen map[string]bool) error {
	if ref == nil {
		return fmt.Errorf("%s does not resolve to one local schema", label)
	}
	if err := rejectPathOverride(ref.Extensions, label); err != nil {
		return err
	}
	if ref.Ref != "" {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(ref.Ref, prefix) {
			return fmt.Errorf("%s does not resolve to one local schema", label)
		}
		name := strings.TrimPrefix(ref.Ref, prefix)
		if name == "" || seen[name] {
			return fmt.Errorf("%s does not resolve to one local schema", label)
		}
		// The mark covers only this recursion path: two completed allOf
		// siblings may reference the same supported schema independently.
		seen[name] = true
		defer delete(seen, name)
		component, ok := doc.Components.Schemas[name]
		if !ok {
			return fmt.Errorf("%s does not resolve to one local schema", label)
		}
		return validatePathSchemaNode(doc, component, label, seen)
	}
	return validatePathSchemaValue(doc, ref.Value, label, seen)
}

func validatePathSchemaValue(doc *openapi3.T, schema *openapi3.Schema, label string, seen map[string]bool) error {
	if schema == nil {
		return fmt.Errorf("%s does not resolve to one local schema", label)
	}
	if err := rejectPathOverride(schema.Extensions, label); err != nil {
		return err
	}
	if schema.Format != "" {
		return fmt.Errorf("%s declares unsupported format %q", label, schema.Format)
	}
	// An allOf wrapper's own type is part of the effective schema: a
	// non-string own type is not made supported by its members.
	if schema.Type != nil && !schema.Type.Is("string") {
		return fmt.Errorf("%s does not resolve to the string type", label)
	}
	if len(schema.OneOf) != 0 || len(schema.AnyOf) != 0 {
		return fmt.Errorf("%s is not one plain string schema", label)
	}
	if len(schema.AllOf) != 0 {
		for _, member := range schema.AllOf {
			if err := validatePathSchemaNode(doc, member, label, seen); err != nil {
				return err
			}
		}
		return nil
	}
	if schema.Type == nil || !schema.Type.Is("string") {
		return fmt.Errorf("%s does not resolve to the string type", label)
	}
	return nil
}

// rejectPathOverride refuses the generator override the pinned SDK would read
// from this schema-reference or schema node. Naming/import metadata alone is
// not a type override and stays allowed.
func rejectPathOverride(extensions map[string]any, label string) error {
	if _, overridden := extensions["x-go-type"]; overridden {
		return fmt.Errorf("%s carries an x-go-type override", label)
	}
	return nil
}

// projectOperations projects every SDK-resolved operation into the native
// envelope: stream operations are classified first and excluded whole, then
// the envelope's addressing limits are checked against the SDK records.
func projectOperations(definitions []codegen.OperationDefinition) ([]operation, error) {
	var operations []operation
	for _, definition := range definitions {
		name := definition.SpecOperationId
		if name == "" {
			return nil, fmt.Errorf("operation %s %s has no source operationId", definition.Method, definition.Path)
		}
		status, stream, err := successResponse(definition.Responses)
		if err != nil {
			return nil, fmt.Errorf("operation %s: %w", name, err)
		}
		if stream {
			continue
		}
		op, err := projectOperation(definition, name, status)
		if err != nil {
			return nil, fmt.Errorf("operation %s: %w", name, err)
		}
		operations = append(operations, op)
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
func successResponse(responses []codegen.ResponseDefinition) (int, bool, error) {
	var statuses []string
	for _, response := range responses {
		key := response.StatusCode
		if !strings.HasPrefix(key, "2") {
			continue
		}
		if len(key) != 3 || !isDigits(key) {
			return 0, false, fmt.Errorf("success response %q is not one exact numeric status", key)
		}
		statuses = append(statuses, key)
	}
	if len(statuses) != 1 {
		return 0, false, fmt.Errorf("declares %d success statuses, want exactly one", len(statuses))
	}
	status, _ := strconv.Atoi(statuses[0])
	var contents []codegen.ResponseContentDefinition
	for _, response := range responses {
		if response.StatusCode == statuses[0] {
			contents = response.Contents
			break
		}
	}
	// A 204 success is the empty success the retained response owner reads as
	// no body at all: it must declare no content, before any media
	// classification.
	if status == noContentStatus {
		if len(contents) != 0 {
			return 0, false, fmt.Errorf("success status %d declares media types %v, want no content", status, contentTypes(contents))
		}
		return status, false, nil
	}
	switch {
	case len(contents) == 1 && contents[0].ContentType == jsonMediaType:
		return status, false, nil
	case len(contents) == 1 && contents[0].ContentType == sseMediaType:
		return status, true, nil
	case len(contents) == 0:
		return 0, false, fmt.Errorf("success status %d declares no content", status)
	default:
		return 0, false, fmt.Errorf("success status %d declares media types %v, want exactly one of %s or %s", status, contentTypes(contents), jsonMediaType, sseMediaType)
	}
}

// contentTypes returns one response's declared media types in sorted order.
func contentTypes(contents []codegen.ResponseContentDefinition) []string {
	types := make([]string, 0, len(contents))
	for _, content := range contents {
		types = append(types, content.ContentType)
	}
	sort.Strings(types)
	return types
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

// projectOperation derives one native operation's addressing from its SDK
// combined path/query parameter records and its resolved request body. A
// referenced path-parameter schema binds the envelope's field member; an
// inline string parameter named id binds session_id. Both must resolve to the
// string type. All query parameters must be string or boolean primitives, and
// a body must be exactly one required application/json object.
func projectOperation(definition codegen.OperationDefinition, name string, status int) (operation, error) {
	op := operation{
		id:      name,
		method:  definition.OperationId,
		status:  status,
		session: modeForbidden,
		field:   modeForbidden,
		query:   modeForbidden,
		body:    modeForbidden,
	}
	if len(definition.HeaderParams) != 0 {
		return operation{}, fmt.Errorf("declares %d header parameters, want none", len(definition.HeaderParams))
	}
	if len(definition.CookieParams) != 0 {
		return operation{}, fmt.Errorf("declares %d cookie parameters, want none", len(definition.CookieParams))
	}
	switch len(definition.PathParams) {
	case 0:
	case 1:
		parameter := definition.PathParams[0]
		schema := parameter.Schema.OAPISchema
		if schema == nil || schema.Type == nil || !schema.Type.Is("string") {
			return operation{}, fmt.Errorf("path parameter %q is not the string type", parameter.ParamName)
		}
		emitted, err := pathType(parameter.Schema.TypeDecl())
		if err != nil {
			return operation{}, fmt.Errorf("path parameter %q: %w", parameter.ParamName, err)
		}
		if parameter.Spec != nil && parameter.Spec.Schema != nil && parameter.Spec.Schema.Ref != "" {
			op.field = modeRequired
			op.fieldType = emitted
		} else if parameter.ParamName == "id" {
			op.session = modeRequired
			op.sessionType = emitted
		} else {
			return operation{}, fmt.Errorf("path parameter %q has no supported addressing", parameter.ParamName)
		}
	default:
		return operation{}, fmt.Errorf("declares %d path parameters, want at most one", len(definition.PathParams))
	}
	for _, parameter := range definition.QueryParams {
		schema := parameter.Schema.OAPISchema
		if schema == nil || schema.Type == nil {
			return operation{}, fmt.Errorf("query parameter %q has an unsupported primitive type", parameter.ParamName)
		}
		member := queryMember{name: parameter.ParamName, required: parameter.Required}
		switch {
		case schema.Type.Is("string"):
			member.kind = "string"
		case schema.Type.Is("boolean"):
			member.kind = "boolean"
		default:
			return operation{}, fmt.Errorf("query parameter %q has an unsupported primitive type", parameter.ParamName)
		}
		op.queryMembers = append(op.queryMembers, member)
	}
	if len(op.queryMembers) != 0 {
		op.query = modeOptional
		for _, member := range op.queryMembers {
			if member.required {
				op.query = modeRequired
				op.queryRequired = append(op.queryRequired, member.name)
			}
		}
	}
	if len(definition.Bodies) != 0 {
		if len(definition.Bodies) != 1 || definition.Bodies[0].ContentType != jsonMediaType {
			types := make([]string, 0, len(definition.Bodies))
			for _, body := range definition.Bodies {
				types = append(types, body.ContentType)
			}
			sort.Strings(types)
			return operation{}, fmt.Errorf("request body declares media types %v, want exactly %s", types, jsonMediaType)
		}
		body := definition.Bodies[0]
		if !body.Required {
			return operation{}, fmt.Errorf("request body is optional, want required")
		}
		schema := body.Schema.OAPISchema
		if schema == nil || schema.Type == nil || !schema.Type.Is("object") {
			return operation{}, fmt.Errorf("request body schema is not one explicit object")
		}
		op.body = modeRequired
	}
	return op, nil
}

// pathType maps one SDK path-parameter type declaration to the Go type the
// generated call binds: the builtin string or an exported package-local
// identifier qualified with the protocol package. Any other type expression is
// an unsupported native shape.
func pathType(declaration string) (string, error) {
	if declaration == "string" {
		return "string", nil
	}
	if token.IsIdentifier(declaration) && token.IsExported(declaration) {
		return "protocol." + declaration, nil
	}
	return "", fmt.Errorf("type %q is not a supported native path type", declaration)
}

// sourceTemplate renders the generated native operation table. go/format
// normalizes the result, so the template only has to emit valid, complete Go.
const sourceTemplate = `// Code generated by internal/protocolgen from protocol/openapi.yaml; DO NOT EDIT.
//
// The native operation table is the SDK-resolved projection of the schema: each
// descriptor carries one operation's envelope member rules, its one numeric
// success status and the generated client call for its bound envelope.
package clientops

{{- if .Imports}}
import (
{{- if .NeedsBytes}}
	"bytes"
{{- end}}
	"context"
	"net/http"

	"github.com/Syntheresis/melta/protocol"
)
{{- end}}

// operationDescriptors is the schema's native operation set keyed by its source
// operationId. Each call binds the operation's generated path and query
// arguments from the already-bound envelope and performs exactly one generated
// SDK request; the shared invoker in bind.go applies the envelope rules and the
// one response owner interprets the result.
var operationDescriptors = map[string]descriptor{
{{- range .Operations}}
	{{printf "%q" .ID}}: {
{{- if .Session}}
		session: memberRequired,
{{- end}}
{{- if .Field}}
		field: memberRequired,
{{- end}}
{{- if .Query}}
		query: member{{if .QueryOptional}}Optional{{else}}Required{{end}},
{{- end}}
{{- if .Body}}
		body: memberRequired,
{{- end}}
		status: {{.Status}},
		call: func(ctx context.Context, c protocol.ClientInterface, env envelope) (*http.Response, error) {
{{- if .Session}}
			pathValue := {{.SessionType}}(env.session)
{{- end}}
{{- if .Field}}
			pathValue := {{.FieldType}}(env.field)
{{- end}}
{{- if .Query}}
			var query protocol.{{.QueryType}}
{{- if .QueryOptional}}
			if env.query != nil {
				if err := bindQuery(env.query, nil, &query); err != nil {
					return nil, err
				}
			}
{{- else}}
			if err := bindQuery(env.query, {{.QueryRequired}}, &query); err != nil {
				return nil, err
			}
{{- end}}
{{- end}}
			return {{.Call}}
		},
	},
{{- end}}
}
`

// armView is one generated descriptor literal.
type armView struct {
	ID            string
	Session       bool
	SessionType   string
	Field         bool
	FieldType     string
	Query         bool
	QueryOptional bool
	QueryRequired string
	QueryType     string
	Body          bool
	Call          string
	Status        int
}

// sourceData is the whole generated file's template data.
type sourceData struct {
	Operations []armView
	Imports    bool
	NeedsBytes bool
}

// renderSource renders and formats the generated native bindings in memory.
func renderSource(operations []operation) ([]byte, error) {
	data := sourceData{Imports: len(operations) != 0}
	for _, op := range operations {
		view := armView{
			ID:          op.id,
			Session:     op.session != modeForbidden,
			SessionType: op.sessionType,
			Field:       op.field != modeForbidden,
			FieldType:   op.fieldType,
			Query:       op.query != modeForbidden,
			QueryType:   op.method + "Params",
			Body:        op.body != modeForbidden,
			Status:      op.status,
			Call:        callExpression(op),
		}
		view.QueryOptional = op.query == modeOptional
		if len(op.queryRequired) != 0 {
			quoted := make([]string, 0, len(op.queryRequired))
			for _, name := range op.queryRequired {
				quoted = append(quoted, strconv.Quote(name))
			}
			view.QueryRequired = "[]string{" + strings.Join(quoted, ", ") + "}"
		} else {
			view.QueryRequired = "nil"
		}
		if view.Body {
			data.NeedsBytes = true
		}
		data.Operations = append(data.Operations, view)
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

// callExpression renders one descriptor's generated SDK call: the bound path
// value, the bound query Params and the raw body reader in the SDK's declared
// argument order.
func callExpression(op operation) string {
	arguments := []string{"ctx"}
	if op.session != modeForbidden || op.field != modeForbidden {
		arguments = append(arguments, "pathValue")
	}
	if op.query != modeForbidden {
		arguments = append(arguments, "&query")
	}
	name := "c." + op.method
	if op.body != modeForbidden {
		name += "WithBody"
		arguments = append(arguments, "contentTypeJSON", "bytes.NewReader(env.body)")
	}
	return name + "(" + strings.Join(arguments, ", ") + ")"
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
// one entry per native operation, keyed by native operation ID, with sorted
// keys and no stream entry.
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
