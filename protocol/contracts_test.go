package protocol_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatalf("decoding test value: %v", err)
	}
	if err := s.VisitJSON(decoded); err != nil {
		t.Fatalf("schema rejected valid value %s: %v", value, err)
	}
}

func rejectJSON(t *testing.T, s *openapi3.Schema, value string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatalf("decoding test value: %v", err)
	}
	if err := s.VisitJSON(decoded); err == nil {
		t.Fatalf("schema accepted invalid value %s", value)
	}
}

const (
	revisionJSON = `{"instance_id": "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f", "generation": "3"}`
	usageJSON    = `{"input_tokens": "12", "cached_input_tokens": "0", "output_tokens": "34"}`
)

func TestDeletionMutationCarriesRequiredNullResult(t *testing.T) {
	s := componentSchema(t, "DeletionMutation")
	acceptJSON(t, s, `{"configuration_revision": `+revisionJSON+`, "result": null}`)
	rejectJSON(t, s, `{"configuration_revision": `+revisionJSON+`}`)
	rejectJSON(t, s, `{"configuration_revision": `+revisionJSON+`, "result": {"removed": true}}`)
}

func TestBothDeleteOperationsUseTheDeletionMutationEnvelope(t *testing.T) {
	doc := schema(t)
	for _, row := range []struct{ path string }{
		{"/v1/providers/detail"},
		{"/v1/providers/models"},
	} {
		item := doc.Paths.Find(row.path)
		if item == nil || item.Delete == nil {
			t.Fatalf("%s has no DELETE operation", row.path)
		}
		responseRef := item.Delete.Responses.Status(200)
		if responseRef == nil || responseRef.Value == nil {
			t.Fatalf("%s DELETE has no 200 response", row.path)
		}
		ref := responseRef.Value.Content.Get("application/json").Schema.Ref
		if want := "#/components/schemas/DeletionMutation"; ref != want {
			t.Fatalf("%s DELETE 200 schema ref = %q, want %q", row.path, ref, want)
		}
	}
}

func TestModelUsageCarriesOneSharedModelReference(t *testing.T) {
	s := componentSchema(t, "ModelUsage")
	acceptJSON(t, s, `{"model": "openrouter/team/model", "usage": `+usageJSON+`}`)
	rejectJSON(t, s, `{"provider": "openrouter", "model": "openrouter/team/model", "usage": `+usageJSON+`}`)
}

func TestGeneratedDeletionResponseSerializesExplicitNullResult(t *testing.T) {
	response := protocol.DeletionMutation{
		ConfigurationRevision: protocol.ConfigurationRevision{
			InstanceId: "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",
			Generation: "3",
		},
		Result: nil,
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshaling generated DeletionMutation: %v", err)
	}
	if !strings.Contains(string(data), `"result":null`) {
		t.Fatalf("generated deletion response JSON = %s, want an explicit null result", data)
	}
	var decoded protocol.DeletionMutation
	if err := json.Unmarshal([]byte(`{"configuration_revision": `+revisionJSON+`, "result": null}`), &decoded); err != nil {
		t.Fatalf("decoding a null-result deletion response into the generated type: %v", err)
	}
	if decoded.ConfigurationRevision.Generation != "3" {
		t.Fatalf("decoded generation = %q", decoded.ConfigurationRevision.Generation)
	}
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

// TestResetProviderFieldEnumNamingPreservesWireValues keeps the generated
// gosec-safe identifier for the api_key_env field while the schema retains
// every original wire enum literal in order.
func TestResetProviderFieldEnumNamingPreservesWireValues(t *testing.T) {
	field := protocol.ResetProviderFieldParamsFieldEnvironmentVariable
	if !field.Valid() {
		t.Fatalf("generated identifier %q is not a valid enum member", field)
	}
	if field != "api_key_env" {
		t.Fatalf("generated identifier encodes %q, want api_key_env", field)
	}

	item := schema(t).Paths.Find("/v1/providers/fields/{field}")
	if item == nil || item.Delete == nil {
		t.Fatal("/v1/providers/fields/{field} has no DELETE operation")
	}
	var param *openapi3.Parameter
	for _, p := range item.Delete.Parameters {
		if p.Value != nil && p.Value.Name == "field" {
			param = p.Value
			break
		}
	}
	if param == nil {
		t.Fatal("field path parameter is missing")
	}
	want := []string{"name", "base_url", "api_key_env", "headers", "options", "system_role", "usage_in_stream", "max_tokens_field", "extra_body", "discovery", "protocol_metadata"}
	got := param.Schema.Value.Enum
	if len(got) != len(want) {
		t.Fatalf("field enum holds %d values, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("field enum[%d] = %v, want %q", i, got[i], v)
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
