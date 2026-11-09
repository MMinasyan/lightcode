package adaptation_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extensions/adaptation"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// bundledGPTGuidanceLine and bundledGPTGuidanceReplacement are the complete
// literal standard run_command guidance line the bundled GPT rows replace and
// its GPT form.
const bundledGPTGuidanceLine = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file."
const bundledGPTGuidanceReplacement = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use apply_patch."

// fixtureCatalog is the ordered eligible catalog the plugin tests resolve
// against: the file tools the GPT rows hide, plus the standard command and an
// unrelated tool whose prose merely names the same phrase.
func fixtureCatalog() []model.ToolDefinition {
	return []model.ToolDefinition{
		{Name: "read_file", Description: "reads one file"},
		{Name: "write_file", Description: "writes one file"},
		{Name: "edit_file", Description: "edits one file"},
		{Name: "apply_patch", Description: "applies one patch"},
		{Name: "run_command", Description: bundledGPTGuidanceLine},
		{Name: "note_editor", Description: "Edit edit_file or write_file via one call."},
	}
}

func fixtureToolNames() []string {
	out := make([]string, 0, len(fixtureCatalog()))
	for _, definition := range fixtureCatalog() {
		out = append(out, definition.Name)
	}
	return out
}

func openHook(t *testing.T, section string) extension.PreparationHook {
	t.Helper()
	instance, err := adaptation.Native().Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
	testassert.NoError(t, err)
	testassert.Require(t, instance.Close == nil, "the adaptation native declares a Close")
	implementation, ok := instance.Capabilities[adaptation.CapabilityID]
	testassert.Require(t, ok, "open supplied %d capabilities, want the declared hook", len(instance.Capabilities))
	testassert.Require(t, implementation.Preparation != nil, "the declared capability carries no preparation hook")
	return implementation.Preparation
}

// prepareSection opens the shipped native with one captured section and
// invokes its single contribution.
func prepareSection(t *testing.T, section string, ref model.ModelRef, mode prompt.Mode) extension.PreparationConsequence {
	t.Helper()
	hook := openHook(t, section)
	consequence, err := hook(context.Background(), extension.CallContext{}, extension.PreparationInput{
		Model:    ref,
		Assembly: prompt.AssemblyInput{Mode: mode},
		Prompt:   "BASE_PROMPT",
		Catalog:  fixtureCatalog(),
		Tools:    fixtureCatalog(),
	})
	testassert.NoError(t, err)
	return consequence
}

func prepareHook(t *testing.T, ref model.ModelRef, mode prompt.Mode) extension.PreparationConsequence {
	t.Helper()
	return prepareSection(t, `{"enabled":true}`, ref, mode)
}

func TestNativeDeclaresOneHarnessScopedPreparationHook(t *testing.T) {
	native := adaptation.Native()
	testassert.Require(t, native.Declaration.ID == "adaptation", "native identity = %q, want adaptation", native.Declaration.ID)
	testassert.Equal(t, len(native.Declaration.Components), 1)
	testassert.Require(t, native.Declaration.Components[0].Lifetime == extension.LifetimeHarness, "component lifetime = %q, want harness", native.Declaration.Components[0].Lifetime)
	testassert.Equal(t, len(native.Declaration.Capabilities), 1)
	capability := native.Declaration.Capabilities[0]
	testassert.Require(t, capability.ID == adaptation.CapabilityID && capability.Kind == extension.KindHook, "capability = %q/%s, want the preparation hook", capability.ID, capability.Kind)
	testassert.Require(t, len(capability.Hooks) == 1 && capability.Hooks[0].Boundary == extension.BoundaryPrepare && capability.Hooks[0].Mode == extension.HookModePure, "hooks = %+v, want a pure prepare boundary", capability.Hooks)
	openHook(t, `{"enabled":true}`)
}

func TestConfiguredSchemaValidatesConsumedTypes(t *testing.T) {
	compiled, err := extension.Compile([]extension.Native{adaptation.Native()})
	testassert.NoError(t, err)
	valid := []string{
		`{"enabled":true}`,
		`{"enabled":true,"entries":[]}`,
		`{"enabled":true,"entries":[{"pattern":""}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","name":"unconsumed","unknown":1}]}`,
		`{"enabled":true,"entries":[{"pattern":"model-*","prompt":"","append":["a",""],"exclude_tools":[],"tool_description_replacements":{}}],"unclaimed":true}`,
	}
	for _, section := range valid {
		{
			err := compiled.ValidateSection("adaptation", json.RawMessage(section))
			testassert.Check(t, err == nil, "ValidateSection(%s) = %v, want valid", section, err)
		}
	}
	invalid := []string{
		`{"entries":[]}`,
		`{"enabled":true,"entries":{}}`,
		`{"enabled":true,"entries":[{}]}`,
		`{"enabled":true,"entries":[{"pattern":1}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","prompt":1}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","append":"a"}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","exclude_tools":[1]}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","tool_description_replacements":{"k":1}}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","tool_description_replacements":{"":"v"}}]}`,
	}
	for _, section := range invalid {
		{
			err := compiled.ValidateSection("adaptation", json.RawMessage(section))
			testassert.Check(t, err != nil, "ValidateSection(%s) accepted an invalid consumed shape", section)
		}
	}
}

func TestPrepareAppliesShippedBundledRows(t *testing.T) {
	// The GPT-family row: excluded file tools, the literal description
	// replacement, the task-execution append paragraph, and the matched view
	// computed from the incoming visible tools.
	got := prepareHook(t, model.ModelRef{Provider: "openai", Model: "gpt-5.5-mini"}, prompt.ModeFull)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"read_file", "apply_patch", "run_command", "note_editor"}), "selected tools = %q, want the excluded file tools absent in catalog order", got.ToolNames)
	{
		want := map[string]string{"run_command": bundledGPTGuidanceReplacement}
		testassert.Require(t, reflect.DeepEqual(got.Descriptions, want), "tool descriptions = %q, want the one literal guidance-line rule", got.Descriptions)
	}
	testassert.Require(t, got.Prompt != nil && strings.HasPrefix(*got.Prompt, "BASE_PROMPT\n\n") && strings.Contains(*got.Prompt, "Use more tool calls when they can improve the response"), "prompt = %v, want the baseline with the shipped guidance paragraph appended", got.Prompt)

	// The grok and gemini rows carry append paragraphs with no tool levers;
	// a match still states the complete visible view.
	for _, ref := range []model.ModelRef{{Provider: "xai", Model: "grok-build-0.1"}, {Provider: "google", Model: "gemini-3-pro"}} {
		got := prepareHook(t, ref, prompt.ModeFull)
		testassert.Require(t, reflect.DeepEqual(got.ToolNames, fixtureToolNames()) && len(got.Descriptions) == 0 && got.Prompt != nil && strings.HasPrefix(*got.Prompt, "BASE_PROMPT\n\n"), "%s treatment = %+v, want append paragraphs and the full visible view", ref.String(), got)
	}
}

func TestPrepareBundledGuidanceOnlyInFullMode(t *testing.T) {
	ref := model.ModelRef{Provider: "openai", Model: "gpt-5.5"}
	full := prepareHook(t, ref, prompt.ModeFull)
	testassert.Require(t, full.Prompt != nil && *full.Prompt != "BASE_PROMPT", "full-mode bundled treatment = %+v, want the guidance append", full)
	for _, mode := range []prompt.Mode{prompt.ModeSimple, prompt.ModeNone} {
		got := prepareHook(t, ref, mode)
		testassert.Require(t, got.Prompt == nil, "%s-mode bundled treatment = %+v, want no automatic bundled guidance", mode, got)
		testassert.Require(t, reflect.DeepEqual(got.ToolNames, full.ToolNames) && reflect.DeepEqual(got.Descriptions, full.Descriptions), "%s-mode bundled treatment = %+v, want the same tool exclusions and replacements", mode, got)
	}
}

func TestConfiguredEntriesPrecedeBundledAndApplyEveryMode(t *testing.T) {
	section := `{"enabled":true,"entries":[{"pattern":"*","prompt":"CONFIGURED","append":["USER_TAIL"]}]}`
	for _, mode := range []prompt.Mode{prompt.ModeFull, prompt.ModeSimple, prompt.ModeNone} {
		// The user catch-all matches the bundled GPT id first, so neither its
		// exclusions nor its full-only guidance apply.
		got := prepareSection(t, section, model.ModelRef{Provider: "openai", Model: "gpt-5.5"}, mode)
		testassert.Require(t, got.Prompt != nil && *got.Prompt == "CONFIGURED\n\nUSER_TAIL", "%s-mode configured treatment = %+v, want the complete replacement followed by the explicit append", mode, got)
		testassert.Require(t, reflect.DeepEqual(got.ToolNames, fixtureToolNames()) && len(got.Descriptions) == 0, "%s-mode configured treatment = %+v, want the unexcluded eligible view and no bundled levers", mode, got)
	}

	// A present empty prompt replacement replaces the text with empty; the
	// explicit append still follows it with the existing blank-line
	// separator.
	empty := `{"enabled":true,"entries":[{"pattern":"*","prompt":"","append":["TAIL"]}]}`
	got := prepareSection(t, empty, model.ModelRef{Provider: "openai", Model: "gpt-5.5"}, prompt.ModeNone)
	testassert.Require(t, got.Prompt != nil && *got.Prompt == "TAIL", "empty replacement treatment = %+v, want the append over empty text", got)

	// Configured entries are ordered with the first match deciding.
	ordered := `{"enabled":true,"entries":[{"pattern":"*","prompt":"FIRST"},{"pattern":"*","prompt":"SECOND"}]}`
	got = prepareSection(t, ordered, model.ModelRef{Provider: "openai", Model: "gpt-5.5"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt != nil && *got.Prompt == "FIRST", "ordered configured treatment = %+v, want the first user match", got)
}

func TestConfiguredEmptyAndCatchAllPatternsMatch(t *testing.T) {
	// An empty pattern is a literal empty id, not a wildcard; it is valid and
	// matches only the empty model component.
	section := `{"enabled":true,"entries":[{"pattern":"","prompt":"EMPTY_ID"}]}`
	got := prepareSection(t, section, model.ModelRef{}, prompt.ModeFull)
	testassert.Require(t, got.Prompt != nil && *got.Prompt == "EMPTY_ID", "empty-pattern treatment = %+v, want the configured replacement", got)
	got = prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil && got.Descriptions == nil, "nonempty-id treatment = %+v, want baseline", got)

	// A catch-all pattern with no bundled-only name/anchor restriction matches
	// every id.
	catchAll := `{"enabled":true,"entries":[{"pattern":"*","prompt":"ANY"}]}`
	got = prepareSection(t, catchAll, model.ModelRef{Provider: "prov", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt != nil && *got.Prompt == "ANY", "catch-all treatment = %+v, want the configured replacement", got)

	// Unknown extension-owned members are uninterpreted by both the schema
	// and the extension's own decode.
	unknown := `{"enabled":true,"unclaimed":1,"entries":[{"pattern":"*","prompt":"UNKNOWN","ignored":["x"]}]}`
	got = prepareSection(t, unknown, model.ModelRef{Provider: "prov", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt != nil && *got.Prompt == "UNKNOWN", "unknown-member treatment = %+v, want the consumed fields only", got)
}

// TestConfiguredDecodeUsesExactCaseJSONNames proves the extension consumes
// only the schema-recognized exact-case member names. A differently cased
// sibling cannot override a valid lowercase value, and unknown/miscased
// members stay uninterpreted even when their value types are invalid.
func TestConfiguredDecodeUsesExactCaseJSONNames(t *testing.T) {
	// Lowercase entries and every lowercase entry field are the only values
	// consumed; the uppercase siblings are ignored.
	section := `{"enabled":true,
		"entries":[{"pattern":"*","prompt":"LOWER","append":["LOWER_APPEND"],"exclude_tools":["write_file"],"tool_description_replacements":{"reads":"LOWER_READ"}}],
		"Entries":[{"pattern":"*","prompt":"UPPER","append":["UPPER_APPEND"],"exclude_tools":["read_file"],"tool_description_replacements":{"writes":"UPPER_WRITE"}}]}`
	got := prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	{
		prompt := "<nil>"
		if got.Prompt != nil {
			prompt = *got.Prompt
		}
		testassert.Require(t, prompt == "LOWER\n\nLOWER_APPEND", "cased section prompt = %q, want the lowercase entry only", prompt)
	}
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"read_file", "edit_file", "apply_patch", "run_command", "note_editor"}), "cased section tools = %q, want the lowercase exclusions only", got.ToolNames)
	testassert.Require(t, reflect.DeepEqual(got.Descriptions, map[string]string{"read_file": "LOWER_READ one file"}), "cased section descriptions = %q, want the lowercase replacement key only", got.Descriptions)

	// Miscased entry members are not aliases of the lowercase names.
	miscased := `{"entries":[{"pattern":"*","Prompt":"UPPER","Append":["UPPER"],"Exclude_Tools":["read_file"],"Tool_Description_Replacements":{"writes":"UPPER"}}]}`
	got = prepareSection(t, miscased, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.Descriptions == nil && reflect.DeepEqual(got.ToolNames, fixtureToolNames()), "miscased entry treatment = %+v, want no consumed configured fields", got)

	// A differently cased entries member alone is not the entries field.
	upperEntries := `{"entries_extra":[],"Entries":[{"pattern":"*","prompt":"UPPER"}]}`
	got = prepareSection(t, upperEntries, model.ModelRef{Provider: "anthropic", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil && got.Descriptions == nil, "uppercase entries treatment = %+v, want baseline", got)

	// Unknown uppercase-only fields are ignored even with arbitrary invalid
	// types; only the exact lowercase pattern opens the entry.
	unknownInvalid := `{"Entries":1,"ENTRIES":{},"entries":[{"pattern":"*","Prompt":1,"Append":{},"Exclude_Tools":"bad","Tool_Description_Replacements":[1]}]}`
	got = prepareSection(t, unknownInvalid, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.Descriptions == nil && reflect.DeepEqual(got.ToolNames, fixtureToolNames()), "unknown invalid fields treatment = %+v, want the matching entry with no configured levers", got)

	// Replacement-map keys are literal data, never case-folded.
	literalKeys := `{"entries":[{"pattern":"*","tool_description_replacements":{"READS":"UPPER"}}]}`
	got = prepareSection(t, literalKeys, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, got.Descriptions == nil, "literal replacement keys = %q, want no case-folded match against reads one file", got.Descriptions)
}

func TestConfiguredToolViewIsVisibleMinusExclusions(t *testing.T) {
	section := `{"enabled":true,"entries":[{"pattern":"*","exclude_tools":["write_file","edit_file"]}]}`
	got := prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"read_file", "apply_patch", "run_command", "note_editor"}), "configured view = %q, want the visible tools minus its exclusions", got.ToolNames)

	all := `{"enabled":true,"entries":[{"pattern":"*","exclude_tools":["read_file","write_file","edit_file","apply_patch","run_command","note_editor"]}]}`
	got = prepareSection(t, all, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, got.ToolNames != nil && len(got.ToolNames) == 0, "all-excluded view = %#v, want a present empty selection", got.ToolNames)

	section = `{"enabled":true,"entries":[{"pattern":"*","exclude_tools":[]}]}`
	got = prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, fixtureToolNames()), "unexcluded view = %q, want exactly the incoming visible tools", got.ToolNames)
}

// TestConfiguredToolViewFollowsIncomingVisibleSubset proves a match selects
// from the tools already-run hooks left visible, not from the complete
// eligible catalog the rule also receives.
func TestConfiguredToolViewFollowsIncomingVisibleSubset(t *testing.T) {
	// The incoming subset is deliberately in reverse catalog order, so a view
	// rebuilt in catalog order is distinguishable from the incoming order.
	tools := []model.ToolDefinition{
		{Name: "note_editor", Description: "Edit edit_file or write_file via one call."},
		{Name: "run_command", Description: bundledGPTGuidanceLine},
		{Name: "apply_patch", Description: "applies one patch"},
	}
	prepare := func(section string, tools []model.ToolDefinition) extension.PreparationConsequence {
		t.Helper()
		hook := openHook(t, section)
		consequence, err := hook(context.Background(), extension.CallContext{}, extension.PreparationInput{
			Model:    model.ModelRef{Provider: "prov", Model: "any"},
			Assembly: prompt.AssemblyInput{Mode: prompt.ModeFull},
			Prompt:   "BASE_PROMPT",
			Catalog:  fixtureCatalog(),
			Tools:    tools,
		})
		testassert.NoError(t, err)
		return consequence
	}

	got := prepare(`{"enabled":true,"entries":[{"pattern":"*","prompt":"SUBSET"}]}`, tools)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"note_editor", "run_command", "apply_patch"}), "prompt-only view = %q, want the incoming visible subset in its order", got.ToolNames)

	got = prepare(`{"enabled":true,"entries":[{"pattern":"*","exclude_tools":["read_file","run_command"]}]}`, tools)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"note_editor", "apply_patch"}), "exclusion view = %q, want the incoming order minus the exclusion, the absent name not restored", got.ToolNames)

	// An earlier hook that hid every tool leaves a present empty selection.
	got = prepare(`{"enabled":true,"entries":[{"pattern":"*","prompt":"NONE_VISIBLE"}]}`, []model.ToolDefinition{})
	testassert.Require(t, got.ToolNames != nil && len(got.ToolNames) == 0, "empty incoming view = %#v, want a present empty selection", got.ToolNames)
}

func TestConfiguredDescriptionReplacementUsesEligibleCatalogText(t *testing.T) {
	section := `{"enabled":true,"entries":[{"pattern":"*","tool_description_replacements":{"edit_file":"apply_patch","apply_patch":"PATCH"}}]}`
	got := prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	want := map[string]string{
		"note_editor": "Edit apply_patch or write_file via one call.",
		"run_command": "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use apply_patch or write_file.",
	}
	testassert.Require(t, reflect.DeepEqual(got.Descriptions, want), "description replacements = %q, want only the tools whose original text contains a key, each transformed once", got.Descriptions)
}

func TestPrepareUnmatchedModelIsBaseline(t *testing.T) {
	got := prepareHook(t, model.ModelRef{Provider: "anthropic", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil && got.Descriptions == nil, "unmatched model treatment = %+v, want the zero baseline", got)
}

func TestPrepareMatchesTheModelComponentOnly(t *testing.T) {
	got := prepareHook(t, model.ModelRef{Provider: "gpt-5", Model: "unrelated"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil, "provider-shaped ref treatment = %+v, want the zero baseline", got)
	got = prepareHook(t, model.ModelRef{Provider: "openai", Model: "GPT-5.4"}, prompt.ModeFull)
	testassert.Require(t, len(got.ToolNames) == 4, "case-insensitive row did not match: %+v", got)
}

func TestPrepareReturnsIndependentContainers(t *testing.T) {
	section := `{"enabled":true,"entries":[{"pattern":"*","append":["USER"],"exclude_tools":["write_file"],"tool_description_replacements":{"reads":"READ"}}]}`
	first := prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, first.Prompt != nil && len(first.ToolNames) != 0 && len(first.Descriptions) != 0, "configured row did not resolve: %+v", first)
	*first.Prompt = "tampered"
	first.ToolNames[0] = "tampered"
	first.Descriptions["read_file"] = "tampered"

	second := prepareSection(t, section, model.ModelRef{Provider: "prov", Model: "any"}, prompt.ModeFull)
	testassert.Require(t, strings.HasPrefix(*second.Prompt, "BASE_PROMPT\n\nUSER") && second.ToolNames[0] == "read_file" && second.Descriptions["read_file"] == "READ one file", "a mutated result leaked into the next resolve: %+v", second)
}

func TestCompiledSchemaRejectsEmptyReplacementKey(t *testing.T) {
	compiled, err := extension.Compile([]extension.Native{adaptation.Native()})
	testassert.NoError(t, err)
	err = compiled.ValidateSection("adaptation", json.RawMessage(`{"enabled":true,"entries":[{"pattern":"*","tool_description_replacements":{"":"v"}}]}`))
	testassert.Require(t, err != nil, "compiled ConfigSchema accepted an empty configured replacement key")
}

// TestOpenRejectsRecognizedWrongTypeMembers keeps the exact lowercase
// consumed names typed: a wrong type under a recognized name is rejected even
// though unknown/miscased names are ignored.
func TestOpenRejectsRecognizedWrongTypeMembers(t *testing.T) {
	for _, section := range []string{
		`{"enabled":true,"entries":{}}`,
		`{"enabled":true,"entries":[{"pattern":1}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","prompt":1}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","append":"a"}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","exclude_tools":"a"}]}`,
		`{"enabled":true,"entries":[{"pattern":"*","tool_description_replacements":{"k":1}}]}`,
	} {
		{
			_, err := adaptation.Native().Open(context.Background(), extension.InstanceContext{Section: json.RawMessage(section)})
			testassert.Require(t, err != nil, "Open accepted recognized wrong-type section %s", section)
		}
	}
}

func TestOpenRejectsCanceledScopeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := adaptation.Native().Open(ctx, extension.InstanceContext{})
	testassert.Require(t, err != nil, "Open with a canceled scope context succeeded")
}
