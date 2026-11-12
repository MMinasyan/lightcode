package worker

import "github.com/Syntheresis/melta/extension"

// Declaration is the wire form of one extension declaration. Every schema and
// parameter byte field is a JSONBytes value so the outer frame encoder never
// compacts or reserializes it.
type Declaration struct {
	ID           string                        `json:"id"`
	Version      string                        `json:"version"`
	ConfigSchema JSONBytes                     `json:"config_schema"`
	Secrets      []extension.SecretRef         `json:"secrets,omitempty"`
	Components   []Component                   `json:"components"`
	Capabilities []Capability                  `json:"capabilities"`
	Edits        []extension.ConfigurationEdit `json:"configuration_edits,omitempty"`
	Exposed      []extension.ExposedMethod     `json:"exposed,omitempty"`
}

// Component is the wire form of one declaration component.
type Component struct {
	ID       string             `json:"id"`
	Lifetime extension.Lifetime `json:"lifetime"`
	Provides []string           `json:"provides"`
	Requires []Binding          `json:"requires,omitempty"`
}

// Binding is the wire form of one declared dependency binding.
type Binding struct {
	Name         string               `json:"name"`
	Provider     string               `json:"provider"`
	CapabilityID string               `json:"capability_id"`
	Definition   CapabilityDefinition `json:"definition"`
}

// CapabilityDefinition is the wire form of one exact structural contract.
type CapabilityDefinition struct {
	ContractID      string                  `json:"contract_id"`
	ContractVersion string                  `json:"contract_version"`
	Kind            extension.Kind          `json:"kind"`
	Methods         []Method                `json:"methods,omitempty"`
	Hooks           []extension.Hook        `json:"hooks,omitempty"`
	Strategy        *extension.StrategyDecl `json:"strategy,omitempty"`
}

// Capability is the wire form of one declared capability.
type Capability struct {
	ID              string                  `json:"id"`
	ContractID      string                  `json:"contract_id"`
	ContractVersion string                  `json:"contract_version"`
	Kind            extension.Kind          `json:"kind"`
	Methods         []Method                `json:"methods,omitempty"`
	Hooks           []extension.Hook        `json:"hooks,omitempty"`
	Strategy        *extension.StrategyDecl `json:"strategy,omitempty"`
}

// Method is the wire form of one declared method: every schema byte field is a
// JSONBytes value.
type Method struct {
	Name    string         `json:"name"`
	Mode    extension.Mode `json:"mode"`
	Input   JSONBytes      `json:"input"`
	Success JSONBytes      `json:"success"`
	Error   JSONBytes      `json:"error,omitempty"`
	Partial JSONBytes      `json:"partial,omitempty"`
}

// NewDeclaration converts one declaration into its wire form.
func NewDeclaration(d extension.Declaration) Declaration {
	out := Declaration{
		ID:           d.ID,
		Version:      d.Version,
		ConfigSchema: JSONBytes(d.ConfigSchema),
		Secrets:      append([]extension.SecretRef(nil), d.Secrets...),
		Components:   make([]Component, len(d.Components)),
		Capabilities: make([]Capability, len(d.Capabilities)),
		Edits:        append([]extension.ConfigurationEdit(nil), d.Edits...),
		Exposed:      append([]extension.ExposedMethod(nil), d.Exposed...),
	}
	for i, component := range d.Components {
		out.Components[i] = Component{
			ID:       component.ID,
			Lifetime: component.Lifetime,
			Provides: append([]string{}, component.Provides...),
			Requires: make([]Binding, len(component.Requires)),
		}
		for j, binding := range component.Requires {
			out.Components[i].Requires[j] = Binding{
				Name:         binding.Name,
				Provider:     binding.Provider,
				CapabilityID: binding.CapabilityID,
				Definition:   newDefinition(binding.Definition),
			}
		}
	}
	for i, capability := range d.Capabilities {
		out.Capabilities[i] = newCapability(capability)
	}
	return out
}

// Value converts the wire declaration into its typed form.
func (d Declaration) Value() extension.Declaration {
	out := extension.Declaration{
		ID:           d.ID,
		Version:      d.Version,
		ConfigSchema: append([]byte(nil), d.ConfigSchema...),
		Secrets:      append([]extension.SecretRef(nil), d.Secrets...),
		Components:   make([]extension.Component, len(d.Components)),
		Capabilities: make([]extension.Capability, len(d.Capabilities)),
		Edits:        append([]extension.ConfigurationEdit(nil), d.Edits...),
		Exposed:      append([]extension.ExposedMethod(nil), d.Exposed...),
	}
	for i, component := range d.Components {
		out.Components[i] = extension.Component{
			ID:       component.ID,
			Lifetime: component.Lifetime,
			Provides: append([]string{}, component.Provides...),
			Requires: make([]extension.Binding, len(component.Requires)),
		}
		for j, binding := range component.Requires {
			out.Components[i].Requires[j] = extension.Binding{
				Name:         binding.Name,
				Provider:     binding.Provider,
				CapabilityID: binding.CapabilityID,
				Definition:   binding.Definition.value(),
			}
		}
	}
	for i, capability := range d.Capabilities {
		out.Capabilities[i] = capability.value()
	}
	return out
}

func newDefinition(d extension.CapabilityDefinition) CapabilityDefinition {
	out := CapabilityDefinition{
		ContractID:      d.ContractID,
		ContractVersion: d.ContractVersion,
		Kind:            d.Kind,
		Methods:         newMethods(d.Methods),
		Hooks:           append([]extension.Hook(nil), d.Hooks...),
	}
	if d.Strategy != nil {
		strategy := *d.Strategy
		out.Strategy = &strategy
	}
	return out
}

func (d CapabilityDefinition) value() extension.CapabilityDefinition {
	out := extension.CapabilityDefinition{
		ContractID:      d.ContractID,
		ContractVersion: d.ContractVersion,
		Kind:            d.Kind,
		Methods:         methodsValue(d.Methods),
		Hooks:           append([]extension.Hook(nil), d.Hooks...),
	}
	if d.Strategy != nil {
		strategy := *d.Strategy
		out.Strategy = &strategy
	}
	return out
}

func newCapability(c extension.Capability) Capability {
	out := Capability{
		ID:              c.ID,
		ContractID:      c.ContractID,
		ContractVersion: c.ContractVersion,
		Kind:            c.Kind,
		Methods:         newMethods(c.Methods),
		Hooks:           append([]extension.Hook(nil), c.Hooks...),
	}
	if c.Strategy != nil {
		strategy := *c.Strategy
		out.Strategy = &strategy
	}
	return out
}

func (c Capability) value() extension.Capability {
	out := extension.Capability{
		ID:              c.ID,
		ContractID:      c.ContractID,
		ContractVersion: c.ContractVersion,
		Kind:            c.Kind,
		Methods:         methodsValue(c.Methods),
		Hooks:           append([]extension.Hook(nil), c.Hooks...),
	}
	if c.Strategy != nil {
		strategy := *c.Strategy
		out.Strategy = &strategy
	}
	return out
}

func newMethods(methods []extension.Method) []Method {
	if methods == nil {
		return nil
	}
	out := make([]Method, len(methods))
	for i, method := range methods {
		out[i] = Method{
			Name:    method.Name,
			Mode:    method.Mode,
			Input:   JSONBytes(method.Input),
			Success: JSONBytes(method.Success),
			Error:   JSONBytes(method.Error),
			Partial: JSONBytes(method.Partial),
		}
	}
	return out
}

func methodsValue(methods []Method) []extension.Method {
	if methods == nil {
		return nil
	}
	out := make([]extension.Method, len(methods))
	for i, method := range methods {
		out[i] = extension.Method{
			Name:    method.Name,
			Mode:    method.Mode,
			Input:   append([]byte(nil), method.Input...),
			Success: append([]byte(nil), method.Success...),
			Error:   append([]byte(nil), method.Error...),
			Partial: append([]byte(nil), method.Partial...),
		}
	}
	return out
}
