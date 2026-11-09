package clientops

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/protocol"
)

// AgentRoster decodes the required roster used for initial Session selection.
func AgentRoster(raw json.RawMessage) ([]protocol.Agent, error) {
	var view struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("invalid agent roster: %w", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(view.Agents, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("invalid agent roster")
	}
	out := make([]protocol.Agent, 0, len(entries))
	seen := make(map[protocol.DefinitionRef]bool, len(entries))
	for _, entry := range entries {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(entry, &members); err != nil || members == nil {
			return nil, fmt.Errorf("invalid agent roster entry")
		}
		for _, key := range []string{"name", "definition", "description", "subagent", "model", "readonly", "write_dir", "tools", "capabilities"} {
			if value, ok := members[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("invalid agent roster entry: missing %s", key)
			}
		}
		for _, key := range []string{"tools", "capabilities"} {
			var values []json.RawMessage
			if err := json.Unmarshal(members[key], &values); err != nil || values == nil {
				return nil, fmt.Errorf("invalid agent roster entry: %s", key)
			}
			for _, value := range values {
				var text string
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
					return nil, fmt.Errorf("invalid agent roster entry: %s", key)
				}
			}
		}
		var agent protocol.Agent
		decoder := json.NewDecoder(bytes.NewReader(entry))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&agent); err != nil || agent.Name == "" || agent.Definition.ExtensionId == "" || agent.Definition.CapabilityId == "" || agent.Definition.DefinitionId == "" || seen[agent.Definition] {
			return nil, fmt.Errorf("invalid agent roster entry")
		}
		seen[agent.Definition] = true
		out = append(out, agent)
	}
	return out, nil
}
