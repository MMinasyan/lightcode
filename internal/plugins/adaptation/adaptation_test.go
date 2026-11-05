package adaptation_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/runtime"
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

// prepareHook opens the shipped plugin's single contribution and invokes it.
func prepareHook(t *testing.T, ref model.ModelRef, mode prompt.Mode) runtime.PreparationConsequence {
	t.Helper()
	instance, err := adaptation.Plugin().Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()})
	testassert.NoError(t, err)
	testassert.Require(t, instance.Close == nil, "the adaptation plugin declares a Close")
	hook, ok := instance.Values[adaptation.CapabilityID].(runtime.PreparationHook)
	testassert.Require(t, ok, "open supplied %T, want a runtime.PreparationHook", instance.Values[adaptation.CapabilityID])
	consequence, err := hook.Prepare(context.Background(), runtime.PreparationInput{
		Model:    ref,
		Assembly: prompt.AssemblyInput{Mode: mode},
		Prompt:   "BASE_PROMPT",
		Catalog:  fixtureCatalog(),
	})
	testassert.NoError(t, err)
	return consequence
}

func TestPluginDeclaresOneRuntimeScopedPreparationHook(t *testing.T) {
	plugin := adaptation.Plugin()
	testassert.Require(t, plugin.ID == "adaptation" && plugin.Scope == runtime.ScopeRuntime, "plugin identity = %q/%s, want adaptation/runtime", plugin.ID, plugin.Scope)
	testassert.Require(t, plugin.ValidateConfig == nil, "the adaptation plugin declares a settings validator")
	testassert.Equal(t, len(plugin.Provides), 1)
	instance, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()})
	testassert.NoError(t, err)
	_, ok := instance.Values[adaptation.CapabilityID].(runtime.PreparationHook)
	testassert.Require(t, ok, "the declared capability = %T, want a runtime.PreparationHook", instance.Values[adaptation.CapabilityID])
}

func TestPrepareAppliesShippedBundledRows(t *testing.T) {
	// The GPT-family row: excluded file tools, the literal description
	// replacement, and the task-execution append paragraph.
	got := prepareHook(t, model.ModelRef{Provider: "openai", Model: "gpt-5.5-mini"}, prompt.ModeFull)
	testassert.Require(t, reflect.DeepEqual(got.ToolNames, []string{"read_file", "apply_patch", "run_command", "note_editor"}), "selected tools = %q, want the excluded file tools absent in catalog order", got.ToolNames)
	{
		want := map[string]string{"run_command": bundledGPTGuidanceReplacement}
		testassert.Require(t, reflect.DeepEqual(got.Descriptions, want), "tool descriptions = %q, want the one literal guidance-line rule", got.Descriptions)
	}
	testassert.Require(t, got.Prompt != nil && strings.HasPrefix(*got.Prompt, "BASE_PROMPT\n\n") && strings.Contains(*got.Prompt, "Use more tool calls when they can improve the response"), "prompt = %v, want the baseline with the shipped guidance paragraph appended", got.Prompt)

	// The grok and gemini rows carry append paragraphs with no tool levers.
	for _, ref := range []model.ModelRef{{Provider: "xai", Model: "grok-build-0.1"}, {Provider: "google", Model: "gemini-3-pro"}} {
		got := prepareHook(t, ref, prompt.ModeFull)
		testassert.Require(t, got.ToolNames == nil && len(got.Descriptions) == 0 && got.Prompt != nil && strings.HasPrefix(*got.Prompt, "BASE_PROMPT\n\n"), "%s treatment = %+v, want append paragraphs only", ref.String(), got)
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

func TestPrepareUnmatchedModelIsBaseline(t *testing.T) {
	got := prepareHook(t, model.ModelRef{Provider: "anthropic", Model: "claude-x"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil && got.Descriptions == nil, "unmatched model treatment = %+v, want the zero baseline", got)
}

func TestPrepareMatchesTheModelComponentOnly(t *testing.T) {
	// A pattern inside the provider component never matches: the matcher
	// receives ref.Model alone.
	got := prepareHook(t, model.ModelRef{Provider: "gpt-5", Model: "unrelated"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil, "provider-shaped ref treatment = %+v, want the zero baseline", got)
	// Matching is case-insensitive over the bare model component.
	got = prepareHook(t, model.ModelRef{Provider: "openai", Model: "GPT-5.4"}, prompt.ModeFull)
	testassert.Require(t, len(got.ToolNames) == 4, "case-insensitive row did not match: %+v", got)
}

func TestPrepareReturnsIndependentContainers(t *testing.T) {
	first := prepareHook(t, model.ModelRef{Provider: "openai", Model: "gpt-5.5"}, prompt.ModeFull)
	testassert.Require(t, first.Prompt != nil && len(first.ToolNames) != 0 && len(first.Descriptions) != 0, "shipped row did not resolve: %+v", first)
	*first.Prompt = "tampered"
	first.ToolNames[0] = "tampered"
	first.Descriptions["run_command"] = "tampered"

	second := prepareHook(t, model.ModelRef{Provider: "openai", Model: "gpt-5.5"}, prompt.ModeFull)
	testassert.Require(t, strings.HasPrefix(*second.Prompt, "BASE_PROMPT\n\n") && second.ToolNames[0] == "read_file" && second.Descriptions["run_command"] == bundledGPTGuidanceReplacement, "a mutated result leaked into the next resolve: %+v", second)
}

func TestOpenRejectsCanceledScopeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := adaptation.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()})
		testassert.Require(t, err != nil, "Open with a canceled scope context succeeded")
	}
}
