package adaptation_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// resolvePlugin opens the shipped plugin's single export and returns the
// ModelAdaptation implementation it supplies.
func resolvePlugin(t *testing.T, ctx context.Context, ref model.ModelRef) (runtime.Adaptation, error) {
	t.Helper()
	instance, err := adaptation.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("plugin Open: %v", err)
	}
	if instance.Close != nil {
		t.Fatal("the adaptation plugin declares a Close")
	}
	resolver, ok := instance.Values["model_adaptation"].(runtime.ModelAdaptation)
	if !ok {
		t.Fatalf("open supplied %T, want a runtime.ModelAdaptation", instance.Values["model_adaptation"])
	}
	return resolver.Resolve(ref)
}

func TestPluginDeclaresOneRuntimeScopedModelAdaptation(t *testing.T) {
	plugin := adaptation.Plugin()
	if plugin.ID != "adaptation" || plugin.Scope != runtime.ScopeRuntime {
		t.Fatalf("plugin identity = %q/%s, want adaptation/runtime", plugin.ID, plugin.Scope)
	}
	if plugin.ValidateConfig != nil {
		t.Fatal("the adaptation plugin declares a settings validator")
	}
	if len(plugin.Provides) != 1 {
		t.Fatalf("provides = %d capabilities, want the single ModelAdaptation export", len(plugin.Provides))
	}
}

func TestResolveShippedBundledRows(t *testing.T) {
	ctx := context.Background()
	// The GPT-family row: excluded file tools, the included hidden patch tool,
	// the description replacement, and the task-execution addition.
	got, err := resolvePlugin(t, ctx, model.ModelRef{Provider: "openai", Model: "gpt-5.5-mini"})
	if err != nil {
		t.Fatalf("Resolve(gpt): %v", err)
	}
	if want := []string{"edit_file", "write_file"}; !reflect.DeepEqual(got.ExcludeTools, want) {
		t.Fatalf("exclude tools = %q, want %q", got.ExcludeTools, want)
	}
	if want := []string{"apply_patch"}; !reflect.DeepEqual(got.IncludeTools, want) {
		t.Fatalf("include tools = %q, want %q", got.IncludeTools, want)
	}
	if got.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] != "apply_patch" {
		t.Fatalf("description replacement = %q, want the apply_patch override", got.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"])
	}
	if got.Additions["task_execution"] == "" {
		t.Fatal("task_execution addition = empty, want the shipped coaching block")
	}

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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ExcludeTools != nil || got.IncludeTools != nil || got.Blocks != nil ||
		got.Additions != nil || got.ToolDescriptionReplacements != nil {
		t.Fatalf("unmatched model adaptation = %+v, want the zero baseline", got)
	}
}

func TestResolveMatchesTheModelComponentOnly(t *testing.T) {
	ctx := context.Background()
	// A pattern inside the provider component never matches: the matcher
	// receives ref.Model alone.
	got, err := resolvePlugin(t, ctx, model.ModelRef{Provider: "gpt-5", Model: "unrelated"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ExcludeTools != nil || got.IncludeTools != nil || got.Additions != nil {
		t.Fatalf("provider-shaped ref adaptation = %+v, want the zero baseline", got)
	}
	// Matching is case-insensitive over the bare model component.
	got, err = resolvePlugin(t, ctx, model.ModelRef{Provider: "openai", Model: "GPT-5.4"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.IncludeTools == nil {
		t.Fatal("case-insensitive row did not match")
	}
}

func TestResolveReturnsIndependentContainers(t *testing.T) {
	first, err := resolvePlugin(t, context.Background(), model.ModelRef{Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first.ExcludeTools[0] = "tampered"
	first.IncludeTools[0] = "tampered"
	first.Additions["task_execution"] = "tampered"
	first.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] = "tampered"

	second, err := resolvePlugin(t, context.Background(), model.ModelRef{Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if second.ExcludeTools[0] != "edit_file" || second.IncludeTools[0] != "apply_patch" ||
		second.Additions["task_execution"] == "tampered" || second.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] != "apply_patch" {
		t.Fatalf("a mutated result leaked into the next resolve: %+v", second)
	}
}

func TestOpenRejectsCanceledScopeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adaptation.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, runtime.Bindings{}); err == nil {
		t.Fatal("Open with a canceled scope context succeeded")
	}
}
