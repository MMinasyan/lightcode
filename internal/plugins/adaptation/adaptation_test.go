package adaptation_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
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

func openHook(t *testing.T) extension.PreparationHook {
	t.Helper()
	instance, err := adaptation.Native().Open(context.Background(), extension.InstanceContext{})
	testassert.NoError(t, err)
	testassert.Require(t, instance.Close == nil, "the adaptation native declares a Close")
	implementation, ok := instance.Capabilities[adaptation.CapabilityID]
	testassert.Require(t, ok, "open supplied %d capabilities, want the declared hook", len(instance.Capabilities))
	testassert.Require(t, implementation.Preparation != nil, "the declared capability carries no preparation hook")
	return implementation.Preparation
}

// prepareHook opens the shipped native's single contribution and invokes it.
func prepareHook(t *testing.T, ref model.ModelRef, mode prompt.Mode) extension.PreparationConsequence {
	t.Helper()
	hook := openHook(t)
	consequence, err := hook(context.Background(), extension.CallContext{}, extension.PreparationInput{
		Model:    ref,
		Assembly: prompt.AssemblyInput{Mode: mode},
		Prompt:   "BASE_PROMPT",
		Catalog:  fixtureCatalog(),
	})
	testassert.NoError(t, err)
	return consequence
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
	openHook(t)
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
	got := prepareHook(t, model.ModelRef{Provider: "gpt-5", Model: "unrelated"}, prompt.ModeFull)
	testassert.Require(t, got.Prompt == nil && got.ToolNames == nil, "provider-shaped ref treatment = %+v, want the zero baseline", got)
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
	_, err := adaptation.Native().Open(ctx, extension.InstanceContext{})
	testassert.Require(t, err != nil, "Open with a canceled scope context succeeded")
}
