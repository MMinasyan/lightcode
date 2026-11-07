package harness

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

func testSelection(name string) AgentSelection {
	return AgentSelection{Definition: &DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}}
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
