package tool_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/engine/tool"
	runtimetool "github.com/MMinasyan/lightcode/internal/tool"
)

// fakeTool is a minimal Tool for advertisement tests.
type fakeTool struct{ name string }

func (f fakeTool) Name() string                                            { return f.name }
func (f fakeTool) Description() string                                     { return f.name + " description" }
func (f fakeTool) ParametersSchema() map[string]any                        { return map[string]any{"type": "object"} }
func (f fakeTool) Execute(context.Context, map[string]any) (string, error) { return "", nil }

// hiddenFakeTool opts out of the baseline advertisement.
type hiddenFakeTool struct{ fakeTool }

func (hiddenFakeTool) DefaultHidden() bool { return true }

func advertisedNames(tools []openai.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

func TestAdvertisedToolsNilMatchesOpenAIToolsWithoutHidden(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(fakeTool{name: "read_file"})
	r.Register(fakeTool{name: "edit_file"})
	if !reflect.DeepEqual(r.AdvertisedTools(nil), r.OpenAITools()) {
		t.Fatal("AdvertisedTools(nil) != OpenAITools() with no DefaultHidden tools")
	}
}

func TestDefaultHiddenAbsentFromBaselineRevealedByInclude(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(fakeTool{name: "read_file"})
	r.Register(hiddenFakeTool{fakeTool{name: "apply_patch"}})

	if r.Advertises("apply_patch", nil) {
		t.Fatal("DefaultHidden tool advertised under baseline")
	}
	if slices.Contains(advertisedNames(r.AdvertisedTools(nil)), "apply_patch") {
		t.Fatal("DefaultHidden tool present in AdvertisedTools(nil)")
	}
	// With a hidden tool registered, the advertised baseline differs from the
	// full OpenAITools() set (which lists every registered tool) — by design.
	if reflect.DeepEqual(r.AdvertisedTools(nil), r.OpenAITools()) {
		t.Fatal("AdvertisedTools(nil) should differ from OpenAITools() when a hidden tool is registered")
	}

	adapt := &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}}
	if !r.Advertises("apply_patch", adapt) {
		t.Fatal("included hidden tool not advertised")
	}
	if !slices.Contains(advertisedNames(r.AdvertisedTools(adapt)), "apply_patch") {
		t.Fatal("included hidden tool missing from advertised set")
	}
}

func TestExcludeWithholdsAndPreservesOrder(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(fakeTool{name: "read_file"})
	r.Register(fakeTool{name: "edit_file"})
	r.Register(fakeTool{name: "write_file"})
	adapt := &adaptation.Adaptation{ExcludeTools: []string{"edit_file", "write_file"}}
	got := advertisedNames(r.AdvertisedTools(adapt))
	if want := []string{"read_file"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertised = %v, want %v", got, want)
	}
}

func TestDefaultHiddenSeenThroughPermWrapped(t *testing.T) {
	r := tool.NewRegistry()
	// PermWrapped forwards Name/Description/ParametersSchema/WrappedTool but not
	// DefaultHidden; the registry must unwrap to the inner tool to see it.
	r.Register(runtimetool.WrapWithPermission(hiddenFakeTool{fakeTool{name: "apply_patch"}}, nil, nil))
	if r.Advertises("apply_patch", nil) {
		t.Fatal("DefaultHidden not detected through PermWrapped (unwrap failed)")
	}
	adapt := &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}}
	if !r.Advertises("apply_patch", adapt) {
		t.Fatal("included hidden tool (PermWrapped) not advertised")
	}
}

func TestIncludeUnregisteredIsNoOp(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(fakeTool{name: "read_file"})
	adapt := &adaptation.Adaptation{IncludeTools: []string{"ghost_tool"}}
	if slices.Contains(advertisedNames(r.AdvertisedTools(adapt)), "ghost_tool") {
		t.Fatal("phantom tool advertised from an unregistered include")
	}
	if r.Advertises("ghost_tool", adapt) {
		t.Fatal("Advertises reported an unregistered include as advertised")
	}
}

func TestExcludePlusIncludePreservesRegistrationOrder(t *testing.T) {
	r := tool.NewRegistry()
	r.Register(fakeTool{name: "read_file"})
	r.Register(fakeTool{name: "edit_file"})
	r.Register(fakeTool{name: "write_file"})
	r.Register(hiddenFakeTool{fakeTool{name: "apply_patch"}})
	adapt := &adaptation.Adaptation{
		ExcludeTools: []string{"edit_file", "write_file"},
		IncludeTools: []string{"apply_patch"},
	}
	// Excluded pair gone, hidden fixture revealed, registration order otherwise kept.
	got := advertisedNames(r.AdvertisedTools(adapt))
	if want := []string{"read_file", "apply_patch"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertised = %v, want %v", got, want)
	}
}
