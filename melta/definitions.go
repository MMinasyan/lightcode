package melta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
)

// agentDefinition is one immutable roster projection built from a resolver's
// list/resolve pair after the captured Harness resources are ready. It is
// display and hydration data: a named admission always resolves again through
// the selected provider.
type agentDefinition struct {
	ref         harness.DefinitionRef
	name        string
	description string
	subagent    bool
	inputs      harness.AgentInputs
}

// projectDefinitions builds the immutable roster projection from every active
// melta.agent_definitions/v1 provider in the captured composition. It runs
// after resource preparation on every publication and mutation path; the
// contract's schema validation and the shared semantic decoders own each
// accepted value, and the owner stamps each full reference from the supplying
// declaration, so a guest's display metadata can never forge another
// provider's source.
func (s *configurationService) projectDefinitions(ctx context.Context, snapshot *configuration) error {
	snapshot.definitions = nil
	if snapshot.resources == nil || snapshot.resources.runtime == nil {
		return nil
	}
	composition := snapshot.composition
	for _, declaration := range composition.compiled.Declarations() {
		for _, declared := range declaration.Declaration.Capabilities {
			if declared.ContractID != extension.ContractAgentDefinitions || declared.ContractVersion != extension.ContractVersion || declared.Kind != extension.KindService {
				continue
			}
			capability, ok := declaration.Capabilities[declared.ID]
			if !ok {
				return configurationFailure(fmt.Errorf("definition resolver %q is not compiled", declared.ID))
			}
			value, ok := snapshot.resources.runtime.capabilities[capability.ID]
			if !ok {
				return configurationFailure(fmt.Errorf("definition resolver %q is not open in the Harness scope", capability.ID))
			}
			identity := callIdentity{revision: snapshot.resources.revisionString(), grant: nativeGrant{pure: true}}
			listed, err := composition.invokeProvider(ctx, value, capability.Methods, "list", json.RawMessage(`{}`), nil, identity, false)
			if err != nil {
				return configurationFailure(fmt.Errorf("definition resolver %q: %w", capability.ID, err))
			}
			if listed.Failure != nil {
				return configurationFailure(fmt.Errorf("definition resolver %q: %s: %s", capability.ID, listed.Failure.Code, listed.Failure.Message))
			}
			summaries, err := extension.DecodeAgentDefinitionList(listed.Value)
			if err != nil {
				return configurationFailure(fmt.Errorf("definition resolver %q: %w", capability.ID, err))
			}
			for _, summary := range summaries {
				arguments, marshalErr := json.Marshal(struct {
					ID string `json:"id"`
				}{ID: summary.ID})
				if marshalErr != nil {
					return configurationFailure(fmt.Errorf("definition resolver %q: %w", capability.ID, marshalErr))
				}
				resolved, callErr := composition.invokeProvider(ctx, value, capability.Methods, "resolve", arguments, nil, identity, false)
				if callErr != nil {
					return configurationFailure(fmt.Errorf("definition resolver %q: %w", capability.ID, callErr))
				}
				if resolved.Failure != nil {
					return configurationFailure(fmt.Errorf("definition resolver %q: %s: %s", capability.ID, resolved.Failure.Code, resolved.Failure.Message))
				}
				resolution, decodeErr := extension.DecodeAgentDefinitionResolution(resolved.Value)
				if decodeErr != nil {
					return configurationFailure(fmt.Errorf("definition resolver %q: %w", capability.ID, decodeErr))
				}
				snapshot.definitions = append(snapshot.definitions, agentDefinition{
					ref:         harness.DefinitionRef{ExtensionID: declaration.Declaration.ID, CapabilityID: capability.ID, DefinitionID: summary.ID},
					name:        summary.ID,
					description: resolution.Description,
					subagent:    resolution.Subagent,
					inputs:      resolution.Inputs.Owned(),
				})
			}
		}
	}
	return nil
}

// resolveDefinition resolves one named selection through its concrete
// provider under the Caller's already-owned preparation reservation. The
// provider result is the complete executable input set: the roster projection
// is never used for admission, and no lossy execution copy is derived from it.
func resolveDefinition(ctx context.Context, snapshot *configuration, ref harness.DefinitionRef) (harness.AgentInputs, error) {
	if snapshot == nil || snapshot.composition == nil || snapshot.resources == nil || snapshot.resources.runtime == nil {
		return harness.AgentInputs{}, fmt.Errorf("no published configuration: %w", harness.ErrInvalid)
	}
	capability, ok := snapshot.composition.compiled.Capability(ref.CapabilityID)
	if !ok || capability.ContractID != extension.ContractAgentDefinitions || capability.ContractVersion != extension.ContractVersion || capability.Kind != extension.KindService {
		return harness.AgentInputs{}, fmt.Errorf("unknown definition resolver %q: %w", ref.CapabilityID, harness.ErrInvalid)
	}
	if capability.Declaration.Declaration.ID != ref.ExtensionID {
		return harness.AgentInputs{}, fmt.Errorf("definition resolver %q is not supplied by extension %q: %w", ref.CapabilityID, ref.ExtensionID, harness.ErrInvalid)
	}
	value, ok := snapshot.resources.runtime.capabilities[capability.ID]
	if !ok {
		return harness.AgentInputs{}, fmt.Errorf("definition resolver %q is not open: %w", capability.ID, harness.ErrInvalid)
	}
	arguments, err := json.Marshal(struct {
		ID string `json:"id"`
	}{ID: ref.DefinitionID})
	if err != nil {
		return harness.AgentInputs{}, err
	}
	identity := callIdentity{revision: snapshot.resources.revisionString(), grant: nativeGrant{pure: true}}
	result, err := snapshot.composition.invokeProvider(ctx, value, capability.Methods, "resolve", arguments, nil, identity, false)
	if err != nil {
		return harness.AgentInputs{}, fmt.Errorf("resolve definition %q: %w: %w", ref.DefinitionID, err, harness.ErrInvalid)
	}
	if result.Failure != nil {
		return harness.AgentInputs{}, fmt.Errorf("resolve definition %q: %s: %s: %w", ref.DefinitionID, result.Failure.Code, result.Failure.Message, harness.ErrInvalid)
	}
	resolution, err := extension.DecodeAgentDefinitionResolution(result.Value)
	if err != nil {
		return harness.AgentInputs{}, fmt.Errorf("resolve definition %q: %w: %w", ref.DefinitionID, err, harness.ErrInvalid)
	}
	return resolution.Inputs.Owned(), nil
}

// projectedDefinition resolves one captured roster projection by its exact
// source-qualified reference for read-only configuration and hydration
// values. Two providers may use the same displayed name; only the full
// reference addresses one entry.
func (c *configuration) projectedDefinition(ref harness.DefinitionRef) (agentDefinition, bool) {
	for _, definition := range c.definitions {
		if definition.ref == ref {
			return definition, true
		}
	}
	return agentDefinition{}, false
}
