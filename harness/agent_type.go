package harness

import (
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// agentSystemPrompt* is the closed set of system prompt modes one AgentType
// may select.
const (
	agentSystemPromptNone   = "none"
	agentSystemPromptSimple = "simple"
	agentSystemPromptFull   = "full"
)

// AgentType is the Harness view of one loaded Agent definition: the name,
// model identity, prompt fields, configured tool names in loaded order
// (duplicates retained), and selected capability IDs in selected order.
// The caller supplies the definitions; resolving selects one by exact name
// and never loads a plugin, resolves a tool, or changes a selection.
type AgentType struct {
	Name         string
	Model        model.ModelRef
	SystemPrompt string
	Prompt       string
	Tools        []string
	Capabilities []string
}

// ResolveAgentType returns an owned copy of the exactly-named AgentType from
// the available definitions. An empty or unknown name, a duplicate
// definition for the requested name, or invalid selected values return
// ErrInvalid. A wholly empty model is a valid selection; a partial identity
// is not. There is no fallback.
func ResolveAgentType(name string, available []AgentType) (AgentType, error) {
	if name == "" {
		return AgentType{}, invalidInput("agent type name must be non-empty")
	}
	found := -1
	for i := range available {
		if available[i].Name != name {
			continue
		}
		if found >= 0 {
			return AgentType{}, invalidInput("agent type %q has duplicate definitions", name)
		}
		found = i
	}
	if found < 0 {
		return AgentType{}, invalidInput("unknown agent type %q", name)
	}
	selected := available[found]
	if err := validateAgentType(selected); err != nil {
		return AgentType{}, invalidInput("agent type %q: %v", name, err)
	}
	out := selected
	out.Tools = append([]string(nil), selected.Tools...)
	out.Capabilities = append([]string(nil), selected.Capabilities...)
	return out, nil
}

// validateAgentType enforces the closed selected-value shape: one of the
// three system prompt modes, a wholly empty or complete model identity,
// nonempty tool names in preserved order with duplicates allowed, and
// unique nonempty capability IDs in preserved order.
func validateAgentType(v AgentType) error {
	switch v.SystemPrompt {
	case agentSystemPromptNone, agentSystemPromptSimple, agentSystemPromptFull:
	default:
		return fmt.Errorf("system_prompt %q is not one of %q, %q, or %q", v.SystemPrompt, agentSystemPromptNone, agentSystemPromptSimple, agentSystemPromptFull)
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
