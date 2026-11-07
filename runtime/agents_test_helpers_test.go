package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/extension"
	agents "github.com/MMinasyan/lightcode/extensions/agents"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// agentType is a test-only assertion view over one projection entry. It is
// not an execution value: production admission and capture consume the
// complete harness.AgentInputs.
type agentType struct {
	Name         string
	Model        model.ModelRef
	SystemPrompt string
	Prompt       string
	Tools        []string
	Capabilities []string
	Readonly     bool
	WriteDir     string
}

// suppliedDefinition builds one supplied Agents named selection for tests.
func suppliedDefinition(name string) harness.AgentSelection {
	return harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}}
}

// agentTypes projects the captured immutable roster projection onto the
// Harness execution view for tests that assert the resolved values. Named
// admission resolves afresh through the provider; this helper reads only the
// published projection.
func (c *configuration) agentTypes() []agentType {
	if c == nil {
		return nil
	}
	out := make([]agentType, 0, len(c.definitions))
	for _, definition := range c.definitions {
		inputs := definition.inputs
		out = append(out, agentType{
			Name:         definition.name,
			Model:        inputs.Model,
			SystemPrompt: inputs.AssemblyMode,
			Prompt:       inputs.Instructions,
			Tools:        append([]string(nil), inputs.Tools...),
			Capabilities: append([]string(nil), inputs.Capabilities...),
			Readonly:     inputs.Constraints.Readonly,
			WriteDir:     inputs.Constraints.WriteDir,
		})
	}
	return out
}

func resolveAgentType(name string, available []agentType) (agentType, error) {
	for _, agent := range available {
		if agent.Name == name {
			return agent, nil
		}
	}
	return agentType{}, fmt.Errorf("unknown agent type %q: %w", name, harness.ErrInvalid)
}

// agentsTestNative builds the real supplied Agents extension over the
// registered universe the caller's fixture composes: the standard tool
// capabilities plus every registered fixture declaration. Tests that need a
// deliberately invalid universe pass none of the fixture plugins.
func agentsTestNative(registered ...extension.Native) extension.Native {
	return agentsTestNativeWithDefaults(nil, registered...)
}

// agentsTestNativeWithDefaults is agentsTestNative with one explicit
// constructor default capability selection.
func agentsTestNativeWithDefaults(defaults []string, registered ...extension.Native) extension.Native {
	capabilities := append([]string(nil), agents.StandardTools...)
	tools := append([]string(nil), agents.StandardTools...)
	capabilities = append(capabilities, agents.DefinitionsCapabilityID, agents.ConfigurationCapabilityID, "model_adaptation")
	for _, native := range registered {
		for _, capability := range native.Declaration.Capabilities {
			capabilities = append(capabilities, capability.ID)
			if capability.Kind == extension.KindTool {
				tools = append(tools, capability.ID)
			}
		}
	}
	return agentsTestNativeIDs(capabilities, tools, defaults)
}

// agentsTestNativeIDs builds the real supplied Agents extension over explicit
// constructor universes.
func agentsTestNativeIDs(capabilities, tools, defaults []string) extension.Native {
	return agents.Native(agents.Options{CapabilityIDs: capabilities, ToolIDs: tools, DefaultCapabilityIDs: defaults})
}

// preparedService is the isolated service harness over the production
// resource path: the composition and data root are attached so candidate
// publication prepares the Harness scope and the roster projection.
func (h *serviceHarness) preparedService(owner context.Context, plugins ...extension.Native) *configurationService {
	h.t.Helper()
	svc := h.service(owner, plugins...)
	c, err := newNativeComposition(plugins)
	if err != nil {
		h.t.Fatalf("newComposition: %v", err)
	}
	svc.composition = c
	svc.dataDir = h.dataDir
	return svc
}

// editAgentModel drives the supplied set_model configuration edit through the
// one extension-edit path for tests that assert the retained model semantics.
func (s *configurationService) editAgentModel(agentType, ref string) configurationEdit {
	arguments, _ := json.Marshal(struct {
		DefinitionID string `json:"definition_id"`
		Model        string `json:"model"`
	}{DefinitionID: agentType, Model: ref})
	return s.editExtension(context.Background(), "agents", "set_model", arguments)
}

// updateSettingsOnly drives the settings arm of the settings/extension-edit
// request union.
func updateSettingsOnly(r *Runtime, ctx context.Context, settings protocol.Settings) (protocol.SettingsMutation, error) {
	return r.updateSettings(ctx, protocol.UpdateSettingsRequest{Settings: &settings})
}

// setAgentTypeModel drives the supplied set_model extension edit through the
// public UpdateSettings request for tests that assert the retained model
// propagation.
func (r *Runtime) setAgentTypeModel(ctx context.Context, agentType, ref string) (protocol.SettingsMutation, error) {
	arguments, err := json.Marshal(struct {
		DefinitionID string `json:"definition_id"`
		Model        string `json:"model"`
	}{DefinitionID: agentType, Model: ref})
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	return r.updateSettings(ctx, protocol.UpdateSettingsRequest{ExtensionEdit: &protocol.ExtensionEdit{
		ExtensionId: "agents",
		EditId:      "set_model",
		Arguments:   string(arguments),
	}})
}
