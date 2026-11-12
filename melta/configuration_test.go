package melta

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agents "github.com/Syntheresis/melta/extensions/agents"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/config"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/prompt"
	"github.com/Syntheresis/melta/protocol"
)

const testConfigDocument = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://api.prov.test/v1", "api_key_env": ""},"discovery": false,
      "models": {"m": {"name": "M", "context_window": 9007199254740993}}
    }
  },
  "sessions": {"auto_archive": false, "archive_after_days": 3},
  "extensions": {"alpha": {"threshold": 9007199254740993}},
  "permissions": {"allow": ["read_file"]},
  "compaction": {"summarizer_provider": "legacy"}
}`

func TestAgentSelectionProtocolModelConversion(t *testing.T) {
	for _, row := range []struct {
		wire protocol.ModelRef
		ref  model.ModelRef
	}{
		{"", model.ModelRef{}},
		{"provider/part/model", model.ModelRef{Provider: "provider", Model: "part/model"}},
	} {
		selection, err := selectionFromProtocol(protocol.AgentSelection{Inputs: &protocol.AgentInputs{Model: row.wire, AssemblyMode: protocol.AgentAssemblyFull}})
		testassert.NoError(t, err)
		testassert.Equal(t, selection.Inputs.Model, row.ref)
		projected := projectAgentSelection(selection)
		testassert.Equal(t, projected.Inputs.Model, row.wire)
		data, err := json.Marshal(projected.Inputs)
		testassert.NoError(t, err)
		var members map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(data, &members))
		var rendered string
		testassert.NoError(t, json.Unmarshal(members["model"], &rendered))
		testassert.Equal(t, rendered, string(row.wire))
	}
	_, err := selectionFromProtocol(protocol.AgentSelection{Inputs: &protocol.AgentInputs{Model: "invalid"}})
	testassert.Require(t, errors.Is(err, harness.ErrInvalid), "invalid wire model = %v", err)
}

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

// assembleConfiguration publishes one captured document set through the real
// candidate path with the supplied Agents extension, so the roster is the
// resolver projection and the carrier fields are the retained test values.
func assembleConfiguration(t *testing.T, generation uint64, configData, agentsData string, capabilities []string) (*configuration, error) {
	t.Helper()
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, configData)
	if agentsData != "" {
		writeAgentDefinitions(t, h.configPath, agentsData)
	}
	svc := h.preparedService(context.Background(), agentsTestNativeIDs(append(capabilities, agents.StandardTools...), testToolUniverse(), nil))
	captured, err := svc.publish(context.Background())
	if err != nil {
		return nil, err
	}
	captured.snapshot.generation = generation
	return captured.snapshot, nil
}

func testSnapshot(t *testing.T) *configuration {
	t.Helper()
	snapshot, err := assembleConfiguration(t, 3, testConfigDocument, testAgentsDocument, testCapabilities())
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
	plugin, ok := snapshot.extensions["alpha"]
	testassert.Require(t, ok, "extensions = %+v, want the alpha entry", snapshot.extensions)
	var pluginFields map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(plugin)))
	decoder.UseNumber()
	testassert.NoError(t, decoder.Decode(&pluginFields))
	{
		number, isNumber := pluginFields["threshold"].(json.Number)
		testassert.Require(t, isNumber && number.String() == "9007199254740993", "plugin threshold = %#v, want the exact json.Number preserved", pluginFields["threshold"])
	}

	// The roster is the supplying resolver's immutable projection; the
	// extension-owned parse/format assertions live with the extension suite.
	testassert.Require(t, len(snapshot.definitions) == 6, "definitions = %+v, want the five built-ins and the configured plan", snapshot.definitions)
	for _, def := range snapshot.definitions {
		if def.name != "plan" {
			continue
		}
		testassert.Require(t, def.inputs.Constraints.Readonly && def.inputs.Constraints.WriteDir == "/tmp/plan" && def.description == "planner" && def.subagent, "resolved plan lost its retained fields: %+v", def)
	}
	testassert.Require(t, len(snapshot.catalogWarnings) == 0, "valid input produced catalog warnings: %+v", snapshot.catalogWarnings)
}

// TestNewConfigurationNullAndDefaults proves the target documents accept one
// object or null-as-empty everywhere: absent or null sections keep the
// true/7/7 session defaults, a null agents document resolves the built-in
// roster, and the legacy whole-document rules (old-shape rejection,
// permission decoding) never reject the captured bytes.
func TestNewConfigurationNullAndDefaults(t *testing.T) {
	for _, doc := range []string{`{}`, `{"sessions":null,"extensions":null,"providers":null}`} {
		snapshot, err := assembleConfiguration(t, 1, doc, "", nil)
		testassert.Require(t, err == nil, "newConfiguration(%s): %v", doc, err)
		testassert.Require(t, snapshot.sessions.AutoArchive && snapshot.sessions.ArchiveAfterDays == 7 && snapshot.sessions.DeleteAfterArchiveDays == 7 && snapshot.extensions == nil, "sessions under %s = %+v, want true/7/7; extensions under %s = %+v, want none", doc, snapshot.sessions, doc, snapshot.extensions)
		testassert.Require(t, len(snapshot.definitions) == 0, "definitions under %s = %+v, want none without a resolver", doc, snapshot.definitions)
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
		{name: "config not an object", configData: `[]`, agentsData: ``},
		{name: "config trailing value", configData: `{} {}`, agentsData: ``},
		{name: "config truncated", configData: `{"providers":`, agentsData: ``},
		{name: "sessions malformed", configData: `{"sessions":{"auto_archive":}}`, agentsData: ``},
		{name: "sessions not an object", configData: `{"sessions":5}`, agentsData: ``},
		{name: "sessions day overflow", configData: `{"sessions":{"archive_after_days":106752}}`, agentsData: ``},
		{name: "delete day overflow", configData: `{"sessions":{"delete_after_archive_days":106752}}`, agentsData: ``},
		{name: "extensions not an object", configData: `{"extensions":[]}`, agentsData: ``},
		{name: "agents not an object", configData: `{}`, agentsData: `[]`},
	} {
		{
			snapshot, err := assembleConfiguration(t, 1, row.configData, row.agentsData, nil)
			testassert.Require(t, err != nil, "%s produced %+v, want rejection", row.name, snapshot)
		}
	}
}

// TestNewConfigurationKeepsExistingWarningTypes proves non-fatal catalog
// problems are retained in the existing warning type instead of failing the
// candidate. Definition-format rejection belongs to the owning extension's
// opening path.
func TestNewConfigurationKeepsExistingWarningTypes(t *testing.T) {
	snapshot, err := assembleConfiguration(t, 0,
		`{"providers":{"broken":5}}`,
		`{}`,
		nil)
	testassert.NoError(t, err)
	testassert.Require(t, len(snapshot.catalogWarnings) == 1 && snapshot.catalogWarnings[0].Kind == "user_config_skip", "catalog warnings = %+v, want the retained catalog warning type", snapshot.catalogWarnings)
}

// TestConfigurationAgentTypesProjection proves the production resolver and
// the source-qualified roster projection agree: the roster order is preserved,
// the resolved identity and prompts survive as public values, the capability
// selection reaches the Harness unchanged, and every returned slice is owned
// by the caller.
func TestConfigurationAgentTypesProjection(t *testing.T) {
	snapshot := testSnapshot(t)
	roster := projectAgents(snapshot)

	want := []string{"primary", "secondary", "explore", "review", "compact", "plan"}
	got := make([]string, len(roster))
	for i, agent := range roster {
		got[i] = agent.Name
	}
	testassert.Require(t, reflect.DeepEqual(got, want), "projected roster = %q, want %q", got, want)

	planRef := harness.DefinitionRef{ExtensionID: agents.ExtensionID, CapabilityID: agents.DefinitionsCapabilityID, DefinitionID: "plan"}
	plan, err := resolveDefinition(context.Background(), snapshot, planRef)
	testassert.NoError(t, err)
	testassert.Require(t, plan.Model == (model.ModelRef{Provider: "prov", Model: "m"}) && plan.AssemblyMode == "simple" && plan.Instructions == "plan prompt", "plan inputs = %+v, want the resolved identity and prompt fields", plan)
	{
		wantTools := []string{"plan_tool", "plan_tool", "read_file"}
		testassert.Require(t, reflect.DeepEqual(plan.Tools, wantTools), "plan tools = %q, want the retained configured names", plan.Tools)
	}
	{
		wantCaps := []string{"cap-b", "cap-a"}
		testassert.Require(t, reflect.DeepEqual(plan.Capabilities, wantCaps), "plan capabilities = %q, want the selected order", plan.Capabilities)
	}
	testassert.Require(t, plan.Constraints.Readonly && plan.Constraints.WriteDir == "/tmp/plan", "plan permission constraints = %+v", plan.Constraints)
	primary, err := resolveDefinition(context.Background(), snapshot, harness.DefinitionRef{ExtensionID: agents.ExtensionID, CapabilityID: agents.DefinitionsCapabilityID, DefinitionID: "primary"})
	testassert.NoError(t, err)
	testassert.Require(t, !primary.Constraints.Readonly && primary.Constraints.WriteDir == "", "primary permission constraints = %+v", primary.Constraints)

	// WriteDir is trimmed exactly once at resolution, preserving the legacy
	// whitespace-as-unset behavior: padded paths arrive clean and a
	// whitespace-only path arrives as unset.
	padded, err := assembleConfiguration(t, 4, testConfigDocument, strings.Replace(testAgentsDocument, `"write_dir": "/tmp/plan"`, `"write_dir": "  /tmp/plan  "`, 1), testCapabilities())
	testassert.NoError(t, err)
	paddedPlan, err := resolveDefinition(context.Background(), padded, planRef)
	testassert.NoError(t, err)
	testassert.Equal(t, paddedPlan.Constraints.WriteDir, "/tmp/plan")
	blank, err := assembleConfiguration(t, 5, testConfigDocument, strings.Replace(testAgentsDocument, `"write_dir": "/tmp/plan"`, `"write_dir": "   "`, 1), testCapabilities())
	testassert.NoError(t, err)
	blankPlan, err := resolveDefinition(context.Background(), blank, planRef)
	testassert.NoError(t, err)
	testassert.Equal(t, blankPlan.Constraints.WriteDir, "")

	// Two Agent types resolved from one snapshot keep their own inputs:
	// explore inherits primary's tools with no capability selection of its
	// own, so declared-but-unselected IDs never appear in it. The inherited
	// default tools are intersected with the compiled tool universe.
	explore, err := resolveDefinition(context.Background(), snapshot, harness.DefinitionRef{ExtensionID: agents.ExtensionID, CapabilityID: agents.DefinitionsCapabilityID, DefinitionID: "explore"})
	testassert.NoError(t, err)
	testassert.Require(t, explore.Model == plan.Model && explore.Capabilities == nil && reflect.DeepEqual(explore.Tools, []string{"read_file"}), "explore inputs = %+v, want the inherited model, an empty selection and the intersected default tools", explore)
	{
		_, err := resolveDefinition(context.Background(), snapshot, harness.DefinitionRef{ExtensionID: agents.ExtensionID, CapabilityID: agents.DefinitionsCapabilityID, DefinitionID: "not-loaded"})
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "unknown selection = %v, want ErrInvalid", err)
	}

	// Alias isolation: mutating one projection or one resolution never feeds
	// back into the snapshot or a later caller-owned result.
	projectedPlan := roster[len(roster)-1]
	projectedPlan.Tools[0] = "tampered"
	projectedPlan.Capabilities[0] = "tampered"
	plan.Tools[0] = "tampered"
	plan.Capabilities[0] = "tampered"
	again := projectAgents(snapshot)
	testassert.Require(t, again[len(again)-1].Tools[0] == "plan_tool" && again[len(again)-1].Capabilities[0] == "cap-b", "projection aliases the snapshot storage: %+v", again[len(again)-1])
	againPlan, err := resolveDefinition(context.Background(), snapshot, planRef)
	testassert.NoError(t, err)
	testassert.Require(t, againPlan.Tools[0] == "plan_tool" && againPlan.Capabilities[0] == "cap-b", "resolution aliases the snapshot storage: %+v", againPlan)
}

// TestInvocationAgentTypesReturnsOwnedRoster proves the public roster view:
// the snapshot's ordering and the projected subagent fields reach the caller,
// the zero Invocation carries an empty roster, and a caller's mutation of the
// returned value never reaches the configuration snapshot.
func TestInvocationAgentTypesReturnsOwnedRoster(t *testing.T) {
	inv := Invocation{snapshot: testSnapshot(t)}

	var roster []protocol.Agent = inv.AgentTypes()
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

	snapshot, err := assembleConfiguration(t, 2, globalDoc, `{"agent":{"model":"prov/m"}}`, nil)
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
