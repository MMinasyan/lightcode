package harness

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// ToolConstraints are the selected hard tool constraints.
type ToolConstraints struct {
	Readonly bool   `json:"readonly"`
	WriteDir string `json:"write_dir"`
}

type StrategySelection struct {
	ID       string          `json:"id"`
	Settings json.RawMessage `json:"-"`
}

func (v StrategySelection) MarshalJSON() ([]byte, error) {
	var settings json.RawMessage
	if len(v.Settings) != 0 {
		settings = frame(v.Settings)
	}
	return json.Marshal(struct {
		ID       string          `json:"id"`
		Settings json.RawMessage `json:"settings_base64,omitempty"`
	}{ID: v.ID, Settings: settings})
}

func (v *StrategySelection) UnmarshalJSON(data []byte) error {
	members, err := decodePayloadObject(data)
	if err != nil {
		return err
	}
	if err := rejectUnknownMembers(members, "id", "settings_base64"); err != nil {
		return err
	}
	id, err := stringMember(members, "id", true)
	if err != nil {
		return err
	}
	var settings []byte
	if _, present := members["settings_base64"]; present {
		settings, err = framedMember(members, "settings_base64", true)
		if err != nil {
			return err
		}
	}
	v.ID, v.Settings = id, json.RawMessage(settings)
	return nil
}

type AgentInputs struct {
	Model        model.ModelRef    `json:"model"`
	Instructions string            `json:"instructions"`
	AssemblyMode string            `json:"assembly_mode"`
	Tools        []string          `json:"tools"`
	Capabilities []string          `json:"capabilities"`
	Constraints  ToolConstraints   `json:"constraints"`
	Strategy     StrategySelection `json:"strategy"`
}

func (v AgentInputs) MarshalJSON() ([]byte, error) {
	type inputs AgentInputs
	if v.Tools == nil {
		v.Tools = []string{}
	}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	var modelRaw json.RawMessage
	if v.Model != (model.ModelRef{}) {
		var err error
		modelRaw, err = encodeModelRef(v.Model)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		inputs
		Model json.RawMessage `json:"model,omitempty"`
	}{inputs: inputs(v), Model: modelRaw})
}

func (v *AgentInputs) UnmarshalJSON(data []byte) error {
	members, err := decodePayloadObject(data)
	if err != nil {
		return err
	}
	if err := rejectUnknownMembers(members, "model", "instructions", "assembly_mode", "tools", "capabilities", "constraints", "strategy"); err != nil {
		return err
	}
	for _, key := range []string{"instructions", "assembly_mode", "tools", "capabilities", "constraints", "strategy"} {
		if _, err := member(members, key, true); err != nil {
			return err
		}
	}
	constraints, err := objectMember(members, "constraints", true)
	if err != nil {
		return err
	}
	if err := rejectUnknownMembers(constraints, "readonly", "write_dir"); err != nil {
		return err
	}
	for _, key := range []string{"readonly", "write_dir"} {
		if _, err := member(constraints, key, true); err != nil {
			return err
		}
	}
	type inputs AgentInputs
	var wire struct {
		*inputs
		Model json.RawMessage `json:"model"`
	}
	wire.inputs = (*inputs)(v)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	modelObject, err := objectMember(members, "model", false)
	if err != nil {
		return err
	}
	v.Model = model.ModelRef{}
	if modelObject != nil {
		ref, err := decodeModelRef(modelObject)
		if err != nil {
			return err
		}
		v.Model = ref
	}
	return nil
}

type DefinitionRef struct {
	ExtensionID  string `json:"extension_id"`
	CapabilityID string `json:"capability_id"`
	DefinitionID string `json:"definition_id"`
}

type AgentSelection struct {
	Inputs     *AgentInputs   `json:"inputs,omitempty"`
	Definition *DefinitionRef `json:"definition,omitempty"`
}

func validateAgentSelection(v AgentSelection) error {
	if (v.Inputs == nil) == (v.Definition == nil) {
		return fmt.Errorf("selection requires exactly one of inputs or definition")
	}
	if v.Definition != nil {
		if v.Definition.ExtensionID == "" || v.Definition.CapabilityID == "" || v.Definition.DefinitionID == "" {
			return fmt.Errorf("definition reference must be complete")
		}
		return nil
	}
	input := v.Inputs
	if err := ValidateAgentInputs(*input); err != nil {
		return err
	}
	return nil
}

func ownAgentSelection(v AgentSelection) AgentSelection {
	if v.Definition != nil {
		ref := *v.Definition
		v.Definition = &ref
	}
	if v.Inputs != nil {
		inputs := *v.Inputs
		inputs.Tools = append([]string(nil), inputs.Tools...)
		inputs.Capabilities = append([]string(nil), inputs.Capabilities...)
		inputs.Strategy.Settings = append(json.RawMessage(nil), inputs.Strategy.Settings...)
		v.Inputs = &inputs
	}
	return v
}

func decodeAgentSelection(raw json.RawMessage) (AgentSelection, error) {
	var selection AgentSelection
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return selection, err
	}
	if err := rejectUnknownMembers(obj, "inputs", "definition"); err != nil {
		return selection, err
	}
	if _, err := member(obj, "inputs", false); err != nil {
		return selection, err
	}
	definition, err := objectMember(obj, "definition", false)
	if err != nil {
		return selection, err
	}
	if definition != nil {
		if err := rejectUnknownMembers(definition, "extension_id", "capability_id", "definition_id"); err != nil {
			return selection, err
		}
		for _, key := range []string{"extension_id", "capability_id", "definition_id"} {
			if _, err := stringMember(definition, key, true); err != nil {
				return selection, err
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		return selection, err
	}
	return selection, validateAgentSelection(selection)
}

// agentSystemPrompt* is the closed set of system prompt modes one AgentType
// may select.
const (
	agentSystemPromptNone   = "none"
	agentSystemPromptSimple = "simple"
	agentSystemPromptFull   = "full"
)

// ValidateAgentInputs enforces the closed selected-value shape: one of the
// three system prompt modes, a wholly empty or complete model identity,
// nonempty tool names in preserved order with duplicates allowed, and
// unique nonempty capability IDs in preserved order.
func ValidateAgentInputs(v AgentInputs) error {
	if len(v.Strategy.Settings) != 0 && !json.Valid(v.Strategy.Settings) {
		return fmt.Errorf("strategy settings must be JSON")
	}
	switch v.AssemblyMode {
	case agentSystemPromptNone, agentSystemPromptSimple, agentSystemPromptFull:
	default:
		return fmt.Errorf("assembly_mode %q is not one of %q, %q, or %q", v.AssemblyMode, agentSystemPromptNone, agentSystemPromptSimple, agentSystemPromptFull)
	}
	if (v.Model.Provider == "") != (v.Model.Model == "") {
		return fmt.Errorf("model must be wholly empty or a complete identity, got provider %q model %q", v.Model.Provider, v.Model.Model)
	}
	for i, tool := range v.Tools {
		if tool == "" {
			return fmt.Errorf("tools[%d]: name must be non-empty", i)
		}
	}
	seen := make(map[string]bool, len(v.Capabilities))
	for i, capability := range v.Capabilities {
		if capability == "" {
			return fmt.Errorf("capabilities[%d]: ID must be non-empty", i)
		}
		if seen[capability] {
			return fmt.Errorf("capabilities[%d]: duplicate ID %q", i, capability)
		}
		seen[capability] = true
	}
	return nil
}
