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
	if schemaErr != nil {
		t.Fatalf("loading/validating protocol/openapi.yaml: %v", schemaErr)
	}
	return schemaDoc
}

func componentSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	component := schema(t).Components.Schemas[name]
	if component == nil {
		t.Fatalf("schema component %q is missing", name)
	}
	return component.Value
}

func acceptJSON(t *testing.T, s *openapi3.Schema, value string) {
	t.Helper()
	if err := s.VisitJSON(decodeSchemaJSON(t, value)); err != nil {
		t.Fatalf("schema rejected valid value %s: %v", value, err)
	}
}

func rejectJSON(t *testing.T, s *openapi3.Schema, value string) {
	t.Helper()
	if err := s.VisitJSON(decodeSchemaJSON(t, value)); err == nil {
		t.Fatalf("schema accepted invalid value %s", value)
	}
}

// decodeSchemaJSON decodes one test value with exact numbers preserved, so
// extreme exponents (1e1000) reach the validator without a float64 overflow.
func decodeSchemaJSON(t *testing.T, value string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decoding test value: %v", err)
	}
	return decoded
}

const (
	revisionJSON = `{"instance_id": "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f", "generation": "3"}`
	usageJSON    = `{"input_tokens": "12", "cached_input_tokens": "0", "output_tokens": "34"}`
)

// TestPluginsSettingsDocumentsStayNamedOpaqueStrings keeps the settings
// plugins member a map of the one named PluginConfigDocument scalar: an
// inline or structured replacement would silently re-interpret plugin
// documents and fails here.
func TestPluginsSettingsDocumentsStayNamedOpaqueStrings(t *testing.T) {
	plugins := componentSchema(t, "PluginsSettings")
	if ref := plugins.AdditionalProperties.Schema; ref == nil || ref.Ref != "#/components/schemas/PluginConfigDocument" {
		t.Fatalf("PluginsSettings additionalProperties = %+v, want the named PluginConfigDocument scalar", ref)
	}
	if len(plugins.Properties) != 0 {
		t.Fatalf("PluginsSettings declares named members %v, want the opaque map only", plugins.Properties)
	}
	document := componentSchema(t, "PluginConfigDocument")
	if document.Type == nil || !document.Type.Is("string") {
		t.Fatalf("PluginConfigDocument type = %v, want string", document.Type)
	}
	acceptJSON(t, document, `"{\"kept\":1,\"exact\":9007199254740993}"`)
	rejectJSON(t, document, `{"kept":1}`)
}

func TestModelUsageCarriesOneSharedModelReference(t *testing.T) {
	s := componentSchema(t, "ModelUsage")
	acceptJSON(t, s, `{"model": "openrouter/team/model", "usage": `+usageJSON+`}`)
	rejectJSON(t, s, `{"provider": "openrouter", "model": "openrouter/team/model", "usage": `+usageJSON+`}`)
}

func TestGeneratedModelUsageSerializesSingleModelReference(t *testing.T) {
	usage := protocol.ModelUsage{
		Model: "openrouter/team/model",
		Usage: protocol.UsageCount{
			InputTokens:       "12",
			CachedInputTokens: "0",
			OutputTokens:      "34",
		},
	}
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshaling generated ModelUsage: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding generated ModelUsage JSON: %v", err)
	}
	if len(decoded) != 2 || decoded["model"] != "openrouter/team/model" {
		t.Fatalf("generated ModelUsage JSON = %s, want one shared model reference and usage", data)
	}
}

func TestRetainedSnapshotDomain(t *testing.T) {
	turn := componentSchema(t, "RetainedTurn")
	acceptJSON(t, turn, `{"turn": 1, "files": []}`)
	rejectJSON(t, turn, `{"turn": 0, "files": []}`)

	revert := componentSchema(t, "RetainedRevertRequest")
	acceptJSON(t, revert, `{"workspace": "/w", "session_id": "0f0f0f0f", "after_turn": -1}`)
	acceptJSON(t, revert, `{"workspace": "/w", "session_id": "0f0f0f0f", "after_turn": 0}`)
}

// TestGeneratedCostRoundTripsDoublePrecision proves the generated Cost
// scalar keeps ordinary price values exact across a JSON roundtrip; float32
// generation collapses 0.123456789 to 0.12345679.
func TestGeneratedCostRoundTripsDoublePrecision(t *testing.T) {
	var cost protocol.Cost
	if err := json.Unmarshal([]byte(`{"input":0.123456789,"output":0.987654321,"cache_read":0.123456789,"cache_write":0.123456789}`), &cost); err != nil {
		t.Fatalf("decoding generated Cost: %v", err)
	}
	data, err := json.Marshal(cost)
	if err != nil {
		t.Fatalf("marshaling generated Cost: %v", err)
	}
	var decoded map[string]float64
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding generated Cost JSON: %v", err)
	}
	for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
		if got := decoded[field]; got != 0.123456789 && got != 0.987654321 {
			t.Fatalf("generated Cost field %q roundtripped to %v (JSON: %s)", field, got, data)
		}
	}
	if decoded["output"] != 0.987654321 {
		t.Fatalf("generated Cost output roundtripped to %v (JSON: %s)", decoded["output"], data)
	}

	empty, err := json.Marshal(protocol.Cost{})
	if err != nil {
		t.Fatalf("marshaling empty Cost: %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("empty generated Cost JSON = %s, want every optional field omitted", empty)
	}
}

// TestScopeEventSharedPayload keeps the scope_opened and scope_closed kinds
// on one shared component with both discriminator literals intact; unknown
// kinds and unrelated fields stay rejected.
func TestScopeEventSharedPayload(t *testing.T) {
	event := componentSchema(t, "Event")
	acceptJSON(t, event, `{"kind": "scope_opened", "scope": {"kind": "runtime"}}`)
	acceptJSON(t, event, `{"kind": "scope_closed", "scope": {"kind": "job", "job_id": "j1"}}`)
	rejectJSON(t, event, `{"kind": "scope_unknown", "scope": {"kind": "runtime"}}`)
	rejectJSON(t, event, `{"kind": "scope_opened", "scope": {"kind": "runtime"}, "extra": 1}`)

	discriminator := event.Discriminator
	if discriminator == nil || discriminator.PropertyName != "kind" {
		t.Fatal("Event discriminator is missing")
	}
	for _, kind := range []string{"scope_opened", "scope_closed"} {
		if got := discriminator.Mapping[kind].Ref; got != "#/components/schemas/ScopeEvent" {
			t.Fatalf("Event discriminator mapping %q = %q, want the shared ScopeEvent component", kind, got)
		}
	}
	scopeEventRefs := 0
	for _, branch := range event.OneOf {
		if branch.Ref == "#/components/schemas/ScopeEvent" {
			scopeEventRefs++
		}
	}
	if scopeEventRefs != 1 {
		t.Fatalf("Event oneOf references ScopeEvent %d times, want exactly once", scopeEventRefs)
	}
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
		if err := json.Unmarshal([]byte(`{"kind": "`+tc.wire+`", "scope": {"kind": "runtime"}}`), &event); err != nil {
			t.Fatalf("decoding generated ScopeEvent %s: %v", tc.wire, err)
		}
		if event.Kind != tc.kind {
			t.Fatalf("decoded ScopeEvent kind = %q, want %q", event.Kind, tc.wire)
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshaling generated ScopeEvent: %v", err)
		}
		if !strings.Contains(string(data), `"`+tc.wire+`"`) {
			t.Fatalf("generated ScopeEvent JSON = %s, want the %s kind preserved", data, tc.wire)
		}
	}
}

// TestRevertDefaultsShareTheCodeRevertErrorContract keeps the two revert
// default responses on one shared union contract: both wire cases (a plain
// schema error and the partial result carrying its error) stay accepted,
// and a body matching neither branch stays rejected.
func TestRevertDefaultsShareTheCodeRevertErrorContract(t *testing.T) {
	doc := schema(t)
	for _, row := range []struct{ path string }{
		{"/v1/sessions/{id}/revert-code"},
		{"/v1/retained-code-snapshots/revert"},
	} {
		item := doc.Paths.Find(row.path)
		if item == nil || item.Post == nil {
			t.Fatalf("%s has no POST operation", row.path)
		}
		defaultResponse := item.Post.Responses.Default()
		if defaultResponse == nil || defaultResponse.Value == nil {
			t.Fatalf("%s POST has no default response", row.path)
		}
		ref := defaultResponse.Value.Content.Get("application/json").Schema.Ref
		if want := "#/components/schemas/CodeRevertError"; ref != want {
			t.Fatalf("%s default schema ref = %q, want %q", row.path, ref, want)
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
	if !field.Valid() || field != "api_key_env" {
		t.Fatalf("generated identifier %q is not the valid api_key_env member", field)
	}
	if !protocol.ProviderFieldHidden.Valid() || !protocol.ModelFieldHidden.Valid() {
		t.Fatal("the shared field enums dropped the hidden member")
	}

	doc := schema(t)
	for _, row := range []struct {
		path      string
		component string
		wantEnum  []string
	}{
		{"/v1/providers/fields/{field}", "ProviderField", []string{
			"name", "base_url", "api_key_env", "headers", "options", "system_role",
			"usage_in_stream", "max_tokens_field", "extra_body", "discovery", "protocol_metadata", "hidden",
		}},
		{"/v1/providers/models/fields/{field}", "ModelField", []string{
			"name", "context_window", "max_output_tokens", "input_modalities", "system_role",
			"usage_in_stream", "extra_body", "cost", "protocol_metadata", "hidden",
		}},
	} {
		item := doc.Paths.Find(row.path)
		if item == nil || item.Delete == nil {
			t.Fatalf("%s has no DELETE operation", row.path)
		}
		var param *openapi3.Parameter
		for _, p := range item.Delete.Parameters {
			if p.Value != nil && p.Value.Name == "field" {
				param = p.Value
				break
			}
		}
		if param == nil {
			t.Fatalf("%s field path parameter is missing", row.path)
		}
		if ref := param.Schema.Ref; ref != "#/components/schemas/"+row.component {
			t.Fatalf("%s field parameter ref = %q, want the shared %s component", row.path, ref, row.component)
		}
		component := componentSchema(t, row.component)
		if len(component.Enum) != len(row.wantEnum) {
			t.Fatalf("%s enum holds %d values, want %d", row.component, len(component.Enum), len(row.wantEnum))
		}
		for i, v := range row.wantEnum {
			if component.Enum[i] != v {
				t.Fatalf("%s enum[%d] = %v, want %q", row.component, i, component.Enum[i], v)
			}
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
		if strings.Join(properties, ",") != strings.Join(want, ",") {
			t.Fatalf("%s properties %v, want exactly the %s enum %v", editName, properties, row.component, want)
		}
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
	if providerEdit.SystemRole == nil || modelEdit.SystemRole == nil ||
		modelView.SystemRole != protocol.SystemRoleDeveloper ||
		provider.SystemRole != protocol.SystemRoleDeveloper {
		t.Fatal("generated SystemRole is not shared across provider/model edits and views")
	}
}

// TestToolMetadataAcceptsEveryNonNullJSONValue pins the tool-owned metadata
// contract: one bounded non-null JSON value of any kind — no type constraint.
// The top-level null literal is not a value and stays rejected.
func TestToolMetadataAcceptsEveryNonNullJSONValue(t *testing.T) {
	s := componentSchema(t, "ToolMetadata")
	acceptJSON(t, s, `{"n": 9007199254740993}`)
	acceptJSON(t, s, `[1, "two", false]`)
	acceptJSON(t, s, `"line count"`)
	acceptJSON(t, s, `true`)
	acceptJSON(t, s, `0`)
	acceptJSON(t, s, `{"rows": [null, {"depth": 1e1000}]}`)
	acceptJSON(t, s, `1e1000`)
	rejectJSON(t, s, `null`)
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
		"OpaquePart.extra=JSONObject",
		"TextPart.extra=JSONObject",
		"ToolCallView.extra=JSONObject",
		"ToolCallView.metadata=ToolMetadata",
		"ToolCallView.normalized_arguments=JSONObject",
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("shared raw-object component references = %v, want exactly %v", refs, want)
	}
}

// TestGeneratedToolMetadataRoundTripsRawValues proves the generated
// tool-metadata and normalized-argument members keep every JSON value
// byte-exact across a decode and re-encode: large integers and extreme
// exponents never pass through a float64, and every non-null kind is
// accepted. The probe decodes through the generated type and re-inspects the
// re-encoded bytes through raw members, so the check exercises exactly the
// generated contract's value fidelity.
func TestGeneratedToolMetadataRoundTripsRawValues(t *testing.T) {
	const wire = `{"id":"call-1","name":"read","arguments":"e30=","metadata":{"n":9007199254740993,"big":1e1000,"list":[null,false,0],"text":"x"}}`
	var view protocol.ToolCallView
	if err := json.Unmarshal([]byte(wire), &view); err != nil {
		t.Fatalf("decoding generated ToolCallView: %v", err)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshaling generated ToolCallView: %v", err)
	}
	for _, want := range []string{"9007199254740993", "1e1000", `[null,false,0]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("re-encoded ToolCallView JSON = %s, want %q preserved", data, want)
		}
	}

	// The same fidelity for the raw-valued normalized-argument object.
	const normalized = `{"id":"call-2","name":"grep","arguments":"e30=","normalized_arguments":{"n":9007199254740993,"big":1e1000}}`
	var normalizedView protocol.ToolCallView
	if err := json.Unmarshal([]byte(normalized), &normalizedView); err != nil {
		t.Fatalf("decoding generated normalized arguments: %v", err)
	}
	reEncoded, err := json.Marshal(normalizedView)
	if err != nil {
		t.Fatalf("marshaling generated normalized arguments: %v", err)
	}
	for _, want := range []string{"9007199254740993", "1e1000"} {
		if !strings.Contains(string(reEncoded), want) {
			t.Fatalf("re-encoded normalized arguments = %s, want %q preserved", reEncoded, want)
		}
	}
}

// TestGeneratedJSONObjectPreservesNumbersThroughUnionConstructors proves the
// generated raw-valued object keeps opaque numbers exact through the
// union constructors a producer uses and the As accessors a consumer uses.
func TestGeneratedJSONObjectPreservesNumbersThroughUnionConstructors(t *testing.T) {
	normalized := protocol.JSONObject{"n": json.RawMessage(`9007199254740993`), "big": json.RawMessage(`1e1000`)}
	metadata := protocol.ToolMetadata(json.RawMessage(`{"n":9007199254740993,"big":1e1000}`))
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
	if err := wire.FromAssistantItem(item); err != nil {
		t.Fatalf("FromAssistantItem: %v", err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshaling generated ConversationItem: %v", err)
	}
	for _, want := range []string{"9007199254740993", "1e1000"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("wire JSON = %s, want %q preserved", data, want)
		}
	}
	decoded, err := wire.AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	if decoded.ToolCalls[0].NormalizedArguments == nil || string((*decoded.ToolCalls[0].NormalizedArguments)["n"]) != `9007199254740993` {
		t.Fatalf("decoded normalized arguments = %v, want the raw number preserved", decoded.ToolCalls[0].NormalizedArguments)
	}
	// A raw-valued object re-encodes canonically (sorted members) while every
	// value stays byte-exact: the two opaque numbers survive untouched.
	if decoded.ToolCalls[0].Metadata == nil || string(*decoded.ToolCalls[0].Metadata) != `{"big":1e1000,"n":9007199254740993}` {
		t.Fatalf("decoded metadata = %v, want the raw value preserved", decoded.ToolCalls[0].Metadata)
	}
}
