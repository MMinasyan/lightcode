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
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
    "readonly": true,
    "write_dir": "/tmp/plan",
    "description": "planner",
    "subagent": true
  }
}`

func testCapabilities() []string { return []string{"cap-a", "cap-b"} }

// testToolUniverse is the compiled tool universe of the test documents.
func testToolUniverse() []string { return []string{"plan_tool", "read_file"} }

// assembleConfiguration drives the factored snapshot entries exactly as a
// caller with one captured document set does: decode once, assemble the
// captured providers with the pure build, then hand both to newConfiguration.
func assembleConfiguration(generation uint64, configData, agentsData string, capabilityIDs []string) (*configuration, error) {
	doc, err := decodeCapturedConfig([]byte(configData))
	if err != nil {
		return nil, err
	}
	return newConfiguration(generation, doc, catalog.Build(catalog.BuildInputs{UserRaw: doc.Providers}), []byte(agentsData), capabilityIDs, testToolUniverse(), nil, nil)
}

func testSnapshot(t *testing.T) *configuration {
	t.Helper()
	snapshot, err := assembleConfiguration(3, testConfigDocument, testAgentsDocument, testCapabilities())
	testassert.NoError(t, err)
	return snapshot
}

// TestNewConfigurationSnapshot proves the private snapshot consumes exactly
// one decoded input set: the sessions policy, the plugin values and the
// catalog layers keep exact numbers through the UseNumber decode, the
// definition roster is the retained loader's complete resolved output, and
// the publication generation is carried unchanged.
func TestNewConfigurationSnapshot(t *testing.T) {
	snapshot := testSnapshot(t)

	testassert.Require(t, snapshot.generation == 3 && !snapshot.sessions.AutoArchive && snapshot.sessions.ArchiveAfterDays == 3 && snapshot.sessions.DeleteAfterArchiveDays == 7, "got %v, want %v; sessions = %+v, want the decoded policy over the true/7/7 defaults", snapshot.generation, 3, snapshot.sessions)

	provider, found := snapshot.catalog.Providers["prov"]
	testassert.Require(t, found, "captured providers never reached the assembled catalog: %+v", snapshot.catalog)
	{
		modelEntry := provider.Models["m"]
		testassert.Require(t, modelEntry != nil && modelEntry.ContextWindow == 9007199254740993, "catalog model = %+v, want the exact integer preserved through the decode", modelEntry)
	}
	// The legacy compaction member is unowned now: unknown members are
	// ignored and the absent model/prompt keep their defaults.
	testassert.Require(t, snapshot.compactionValues.model.IsZero() && snapshot.compactionValues.prompt == prompt.DefaultSummarizerPrompt, "compaction values = %+v, want the absent-section defaults", snapshot.compactionValues)
	plugin, ok := snapshot.plugins["alpha"]
	testassert.Require(t, ok, "plugins = %+v, want the alpha entry", snapshot.plugins)
	var pluginFields map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(plugin)))
	decoder.UseNumber()
	testassert.NoError(t, decoder.Decode(&pluginFields))
	{
		number, isNumber := pluginFields["threshold"].(json.Number)
		testassert.Require(t, isNumber && number.String() == "9007199254740993", "plugin threshold = %#v, want the exact json.Number preserved", pluginFields["threshold"])
	}

	reference, err := agents.ParseWithCapabilities([]byte(testAgentsDocument), testCapabilities(), testToolUniverse(), nil)
	testassert.NoError(t, err)
	{
		want := reference.All()
		testassert.Require(t, reflect.DeepEqual(snapshot.definitions, want), "definitions = %+v, want the retained roster with inherited fields, built-in overlays and prompt bytes", snapshot.definitions)
	}
	for _, def := range snapshot.definitions { // the complete private fields stay for their owning later phases
		if def.Name != "plan" {
			continue
		}
		testassert.Require(t, def.Readonly && def.WriteDir == "/tmp/plan" && def.Description == "planner" && def.Subagent && !def.Builtin, "resolved plan lost its retained fields: %+v", def)
	}
	testassert.Require(t, len(snapshot.catalogWarnings) == 0 && len(snapshot.agentWarnings) == 0, "valid input produced warnings: catalog %+v agents %+v", snapshot.catalogWarnings, snapshot.agentWarnings)
}

// TestNewConfigurationNullAndDefaults proves the target documents accept one
// object or null-as-empty everywhere: absent or null sections keep the
// true/7/7 session defaults, a null agents document resolves the built-in
// roster, and the legacy whole-document rules (old-shape rejection,
// permission decoding) never reject the captured bytes.
func TestNewConfigurationNullAndDefaults(t *testing.T) {
	for _, doc := range []string{`{}`, `{"sessions":null,"plugins":null,"providers":null}`} {
		snapshot, err := assembleConfiguration(1, doc, `null`, nil)
		testassert.Require(t, err == nil, "newConfiguration(%s): %v", doc, err)
		testassert.Require(t, snapshot.sessions.AutoArchive && snapshot.sessions.ArchiveAfterDays == 7 && snapshot.sessions.DeleteAfterArchiveDays == 7 && snapshot.plugins == nil, "sessions under %s = %+v, want true/7/7; plugins under %s = %+v, want none", doc, snapshot.sessions, doc, snapshot.plugins)
		wantBuiltins := []string{"primary", "secondary", "explore", "review", "compact"}
		gotNames := make([]string, len(snapshot.definitions))
		for i, def := range snapshot.definitions {
			gotNames[i] = def.Name
		}
		testassert.Require(t, reflect.DeepEqual(gotNames, wantBuiltins), "definitions under %s = %q, want the five built-ins", doc, gotNames)
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
		{
			snapshot, err := assembleConfiguration(1, row.configData, row.agentsData, nil)
			testassert.Require(t, err != nil, "%s produced %+v, want rejection", row.name, snapshot)
		}
	}
}

// TestNewConfigurationKeepsExistingWarningTypes proves non-fatal catalog and
// definition problems are retained in the existing warning types instead of
// failing the candidate.
func TestNewConfigurationKeepsExistingWarningTypes(t *testing.T) {
	snapshot, err := assembleConfiguration(0,
		`{"providers":{"broken":5}}`,
		`{"ghost":{"capabilities":["not-declared"]}}`,
		nil)
	testassert.NoError(t, err)
	testassert.Require(t, len(snapshot.catalogWarnings) == 1 && snapshot.catalogWarnings[0].Kind == "user_config_skip" && len(snapshot.agentWarnings) == 1 && snapshot.agentWarnings[0].Kind == "invalid_agent_type" && snapshot.agentWarnings[0].Name == "ghost", "catalog warnings = %+v, want the retained catalog warning type; agent warnings = %+v, want the retained invalid-agent drop", snapshot.catalogWarnings, snapshot.agentWarnings)
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
	testassert.Require(t, reflect.DeepEqual(got, want), "projected roster = %q, want %q", got, want)

	plan, err := harness.ResolveAgentType("plan", types)
	testassert.NoError(t, err)
	testassert.Require(t, plan.Model == (model.ModelRef{Provider: "prov", Model: "m"}) && plan.SystemPrompt == "simple" && plan.Prompt == "plan prompt", "plan view = %+v, want the public identity and prompt fields", plan)
	{
		wantTools := []string{"plan_tool", "plan_tool", "read_file"}
		testassert.Require(t, reflect.DeepEqual(plan.Tools, wantTools), "plan tools = %q, want the retained configured names", plan.Tools)
	}
	{
		wantCaps := []string{"cap-b", "cap-a"}
		testassert.Require(t, reflect.DeepEqual(plan.Capabilities, wantCaps), "plan capabilities = %q, want the selected order", plan.Capabilities)
	}
	testassert.Require(t, plan.Readonly && plan.WriteDir == "/tmp/plan" && plan.Subagent && plan.Description == "planner", "plan permission constraints = %v %q, want the definition's readonly true and its write_dir; plan roster fields = subagent %v description %q, want the definition's subagent flag and description", plan.Readonly, plan.WriteDir, plan.Subagent, plan.Description)
	primaryView, err := harness.ResolveAgentType("primary", types)
	testassert.NoError(t, err)
	testassert.Require(t, !primaryView.Readonly && primaryView.WriteDir == "" && !primaryView.Subagent && primaryView.Description == "", "primary permission constraints = %v %q, want the unset defaults; primary roster fields = subagent %v description %q, want the unset defaults", primaryView.Readonly, primaryView.WriteDir, primaryView.Subagent, primaryView.Description)

	// WriteDir is trimmed exactly once at projection, preserving the legacy
	// whitespace-as-unset behavior: padded paths arrive clean and a
	// whitespace-only path arrives as unset.
	padded, err := assembleConfiguration(4, testConfigDocument, strings.Replace(testAgentsDocument, `"write_dir": "/tmp/plan"`, `"write_dir": "  /tmp/plan  "`, 1), testCapabilities())
	testassert.NoError(t, err)
	paddedPlan, err := harness.ResolveAgentType("plan", padded.agentTypes())
	testassert.NoError(t, err)
	testassert.Equal(t, paddedPlan.WriteDir, "/tmp/plan")
	blank, err := assembleConfiguration(5, testConfigDocument, strings.Replace(testAgentsDocument, `"write_dir": "/tmp/plan"`, `"write_dir": "   "`, 1), testCapabilities())
	testassert.NoError(t, err)
	blankPlan, err := harness.ResolveAgentType("plan", blank.agentTypes())
	testassert.NoError(t, err)
	testassert.Equal(t, blankPlan.WriteDir, "")

	// Two Agent types selected from one projection keep their own views:
	// explore inherits primary's tools with no capability selection of its
	// own, so declared-but-unselected IDs never appear in it. The inherited
	// default tools are intersected with the compiled tool universe.
	explore, err := harness.ResolveAgentType("explore", types)
	testassert.NoError(t, err)
	testassert.Require(t, explore.Model == plan.Model && explore.Capabilities == nil && reflect.DeepEqual(explore.Tools, []string{"read_file"}), "explore view = %+v, want the inherited model, an empty selection and the intersected default tools", explore)
	{
		_, err := harness.ResolveAgentType("not-loaded", types)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "unknown selection = %v, want ErrInvalid", err)
	}

	// Alias isolation: mutating a projection never feeds back into the
	// snapshot or a later projection of the same snapshot.
	plan.Tools[0] = "tampered"
	plan.Capabilities[0] = "tampered"
	again, err := harness.ResolveAgentType("plan", snapshot.agentTypes())
	testassert.NoError(t, err)
	testassert.Require(t, again.Tools[0] == "plan_tool" && again.Capabilities[0] == "cap-b", "projection aliases the snapshot storage: %+v", again)
}

// TestInvocationAgentTypesReturnsOwnedRoster proves the public roster view:
// the snapshot's ordering and the projected subagent fields reach the caller,
// the zero Invocation carries an empty roster, and a caller's mutation of the
// returned value never reaches the configuration snapshot.
func TestInvocationAgentTypesReturnsOwnedRoster(t *testing.T) {
	inv := Invocation{snapshot: testSnapshot(t)}

	roster := inv.AgentTypes()
	want := []string{"primary", "secondary", "explore", "review", "compact", "plan"}
	got := make([]string, len(roster))
	for i, at := range roster {
		got[i] = at.Name
	}
	testassert.Require(t, reflect.DeepEqual(got, want), "roster order = %q, want %q", got, want)
	{
		plan := roster[len(roster)-1]
		testassert.Require(t, plan.Subagent && plan.Description == "planner", "plan roster entry = %+v, want the projected subagent fields", plan)
	}

	// A caller's mutation — of an entry's own slices and of the returned
	// slice itself — never reaches the snapshot.
	testassert.Require(t, len(roster[0].Tools) != 0, "primary tools = %v, want a projected list to mutate", roster[0].Tools)
	roster[0].Tools[0] = "tampered"
	clear(roster)

	again := inv.AgentTypes()
	testassert.Equal(t, len(again), len(want))
	testassert.Require(t, again[0].Tools[0] == "read_file", "second roster = %+v, want the pristine snapshot projection", again[0])
	{
		plan := again[len(again)-1]
		testassert.Require(t, plan.Subagent && plan.Description == "planner", "second plan entry = %+v, want the projected subagent fields", plan)
	}
	{
		empty := (Invocation{}).AgentTypes()
		testassert.Require(t, len(empty) == 0, "zero Invocation roster = %v, want empty", empty)
	}
}

// TestConfigurationPermissionCapture proves the captured permission inputs:
// the raw global member and the Workspace bytes keyed by directory ID resolve
// through the same revision, an unknown Workspace resolves global-only, a
// malformed block follows the built-in fallback posture without failing the
// candidate, and the retained legacy interactive member is malformed target
// policy.
func TestConfigurationPermissionCapture(t *testing.T) {
	const globalAllow = `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`
	const workspaceDeny = `{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}`
	const globalDoc = `{"providers":{"prov":{"transport":{"base_url":"https://api.prov.test/v1","api_key_env":""},"models":{"m":{"name":"M","context_window":100}}}},"permissions":` + globalAllow + `}`

	snapshot, err := assembleConfiguration(2, globalDoc, `{"agent":{"model":"prov/m"}}`, nil)
	testassert.NoError(t, err)
	testassert.Require(t, string(snapshot.permissions) == globalAllow, "captured global member = %q, want the raw bytes unchanged", snapshot.permissions)
	wsID, err := config.WorkspacePermissionDirID("/ws")
	testassert.NoError(t, err)
	snapshot.workspacePermissions = map[string]json.RawMessage{wsID: json.RawMessage(workspaceDeny)}

	// The Workspace rule decides over the global rule from the same capture.
	want := harness.ResolvePermissionPolicy(json.RawMessage(globalAllow), json.RawMessage(workspaceDeny))
	{
		got := snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, want), "workspace policy = %#v, want the resolved workspace-over-global capture", got)
	}
	// An unknown Workspace resolves global-only.
	globalOnly := harness.ResolvePermissionPolicy(json.RawMessage(globalAllow), nil)
	{
		got := snapshot.permissionPolicy("/other")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "unknown-workspace policy = %#v, want the global-only capture", got)
	}
	// A malformed Workspace block discards both user levels for that
	// Workspace's resolutions: built-in fallback despite the valid global
	// member.
	snapshot.workspacePermissions = map[string]json.RawMessage{wsID: json.RawMessage(`{"rules":5}`)}
	var builtin harness.PermissionPolicy
	{
		got := snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, builtin), "malformed-workspace policy = %#v, want the built-in fallback", got)
	}
	{
		got := snapshot.permissionPolicy("/other")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "other-workspace policy after the malformed sibling = %#v, want the untouched global capture", got)
	}

	// The retained legacy interactive permissions shape is not migrated: it
	// is malformed target policy, and the candidate still builds.
	legacy := testSnapshot(t)
	{
		got := legacy.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, builtin), "legacy-member policy = %#v, want the built-in fallback", got)
	}
}
