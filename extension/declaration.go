// Package extension defines the data contract of a native MELTA extension:
// one owned declaration, its compiled schemas and capability graph, the fixed
// typed contributions an implementation supplies, and the mediated call
// contexts through which declared bindings are reached.
package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// The fixed contract identities. Contract notation name/v1 is the pair
// contract_id=name, contract_version="1".
const (
	ContractTool             = "melta.tool"
	ContractHook             = "melta.hook"
	ContractContextStrategy  = "melta.context_strategy"
	ContractChildLaunch      = "melta.child_launch"
	ContractAgentDefinitions = "melta.agent_definitions"
	ContractPrompt           = "melta.prompt"
	ContractData             = "melta.data"
	ContractSessionCleanup   = "melta.session_cleanup"
	ContractCredentials      = "melta.credentials"
	ContractCommands         = "melta.commands"

	// ContractVersion is the one accepted version of every fixed contract.
	ContractVersion = "1"

	// HostProvider identifies the host as a binding provider. It is reserved:
	// no extension identity may use it.
	HostProvider = "host"
)

// Lifetime names one construction scope tier. Rank orders them from
// longest- to shortest-lived.
type Lifetime string

const (
	LifetimeHarness    Lifetime = "harness"
	LifetimeWorkspace  Lifetime = "workspace"
	LifetimeOperation  Lifetime = "operation"
	LifetimeInvocation Lifetime = "invocation"
)

// Valid reports whether the lifetime is one of the four declared scopes.
func (l Lifetime) Valid() bool {
	switch l {
	case LifetimeHarness, LifetimeWorkspace, LifetimeOperation, LifetimeInvocation:
		return true
	}
	return false
}

// Rank orders lifetimes from longest- to shortest-lived. An unknown lifetime
// ranks zero.
func (l Lifetime) Rank() int {
	switch l {
	case LifetimeHarness:
		return 1
	case LifetimeWorkspace:
		return 2
	case LifetimeOperation:
		return 3
	case LifetimeInvocation:
		return 4
	}
	return 0
}

// Kind is the closed capability kind discriminator.
type Kind string

const (
	KindService  Kind = "service"
	KindTool     Kind = "tool"
	KindHook     Kind = "hook"
	KindStrategy Kind = "strategy"
)

func (k Kind) valid() bool {
	switch k {
	case KindService, KindTool, KindHook, KindStrategy:
		return true
	}
	return false
}

// Mode is one declared method's or hook's semantic mode.
type Mode string

const (
	ModePure      Mode = "pure"
	ModeServiceIO Mode = "service_io"
)

func (m Mode) valid() bool { return m == ModePure || m == ModeServiceIO }

// HookBoundary is one closed hook boundary.
type HookBoundary string

const (
	BoundaryPrepare    HookBoundary = "prepare"
	BoundaryRequest    HookBoundary = "request"
	BoundaryBeforeTool HookBoundary = "before_tool"
	BoundaryAfterTool  HookBoundary = "after_tool"
)

func (b HookBoundary) valid() bool {
	switch b {
	case BoundaryPrepare, BoundaryRequest, BoundaryBeforeTool, BoundaryAfterTool:
		return true
	}
	return false
}

// SecretLifetime names when one declared credential reference resolves.
type SecretLifetime string

const (
	SecretActivation SecretLifetime = "activation"
	SecretOperation  SecretLifetime = "operation"
)

// SecretRef declares one JSON-pointer field in the active section whose value
// names a credential. Metadata is inspected without executing the extension.
type SecretRef struct {
	Field    string         `json:"field"`
	Lifetime SecretLifetime `json:"lifetime"`
}

// Declaration is the owned data contract of one extension implementation.
type Declaration struct {
	ID           string              `json:"id"`
	Version      string              `json:"version"`
	ConfigSchema json.RawMessage     `json:"config_schema"`
	Secrets      []SecretRef         `json:"secrets,omitempty"`
	Components   []Component         `json:"components"`
	Capabilities []Capability        `json:"capabilities"`
	Edits        []ConfigurationEdit `json:"configuration_edits,omitempty"`
}

// Component declares one independent implementation instance at one lifetime.
type Component struct {
	ID       string    `json:"id"`
	Lifetime Lifetime  `json:"lifetime"`
	Provides []string  `json:"provides"`
	Requires []Binding `json:"requires,omitempty"`
}

// Capability declares one contributed contract instance.
type Capability struct {
	ID              string          `json:"id"`
	ContractID      string          `json:"contract_id"`
	ContractVersion string          `json:"contract_version"`
	Kind            Kind            `json:"kind"`
	Methods         []Method        `json:"methods,omitempty"`
	Hooks           []Hook          `json:"hooks,omitempty"`
	Strategy        *StrategyDecl   `json:"strategy,omitempty"`
	Exposed         []ExposedMethod `json:"exposed,omitempty"`
}

// Method declares one named-service method and its closed input/result domain.
// A present Error schema declares failure data, a present Partial schema
// declares an optional partial result.
type Method struct {
	Name    string          `json:"name"`
	Mode    Mode            `json:"mode"`
	Input   json.RawMessage `json:"input"`
	Success json.RawMessage `json:"success"`
	Error   json.RawMessage `json:"error,omitempty"`
	Partial json.RawMessage `json:"partial,omitempty"`
}

// HookMode is one hook contribution's semantic mode. It is separate from the
// named-service Method.Mode domain.
type HookMode string

const (
	HookModePure      HookMode = "pure"
	HookModeEffectful HookMode = "effectful"
)

func (m HookMode) valid() bool { return m == HookModePure || m == HookModeEffectful }

// Hook declares one hook boundary and its semantic mode.
type Hook struct {
	Boundary HookBoundary `json:"boundary"`
	Mode     HookMode     `json:"mode"`
}

// StrategyDecl declares the context strategy's pre-admission metadata.
type StrategyDecl struct {
	RequiresContextWindow bool `json:"requires_context_window"`
}

// ExposedMethod declares one named-service MethodKey's public invocation
// shape. It is owned data now; the endpoint lands with its consumer.
type ExposedMethod struct {
	Method string `json:"method"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// Binding declares one local dependency name and the exact contract the
// consumer expects from its provider.
type Binding struct {
	Name         string               `json:"name"`
	Provider     string               `json:"provider"`
	CapabilityID string               `json:"capability_id"`
	Definition   CapabilityDefinition `json:"definition"`
}

// CapabilityDefinition is the exact structural contract shared by a provider
// and every consumer: the provider's capability declaration minus its opaque
// identity and exposure.
type CapabilityDefinition struct {
	ContractID      string        `json:"contract_id"`
	ContractVersion string        `json:"contract_version"`
	Kind            Kind          `json:"kind"`
	Methods         []Method      `json:"methods,omitempty"`
	Hooks           []Hook        `json:"hooks,omitempty"`
	Strategy        *StrategyDecl `json:"strategy,omitempty"`
}

// ConfigurationEdit declares one edit ID and the own Harness-scoped pure
// service method that transforms the latest owning section.
type ConfigurationEdit struct {
	ID     string    `json:"id"`
	Method MethodKey `json:"method"`
}

// MethodKey addresses one named-service method.
type MethodKey struct {
	CapabilityID string `json:"capability_id"`
	Method       string `json:"method"`
}

// Definition returns the exact contract a capability declares.
func (c Capability) Definition() CapabilityDefinition {
	return c.definition()
}

// definition returns the exact contract a capability declares.
func (c Capability) definition() CapabilityDefinition {
	return CapabilityDefinition{
		ContractID:      c.ContractID,
		ContractVersion: c.ContractVersion,
		Kind:            c.Kind,
		Methods:         c.Methods,
		Hooks:           c.Hooks,
		Strategy:        c.Strategy,
	}
}

// Owned returns a deep copy of one Native registration: its declaration (all
// nested slices and raw byte buffers) and its Describe map container. The
// callbacks and Open function values are immutable and are copied as-is.
func (n Native) Owned() Native {
	out := n
	out.Declaration = n.Declaration.owned()
	if n.Describe != nil {
		describe := make(map[string]ToolDescribe, len(n.Describe))
		for id, callback := range n.Describe {
			describe[id] = callback
		}
		out.Describe = describe
	}
	return out
}

// owned returns a deep copy of the declaration, so no caller slice or byte
// buffer remains reachable through the compiled graph.
func (d Declaration) owned() Declaration {
	out := d
	out.ConfigSchema = bytes.Clone(d.ConfigSchema)
	out.Secrets = append([]SecretRef(nil), d.Secrets...)
	out.Components = make([]Component, len(d.Components))
	for i, component := range d.Components {
		owned := component
		owned.Provides = append([]string(nil), component.Provides...)
		owned.Requires = make([]Binding, len(component.Requires))
		for j, binding := range component.Requires {
			owned.Requires[j] = binding.owned()
		}
		out.Components[i] = owned
	}
	out.Capabilities = make([]Capability, len(d.Capabilities))
	for i, capability := range d.Capabilities {
		owned := capability
		owned.Methods = make([]Method, len(capability.Methods))
		for j, method := range capability.Methods {
			owned.Methods[j] = method.owned()
		}
		owned.Hooks = append([]Hook(nil), capability.Hooks...)
		if capability.Strategy != nil {
			strategy := *capability.Strategy
			owned.Strategy = &strategy
		}
		owned.Exposed = append([]ExposedMethod(nil), capability.Exposed...)
		out.Capabilities[i] = owned
	}
	out.Edits = append([]ConfigurationEdit(nil), d.Edits...)
	return out
}

func (b Binding) owned() Binding {
	out := b
	if b.Definition.Methods != nil {
		out.Definition.Methods = make([]Method, len(b.Definition.Methods))
		for i, method := range b.Definition.Methods {
			out.Definition.Methods[i] = method.owned()
		}
	}
	out.Definition.Hooks = append([]Hook(nil), b.Definition.Hooks...)
	if b.Definition.Strategy != nil {
		strategy := *b.Definition.Strategy
		out.Definition.Strategy = &strategy
	}
	return out
}

func (m Method) owned() Method {
	out := m
	out.Input = bytes.Clone(m.Input)
	out.Success = bytes.Clone(m.Success)
	out.Error = bytes.Clone(m.Error)
	out.Partial = bytes.Clone(m.Partial)
	return out
}

// contractName renders the fixed notation name/v1 for diagnostics.
func contractName(id, version string) string {
	return id + "/v" + version
}

// hostCapability returns the host-supplied definition of one fixed named
// service. Only services the shipped host exposes are declared here.
func hostCapability(id string) (CapabilityDefinition, bool) {
	switch id {
	case ContractPrompt:
		return hostPromptDefinition(), true
	case ContractChildLaunch:
		return hostChildLaunchDefinition(), true
	}
	return CapabilityDefinition{}, false
}

// HostCapability returns the exact host-supplied capability definition a
// declaration must structurally match to bind that host service.
func HostCapability(id string) (CapabilityDefinition, bool) {
	return hostCapability(id)
}

// decodeJSON decodes one JSON value losslessly: numbers keep their lexemes and
// object member order is immaterial. An empty value is invalid.
func decodeJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("missing JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, fmt.Errorf("trailing JSON input: %w", err)
	}
	return value, nil
}
