package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

func suppliedDefinition(name string) harness.AgentSelection {
	return harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}}
}

func defaultAgentSelection() harness.AgentSelection {
	declarations := standardToolDeclarations()
	tools := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		tools = append(tools, declaration.id)
	}
	return harness.AgentSelection{Inputs: &harness.AgentInputs{AssemblyMode: "full", Tools: tools}}
}

func selectionFromProtocol(value protocol.AgentSelection) (harness.AgentSelection, error) {
	var out harness.AgentSelection
	if value.Definition != nil {
		out.Definition = &harness.DefinitionRef{ExtensionID: value.Definition.ExtensionId, CapabilityID: value.Definition.CapabilityId, DefinitionID: value.Definition.DefinitionId}
	}
	if value.Inputs != nil {
		input := value.Inputs
		var ref model.ModelRef
		if input.Model != "" {
			var err error
			ref, err = model.Parse(string(input.Model))
			if err != nil {
				return out, fmt.Errorf("model: %v: %w", err, harness.ErrInvalid)
			}
		}
		out.Inputs = &harness.AgentInputs{Model: ref, Instructions: input.Instructions, AssemblyMode: string(input.AssemblyMode), Tools: append([]string(nil), input.Tools...), Capabilities: append([]string(nil), input.Capabilities...), Constraints: harness.ToolConstraints{Readonly: input.Constraints.Readonly, WriteDir: input.Constraints.WriteDir}, Strategy: harness.StrategySelection{ID: input.Strategy.Id, Settings: json.RawMessage(input.Strategy.Settings)}}
	}
	return out, nil
}

func projectAgentSelection(value harness.AgentSelection) protocol.AgentSelection {
	var out protocol.AgentSelection
	if value.Definition != nil {
		out.Definition = &protocol.DefinitionRef{ExtensionId: value.Definition.ExtensionID, CapabilityId: value.Definition.CapabilityID, DefinitionId: value.Definition.DefinitionID}
	}
	if value.Inputs != nil {
		input := value.Inputs
		out.Inputs = &protocol.AgentInputs{Model: protocol.ModelRef(input.Model.String()), Instructions: input.Instructions, AssemblyMode: protocol.AgentInputsAssemblyMode(input.AssemblyMode), Tools: ownStrings(input.Tools), Capabilities: ownStrings(input.Capabilities), Constraints: protocol.ToolConstraints{Readonly: input.Constraints.Readonly, WriteDir: input.Constraints.WriteDir}, Strategy: protocol.StrategySelection{Id: input.Strategy.ID, Settings: string(input.Strategy.Settings)}}
	}
	return out
}
