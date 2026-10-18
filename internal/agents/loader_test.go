package agents

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	secondary, err := cfg.Resolve("secondary")
	if err != nil {
		t.Fatalf("Resolve(secondary): %v", err)
	}
	if secondary.Model != "test/secondary-model" || secondary.SystemPrompt != SystemPromptSimple {
		t.Fatalf("secondary = model:%q system_prompt:%q, want its own overlay and simple prompt", secondary.Model, secondary.SystemPrompt)
	}

	explore, err := cfg.Resolve("explore")
	if err != nil {
		t.Fatalf("Resolve(explore): %v", err)
	}
	if explore.Model != "test/secondary-model" {
		t.Fatalf("explore model = %q, want secondary inheritance", explore.Model)
	}
	if explore.SystemPrompt != SystemPromptSimple {
		t.Fatalf("explore system prompt = %q, want simple", explore.SystemPrompt)
	}
	if !explore.LSP || !explore.Subagent || !explore.Readonly {
		t.Fatalf("explore booleans = lsp:%v subagent:%v readonly:%v", explore.LSP, explore.Subagent, explore.Readonly)
	}

	review, err := cfg.Resolve("review")
	if err != nil {
		t.Fatalf("Resolve(review): %v", err)
	}
	if !reflect.DeepEqual(review.Tools, StandardTools) || review.Model != "test/secondary-model" || review.SystemPrompt != SystemPromptSimple {
		t.Fatalf("review = model:%q system_prompt:%q tools:%v, want secondary's resolved values inherited unmodified", review.Model, review.SystemPrompt, review.Tools)
	}
}

func TestRetainedBuiltinRosterOrderAndResolution(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	wantOrder := []string{"primary", "secondary", "explore", "review", "compact"}
	got := namesOf(cfg.All())
	if !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("builtin roster = %v, want %v in order", got, wantOrder)
	}
	for _, name := range wantOrder {
		resolved, err := cfg.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		if !resolved.Builtin {
			t.Fatalf("%s Builtin = false, want true", name)
		}
	}

	if _, err := cfg.Resolve("plan"); err == nil || !strings.Contains(err.Error(), `unknown agent type "plan"`) {
		t.Fatalf("Resolve(plan) error = %v, want unknown agent type before any resolution", err)
	}
}

func TestCustomTypeNamedPlanFollowsOrdinaryRules(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "primary": {"model":"test/base-model"},
  "plan": {"prompt":"Write design notes.","tools":["read_file"],"readonly":true,"write_dir":"docs","description":"Notes writer."}
}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	resolved, err := cfg.Resolve("plan")
	if err != nil {
		t.Fatalf("Resolve(plan): %v", err)
	}
	if resolved.Builtin {
		t.Fatal(`custom type "plan" marked as builtin`)
	}
	if resolved.Model != "test/base-model" || resolved.SystemPrompt != SystemPromptSimple {
		t.Fatalf(`custom plan = model:%q system_prompt:%q, want ordinary secondary inheritance of unset values`, resolved.Model, resolved.SystemPrompt)
	}
	if !reflect.DeepEqual(resolved.Tools, []string{"read_file"}) {
		t.Fatalf("plan tools = %v, want the custom set only", resolved.Tools)
	}
	if got := strings.TrimSpace(resolved.Prompt); got != "Write design notes." || contains(resolved.Tools, "submit_plan") {
		t.Fatalf(`custom plan prompt/tools = %q/%v, want its own prompt without a built-in tool`, resolved.Prompt, resolved.Tools)
	}
	if !resolved.Readonly || resolved.WriteDir != "docs" {
		t.Fatalf("plan readonly/write_dir = %v/%q, want the literal custom values", resolved.Readonly, resolved.WriteDir)
	}
	if resolved.Description != "Notes writer." || !resolved.Subagent {
		t.Fatalf("custom plan description/subagent = %q/%v, want its own description and the inherited subagent flag", resolved.Description, resolved.Subagent)
	}

	all := namesOf(cfg.All())
	wantOrder := []string{"primary", "secondary", "explore", "review", "compact", "plan"}
	if !reflect.DeepEqual(all, wantOrder) {
		t.Fatalf("All = %v, want builtins in order then the custom type", all)
	}
}

func TestSubmitPlanToolIsRejectedInCustomDefinitions(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "planner": {"tools":["read_file","submit_plan"]},
  "reader": {"tools":["read_file"]}
}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	if _, err := cfg.Resolve("planner"); err == nil || !strings.Contains(err.Error(), `unknown agent type`) {
		t.Fatalf(`Resolve(planner with submit_plan) error = %v, want the entry dropped as unknown`, err)
	}
	reader, err := cfg.Resolve("reader")
	if err != nil {
		t.Fatalf("Resolve(reader): %v", err)
	}
	if !reflect.DeepEqual(reader.Tools, []string{"read_file"}) {
		t.Fatalf("known-tool custom definition tools = %v, want it retained", reader.Tools)
	}

	warnings := cfg.Warnings()
	if len(warnings) != 1 || warnings[0].Kind != "invalid_agent_type" || warnings[0].Name != "planner" {
		t.Fatalf("warnings = %#v, want one invalid custom warning naming planner", warnings)
	}
	if !strings.Contains(warnings[0].Message, `unknown tool "submit_plan"`) {
		t.Fatalf(`warning message = %q, want the unknown submit_plan tool named`, warnings[0].Message)
	}
}

func TestExecutePendingToolIsRejectedInCustomDefinitions(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "stager": {"tools":["read_file","execute_pending"]},
  "reader": {"tools":["read_file"]}
}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	if _, err := cfg.Resolve("stager"); err == nil || !strings.Contains(err.Error(), `unknown agent type`) {
		t.Fatalf(`Resolve(stager with execute_pending) error = %v, want the entry dropped as unknown`, err)
	}
	reader, err := cfg.Resolve("reader")
	if err != nil {
		t.Fatalf("Resolve(reader): %v", err)
	}
	if !reflect.DeepEqual(reader.Tools, []string{"read_file"}) {
		t.Fatalf("known-tool custom definition tools = %v, want it retained", reader.Tools)
	}

	warnings := cfg.Warnings()
	if len(warnings) != 1 || warnings[0].Kind != "invalid_agent_type" || warnings[0].Name != "stager" {
		t.Fatalf("warnings = %#v, want one invalid custom warning naming stager", warnings)
	}
	if !strings.Contains(warnings[0].Message, `unknown tool "execute_pending"`) {
		t.Fatalf(`warning message = %q, want the unknown execute_pending tool named`, warnings[0].Message)
	}
}

func TestBuiltinsExposeConcreteRootAndCodeOnlyCompact(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	primary, err := cfg.Resolve("primary")
	if err != nil {
		t.Fatalf("Resolve(primary): %v", err)
	}
	if primary.SystemPrompt != SystemPromptFull || !reflect.DeepEqual(primary.Tools, StandardTools) {
		t.Fatalf("primary = %+v, want full prompt and standard tools", primary)
	}
	if !primary.LSP || primary.Readonly || primary.Subagent {
		t.Fatalf("primary booleans = lsp:%v readonly:%v subagent:%v", primary.LSP, primary.Readonly, primary.Subagent)
	}

	compactAgent, err := cfg.Resolve("compact")
	if err != nil {
		t.Fatalf("Resolve(compact): %v", err)
	}
	if compactAgent.SystemPrompt != SystemPromptNone || compactAgent.Prompt != retainedSummarizerPrompt {
		t.Fatalf("compact prompt = system:%q body:%q", compactAgent.SystemPrompt, compactAgent.Prompt)
	}
	if len(compactAgent.Tools) != 0 || compactAgent.LSP || !compactAgent.Readonly || compactAgent.Subagent {
		t.Fatalf("compact capability fields = %+v", compactAgent)
	}
}

func TestToolsPresenceControlsInheritanceEmptyAndSubset(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
  "empty": {"tools": []},
  "reader": {"tools": ["read_file", "run_command"]}
}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}

	empty, err := cfg.Resolve("empty")
	if err != nil {
		t.Fatalf("Resolve(empty): %v", err)
	}
	if len(empty.Tools) != 0 {
		t.Fatalf("empty tools = %v, want explicit empty set", empty.Tools)
	}
	reader, err := cfg.Resolve("reader")
	if err != nil {
		t.Fatalf("Resolve(reader): %v", err)
	}
	if !reflect.DeepEqual(reader.Tools, []string{"read_file", "run_command"}) {
		t.Fatalf("reader tools = %v, want exact subset", reader.Tools)
	}
	review, err := cfg.Resolve("review")
	if err != nil {
		t.Fatalf("Resolve(review): %v", err)
	}
	if !reflect.DeepEqual(review.Tools, StandardTools) {
		t.Fatalf("review tools = %v, want inherited standard tools", review.Tools)
	}
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
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	explore, err := cfg.Resolve("explore")
	if err != nil {
		t.Fatalf("Resolve(explore): %v", err)
	}
	if explore.Model != "test/explore-model" {
		t.Fatalf("explore model = %q, want override", explore.Model)
	}
	if explore.SystemPrompt != SystemPromptSimple || !explore.Readonly || explore.Description == "override" || len(explore.Tools) == 0 {
		t.Fatalf("locked fields were not ignored: %+v", explore)
	}
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
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	if _, err := cfg.Resolve("good"); err != nil {
		t.Fatalf("Resolve(good): %v", err)
	}
	for _, name := range []string{"bad_tool", "bad_prompt", "bad_model", "bad_shape", "bad_tools_shape"} {
		if _, err := cfg.Resolve(name); err == nil {
			t.Fatalf("Resolve(%s) succeeded, want dropped", name)
		}
	}
	warnings := cfg.Warnings()
	if len(warnings) != 5 {
		t.Fatalf("warnings = %#v, want 5 invalid custom warnings", warnings)
	}
	for _, warning := range warnings {
		if warning.Kind != "invalid_agent_type" {
			t.Fatalf("warning kind = %q, want invalid_agent_type", warning.Kind)
		}
	}
}

func TestBuiltinNameWinsOverUserLockedFields(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{"explore":{"prompt":"custom prompt","description":"custom","subagent":false}}`), nil, StandardTools, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	explore, err := cfg.Resolve("explore")
	if err != nil {
		t.Fatalf("Resolve(explore): %v", err)
	}
	if explore.Prompt == "custom prompt" || explore.Description == "custom" || !explore.Subagent {
		t.Fatalf("builtin was overridden by colliding user entry: %+v", explore)
	}
}

func TestPathForConfigUsesConfigDirectory(t *testing.T) {
	got := PathForConfig(filepath.Join("/tmp", "lightcode-test", "config.json"))
	want := filepath.Join("/tmp", "lightcode-test", "agents.json")
	if got != want {
		t.Fatalf("PathForConfig = %q, want %q", got, want)
	}
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
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	for _, drop := range []string{"unknown_cap", "duplicate_cap", "broken"} {
		if _, err := cfg.Resolve(drop); err == nil {
			t.Fatalf("Resolve(%s) succeeded, want dropped", drop)
		}
	}
	warnings := cfg.Warnings()
	if len(warnings) != 3 {
		t.Fatalf("warnings = %#v, want three invalid_agent_type drops", warnings)
	}
	for _, warning := range warnings {
		if warning.Kind != "invalid_agent_type" {
			t.Fatalf("warning kind = %q, want invalid_agent_type", warning.Kind)
		}
	}

	plan, err := cfg.Resolve("plan")
	if err != nil {
		t.Fatalf("Resolve(plan): %v", err)
	}
	if want := []string{"cap-b", "cap-a"}; !reflect.DeepEqual(plan.Capabilities, want) {
		t.Fatalf("plan capabilities = %q, want the selected order", plan.Capabilities)
	}
	if want := []string{"own_tool"}; !reflect.DeepEqual(plan.Tools, want) {
		t.Fatalf("plan tools = %q, want the declared selection", plan.Tools)
	}
	// A custom type inherits through secondary and primary; the built-ins
	// keep their locked fields and an empty capability default because this
	// caller supplies no default list.
	for _, name := range []string{"sweeper", "plain", "secondary"} {
		resolved, err := cfg.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		if resolved.Capabilities != nil {
			t.Fatalf("%s capabilities = %q, want absent, cleared, or inherited-empty", name, resolved.Capabilities)
		}
	}
	for _, resolved := range cfg.All() {
		if resolved.Builtin && resolved.Capabilities != nil {
			t.Fatalf("builtin %s capabilities = %q, want the empty default", resolved.Name, resolved.Capabilities)
		}
	}
	// Configured tool names are retained without an available-tool list,
	// duplicates included; a repeated name neither drops the definition nor
	// changes the resolved order.
	worker, err := cfg.Resolve("custom_tools")
	if err != nil {
		t.Fatalf("Resolve(custom_tools): %v", err)
	}
	if want := []string{"own_tool", "own_tool", "read_file"}; !reflect.DeepEqual(worker.Tools, want) {
		t.Fatalf("custom_tools = %q, want %q retained", worker.Tools, want)
	}
}

func TestParseWithCapabilitiesRejectsEmptyToolNames(t *testing.T) {
	// A list containing only the empty name: the empty-name rule is the
	// only one that can fire.
	cfg, err := ParseWithCapabilities([]byte(`{"bad": {"tools": [""]}}`), nil, nil, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	if _, err := cfg.Resolve("bad"); err == nil {
		t.Fatal("Resolve(bad) succeeded, want dropped for an empty tool name")
	}
	warnings := cfg.Warnings()
	if len(warnings) != 1 || warnings[0].Kind != "invalid_agent_type" {
		t.Fatalf("warnings = %#v, want one invalid_agent_type drop", warnings)
	}
	if warnings[0].Message != "tool name is empty" {
		t.Fatalf("warning message = %q, want the empty-tool-name rejection text", warnings[0].Message)
	}
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
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	if _, err := cfg.Resolve("unknown_tool"); err == nil {
		t.Fatal("Resolve(unknown_tool) succeeded, want dropped for an uncompiled tool name")
	}
	warnings := cfg.Warnings()
	if len(warnings) != 1 || warnings[0].Kind != "invalid_agent_type" || warnings[0].Name != "unknown_tool" {
		t.Fatalf("warnings = %#v, want one invalid_agent_type drop for unknown_tool", warnings)
	}
	if !strings.Contains(warnings[0].Message, "not_compiled") {
		t.Fatalf("warning message = %q, want the unknown tool name", warnings[0].Message)
	}
	// Duplicates remain valid and keep their resolved order and repetition.
	worker, err := cfg.Resolve("duplicate_tools")
	if err != nil {
		t.Fatalf("Resolve(duplicate_tools): %v", err)
	}
	if want := []string{"own_tool", "own_tool", "read_file"}; !reflect.DeepEqual(worker.Tools, want) {
		t.Fatalf("duplicate_tools = %q, want %q retained", worker.Tools, want)
	}
	// Omitted stays omitted at the definition level (its resolved value still
	// inherits) and an explicit empty list stays empty.
	if cfg.defs["omitted"].Tools != nil {
		t.Fatalf("omitted definition tools = %v, want nil", cfg.defs["omitted"].Tools)
	}
	// The explicit empty list stays empty at the definition level (its
	// resolved projection normalizes zero-length slices to nil, the package's
	// empty form).
	if emptyDef := cfg.defs["empty"]; emptyDef.Tools == nil || len(*emptyDef.Tools) != 0 {
		t.Fatalf("empty definition tools = %#v, want an explicit empty list", emptyDef.Tools)
	}
	if empty, err := cfg.Resolve("empty"); err != nil || empty.Tools != nil {
		t.Fatalf("resolved empty tools = %#v (%v), want the normalized empty form", empty.Tools, err)
	}
}

func TestParseWithCapabilitiesIntersectsDefaultTools(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{}`), nil, []string{"read_file", "run_command", "sleep"}, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	primary, err := cfg.Resolve("primary")
	if err != nil {
		t.Fatalf("Resolve(primary): %v", err)
	}
	// Default and inherited tools are intersected with the compiled universe:
	// process and task stay in the retained roster input but are not
	// fabricated as executable placeholders, and nothing outside the universe
	// survives.
	if want := []string{"read_file", "run_command", "sleep"}; !reflect.DeepEqual(primary.Tools, want) {
		t.Fatalf("primary default tools = %q, want the intersected roster %q", primary.Tools, want)
	}
	secondary, err := cfg.Resolve("secondary")
	if err != nil {
		t.Fatalf("Resolve(secondary): %v", err)
	}
	if !reflect.DeepEqual(secondary.Tools, primary.Tools) {
		t.Fatalf("secondary inherited tools = %q, want the intersected default %q", secondary.Tools, primary.Tools)
	}
	// The retained roster input itself is unchanged.
	for _, name := range []string{"process", "task", "edit_file", "apply_patch"} {
		if !contains(StandardTools, name) {
			t.Fatalf("StandardTools lost %q: %q", name, StandardTools)
		}
	}
	// An empty compiled universe leaves the defaults empty, not fabricated.
	none, err := ParseWithCapabilities([]byte(`{}`), nil, nil, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	nonePrimary, err := none.Resolve("primary")
	if err != nil {
		t.Fatalf("Resolve(primary): %v", err)
	}
	if len(nonePrimary.Tools) != 0 {
		t.Fatalf("primary tools under an empty universe = %q, want none fabricated", nonePrimary.Tools)
	}
}

func TestParseWithCapabilitiesDefaultProjection(t *testing.T) {
	cfg, err := ParseWithCapabilities([]byte(`{
	  "plan": {"capabilities": ["cap-a"]},
	  "sweeper": {"capabilities": []},
	  "plain": {},
	  "model_a": {"model": "prov/a"},
	  "model_b": {"model": "prov/b"}
	}`), []string{"cap-a", "cap-b", "model_adaptation"}, []string{"read_file"}, []string{"model_adaptation"})
	if err != nil {
		t.Fatalf("ParseWithCapabilities: %v", err)
	}
	// The default list installs on the built-in primary definition alone;
	// ordinary inheritance propagates it through secondary to customs.
	want := []string{"model_adaptation"}
	primary, err := cfg.Resolve("primary")
	if err != nil {
		t.Fatalf("Resolve(primary): %v", err)
	}
	if !reflect.DeepEqual(primary.Capabilities, want) {
		t.Fatalf("primary capabilities = %q, want the default %q", primary.Capabilities, want)
	}
	for _, name := range []string{"secondary", "explore", "review", "compact", "plain", "model_a", "model_b"} {
		resolved, err := cfg.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		if !reflect.DeepEqual(resolved.Capabilities, want) {
			t.Fatalf("%s capabilities = %q, want the inherited default %q", name, resolved.Capabilities, want)
		}
	}
	// An explicit selection overrides the default and an explicit empty list
	// clears it.
	plan, err := cfg.Resolve("plan")
	if err != nil {
		t.Fatalf("Resolve(plan): %v", err)
	}
	if want := []string{"cap-a"}; !reflect.DeepEqual(plan.Capabilities, want) {
		t.Fatalf("plan capabilities = %q, want the explicit selection %q", plan.Capabilities, want)
	}
	sweeper, err := cfg.Resolve("sweeper")
	if err != nil {
		t.Fatalf("Resolve(sweeper): %v", err)
	}
	if sweeper.Capabilities != nil {
		t.Fatalf("sweeper capabilities = %q, want the explicit empty clear", sweeper.Capabilities)
	}
}

func TestParseWithCapabilitiesEmptyDefaultsLeaveBaseline(t *testing.T) {
	const doc = `{"plain": {}}`
	nilDefaults, err := ParseWithCapabilities([]byte(doc), []string{"cap-a"}, nil, nil)
	if err != nil {
		t.Fatalf("ParseWithCapabilities(nil defaults): %v", err)
	}
	emptyDefaults, err := ParseWithCapabilities([]byte(doc), []string{"cap-a"}, nil, []string{})
	if err != nil {
		t.Fatalf("ParseWithCapabilities(empty defaults): %v", err)
	}
	if !reflect.DeepEqual(nilDefaults.All(), emptyDefaults.All()) {
		t.Fatal("an empty default list changed the resolved definitions")
	}
	// Both stay byte-identical to the built-ins' empty capability default.
	for _, resolved := range nilDefaults.All() {
		if resolved.Capabilities != nil {
			t.Fatalf("%s capabilities = %q, want the empty default", resolved.Name, resolved.Capabilities)
		}
	}
}
