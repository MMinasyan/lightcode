package agents

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// retainedSummarizerPrompt is the compact built-in's retained summarizer
// prompt, pinned here as an independent literal of the resolved compact
// definition.
const retainedSummarizerPrompt = `You are summarizing a coding agent conversation so the agent can continue working with reduced context. Your summary must preserve everything the agent needs to continue without re-reading files or re-asking the user.

Produce a summary with these sections:

## Goal
What the user asked for, in their words. Include exact quotes of key instructions.

## Files
Which files were read, created, or modified. For modified files: what changed and why.

## Current state
What has been done so far. What works, what doesn't.

## Unfinished work
Tasks started but not completed. Errors not yet resolved. Things the agent said it would do next.

## Decisions
Choices the user made or confirmed. Constraints they stated. Things they rejected.

Be specific — include file paths, function names, error messages, line numbers. Do not generalize. If the user said "don't use React," write that exactly, not "user has framework preferences."`

func TestResolveBuiltinsInheritThroughPrimary(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{"primary":{"model":"test/base-model"},"secondary":{"model":"test/secondary-model"}}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	secondary, err := cfg.Resolve("secondary")
	testassert.NoError(t, err)
	testassert.Require(t, secondary.Model == "test/secondary-model" && secondary.SystemPrompt == SystemPromptSimple, "secondary = model:%q system_prompt:%q, want its own overlay and simple prompt", secondary.Model, secondary.SystemPrompt)

	explore, err := cfg.Resolve("explore")
	testassert.NoError(t, err)
	testassert.Require(t, explore.Model == "test/secondary-model" && explore.SystemPrompt == SystemPromptSimple && explore.LSP && explore.Subagent && explore.Readonly, "got %v, want %v; got %v, want %v; explore booleans = lsp:%v subagent:%v readonly:%v", explore.Model, "test/secondary-model", explore.SystemPrompt, SystemPromptSimple, explore.LSP, explore.Subagent, explore.Readonly)

	review, err := cfg.Resolve("review")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(review.Tools, StandardTools) && review.Model == "test/secondary-model" && review.SystemPrompt == SystemPromptSimple, "review = model:%q system_prompt:%q tools:%v, want secondary's resolved values inherited unmodified", review.Model, review.SystemPrompt, review.Tools)
}

func TestRetainedBuiltinRosterOrderAndResolution(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, StandardTools, nil)
	testassert.NoError(t, err)
	wantOrder := []string{"primary", "secondary", "explore", "review", "compact"}
	got := namesOf(cfg.All())
	testassert.Require(t, reflect.DeepEqual(got, wantOrder), "builtin roster = %v, want %v in order", got, wantOrder)
	for _, name := range wantOrder {
		resolved, err := cfg.Resolve(name)
		testassert.Require(t, err == nil, "Resolve(%s): %v", name, err)
		testassert.Require(t, resolved.Builtin, "%s Builtin = false, want true", name)
	}

	{
		_, err := cfg.Resolve("plan")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), `unknown agent type "plan"`), "Resolve(plan) error = %v, want unknown agent type before any resolution", err)
	}
}

func TestCustomTypeNamedPlanFollowsOrdinaryRules(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "primary": {"model":"test/base-model"},
  "plan": {"prompt":"Write design notes.","tools":["read_file"],"readonly":true,"write_dir":"docs","description":"Notes writer."}
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	resolved, err := cfg.Resolve("plan")
	testassert.NoError(t, err)
	testassert.Require(t, !resolved.Builtin, `custom type "plan" marked as builtin`)
	testassert.Require(t, resolved.Model == "test/base-model" && resolved.SystemPrompt == SystemPromptSimple && reflect.DeepEqual(resolved.Tools, []string{"read_file"}), "custom plan = model:%q system_prompt:%q, want ordinary secondary inheritance of unset values; plan tools = %v, want the custom set only", resolved.Model, resolved.SystemPrompt, resolved.Tools)
	{
		got := strings.TrimSpace(resolved.Prompt)
		testassert.Require(t, got == "Write design notes." && !contains(resolved.Tools, "submit_plan"), `custom plan prompt/tools = %q/%v, want its own prompt without a built-in tool`, resolved.Prompt, resolved.Tools)
	}
	testassert.Require(t, resolved.Readonly && resolved.WriteDir == "docs" && resolved.Description == "Notes writer." && resolved.Subagent, "plan readonly/write_dir = %v/%q, want the literal custom values; custom plan description/subagent = %q/%v, want its own description and the inherited subagent flag", resolved.Readonly, resolved.WriteDir, resolved.Description, resolved.Subagent)

	all := namesOf(cfg.All())
	wantOrder := []string{"primary", "secondary", "explore", "review", "compact", "plan"}
	testassert.Require(t, reflect.DeepEqual(all, wantOrder), "All = %v, want builtins in order then the custom type", all)
}

func TestSubmitPlanToolIsRejectedInCustomDefinitions(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "planner": {"tools":["read_file","submit_plan"]},
  "reader": {"tools":["read_file"]}
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	{
		_, err := cfg.Resolve("planner")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), `unknown agent type`), `Resolve(planner with submit_plan) error = %v, want the entry dropped as unknown`, err)
	}
	reader, err := cfg.Resolve("reader")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(reader.Tools, []string{"read_file"}), "known-tool custom definition tools = %v, want it retained", reader.Tools)

	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "invalid_agent_type" && warnings[0].Name == "planner", "warnings = %#v, want one invalid custom warning naming planner", warnings)
	testassert.Require(t, strings.Contains(warnings[0].Message, `unknown tool "submit_plan"`), `warning message = %q, want the unknown submit_plan tool named`, warnings[0].Message)
}

func TestExecutePendingToolIsRejectedInCustomDefinitions(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "stager": {"tools":["read_file","execute_pending"]},
  "reader": {"tools":["read_file"]}
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	{
		_, err := cfg.Resolve("stager")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), `unknown agent type`), `Resolve(stager with execute_pending) error = %v, want the entry dropped as unknown`, err)
	}
	reader, err := cfg.Resolve("reader")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(reader.Tools, []string{"read_file"}), "known-tool custom definition tools = %v, want it retained", reader.Tools)

	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "invalid_agent_type" && warnings[0].Name == "stager", "warnings = %#v, want one invalid custom warning naming stager", warnings)
	testassert.Require(t, strings.Contains(warnings[0].Message, `unknown tool "execute_pending"`), `warning message = %q, want the unknown execute_pending tool named`, warnings[0].Message)
}

func TestBuiltinsExposeConcreteRootAndCodeOnlyCompact(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	primary, err := cfg.Resolve("primary")
	testassert.NoError(t, err)
	testassert.Require(t, primary.SystemPrompt == SystemPromptFull && reflect.DeepEqual(primary.Tools, StandardTools) && primary.LSP && !primary.Readonly && !primary.Subagent, "primary = %+v, want full prompt and standard tools; primary booleans = lsp:%v readonly:%v subagent:%v", primary, primary.LSP, primary.Readonly, primary.Subagent)

	compactAgent, err := cfg.Resolve("compact")
	testassert.NoError(t, err)
	testassert.Require(t, compactAgent.SystemPrompt == SystemPromptNone && compactAgent.Prompt == retainedSummarizerPrompt && len(compactAgent.Tools) == 0 && !compactAgent.LSP && compactAgent.Readonly && !compactAgent.Subagent, "compact prompt = system:%q body:%q; compact capability fields = %+v", compactAgent.SystemPrompt, compactAgent.Prompt, compactAgent)
}

func TestToolsPresenceControlsInheritanceEmptyAndSubset(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "empty": {"tools": []},
  "reader": {"tools": ["read_file", "run_command"]}
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)

	empty, err := cfg.Resolve("empty")
	testassert.NoError(t, err)
	testassert.Require(t, len(empty.Tools) == 0, "empty tools = %v, want explicit empty set", empty.Tools)
	reader, err := cfg.Resolve("reader")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(reader.Tools, []string{"read_file", "run_command"}), "reader tools = %v, want exact subset", reader.Tools)
	review, err := cfg.Resolve("review")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(review.Tools, StandardTools), "review tools = %v, want inherited standard tools", review.Tools)
}

func TestBuiltinLockedFieldsIgnoredAndModelHonored(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "explore": {
    "model": "test/explore-model",
    "system_prompt": "none",
    "tools": "not an array",
    "readonly": false,
    "description": "override"
  }
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)
	explore, err := cfg.Resolve("explore")
	testassert.NoError(t, err)
	testassert.Require(t, explore.Model == "test/explore-model" && explore.SystemPrompt == SystemPromptSimple && explore.Readonly && explore.Description != "override" && len(explore.Tools) != 0, "got %v, want %v; locked fields were not ignored: %+v", explore.Model, "test/explore-model", explore)
	if len(cfg.Warnings()) != 0 {
		t.Fatalf("warnings = %#v, want malformed locked built-in fields ignored", cfg.Warnings())
	}
}

func TestInvalidCustomDropsOnlyBadEntryWithWarning(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "good": {"tools": ["read_file"], "system_prompt": "simple", "model": "test/good"},
  "bad_tool": {"tools": ["does_not_exist"]},
  "bad_prompt": {"system_prompt": "huge"},
  "bad_model": {"model": "not-prefixed"},
  "bad_shape": "not an object",
  "bad_tools_shape": {"tools": "read_file"}
}`), nil, StandardTools, nil)
	testassert.NoError(t, err)
	{
		_, err := cfg.Resolve("good")
		testassert.NoError(t, err)
	}
	for _, name := range []string{"bad_tool", "bad_prompt", "bad_model", "bad_shape", "bad_tools_shape"} {
		{
			_, err := cfg.Resolve(name)
			testassert.Require(t, err != nil, "Resolve(%s) succeeded, want dropped", name)
		}
	}
	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 5, "warnings = %#v, want 5 invalid custom warnings", warnings)
	for _, warning := range warnings {
		testassert.Equal(t, warning.Kind, "invalid_agent_type")
	}
}

func TestBuiltinNameWinsOverUserLockedFields(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{"explore":{"prompt":"custom prompt","description":"custom","subagent":false}}`), nil, StandardTools, nil)
	testassert.NoError(t, err)
	explore, err := cfg.Resolve("explore")
	testassert.NoError(t, err)
	testassert.Require(t, explore.Prompt != "custom prompt" && explore.Description != "custom" && explore.Subagent, "builtin was overridden by colliding user entry: %+v", explore)
}

func TestPathForConfigUsesConfigDirectory(t *testing.T) {
	got := PathForConfig(filepath.Join("/tmp", "lightcode-test", "config.json"))
	want := filepath.Join("/tmp", "lightcode-test", "agents.json")
	testassert.Equal(t, got, want)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func namesOf(values []Resolved) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Name
	}
	return out
}

// TestParseWithCapabilitiesSelection pins capability and tool selection
// against the declared universes: declared values are retained in order, an
// empty capability list clears the inherited selection, and a non-declared
// capability value, a duplicate capability and a wrong-typed capabilities
// member each drop the definition with the retained invalid_agent_type
// warning. Configured tool names are retained in order, duplicates included.
func TestParseWithCapabilitiesSelection(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "plan": {"capabilities": ["cap-b", "cap-a"], "tools": ["own_tool"]},
  "sweeper": {"capabilities": []},
  "plain": {},
  "unknown_cap": {"capabilities": ["ghost"]},
  "duplicate_cap": {"capabilities": ["cap-a", "cap-a"]},
  "broken": {"capabilities": "not-a-list"},
  "custom_tools": {"tools": ["own_tool", "own_tool", "read_file"]}
}`), []string{"cap-a", "cap-b"}, []string{"own_tool", "read_file"}, nil)
	testassert.NoError(t, err)
	for _, drop := range []string{"unknown_cap", "duplicate_cap", "broken"} {
		{
			_, err := cfg.Resolve(drop)
			testassert.Require(t, err != nil, "Resolve(%s) succeeded, want dropped", drop)
		}
	}
	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 3, "warnings = %#v, want three invalid_agent_type drops", warnings)
	for _, warning := range warnings {
		testassert.Equal(t, warning.Kind, "invalid_agent_type")
	}

	plan, err := cfg.Resolve("plan")
	testassert.NoError(t, err)
	{
		want := []string{"cap-b", "cap-a"}
		testassert.Require(t, reflect.DeepEqual(plan.Capabilities, want), "plan capabilities = %q, want the selected order", plan.Capabilities)
	}
	{
		want := []string{"own_tool"}
		testassert.Require(t, reflect.DeepEqual(plan.Tools, want), "plan tools = %q, want the declared selection", plan.Tools)
	}
	// A custom type inherits through secondary and primary; the built-ins
	// keep their locked fields and an empty capability default because this
	// caller supplies no default list.
	for _, name := range []string{"sweeper", "plain", "secondary"} {
		resolved, err := cfg.Resolve(name)
		testassert.Require(t, err == nil, "Resolve(%s): %v", name, err)
		testassert.Require(t, resolved.Capabilities == nil, "%s capabilities = %q, want absent, cleared, or inherited-empty", name, resolved.Capabilities)
	}
	for _, resolved := range cfg.All() {
		testassert.Require(t, !resolved.Builtin || resolved.Capabilities == nil, "builtin %s capabilities = %q, want the empty default", resolved.Name, resolved.Capabilities)
	}
	// Configured tool names are retained without an available-tool list,
	// duplicates included; a repeated name neither drops the definition nor
	// changes the resolved order.
	worker, err := cfg.Resolve("custom_tools")
	testassert.NoError(t, err)
	{
		want := []string{"own_tool", "own_tool", "read_file"}
		testassert.Require(t, reflect.DeepEqual(worker.Tools, want), "custom_tools = %q, want %q retained", worker.Tools, want)
	}
}

func TestParseWithCapabilitiesRejectsEmptyToolNames(t *testing.T) {
	// A list containing only the empty name: the empty-name rule is the
	// only one that can fire.
	cfg, err := ParseWithCapabilities([]byte(`{"bad": {"tools": [""]}}`), nil, nil, nil)
	testassert.NoError(t, err)
	{
		_, err := cfg.Resolve("bad")
		testassert.Require(t, err != nil, "Resolve(bad) succeeded, want dropped for an empty tool name")
	}
	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "invalid_agent_type", "warnings = %#v, want one invalid_agent_type drop", warnings)
	testassert.Equal(t, warnings[0].Message, "tool name is empty")
}

// TestParseWithCapabilitiesToolUniverseValidation proves the compiled-tool
// rows: an unknown explicit custom tool name drops that definition with the
// retained warning, duplicates remain valid input, omitted and empty tool
// lists are preserved, and default and inherited tool lists are intersected
// with the compiled universe while the retained roster input itself keeps
// process and task.
func TestParseWithCapabilitiesToolUniverseValidation(t *testing.T) {
	doc := []byte(`{
  "unknown_tool": {"tools": ["own_tool", "not_compiled"]},
  "duplicate_tools": {"tools": ["own_tool", "own_tool", "read_file"]},
  "omitted": {},
  "empty": {"tools": []}
}`)
	cfg, err := ParseWithCapabilities(doc, nil, []string{"own_tool", "read_file", "write_file"}, nil)
	testassert.NoError(t, err)
	{
		_, err := cfg.Resolve("unknown_tool")
		testassert.Require(t, err != nil, "Resolve(unknown_tool) succeeded, want dropped for an uncompiled tool name")
	}
	warnings := cfg.Warnings()
	testassert.Require(t, len(warnings) == 1 && warnings[0].Kind == "invalid_agent_type" && warnings[0].Name == "unknown_tool", "warnings = %#v, want one invalid_agent_type drop for unknown_tool", warnings)
	testassert.Require(t, strings.Contains(warnings[0].Message, "not_compiled"), "warning message = %q, want the unknown tool name", warnings[0].Message)
	// Duplicates remain valid and keep their resolved order and repetition.
	worker, err := cfg.Resolve("duplicate_tools")
	testassert.NoError(t, err)
	{
		want := []string{"own_tool", "own_tool", "read_file"}
		testassert.Require(t, reflect.DeepEqual(worker.Tools, want), "duplicate_tools = %q, want %q retained", worker.Tools, want)
	}
	// Omitted stays omitted at the definition level (its resolved value still
	// inherits) and an explicit empty list stays empty.
	testassert.Require(t, cfg.defs["omitted"].Tools == nil, "omitted definition tools = %v, want nil", cfg.defs["omitted"].Tools)
	// The explicit empty list stays empty at the definition level (its
	// resolved projection normalizes zero-length slices to nil, the package's
	// empty form).
	{
		emptyDef := cfg.defs["empty"]
		testassert.Require(t, emptyDef.Tools != nil && len(*emptyDef.Tools) == 0, "empty definition tools = %#v, want an explicit empty list", emptyDef.Tools)
	}
	{
		empty, err := cfg.Resolve("empty")
		testassert.Require(t, err == nil && empty.Tools == nil, "resolved empty tools = %#v (%v), want the normalized empty form", empty.Tools, err)
	}
}

func TestParseWithCapabilitiesIntersectsDefaultTools(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, []string{"read_file", "run_command", "sleep"}, nil)
	testassert.NoError(t, err)
	primary, err := cfg.Resolve("primary")
	testassert.NoError(t, err)
	// Default and inherited tools are intersected with the compiled universe:
	// process and task stay in the retained roster input but are not
	// fabricated as executable placeholders, and nothing outside the universe
	// survives.
	{
		want := []string{"read_file", "run_command", "sleep"}
		testassert.Require(t, reflect.DeepEqual(primary.Tools, want), "primary default tools = %q, want the intersected roster %q", primary.Tools, want)
	}
	secondary, err := cfg.Resolve("secondary")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(secondary.Tools, primary.Tools), "secondary inherited tools = %q, want the intersected default %q", secondary.Tools, primary.Tools)
	// The retained roster input itself is unchanged.
	for _, name := range []string{"process", "task", "edit_file", "apply_patch"} {
		testassert.Require(t, contains(StandardTools, name), "StandardTools lost %q: %q", name, StandardTools)
	}
	// An empty compiled universe leaves the defaults empty, not fabricated.
	none, err := ParseWithCapabilities([]byte(`{}`), nil, nil, nil)
	testassert.NoError(t, err)
	nonePrimary, err := none.Resolve("primary")
	testassert.NoError(t, err)
	testassert.Require(t, len(nonePrimary.Tools) == 0, "primary tools under an empty universe = %q, want none fabricated", nonePrimary.Tools)
}

func TestParseWithCapabilitiesDefaultProjection(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
	  "plan": {"capabilities": ["cap-a"]},
	  "sweeper": {"capabilities": []},
	  "plain": {},
	  "model_a": {"model": "prov/a"},
	  "model_b": {"model": "prov/b"}
	}`), []string{"cap-a", "cap-b", "model_adaptation"}, []string{"read_file"}, []string{"model_adaptation"})
	testassert.NoError(t, err)
	// The default list installs on the built-in primary definition alone;
	// ordinary inheritance propagates it through secondary to customs.
	want := []string{"model_adaptation"}
	primary, err := cfg.Resolve("primary")
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(primary.Capabilities, want), "primary capabilities = %q, want the default %q", primary.Capabilities, want)
	for _, name := range []string{"secondary", "explore", "review", "compact", "plain", "model_a", "model_b"} {
		resolved, err := cfg.Resolve(name)
		testassert.Require(t, err == nil, "Resolve(%s): %v", name, err)
		testassert.Require(t, reflect.DeepEqual(resolved.Capabilities, want), "%s capabilities = %q, want the inherited default %q", name, resolved.Capabilities, want)
	}
	// An explicit selection overrides the default and an explicit empty list
	// clears it.
	plan, err := cfg.Resolve("plan")
	testassert.NoError(t, err)
	{
		want := []string{"cap-a"}
		testassert.Require(t, reflect.DeepEqual(plan.Capabilities, want), "plan capabilities = %q, want the explicit selection %q", plan.Capabilities, want)
	}
	sweeper, err := cfg.Resolve("sweeper")
	testassert.NoError(t, err)
	testassert.Require(t, sweeper.Capabilities == nil, "sweeper capabilities = %q, want the explicit empty clear", sweeper.Capabilities)
}

func TestParseWithCapabilitiesEmptyDefaultsLeaveBaseline(t *testing.T) {
	const doc = `{"plain": {}}`
	nilDefaults, err := ParseWithCapabilities([]byte(doc), []string{"cap-a"}, nil, nil)
	testassert.NoError(t, err)
	emptyDefaults, err := ParseWithCapabilities([]byte(doc), []string{"cap-a"}, nil, []string{})
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(nilDefaults.All(), emptyDefaults.All()), "an empty default list changed the resolved definitions")
	// Both stay byte-identical to the built-ins' empty capability default.
	for _, resolved := range nilDefaults.All() {
		testassert.Require(t, resolved.Capabilities == nil, "%s capabilities = %q, want the empty default", resolved.Name, resolved.Capabilities)
	}
}
