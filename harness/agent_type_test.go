package harness

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Syntheresis/melta/model"
)

func testSelection(name string) AgentSelection {
	return AgentSelection{Definition: &DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}}
}

// TestDefinitionRefPermissionTarget proves the canonical target carries the
// complete reference: each opaque component is path-escaped before the fixed
// separator join, so delimiter and percent bytes cannot make distinct
// references share one target and arbitrary bytes survive.
func TestDefinitionRefPermissionTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  DefinitionRef
		want string
	}{
		{"plain", DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "secondary"}, "agents/agents.definitions/secondary"},
		{"slash in extension", DefinitionRef{ExtensionID: "a/b", CapabilityID: "c", DefinitionID: "d"}, "a%2Fb/c/d"},
		{"literal percent in capability", DefinitionRef{ExtensionID: "a", CapabilityID: "b%2Fc", DefinitionID: "d"}, "a/b%252Fc/d"},
		{"delimiters, space and question in definition", DefinitionRef{ExtensionID: "a", CapabilityID: "b", DefinitionID: "c/d% e?"}, "a/b/c%2Fd%25%20e%3F"},
		{"unicode and arbitrary byte", DefinitionRef{ExtensionID: "a", CapabilityID: "b", DefinitionID: "\u00fc\xff"}, "a/b/%C3%BC%FF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.PermissionTarget(); got != tc.want {
				t.Fatalf("PermissionTarget(%+v) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
	refs := []DefinitionRef{
		{ExtensionID: "a/b", CapabilityID: "c", DefinitionID: "d"},
		{ExtensionID: "a", CapabilityID: "b/c", DefinitionID: "d"},
		{ExtensionID: "a", CapabilityID: "b", DefinitionID: "c/d"},
		{ExtensionID: "a%2Fb", CapabilityID: "c", DefinitionID: "d"},
		{ExtensionID: "a", CapabilityID: "b%2Fc", DefinitionID: "d"},
		{ExtensionID: "a", CapabilityID: "b", DefinitionID: "c%2Fd"},
	}
	seen := make(map[string]DefinitionRef, len(refs))
	for _, ref := range refs {
		target := ref.PermissionTarget()
		if prior, ok := seen[target]; ok {
			t.Fatalf("distinct references %+v and %+v share target %q", prior, ref, target)
		}
		seen[target] = ref
	}
}

func TestAgentSelectionValidationAndOwnership(t *testing.T) {
	inputs := AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "gpt-x"}, AssemblyMode: "full", Instructions: "prompt", Tools: []string{"read_file", "write_file", "read_file"}, Capabilities: []string{"cap-b", "cap-a"}, Constraints: ToolConstraints{Readonly: true, WriteDir: "/w/sub"}, Strategy: StrategySelection{Settings: json.RawMessage(" \n{\"exact\":9007199254740993} \n")}}
	valid := AgentSelection{Inputs: &inputs}
	if err := validateAgentSelection(valid); err != nil {
		t.Fatal(err)
	}
	copy := ownAgentSelection(valid)
	copy.Inputs.Tools[0], copy.Inputs.Capabilities[0], copy.Inputs.Strategy.Settings[0] = "changed", "changed", '['
	if inputs.Tools[0] != "read_file" || inputs.Capabilities[0] != "cap-b" || inputs.Strategy.Settings[0] != ' ' {
		t.Fatal("selection aliases caller data")
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAgentSelection(data)
	if err != nil || !reflect.DeepEqual(decoded, valid) {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	for _, selection := range []AgentSelection{testSelection("primary"), {Inputs: &AgentInputs{AssemblyMode: "none", Tools: []string{"t", "t"}}}} {
		if err := validateAgentSelection(selection); err != nil {
			t.Fatalf("valid selection %+v: %v", selection, err)
		}
	}
	for name, selection := range map[string]AgentSelection{
		"empty":                {},
		"both":                 {Inputs: &inputs, Definition: testSelection("primary").Definition},
		"partial reference":    {Definition: &DefinitionRef{ExtensionID: "agents"}},
		"mode":                 {Inputs: &AgentInputs{AssemblyMode: "other"}},
		"partial model":        {Inputs: &AgentInputs{AssemblyMode: "full", Model: model.ModelRef{Provider: "p"}}},
		"empty tool":           {Inputs: &AgentInputs{AssemblyMode: "full", Tools: []string{""}}},
		"duplicate capability": {Inputs: &AgentInputs{AssemblyMode: "full", Capabilities: []string{"x", "x"}}},
		"bad settings":         {Inputs: &AgentInputs{AssemblyMode: "full", Strategy: StrategySelection{Settings: json.RawMessage(` `)}}},
	} {
		if err := validateAgentSelection(selection); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, raw := range map[string]string{
		"null sibling":           `{"inputs":null,"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"}}`,
		"miscased member":        `{"Definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary"}}`,
		"extra reference member": `{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"primary","extra":1}}`,
		"missing input fields":   `{"inputs":{"assembly_mode":"full"}}`,
		"null constraint":        `{"inputs":{"instructions":"","assembly_mode":"full","tools":[],"capabilities":[],"constraints":{"readonly":null,"write_dir":""},"strategy":{"id":""}}}`,
	} {
		if _, err := decodeAgentSelection(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted: %s", name, raw)
		}
	}
}

func TestAgentSelectionRetainsStrategyBytes(t *testing.T) {
	settings := json.RawMessage{'"', 0xff, '"'}
	if !json.Valid(settings) {
		t.Fatal("fixture is not accepted JSON")
	}
	selection := AgentSelection{Inputs: &AgentInputs{AssemblyMode: "full", Strategy: StrategySelection{Settings: settings}}}
	wire, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAgentSelection(wire)
	if err != nil || !bytes.Equal(decoded.Inputs.Strategy.Settings, settings) {
		t.Fatalf("strategy settings = %x, want %x (error %v)", decoded.Inputs.Strategy.Settings, settings, err)
	}
}
