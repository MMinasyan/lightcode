package adaptation_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// resolvePlugin opens the shipped plugin's single export and returns the
// ModelAdaptation implementation it supplies.
func resolvePlugin(t *testing.T, ctx context.Context, ref model.ModelRef) (runtime.Adaptation, error) {
	t.Helper()
	instance, err := adaptation.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, runtime.Bindings{})
	testassert.NoError(t, err)
	testassert.Require(t, instance.Close == nil, "the adaptation plugin declares a Close")
	resolver, ok := instance.Values["model_adaptation"].(runtime.ModelAdaptation)
	testassert.Require(t, ok, "open supplied %T, want a runtime.ModelAdaptation", instance.Values["model_adaptation"])
	return resolver.Resolve(ref)
}

func TestPluginDeclaresOneRuntimeScopedModelAdaptation(t *testing.T) {
	plugin := adaptation.Plugin()
	testassert.Require(t, plugin.ID == "adaptation" && plugin.Scope == runtime.ScopeRuntime, "plugin identity = %q/%s, want adaptation/runtime", plugin.ID, plugin.Scope)
	testassert.Require(t, plugin.ValidateConfig == nil, "the adaptation plugin declares a settings validator")
	testassert.Equal(t, len(plugin.Provides), 1)
}

func TestResolveShippedBundledRows(t *testing.T) {
	ctx := context.Background()
	// The GPT-family row: excluded file tools, the included hidden patch tool,
	// the description replacement, and the task-execution addition.
	got, err := resolvePlugin(t, ctx, model.ModelRef{Provider: "openai", Model: "gpt-5.5-mini"})
	testassert.NoError(t, err)
	{
		want := []string{"edit_file", "write_file"}
		testassert.Require(t, reflect.DeepEqual(got.ExcludeTools, want), "exclude tools = %q, want %q", got.ExcludeTools, want)
	}
	{
		want := []string{"apply_patch"}
		testassert.Require(t, reflect.DeepEqual(got.IncludeTools, want), "include tools = %q, want %q", got.IncludeTools, want)
	}
	testassert.Equal(t, got.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"], "apply_patch")
	testassert.Require(t, got.Additions["task_execution"] != "", "task_execution addition = empty, want the shipped coaching block")

	// The grok and gemini rows carry additions with no tool levers.
	for _, ref := range []model.ModelRef{{Provider: "xai", Model: "grok-build-0.1"}, {Provider: "google", Model: "gemini-3-pro"}} {
		got, err = resolvePlugin(t, ctx, ref)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", ref.String(), err)
		}
		if got.ExcludeTools != nil || got.IncludeTools != nil || got.Additions["task_execution"] == "" {
			t.Fatalf("%s adaptation = %+v, want additions only", ref.String(), got)
		}
	}
}

func TestResolveUnmatchedModelIsBaseline(t *testing.T) {
	got, err := resolvePlugin(t, context.Background(), model.ModelRef{Provider: "anthropic", Model: "claude-x"})
	testassert.NoError(t, err)
	testassert.Require(t, got.ExcludeTools == nil && got.IncludeTools == nil && got.Blocks == nil && got.Additions == nil && got.ToolDescriptionReplacements == nil, "unmatched model adaptation = %+v, want the zero baseline", got)
}

func TestResolveMatchesTheModelComponentOnly(t *testing.T) {
	ctx := context.Background()
	// A pattern inside the provider component never matches: the matcher
	// receives ref.Model alone.
	got, err := resolvePlugin(t, ctx, model.ModelRef{Provider: "gpt-5", Model: "unrelated"})
	testassert.NoError(t, err)
	testassert.Require(t, got.ExcludeTools == nil && got.IncludeTools == nil && got.Additions == nil, "provider-shaped ref adaptation = %+v, want the zero baseline", got)
	// Matching is case-insensitive over the bare model component.
	got, err = resolvePlugin(t, ctx, model.ModelRef{Provider: "openai", Model: "GPT-5.4"})
	testassert.NoError(t, err)
	testassert.Require(t, got.IncludeTools != nil, "case-insensitive row did not match")
}

func TestResolveReturnsIndependentContainers(t *testing.T) {
	first, err := resolvePlugin(t, context.Background(), model.ModelRef{Provider: "openai", Model: "gpt-5.5"})
	testassert.NoError(t, err)
	first.ExcludeTools[0] = "tampered"
	first.IncludeTools[0] = "tampered"
	first.Additions["task_execution"] = "tampered"
	first.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] = "tampered"

	second, err := resolvePlugin(t, context.Background(), model.ModelRef{Provider: "openai", Model: "gpt-5.5"})
	testassert.NoError(t, err)
	testassert.Require(t, second.ExcludeTools[0] == "edit_file" && second.IncludeTools[0] == "apply_patch" && second.Additions["task_execution"] != "tampered" && second.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] == "apply_patch", "a mutated result leaked into the next resolve: %+v", second)
}

func TestOpenRejectsCanceledScopeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := adaptation.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, runtime.Bindings{})
		testassert.Require(t, err != nil, "Open with a canceled scope context succeeded")
	}
}
