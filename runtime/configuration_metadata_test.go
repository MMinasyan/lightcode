package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The metadata mutation suite: the retained provider/model editor operations
// run through the one validated configuration writer. Every row pins one
// operator × field × source axis of the semantic matrix over real file
// bytes, the captured catalog, generation, and event oracles.

// bundledCatalogModel returns one bundled model ID of the named provider,
// resolved from the runtime's published catalog.
func bundledCatalogModel(t *testing.T, r *Runtime, providerID string) string {
	t.Helper()
	for id, m := range r.config.current().catalog.Providers[providerID].Models {
		if m.Source == catalog.SourceBundled {
			return id
		}
	}
	t.Fatalf("no bundled model on provider %q in the published catalog", providerID)
	return ""
}

// metadataBundledFS is the metadata suite's bundled catalog: one builtin
// provider with bundled attribution headers and a bundled model (discovery
// disabled — a captured build never attempts a network fetch), one
// discovery-enabled builtin whose credential is never set, so its refresh
// readiness fails before any network attempt while a seeded transport-bound
// discovery record still supplies a discovered model, and an openrouter
// fixture whose bundled headers the retained strip rule reads from the real
// embedded catalog.
func metadataBundledFS() fstest.MapFS {
	return fstest.MapFS{
		"builtin/bstatic.json": {Data: []byte(`{
			"id": "bstatic",
			"transport": {"base_url": "http://bstatic.test/v1", "api_key_env": "", "headers": {"HTTP-Referer": "https://lightcode.test", "X-Title": "Lightcode"}},
			"discovery": false,
			"models": {"bm": {"context_window": 1000}}
		}`)},
		"builtin/disco.json": {Data: []byte(`{
			"id": "disco",
			"transport": {"base_url": "https://disco.test/v1", "api_key_env": "DISCO_TEST_KEY"},
			"models": {"bm": {"context_window": 1000}}
		}`)},
		"builtin/openrouter.json": {Data: []byte(`{
			"id": "openrouter",
			"transport": {"base_url": "https://openrouter.test/v1", "api_key_env": "", "headers": {"HTTP-Referer": "https://lightcode.test", "X-Title": "Lightcode"}},
			"discovery": false,
			"models": {"om": {"context_window": 1000}}
		}`)},
	}
}

// metadataConfigDocument is the metadata fixture: two custom providers with
// an exact big-integer lexeme in prov's extra_body, and one unowned root
// member. other occupies the OTHER_TEST_KEY env name.
const metadataConfigDocument = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://prov.test/v1", "api_key_env": "META_TEST_KEY", "headers": {"X-Trace": "t1"}},
      "discovery": false,
      "extra_body": {"side": 1, "big": 9007199254740993},
      "models": {
        "m": {"name": "M", "context_window": 4096, "max_output_tokens": 100, "usage_in_stream": false},
        "wide": {"name": "W", "context_window": 8192}
      }
    },
    "other": {"transport": {"base_url": "https://other.test/v1", "api_key_env": "OTHER_TEST_KEY"}, "discovery": false, "models": {"o": {"context_window": 9007199254740993}}}
  },
  "custom_flag": true
}`

func newMetadataHarness(t *testing.T) *serviceHarness {
	t.Helper()
	h := newServiceHarness(t)
	h.loader = catalog.NewLoader(h.home, metadataBundledFS())
	return h
}

// metadataBuiltinOverrideDocument is the reset-test fixture: the metadata
// document plus a builtin whose raw user layer carries base_url and
// api_key_env overrides the resets must remove (the env override's
// credential is never set, so the builtin stays disconnected).
func metadataBuiltinOverrideDocument() string {
	return strings.Replace(metadataConfigDocument,
		`"other": {`,
		`"bstatic": {"transport": {"base_url": "https://override.test/v1", "api_key_env": "BUILTIN_OVERRIDE_KEY"}}, "other": {`, 1)
}

// metadataService publishes the metadata fixture once and returns the
// subscribed service with its attached warning store.
func metadataService(t *testing.T, h *serviceHarness) (*configurationService, *Subscription) {
	t.Helper()
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	svc.attachWarnings(newWarningStore())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(32)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	return svc, sub
}

// assertMetadataRefused pins one refused edit: the typed (or invalid-class)
// failure, byte-identical owning file, unchanged publication pointer and
// generation, and event silence.
func assertMetadataRefused(t *testing.T, svc *configurationService, sub *Subscription, configPath string, before []byte, first *configuration, warnRev uint64, err error, want error) {
	t.Helper()
	testassert.Require(t, err != nil, "metadata edit succeeded, want a refusal")
	testassert.Require(t, want == nil || errors.Is(err, want), "refusal = %v, want %v", err, want)
	after, rerr := os.ReadFile(configPath)
	testassert.Require(t, rerr == nil && string(before) == string(after), "a refused metadata edit changed the owning file (%v)", rerr)
	if svc.current() != first {
		t.Fatalf("a refused metadata edit replaced the publication (generation %d)", svc.current().generation)
	}
	{
		got, _ := svc.warnings.snapshot()
		testassert.Equal(t, got, warnRev)
	}
	assertNoEvent(t, sub)
}

// metadataBaseline reads the fixture file bytes, the published pointer, and
// the warning revision before a refusal assertion.
func metadataBaseline(t *testing.T, svc *configurationService, configPath string) ([]byte, *configuration, uint64) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	testassert.NoError(t, err)
	warnRev, _ := svc.warnings.snapshot()
	return data, svc.current(), warnRev
}

// drainMutationEvent consumes exactly the one event a successful edit
// publishes, so a later refusal's silence assertion observes a quiet stream.
// The one global warning event a warning-changing edit additionally requires
// is tolerated only by callers that drain it explicitly.
func drainMutationEvent(t *testing.T, sub *Subscription, generation string) {
	t.Helper()
	nextConfigurationEvent(t, sub, generation)
}

// bigLexeme decodes one raw JSON member's exact number lexeme.
func bigLexeme(t *testing.T, data []byte, path string) string {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	testassert.NoError(t, decoder.Decode(&value))
	current := any(value)
	for _, member := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		testassert.Require(t, ok, "member path %q is not an object at %q", path, member)
		current = object[member]
	}
	number, ok := current.(json.Number)
	testassert.Require(t, ok, "member path %q = %v, want an exact json.Number lexeme", path, current)
	return number.String()
}

// --- create ---

// TestMetadataEditCreateCustomProvider proves the create operator: the new
// trimmed nonempty ID with its provided members and models lands in the
// latest raw layer, the candidate publishes with exactly one generation and
// event, unowned members and numeric lexemes survive, the created models are
// user-sourced with the incomplete one admitted, and no secret value is ever
// written — the create carries only a non-secret env name.
func TestMetadataEditCreateCustomProvider(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	writeServiceFile(t, agentsPath, workerAgentsDocument)
	agentsBefore := mustRead(t, agentsPath)
	svc, sub := metadataService(t, h)
	unsetenv(t, "NEW_KEY")
	// The fixture is a genuine credential consumer: the referenced env
	// carries an actual secret that must never reach the file, the mutation
	// result, or any read — the create stores the env NAME only.
	t.Setenv("NEW_KEY", "sk-real-credential-98765")
	window := 4096
	cost := protocol.Cost{Input: ptrTo(1.5)}
	meta := protocol.ProtocolMetadata{Family: ptrTo("testfam")}
	models := map[string]protocol.ModelEdit{
		"m":          {Name: ptrTo("One"), ContextWindow: &window, Cost: &cost},
		"incomplete": {Name: ptrTo("I")},
		"a/b":        {ContextWindow: &window},
	}
	patch := protocol.ProviderEdit{
		Name:             ptrTo("Created"),
		BaseUrl:          ptrTo("https://new.test/v1"),
		ApiKeyEnv:        ptrTo("NEW_KEY"),
		Headers:          &map[string]string{"X-Custom": "c"},
		SystemRole:       ptrTo(protocol.SystemRole("user")),
		UsageInStream:    ptrTo(false),
		ExtraBody:        &map[string]any{"side": json.Number("9007199254740993")},
		Discovery:        ptrTo(false),
		ProtocolMetadata: &meta,
	}
	candidate, err := svc.mutate(context.Background(), svc.editProviderCreate(" new ", patch, models, nil))
	testassert.NoError(t, err)
	testassert.Require(t, candidate.snapshot.generation == 2 && svc.current() == candidate.snapshot, "create = generation %d, want 2 published", candidate.snapshot.generation)
	prov := candidate.snapshot.catalog.Providers["new"]
	testassert.Require(t, prov != nil && !prov.Builtin, "created provider = %+v, want a non-builtin catalog member", prov)
	testassert.Require(t, prov.Transport.BaseURL == "https://new.test/v1" && prov.Transport.APIKeyEnv == "NEW_KEY" && prov.Name == "Created" && prov.SystemRole == catalog.SystemRole("user") && !prov.UsageInStream && !prov.Discovery, "created provider = %+v, want the provided members", prov)
	if usableModelCount(prov) != 2 {
		t.Fatalf("created usable models = %d, want the two usable entries", usableModelCount(prov))
	}
	entry := prov.Models["a/b"]
	testassert.Require(t, entry != nil && entry.Source == catalog.SourceUser && entry.Name == "a/b", "slash model = %+v, want a user-sourced entry named by its ID", entry)
	drainMutationEventWithWarning(t, sub, "2", svc.warnings)

	// The raw file: the written members, an untouched unowned member, the
	// untouched sibling's exact numeric lexeme, and no secret value anywhere.
	data := mustRead(t, h.configPath)
	var root map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(data, &root))
	testassert.Equal(t, bigLexeme(t, data, "providers.other.models.o.context_window"), "9007199254740993")
	testassert.Require(t, strings.Contains(string(root["providers"]), `"NEW_KEY"`), "created transport = %s, want the named env binding", root["providers"])
	// The owning file and the generated mutation result carry the env name
	// and never the referenced secret value.
	testassert.Require(t, !strings.Contains(string(data), "sk-real-credential-98765"), "the create wrote secret bytes: %s", data)
	mutationBytes, err := json.Marshal(protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(candidate.snapshot),
		Result:                providerPostState(candidate, "new"),
	})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(mutationBytes), "NEW_KEY") && !strings.Contains(string(mutationBytes), "sk-real-credential-98765"), "mutation result carries the wrong credential bytes: %s", mutationBytes)
	testassert.Equal(t, bigLexeme(t, data, "providers.new.extra_body.side"), "9007199254740993")
	testassert.Require(t, string(mustRead(t, agentsPath)) == string(agentsBefore), "the metadata edit changed the agents file")

	// The provided empty headers map goes through the same wholesale writer
	// as the update: the raw headers member stays absent, not an empty
	// object.
	emptyHeaders := map[string]string{}
	window2 := 1
	{
		_, err := svc.mutate(context.Background(), svc.editProviderCreate("noheaders",
			protocol.ProviderEdit{BaseUrl: ptrTo("https://nh.test/v1"), Headers: &emptyHeaders},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window2}}, nil))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "3")
	createdProvider := metadataRawProvider(t, h.configPath, "noheaders")
	var createdTransport map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(createdProvider["transport"], &createdTransport))
	{
		_, present := createdTransport["headers"]
		testassert.Require(t, !present, "created transport = %s, want the empty provided headers member absent", createdProvider["transport"])
	}
}

// TestMetadataEditCreateRefusals pins the create's nearest forbidden
// siblings: every refusal happens before the owning write, leaving the file,
// publication, generation, and event stream untouched. Catalog-valid
// incomplete metadata — no usable model, an empty models map, an occupied or
// absent binding without a supplied key — is representable and covered by
// the create positives, not refused here.
func TestMetadataEditCreateRefusals(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	base := "https://new.test/v1"
	window := 1
	valid := map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}

	rows := []struct {
		name   string
		id     string
		patch  protocol.ProviderEdit
		models map[string]protocol.ModelEdit
		want   error
	}{
		{"empty id", "   ", protocol.ProviderEdit{BaseUrl: &base}, valid, harness.ErrInvalid},
		{"slash id", "a/b", protocol.ProviderEdit{BaseUrl: &base}, valid, ErrConfiguration},
		{"missing base_url", "new", protocol.ProviderEdit{}, valid, harness.ErrInvalid},
		{"duplicate normalized model", "new", protocol.ProviderEdit{BaseUrl: &base},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}, " m ": {ContextWindow: &window}}, harness.ErrInvalid},
		{"empty model id", "new", protocol.ProviderEdit{BaseUrl: &base},
			map[string]protocol.ModelEdit{"  ": {ContextWindow: &window}}, harness.ErrInvalid},
		{"credential header Authorization", "new",
			protocol.ProviderEdit{BaseUrl: &base, Headers: &map[string]string{"authorization": "Bearer x"}}, valid, harness.ErrInvalid},
		{"credential header padded Proxy-Authorization", "new",
			protocol.ProviderEdit{BaseUrl: &base, Headers: &map[string]string{"  Proxy-Authorization  ": "x"}}, valid, harness.ErrInvalid},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := svc.mutate(context.Background(), svc.editProviderCreate(row.id, row.patch, row.models, nil))
			assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, row.want)
		})
	}
	// The duplicate axes: a raw-only external definition is not new either.
	// The immutable baseline is taken AFTER the external write — latest
	// bytes, published pointer, generation, warning revision, and events.
	external := strings.Replace(metadataConfigDocument, `"prov": {`, `"ghost": {"transport": {"base_url": "https://ghost.test/v1"}, "models": {"g": {"context_window": 1}}}, "prov": {`, 1)
	writeServiceFile(t, h.configPath, external)
	dupBefore, dupFirst, dupWarnRev := metadataBaseline(t, svc, h.configPath)
	_, dupErr := svc.mutate(context.Background(), svc.editProviderCreate("ghost", protocol.ProviderEdit{BaseUrl: &base}, valid, nil))
	assertMetadataRefused(t, svc, sub, h.configPath, dupBefore, dupFirst, dupWarnRev, dupErr, harness.ErrInvalid)
}

// TestMetadataEditCreateRegistersIncompleteMetadata pins the
// candidate-validity rule: an editor-only usable-model count and an
// env-reference uniqueness guard do not govern metadata creation, so a
// zero-window model, an empty models map, and a binding another provider
// references (or no binding at all) register without a credential action.
func TestMetadataEditCreateRegistersIncompleteMetadata(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	base := "https://new.test/v1"
	window := 1
	zeroWindow := map[string]protocol.ModelEdit{"m": {}}
	empty := map[string]protocol.ModelEdit{}

	generation := 1
	for _, row := range []struct {
		name   string
		id     string
		patch  protocol.ProviderEdit
		models map[string]protocol.ModelEdit
	}{
		{"zero-window model", "incomplete", protocol.ProviderEdit{BaseUrl: &base}, zeroWindow},
		{"empty models map", "nomodels", protocol.ProviderEdit{BaseUrl: &base}, empty},
		{"occupied binding", "occupiedp", protocol.ProviderEdit{BaseUrl: &base, ApiKeyEnv: ptrTo("OTHER_TEST_KEY")}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}},
		{"absent binding", "missingp", protocol.ProviderEdit{BaseUrl: &base, ApiKeyEnv: ptrTo("MISSING_PROBE_KEY")}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}},
		{"no binding member", "nobinding", protocol.ProviderEdit{BaseUrl: &base}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The metadata service runs without a managed-env manager: a
			// create that planned any key action would fail the manager's
			// typed refusal, so the row's success itself proves no
			// credential action was planned.
			candidate, err := svc.mutate(context.Background(), svc.editProviderCreate(row.id, row.patch, row.models, nil))
			testassert.Require(t, err == nil, "create %s: %v", row.id, err)
			generation++
			testassert.Require(t, candidate.snapshot.generation == uint64(generation), "create generation = %d, want %d", candidate.snapshot.generation, generation)
			drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
			created := candidate.snapshot.catalog.Providers[row.id]
			testassert.Require(t, created != nil, "created provider %s missing from the candidate", row.id)
			// The real binding truth: the created transport carries exactly
			// the supplied binding — empty for the keyless shapes — never an
			// invented one.
			var transport struct {
				APIKeyEnv string `json:"api_key_env"`
			}
			testassert.NoError(t, json.Unmarshal(metadataRawProvider(t, h.configPath, row.id)["transport"], &transport))
			wantEnv := ""
			if row.patch.ApiKeyEnv != nil {
				wantEnv = *row.patch.ApiKeyEnv
			}
			testassert.Require(t, transport.APIKeyEnv == wantEnv && created.Transport.APIKeyEnv == wantEnv, "%s binding = (raw %q, effective %q), want exactly the supplied %q", row.id, transport.APIKeyEnv, created.Transport.APIKeyEnv, wantEnv)
		})
	}
}

// --- update ---

// TestMetadataEditUpdateCustomProviderPatchSemantics pins the patch rules on
// a custom provider: pointer absence leaves every raw member untouched,
// provided members replace their whole value (0, false, and empty objects
// included where the validators allow), the headers member is written
// wholesale so an empty object deletes the raw user headers, and an invalid
// provided value refuses through the candidate check without a write.
func TestMetadataEditUpdateCustomProviderPatchSemantics(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	unsetenv(t, "META_TEST_KEY", "NEW_TEST_KEY") // prov is not connected

	// Pointer absence: an empty patch still publishes (identical bytes
	// included) but changes no raw member.
	unchanged := mustRead(t, h.configPath)
	candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{}))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 2, "empty patch = (%v, generation %d), want a published edit at 2", err, candidate.snapshot.generation)
	var beforeRoot, afterRoot map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(unchanged, &beforeRoot))
	testassert.NoError(t, json.Unmarshal(mustRead(t, h.configPath), &afterRoot))
	var beforeValue, afterValue any
	beforeDecoder := json.NewDecoder(strings.NewReader(string(beforeRoot["providers"])))
	beforeDecoder.UseNumber()
	afterDecoder := json.NewDecoder(strings.NewReader(string(afterRoot["providers"])))
	afterDecoder.UseNumber()
	testassert.NoError(t, beforeDecoder.Decode(&beforeValue))
	testassert.NoError(t, afterDecoder.Decode(&afterValue))
	testassert.Require(t, reflect.DeepEqual(beforeValue, afterValue), "the empty patch changed the raw providers member")
	drainMutationEvent(t, sub, "2")

	// Wholesale replaces: false and an empty extra_body object are written
	// as provided — the validators allow them.
	falseValue := false
	emptyObject := map[string]any{}
	{
		candidate, err = svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{
			UsageInStream: &falseValue,
			ExtraBody:     &emptyObject,
		}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "3")
	rawProvider := metadataRawProvider(t, h.configPath, "prov")
	var headerProbe struct {
		Headers map[string]string `json:"headers"`
	}
	testassert.NoError(t, json.Unmarshal(rawProvider["transport"], &headerProbe))
	var extraBody map[string]any
	testassert.NoError(t, json.Unmarshal(rawProvider["extra_body"], &extraBody))
	testassert.Require(t, extraBody != nil && len(extraBody) == 0 && headerProbe.Headers["X-Trace"] == "t1", "extra_body = %v, want the provided empty object; headers = %v, want the absent member untouched", extraBody, headerProbe.Headers)

	// The headers exception: an empty provided object deletes the raw user
	// headers member — the file, not merely the filtered GET.
	emptyHeaders := map[string]string{}
	{
		candidate, err = svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{Headers: &emptyHeaders}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "4")
	rawProvider = metadataRawProvider(t, h.configPath, "prov")
	var transport map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(rawProvider["transport"], &transport))
	{
		_, present := transport["headers"]
		testassert.Require(t, !present, "raw transport = %s, want the headers member deleted", rawProvider["transport"])
	}

	// The wholesale header replacement: a provided map replaces the whole
	// raw value, so the headers member is exactly the provided map.
	replaced := map[string]string{"X-New": "n"}
	{
		candidate, err = svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{Headers: &replaced}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "5")
	transport = nil
	rawProvider = metadataRawProvider(t, h.configPath, "prov")
	testassert.NoError(t, json.Unmarshal(rawProvider["transport"], &transport))
	var headersMember map[string]string
	testassert.NoError(t, json.Unmarshal(transport["headers"], &headersMember))
	testassert.Require(t, len(headersMember) == 1 && headersMember["X-New"] == "n", "raw headers = %s, want the wholesale provided map", transport["headers"])

	// The malformed field value refuses through the candidate check without
	// a write (the credential-header refusal row is pinned at the direct
	// Runtime entrypoint).
	bogusRole := protocol.SystemRole("assistant")
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	{
		_, err = svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{SystemRole: &bogusRole}))
		testassert.Require(t, err != nil && errors.Is(err, ErrConfiguration), "malformed system role = %v, want a rejected candidate", err)
	}
	// The existing state oracle: both refusals left the file, the
	// publication, warning revision, and event stream untouched.
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	// The untouched unowned members and numeric lexemes survive every write.
	testassert.Equal(t, bigLexeme(t, mustRead(t, h.configPath), "providers.other.models.o.context_window"), "9007199254740993")
	testassert.Equal(t, string(fileRoot(t, h.configPath)["custom_flag"]), "true")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	testassert.Require(t, err == nil, "read %s: %v", path, err)
	return data
}

// TestMetadataEditUpdateEnvRules pins the api_key_env axes on a custom
// provider under the user-layer rule: no connection state or sibling
// reference constrains the rebinding — a connected provider rebinds and its
// old credential stays untouched — and the supplied binding is the whole
// value: an invalid padded or non-whitespace identifier refuses through the
// candidate check exactly like a hand-edited file. No other field or
// identity is normalized.
func TestMetadataEditUpdateEnvRules(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	unsetenv(t, "META_TEST_KEY", "NEW_TEST_KEY")
	t.Setenv("META_TEST_KEY", "secret-value") // prov is connected via its env key

	// The connected rebindings land: a different nonempty name and a clear
	// to keyless are both user-layer edits, and the referenced credential
	// itself is never unset.
	newEnv := "NEW_TEST_KEY"
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{ApiKeyEnv: &newEnv}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "2")
	testassert.Equal(t, svc.current().catalog.Providers["prov"].Transport.APIKeyEnv, "NEW_TEST_KEY")
	if os.Getenv("META_TEST_KEY") != "secret-value" {
		t.Fatalf("the rebinding unset the old credential: %q", os.Getenv("META_TEST_KEY"))
	}

	// The supplied binding is the whole value: a padded invalid env name
	// refuses through the candidate check with the owning file and
	// publication untouched — the same governance a hand-edited padded
	// file gets.
	padded := "  NEW_TEST_KEY  "
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{ApiKeyEnv: &padded}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	// An invalid non-whitespace identifier is catalog-rejected the same
	// way.
	bogus := "NOT VALID!"
	_, err = svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{ApiKeyEnv: &bogus}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	// The clear to keyless lands.
	clear := ""
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{ApiKeyEnv: &clear}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "3")
	testassert.Equal(t, svc.current().catalog.Providers["prov"].Transport.APIKeyEnv, "")
	if os.Getenv("NEW_TEST_KEY") != "" {
		t.Fatalf("the clear unset the previously referenced credential: %q", os.Getenv("NEW_TEST_KEY"))
	}
}

// TestMetadataEditUpdateBuiltinProvider pins the user-layer rule on a
// builtin: every ProviderField member lands as the bundled provider's user
// override — presence of an equal value included — the wholesale headers
// write carries bundled-collision keys exactly as provided (no source-based
// strip), and the credential-header refusal and the reserved extra_body
// candidate refusal stay.
func TestMetadataEditUpdateBuiltinProvider(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)

	// Every non-transport field lands on the builtin's user override.
	generation := 1
	for _, row := range []struct {
		name  string
		patch protocol.ProviderEdit
		check func(*catalog.Provider)
	}{
		{"name", protocol.ProviderEdit{Name: ptrTo("Renamed")}, func(p *catalog.Provider) {
			testassert.Equal(t, p.Name, "Renamed")
		}},
		{"base_url", protocol.ProviderEdit{BaseUrl: ptrTo("https://override.test/v1")}, func(p *catalog.Provider) {
			testassert.Equal(t, p.Transport.BaseURL, "https://override.test/v1")
		}},
		{"system_role", protocol.ProviderEdit{SystemRole: ptrTo(protocol.SystemRole("user"))}, func(p *catalog.Provider) {
			testassert.Require(t, p.SystemRole == catalog.SystemRole("user"), "builtin system_role = %q, want the override", p.SystemRole)
		}},
		{"usage_in_stream", protocol.ProviderEdit{UsageInStream: ptrTo(false)}, func(p *catalog.Provider) {
			testassert.Require(t, !p.UsageInStream, "builtin usage_in_stream = true, want the override")
		}},
		{"protocol_metadata", protocol.ProviderEdit{ProtocolMetadata: &protocol.ProtocolMetadata{Family: ptrTo("fam")}}, func(p *catalog.Provider) {
			testassert.Require(t, p.ProtocolMetadata != nil && p.ProtocolMetadata.Family == "fam", "builtin protocol_metadata = %+v, want the override", p.ProtocolMetadata)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", row.patch))
			testassert.Require(t, err == nil, "builtin %s patch: %v", row.name, err)
			generation++
			testassert.Require(t, candidate.snapshot.generation == uint64(generation), "builtin %s patch generation = %d, want %d", row.name, candidate.snapshot.generation, generation)
			drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
			row.check(candidate.snapshot.catalog.Providers["bstatic"])
		})
	}

	// The wholesale headers write carries bundled-collision keys exactly as
	// provided: a leaked raw override and a provided collision key both land
	// in the file (the bundled headers come from the real embedded catalog;
	// nothing strips them anywhere).
	external := strings.Replace(metadataConfigDocument,
		`"other": {`,
		`"bstatic": {"transport": {"headers": {"x-title": "leak", "X-Keep": "k"}}}, "other": {`, 1)
	writeServiceFile(t, h.configPath, external)
	headers := map[string]string{"X-Custom": "c"}
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{Headers: &headers}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	var transport struct {
		Headers map[string]string `json:"headers"`
	}
	rawProviders := map[string]map[string]json.RawMessage{}
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	testassert.NoError(t, json.Unmarshal(rawProviders["bstatic"]["transport"], &transport))
	testassert.Require(t, len(transport.Headers) == 1 && transport.Headers["X-Custom"] == "c", "builtin raw headers = %v, want the wholesale provided map", transport.Headers)
	colliding := map[string]string{"x-title": "user", "X-Other": "o"}
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("openrouter", protocol.ProviderEdit{Headers: &colliding}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	transport.Headers = nil
	testassert.NoError(t, json.Unmarshal(rawProviders["openrouter"]["transport"], &transport))
	testassert.Require(t, len(transport.Headers) == 2 && transport.Headers["x-title"] == "user" && transport.Headers["X-Other"] == "o", "builtin raw headers = %v, want the collision key stored as provided", transport.Headers)

	// The forbidden credential header is refused on a builtin too.
	forbidden := map[string]string{"Proxy-Authorization": "x"}
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	_, credErr := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{Headers: &forbidden}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, credErr, harness.ErrInvalid)

	// A reserved extra_body key on a writable builtin field refuses through
	// the candidate check: no target drop, no write, unchanged published
	// catalog.
	reserved := map[string]any{"model": "x"}
	before, first, warnRev = metadataBaseline(t, svc, h.configPath)
	_, reservedErr := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{ExtraBody: &reserved}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, reservedErr, ErrConfiguration)
	testassert.Require(t, svc.current().catalog.Providers["bstatic"] != nil, "the refused edit dropped the builtin from the published catalog")
}

// --- delete ---

// TestMetadataEditDeleteProvider pins the delete axes under the user-layer
// rule: every addressed identity loses only its user node — a builtin's
// override removal reveals the bundled base, a connected env-keyed custom is
// removable, an identity whose raw node an external editor already removed
// still publishes — and the unknown subject fails without a write.
func TestMetadataEditDeleteProvider(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataBuiltinOverrideDocument())
	svc, sub := metadataService(t, h)

	// The builtin delete removes only the user node: the bundled base
	// remains the effective subject.
	candidate, err := svc.mutate(context.Background(), svc.editProviderDelete("bstatic"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 2, "builtin delete = (%v, generation %d), want a publication at 2", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "2")
	{
		got := candidate.snapshot.catalog.Providers["bstatic"]
		testassert.Require(t, got != nil && got.Builtin, "builtin delete post-state = %+v, want the surviving bundled base", got)
	}
	testassert.Equal(t, candidate.snapshot.catalog.Providers["bstatic"].Transport.BaseURL, "http://bstatic.test/v1")
	var rawProviders map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	if _, present := rawProviders["bstatic"]; present {
		t.Fatalf("raw providers = %s, want the user node gone", fileRoot(t, h.configPath)["providers"])
	}
	{
		_, present := rawProviders["prov"]
		testassert.Require(t, present, "the delete touched an unrelated provider")
	}

	// The connected env-keyed custom is removable too.
	unsetenv(t, "META_TEST_KEY")
	t.Setenv("META_TEST_KEY", "secret-value")
	candidate, err = svc.mutate(context.Background(), svc.editProviderDelete("prov"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 3, "connected delete = (%v, generation %d), want a publication at 3", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "3")
	testassert.Require(t, candidate.snapshot.catalog.Providers["prov"] == nil, "the deleted custom provider stayed in the candidate catalog")
	if os.Getenv("META_TEST_KEY") != "secret-value" {
		t.Fatalf("the delete unset the referenced credential: %q", os.Getenv("META_TEST_KEY"))
	}
	_ = os.Unsetenv("META_TEST_KEY")

	// The successful standalone delete removes only the owning definition.
	candidate, err = svc.mutate(context.Background(), svc.editProviderDelete("other"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 4, "delete = (%v, generation %d), want a publication at 4", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "4")
	rawProviders = map[string]json.RawMessage{}
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	if _, present := rawProviders["other"]; present {
		t.Fatalf("raw providers = %s, want the deleted entry gone", fileRoot(t, h.configPath)["providers"])
	}
	testassert.Equal(t, string(fileRoot(t, h.configPath)["custom_flag"]), "true")

	// A custom definition deleted from the latest raw layer by an external
	// editor (without a reload) is not reconstructed: the identity is still
	// effective, so the delete succeeds, publishes, and leaves no subject —
	// the post-state is null, and the raw file is unchanged apart from the
	// shared writer's own formatting.
	window := 1
	valid := map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}
	{
		_, err := svc.mutate(context.Background(), svc.editProviderCreate("temp",
			protocol.ProviderEdit{BaseUrl: ptrTo("https://temp.test/v1")}, valid, nil))
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, svc.warnings, sub, "5")
	external := `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "META_TEST_KEY", `{"m":{"name":"M","context_window":4096}}`) + `},"custom_flag":true}`
	writeServiceFile(t, h.configPath, external)
	candidate, err = svc.mutate(context.Background(), svc.editProviderDelete("temp"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 6, "externally-removed delete = (%v, generation %d), want a publication at 6", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "6")
	testassert.Require(t, candidate.snapshot.catalog.Providers["temp"] == nil, "the externally-removed provider stayed in the candidate catalog")

	// The unknown subject fails its typed not-found without a write.
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	_, ghostErr := svc.mutate(context.Background(), svc.editProviderDelete("ghost"))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, ghostErr, catalog.ErrUnknownProvider)
}

// --- provider field reset ---

// TestMetadataEditResetProviderFields pins the provider reset matrix: every
// shared field removes exactly its own user override from its owning raw
// path and restores the effective default, a custom provider's required
// transport keys refuse through the candidate check (a connected reset
// included — no connection state gates the reset), resetting hidden removes
// its override, a builtin's fixture-seeded base_url and disconnected
// api_key_env overrides restore their bundled values with only their own raw
// member changed, and a reset with no override left still rewrites the
// owning file and publishes the next generation under the one shared
// successful-edit rule.
func TestMetadataEditResetProviderFields(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataBuiltinOverrideDocument())
	svc, sub := metadataService(t, h)

	// Seed one raw override per non-transport field via the update edit.
	name := "Renamed"
	extra := map[string]any{"x": 1}
	stream := false
	role := protocol.SystemRole("developer")
	meta := protocol.ProtocolMetadata{Family: ptrTo("fam")}
	discovery := false
	hidden := true
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{
			Name: &name, ExtraBody: &extra, SystemRole: &role, UsageInStream: &stream,
			ProtocolMetadata: &meta, Discovery: &discovery, Hidden: &hidden,
		}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "2")

	// Non-transport resets remove exactly their own raw member.
	generation := 2
	for _, field := range []protocol.ProviderField{
		protocol.ProviderFieldName,
		protocol.ProviderFieldExtraBody,
		protocol.ProviderFieldSystemRole,
		protocol.ProviderFieldUsageInStream,
		protocol.ProviderFieldProtocolMetadata,
		protocol.ProviderFieldDiscovery,
		protocol.ProviderFieldHidden,
	} {
		candidate, err := svc.mutate(context.Background(), svc.editProviderFieldReset("prov", field))
		testassert.Require(t, err == nil && candidate.snapshot.generation == svc.current().generation, "reset %q = (%v, generation %d), want a publication", field, err, candidate.snapshot.generation)
		generation++
		drainMutationEvent(t, sub, strconv.Itoa(generation))
		prov := metadataRawProvider(t, h.configPath, "prov")
		{
			_, present := prov[string(field)]
			testassert.Require(t, !present, "reset %q left the raw override: %s", field, prov)
		}
	}

	// Transport resets remove exactly their own transport member.
	for _, field := range []protocol.ProviderField{
		protocol.ProviderFieldHeaders,
	} {
		{
			_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("prov", field))
			testassert.Require(t, err == nil, "reset %q: %v", field, err)
		}
		generation++
		drainMutationEvent(t, sub, strconv.Itoa(generation))
		rawProvider := metadataRawProvider(t, h.configPath, "prov")
		var transport map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(rawProvider["transport"], &transport))
		{
			_, present := transport[string(field)]
			testassert.Require(t, !present, "reset %q left the raw transport override: %s", field, rawProvider["transport"])
		}
	}

	// The required custom transport keys fail the candidate validation
	// before the write — a connected provider's api_key_env reset included,
	// since no connection state gates the reset.
	t.Setenv("META_TEST_KEY", "secret-value")
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	for _, field := range []protocol.ProviderField{
		protocol.ProviderFieldBaseUrl,
		protocol.ProviderFieldEnvironmentVariable,
	} {
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("prov", field))
		assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)
	}
	_ = os.Unsetenv("META_TEST_KEY")

	// The builtin base_url override (from the fixture's raw user layer)
	// resets to the bundled URL: only its own raw member changes.
	unsetenv(t, "BUILTIN_OVERRIDE_KEY") // the builtin env override is disconnected
	builtinBefore := metadataRawProvider(t, h.configPath, "bstatic")
	{
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("bstatic", protocol.ProviderFieldBaseUrl))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	testassert.Equal(t, svc.current().catalog.Providers["bstatic"].Transport.BaseURL, "http://bstatic.test/v1")
	var beforeTransport, afterTransport map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(builtinBefore["transport"], &beforeTransport))
	testassert.NoError(t, json.Unmarshal(metadataRawProvider(t, h.configPath, "bstatic")["transport"], &afterTransport))
	{
		_, present := beforeTransport["base_url"]
		testassert.Require(t, present, "fixture base_url override missing")
	}
	if _, present := afterTransport["base_url"]; present {
		t.Fatalf("raw transport = %s, want the base_url override gone", metadataRawProvider(t, h.configPath, "bstatic")["transport"])
	}
	testassert.Require(t, len(afterTransport) == len(beforeTransport)-1, "the reset changed members beyond its own field: %v → %v", beforeTransport, afterTransport)
	{
		_, present := afterTransport["api_key_env"]
		testassert.Require(t, present, "the reset touched the env override")
	}

	// The builtin DISCONNECTED api_key_env override resets to the bundled
	// empty name: no connection state gates the reset.
	{
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("bstatic", protocol.ProviderFieldEnvironmentVariable))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	testassert.Equal(t, svc.current().catalog.Providers["bstatic"].Transport.APIKeyEnv, "")
	{
		got := metadataRawProvider(t, h.configPath, "bstatic")["transport"]
		testassert.Require(t, got != nil, "the builtin transport member vanished")
	}

	// A second base_url reset has no override left: the shared successful
	// edit rule still rewrites the owning file and publishes the next
	// generation with its event.
	{
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("bstatic", protocol.ProviderFieldBaseUrl))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))

	// An absent override is the same successful edit: no member to remove,
	// yet the owning file is rewritten and the next generation published.
	{
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("prov", protocol.ProviderFieldName))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))

	// The builtin reset removes its own user override and restores the
	// bundled value; a builtin with no raw entry follows the same shared
	// successful-edit rule.
	userHeaders := map[string]string{"X-Custom": "c"}
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{Headers: &userHeaders}))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	{
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("bstatic", protocol.ProviderFieldHeaders))
		testassert.NoError(t, err)
	}
	bundled := svc.current().catalog.Providers["bstatic"].Transport.Headers
	testassert.Require(t, bundled["HTTP-Referer"] != "" && bundled["X-Title"] != "" && len(bundled) == 2, "builtin reset headers = %v, want the bundled attribution headers restored", bundled)
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
}

// --- model upsert ---

// TestMetadataEditModelUpsert pins the model PUT axes: a user model patch
// merges its provided members onto the prior raw entry (absent members
// untouched), a missing model creates a user model under a known provider
// (custom raw definition required, builtin scaffolded), bundled and
// discovered models refuse their identity and protocol members by presence,
// a reserved extra_body key refuses through the candidate check, and
// slash-containing model IDs stay exact query values.
func TestMetadataEditModelUpsert(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)

	// No source-specific lock applies: every identity/protocol member lands
	// on the bundled model as its user override.
	generation := 1
	for _, row := range []struct {
		name  string
		patch protocol.ModelEdit
		check func(*catalog.Model)
	}{
		{"name", protocol.ModelEdit{Name: ptrTo("Renamed")}, func(m *catalog.Model) {
			testassert.Equal(t, m.Name, "Renamed")
		}},
		{"system_role", protocol.ModelEdit{SystemRole: ptrTo(protocol.SystemRole("user"))}, func(m *catalog.Model) {
			testassert.Require(t, m.SystemRole == catalog.SystemRole("user"), "bundled model system_role = %q, want the override", m.SystemRole)
		}},
		{"usage_in_stream", protocol.ModelEdit{UsageInStream: ptrTo(false)}, func(m *catalog.Model) {
			testassert.Require(t, !m.UsageInStream, "bundled model usage_in_stream = true, want the override")
		}},
		{"input_modalities", protocol.ModelEdit{InputModalities: &[]protocol.InputModality{"text"}}, func(m *catalog.Model) {
			testassert.Require(t, len(m.InputModalities) == 1 && m.InputModalities[0] == catalog.Modality("text"), "bundled model modalities = %v, want the override", m.InputModalities)
		}},
		{"protocol_metadata", protocol.ModelEdit{ProtocolMetadata: &protocol.ProtocolMetadata{Family: ptrTo("fam")}}, func(m *catalog.Model) {
			testassert.Require(t, m.ProtocolMetadata != nil && m.ProtocolMetadata.Family == "fam", "bundled model protocol_metadata = %+v, want the override", m.ProtocolMetadata)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			candidate, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", row.patch))
			testassert.Require(t, err == nil, "bundled %s patch: %v", row.name, err)
			generation++
			testassert.Require(t, candidate.snapshot.generation == uint64(generation), "bundled %s patch generation = %d, want %d", row.name, candidate.snapshot.generation, generation)
			drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
			row.check(candidate.snapshot.catalog.Providers["bstatic"].Models["bm"])
		})
	}

	// The correctable members land on the bundled model and the reserved
	// extra_body key refuses through the candidate check.
	window := 2000
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{ContextWindow: &window}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	testassert.Equal(t, svc.current().catalog.Providers["bstatic"].Models["bm"].ContextWindow, 2000)
	reserved := map[string]any{"stream": true}
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	_, saveErr := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{ExtraBody: &reserved}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, saveErr, ErrConfiguration)

	// A missing model creates a user model; the second save of the same ID
	// is a legitimate edit, not a duplicate error.
	created := "Created"
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "new-model", protocol.ModelEdit{Name: &created, ContextWindow: &window}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	entry := svc.current().catalog.Providers["prov"].Models["new-model"]
	testassert.Require(t, entry != nil && entry.Source == catalog.SourceUser && entry.Name == "Created", "created model = %+v, want a user-sourced entry", entry)
	renamed := "Renamed"
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "new-model", protocol.ModelEdit{Name: &renamed}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	edited := svc.current().catalog.Providers["prov"].Models["new-model"]
	testassert.Require(t, edited != nil && edited.Name == "Renamed" && edited.ContextWindow == 2000, "edited model = %+v, want the renamed entry with its created window 2000 preserved", edited)

	// A slash-containing model ID is an exact query value.
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "org/model", protocol.ModelEdit{ContextWindow: &window}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	testassert.Require(t, svc.current().catalog.Providers["prov"].Models["org/model"] != nil, "the slash model ID is not a catalog member")

	// Missing provider and the unknown-model identity rules.
	before, first, warnRev = metadataBaseline(t, svc, h.configPath)
	_, ghostErr := svc.mutate(context.Background(), svc.editModelSave("ghost", "m", protocol.ModelEdit{}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, ghostErr, catalog.ErrUnknownProvider)

	// The empty patch on a new model is a valid upsert.
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "bare", protocol.ModelEdit{}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))

	// An explicit zero context_window is a real value (the typed target and
	// the catalog admit 0/incomplete models; there is no positivity rule):
	// the patch writes it and the entry stays a catalog member, unusable.
	zero := 0
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{ContextWindow: &zero}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	zeroEntry := svc.current().catalog.Providers["prov"].Models["m"]
	testassert.Require(t, zeroEntry != nil && zeroEntry.ContextWindow == 0, "model after explicit zero window = %+v, want the written 0 window retained as incomplete", zeroEntry)
}

// --- model delete ---

// TestMetadataEditModelDelete pins the model deletion axes: only user-source
// models are deletable, missing identities fail their typed unknown errors
// (no silent success), a model deleted from the latest raw layer refuses, and
// the successful delete removes exactly the owning raw entry.
// TestMetadataEditModelDelete pins the model deletion axes under the
// user-layer rule: an addressed model loses only its user node — a bundled
// model's override removal reveals the bundled base, missing identities fail
// their typed unknown errors — and a model already removed from the latest
// raw layer still publishes with no subject left behind.
func TestMetadataEditModelDelete(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)

	// The bundled model's override removal reveals the bundled base: the
	// raw entry leaves, the effective model stays.
	hidden := true
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{Hidden: &hidden}))
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, svc.warnings, sub, "2")
	candidate, err := svc.mutate(context.Background(), svc.editModelDelete("bstatic", "bm"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 3, "bundled model delete = (%v, generation %d), want a publication at 3", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "3")
	entry := candidate.snapshot.catalog.Providers["bstatic"].Models["bm"]
	testassert.Require(t, entry != nil && entry.Source == catalog.SourceBundled, "bundled model after delete = %+v, want the surviving bundled base", entry)
	var rawModels map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(metadataRawProvider(t, h.configPath, "bstatic")["models"], &rawModels))
	if _, present := rawModels["bm"]; present {
		t.Fatalf("raw models = %s, want the user node gone", metadataRawProvider(t, h.configPath, "bstatic")["models"])
	}

	// Missing identities fail their typed errors.
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	_, err = svc.mutate(context.Background(), svc.editModelDelete("ghost", "m"))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, catalog.ErrUnknownProvider)
	_, err = svc.mutate(context.Background(), svc.editModelDelete("prov", "ghost"))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, catalog.ErrUnknownModel)

	// The successful user-model delete removes exactly the raw entry.
	{
		_, err := svc.mutate(context.Background(), svc.editModelDelete("prov", "wide"))
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, svc.warnings, sub, "4")
	rawProv := metadataRawProvider(t, h.configPath, "prov")
	var models map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(rawProv["models"], &models))
	{
		_, present := models["wide"]
		testassert.Require(t, !present, "raw models = %s, want the deleted entry gone", rawProv["models"])
	}
	{
		_, present := models["m"]
		testassert.Require(t, present, "the delete touched an unrelated model")
	}
	var rawProviders map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	{
		_, present := rawProviders["other"]
		testassert.Require(t, present, "the delete touched an unrelated provider")
	}

	// A user model already deleted from the latest raw layer by an external
	// editor (without a reload) still publishes: the identity is effective,
	// no user node remains, and the post-state carries no subject. The
	// external write removes only "m" from the raw layer.
	external := `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":"META_TEST_KEY","headers":{"X-Trace":"t1"}},"discovery":false,"extra_body":{"side":1,"big":9007199254740993},"models":{"wide":{"context_window":8192}}},"other":{"transport":{"base_url":"https://other.test/v1","api_key_env":"OTHER_TEST_KEY"},"discovery":false,"models":{"o":{"context_window":9007199254740993}}}},"custom_flag":true}`
	writeServiceFile(t, h.configPath, external)
	candidate, err = svc.mutate(context.Background(), svc.editModelDelete("prov", "m"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 5, "externally-removed model delete = (%v, generation %d), want a publication at 5", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "5")
	testassert.Require(t, candidate.snapshot.catalog.Providers["prov"].Models["m"] == nil, "the externally-removed model stayed in the candidate catalog")
}

// --- model field reset ---

// TestMetadataEditResetModelFields pins the model reset matrix: every closed
// field removes exactly its own user override, the user-model context_window
// reset is refused (the user layer is that model's whole definition), a
// bundled model's reset restores the bundled value, a discovered model's
// context_window override resets to the accepted record's window, resetting
// hidden is invalid, and a reset with no override left still rewrites the
// owning file and publishes the next generation under the one shared
// successful-edit rule.
func TestMetadataEditResetModelFields(t *testing.T) {
	h, svc, sub := newOverlayFixture(t)

	// The user-model context_window reset lands: removing the sole window
	// leaves a valid incomplete model that cannot make a new admission
	// usable.
	candidate, err := svc.mutate(context.Background(), svc.editModelFieldReset("prov", "m", protocol.ModelFieldContextWindow))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 2, "user context_window reset = (%v, generation %d), want a publication at 2", err, candidate.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "2")
	entry := candidate.snapshot.catalog.Providers["prov"].Models["m"]
	testassert.Require(t, entry != nil && entry.ContextWindow == 0, "user model after the window reset = %+v, want the incomplete model retained", entry)

	// Seed every resettable override on the user model, then reset each one.
	name := "M2"
	maxOut := 55
	stream := true
	extra := map[string]any{"x": 1}
	cost := protocol.Cost{Input: ptrTo(2.5)}
	role := protocol.SystemRole("user")
	modalities := []protocol.InputModality{"text", "image"}
	meta := protocol.ProtocolMetadata{Family: ptrTo("fam")}
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{
			Name: &name, MaxOutputTokens: &maxOut, UsageInStream: &stream, ExtraBody: &extra,
			Cost: &cost, SystemRole: &role, InputModalities: &modalities, ProtocolMetadata: &meta,
		}))
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, svc.warnings, sub, "3")

	// The other closed fields remove their own user override from the user
	// model's raw definition.
	generation := 3
	for _, field := range []protocol.ModelField{
		protocol.ModelFieldName,
		protocol.ModelFieldMaxOutputTokens,
		protocol.ModelFieldUsageInStream,
		protocol.ModelFieldExtraBody,
		protocol.ModelFieldCost,
		protocol.ModelFieldProtocolMetadata,
		protocol.ModelFieldSystemRole,
		protocol.ModelFieldInputModalities,
		protocol.ModelFieldHidden,
	} {
		candidate, err := svc.mutate(context.Background(), svc.editModelFieldReset("prov", "m", field))
		testassert.Require(t, err == nil && candidate.snapshot.generation == svc.current().generation, "reset %q = (%v, generation %d), want a publication", field, err, candidate.snapshot.generation)
		generation++
		drainMutationEvent(t, sub, strconv.Itoa(generation))
		var rawModels map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(metadataRawProvider(t, h.configPath, "prov")["models"], &rawModels))
		var rawModel map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(rawModels["m"], &rawModel))
		{
			_, present := rawModel[string(field)]
			testassert.Require(t, !present, "reset %q left the raw override: %s", field, rawModels["m"])
		}
	}

	// A bundled model's override reset restores the bundled value; an
	// absent override publishes under the same shared successful-edit rule.
	window := 50
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{ContextWindow: &window}))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	if _, err := svc.mutate(context.Background(), svc.editModelFieldReset("bstatic", "bm", protocol.ModelFieldContextWindow)); err != nil || svc.current().catalog.Providers["bstatic"].Models["bm"].ContextWindow != 1000 {
		t.Fatalf("bundled reset = (%v, window %d), want the bundled 1000 restored", err, svc.current().catalog.Providers["bstatic"].Models["bm"].ContextWindow)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	// An absent override is the shared successful edit too: no member to
	// remove, yet the owning file is rewritten and the next generation
	// published.
	{
		_, err := svc.mutate(context.Background(), svc.editModelFieldReset("bstatic", "bm", protocol.ModelFieldCost))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))

	// The discovered model's context_window override resets to the accepted
	// discovery record's window: the override lands first, the reset removes
	// only its own raw member, and the source stays discovered.
	recordWindow := 1024
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{ContextWindow: &recordWindow}))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	{
		_, err := svc.mutate(context.Background(), svc.editModelFieldReset("disco", "disc-model", protocol.ModelFieldContextWindow))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	entry = svc.current().catalog.Providers["disco"].Models["disc-model"]
	testassert.Require(t, entry != nil && entry.ContextWindow == 2048 && entry.Source == catalog.SourceDiscovered, "discovered model after reset = %+v, want the record window 2048 restored with its source", entry)
	if _, present := metadataRawModel(t, h.configPath, "disco", "disc-model")["context_window"]; present {
		t.Fatalf("discovered raw model = %s, want the reset context_window member gone", metadataRawModel(t, h.configPath, "disco", "disc-model"))
	}
}

// --- discovered source ---

// seedDiscoveredModel seeds the transport-bound discovery record for the
// bundled disco provider from one pre-build of the same loader, before the
// service publishes: the record supplies the discovered "disc-model" with no
// network attempt (the fresh attempt time keeps the provider out of the
// refresh candidates).
func seedDiscoveredModel(t *testing.T, h *serviceHarness) {
	t.Helper()
	preBuilt, err := h.loader.LoadCaptured(context.Background(), map[string]any{})
	testassert.NoError(t, err)
	disco := preBuilt.Catalog.Providers["disco"]
	testassert.Require(t, disco != nil, "bundled disco provider missing: %+v", preBuilt.Catalog.Providers)
	{
		ok, err := catalog.TryWriteDiscoveryCache(h.home, "disco", disco.Transport,
			catalog.DiscoveredProvider{Models: map[string]catalog.DiscoveredModel{
				"disc-model": {Name: "Disc", ContextWindow: 2048, MaxOutputTokens: 256},
			}}, time.Now().UTC())
		testassert.Require(t, err == nil && ok, "seed discovery cache: %v %v", ok, err)
	}
}

// TestMetadataEditDiscoveredModelSource pins the discovered-source axes: a
// transport-bound discovery record supplies a discovered model whose
// identity members are locked and which is not deletable, while its
// correctable members land and its overrides reset to the discovered value.
func TestMetadataEditDiscoveredModelSource(t *testing.T) {
	_, svc, sub := newOverlayFixture(t)

	entry := svc.current().catalog.Providers["disco"].Models["disc-model"]
	testassert.Require(t, entry != nil && entry.Source == catalog.SourceDiscovered, "discovered model = %+v, want a discovered-source entry", entry)

	// No source-specific lock applies: the identity/protocol members land
	// on the discovered model as its user override.
	generation := 1
	for _, row := range []struct {
		name  string
		patch protocol.ModelEdit
		check func(*catalog.Model)
	}{
		{"name", protocol.ModelEdit{Name: ptrTo("Renamed")}, func(m *catalog.Model) {
			testassert.Equal(t, m.Name, "Renamed")
		}},
		{"system_role", protocol.ModelEdit{SystemRole: ptrTo(protocol.SystemRole("user"))}, func(m *catalog.Model) {
			testassert.Require(t, m.SystemRole == catalog.SystemRole("user"), "discovered model system_role = %q, want the override", m.SystemRole)
		}},
		{"usage_in_stream", protocol.ModelEdit{UsageInStream: ptrTo(false)}, func(m *catalog.Model) {
			testassert.Require(t, !m.UsageInStream, "discovered model usage_in_stream = true, want the override")
		}},
		{"input_modalities", protocol.ModelEdit{InputModalities: &[]protocol.InputModality{"text"}}, func(m *catalog.Model) {
			testassert.Require(t, len(m.InputModalities) == 1 && m.InputModalities[0] == catalog.Modality("text"), "discovered model modalities = %v, want the override", m.InputModalities)
		}},
		{"protocol_metadata", protocol.ModelEdit{ProtocolMetadata: &protocol.ProtocolMetadata{Family: ptrTo("fam")}}, func(m *catalog.Model) {
			testassert.Require(t, m.ProtocolMetadata != nil && m.ProtocolMetadata.Family == "fam", "discovered model protocol_metadata = %+v, want the override", m.ProtocolMetadata)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			candidate, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", row.patch))
			testassert.Require(t, err == nil, "discovered %s patch: %v", row.name, err)
			generation++
			drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
			row.check(candidate.snapshot.catalog.Providers["disco"].Models["disc-model"])
		})
	}

	// The override deletion reveals the discovered base.
	candidate, err := svc.mutate(context.Background(), svc.editModelDelete("disco", "disc-model"))
	testassert.NoError(t, err)
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	entry = candidate.snapshot.catalog.Providers["disco"].Models["disc-model"]
	testassert.Require(t, entry != nil && entry.Source == catalog.SourceDiscovered && entry.MaxOutputTokens == 256, "discovered model after delete = %+v, want the discovered base restored", entry)

	// The correctable member lands again; the override resets to the
	// discovered value.
	maxOut := 100
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{MaxOutputTokens: &maxOut}))
		testassert.NoError(t, err)
	}
	generation++
	drainConnectionEvent(t, svc.warnings, sub, strconv.Itoa(generation))
	candidate, err = svc.mutate(context.Background(), svc.editModelFieldReset("disco", "disc-model", protocol.ModelFieldMaxOutputTokens))
	testassert.Require(t, err == nil && candidate.snapshot.catalog.Providers["disco"].Models["disc-model"].MaxOutputTokens == 256, "discovered reset = (%v, max output %d), want the discovered 256 restored", err, candidate.snapshot.catalog.Providers["disco"].Models["disc-model"].MaxOutputTokens)
}

// --- Runtime operators over the composed runtime ---

// TestRuntimeProviderMetadataOperators proves the provider metadata
// operators end to end over the composed runtime: create, patch, reset, and
// delete each publish exactly one generation and project their result from
// the returned candidate, while the retained refusals — a duplicate create,
// a builtin removal, an unknown identity, a hidden reset, and a credential
// header — fail before any write.
func TestRuntimeProviderMetadataOperators(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		testassert.NoError(t, err)
		defer sub.Close()
		window := 4096
		models := map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}

		// The create: the projected result is the new provider view.
		mutation, err := r.addProvider(ctx, "newp",
			protocol.ProviderEdit{BaseUrl: ptrTo("https://new.test/v1")}, models, nil)
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "2", "addProvider = (%v, %+v), want generation 2", err, mutation.ConfigurationRevision)
		testassert.Require(t, mutation.Result.Id == "newp" && !mutation.Result.Builtin && len(mutation.Result.Models) == 1 && mutation.Result.Models[0].Source == protocol.ModelSource("user"), "created result = %+v, want the newp provider view with its user model", mutation.Result)
		drainMutationEventWithWarning(t, sub, "2", r.warnings)

		// The patch: the result projects the returned candidate.
		name := "Patched"
		mutation, err = r.updateProvider(ctx, "prov", protocol.ProviderEdit{Name: &name})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "3" && mutation.Result.Name == "Patched", "updateProvider = (%v, %+v), want the patched view at generation 3", err, mutation)
		drainMutationEventWithWarning(t, sub, "3", r.warnings)

		// The reset: the previous effective name is restored.
		mutation, err = r.resetProviderField(ctx, "prov", protocol.ProviderFieldName)
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "4" && mutation.Result.Name != "Patched", "resetProviderField = (%v, %+v), want the restored view at generation 4", err, mutation)
		drainMutationEvent(t, sub, "4")

		// The builtin user node's delete reveals the bundled base: the
		// result carries the surviving effective post-state.
		mutation, err = r.deleteProvider(ctx, "openrouter")
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "5" && mutation.Result != nil && mutation.Result.Id == "openrouter" && mutation.Result.Builtin, "builtin delete = (%v, %+v), want the revealed bundled base at generation 5", err, mutation)
		drainConnectionEvent(t, r.warnings, sub, "5")

		// The hidden reset removes its own override.
		{
			_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Hidden: ptrTo(true)})
			testassert.NoError(t, err)
		}
		drainMutationEvent(t, sub, "6")
		mutation, err = r.resetProviderField(ctx, "prov", protocol.ProviderFieldHidden)
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "7" && !mutation.Result.Hidden, "hidden reset = (%v, %+v), want the removed override at generation 7", err, mutation)
		drainConnectionEvent(t, r.warnings, sub, "7")

		// The refusals, each with the full state oracle: the typed failure
		// class and no file write, generation, warning revision, or event.
		refusalRows := []struct {
			name string
			run  func() error
			want error
		}{
			{"duplicate create", func() error {
				_, err := r.addProvider(ctx, "prov", protocol.ProviderEdit{BaseUrl: ptrTo("https://x.test/v1")}, models, nil)
				return err
			}, harness.ErrInvalid},
			{"unknown delete", func() error {
				_, err := r.deleteProvider(ctx, "ghost")
				return err
			}, catalog.ErrUnknownProvider},
			{"credential header", func() error {
				_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Headers: &map[string]string{"Authorization": "x"}})
				return err
			}, harness.ErrInvalid},
		}
		for _, row := range refusalRows {
			t.Run(row.name, func(t *testing.T) {
				before, generation, warnRev := runtimeMutationBaseline(t, r)
				err := row.run()
				assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, row.want)
			})
		}

		// The standalone user definition's delete: the same operator with a
		// null post-state — no subject remains.
		mutation, err = r.deleteProvider(ctx, "newp")
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "8" && mutation.Result == nil, "deleteProvider = (%v, %+v), want the null post-state at generation 8", err, mutation)
		drainMutationEvent(t, sub, "8")
	})
}

// TestRuntimeModelMetadataOperators proves the model metadata operators over
// the composed runtime: the user-model upsert and delete, the model field
// reset, a bundled model's lock refusal and correctable patch, the
// slash-containing model ID as an exact query value, and the typed unknown
// failures.
func TestRuntimeModelMetadataOperators(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()

		// The bundled model of the builtin, resolved dynamically from the
		// published catalog.
		bundledModel := bundledCatalogModel(t, r, "openrouter")

		// No source-specific lock applies: the rename lands as the bundled
		// model's user override.
		lockedName := "x"
		mutation, err := r.saveModel(ctx, "openrouter", bundledModel, protocol.ModelEdit{Name: &lockedName})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "2" && mutation.Result.Name == lockedName, "bundled rename = (%v, %+v), want the landed override at generation 2", err, mutation)

		// The correctable member lands on the bundled model.
		window := 2048
		mutation, err = r.saveModel(ctx, "openrouter", bundledModel, protocol.ModelEdit{ContextWindow: &window})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "3" && mutation.Result.ContextWindow == 2048, "bundled correctable patch = (%v, %+v), want the new window at generation 3", err, mutation)

		// The override's delete reveals the bundled base; the reset restores
		// the bundled window too.
		mutation2, err := r.deleteModel(ctx, "openrouter", bundledModel)
		testassert.Require(t, err == nil && mutation2.ConfigurationRevision.Generation == "4" && mutation2.Result != nil && !mutation2.Result.Hidden, "bundled delete = (%v, %+v), want the revealed base at generation 4", err, mutation2)
		mutation, err = r.resetModelField(ctx, "openrouter", bundledModel, protocol.ModelFieldContextWindow)
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "5", "bundled reset = (%v, %+v), want generation 5", err, mutation)
		testassert.Require(t, mutation.Result.ContextWindow != 2048 && mutation.Result.ContextWindow != 0, "reset window = %d, want the restored bundled value", mutation.Result.ContextWindow)

		// The slash-containing model ID is an exact query value; the same ID
		// upserts twice without a duplicate error.
		slashWindow := 10
		for _, generation := range []string{"6", "7"} {
			mutation, err = r.saveModel(ctx, "prov", "org/model", protocol.ModelEdit{ContextWindow: &slashWindow})
			testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == generation && mutation.Result.Id == "org/model", "slash upsert = (%v, %+v), want the model at generation %s", err, mutation, generation)
		}

		// The standalone user model's delete: the same operator with a null
		// post-state.
		deletion, err := r.deleteModel(ctx, "prov", "org/model")
		testassert.Require(t, err == nil && deletion.ConfigurationRevision.Generation == "8" && deletion.Result == nil, "deleteModel = (%v, %+v), want the null post-state at generation 8", err, deletion)

		// The typed unknown failures.
		{
			_, err := r.saveModel(ctx, "ghost", "m", protocol.ModelEdit{})
			testassert.Require(t, errors.Is(err, catalog.ErrUnknownProvider), "unknown provider = %v, want catalog.ErrUnknownProvider", err)
		}
		{
			_, err := r.deleteModel(ctx, "prov", "ghost")
			testassert.Require(t, errors.Is(err, catalog.ErrUnknownModel), "unknown model = %v, want catalog.ErrUnknownModel", err)
		}
		{
			_, err := r.saveModel(ctx, "", "m", protocol.ModelEdit{})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "empty provider id = %v, want harness.ErrInvalid", err)
		}

		// The owning file carried every published edit and nothing else.
		root := fileRoot(t, e.configPath)
		var providers map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(root["providers"], &providers))
		{
			_, present := providers["org/model"]
			testassert.Require(t, !present, "a slash model ID became a provider-level member")
		}
	})
}

// TestRuntimeModelMetadataMutationAffectsNextAdmissions proves the metadata
// edit's effect boundary over the real admission path: a durably running
// Operation keeps its captured configuration while the next admission of
// another Session prepares under the new generation. The committed captures
// carry the observation.
func TestRuntimeModelMetadataMutationAffectsNextAdmissions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerConfigDocument(e.server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{"worker":{"model":"prov/m","system_prompt":"simple"}}`)
		r, err := e.open(context.Background(), e.storagePlugin(store))
		testassert.NoError(t, err)
		defer closeProjectionRuntime(r)
		ctx := context.Background()

		a := projectionSession(t, r, "/tmp/metadata-a", "worker")
		b := projectionSession(t, r, "/tmp/metadata-b", "worker")

		// One durably running Operation on session A holds the original
		// capture while the metadata mutation lands.
		gate := make(chan struct{})
		e.server.setHold(gate)
		submitThroughRuntime(t, r, a.Identity.SessionID, "op-active", "running")
		awaitModelArrival(t, e)

		// The mutated window keeps ordinary rows fitting: the edit's effect
		// boundary is under test, not a compaction trigger.
		window := 131072
		mutation, err := r.saveModel(ctx, "prov", "m", protocol.ModelEdit{ContextWindow: &window})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "2", "saveModel = (%v, %+v), want generation 2", err, mutation.ConfigurationRevision)

		// Session B's next admission commits its capture under the new
		// generation while the active Operation has not re-captured.
		submitThroughRuntime(t, r, b.Identity.SessionID, "op-b1", "b turn")
		testassert.Equal(t, committedAdmission(t, r, b.Identity.SessionID, "op-b1"), "worker=prov/m@2")
		testassert.Equal(t, committedAdmission(t, r, a.Identity.SessionID, "op-active"), "worker=prov/m@1")

		// Both captures settle; A's next admission uses the new publication.
		close(gate)
		awaitIdleSession(t, r, a.Identity.SessionID)
		awaitIdleSession(t, r, b.Identity.SessionID)
		submitConvergedThroughRuntime(t, r, a.Identity.SessionID, "op-a2", "a turn", harness.OperationSuccess)
		testassert.Equal(t, committedAdmission(t, r, a.Identity.SessionID, "op-a2"), "worker=prov/m@2")
	})
}

// --- patch preservation (one patch rule, no entry replacement) ---

// TestMetadataEditModelPatchPreservesUnsuppliedValues pins the patch rule for
// every model source: a partial patch of ONE member preserves every
// unsupplied raw value and its meaningful catalog projection — the usable
// window included — on a user model, a bundled override, and a discovered
// override, and an empty patch wipes nothing.
func TestMetadataEditModelPatchPreservesUnsuppliedValues(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)

	// Seed the user model with every meaningful writable member.
	name := "M2"
	cost := protocol.Cost{Input: ptrTo(2.5)}
	extra := map[string]any{"x": 1}
	meta := protocol.ProtocolMetadata{Family: ptrTo("fam")}
	stream := true
	hidden := true
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{
			Name: &name, ContextWindow: ptrTo(4096), MaxOutputTokens: ptrTo(100),
			Cost: &cost, ExtraBody: &extra, ProtocolMetadata: &meta, UsageInStream: &stream, Hidden: &hidden,
		}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "2")

	// The one-member patch: everything unsupplied survives in the raw entry
	// and in the catalog projection.
	renamed := "M3"
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{Name: &renamed}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "3")
	entry := svc.current().catalog.Providers["prov"].Models["m"]
	testassert.Require(t, entry != nil && entry.Name == "M3" && entry.ContextWindow == 4096 && entry.MaxOutputTokens == 100 && entry.Cost != nil && entry.Cost.Input != nil && *entry.Cost.Input == 2.5 && fmt.Sprint(entry.ExtraBody["x"]) == "1" && entry.ProtocolMetadata != nil && entry.ProtocolMetadata.Family == "fam" && entry.UsageInStream && entry.Hidden, "user model after one-member patch = %+v, want every unsupplied value preserved", entry)
	rawModel := metadataRawModel(t, h.configPath, "prov", "m")
	for _, member := range []string{"context_window", "max_output_tokens", "cost", "extra_body", "protocol_metadata", "usage_in_stream", "hidden"} {
		{
			_, present := rawModel[member]
			testassert.Require(t, present, "user raw model = %s, want the unsupplied %s member preserved", rawModel, member)
		}
	}

	// An empty patch wipes nothing: the user model's definition — its usable
	// window included — is retained, not cleared.
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{}))
		testassert.NoError(t, err)
	}
	{
		entry = svc.current().catalog.Providers["prov"].Models["m"]
		testassert.Require(t, entry != nil && entry.ContextWindow == 4096 && entry.Name == "M3", "user model after empty patch = %+v, want the whole definition retained", entry)
	}
	drainMutationEvent(t, sub, "4")

	// The bundled override: a one-member patch preserves the earlier
	// override members and the bundled defaults of the unsupplied ones.
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{
			ContextWindow: ptrTo(2000), MaxOutputTokens: ptrTo(50), Cost: &cost, ExtraBody: &extra, Hidden: &hidden,
		}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "5")
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{MaxOutputTokens: ptrTo(60)}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "6")
	entry = svc.current().catalog.Providers["bstatic"].Models["bm"]
	testassert.Require(t, entry != nil && entry.ContextWindow == 2000 && entry.MaxOutputTokens == 60 && entry.Cost != nil && entry.Cost.Input != nil && *entry.Cost.Input == 2.5 && fmt.Sprint(entry.ExtraBody["x"]) == "1" && entry.Hidden, "bundled model after one-member patch = %+v, want the earlier override members preserved", entry)

	// The discovered override: the same preservation rule.
	unsetenv(t, "DISCO_TEST_KEY") // never ready: no network anywhere below
	seedDiscoveredModel(t, h)
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{
			MaxOutputTokens: ptrTo(100), Cost: &cost, ExtraBody: &extra, Hidden: &hidden,
		}))
		testassert.NoError(t, err)
	}
	drainMutationEvent(t, sub, "7")
	{
		_, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{Cost: &protocol.Cost{Output: ptrTo(3.0)}}))
		testassert.NoError(t, err)
	}
	entry = svc.current().catalog.Providers["disco"].Models["disc-model"]
	testassert.Require(t, entry != nil && entry.MaxOutputTokens == 100 && entry.Cost != nil && entry.Cost.Output != nil && *entry.Cost.Output == 3 && entry.Cost.Input == nil && fmt.Sprint(entry.ExtraBody["x"]) == "1" && entry.Hidden, "discovered model after one-member patch = %+v, want the earlier override members preserved", entry)
}

// metadataRawModel decodes one provider's raw model entry from the owning
// file.
func metadataRawModel(t *testing.T, configPath, providerID, modelID string) map[string]json.RawMessage {
	t.Helper()
	root := fileRoot(t, configPath)
	var rawProviders map[string]map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["providers"], &rawProviders))
	var rawModels map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(rawProviders[providerID]["models"], &rawModels))
	var rawModel map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(rawModels[modelID], &rawModel))
	return rawModel
}

// --- hidden visibility ---

// TestMetadataEditHiddenVisibility pins the hidden patch axis on every
// source with no second setter family: TRUE and FALSE write the owned raw
// hidden member and the post state through the ordinary provider/model
// patches, an absent hidden member leaves it untouched, and the hidden reset
// stays invalid. The connected keyless provider's model leaves and rejoins
// the visible picker while staying in the full list.
func TestMetadataEditHiddenVisibility(t *testing.T) {
	h, svc, sub := newOverlayFixture(t)

	// Provider custom + builtin, TRUE and FALSE: every completed synchronous
	// mutation publishes exactly its own generation's event and nothing is
	// left queued.
	generation := 1
	for _, row := range []struct {
		providerID string
		builtin    bool
	}{
		{"prov", false},
		{"bstatic", true},
	} {
		for _, hidden := range []bool{true, false} {
			candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate(row.providerID, protocol.ProviderEdit{Hidden: &hidden}))
			if err != nil || candidate.snapshot.catalog.Providers[row.providerID].Hidden != hidden {
				t.Fatalf("%s hidden %v patch = (%v, hidden %v), want the written state", row.providerID, hidden, err, candidate.snapshot.catalog.Providers[row.providerID].Hidden)
			}
			generation++
			drainMutationEvent(t, sub, strconv.Itoa(generation))
			assertNoEvent(t, sub)
			rawModel := metadataRawProvider(t, h.configPath, row.providerID)
			{
				got, present := rawModel["hidden"]
				testassert.Require(t, present && string(got) == strconv.FormatBool(hidden), "%s raw hidden = %s (present %v), want %v", row.providerID, got, present, hidden)
			}
		}
	}

	// Model user + bundled + discovered, TRUE and FALSE: the same one-event
	// settlement per patch.
	for _, row := range []struct{ providerID, modelID string }{
		{"prov", "m"},
		{"bstatic", "bm"},
		{"disco", "disc-model"},
	} {
		for _, hidden := range []bool{true, false} {
			candidate, err := svc.mutate(context.Background(), svc.editModelSave(row.providerID, row.modelID, protocol.ModelEdit{Hidden: &hidden}))
			testassert.Require(t, err == nil && candidate.snapshot.catalog.Providers[row.providerID].Models[row.modelID] != nil && candidate.snapshot.catalog.Providers[row.providerID].Models[row.modelID].Hidden == hidden, "%s/%s hidden %v patch = %v, want the written state", row.providerID, row.modelID, hidden, err)
			generation++
			drainMutationEvent(t, sub, strconv.Itoa(generation))
			assertNoEvent(t, sub)
			rawModel := metadataRawModel(t, h.configPath, row.providerID, row.modelID)
			{
				got, present := rawModel["hidden"]
				testassert.Require(t, present && string(got) == strconv.FormatBool(hidden), "%s/%s raw hidden = %s (present %v), want %v", row.providerID, row.modelID, got, present, hidden)
			}
		}
	}

	// An absent hidden member leaves the written value untouched (the patch
	// rule, not a visibility setter).
	{
		_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{Name: ptrTo("Renamed")}))
		testassert.NoError(t, err)
	}
	generation++
	drainMutationEvent(t, sub, strconv.Itoa(generation))
	assertNoEvent(t, sub)
	rawModel := metadataRawProvider(t, h.configPath, "prov")
	{
		got, present := rawModel["hidden"]
		testassert.Require(t, present && string(got) == "false", "prov raw hidden after absent patch = %s (present %v), want false untouched", got, present)
	}
}

// metadataRawProvider decodes one provider's raw entry map from the owning
// file.
func metadataRawProvider(t *testing.T, configPath, providerID string) map[string]json.RawMessage {
	t.Helper()
	root := fileRoot(t, configPath)
	var rawProviders map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["providers"], &rawProviders))
	var rawProvider map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(rawProviders[providerID], &rawProvider))
	return rawProvider
}

// TestRuntimeHiddenVisibilityPicker proves the hidden patch through the
// private Runtime operators over the connected picker: the hidden provider's
// model leaves the visible list and rejoins it on hidden=false while the
// full list keeps it, and the owned raw hidden member follows every write.
func TestRuntimeHiddenVisibilityPicker(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		window := 4096
		models := map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}

		// The connected keyless custom provider: its model is visible.
		mutation, err := r.addProvider(ctx, "keyless2",
			protocol.ProviderEdit{BaseUrl: ptrTo("https://k2.test/v1")}, models, nil)
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "2", "addProvider = (%v, %+v), want generation 2", err, mutation.ConfigurationRevision)
		visible, err := r.listModels(ctx, protocol.ListModelsParams{})
		testassert.Require(t, err == nil && modelListHas(visible, "keyless2", "m"), "visible picker = %+v (%v), want the connected keyless model", visible, err)

		// hidden=true: the model leaves the visible picker, the full list
		// keeps it, and the raw file carries the owned member.
		hidden := true
		mutation, err = r.updateProvider(ctx, "keyless2", protocol.ProviderEdit{Hidden: &hidden})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "3" && mutation.Result.Hidden, "hidden patch = (%v, %+v), want the hidden view at generation 3", err, mutation)
		visible, err = r.listModels(ctx, protocol.ListModelsParams{})
		testassert.Require(t, err == nil && !modelListHas(visible, "keyless2", "m"), "visible picker after hide = %+v (%v), want the model excluded", visible, err)
		full, err := r.listModels(ctx, protocol.ListModelsParams{All: true})
		testassert.Require(t, err == nil && modelListHas(full, "keyless2", "m"), "full picker after hide = %+v (%v), want the model retained", full, err)
		{
			got := metadataRawProvider(t, e.configPath, "keyless2")["hidden"]
			testassert.Require(t, string(got) == "true", "raw hidden = %s, want true", got)
		}

		// hidden=false: the model rejoins the visible picker.
		hidden = false
		mutation, err = r.updateProvider(ctx, "keyless2", protocol.ProviderEdit{Hidden: &hidden})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "4" && !mutation.Result.Hidden, "unhide patch = (%v, %+v), want the visible view at generation 4", err, mutation)
		visible, err = r.listModels(ctx, protocol.ListModelsParams{})
		testassert.Require(t, err == nil && modelListHas(visible, "keyless2", "m"), "visible picker after unhide = %+v (%v), want the model back", visible, err)
		{
			got := metadataRawProvider(t, e.configPath, "keyless2")["hidden"]
			testassert.Require(t, string(got) == "false", "raw hidden = %s, want false", got)
		}

		// The builtin provider hides through the same patch; its raw
		// override entry carries the member.
		hidden = true
		mutation, err = r.updateProvider(ctx, "openrouter", protocol.ProviderEdit{Hidden: &hidden})
		testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "5" && mutation.Result.Hidden, "builtin hide = (%v, %+v), want the hidden view at generation 5", err, mutation)
		{
			got := metadataRawProvider(t, e.configPath, "openrouter")["hidden"]
			testassert.Require(t, string(got) == "true", "builtin raw hidden = %s, want true", got)
		}

		// The user model hides through the model patch.
		modelMutation, err := r.saveModel(ctx, "prov", "m", protocol.ModelEdit{Hidden: &hidden})
		testassert.Require(t, err == nil && modelMutation.ConfigurationRevision.Generation == "6" && modelMutation.Result.Hidden, "model hide = (%v, %+v), want the hidden view at generation 6", err, modelMutation)
		{
			got := metadataRawModel(t, e.configPath, "prov", "m")["hidden"]
			testassert.Require(t, string(got) == "true", "model raw hidden = %s, want true", got)
		}
	})
}

// modelListHas reports whether the picker list contains one provider/model.
func modelListHas(list protocol.ModelList, providerID, modelID string) bool {
	for _, entry := range list.Models {
		if entry.Provider == providerID && entry.Model == modelID {
			return true
		}
	}
	return false
}

// --- direct Runtime special-ID table ---

// TestRuntimeSpecialProviderIDsDirectOperators drives the retained identity
// rule through every DIRECT Runtime operator — create, update, model save,
// provider reset, model reset, model delete, provider delete, all seven —
// not only the edit constructors: `.` `..` `?` `#` `%` are valid provider
// IDs, preserved exactly in every result, file key, and lookup, with the
// slash refused and the slash-containing model ID accepted.
func TestRuntimeSpecialProviderIDsDirectOperators(t *testing.T) {
	for _, id := range []string{".", "..", "?", "#", "%"} {
		t.Run(id, func(t *testing.T) {
			eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
				r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
				defer closeProjectionRuntime(r)
				ctx := context.Background()
				window := 1
				models := map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}

				mutation, err := r.addProvider(ctx, id, protocol.ProviderEdit{BaseUrl: ptrTo("https://id.test/v1")}, models, nil)
				testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == "2" && mutation.Result.Id == id, "addProvider(%q) = (%v, %+v), want the exact ID at generation 2", id, err, mutation.Result)
				{
					_, present := metadataRawProvider(t, e.configPath, id)["transport"]
					testassert.Require(t, present, "raw providers key %q missing", id)
				}
				name := "Ident"
				mutation, err = r.updateProvider(ctx, id, protocol.ProviderEdit{Name: &name})
				testassert.Require(t, err == nil && mutation.Result.Id == id && mutation.Result.Name == name, "updateProvider(%q) = (%v, %+v), want the exact ID", id, err, mutation.Result)
				modelMutation, err := r.saveModel(ctx, id, "m", protocol.ModelEdit{MaxOutputTokens: &window})
				testassert.Require(t, err == nil && modelMutation.Result.Id == "m" && !modelMutation.Result.ProviderHidden, "saveModel under %q = (%v, %+v), want the model view", id, err, modelMutation.Result)
				mutation, err = r.resetProviderField(ctx, id, protocol.ProviderFieldName)
				testassert.Require(t, err == nil && mutation.Result.Id == id && mutation.Result.Name != name, "resetProviderField(%q) = (%v, %+v), want the restored name", id, err, mutation.Result)
				// The model-field reset and the user-model delete keep the
				// exact identities too: all seven operators.
				modelMutation, err = r.resetModelField(ctx, id, "m", protocol.ModelFieldMaxOutputTokens)
				testassert.Require(t, err == nil && modelMutation.Result.Id == "m" && modelMutation.Result.MaxOutputTokens == 0, "resetModelField(%q, m) = (%v, %+v), want the restored default", id, err, modelMutation.Result)
				modelDeletion, err := r.deleteModel(ctx, id, "m")
				testassert.Require(t, err == nil && modelDeletion.ConfigurationRevision.Generation == "7" && modelDeletion.Result == nil, "deleteModel(%q, m) = (%v, %+v), want the null deletion at generation 7", id, err, modelDeletion)
				deletion, err := r.deleteProvider(ctx, id)
				testassert.Require(t, err == nil && deletion.ConfigurationRevision.Generation == "8" && deletion.Result == nil, "deleteProvider(%q) = (%v, %+v), want the null deletion at generation 8", id, err, deletion)
				{
					_, present := fileRoot(t, e.configPath)["providers"]
					testassert.Require(t, present, "providers member missing")
				}
				var providers map[string]json.RawMessage
				testassert.NoError(t, json.Unmarshal(fileRoot(t, e.configPath)["providers"], &providers))
				{
					_, present := providers[id]
					testassert.Require(t, !present, "raw providers key %q survived the delete", id)
				}
			})
		})
	}
}

// --- corrupt raw types ---

// --- typed raw-state failures through the Runtime operators ---

// runtimeMutationBaseline reads the immutable post-external-write oracle set:
// latest owning bytes, published generation, and warning revision.
func runtimeMutationBaseline(t *testing.T, r *Runtime) ([]byte, uint64, uint64) {
	t.Helper()
	data, err := os.ReadFile(r.config.configPath)
	testassert.NoError(t, err)
	current := r.config.current()
	testassert.Require(t, current != nil, "no published configuration")
	warnRev, _ := r.warnings.snapshot()
	return data, current.generation, warnRev
}

// assertRuntimeMutationRefused pins one refused Runtime operator: the typed
// failure class, byte-identical latest file, unchanged generation, warning
// revision, and event silence.
func assertRuntimeMutationRefused(t *testing.T, r *Runtime, sub *Subscription, before []byte, generation uint64, warnRev uint64, err error, want error) {
	t.Helper()
	testassert.Require(t, err != nil, "Runtime metadata operator succeeded, want a refusal")
	testassert.Require(t, errors.Is(err, want), "refusal = %v, want class %v", err, want)
	after, rerr := os.ReadFile(r.config.configPath)
	testassert.Require(t, rerr == nil && string(before) == string(after), "a refused Runtime operator changed the owning file (%v)", rerr)
	{
		current := r.config.current()
		testassert.Require(t, current != nil && current.generation == generation, "a refused Runtime operator advanced the generation to %v", current)
	}
	{
		got, _ := r.warnings.snapshot()
		testassert.Equal(t, got, warnRev)
	}
	assertNoEvent(t, sub)
}

// TestRuntimeMetadataCorruptRawTypesTypedFailure refuses every operator
// whose external raw configuration input decodes or types wrong — the whole
// providers member as a scalar or array, and a non-object provider entry,
// transport, models member, or model entry under a KNOWN published provider —
// through the Runtime operators with the shared configurationFailure class,
// the baseline taken after the external bad write, and the latest file,
// generation, warning revision, and event stream unchanged.
func TestRuntimeMetadataCorruptRawTypesTypedFailure(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		testassert.NoError(t, err)
		defer sub.Close()
		testassert.Require(t, r.config.current().catalog.Providers["prov"] != nil, "fixture provider not published")

		rows := []struct {
			name   string
			config string
			run    func() error
		}{
			{"providers member scalar", `{"providers":5,"sessions":{"archive_after_days":3}}`,
				func() error {
					name := "X"
					_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Name: &name})
					return err
				}},
			{"providers member array", `{"providers":[],"sessions":{"archive_after_days":3}}`,
				func() error {
					name := "X"
					_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Name: &name})
					return err
				}},
			{"provider entry nonobject", `{"providers":{"prov":5}}`,
				func() error {
					name := "X"
					_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Name: &name})
					return err
				}},
			{"transport nonobject", `{"providers":{"prov":{"transport":5,"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`,
				func() error {
					name := "X"
					_, err := r.updateProvider(ctx, "prov", protocol.ProviderEdit{Name: &name})
					return err
				}},
			{"models member nonobject", `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":"SHELL_TEST_KEY"},"discovery":false,"models":5}}}`,
				func() error {
					maxOut := 10
					_, err := r.saveModel(ctx, "prov", "m", protocol.ModelEdit{MaxOutputTokens: &maxOut})
					return err
				}},
			{"model entry nonobject", `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":"SHELL_TEST_KEY"},"discovery":false,"models":{"m":5}}}}`,
				func() error {
					maxOut := 10
					_, err := r.saveModel(ctx, "prov", "m", protocol.ModelEdit{MaxOutputTokens: &maxOut})
					return err
				}},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				// The external corruption is the latest raw layer; the
				// baseline is taken AFTER the bad write.
				writeServiceFile(t, e.configPath, row.config)
				before, generation, warnRev := runtimeMutationBaseline(t, r)
				err := row.run()
				assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
			})
		}
	})
}

// --- supplied field values: exact truth and file/API equality ---

// newOverlayFixture seeds the metadata fixture with its discovered record and
// publishes one identically configured service: the metadata document, the
// bundled disco record, and the subscribed service with its attached warning
// store.
func newOverlayFixture(t *testing.T) (*serviceHarness, *configurationService, *Subscription) {
	t.Helper()
	h := newMetadataHarness(t)
	unsetenv(t, "DISCO_TEST_KEY")
	seedDiscoveredModel(t, h)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	return h, svc, sub
}

// overlayJSON renders one projected view for the file/API comparison.
func overlayJSON(t *testing.T, view any) string {
	t.Helper()
	data, err := json.Marshal(view)
	testassert.NoError(t, err)
	return string(data)
}

// TestMetadataEditFieldValuesExactAndFileAPIEquivalent pins the supplied
// field's value truth: a provided member replaces its whole VALUE — valid
// whitespace-containing names and base URLs land exactly as provided, with
// the selected validators, not the writers, governing them — and the
// identical overlay through the edit API and through a hand-edited file
// plus reload produces the equal effective view for every actual source:
// custom and builtin providers, user, bundled, and discovered models, over
// two identically seeded fixtures with no reset surgery.
func TestMetadataEditFieldValuesExactAndFileAPIEquivalent(t *testing.T) {
	providerRows := []struct {
		name       string
		providerID string
		exactValue string
		fileDoc    func(doc string) string
		edit       func(svc *configurationService) (protocol.Provider, error)
	}{
		{"custom provider padded name", "prov", "  Padded Prov  ",
			func(doc string) string {
				return strings.Replace(doc, `"prov": {`, `"prov": {"name": "  Padded Prov  ",`, 1)
			},
			func(svc *configurationService) (protocol.Provider, error) {
				padded := "  Padded Prov  "
				candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{Name: &padded}))
				if err != nil {
					return protocol.Provider{}, err
				}
				return projectProvider(candidate, candidate.snapshot.catalog.Providers["prov"]), nil
			}},
		{"builtin provider padded name", "bstatic", "  Builtin Padded  ",
			func(doc string) string {
				return strings.Replace(doc, `"other": {`, `"bstatic": {"name": "  Builtin Padded  "}, "other": {`, 1)
			},
			func(svc *configurationService) (protocol.Provider, error) {
				padded := "  Builtin Padded  "
				candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{Name: &padded}))
				if err != nil {
					return protocol.Provider{}, err
				}
				return projectProvider(candidate, candidate.snapshot.catalog.Providers["bstatic"]), nil
			}},
		{"custom provider padded base_url", "prov", "https://x.test/v1 ",
			func(doc string) string {
				return strings.Replace(doc, `"base_url": "https://prov.test/v1"`, `"base_url": "https://x.test/v1 "`, 1)
			},
			func(svc *configurationService) (protocol.Provider, error) {
				padded := "https://x.test/v1 "
				candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{BaseUrl: &padded}))
				if err != nil {
					return protocol.Provider{}, err
				}
				return projectProvider(candidate, candidate.snapshot.catalog.Providers["prov"]), nil
			}},
		{"custom provider blank name", "prov", "   ",
			func(doc string) string {
				return strings.Replace(doc, `"prov": {`, `"prov": {"name": "   ",`, 1)
			},
			func(svc *configurationService) (protocol.Provider, error) {
				blank := "   "
				candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{Name: &blank}))
				if err != nil {
					return protocol.Provider{}, err
				}
				return projectProvider(candidate, candidate.snapshot.catalog.Providers["prov"]), nil
			}},
	}
	for _, row := range providerRows {
		t.Run(row.name, func(t *testing.T) {
			// Through the edit API.
			_, svcA, subA := newOverlayFixture(t)
			viaAPI, err := row.edit(svcA)
			testassert.NoError(t, err)
			drainConnectionEvent(t, svcA.warnings, subA, "2")
			// Through the file plus reload.
			hB, svcB, _ := newOverlayFixture(t)
			writeServiceFile(t, hB.configPath, row.fileDoc(metadataConfigDocument))
			viaFile, err := svcB.publish(context.Background())
			testassert.NoError(t, err)
			fileView := projectProvider(viaFile, viaFile.snapshot.catalog.Providers[row.providerID])
			apiJSON, fileJSON := overlayJSON(t, viaAPI), overlayJSON(t, fileView)
			testassert.Require(t, apiJSON == fileJSON && strings.Contains(apiJSON, row.exactValue), "got %v, want %v; the overlay view = %s, want the exact provided value %q", apiJSON, fileJSON, apiJSON, row.exactValue)
		})
	}

	modelRows := []struct {
		name       string
		providerID string
		modelID    string
		exactValue string
		check      func(t *testing.T, view protocol.ModelView)
		fileDoc    func(doc string) string
		edit       func(svc *configurationService) (protocol.ModelView, error)
	}{
		{"user model padded name", "prov", "m", "  Padded M  ", nil,
			func(doc string) string {
				return strings.Replace(doc, `"m": {"name": "M",`, `"m": {"name": "  Padded M  ",`, 1)
			},
			func(svc *configurationService) (protocol.ModelView, error) {
				padded := "  Padded M  "
				candidate, err := svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{Name: &padded}))
				if err != nil {
					return protocol.ModelView{}, err
				}
				return projectModelView(candidate.snapshot.catalog.Providers["prov"], candidate.snapshot.catalog.Providers["prov"].Models["m"]), nil
			}},
		{"bundled model padded name", "bstatic", "bm", "  Padded Bm  ", nil,
			func(doc string) string {
				return strings.Replace(doc, `"other": {`, `"bstatic": {"models": {"bm": {"name": "  Padded Bm  "}}}, "other": {`, 1)
			},
			func(svc *configurationService) (protocol.ModelView, error) {
				padded := "  Padded Bm  "
				candidate, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{Name: &padded}))
				if err != nil {
					return protocol.ModelView{}, err
				}
				return projectModelView(candidate.snapshot.catalog.Providers["bstatic"], candidate.snapshot.catalog.Providers["bstatic"].Models["bm"]), nil
			}},
		{"bundled multi-member name and context window", "bstatic", "bm", "Renamed", func(t *testing.T, view protocol.ModelView) {
			testassert.Equal(t, view.ContextWindow, 2000)
		},
			func(doc string) string {
				return strings.Replace(doc, `"other": {`, `"bstatic": {"models": {"bm": {"name": "Renamed", "context_window": 2000}}}, "other": {`, 1)
			},
			func(svc *configurationService) (protocol.ModelView, error) {
				renamed := "Renamed"
				window := 2000
				candidate, err := svc.mutate(context.Background(), svc.editModelSave("bstatic", "bm", protocol.ModelEdit{Name: &renamed, ContextWindow: &window}))
				if err != nil {
					return protocol.ModelView{}, err
				}
				return projectModelView(candidate.snapshot.catalog.Providers["bstatic"], candidate.snapshot.catalog.Providers["bstatic"].Models["bm"]), nil
			}},
		{"discovered model padded name", "disco", "disc-model", "  Padded Disc  ", nil,
			func(doc string) string {
				return strings.Replace(doc, `"other": {`, `"disco": {"models": {"disc-model": {"name": "  Padded Disc  "}}}, "other": {`, 1)
			},
			func(svc *configurationService) (protocol.ModelView, error) {
				padded := "  Padded Disc  "
				candidate, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{Name: &padded}))
				if err != nil {
					return protocol.ModelView{}, err
				}
				return projectModelView(candidate.snapshot.catalog.Providers["disco"], candidate.snapshot.catalog.Providers["disco"].Models["disc-model"]), nil
			}},
		{"discovered max_output_tokens", "disco", "disc-model", `"max_output_tokens":100`, func(t *testing.T, view protocol.ModelView) {
			testassert.Equal(t, view.MaxOutputTokens, 100)
		},
			func(doc string) string {
				return strings.Replace(doc, `"other": {`, `"disco": {"models": {"disc-model": {"max_output_tokens": 100}}}, "other": {`, 1)
			},
			func(svc *configurationService) (protocol.ModelView, error) {
				maxOutput := 100
				candidate, err := svc.mutate(context.Background(), svc.editModelSave("disco", "disc-model", protocol.ModelEdit{MaxOutputTokens: &maxOutput}))
				if err != nil {
					return protocol.ModelView{}, err
				}
				return projectModelView(candidate.snapshot.catalog.Providers["disco"], candidate.snapshot.catalog.Providers["disco"].Models["disc-model"]), nil
			}},
	}
	for _, row := range modelRows {
		t.Run(row.name, func(t *testing.T) {
			_, svcA, subA := newOverlayFixture(t)
			viaAPI, err := row.edit(svcA)
			testassert.NoError(t, err)
			drainConnectionEvent(t, svcA.warnings, subA, "2")
			hB, svcB, _ := newOverlayFixture(t)
			writeServiceFile(t, hB.configPath, row.fileDoc(metadataConfigDocument))
			viaFile, err := svcB.publish(context.Background())
			testassert.NoError(t, err)
			fileView := projectModelView(viaFile.snapshot.catalog.Providers[row.providerID], viaFile.snapshot.catalog.Providers[row.providerID].Models[row.modelID])
			apiJSON, fileJSON := overlayJSON(t, viaAPI), overlayJSON(t, fileView)
			testassert.Require(t, apiJSON == fileJSON && strings.Contains(apiJSON, row.exactValue), "got %v, want %v; the overlay view = %s, want the exact provided value %q", apiJSON, fileJSON, apiJSON, row.exactValue)
			if row.check != nil {
				row.check(t, viaAPI)
				row.check(t, fileView)
			}
		})
	}
}

// TestMetadataEditPaddedInvalidValuesNeverBecomeValid pins the uniform
// candidate governance for invalid padded values: the selected validators,
// not the writers, decide them — an api_key_env or system_role carrying
// padding refuses the edit with the owning file untouched, and the identical
// file-written member makes the provider invalid in the published catalog
// (dropped with a warning) so the padded value never becomes effective
// through either path.
func TestMetadataEditPaddedInvalidValuesNeverBecomeValid(t *testing.T) {
	refusals := []struct {
		name     string
		patch    protocol.ProviderEdit
		fileDoc  func(doc string) string
		rawProbe string
	}{
		{"padded api_key_env", protocol.ProviderEdit{ApiKeyEnv: ptrTo("  NEW_TEST_KEY  ")},
			func(doc string) string {
				return strings.Replace(doc, `"api_key_env": "META_TEST_KEY"`, `"api_key_env": "  NEW_TEST_KEY  "`, 1)
			}, `"api_key_env": "  NEW_TEST_KEY  "`},
		{"padded system_role", protocol.ProviderEdit{SystemRole: ptrTo(protocol.SystemRole("  developer  "))},
			func(doc string) string {
				return strings.Replace(doc, `"prov": {`, `"prov": {"system_role": "  developer  ",`, 1)
			}, `"system_role": "  developer  "`},
	}

	// The edit API refuses every padded invalid value with the owning file
	// and publication untouched.
	_, svc, sub := newOverlayFixture(t)
	before, first, warnRev := metadataBaseline(t, svc, svc.configPath)
	for _, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", row.patch))
			assertMetadataRefused(t, svc, sub, svc.configPath, before, first, warnRev, err, ErrConfiguration)
		})
	}

	// The identical file-written member makes the provider invalid in the
	// published catalog: the reload publishes, the provider is gone from
	// the effective view, and the padded raw value stays in the file.
	for _, row := range refusals {
		t.Run(row.name+"/file", func(t *testing.T) {
			h, svc, _ := newOverlayFixture(t)
			writeServiceFile(t, h.configPath, row.fileDoc(metadataConfigDocument))
			viaFile, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			testassert.Require(t, viaFile.snapshot.catalog.Providers["prov"] == nil, "the padded %s became effective through the file path", row.name)
			{
				data := mustRead(t, h.configPath)
				testassert.Require(t, strings.Contains(string(data), row.rawProbe), "the raw file = %q, want the padded value retained", data)
			}
		})
	}
}

// TestMetadataEditRetiredProviderControlsFollowUnknownFieldPolicy pins the
// retired controls' shared unknown-field policy through the file path: a
// user definition carrying max_tokens_field or transport.options is an
// ordinary invalid provider, so the publication drops it under the shared
// user_config_skip warning while the raw member stays in the file untouched.
func TestMetadataEditRetiredProviderControlsFollowUnknownFieldPolicy(t *testing.T) {
	for _, row := range []struct {
		name     string
		fileDoc  func(doc string) string
		rawProbe string
	}{
		{"retired max_tokens_field", func(doc string) string {
			return strings.Replace(doc, `"prov": {`, `"prov": {"max_tokens_field": "max_completion_tokens",`, 1)
		}, `"max_tokens_field": "max_completion_tokens"`},
		{"retired transport options", func(doc string) string {
			return strings.Replace(doc, `"headers": {"X-Trace": "t1"}`, `"headers": {"X-Trace": "t1"}, "options": {"retries": 3}`, 1)
		}, `"options": {"retries": 3}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, svc, _ := newOverlayFixture(t)
			writeServiceFile(t, h.configPath, row.fileDoc(metadataConfigDocument))
			viaFile, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			testassert.Require(t, viaFile.snapshot.catalog.Providers["prov"] == nil, "the %s became effective through the file path", row.name)
			warnings := viaFile.snapshot.catalogWarnings
			testassert.Require(t, len(warnings) != 0 && warnings[0].Kind == "user_config_skip" && warnings[0].Provider == "prov", "catalog warnings = %#v, want the shared user_config_skip drop", warnings)
			{
				data := mustRead(t, h.configPath)
				testassert.Require(t, strings.Contains(string(data), row.rawProbe), "the raw file = %q, want the retired member retained", data)
			}
		})
	}
}

// TestMetadataEditExternalRemovalRefusesWithoutReconstruction pins the
// externally-removed custom provider: while the captured catalog still
// knows the identity, the latest raw layer no longer owns a definition — an
// update and a model save both apply nothing and refuse through the
// candidate check, and the raw layer is never reconstructed with a
// providers.<id> node. A model upsert under a provider whose definition is
// present stays valid (covered by the upsert suite).
func TestMetadataEditExternalRemovalRefusesWithoutReconstruction(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	// The external editor removes the provider from the latest raw layer
	// without a reload: the captured catalog still knows the identity while
	// the raw layer no longer owns a definition.
	external := `{"providers":{"other":{"transport":{"base_url":"https://other.test/v1","api_key_env":"OTHER_TEST_KEY"},"discovery":false,"models":{"o":{"context_window":9007199254740993}}}},"custom_flag":true}`
	writeServiceFile(t, h.configPath, external)
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)

	// The patch is repair-capable — it names the provider and supplies a
	// complete transport — so only the no-reconstruction rule keeps the
	// removed definition from being silently rebuilt: the latest raw layer
	// owns no definition to write, the candidate check refuses, and the
	// refusals leave everything untouched.
	name := "Reconstructed"
	fixedURL := "https://reconstructed.test/v1"
	fixedEnv := "RECONSTRUCTED_TEST_KEY"
	_, err := svc.mutate(context.Background(), svc.editProviderUpdate("prov", protocol.ProviderEdit{
		Name: &name, BaseUrl: &fixedURL, ApiKeyEnv: &fixedEnv,
	}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	modelName := "Reconstructed M"
	_, err = svc.mutate(context.Background(), svc.editModelSave("prov", "m", protocol.ModelEdit{Name: &modelName}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	// No partial patch reconstructed the removed definition.
	var rawProviders map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	if _, present := rawProviders["prov"]; present {
		t.Fatalf("the refused edits reconstructed the removed definition: %s", fileRoot(t, h.configPath)["providers"])
	}
}

// TestMetadataEditRawOnlyIdentityAddressable pins the raw-only identity
// rule: a provider that exists only in the latest raw layer (invalid, so the
// catalog dropped it) is still addressable — a non-repairing update refuses
// through the candidate check, a repairing update publishes the now-valid
// subject, and the delete removes exactly the raw node with no subject
// remaining.
func TestMetadataEditRawOnlyIdentityAddressable(t *testing.T) {
	rawOnlyDoc := strings.Replace(metadataConfigDocument, `"other": {`,
		`"brokenp": {"transport": {"api_key_env": "BROKEN_TEST_KEY"}, "models": {"m": {}}}, "brokenq": {"transport": {"api_key_env": "BROKENQ_TEST_KEY"}, "models": {"m": {}}}, "other": {`, 1)
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, rawOnlyDoc)
	svc, sub := metadataService(t, h)
	testassert.Require(t, svc.current().catalog.Providers["brokenp"] == nil && svc.current().catalog.Providers["brokenq"] == nil, "the raw-only providers were not dropped by the initial publication")

	// A non-repairing update applies to the raw node and refuses through the
	// candidate check: the identity stays addressable, the file unchanged.
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	padded := "Still Broken"
	_, err := svc.mutate(context.Background(), svc.editProviderUpdate("brokenp", protocol.ProviderEdit{Name: &padded}))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)

	// The repairing update publishes the now-valid subject.
	fixed := "https://fixed.test/v1"
	candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("brokenp", protocol.ProviderEdit{BaseUrl: &fixed}))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 2, "repairing update = (%v, generation %d), want the repaired publication at 2", err, candidate.snapshot.generation)
	repaired := candidate.snapshot.catalog.Providers["brokenp"]
	testassert.Require(t, repaired != nil && repaired.Transport.BaseURL == fixed, "repaired provider = %+v, want the valid subject with its supplied base URL", repaired)
	drainConnectionEvent(t, svc.warnings, sub, "2")

	// The raw-only delete removes exactly the raw node; no subject remains.
	deletion, err := svc.mutate(context.Background(), svc.editProviderDelete("brokenq"))
	testassert.Require(t, err == nil && deletion.snapshot.generation == 3, "raw-only delete = (%v, generation %d), want a publication at 3", err, deletion.snapshot.generation)
	drainConnectionEvent(t, svc.warnings, sub, "3")
	testassert.Require(t, deletion.snapshot.catalog.Providers["brokenq"] == nil, "the raw-only delete left the subject in the candidate catalog")
	var rawProviders map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(fileRoot(t, h.configPath)["providers"], &rawProviders))
	if _, present := rawProviders["brokenq"]; present {
		t.Fatalf("raw providers = %s, want the raw-only node gone", fileRoot(t, h.configPath)["providers"])
	}
	{
		_, present := rawProviders["brokenp"]
		testassert.Require(t, present, "the raw-only delete touched its repaired sibling")
	}
}

// TestMetadataEditProviderTargetUsesSharedRawObjectAccess pins the provider
// target's absent/null/non-object outcomes while its raw navigation reuses
// rawObjectMember: absent and explicit null raw-only identities remain
// not-found, a present non-object remains the typed raw-configuration
// failure, and a builtin's explicit null override still scaffolds on a write.
func TestMetadataEditProviderTargetUsesSharedRawObjectAccess(t *testing.T) {
	for _, row := range []struct {
		name string
		doc  string
		want error
	}{
		{"absent", metadataConfigDocument, catalog.ErrUnknownProvider},
		{"null", strings.Replace(metadataConfigDocument, `"other": {`, `"nullprov": null, "other": {`, 1), catalog.ErrUnknownProvider},
		{"non-object", strings.Replace(metadataConfigDocument, `"other": {`, `"scalarprov": 5, "other": {`, 1), ErrConfiguration},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newMetadataHarness(t)
			writeServiceFile(t, h.configPath, row.doc)
			svc, sub := metadataService(t, h)
			before, first, warnRev := metadataBaseline(t, svc, h.configPath)
			id := "absentprov"
			if row.name == "null" {
				id = "nullprov"
			} else if row.name == "non-object" {
				id = "scalarprov"
			}
			name := "X"
			_, err := svc.mutate(context.Background(), svc.editProviderUpdate(id, protocol.ProviderEdit{Name: &name}))
			assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, row.want)
		})
	}

	// A null raw value does not prevent an effective builtin's normal
	// absent-override scaffold on a write path.
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataBuiltinOverrideDocument())
	svc, _ := metadataService(t, h)
	writeServiceFile(t, h.configPath, `{"providers":{"bstatic":null}}`)
	name := "Null Override Replaced"
	candidate, err := svc.mutate(context.Background(), svc.editProviderUpdate("bstatic", protocol.ProviderEdit{Name: &name}))
	testassert.Require(t, err == nil && candidate.snapshot.catalog.Providers["bstatic"] != nil && candidate.snapshot.catalog.Providers["bstatic"].Name == name, "builtin null override update = (%v, %+v), want the ordinary scaffolded override", err, candidate.snapshot.catalog.Providers["bstatic"])
	{
		_, present := metadataRawProvider(t, h.configPath, "bstatic")["name"]
		testassert.Require(t, present, "the builtin null raw member was not replaced by the user override")
	}
}

// TestMetadataEditInvalidResetFields pins the closed reset enums: a field
// outside the shared vocabulary refuses invalid before any write, for the
// provider and the model reset paths alike — the retired provider field
// literals included.
func TestMetadataEditInvalidResetFields(t *testing.T) {
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	svc, sub := metadataService(t, h)
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)

	for _, field := range []protocol.ProviderField{"bogus", "options", "max_tokens_field"} {
		_, err := svc.mutate(context.Background(), svc.editProviderFieldReset("prov", field))
		assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, harness.ErrInvalid)
	}
	_, err := svc.mutate(context.Background(), svc.editModelFieldReset("prov", "m", protocol.ModelField("bogus")))
	assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, harness.ErrInvalid)
}

// TestRuntimeBuiltinWritablePositiveTable proves the builtin's writable
// members through the Runtime operator over the real shipped provider: the
// disconnected api_key_env (its bundled credential is known unset), a
// harmless non-reserved extra_body, and discovery false/true each write the
// owned raw member, publish exactly one generation with its event, and
// project the post state — with no credential value ever written.
func TestRuntimeBuiltinWritablePositiveTable(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		testassert.NoError(t, err)
		defer sub.Close()

		rows := []struct {
			name  string
			patch protocol.ProviderEdit
			check func(protocol.ProviderMutation)
		}{
			{"disconnected api_key_env", protocol.ProviderEdit{ApiKeyEnv: ptrTo("NEW_BUILTIN_TEST_KEY")},
				func(m protocol.ProviderMutation) {
					testassert.Require(t, m.Result.ApiKeyEnv == "NEW_BUILTIN_TEST_KEY" && !m.Result.Connected, "builtin env patch result = %+v, want the new name while disconnected", m.Result)
					if !strings.Contains(string(metadataRawProvider(t, e.configPath, "openrouter")["transport"]), `"NEW_BUILTIN_TEST_KEY"`) {
						t.Fatalf("builtin raw transport = %s, want the owned env name", metadataRawProvider(t, e.configPath, "openrouter")["transport"])
					}
				}},
			{"harmless extra_body", protocol.ProviderEdit{ExtraBody: &map[string]any{"provider_hint": "x"}},
				func(m protocol.ProviderMutation) {
					{
						got := *m.Result.ExtraBody
						testassert.Require(t, got["provider_hint"] == "x", "builtin extra_body result = %v, want the written member", got)
					}
				}},
			{"discovery false", protocol.ProviderEdit{Discovery: ptrTo(false)},
				func(m protocol.ProviderMutation) {
					testassert.Require(t, !m.Result.Discovery, "builtin discovery result = true, want false")
				}},
			{"discovery true", protocol.ProviderEdit{Discovery: ptrTo(true)},
				func(m protocol.ProviderMutation) {
					testassert.Require(t, m.Result.Discovery, "builtin discovery result = false, want true")
				}},
		}
		generation := 1
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				generation++
				mutation, err := r.updateProvider(ctx, "openrouter", row.patch)
				testassert.Require(t, err == nil && mutation.ConfigurationRevision.Generation == strconv.Itoa(generation), "builtin writable patch = (%v, %+v), want generation %d", err, mutation.ConfigurationRevision, generation)
				row.check(mutation)
				drainMutationEvent(t, sub, strconv.Itoa(generation))
				assertNoEvent(t, sub)
			})
		}

		// The refusal sibling stays: a credential header refuses without any
		// write (the identity members are writable now).
		before, publishedGeneration, warnRev := runtimeMutationBaseline(t, r)
		forbidden := map[string]string{"Authorization": "x"}
		_, lockErr := r.updateProvider(ctx, "openrouter", protocol.ProviderEdit{Headers: &forbidden})
		assertRuntimeMutationRefused(t, r, sub, before, publishedGeneration, warnRev, lockErr, harness.ErrInvalid)
	})
}
