package harness

import (
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func testAgentTypes() []AgentType {
	return []AgentType{
		{
			Name:         "primary",
			Model:        model.ModelRef{Provider: "prov", Model: "gpt-x"},
			SystemPrompt: "full",
			Prompt:       "primary prompt",
			Tools:        []string{"read_file", "write_file", "read_file"},
			Capabilities: []string{"cap-b", "cap-a"},
			Readonly:     true,
			WriteDir:     "/w/sub",
		},
		{
			Name:         "explore",
			SystemPrompt: "simple",
			Prompt:       "explore prompt",
			Tools:        []string{"read_file"},
		},
	}
}

// TestResolveAgentTypeSelectsExactName proves the pure lookup: the exact-name
// match returns every selected field with the loaded tool order and duplicates
// and the selected capability order preserved, the selection is the requested
// name with no fallback, and the returned value is owned rather than aliased
// to the supplied definitions.
func TestResolveAgentTypeSelectsExactName(t *testing.T) {
	available := testAgentTypes()

	selected, err := ResolveAgentType("primary", available)
	testassert.NoError(t, err)
	testassert.Require(t, selected.Name == "primary" && selected.Model == available[0].Model && selected.SystemPrompt == "full" && selected.Prompt == "primary prompt", "selected = %+v, want the primary fields unchanged", selected)
	{
		want := []string{"read_file", "write_file", "read_file"}
		testassert.Require(t, len(selected.Tools) == 3 && selected.Tools[0] == want[0] && selected.Tools[1] == want[1] && selected.Tools[2] == want[2], "tools = %q, want loaded order with duplicates retained", selected.Tools)
	}
	{
		want := []string{"cap-b", "cap-a"}
		testassert.Require(t, len(selected.Capabilities) == 2 && selected.Capabilities[0] == want[0] && selected.Capabilities[1] == want[1], "capabilities = %q, want selected order", selected.Capabilities)
	}

	other, err := ResolveAgentType("explore", available)
	testassert.NoError(t, err)
	testassert.Require(t, other.Model == (model.ModelRef{}) && other.Capabilities == nil && !other.Readonly && other.WriteDir == "", "two Agent types share selection state: %+v", other)

	selected.Tools[0] = "tampered"
	selected.Capabilities[0] = "tampered"
	again, err := ResolveAgentType("primary", available)
	testassert.NoError(t, err)
	testassert.Require(t, again.Tools[0] == "read_file" && again.Capabilities[0] == "cap-b", "selection aliases the supplied definitions: %+v", again)
}

// TestResolveAgentTypeRejections proves the invalid-input matrix: an empty or
// unknown name, duplicate definitions for the requested name, and invalid
// selected values all return ErrInvalid with no fallback selection.
func TestResolveAgentTypeRejections(t *testing.T) {
	available := testAgentTypes()
	for _, row := range []struct {
		name      string
		available []AgentType
	}{
		{"empty name", available},
		{"unknown name", available},
		{"unknown case-sensitive name", available},
		{"duplicate definitions", append(append([]AgentType{}, available...), AgentType{Name: "primary", SystemPrompt: "full"})},
		{"invalid system prompt", []AgentType{{Name: "x", SystemPrompt: "huge"}}},
		{"empty system prompt", []AgentType{{Name: "x"}}},
		{"partial model provider", []AgentType{{Name: "x", SystemPrompt: "none", Model: model.ModelRef{Provider: "prov"}}}},
		{"partial model name", []AgentType{{Name: "x", SystemPrompt: "none", Model: model.ModelRef{Model: "m"}}}},
		{"empty tool name", []AgentType{{Name: "x", SystemPrompt: "none", Tools: []string{"ok", ""}}}},
		{"empty capability ID", []AgentType{{Name: "x", SystemPrompt: "none", Capabilities: []string{""}}}},
		{"duplicate capability ID", []AgentType{{Name: "x", SystemPrompt: "none", Capabilities: []string{"cap-a", "cap-a"}}}},
	} {
		var (
			got AgentType
			err error
		)
		switch row.name {
		case "empty name":
			got, err = ResolveAgentType("", available)
		case "unknown name":
			got, err = ResolveAgentType("ghost", available)
		case "unknown case-sensitive name":
			got, err = ResolveAgentType("Primary", available)
		default:
			got, err = ResolveAgentType("primary", row.available)
			if row.name != "duplicate definitions" {
				got, err = ResolveAgentType("x", row.available)
			}
		}
		testassert.Require(t, errors.Is(err, ErrInvalid), "%s: err = %v, want ErrInvalid", row.name, err)
		testassert.Require(t, got.Name == "" && got.Model == (model.ModelRef{}) && got.SystemPrompt == "" && got.Prompt == "" && got.Tools == nil && got.Capabilities == nil, "%s: returned %+v alongside the error, want the zero value", row.name, got)
	}
}

// TestResolveAgentTypeAcceptsEmptySelection proves the wholly-empty model,
// absent tools, and an absent capability selection are valid selected values,
// while duplicate tools stay valid because uniqueness belongs to the
// advertised definitions, not the configured names.
func TestResolveAgentTypeAcceptsEmptySelection(t *testing.T) {
	available := []AgentType{{Name: "bare", SystemPrompt: "none", Tools: []string{"t", "t"}}}
	selected, err := ResolveAgentType("bare", available)
	testassert.NoError(t, err)
	testassert.Require(t, selected.Model.IsZero() && selected.Prompt == "" && selected.Capabilities == nil && len(selected.Tools) == 2, "selected = %+v; tools = %q, want duplicates retained", selected, selected.Tools)
}
