package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInvalid marks every rejected declaration, graph, schema, binding or
// instance shape.
var ErrInvalid = errors.New("extension: invalid declaration")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// methodContract is one compiled method declaration.
type methodContract struct {
	Method
	input   *jsonschema.Schema
	success *jsonschema.Schema
	failure *jsonschema.Schema
	partial *jsonschema.Schema
}

// Compiled is the one owned compilation result: declarations, compiled
// schemas, resolved exports and per-lifetime construction order.
type Compiled struct {
	declarations []*CompiledDeclaration
	byID         map[string]*CompiledDeclaration
	capabilities map[string]*CompiledCapability
	byLifetime   map[Lifetime][]*CompiledComponent
	natives      map[string]Native
}

// CompiledDeclaration is one declaration's owned compiled form.
type CompiledDeclaration struct {
	Declaration  Declaration
	config       *jsonschema.Schema
	Capabilities map[string]*CompiledCapability
	Components   map[string]*CompiledComponent
	Exposed      map[MethodKey]ExposedMethod
}

// MethodSet is one compiled method declaration set. It is the common call
// boundary for nested declared calls and host-initiated provider calls, so
// both validate through the same compiled schemas.
type MethodSet map[string]*methodContract

// CompiledCapability is one resolved exported capability.
type CompiledCapability struct {
	Declaration *CompiledDeclaration
	Capability
	Component *CompiledComponent
	Methods   MethodSet
}

// CompiledComponent is one component in the resolved construction graph.
type CompiledComponent struct {
	Declaration *CompiledDeclaration
	Component
	Provides []*CompiledCapability
	Requires []*CompiledBinding

	// bindingMethods holds each declaration-stage binding definition's
	// compiled method set, keyed by the local binding name.
	bindingMethods map[string]MethodSet
}

// CompiledBinding is one resolved local dependency name.
type CompiledBinding struct {
	Binding
	Host    bool
	Target  *CompiledCapability
	Methods MethodSet
}

// Compile validates every native declaration, compiles each local schema once,
// resolves the global export table and binding graph, rejects identity,
// contract, lifetime and cycle violations, and returns the owned construction
// plan. Native callbacks are validated against the declaration in this same
// pass.
func Compile(natives []Native) (*Compiled, error) {
	c := &Compiled{
		byID:         make(map[string]*CompiledDeclaration, len(natives)),
		capabilities: make(map[string]*CompiledCapability),
		byLifetime:   make(map[Lifetime][]*CompiledComponent),
		natives:      make(map[string]Native, len(natives)),
	}
	for i, native := range natives {
		native = native.Owned()
		if err := c.addDeclaration(native.Declaration, i); err != nil {
			return nil, err
		}
		c.natives[native.Declaration.ID] = native
	}
	if err := c.resolveBindings(); err != nil {
		return nil, err
	}
	if err := c.validateContracts(); err != nil {
		return nil, err
	}
	if err := c.orderConstruction(); err != nil {
		return nil, err
	}
	for id, native := range c.natives {
		if err := c.validateNative(id, native); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Compiled) addDeclaration(declaration Declaration, index int) error {
	if declaration.ID == "" {
		return invalid("extension %d: empty extension ID", index)
	}
	if declaration.ID == HostProvider {
		return invalid("extension %q: reserved host identity", declaration.ID)
	}
	if declaration.Version == "" {
		return invalid("extension %q: empty implementation version", declaration.ID)
	}
	if _, duplicate := c.byID[declaration.ID]; duplicate {
		return invalid("extension %q: duplicate extension ID", declaration.ID)
	}
	config, err := compileSchema(declaration.ConfigSchema, declaration.ID+"/config")
	if err != nil {
		return invalid("extension %q: config schema: %v", declaration.ID, err)
	}
	compiled := &CompiledDeclaration{
		Declaration:  declaration,
		config:       config,
		Capabilities: make(map[string]*CompiledCapability, len(declaration.Capabilities)),
		Components:   make(map[string]*CompiledComponent, len(declaration.Components)),
		Exposed:      make(map[MethodKey]ExposedMethod, len(declaration.Exposed)),
	}
	for _, secret := range declaration.Secrets {
		if !ValidSecretField(secret.Field) {
			return invalid("extension %q: secret field %q is not a JSON pointer", declaration.ID, secret.Field)
		}
		if secret.Lifetime != SecretActivation && secret.Lifetime != SecretOperation {
			return invalid("extension %q: secret %q has invalid lifetime %q", declaration.ID, secret.Field, secret.Lifetime)
		}
	}
	seenSecrets := make(map[string]bool, len(declaration.Secrets))
	for _, secret := range declaration.Secrets {
		if seenSecrets[secret.Field] {
			return invalid("extension %q: duplicate secret field %q", declaration.ID, secret.Field)
		}
		seenSecrets[secret.Field] = true
	}
	cleanupCapabilities := 0
	for _, capability := range declaration.Capabilities {
		if capability.ID == "" {
			return invalid("extension %q: empty capability ID", declaration.ID)
		}
		if _, duplicate := c.capabilities[capability.ID]; duplicate {
			return invalid("capability %q: duplicate provider", capability.ID)
		}
		if capability.ContractID == ContractSessionCleanup {
			cleanupCapabilities++
			if cleanupCapabilities > 1 {
				return invalid("extension %q: declares more than one %s capability", declaration.ID, ContractSessionCleanup)
			}
		}
		resolved, err := c.compileCapability(compiled, capability)
		if err != nil {
			return err
		}
		compiled.Capabilities[capability.ID] = resolved
		c.capabilities[capability.ID] = resolved
	}
	provided := make(map[string]*CompiledComponent, len(declaration.Capabilities))
	for _, component := range declaration.Components {
		if component.ID == "" {
			return invalid("extension %q: empty component ID", declaration.ID)
		}
		if component.ID == HostProvider {
			return invalid("extension %q: reserved component identity %q", declaration.ID, component.ID)
		}
		if !component.Lifetime.Valid() {
			return invalid("extension %q component %q: invalid lifetime %q", declaration.ID, component.ID, component.Lifetime)
		}
		if _, duplicate := compiled.Components[component.ID]; duplicate {
			return invalid("extension %q: duplicate component %q", declaration.ID, component.ID)
		}
		resolved := &CompiledComponent{Declaration: compiled, Component: component}
		seenProvides := make(map[string]bool, len(component.Provides))
		for _, id := range component.Provides {
			capability, ok := compiled.Capabilities[id]
			if !ok {
				return invalid("extension %q component %q: provides undeclared capability %q", declaration.ID, component.ID, id)
			}
			if seenProvides[id] {
				return invalid("extension %q component %q: duplicate provided capability %q", declaration.ID, component.ID, id)
			}
			seenProvides[id] = true
			if previous, taken := provided[id]; taken {
				return invalid("capability %q: provided by both %q and %q", id, previous.ID, component.ID)
			}
			provided[id] = resolved
			capability.Component = resolved
			resolved.Provides = append(resolved.Provides, capability)
		}
		if err := validateComponentRequires(compiled, resolved); err != nil {
			return err
		}
		compiled.Components[component.ID] = resolved
		c.byLifetime[component.Lifetime] = append(c.byLifetime[component.Lifetime], resolved)
	}
	for id, capability := range compiled.Capabilities {
		if provided[id] == nil {
			return invalid("extension %q capability %q: not provided by any component", declaration.ID, id)
		}
		switch {
		case capability.Kind == KindHook && hasBoundary(capability.Hooks, BoundaryPrepare) && capability.Component.Lifetime.Rank() > LifetimeWorkspace.Rank():
			return invalid("capability %q: preparation hooks require a Harness or Workspace lifetime", id)
		case capability.Kind == KindStrategy && capability.Component.Lifetime.Rank() > LifetimeOperation.Rank():
			return invalid("capability %q: context strategies require an Operation-or-longer lifetime", id)
		case capability.Kind == KindService && capability.ContractID == ContractAgentDefinitions && capability.Component.Lifetime != LifetimeHarness:
			return invalid("capability %q: agent definition resolvers require a Harness lifetime", id)
		case capability.Kind == KindService && capability.ContractID == ContractSessionCleanup && capability.Component.Lifetime != LifetimeHarness:
			return invalid("capability %q: session cleanup services require a Harness lifetime", id)
		}
	}
	if err := validateEdits(compiled); err != nil {
		return err
	}
	if err := validateExposures(compiled); err != nil {
		return err
	}
	c.declarations = append(c.declarations, compiled)
	c.byID[declaration.ID] = compiled
	return nil
}

func (c *Compiled) compileCapability(declaration *CompiledDeclaration, capability Capability) (*CompiledCapability, error) {
	if !capability.Kind.valid() {
		return nil, invalid("extension %q capability %q: invalid kind %q", declaration.Declaration.ID, capability.ID, capability.Kind)
	}
	if capability.ContractID == "" || capability.ContractVersion == "" {
		return nil, invalid("capability %q: incomplete contract identity", capability.ID)
	}
	if err := validateFixedContractIdentity(capability); err != nil {
		return nil, err
	}
	if duplicateMethodNames(capability.Methods) {
		return nil, invalid("capability %q: duplicate method name", capability.ID)
	}
	resolved := &CompiledCapability{Declaration: declaration, Capability: capability, Methods: make(MethodSet, len(capability.Methods))}
	for i, method := range capability.Methods {
		if method.Name == "" {
			return nil, invalid("capability %q: empty method name", capability.ID)
		}
		if !method.Mode.valid() {
			return nil, invalid("capability %q method %q: invalid mode %q", capability.ID, method.Name, method.Mode)
		}
		compiled, err := compileMethod(capability.ID, method, i)
		if err != nil {
			return nil, err
		}
		resolved.Methods[method.Name] = compiled
	}
	if err := validateCapabilityShape(declaration, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// validateFixedContractIdentity enforces the supplied fixed contract
// identities in both directions: a known fixed ID carries exactly its v1
// kind/method definition, and a fixed tool/hook/strategy kind cannot be
// reclassified under an author-defined named-service contract. Author-defined
// named-service versions stay opaque and exactly matched.
func validateFixedContractIdentity(capability Capability) error {
	switch capability.ContractID {
	case ContractTool:
		if capability.ContractVersion != ContractVersion || capability.Kind != KindTool || !methodsStructurallyEqual(capability.Methods, toolMethods()) {
			return invalid("capability %q: fixed contract %s requires version %s, kind %s and its exact method definitions", capability.ID, ContractTool, ContractVersion, KindTool)
		}
		return nil
	case ContractHook:
		if capability.ContractVersion != ContractVersion || capability.Kind != KindHook || !methodsStructurallyEqual(capability.Methods, hookMethods(capability.Hooks)) {
			return invalid("capability %q: fixed contract %s requires version %s, kind %s and its exact boundary method definitions", capability.ID, ContractHook, ContractVersion, KindHook)
		}
		return nil
	case ContractContextStrategy:
		if capability.ContractVersion != ContractVersion || capability.Kind != KindStrategy || !methodsStructurallyEqual(capability.Methods, strategyMethods()) {
			return invalid("capability %q: fixed contract %s requires version %s, kind %s and its exact method definitions", capability.ID, ContractContextStrategy, ContractVersion, KindStrategy)
		}
		return nil
	case ContractAgentDefinitions:
		if capability.ContractVersion != ContractVersion || capability.Kind != KindService || !methodsStructurallyEqual(capability.Methods, agentDefinitionMethods()) {
			return invalid("capability %q: fixed contract %s requires version %s, kind %s and its exact method definitions", capability.ID, ContractAgentDefinitions, ContractVersion, KindService)
		}
		return nil
	case ContractPrompt, ContractChildLaunch, ContractData, ContractCredentials, ContractCommands:
		definition, ok := hostCapability(capability.ContractID)
		if !ok {
			return invalid("capability %q: fixed host contract %s has no definition", capability.ID, capability.ContractID)
		}
		if capability.ContractVersion != ContractVersion || capability.Kind != definition.Kind || !methodsStructurallyEqual(capability.Methods, definition.Methods) {
			return invalid("capability %q: fixed host contract %s requires version %s, kind %s and its exact method definitions", capability.ID, capability.ContractID, ContractVersion, definition.Kind)
		}
		return nil
	case ContractSessionCleanup:
		if capability.ContractVersion != ContractVersion || capability.Kind != KindService || !methodsStructurallyEqual(capability.Methods, cleanupMethods()) {
			return invalid("capability %q: fixed contract %s requires version %s, kind %s and its exact method definitions", capability.ID, ContractSessionCleanup, ContractVersion, KindService)
		}
		return nil
	default:
		switch capability.Kind {
		case KindTool, KindHook, KindStrategy:
			return invalid("capability %q: %s kind requires its supplied fixed contract identity", capability.ID, capability.Kind)
		}
		return nil
	}
}

func duplicateMethodNames(methods []Method) bool {
	seen := make(map[string]bool, len(methods))
	for _, method := range methods {
		if seen[method.Name] {
			return true
		}
		seen[method.Name] = true
	}
	return false
}

func compileMethod(capabilityID string, method Method, index int) (*methodContract, error) {
	label := fmt.Sprintf("%s/%s", capabilityID, method.Name)
	input, err := compileSchema(method.Input, label+"/input")
	if err != nil {
		return nil, invalid("capability %q method %q input schema: %v", capabilityID, method.Name, err)
	}
	success, err := compileSchema(method.Success, label+"/success")
	if err != nil {
		return nil, invalid("capability %q method %q success schema: %v", capabilityID, method.Name, err)
	}
	var failure, partial *jsonschema.Schema
	if len(method.Error) != 0 {
		failure, err = compileSchema(method.Error, label+"/error")
		if err != nil {
			return nil, invalid("capability %q method %q error schema: %v", capabilityID, method.Name, err)
		}
	}
	if len(method.Partial) != 0 {
		partial, err = compileSchema(method.Partial, label+"/partial")
		if err != nil {
			return nil, invalid("capability %q method %q partial schema: %v", capabilityID, method.Name, err)
		}
	}
	return &methodContract{Method: method, input: input, success: success, failure: failure, partial: partial}, nil
}

func validateCapabilityShape(declaration *CompiledDeclaration, capability *CompiledCapability) error {
	switch capability.Kind {
	case KindTool:
		if len(capability.Hooks) != 0 || capability.Strategy != nil {
			return invalid("capability %q: tool carries hook or strategy metadata", capability.ID)
		}
	case KindHook:
		if len(capability.Hooks) == 0 {
			return invalid("capability %q: hook kind requires hook metadata", capability.ID)
		}
		if capability.Strategy != nil {
			return invalid("capability %q: hook carries strategy metadata", capability.ID)
		}
		seen := make(map[HookBoundary]bool, len(capability.Hooks))
		for _, hook := range capability.Hooks {
			if !hook.Boundary.valid() {
				return invalid("capability %q: invalid hook boundary %q", capability.ID, hook.Boundary)
			}
			if !hook.Mode.valid() {
				return invalid("capability %q: invalid hook mode %q", capability.ID, hook.Mode)
			}
			if seen[hook.Boundary] {
				return invalid("capability %q: duplicate hook boundary %q", capability.ID, hook.Boundary)
			}
			seen[hook.Boundary] = true
			if (hook.Boundary == BoundaryPrepare || hook.Boundary == BoundaryAfterTool) && hook.Mode != HookModePure {
				return invalid("capability %q: %s hooks must be pure", capability.ID, hook.Boundary)
			}
		}
	case KindStrategy:
		if capability.Strategy == nil {
			return invalid("capability %q: strategy kind requires strategy metadata", capability.ID)
		}
		if len(capability.Hooks) != 0 {
			return invalid("capability %q: strategy carries hook metadata", capability.ID)
		}
	case KindService:
		if len(capability.Hooks) != 0 || capability.Strategy != nil {
			return invalid("capability %q: service carries hook or strategy metadata", capability.ID)
		}
		if len(capability.Methods) == 0 {
			return invalid("capability %q: service declares no methods", capability.ID)
		}
	}
	return nil
}

func validateComponentRequires(declaration *CompiledDeclaration, component *CompiledComponent) error {
	component.bindingMethods = make(map[string]MethodSet, len(component.Component.Requires))
	seen := make(map[string]bool, len(component.Component.Requires))
	for _, binding := range component.Component.Requires {
		if binding.Name == "" {
			return invalid("extension %q component %q: empty binding name", declaration.Declaration.ID, component.ID)
		}
		if seen[binding.Name] {
			return invalid("extension %q component %q: duplicate binding %q", declaration.Declaration.ID, component.ID, binding.Name)
		}
		seen[binding.Name] = true
		if binding.CapabilityID == "" {
			return invalid("extension %q component %q binding %q: empty capability ID", declaration.Declaration.ID, component.ID, binding.Name)
		}
		if binding.Definition.ContractID == "" || binding.Definition.ContractVersion == "" {
			return invalid("extension %q component %q binding %q: incomplete expected contract", declaration.Declaration.ID, component.ID, binding.Name)
		}
		if !sameMethodsFromDefinition(binding.Definition) {
			return invalid("extension %q component %q binding %q: method definitions are inconsistent", declaration.Declaration.ID, component.ID, binding.Name)
		}
		methods, err := compileDefinitionMethods(binding.CapabilityID, binding.Definition)
		if err != nil {
			return err
		}
		component.bindingMethods[binding.Name] = methods
	}
	return nil
}

// sameMethodsFromDefinition rejects duplicate names in the expected method
// definition; schema compilation and exact provider equality happen later.
func sameMethodsFromDefinition(definition CapabilityDefinition) bool {
	return !duplicateMethodNames(definition.Methods)
}

func (c *Compiled) resolveBindings() error {
	for _, compiled := range c.declarations {
		for _, component := range compiled.Components {
			for _, binding := range component.Component.Requires {
				resolved, err := c.resolveBinding(compiled, component, binding)
				if err != nil {
					return err
				}
				component.Requires = append(component.Requires, resolved)
			}
		}
	}
	return nil
}

func (c *Compiled) resolveBinding(declaration *CompiledDeclaration, component *CompiledComponent, binding Binding) (*CompiledBinding, error) {
	resolved := &CompiledBinding{Binding: binding.owned()}
	if binding.Provider == HostProvider {
		definition, ok := hostCapability(binding.CapabilityID)
		if !ok {
			return nil, invalid("extension %q component %q binding %q: unknown host service %q", declaration.Declaration.ID, component.ID, binding.Name, binding.CapabilityID)
		}
		if !definitionStructurallyEqual(definition, binding.Definition) {
			return nil, invalid("extension %q component %q binding %q: expected host contract differs from the supplied %s", declaration.Declaration.ID, component.ID, binding.Name, contractName(definition.ContractID, definition.ContractVersion))
		}
		resolved.Host = true
		resolved.Methods = component.bindingMethods[binding.Name]
		return resolved, nil
	}
	target, ok := c.capabilities[binding.CapabilityID]
	if !ok {
		return nil, invalid("extension %q component %q binding %q: unknown capability %q", declaration.Declaration.ID, component.ID, binding.Name, binding.CapabilityID)
	}
	if target.Kind != KindService {
		return nil, invalid("extension %q component %q binding %q: capability %q is not a named service", declaration.Declaration.ID, component.ID, binding.Name, binding.CapabilityID)
	}
	providerLabel := target.Component.Declaration.Declaration.ID + "/" + target.Component.ID
	if binding.Provider != providerLabel {
		return nil, invalid("extension %q component %q binding %q: provider %q does not identify the provider %q of capability %q", declaration.Declaration.ID, component.ID, binding.Name, binding.Provider, providerLabel, binding.CapabilityID)
	}
	if !definitionStructurallyEqual(target.definition(), binding.Definition) {
		return nil, invalid("extension %q component %q binding %q: expected contract differs from provider %q", declaration.Declaration.ID, component.ID, binding.Name, binding.CapabilityID)
	}
	if target.Component.Lifetime.Rank() > component.Lifetime.Rank() {
		return nil, invalid("extension %q component %q (%s) requires %q from shorter-lived %s provider %q", declaration.Declaration.ID, component.ID, component.Lifetime, binding.Name, target.Component.Lifetime, binding.Provider)
	}
	resolved.Target = target
	resolved.Methods = target.Methods
	return resolved, nil
}

func compileDefinitionMethods(capabilityID string, definition CapabilityDefinition) (MethodSet, error) {
	methods := make(MethodSet, len(definition.Methods))
	for i, method := range definition.Methods {
		compiled, err := compileMethod(capabilityID, method, i)
		if err != nil {
			return nil, err
		}
		methods[method.Name] = compiled
	}
	return methods, nil
}

func (c *Compiled) validateContracts() error {
	seen := make(map[contractIdentity]CapabilityDefinition)
	for _, declaration := range c.declarations {
		for _, capability := range declaration.Capabilities {
			key := contractIdentity{id: capability.ContractID, version: capability.ContractVersion}
			definition := capability.definition()
			if previous, exists := seen[key]; exists {
				if !contractStructurallyEqual(previous, definition) {
					return invalid("contract %s: incompatible definitions reused", contractName(key.id, key.version))
				}
				continue
			}
			seen[key] = definition
		}
	}
	// A host definition reused with a different shape is incompatible with
	// every bound consumer even when an individual call might pass.
	for _, declaration := range c.declarations {
		for _, component := range declaration.Components {
			for _, binding := range component.Component.Requires {
				if binding.Provider != HostProvider {
					continue
				}
				definition, ok := hostCapability(binding.CapabilityID)
				if !ok {
					continue
				}
				key := contractIdentity{id: definition.ContractID, version: definition.ContractVersion}
				if previous, exists := seen[key]; exists && !contractStructurallyEqual(previous, definition) {
					return invalid("contract %s: incompatible host definition reused", contractName(key.id, key.version))
				}
				seen[key] = definition
			}
		}
	}
	return nil
}

func (c *Compiled) orderConstruction() error {
	for _, lifetime := range []Lifetime{LifetimeHarness, LifetimeWorkspace, LifetimeOperation, LifetimeInvocation} {
		components := c.byLifetime[lifetime]
		if len(components) == 0 {
			continue
		}
		index := make(map[*CompiledComponent]int, len(components))
		for i, component := range components {
			index[component] = i
		}
		consumedBy := make([][]int, len(components))
		indegree := make([]int, len(components))
		edges := make([]map[int]bool, len(components))
		for i := range edges {
			edges[i] = make(map[int]bool)
		}
		for i, component := range components {
			for _, binding := range component.Requires {
				if binding.Host || binding.Target == nil || binding.Target.Component.Lifetime != lifetime {
					continue
				}
				provider, ok := index[binding.Target.Component]
				if !ok {
					continue
				}
				if edges[i][provider] {
					continue
				}
				edges[i][provider] = true
				consumedBy[provider] = append(consumedBy[provider], i)
				indegree[i]++
			}
		}
		ordered := make([]*CompiledComponent, 0, len(components))
		emitted := make([]bool, len(components))
		for len(ordered) < len(components) {
			next := -1
			for i := range components {
				if !emitted[i] && indegree[i] == 0 {
					next = i
					break
				}
			}
			if next < 0 {
				return invalid("dependency cycle among %s components", lifetime)
			}
			emitted[next] = true
			ordered = append(ordered, components[next])
			for _, consumer := range consumedBy[next] {
				indegree[consumer]--
			}
		}
		c.byLifetime[lifetime] = ordered
	}
	return nil
}

func validateEdits(declaration *CompiledDeclaration) error {
	seen := make(map[string]bool, len(declaration.Declaration.Edits))
	for _, edit := range declaration.Declaration.Edits {
		if edit.ID == "" {
			return invalid("extension %q: empty configuration edit ID", declaration.Declaration.ID)
		}
		if seen[edit.ID] {
			return invalid("extension %q: duplicate configuration edit %q", declaration.Declaration.ID, edit.ID)
		}
		seen[edit.ID] = true
		capability, ok := declaration.Capabilities[edit.Method.CapabilityID]
		if !ok {
			return invalid("extension %q edit %q: unknown capability %q", declaration.Declaration.ID, edit.ID, edit.Method.CapabilityID)
		}
		if capability.Kind != KindService {
			return invalid("extension %q edit %q: capability %q is not a service", declaration.Declaration.ID, edit.ID, edit.Method.CapabilityID)
		}
		method, ok := capability.Methods[edit.Method.Method]
		if !ok {
			return invalid("extension %q edit %q: unknown method %q", declaration.Declaration.ID, edit.ID, edit.Method.Method)
		}
		if capability.Component.Lifetime != LifetimeHarness {
			return invalid("extension %q edit %q: capability %q is not Harness-scoped", declaration.Declaration.ID, edit.ID, edit.Method.CapabilityID)
		}
		if method.Mode != ModePure {
			return invalid("extension %q edit %q: method %q is not pure", declaration.Declaration.ID, edit.ID, edit.Method.Method)
		}
	}
	return nil
}

// validateExposures resolves each publication entry against the declaration's
// own existing service methods. Only Harness- and Workspace-scoped components
// can be invoked from outside; Operation- and invocation-scoped methods stay
// internal calls under genuine admitted context.
func validateExposures(declaration *CompiledDeclaration) error {
	for _, exposed := range declaration.Declaration.Exposed {
		if !exposed.Kind.valid() {
			return invalid("extension %q exposure %s/%s: invalid kind %q", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method, exposed.Kind)
		}
		if !exposed.Target.valid() {
			return invalid("extension %q exposure %s/%s: invalid target %q", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method, exposed.Target)
		}
		capability, ok := declaration.Capabilities[exposed.Method.CapabilityID]
		if !ok {
			return invalid("extension %q exposure: unknown capability %q", declaration.Declaration.ID, exposed.Method.CapabilityID)
		}
		if capability.Kind != KindService {
			return invalid("extension %q exposure %s/%s: capability is not a service", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method)
		}
		if _, ok := capability.Methods[exposed.Method.Method]; !ok {
			return invalid("extension %q exposure %s/%s: unknown method", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method)
		}
		if capability.Component.Lifetime != LifetimeHarness && capability.Component.Lifetime != LifetimeWorkspace {
			return invalid("extension %q exposure %s/%s: %s capability cannot be exposed", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method, capability.Component.Lifetime)
		}
		if capability.Component.Lifetime == LifetimeWorkspace && exposed.Target == ExposedTargetHarness {
			return invalid("extension %q exposure %s/%s: Workspace-scoped capability cannot expose the harness target", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method)
		}
		if _, duplicate := declaration.Exposed[exposed.Method]; duplicate {
			return invalid("extension %q exposure %s/%s: duplicate", declaration.Declaration.ID, exposed.Method.CapabilityID, exposed.Method.Method)
		}
		declaration.Exposed[exposed.Method] = exposed
	}
	return nil
}

func (c *Compiled) validateNative(id string, native Native) error {
	if native.Open == nil {
		return invalid("extension %q: missing Open", id)
	}
	compiled := c.byID[id]
	tools := make(map[string]bool)
	for _, capability := range compiled.Capabilities {
		if capability.Kind == KindTool {
			tools[capability.ID] = true
		}
	}
	for capabilityID, describe := range native.Describe {
		if !tools[capabilityID] {
			return invalid("extension %q: describe callback for undeclared tool %q", id, capabilityID)
		}
		if describe == nil {
			return invalid("extension %q tool %q: nil describe callback", id, capabilityID)
		}
		delete(tools, capabilityID)
	}
	for capabilityID := range tools {
		return invalid("extension %q tool %q: missing describe callback", id, capabilityID)
	}
	return nil
}

// Native returns the registered native implementation of one extension.
func (c *Compiled) Native(id string) (Native, bool) {
	native, ok := c.natives[id]
	return native, ok
}

// Declaration returns one compiled declaration.
func (c *Compiled) Declaration(id string) (*CompiledDeclaration, bool) {
	declaration, ok := c.byID[id]
	return declaration, ok
}

// Declarations returns compiled declarations in registration order.
func (c *Compiled) Declarations() []*CompiledDeclaration { return c.declarations }

// Capability resolves one exported capability.
func (c *Compiled) Capability(id string) (*CompiledCapability, bool) {
	capability, ok := c.capabilities[id]
	return capability, ok
}

// CleanupCapability resolves one installed extension's single fixed session
// cleanup capability. An extension with no such declaration reports false.
func (c *Compiled) CleanupCapability(extensionID string) (*CompiledCapability, bool) {
	declaration, ok := c.byID[extensionID]
	if !ok {
		return nil, false
	}
	for _, capability := range declaration.Capabilities {
		if capability.ContractID == ContractSessionCleanup {
			return capability, true
		}
	}
	return nil, false
}

// Components returns one lifetime's construction order.
func (c *Compiled) Components(lifetime Lifetime) []*CompiledComponent {
	return c.byLifetime[lifetime]
}

// ValidateSection validates one active non-secret section against its local
// schema.
func (c *Compiled) ValidateSection(id string, section json.RawMessage) error {
	declaration, ok := c.byID[id]
	if !ok {
		return invalid("extension %q is not installed", id)
	}
	value, err := decodeJSON(section)
	if err != nil {
		return invalid("extension %q section: %v", id, err)
	}
	if err := declaration.config.Validate(value); err != nil {
		return invalid("extension %q section: %v", id, err)
	}
	return nil
}

// CompileNative compiles one native declaration's capabilities, components and
// metadata callbacks without resolving the surrounding composition's bindings.
// It applies the per-declaration stage Compile performs before binding
// resolution, so an implementation whose declaration binds another extension
// can validate its own construction without owning the dependency graph.
func CompileNative(native Native) (*Compiled, error) {
	c := &Compiled{
		byID:         make(map[string]*CompiledDeclaration, 1),
		capabilities: make(map[string]*CompiledCapability),
		byLifetime:   make(map[Lifetime][]*CompiledComponent),
		natives:      make(map[string]Native, 1),
	}
	owned := native.Owned()
	if err := c.addDeclaration(owned.Declaration, 0); err != nil {
		return nil, err
	}
	c.natives[owned.Declaration.ID] = owned
	if err := c.validateNative(owned.Declaration.ID, owned); err != nil {
		return nil, err
	}
	return c, nil
}

// ValidateInstance checks one opened component's exact declared binding shape
// against this declaration.
func (d *CompiledDeclaration) ValidateInstance(componentID string, instance Instance) error {
	component, ok := d.Components[componentID]
	if !ok {
		return invalid("component %q is not declared", componentID)
	}
	if len(instance.Capabilities) != len(component.Provides) {
		return invalid("component %q supplied %d capabilities, want %d", componentID, len(instance.Capabilities), len(component.Provides))
	}
	declared := make(map[string]*CompiledCapability, len(component.Provides))
	for _, capability := range component.Provides {
		declared[capability.ID] = capability
	}
	for id := range instance.Capabilities {
		if _, ok := declared[id]; !ok {
			return invalid("component %q exported undeclared capability %q", componentID, id)
		}
	}
	for id, capability := range declared {
		implementation, ok := instance.Capabilities[id]
		if !ok {
			return invalid("component %q is missing declared capability %q", componentID, id)
		}
		if err := implementation.validate(capability); err != nil {
			return err
		}
	}
	return nil
}

// ValidateInstance checks one opened component's exact declared binding shape.
func (c *Compiled) ValidateInstance(extensionID, componentID string, instance Instance) error {
	declaration, ok := c.byID[extensionID]
	if !ok {
		return invalid("component %q is not declared", componentID)
	}
	return declaration.ValidateInstance(componentID, instance)
}

// ValidateArguments validates one named-service call input against the exact
// declared schema.
func ValidateArguments(methods MethodSet, method string, arguments json.RawMessage) error {
	contract, ok := methods[method]
	if !ok {
		return invalid("method %q is not declared", method)
	}
	return validateValue(contract.input, arguments, "arguments")
}

// ValidateResultEnvelope checks one result's common union envelope: exactly
// one success value or failure arm and nonempty failure code/message. Payload
// bytes stay opaque; their schema owners validate the enclosed values.
func ValidateResultEnvelope(result Result) error {
	if (result.Failure == nil) == (result.Value == nil) {
		return errors.New("result must carry exactly one success value or failure")
	}
	if result.Failure != nil && (result.Failure.Code == "" || result.Failure.Message == "") {
		return errors.New("failure requires nonempty code and message")
	}
	return nil
}

// ValidateResult validates one named-service result union against the exact
// declared schemas.
func ValidateResult(methods MethodSet, method string, result Result) error {
	contract, ok := methods[method]
	if !ok {
		return invalid("method %q is not declared", method)
	}
	if err := ValidateResultEnvelope(result); err != nil {
		return invalid("method %q: %v", method, err)
	}
	if result.Failure != nil {
		if len(result.Failure.Data) != 0 {
			if contract.failure == nil {
				return invalid("method %q failure data is not declared", method)
			}
			if err := validateValue(contract.failure, result.Failure.Data, "failure data"); err != nil {
				return err
			}
		}
		if len(result.Failure.Partial) != 0 {
			if contract.partial == nil {
				return invalid("method %q partial result is not declared", method)
			}
			if err := validateValue(contract.partial, result.Failure.Partial, "partial result"); err != nil {
				return err
			}
		}
		return nil
	}
	return validateValue(contract.success, result.Value, "success value")
}

// ValidateFailures validates only the declared-failure shape, for callers that
// already own the success path.
func validateValue(schema *jsonschema.Schema, raw json.RawMessage, label string) error {
	value, err := decodeJSON(raw)
	if err != nil {
		return invalid("%s: %v", label, err)
	}
	if err := schema.Validate(value); err != nil {
		return invalid("%s: %v", label, err)
	}
	return nil
}

// schemaCacheKey identifies one compiled schema by its exact resource label
// and exact schema bytes.
type schemaCacheKey struct {
	label string
	raw   string
}

// compiledSchemas memoizes successful compilations process-wide. A compiled
// *jsonschema.Schema is fully built by Compiler.Compile and validated only
// through per-call validator state, so it is immutable after compilation and
// safe for concurrent Validate. Failed compilations are never cached.
var compiledSchemas sync.Map // schemaCacheKey -> *jsonschema.Schema

// compileSchema compiles one local schema. Relative references resolve only
// inside the supplied document; a remote reference reaches the rejecting
// loader and fails. The compiler URL is private to this compile call, and
// identical label and bytes reuse one compiled schema.
func compileSchema(raw json.RawMessage, label string) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("missing schema")
	}
	key := schemaCacheKey{label: label, raw: string(raw)}
	if cached, ok := compiledSchemas.Load(key); ok {
		return cached.(*jsonschema.Schema), nil
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(rejectingLoader{})
	const location = "melta:///local-schema"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return nil, err
	}
	compiledSchemas.Store(key, schema)
	return schema, nil
}

type rejectingLoader struct{}

func (rejectingLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is not permitted", url)
}

type contractIdentity struct {
	id      string
	version string
}

func contractStructurallyEqual(a, b CapabilityDefinition) bool {
	if a.ContractID != b.ContractID || a.ContractVersion != b.ContractVersion || a.Kind != b.Kind {
		return false
	}
	if a.Kind == KindHook || a.Kind == KindStrategy {
		return true
	}
	return methodsStructurallyEqual(a.Methods, b.Methods)
}

func definitionStructurallyEqual(a, b CapabilityDefinition) bool {
	return a.ContractID == b.ContractID && a.ContractVersion == b.ContractVersion && a.Kind == b.Kind &&
		methodsStructurallyEqual(a.Methods, b.Methods)
}

func methodsStructurallyEqual(a, b []Method) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]Method, len(a))
	for _, method := range a {
		index[method.Name] = method
	}
	for _, method := range b {
		left, ok := index[method.Name]
		if !ok || !methodStructurallyEqual(left, method) {
			return false
		}
	}
	return true
}

func methodStructurallyEqual(a, b Method) bool {
	if a.Name != b.Name || a.Mode != b.Mode {
		return false
	}
	for _, pair := range [][2]json.RawMessage{{a.Input, b.Input}, {a.Success, b.Success}, {a.Error, b.Error}, {a.Partial, b.Partial}} {
		if !schemaStructurallyEqual(pair[0], pair[1]) {
			return false
		}
	}
	return true
}

func schemaStructurallyEqual(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	emptyA := len(bytes.TrimSpace(a)) == 0
	emptyB := len(bytes.TrimSpace(b)) == 0
	if emptyA || emptyB {
		return emptyA && emptyB
	}
	left, err := decodeJSON(a)
	if err != nil {
		return false
	}
	right, err := decodeJSON(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func hasBoundary(hooks []Hook, boundary HookBoundary) bool {
	for _, hook := range hooks {
		if hook.Boundary == boundary {
			return true
		}
	}
	return false
}
