package extension

import (
	"encoding/json"

	"github.com/MMinasyan/lightcode/harness"
)

// AgentDefinitionSummary is one resolver's list entry: the displayed
// definition name and its description. The owning consumer stamps the
// source-qualified reference from the supplying declaration; a resolver
// result never carries provider identity.
type AgentDefinitionSummary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// AgentDefinitionResolution is one resolver's resolved definition: the
// complete AgentInputs value, the display description and the task
// eligibility flag.
type AgentDefinitionResolution struct {
	Inputs      harness.AgentInputs `json:"inputs"`
	Description string              `json:"description"`
	Subagent    bool                `json:"subagent"`
}

// agentDefinitionMethods is the fixed melta.agent_definitions/v1 service
// method set: one pure listing and one pure resolution. A read-only resolver
// supplies exactly these methods and no configuration edit.
func agentDefinitionMethods() []Method {
	return []Method{
		method("list", ModePure,
			string(defsSchema(`{"type":"object","properties":{},"additionalProperties":false}`)),
			string(defsSchema(`{"type":"array","items":{"type":"object","required":["id","description"],"properties":{"id":{"type":"string","minLength":1},"description":{"type":"string"}},"additionalProperties":false}}`)), "", ""),
		method("resolve", ModePure,
			string(defsSchema(`{"type":"object","required":["id"],"properties":{"id":{"type":"string","minLength":1}},"additionalProperties":false}`)),
			string(defsSchema(`{"type":"object","required":["inputs","description","subagent"],"properties":{"inputs":{"$ref":"#/$defs/agentInputs"},"description":{"type":"string"},"subagent":{"type":"boolean"}},"additionalProperties":false}`)), "", ""),
	}
}

// AgentDefinitionsCapability constructs the fixed melta.agent_definitions/v1
// named-service capability under one concrete capability ID.
func AgentDefinitionsCapability(id string) Capability {
	return Capability{
		ID:              id,
		ContractID:      ContractAgentDefinitions,
		ContractVersion: ContractVersion,
		Kind:            KindService,
		Methods:         agentDefinitionMethods(),
	}
}

// DecodeAgentDefinitionList decodes and semantically owns one resolver list
// success value. Empty or duplicate IDs are invalid: IDs address one
// definition inside their own resolver.
func DecodeAgentDefinitionList(raw json.RawMessage) ([]AgentDefinitionSummary, error) {
	var list []AgentDefinitionSummary
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, invalid("agent definitions list: %v", err)
	}
	seen := make(map[string]bool, len(list))
	for i, summary := range list {
		if summary.ID == "" {
			return nil, invalid("agent definitions list: entry %d has an empty id", i)
		}
		if seen[summary.ID] {
			return nil, invalid("agent definitions list: duplicate id %q", summary.ID)
		}
		seen[summary.ID] = true
	}
	return list, nil
}

// DecodeAgentDefinitionResolution decodes and semantically validates one
// resolver resolution success value: the shared AgentInputs shape is
// validated at this owning contract boundary, so every consumer accepts the
// same resolved values.
func DecodeAgentDefinitionResolution(raw json.RawMessage) (AgentDefinitionResolution, error) {
	var resolution AgentDefinitionResolution
	if err := json.Unmarshal(raw, &resolution); err != nil {
		return AgentDefinitionResolution{}, invalid("agent definition resolution: %v", err)
	}
	if err := harness.ValidateAgentInputs(resolution.Inputs); err != nil {
		return AgentDefinitionResolution{}, invalid("agent definition resolution: %v", err)
	}
	return resolution, nil
}
