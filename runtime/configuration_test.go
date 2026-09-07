package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/model"
)

const testConfigDocument = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://api.prov.test/v1", "api_key_env": ""},
      "models": {"m": {"name": "M", "context_window": 9007199254740993}}
    }
  },
  "sessions": {"auto_archive": false, "archive_after_days": 3},
  "plugins": {"alpha": {"threshold": 9007199254740993}},
  "permissions": {"allow": ["read_file"]},
  "compaction": {"summarizer_provider": "legacy"}
}`

const testAgentsDocument = `{
  "primary": {"model": "prov/m"},
  "plan": {
    "model": "prov/m",
    "system_prompt": "simple",
    "prompt": "plan prompt",
    "tools": ["plan_tool", "plan_tool", "read_file"],
    "capabilities": ["cap-b", "cap-a"],
    "lsp": false,
    "readonly": true,
    "write_dir": "/tmp/plan",
    "description": "planner",
    "subagent": true
  }
}`

func testCapabilities() []string { return []string{"cap-a", "cap-b"} }

func testSnapshot(t *testing.T) *configuration {
	t.Helper()
	snapshot, err := newConfiguration(3, []byte(testConfigDocument), []byte(testAgentsDocument), catalog.BuildInputs{}, testCapabilities())
	if err != nil {
		t.Fatalf("newConfiguration: %v", err)
	}
	return snapshot
}

// TestNewConfigurationSnapshot proves the private snapshot consumes exactly
// one decoded input set: the sessions policy, the plugin values and the
// catalog layers keep exact numbers through the UseNumber decode, the
// definition roster is the retained loader's complete resolved output, and
// the publication generation is carried unchanged.
func TestNewConfigurationSnapshot(t *testing.T) {
	snapshot := testSnapshot(t)

	if snapshot.generation != 3 {
		t.Fatalf("generation = %d, want the supplied publication generation", snapshot.generation)
	}
	if snapshot.sessions.AutoArchive || snapshot.sessions.ArchiveAfterDays != 3 || snapshot.sessions.DeleteAfterArchiveDays != 7 {
		t.Fatalf("sessions = %+v, want the decoded policy over the true/7/7 defaults", snapshot.sessions)
	}

	provider, found := snapshot.catalog.Providers["prov"]
	if !found {
		t.Fatalf("captured providers never reached the assembled catalog: %+v", snapshot.catalog)
	}
	if modelEntry := provider.Models["m"]; modelEntry == nil || modelEntry.ContextWindow != 9007199254740993 {
		t.Fatalf("catalog model = %+v, want the exact integer preserved through the decode", modelEntry)
	}
	plugin, ok := snapshot.plugins["alpha"]
	if !ok {
		t.Fatalf("plugins = %+v, want the alpha entry", snapshot.plugins)
	}
	var pluginFields map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(plugin)))
	decoder.UseNumber()
	if err := decoder.Decode(&pluginFields); err != nil {
		t.Fatalf("plugin value must stay valid JSON: %v", err)
	}
	if number, isNumber := pluginFields["threshold"].(json.Number); !isNumber || number.String() != "9007199254740993" {
		t.Fatalf("plugin threshold = %#v, want the exact json.Number preserved", pluginFields["threshold"])
	}

	reference, err := agents.ParseWithCapabilities([]byte(testAgentsDocument), testCapabilities())
	if err != nil {
		t.Fatalf("reference ParseWithCapabilities: %v", err)
	}
	if want := reference.All(); !reflect.DeepEqual(snapshot.definitions, want) {
		t.Fatalf("definitions = %+v, want the retained roster with inherited fields, built-in overlays and prompt bytes", snapshot.definitions)
	}
	for _, def := range snapshot.definitions { // the complete private fields stay for their owning later phases
		if def.Name != "plan" {
			continue
		}
		if def.LSP || !def.Readonly || def.WriteDir != "/tmp/plan" || def.Description != "planner" || !def.Subagent || def.Builtin {
			t.Fatalf("resolved plan lost its retained fields: %+v", def)
		}
	}
	if len(snapshot.catalogWarnings) != 0 || len(snapshot.agentWarnings) != 0 {
		t.Fatalf("valid input produced warnings: catalog %+v agents %+v", snapshot.catalogWarnings, snapshot.agentWarnings)
	}
}

// TestNewConfigurationNullAndDefaults proves the target documents accept one
// object or null-as-empty everywhere: absent or null sections keep the
// true/7/7 session defaults, a null agents document resolves the built-in
// roster, and the legacy whole-document rules (old-shape rejection,
// permission decoding) never reject the captured bytes.
func TestNewConfigurationNullAndDefaults(t *testing.T) {
	for _, doc := range []string{`{}`, `{"sessions":null,"plugins":null,"providers":null}`} {
		snapshot, err := newConfiguration(1, []byte(doc), []byte(`null`), catalog.BuildInputs{}, nil)
		if err != nil {
			t.Fatalf("newConfiguration(%s): %v", doc, err)
		}
		if !snapshot.sessions.AutoArchive || snapshot.sessions.ArchiveAfterDays != 7 || snapshot.sessions.DeleteAfterArchiveDays != 7 {
			t.Fatalf("sessions under %s = %+v, want true/7/7", doc, snapshot.sessions)
		}
		if snapshot.plugins != nil {
			t.Fatalf("plugins under %s = %+v, want none", doc, snapshot.plugins)
		}
		wantBuiltins := []string{"primary", "secondary", "explore", "review", "compact"}
		gotNames := make([]string, len(snapshot.definitions))
		for i, def := range snapshot.definitions {
			gotNames[i] = def.Name
		}
		if !reflect.DeepEqual(gotNames, wantBuiltins) {
			t.Fatalf("definitions under %s = %q, want the five built-ins", doc, gotNames)
		}
	}
}

// TestNewConfigurationRejectsCandidates proves every malformed consumed
// input rejects the candidate: trailing or non-object documents, malformed
// or non-object sections, and the overflowed session days never produce a
// partially usable snapshot.
func TestNewConfigurationRejectsCandidates(t *testing.T) {
	for _, row := range []struct {
		name       string
		configData string
		agentsData string
	}{
		{name: "config not an object", configData: `[]`, agentsData: `{}`},
		{name: "config trailing value", configData: `{} {}`, agentsData: `{}`},
		{name: "config truncated", configData: `{"providers":`, agentsData: `{}`},
		{name: "sessions malformed", configData: `{"sessions":{"auto_archive":}}`, agentsData: `{}`},
		{name: "sessions not an object", configData: `{"sessions":5}`, agentsData: `{}`},
		{name: "sessions day overflow", configData: `{"sessions":{"archive_after_days":106752}}`, agentsData: `{}`},
		{name: "delete day overflow", configData: `{"sessions":{"delete_after_archive_days":106752}}`, agentsData: `{}`},
		{name: "plugins not an object", configData: `{"plugins":[]}`, agentsData: `{}`},
		{name: "agents not an object", configData: `{}`, agentsData: `[]`},
	} {
		if snapshot, err := newConfiguration(1, []byte(row.configData), []byte(row.agentsData), catalog.BuildInputs{}, nil); err == nil {
			t.Fatalf("%s produced %+v, want rejection", row.name, snapshot)
		}
	}
}

// TestNewConfigurationKeepsExistingWarningTypes proves non-fatal catalog and
// definition problems are retained in the existing warning types instead of
// failing the candidate.
func TestNewConfigurationKeepsExistingWarningTypes(t *testing.T) {
	snapshot, err := newConfiguration(0,
		[]byte(`{"providers":{"broken":5}}`),
		[]byte(`{"ghost":{"capabilities":["not-declared"]}}`),
		catalog.BuildInputs{}, nil)
	if err != nil {
		t.Fatalf("newConfiguration: %v", err)
	}
	if len(snapshot.catalogWarnings) != 1 || snapshot.catalogWarnings[0].Kind != "user_config_skip" {
		t.Fatalf("catalog warnings = %+v, want the retained catalog warning type", snapshot.catalogWarnings)
	}
	if len(snapshot.agentWarnings) != 1 || snapshot.agentWarnings[0].Kind != "invalid_agent_type" || snapshot.agentWarnings[0].Name != "ghost" {
		t.Fatalf("agent warnings = %+v, want the retained invalid-agent drop", snapshot.agentWarnings)
	}
}

// TestConfigurationAgentTypesProjection proves the per-Agent view over the
// reused instances: the roster order is preserved, identity and prompts
// survive as public values, the capability selection reaches the Harness
// unchanged, and every returned slice is owned by the caller.
func TestConfigurationAgentTypesProjection(t *testing.T) {
	snapshot := testSnapshot(t)
	types := snapshot.agentTypes()

	want := []string{"primary", "secondary", "explore", "review", "compact", "plan"}
	got := make([]string, len(types))
	for i, at := range types {
		got[i] = at.Name
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected roster = %q, want %q", got, want)
	}

	plan, err := harness.ResolveAgentType("plan", types)
	if err != nil {
		t.Fatalf("ResolveAgentType(plan): %v", err)
	}
	if plan.Model != (model.ModelRef{Provider: "prov", Model: "m"}) || plan.SystemPrompt != "simple" || plan.Prompt != "plan prompt" {
		t.Fatalf("plan view = %+v, want the public identity and prompt fields", plan)
	}
	if wantTools := []string{"plan_tool", "plan_tool", "read_file"}; !reflect.DeepEqual(plan.Tools, wantTools) {
		t.Fatalf("plan tools = %q, want the retained configured names", plan.Tools)
	}
	if wantCaps := []string{"cap-b", "cap-a"}; !reflect.DeepEqual(plan.Capabilities, wantCaps) {
		t.Fatalf("plan capabilities = %q, want the selected order", plan.Capabilities)
	}

	// Two Agent types selected from one projection keep their own views:
	// explore inherits primary's tools with no capability selection of its
	// own, so declared-but-unselected IDs never appear in it.
	explore, err := harness.ResolveAgentType("explore", types)
	if err != nil {
		t.Fatalf("ResolveAgentType(explore): %v", err)
	}
	if explore.Model != plan.Model || explore.Capabilities != nil || len(explore.Tools) != len(agents.StandardTools) {
		t.Fatalf("explore view = %+v, want the inherited model and an empty selection", explore)
	}
	if _, err := harness.ResolveAgentType("not-loaded", types); !errors.Is(err, harness.ErrInvalid) {
		t.Fatalf("unknown selection = %v, want ErrInvalid", err)
	}

	// Alias isolation: mutating a projection never feeds back into the
	// snapshot or a later projection of the same snapshot.
	plan.Tools[0] = "tampered"
	plan.Capabilities[0] = "tampered"
	again, err := harness.ResolveAgentType("plan", snapshot.agentTypes())
	if err != nil {
		t.Fatalf("re-project: %v", err)
	}
	if again.Tools[0] != "plan_tool" || again.Capabilities[0] != "cap-b" {
		t.Fatalf("projection aliases the snapshot storage: %+v", again)
	}
}
