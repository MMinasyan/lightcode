package protocol_test

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/MMinasyan/lightcode/protocol"
)

// The shared contract corpus: one committed JSON document with every fixture
// payload stored as raw JSON text, so an outer parse can never round an
// opaque numeric lexeme before the generated Go type or the schema sees it.
// The generated TypeScript literal checks read the same file.
const contractCorpusPath = "testdata/protocol_contract_fixtures.json"

type contractFixture struct {
	Name string `json:"name"`
	JSON string `json:"json"`
}

type contractComponent struct {
	Schema string            `json:"schema"`
	Accept []contractFixture `json:"accept"`
	Reject []contractFixture `json:"reject"`
}

type contractRawFixture struct {
	Schema   string   `json:"schema"`
	JSON     string   `json:"json"`
	Preserve []string `json:"preserve"`
}

type contractOperation struct {
	OperationID string             `json:"operation_id"`
	Method      string             `json:"method"`
	Path        string             `json:"path"`
	Request     *string            `json:"request"`
	Responses   map[string]*string `json:"responses"`
}

type contractCorpus struct {
	Components  []contractComponent  `json:"components"`
	Operations  []contractOperation  `json:"operations"`
	RawFidelity []contractRawFixture `json:"raw_fidelity"`
}

func loadContractCorpus(t *testing.T) contractCorpus {
	t.Helper()
	data, err := os.ReadFile(contractCorpusPath)
	if err != nil {
		t.Fatalf("reading the shared contract corpus: %v", err)
	}
	var corpus contractCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("decoding the shared contract corpus: %v", err)
	}
	return corpus
}

// contractComponentFixture locates one named fixture inside one component.
func contractComponentFixture(t *testing.T, corpus contractCorpus, component, fixture string) string {
	t.Helper()
	for _, entry := range corpus.Components {
		if entry.Schema != component {
			continue
		}
		for _, candidate := range entry.Accept {
			if candidate.Name == fixture {
				return candidate.JSON
			}
		}
		for _, candidate := range entry.Reject {
			if candidate.Name == fixture {
				return candidate.JSON
			}
		}
		t.Fatalf("component %q has no fixture %q", component, fixture)
	}
	t.Fatalf("the contract corpus has no component %q", component)
	return ""
}

// contractFixtureObjects decodes one component's accepted fixtures as JSON
// member maps; every component fixture is a JSON object.
func contractFixtureObjects(t *testing.T, corpus contractCorpus, component string) []map[string]json.RawMessage {
	t.Helper()
	for _, entry := range corpus.Components {
		if entry.Schema != component {
			continue
		}
		objects := make([]map[string]json.RawMessage, 0, len(entry.Accept))
		for _, fixture := range entry.Accept {
			var members map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fixture.JSON), &members); err != nil {
				t.Fatalf("%s accept %q: %v", component, fixture.Name, err)
			}
			objects = append(objects, members)
		}
		return objects
	}
	t.Fatalf("the contract corpus has no component %q", component)
	return nil
}

// requireSameSet fails unless both sets hold exactly the same values.
func requireSameSet(t *testing.T, what string, want, have map[string]bool) {
	t.Helper()
	var missing, extra []string
	for value := range want {
		if !have[value] {
			missing = append(missing, value)
		}
	}
	for value := range have {
		if !want[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("%s fixture coverage missing=%v extra=%v", what, missing, extra)
	}
}

// TestContractCorpusMatchesSchemaComponents proves the corpus names exactly
// the schema's component set in both directions — adding a schema component
// without a fixture, or keeping a fixture after its component disappears,
// fails here.
func TestContractCorpusMatchesSchemaComponents(t *testing.T) {
	doc := schema(t)
	corpus := loadContractCorpus(t)
	inCorpus := map[string]bool{}
	for _, entry := range corpus.Components {
		inCorpus[entry.Schema] = true
	}
	var missing, unknown []string
	for name := range doc.Components.Schemas {
		if !inCorpus[name] {
			missing = append(missing, name)
		}
	}
	for name := range inCorpus {
		if doc.Components.Schemas[name] == nil {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	if len(missing) != 0 || len(unknown) != 0 {
		t.Fatalf("schema↔corpus component set mismatch: missing from corpus=%v, unknown to schema=%v", missing, unknown)
	}
}

// TestContractCorpusFixtureNamesUnique rejects a repeated fixture name inside
// one component, across both buckets: names address fixtures.
func TestContractCorpusFixtureNamesUnique(t *testing.T) {
	corpus := loadContractCorpus(t)
	for _, entry := range corpus.Components {
		seen := map[string]string{}
		for bucket, fixtures := range map[string][]contractFixture{"accept": entry.Accept, "reject": entry.Reject} {
			for _, fixture := range fixtures {
				if where, ok := seen[fixture.Name]; ok {
					t.Errorf("%s fixture %q repeats (%s and %s)", entry.Schema, fixture.Name, where, bucket)
					continue
				}
				seen[fixture.Name] = bucket
			}
		}
	}
}

// TestContractCorpusGeneratedTypeCoverage pins the corpus to the generated Go
// surface: every corpus component has exactly one generated target, one
// accepted fixture, and every generated target is represented in the corpus.
func TestContractCorpusGeneratedTypeCoverage(t *testing.T) {
	corpus := loadContractCorpus(t)
	seen := map[string]bool{}
	for _, entry := range corpus.Components {
		if seen[entry.Schema] {
			t.Fatalf("component %q appears twice in the corpus", entry.Schema)
		}
		seen[entry.Schema] = true
		if _, ok := contractGeneratedTypes[entry.Schema]; !ok {
			t.Fatalf("component %q has no generated Go target", entry.Schema)
		}
		if len(entry.Accept) == 0 {
			t.Fatalf("component %q has no accepted fixture", entry.Schema)
		}
	}
	var untested []string
	for name := range contractGeneratedTypes {
		if !seen[name] {
			untested = append(untested, name)
		}
	}
	sort.Strings(untested)
	if len(untested) != 0 {
		t.Fatalf("generated targets missing from the corpus: %v", untested)
	}
}

// TestContractCorpusAcceptedFixturesRoundTrip drives every accepted fixture
// through the schema validator, the generated Go type, and the validator
// again over the re-encoded bytes.
func TestContractCorpusAcceptedFixturesRoundTrip(t *testing.T) {
	corpus := loadContractCorpus(t)
	for _, entry := range corpus.Components {
		schemaRef := componentSchema(t, entry.Schema)
		target := contractGeneratedTypes[entry.Schema]
		for _, fixture := range entry.Accept {
			if err := schemaRef.VisitJSON(decodeSchemaJSON(t, fixture.JSON)); err != nil {
				t.Errorf("%s accept %q: schema rejected valid value: %v\n%s", entry.Schema, fixture.Name, err, fixture.JSON)
				continue
			}
			value := target()
			if err := json.Unmarshal([]byte(fixture.JSON), value); err != nil {
				t.Errorf("%s accept %q: generated Go type rejected valid value: %v", entry.Schema, fixture.Name, err)
				continue
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Errorf("%s accept %q: re-encoding generated value: %v", entry.Schema, fixture.Name, err)
				continue
			}
			if err := schemaRef.VisitJSON(decodeSchemaJSON(t, string(encoded))); err != nil {
				t.Errorf("%s accept %q: schema rejected the re-encoded value %s: %v", entry.Schema, fixture.Name, encoded, err)
			}
		}
	}
}

// TestContractCorpusRejectedFixturesFailSchema proves every negative fixture
// is outside its component's validated set.
func TestContractCorpusRejectedFixturesFailSchema(t *testing.T) {
	corpus := loadContractCorpus(t)
	for _, entry := range corpus.Components {
		schemaRef := componentSchema(t, entry.Schema)
		for _, fixture := range entry.Reject {
			if err := schemaRef.VisitJSON(decodeSchemaJSON(t, fixture.JSON)); err == nil {
				t.Errorf("%s reject %q: schema accepted an invalid value: %s", entry.Schema, fixture.Name, fixture.JSON)
			}
		}
	}
}

// TestContractCorpusEnumValueCoverage derives the closed value sets from the
// schema — every Error.code and every ConversationItem/Event discriminator
// kind — and requires an accepted fixture for each value.
func TestContractCorpusEnumValueCoverage(t *testing.T) {
	corpus := loadContractCorpus(t)

	codeProperty := componentSchema(t, "Error").Properties["code"]
	if codeProperty == nil || codeProperty.Value == nil {
		t.Fatal("Error.code has no schema property")
	}
	allowedCodes := map[string]bool{}
	for _, value := range codeProperty.Value.Enum {
		code, ok := value.(string)
		if !ok {
			t.Fatalf("Error.code enum value %v is not a string", value)
		}
		allowedCodes[code] = true
	}
	haveCodes := map[string]bool{}
	for _, members := range contractFixtureObjects(t, corpus, "Error") {
		var code string
		if err := json.Unmarshal(members["code"], &code); err != nil {
			t.Fatalf("Error accept fixture code: %v", err)
		}
		haveCodes[code] = true
	}
	requireSameSet(t, "Error.code", allowedCodes, haveCodes)

	for _, union := range []string{"ConversationItem", "Event"} {
		unionSchema := componentSchema(t, union)
		if unionSchema.Discriminator == nil {
			t.Fatalf("%s has no discriminator", union)
		}
		allowedKinds := map[string]bool{}
		for kind := range unionSchema.Discriminator.Mapping {
			allowedKinds[kind] = true
		}
		haveKinds := map[string]bool{}
		for _, members := range contractFixtureObjects(t, corpus, union) {
			var kind string
			if err := json.Unmarshal(members["kind"], &kind); err != nil {
				t.Fatalf("%s accept fixture kind: %v", union, err)
			}
			haveKinds[kind] = true
		}
		requireSameSet(t, union+".kind", allowedKinds, haveKinds)
	}
}

// TestContractCorpusOptionalMembersPresentAndAbsent derives every optional
// member from the schema and requires accepted fixtures on both sides: one
// that carries the member and one that omits it.
func TestContractCorpusOptionalMembersPresentAndAbsent(t *testing.T) {
	corpus := loadContractCorpus(t)
	for _, entry := range corpus.Components {
		schemaRef := componentSchema(t, entry.Schema)
		required := map[string]bool{}
		for _, name := range schemaRef.Required {
			required[name] = true
		}
		var optional []string
		for name := range schemaRef.Properties {
			if !required[name] {
				optional = append(optional, name)
			}
		}
		if len(optional) == 0 {
			continue
		}
		sort.Strings(optional)
		accepted := contractFixtureObjects(t, corpus, entry.Schema)
		for _, member := range optional {
			present, absent := false, false
			for _, members := range accepted {
				if _, ok := members[member]; ok {
					present = true
				} else {
					absent = true
				}
			}
			if !present || !absent {
				t.Errorf("%s optional member %q is not pinned by accepted present and absent fixtures (present=%v absent=%v)", entry.Schema, member, present, absent)
			}
		}
	}
}

// TestContractCorpusOperationsMatchSchema derives every operation's request
// and response component from the schema and compares it with the corpus
// mapping, proving the corpus covers each request/response shape exactly.
func TestContractCorpusOperationsMatchSchema(t *testing.T) {
	doc := schema(t)
	corpus := loadContractCorpus(t)
	have := map[string]contractOperation{}
	for _, operation := range corpus.Operations {
		if _, duplicate := have[operation.OperationID]; duplicate {
			t.Fatalf("operation %q appears twice in the corpus", operation.OperationID)
		}
		have[operation.OperationID] = operation
	}
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			if method != "GET" && method != "POST" && method != "PUT" && method != "DELETE" {
				continue
			}
			want := contractOperation{
				OperationID: operation.OperationID,
				Method:      strings.ToLower(method),
				Path:        path,
				Request:     operationRequestComponent(operation),
				Responses:   operationResponseComponents(doc, operation),
			}
			found, ok := have[operation.OperationID]
			if !ok {
				t.Fatalf("operation %q is missing from the contract corpus", operation.OperationID)
			}
			if !reflect.DeepEqual(found, want) {
				t.Fatalf("operation %q corpus mapping = %+v, want %+v", operation.OperationID, found, want)
			}
			delete(have, operation.OperationID)
		}
	}
	if len(have) != 0 {
		var unknown []string
		for id := range have {
			unknown = append(unknown, id)
		}
		sort.Strings(unknown)
		t.Fatalf("contract corpus names unknown operations: %v", unknown)
	}
	for _, operation := range corpus.Operations {
		for _, component := range []*string{operation.Request} {
			if component == nil || *component == "" {
				continue
			}
			requireAcceptedComponent(t, corpus, operation.OperationID, *component)
		}
		for status, component := range operation.Responses {
			if component == nil {
				continue
			}
			requireAcceptedComponent(t, corpus, operation.OperationID+" response "+status, *component)
		}
	}
}

func requireAcceptedComponent(t *testing.T, corpus contractCorpus, where, name string) {
	t.Helper()
	name = strings.TrimSuffix(name, "[]")
	for _, entry := range corpus.Components {
		if entry.Schema == name && len(entry.Accept) > 0 {
			return
		}
	}
	t.Fatalf("%s references component %q with no accepted fixture", where, name)
}

// operationRequestComponent reports the corpus spelling of one operation's
// JSON request body: a component name, "" for the inline empty object, or nil
// when the operation carries no JSON body.
func operationRequestComponent(operation *openapi3.Operation) *string {
	if operation.RequestBody == nil || operation.RequestBody.Value == nil {
		return nil
	}
	media := operation.RequestBody.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil {
		return nil
	}
	ref := media.Schema
	if ref.Ref != "" {
		name := componentName(ref.Ref)
		return &name
	}
	if ref.Value != nil && ref.Value.Type != nil && ref.Value.Type.Is("object") &&
		len(ref.Value.Properties) == 0 && ref.Value.AdditionalProperties.Has != nil && !*ref.Value.AdditionalProperties.Has {
		empty := ""
		return &empty
	}
	return nil
}

// operationResponseComponents maps every JSON response of one operation to
// its component name; an array response maps to its item component.
func operationResponseComponents(doc *openapi3.T, operation *openapi3.Operation) map[string]*string {
	out := map[string]*string{}
	for status, response := range operation.Responses.Map() {
		if response.Ref != "" {
			response = doc.Components.Responses[componentName(response.Ref)]
			if response == nil {
				continue
			}
		}
		if response.Value == nil {
			continue
		}
		media := response.Value.Content.Get("application/json")
		if media == nil || media.Schema == nil {
			continue
		}
		ref := media.Schema
		switch {
		case ref.Ref != "":
			name := componentName(ref.Ref)
			out[status] = &name
		case ref.Value != nil && ref.Value.Type != nil && ref.Value.Type.Is("array") && ref.Value.Items != nil && ref.Value.Items.Ref != "":
			name := componentName(ref.Value.Items.Ref) + "[]"
			out[status] = &name
		default:
			out[status] = nil
		}
	}
	return out
}

func componentName(ref string) string {
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// TestContractCorpusRawFidelity pins the one place arbitrary JSON values must
// survive generated Go decoding byte-exact: tool metadata and raw objects.
func TestContractCorpusRawFidelity(t *testing.T) {
	corpus := loadContractCorpus(t)
	for _, entry := range corpus.RawFidelity {
		schemaRef := componentSchema(t, entry.Schema)
		if err := schemaRef.VisitJSON(decodeSchemaJSON(t, entry.JSON)); err != nil {
			t.Fatalf("%s raw fixture %s: schema rejected it: %v", entry.Schema, entry.JSON, err)
		}
		target, ok := contractGeneratedTypes[entry.Schema]
		if !ok {
			t.Fatalf("raw fixture schema %q has no generated Go target", entry.Schema)
		}
		value := target()
		if err := json.Unmarshal([]byte(entry.JSON), value); err != nil {
			t.Fatalf("%s raw fixture: generated decode: %v", entry.Schema, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("%s raw fixture: re-encode: %v", entry.Schema, err)
		}
		for _, want := range entry.Preserve {
			if !strings.Contains(string(encoded), want) {
				t.Fatalf("%s raw fixture re-encoded to %s, want %q preserved", entry.Schema, encoded, want)
			}
		}
	}
}

// TestContractCorpusSSEIdentity pins the stream identity rule: a revisioned
// event embeds its owner inside the revision object, transient progress
// inherits the authenticated connection and carries no identity, and no
// event accepts a duplicate top-level instance id.
func TestContractCorpusSSEIdentity(t *testing.T) {
	corpus := loadContractCorpus(t)

	revisioned := map[string]string{
		"ConfigurationChangedEvent": "configuration_revision",
		"SessionChangedEvent":       "session_revision",
		"WarningChangedEvent":       "warnings_revision",
	}
	for component, revisionMember := range revisioned {
		for _, fixture := range []struct{ name, json string }{
			{component, contractComponentFixture(t, corpus, component, "revisioned")},
			{"Event", contractComponentFixture(t, corpus, "Event", component)},
		} {
			var members map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fixture.json), &members); err != nil {
				t.Fatalf("%s fixture %q: %v", component, fixture.name, err)
			}
			if _, duplicate := members["instance_id"]; duplicate {
				t.Fatalf("%s fixture %q carries a top-level instance_id", component, fixture.name)
			}
			var revision map[string]json.RawMessage
			if err := json.Unmarshal(members[revisionMember], &revision); err != nil {
				t.Fatalf("%s fixture %q: revision member %q: %v", component, fixture.name, revisionMember, err)
			}
			if _, ok := revision["instance_id"]; !ok {
				t.Fatalf("%s fixture %q: revision %q has no instance_id", component, fixture.name, revisionMember)
			}
		}
		if err := componentSchema(t, component).VisitJSON(decodeSchemaJSON(t, contractComponentFixture(t, corpus, component, "duplicate-top-level-instance-id"))); err == nil {
			t.Fatalf("%s accepted a duplicate top-level instance_id", component)
		}
	}

	for _, component := range []string{"TextDeltaEvent", "ToolStartedEvent", "ToolFinishedEvent"} {
		fixture := contractComponentFixture(t, corpus, component, "progress")
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(fixture), &members); err != nil {
			t.Fatalf("%s progress fixture: %v", component, err)
		}
		if _, ok := members["instance_id"]; ok {
			t.Fatalf("%s progress fixture carries an instance identity", component)
		}
		if err := componentSchema(t, component).VisitJSON(decodeSchemaJSON(t, contractComponentFixture(t, corpus, component, "duplicate-top-level-instance-id"))); err == nil {
			t.Fatalf("%s accepted a duplicate top-level instance_id", component)
		}
	}
}

// contractGeneratedTypes maps every corpus component to its generated Go
// target; the coverage test proves the map and the corpus name the same set.
var contractGeneratedTypes = map[string]func() any{
	"Agent":                       func() any { return new(protocol.Agent) },
	"AgentMutation":               func() any { return new(protocol.AgentMutation) },
	"AssistantItem":               func() any { return new(protocol.AssistantItem) },
	"BackgroundMember":            func() any { return new(protocol.BackgroundMember) },
	"CodeRevertError":             func() any { return new(protocol.CodeRevertError) },
	"CodeRevertResult":            func() any { return new(protocol.CodeRevertResult) },
	"CodeSnapshotGroup":           func() any { return new(protocol.CodeSnapshotGroup) },
	"CompactRequest":              func() any { return new(protocol.CompactRequest) },
	"CompactionItem":              func() any { return new(protocol.CompactionItem) },
	"ConfigurationChangedEvent":   func() any { return new(protocol.ConfigurationChangedEvent) },
	"ConfigurationRevision":       func() any { return new(protocol.ConfigurationRevision) },
	"ConfigurationView":           func() any { return new(protocol.ConfigurationView) },
	"ConnectRequest":              func() any { return new(protocol.ConnectRequest) },
	"ContentPart":                 func() any { return new(protocol.ContentPart) },
	"ConversationItem":            func() any { return new(protocol.ConversationItem) },
	"ConversationPage":            func() any { return new(protocol.ConversationPage) },
	"Cost":                        func() any { return new(protocol.Cost) },
	"CreateProviderRequest":       func() any { return new(protocol.CreateProviderRequest) },
	"CreateSessionRequest":        func() any { return new(protocol.CreateSessionRequest) },
	"DeletionMutation":            func() any { return new(protocol.DeletionMutation) },
	"DiscoveredModelCandidate":    func() any { return new(protocol.DiscoveredModelCandidate) },
	"DiscoveryRequest":            func() any { return new(protocol.DiscoveryRequest) },
	"Error":                       func() any { return new(protocol.Error) },
	"Event":                       func() any { return new(protocol.Event) },
	"ForkRequest":                 func() any { return new(protocol.ForkRequest) },
	"ForkResult":                  func() any { return new(protocol.ForkResult) },
	"Health":                      func() any { return new(protocol.Health) },
	"HistoryPage":                 func() any { return new(protocol.HistoryPage) },
	"Hydration":                   func() any { return new(protocol.Hydration) },
	"ImageURLPart":                func() any { return new(protocol.ImageURLPart) },
	"InputItem":                   func() any { return new(protocol.InputItem) },
	"InputModality":               func() any { return new(protocol.InputModality) },
	"InputOrigin":                 func() any { return new(protocol.InputOrigin) },
	"JSONObject":                  func() any { return new(protocol.JSONObject) },
	"ModelEdit":                   func() any { return new(protocol.ModelEdit) },
	"ModelList":                   func() any { return new(protocol.ModelList) },
	"ModelListEntry":              func() any { return new(protocol.ModelListEntry) },
	"ModelMutation":               func() any { return new(protocol.ModelMutation) },
	"ModelRef":                    func() any { return new(protocol.ModelRef) },
	"ModelSource":                 func() any { return new(protocol.ModelSource) },
	"ModelUsage":                  func() any { return new(protocol.ModelUsage) },
	"ModelView":                   func() any { return new(protocol.ModelView) },
	"OpaquePart":                  func() any { return new(protocol.OpaquePart) },
	"Operation":                   func() any { return new(protocol.Operation) },
	"OperationEndItem":            func() any { return new(protocol.OperationEndItem) },
	"PendingInput":                func() any { return new(protocol.PendingInput) },
	"PendingQueues":               func() any { return new(protocol.PendingQueues) },
	"PendingSnapshot":             func() any { return new(protocol.PendingSnapshot) },
	"PluginConfigDocument":        func() any { return new(protocol.PluginConfigDocument) },
	"PluginsSettings":             func() any { return new(protocol.PluginsSettings) },
	"ProtocolMetadata":            func() any { return new(protocol.ProtocolMetadata) },
	"Provider":                    func() any { return new(protocol.Provider) },
	"ProviderDetail":              func() any { return new(protocol.ProviderDetail) },
	"ProviderEdit":                func() any { return new(protocol.ProviderEdit) },
	"ProviderList":                func() any { return new(protocol.ProviderList) },
	"ProviderModelList":           func() any { return new(protocol.ProviderModelList) },
	"ProviderMutation":            func() any { return new(protocol.ProviderMutation) },
	"ReadFileRequest":             func() any { return new(protocol.ReadFileRequest) },
	"ReadFileResult":              func() any { return new(protocol.ReadFileResult) },
	"RetainedRevertRequest":       func() any { return new(protocol.RetainedRevertRequest) },
	"RetainedTurn":                func() any { return new(protocol.RetainedTurn) },
	"RetainedTurns":               func() any { return new(protocol.RetainedTurns) },
	"RevertCodeRequest":           func() any { return new(protocol.RevertCodeRequest) },
	"RevisionResponse":            func() any { return new(protocol.RevisionResponse) },
	"Scope":                       func() any { return new(protocol.Scope) },
	"ScopeEvent":                  func() any { return new(protocol.ScopeEvent) },
	"Session":                     func() any { return new(protocol.Session) },
	"SessionChangedEvent":         func() any { return new(protocol.SessionChangedEvent) },
	"SessionLifecycle":            func() any { return new(protocol.SessionLifecycle) },
	"SessionRevision":             func() any { return new(protocol.SessionRevision) },
	"SessionsSettings":            func() any { return new(protocol.SessionsSettings) },
	"SetAgentTypeModelRequest":    func() any { return new(protocol.SetAgentTypeModelRequest) },
	"SetSessionAgentTypeRequest":  func() any { return new(protocol.SetSessionAgentTypeRequest) },
	"Settings":                    func() any { return new(protocol.Settings) },
	"SettingsMutation":            func() any { return new(protocol.SettingsMutation) },
	"SignalItem":                  func() any { return new(protocol.SignalItem) },
	"SkippedFile":                 func() any { return new(protocol.SkippedFile) },
	"SnapshotFile":                func() any { return new(protocol.SnapshotFile) },
	"SubmitRequest":               func() any { return new(protocol.SubmitRequest) },
	"SubmitResult":                func() any { return new(protocol.SubmitResult) },
	"SystemRole":                  func() any { return new(protocol.SystemRole) },
	"TextDeltaEvent":              func() any { return new(protocol.TextDeltaEvent) },
	"TextPart":                    func() any { return new(protocol.TextPart) },
	"ToolCallStatus":              func() any { return new(protocol.ToolCallStatus) },
	"ToolCallView":                func() any { return new(protocol.ToolCallView) },
	"ToolFinishedEvent":           func() any { return new(protocol.ToolFinishedEvent) },
	"ToolMetadata":                func() any { return new(protocol.ToolMetadata) },
	"ToolStartedEvent":            func() any { return new(protocol.ToolStartedEvent) },
	"UpdateProviderDetailRequest": func() any { return new(protocol.UpdateProviderDetailRequest) },
	"UpdateProviderModelRequest":  func() any { return new(protocol.UpdateProviderModelRequest) },
	"UpdateSettingsRequest":       func() any { return new(protocol.UpdateSettingsRequest) },
	"UsageContext":                func() any { return new(protocol.UsageContext) },
	"UsageCount":                  func() any { return new(protocol.UsageCount) },
	"UsageProjection":             func() any { return new(protocol.UsageProjection) },
	"UsageSnapshot":               func() any { return new(protocol.UsageSnapshot) },
	"UsageTotals":                 func() any { return new(protocol.UsageTotals) },
	"Warning":                     func() any { return new(protocol.Warning) },
	"WarningChangedEvent":         func() any { return new(protocol.WarningChangedEvent) },
	"WarningsRevision":            func() any { return new(protocol.WarningsRevision) },
	"WarningsSnapshot":            func() any { return new(protocol.WarningsSnapshot) },
	"Workspace":                   func() any { return new(protocol.Workspace) },
}
