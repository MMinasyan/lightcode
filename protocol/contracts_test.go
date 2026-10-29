package protocol_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The schema is loaded and validated once; every contract case below checks
// one axis of that validated document or of the generated Go artifacts.
var (
	schemaOnce sync.Once
	schemaDoc  *openapi3.T
	schemaErr  error
)

func schema(t *testing.T) *openapi3.T {
	t.Helper()
	schemaOnce.Do(func() {
		loader := openapi3.NewLoader()
		schemaDoc, schemaErr = loader.LoadFromFile("openapi.yaml")
		if schemaErr == nil {
			schemaErr = schemaDoc.Validate(context.Background())
		}
	})
	testassert.Require(t, schemaErr == nil, "loading/validating protocol/openapi.yaml: %v", schemaErr)
	return schemaDoc
}

func componentSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	component := schema(t).Components.Schemas[name]
	testassert.Require(t, component != nil, "schema component %q is missing", name)
	return component.Value
}

func acceptJSON(t *testing.T, s *openapi3.Schema, value string) {
	t.Helper()
	{
		err := s.VisitJSON(decodeSchemaJSON(t, value))
		testassert.Require(t, err == nil, "schema rejected valid value %s: %v", value, err)
	}
}

func rejectJSON(t *testing.T, s *openapi3.Schema, value string) {
	t.Helper()
	{
		err := s.VisitJSON(decodeSchemaJSON(t, value))
		testassert.Require(t, err != nil, "schema accepted invalid value %s", value)
	}
}

// decodeSchemaJSON decodes one test value with exact numbers preserved, so
// extreme exponents (1e1000) reach the validator without a float64 overflow.
func decodeSchemaJSON(t *testing.T, value string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	testassert.NoError(t, decoder.Decode(&decoded))
	return decoded
}

const (
	revisionJSON = `{"instance_id": "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f", "generation": "3"}`
)

// TestPluginsSettingsDocumentsStayNamedOpaqueStrings keeps the settings
// plugins member a map of the one named PluginConfigDocument scalar: an
// inline or structured replacement would silently re-interpret plugin
// documents and fails here.
func TestPluginsSettingsDocumentsStayNamedOpaqueStrings(t *testing.T) {
	plugins := componentSchema(t, "PluginsSettings")
	{
		ref := plugins.AdditionalProperties.Schema
		testassert.Require(t, ref != nil && ref.Ref == "#/components/schemas/PluginConfigDocument", "PluginsSettings additionalProperties = %+v, want the named PluginConfigDocument scalar", ref)
	}
	testassert.Require(t, len(plugins.Properties) == 0, "PluginsSettings declares named members %v, want the opaque map only", plugins.Properties)
	document := componentSchema(t, "PluginConfigDocument")
	testassert.Require(t, document.Type != nil && document.Type.Is("string"), "PluginConfigDocument type = %v, want string", document.Type)
}

// TestScopeEventSharedPayload keeps the scope_opened and scope_closed kinds
// on one shared component with both discriminator literals intact; unknown
// kinds and unrelated fields stay rejected.
func TestScopeEventSharedPayload(t *testing.T) {
	event := componentSchema(t, "Event")
	acceptJSON(t, event, `{"kind": "scope_opened", "scope": {"kind": "runtime"}}`)
	acceptJSON(t, event, `{"kind": "scope_closed", "scope": {"kind": "job", "session_id": "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f", "job_id": "j1"}}`)
	rejectJSON(t, event, `{"kind": "scope_unknown", "scope": {"kind": "runtime"}}`)
	rejectJSON(t, event, `{"kind": "scope_opened", "scope": {"kind": "runtime"}, "extra": 1}`)

	discriminator := event.Discriminator
	testassert.Require(t, discriminator != nil && discriminator.PropertyName == "kind", "Event discriminator is missing")
	for _, kind := range []string{"scope_opened", "scope_closed"} {
		{
			got := discriminator.Mapping[kind].Ref
			testassert.Require(t, got == "#/components/schemas/ScopeEvent", "Event discriminator mapping %q = %q, want the shared ScopeEvent component", kind, got)
		}
	}
	scopeEventRefs := 0
	for _, branch := range event.OneOf {
		if branch.Ref == "#/components/schemas/ScopeEvent" {
			scopeEventRefs++
		}
	}
	testassert.Equal(t, scopeEventRefs, 1)
}

func TestGeneratedScopeEventRoundTripsBothKinds(t *testing.T) {
	for _, tc := range []struct {
		kind protocol.ScopeEventKind
		wire string
	}{
		{protocol.ScopeOpened, "scope_opened"},
		{protocol.ScopeClosed, "scope_closed"},
	} {
		var event protocol.ScopeEvent
		{
			err := json.Unmarshal([]byte(`{"kind": "`+tc.wire+`", "scope": {"kind": "runtime"}}`), &event)
			testassert.Require(t, err == nil, "decoding generated ScopeEvent %s: %v", tc.wire, err)
		}
		testassert.Require(t, event.Kind == tc.kind, "decoded ScopeEvent kind = %q, want %q", event.Kind, tc.wire)
		data, err := json.Marshal(event)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), `"`+tc.wire+`"`), "generated ScopeEvent JSON = %s, want the %s kind preserved", data, tc.wire)
	}
}

// TestRevertDefaultsShareTheCodeRevertErrorContract keeps the revert default
// response on its shared union contract: both wire cases (a plain schema
// error and the partial result carrying its error) stay accepted, and a body
// matching neither branch stays rejected.
func TestRevertDefaultsShareTheCodeRevertErrorContract(t *testing.T) {
	doc := schema(t)
	for _, row := range []struct{ path string }{
		{"/v1/sessions/{id}/revert-code"},
	} {
		item := doc.Paths.Find(row.path)
		testassert.Require(t, item != nil && item.Post != nil, "%s has no POST operation", row.path)
		defaultResponse := item.Post.Responses.Default()
		testassert.Require(t, defaultResponse != nil && defaultResponse.Value != nil, "%s POST has no default response", row.path)
		ref := defaultResponse.Value.Content.Get("application/json").Schema.Ref
		{
			want := "#/components/schemas/CodeRevertError"
			testassert.Require(t, ref == want, "%s default schema ref = %q, want %q", row.path, ref, want)
		}
	}

	s := componentSchema(t, "CodeRevertError")
	acceptJSON(t, s, `{"code": "internal", "message": "traversal failed"}`)
	acceptJSON(t, s, `{"restored": [], "skipped": [], "error": {"code": "storage", "message": "io"}}`)
	rejectJSON(t, s, `{}`)
}

// TestProviderAndModelFieldSetsShareEditAndResetPaths pins the one shared
// field vocabulary per subject: the reset path parameters reference the
// shared components, the components carry the exact editable-member sets
// including hidden, and each edit request's property names equal its field
// enum exactly — so the edit and reset paths cannot drift apart. The
// api_key_env member keeps its gosec-safe generated identifier.
func TestProviderAndModelFieldSetsShareEditAndResetPaths(t *testing.T) {
	field := protocol.ProviderFieldEnvironmentVariable
	testassert.Require(t, field.Valid() && field == "api_key_env", "generated identifier %q is not the valid api_key_env member", field)
	testassert.Require(t, protocol.ProviderFieldHidden.Valid() && protocol.ModelFieldHidden.Valid(), "the shared field enums dropped the hidden member")

	doc := schema(t)
	for _, row := range []struct {
		path      string
		component string
		wantEnum  []string
	}{
		{"/v1/providers/fields/{field}", "ProviderField", []string{
			"name", "base_url", "api_key_env", "headers", "system_role",
			"usage_in_stream", "extra_body", "discovery", "protocol_metadata", "hidden",
		}},
		{"/v1/providers/models/fields/{field}", "ModelField", []string{
			"name", "context_window", "max_output_tokens", "input_modalities", "system_role",
			"usage_in_stream", "extra_body", "cost", "protocol_metadata", "hidden",
		}},
	} {
		item := doc.Paths.Find(row.path)
		testassert.Require(t, item != nil && item.Delete != nil, "%s has no DELETE operation", row.path)
		var param *openapi3.Parameter
		for _, p := range item.Delete.Parameters {
			if p.Value != nil && p.Value.Name == "field" {
				param = p.Value
				break
			}
		}
		testassert.Require(t, param != nil, "%s field path parameter is missing", row.path)
		{
			ref := param.Schema.Ref
			testassert.Require(t, ref == "#/components/schemas/"+row.component, "%s field parameter ref = %q, want the shared %s component", row.path, ref, row.component)
		}
		component := componentSchema(t, row.component)
		testassert.Require(t, len(component.Enum) == len(row.wantEnum), "%s enum holds %d values, want %d", row.component, len(component.Enum), len(row.wantEnum))
		for i, v := range row.wantEnum {
			testassert.Require(t, component.Enum[i] == v, "%s enum[%d] = %v, want %q", row.component, i, component.Enum[i], v)
		}
		// The edit request's property names are exactly the field enum: one
		// vocabulary shared by both paths.
		editName := strings.TrimSuffix(row.component, "Field") + "Edit"
		edit := componentSchema(t, editName)
		var properties []string
		for name := range edit.Properties {
			properties = append(properties, name)
		}
		sort.Strings(properties)
		want := append([]string(nil), row.wantEnum...)
		sort.Strings(want)
		testassert.Require(t, strings.Join(properties, ",") == strings.Join(want, ","), "%s properties %v, want exactly the %s enum %v", editName, properties, row.component, want)
	}
}

// TestGeneratedSystemRoleSharedAcrossEditsAndViews is a compile-time check:
// one generated SystemRole value must be assignable to the provider and
// model edit and view fields that share the concept.
func TestGeneratedSystemRoleSharedAcrossEditsAndViews(t *testing.T) {
	role := protocol.SystemRoleDeveloper
	providerEdit := protocol.ProviderEdit{SystemRole: &role}
	modelEdit := protocol.ModelEdit{SystemRole: &role}
	modelView := protocol.ModelView{SystemRole: role}
	provider := protocol.Provider{SystemRole: role}
	testassert.Require(t, providerEdit.SystemRole != nil && modelEdit.SystemRole != nil && modelView.SystemRole == protocol.SystemRoleDeveloper && provider.SystemRole == protocol.SystemRoleDeveloper, "generated SystemRole is not shared across provider/model edits and views")
}

// TestConversationRawObjectFieldsShareOneComponent keeps the conversation
// extra/normalized-argument fields on the one shared raw-valued object
// component and the tool metadata on its own component — and nothing else in
// the schema on either.
func TestConversationRawObjectFieldsShareOneComponent(t *testing.T) {
	doc := schema(t)
	var refs []string
	for name, schemaRef := range doc.Components.Schemas {
		for field, prop := range schemaRef.Value.Properties {
			switch prop.Ref {
			case "#/components/schemas/JSONObject":
				refs = append(refs, name+"."+field+"=JSONObject")
			case "#/components/schemas/ToolMetadata":
				refs = append(refs, name+"."+field+"=ToolMetadata")
			}
		}
	}
	sort.Strings(refs)
	want := []string{
		"AssistantItem.extra=JSONObject",
		"ImageURLPart.extra=JSONObject",
		"Message.extra=JSONObject",
		"MessageToolCall.extra=JSONObject",
		"OpaquePart.extra=JSONObject",
		"TextPart.extra=JSONObject",
		"ToolCallView.extra=JSONObject",
		"ToolCallView.metadata=ToolMetadata",
		"ToolCallView.normalized_arguments=JSONObject",
	}
	testassert.Require(t, reflect.DeepEqual(refs, want), "shared raw-object component references = %v, want exactly %v", refs, want)
}

// TestGeneratedJSONObjectPreservesNumbersThroughUnionConstructors proves the
// generated raw-valued object keeps opaque numbers exact through the
// union constructors a producer uses and the As accessors a consumer uses.
// Every opaque value comes from the corpus's JSONObject fixture.
func TestGeneratedJSONObjectPreservesNumbersThroughUnionConstructors(t *testing.T) {
	raw := contractComponentFixture(t, loadContractCorpus(t), "JSONObject", "raw-numbers")
	var members map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal([]byte(raw), &members))
	normalized := protocol.JSONObject(members)
	metadata := protocol.ToolMetadata(raw)
	item := protocol.AssistantItem{
		ItemId:      "assistant-1",
		CommittedAt: time.Time{}.UTC(),
		Status:      protocol.AssistantItemStatusCompleted,
		Source:      "prov/m",
		Content:     []protocol.ContentPart{},
		ToolCalls: []protocol.ToolCallView{{
			Id:                  "call-1",
			Name:                "read",
			Arguments:           "{}",
			NormalizedArguments: &normalized,
			Status:              &[]protocol.ToolCallStatus{protocol.ToolCallStatusSuccess}[0],
			Content:             &[]string{""}[0],
			Metadata:            &metadata,
		}},
	}
	var wire protocol.ConversationItem
	testassert.NoError(t, wire.FromAssistantItem(item))
	data, err := json.Marshal(wire)
	testassert.NoError(t, err)
	for _, want := range []string{string(members["n"]), string(members["big"])} {
		testassert.Require(t, strings.Contains(string(data), want), "wire JSON = %s, want %q preserved", data, want)
	}
	decoded, err := wire.AsAssistantItem()
	testassert.NoError(t, err)
	testassert.Require(t, decoded.ToolCalls[0].NormalizedArguments != nil && string((*decoded.ToolCalls[0].NormalizedArguments)["n"]) == string(members["n"]), "decoded normalized arguments = %v, want the raw number preserved", decoded.ToolCalls[0].NormalizedArguments)
	// A raw-valued object re-encodes canonically (sorted members) while every
	// value stays byte-exact: the fixture's opaque numbers survive untouched.
	want := "{\"big\":" + string(members["big"]) + ",\"n\":" + string(members["n"]) + "}"
	testassert.Require(t, decoded.ToolCalls[0].Metadata != nil && string(*decoded.ToolCalls[0].Metadata) == want, "decoded metadata = %v, want the raw value preserved", decoded.ToolCalls[0].Metadata)
}

// TestSettingsBaselineSectionsStayRawObjects keeps the top-level tools, jobs
// and compaction members schema-defined JSON objects generated as the raw
// message alias: an inline structured type would renumber or reinterpret
// their members, while a plugin-document string would change the transport
// shape.
func TestSettingsBaselineSectionsStayRawObjects(t *testing.T) {
	settings := componentSchema(t, "Settings")
	for _, name := range []string{"tools", "jobs", "compaction"} {
		property, ok := settings.Properties[name]
		testassert.Require(t, ok && property.Ref == "#/components/schemas/"+strings.ToUpper(name[:1])+name[1:]+"Settings", "Settings.%s = %+v, want its named object component", name, property)
		component := componentSchema(t, strings.ToUpper(name[:1])+name[1:]+"Settings")
		testassert.Require(t, component.Type != nil && component.Type.Is("object"), "%sSettings type = %v, want object", name, component.Type)
		testassert.Require(t, component.Extensions["x-go-type"] == "json.RawMessage", "%sSettings x-go-type = %v, want json.RawMessage", name, component.Extensions["x-go-type"])
		testassert.Require(t, component.AdditionalProperties.Has != nil && *component.AdditionalProperties.Has, "%sSettings must accept raw members", name)
	}
}
