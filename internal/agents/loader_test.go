package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/compact"
)

func TestResolveBuiltinsInheritThroughPrimary(t *testing.T) {
	cfg, err := Parse([]byte(`{"primary":{"model":"test/base-model"},"secondary":{"model":"test/secondary-model"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{
  "primary": {"model":"test/base-model"},
  "plan": {"prompt":"Write design notes.","tools":["read_file"],"readonly":true,"write_dir":"docs","description":"Notes writer."}
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{
  "planner": {"tools":["read_file","submit_plan"]},
  "reader": {"tools":["read_file"]}
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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

func TestBuiltinsExposeConcreteRootAndCodeOnlyCompact(t *testing.T) {
	cfg, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	if compactAgent.SystemPrompt != SystemPromptNone || compactAgent.Prompt != compact.DefaultSummarizerPrompt {
		t.Fatalf("compact prompt = system:%q body:%q", compactAgent.SystemPrompt, compactAgent.Prompt)
	}
	if len(compactAgent.Tools) != 0 || compactAgent.LSP || !compactAgent.Readonly || compactAgent.Subagent {
		t.Fatalf("compact capability fields = %+v", compactAgent)
	}
}

func TestToolsPresenceControlsInheritanceEmptyAndSubset(t *testing.T) {
	cfg, err := Parse([]byte(`{
  "empty": {"tools": []},
  "reader": {"tools": ["read_file", "run_command"]}
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{
  "explore": {
    "model": "test/explore-model",
    "system_prompt": "none",
    "tools": "not an array",
    "readonly": false,
    "description": "override"
  }
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{
  "good": {"tools": ["read_file"], "system_prompt": "simple", "model": "test/good"},
  "bad_tool": {"tools": ["does_not_exist"]},
  "bad_prompt": {"system_prompt": "huge"},
  "bad_model": {"model": "not-prefixed"},
  "bad_shape": "not an object",
  "bad_tools_shape": {"tools": "read_file"}
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
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
	cfg, err := Parse([]byte(`{"explore":{"prompt":"custom prompt","description":"custom","subagent":false}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	explore, err := cfg.Resolve("explore")
	if err != nil {
		t.Fatalf("Resolve(explore): %v", err)
	}
	if explore.Prompt == "custom prompt" || explore.Description == "custom" || !explore.Subagent {
		t.Fatalf("builtin was overridden by colliding user entry: %+v", explore)
	}
}

func TestLoadMissingFileCreatesSkeletonAndBuiltins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config-dir", "agents.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created agents.json: %v", err)
	}
	if string(data) != emptyAgentsTemplate {
		t.Fatalf("created agents.json = %q, want skeleton", data)
	}
	all := cfg.All()
	if len(all) < len(builtinOrder) {
		t.Fatalf("All returned %d types, want at least builtins", len(all))
	}
	if all[0].Name != "primary" || all[len(builtinOrder)-1].Name != "compact" {
		t.Fatalf("builtin order = %v", namesOf(all[:len(builtinOrder)]))
	}
}

func TestPathForConfigUsesConfigDirectory(t *testing.T) {
	got := PathForConfig(filepath.Join("/tmp", "lightcode-test", "config.json"))
	want := filepath.Join("/tmp", "lightcode-test", "agents.json")
	if got != want {
		t.Fatalf("PathForConfig = %q, want %q", got, want)
	}
}

func TestWriteModelUpdatesAgentsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path, []byte(`{"custom":{"tools":["read_file"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteModel(path, "primary", "test/new-model"); err != nil {
		t.Fatalf("WriteModel primary: %v", err)
	}
	if err := WriteModel(path, "custom", "test/custom-model"); err != nil {
		t.Fatalf("WriteModel custom: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after WriteModel: %v", err)
	}
	primary, _ := cfg.Resolve("primary")
	custom, _ := cfg.Resolve("custom")
	if primary.Model != "test/new-model" || custom.Model != "test/custom-model" {
		t.Fatalf("models after write = primary:%q custom:%q", primary.Model, custom.Model)
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
