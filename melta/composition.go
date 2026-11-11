package melta

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

// Error identities reported by typed composition and scope lifetime.
var (
	// ErrComposition wraps invalid declarations, missing or wrongly typed
	// bindings, invalid instance values, and failed construction.
	ErrComposition = errors.New("melta: composition failure")

	// ErrClosed reports a closed or owner-canceled scope or owner: no guard
	// can enter and no binding resolves from an unavailable supplying scope.
	ErrClosed = errors.New("melta: closed")
)

// ScopeKind names one lifetime tier of the nested host scope hierarchy. It is
// the host's name for the corresponding extension lifetime.
type ScopeKind string

const (
	ScopeRuntime   ScopeKind = "runtime"
	ScopeWorkspace ScopeKind = "workspace"
	ScopeOperation ScopeKind = "operation"
	ScopeAgent     ScopeKind = "agent"
)

// rank orders scopes from longer- to shorter-lived. An unknown kind ranks zero.
func (k ScopeKind) rank() int {
	switch k {
	case ScopeRuntime:
		return 1
	case ScopeWorkspace:
		return 2
	case ScopeOperation:
		return 3
	case ScopeAgent:
		return 4
	}
	return 0
}

// lifetime maps the host scope tier onto the declaration lifetime vocabulary.
func (k ScopeKind) lifetime() extension.Lifetime {
	switch k {
	case ScopeRuntime:
		return extension.LifetimeHarness
	case ScopeWorkspace:
		return extension.LifetimeWorkspace
	case ScopeOperation:
		return extension.LifetimeOperation
	case ScopeAgent:
		return extension.LifetimeInvocation
	}
	return ""
}

// ScopeInfo is the immutable identity handed to one factory. DataDir is the
// same normalized owner root in every ScopeInfo. Workspace scopes carry only
// lexical Workspace identity, never Session or Operation attribution; the
// Melta scope carries no narrower attribution at all; short scopes carry
// the complete Workspace/Session/Operation identity. The captured section and
// warning callback reach a factory only through extension.InstanceContext.
type ScopeInfo struct {
	Kind         ScopeKind
	DataDir      string
	Workspace    string
	SessionID    string
	OperationID  string
	InvocationID string
	Purpose      string
	Revision     string

	sections map[string]json.RawMessage

	// declaredBindings are the candidate's captured declared field values
	// this scope's components resolve for activation. operationSecrets are the
	// admitted Operation's captured operation fields, present only on the
	// Operation and Invocation scopes.
	declaredBindings map[declaredField]declaredBinding
	operationSecrets map[string]map[string]string

	// artifactLease is the execution's already-held Session interval shared
	// by its Operation and invocation scopes. Standalone callbacks leave it
	// nil and create their own call-owned lease.
	artifactLease *artifactCallLease
}

// toolDeclaration is one selected tool's composition declaration: an owner
// standard tool or a native extension capability.
type toolDeclaration struct {
	id               string
	standardDescribe func(Invocation, harness.ToolConstraints, harness.SessionIdentity) (ToolDescription, error)
	capability       *extension.CompiledCapability
	native           extension.Native
}

func (t toolDeclaration) describe(ctx context.Context, inv Invocation, selected model.ModelRef, constraints harness.ToolConstraints, identity harness.SessionIdentity, inputs harness.AgentInputs) (ToolDescription, error) {
	if t.standardDescribe != nil {
		return t.standardDescribe(inv, constraints, identity)
	}
	if t.capability == nil {
		return ToolDescription{}, fmt.Errorf("tool %q has no declaration: %w", t.id, ErrComposition)
	}
	describe := t.native.Describe[t.id]
	if describe == nil {
		return ToolDescription{}, fmt.Errorf("tool %q has no metadata callback: %w", t.id, ErrComposition)
	}
	ownedInputs := inputs.Owned()
	description, err := describe(ctx, extension.DescribeInput{
		Session:     identity,
		Model:       ownedInputs.Model,
		Constraints: ownedInputs.Constraints,
		Revision:    inv.Revision(),
		Config:      inv.Config(t.capability.Declaration.Declaration.ID),
		AgentTypes:  inv.AgentTypes(),
		Inputs:      ownedInputs,
	})
	if err != nil {
		return ToolDescription{}, err
	}
	definition, err := model.NewToolDefinition(description.Definition)
	if err != nil {
		return ToolDescription{}, fmt.Errorf("tool %q: %w", t.id, err)
	}
	if definition.Name != t.id {
		return ToolDescription{}, fmt.Errorf("tool %q: description declares name %q", t.id, definition.Name)
	}
	description.Definition = definition
	return ToolDescription{Definition: description.Definition, Available: description.Available}, nil
}

// capabilityExport is one entry in the composition's single export table.
type capabilityExport struct {
	standard         bool
	standardDescribe func(Invocation, harness.ToolConstraints, harness.SessionIdentity) (ToolDescription, error)
	capability       *extension.CompiledCapability
	native           extension.Native
}

// composition is the validated static native set: an owned compiled
// declaration graph, per-lifetime construction plans and the one export table
// every capability ID resolves through.
type composition struct {
	natives  []extension.Native
	compiled *extension.Compiled

	plan    map[ScopeKind][]*extension.CompiledComponent
	exports map[string]capabilityExport

	// standardValues holds the owner-constructed standard tool values the
	// Melta scope binds as its own provided capabilities.
	standardValues map[string]Tool

	// defaultStrategy is the host-selected default strategy binding used when
	// no strategy ID is selected.
	defaultStrategy *harness.StrategyBinding

	// reportWarning is the owner's neutral plugin-warning sink.
	reportWarning func(pluginID, kind, message string)

	// hosts supplies the fixed host named services. The child-launch handler
	// reads the Melta's background bridge at call time; it is nil until the
	// owner arms it. The data handler reads the owner Harness and Session
	// artifact registry from the same shared value.
	hosts *hostServices
}

type hostServices struct {
	background *backgroundBridge
	harness    *harness.Harness
	artifacts  *artifactIntervals
	dataDir    string
}

// registeredComposition owns registration data without composing inactive
// extensions.
func registeredComposition(natives []extension.Native) *composition {
	owned := make([]extension.Native, len(natives))
	for i, native := range natives {
		owned[i] = native.Owned()
	}
	c := &composition{
		natives:        owned,
		plan:           make(map[ScopeKind][]*extension.CompiledComponent),
		standardValues: make(map[string]Tool),
		hosts:          &hostServices{},
	}
	return c
}

// newComposition compiles the complete selected native set before any factory
// can run and seeds the seven standard tool declarations into the same export
// table as owner-provided Harness-lifetime capabilities.
func newNativeComposition(natives []extension.Native) (*composition, error) {
	owned := make([]extension.Native, len(natives))
	copy(owned, natives)
	compiled, err := extension.Compile(owned)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrComposition, err)
	}
	c := &composition{
		natives:        owned,
		compiled:       compiled,
		plan:           make(map[ScopeKind][]*extension.CompiledComponent),
		standardValues: make(map[string]Tool),
		exports:        make(map[string]capabilityExport),
	}
	for _, declaration := range standardToolDeclarations() {
		if _, duplicate := c.exports[declaration.id]; duplicate {
			return nil, fmt.Errorf("standard tool %q declared twice: %w", declaration.id, ErrComposition)
		}
		c.exports[declaration.id] = capabilityExport{standard: true, standardDescribe: declaration.describe}
	}
	for _, lifetime := range []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace, extension.LifetimeOperation, extension.LifetimeInvocation} {
		for _, component := range compiled.Components(lifetime) {
			c.plan[scopeKindFor(lifetime)] = append(c.plan[scopeKindFor(lifetime)], component)
			native, _ := compiled.Native(component.Declaration.Declaration.ID)
			for _, capability := range component.Provides {
				if _, duplicate := c.exports[capability.ID]; duplicate {
					return nil, fmt.Errorf("capability %q already exported: %w", capability.ID, ErrComposition)
				}
				c.exports[capability.ID] = capabilityExport{capability: capability, native: native}
			}
		}
	}
	return c, nil
}

func scopeKindFor(lifetime extension.Lifetime) ScopeKind {
	switch lifetime {
	case extension.LifetimeHarness:
		return ScopeRuntime
	case extension.LifetimeWorkspace:
		return ScopeWorkspace
	case extension.LifetimeOperation:
		return ScopeOperation
	case extension.LifetimeInvocation:
		return ScopeAgent
	}
	return ""
}

// validateSettings invokes each installed active extension's local schema
// before publication.
func (c *composition) validateSettings(sections map[string]json.RawMessage) error {
	for _, native := range c.natives {
		id := native.Declaration.ID
		raw, ok := sections[id]
		if !ok || !sectionEnabled(raw) {
			continue
		}
		section := bytes.Clone(raw)
		if err := c.compiled.ValidateSection(id, section); err != nil {
			return fmt.Errorf("%w: %w", ErrComposition, err)
		}
	}
	return nil
}

// validateScopeInfo enforces the per-kind attribution shape.
func validateScopeInfo(info ScopeInfo) error {
	if info.DataDir == "" {
		return fmt.Errorf("scope %s: missing data directory: %w", info.Kind, ErrComposition)
	}
	switch info.Kind {
	case ScopeRuntime:
		if info.Workspace != "" || info.SessionID != "" || info.OperationID != "" || info.InvocationID != "" {
			return fmt.Errorf("runtime scope carries narrower attribution %q/%q/%q/%q: %w", info.Workspace, info.SessionID, info.OperationID, info.InvocationID, ErrComposition)
		}
	case ScopeWorkspace:
		if info.Workspace == "" || info.SessionID != "" || info.OperationID != "" || info.InvocationID != "" {
			return fmt.Errorf("workspace scope requires only the Workspace identity, got %q/%q/%q/%q: %w", info.Workspace, info.SessionID, info.OperationID, info.InvocationID, ErrComposition)
		}
	case ScopeOperation:
		if info.Workspace == "" || info.SessionID == "" || info.OperationID == "" || info.InvocationID != "" {
			return fmt.Errorf("operation scope requires Workspace, Session, and Operation identity: %w", ErrComposition)
		}
	case ScopeAgent:
		if info.Workspace == "" || info.SessionID == "" || info.OperationID == "" || info.InvocationID == "" || info.Purpose == "" {
			return fmt.Errorf("invocation scope requires Workspace, Session, Operation, and invocation identity: %w", ErrComposition)
		}
	default:
		return fmt.Errorf("invalid scope kind %q: %w", info.Kind, ErrComposition)
	}
	return nil
}

// bindReportWarning returns one factory's passive warning callback.
func (c *composition) bindReportWarning(scopeCtx context.Context, info ScopeInfo, extensionID string, report func(string, string, string)) func(kind, message string) {
	if info.Kind != ScopeRuntime || report == nil {
		return nil
	}
	return func(kind, message string) {
		if scopeCtx.Err() != nil {
			return
		}
		report(extensionID, kind, message)
	}
}

// capabilityValue is one constructed capability instance and its resolved
// dependencies.
type capabilityValue struct {
	capability     *extension.CompiledCapability
	component      *extension.CompiledComponent
	instanceID     string
	implementation extension.Implementation
	standard       Tool
	scope          *scope
	targets        map[string]*capabilityValue

	// activationSecrets holds the declared activation credential values
	// captured when this component opened. Every capability view created for
	// the component shares the map; no durable value carries it.
	activationSecrets map[string]string
}

// scopeConstructionError keeps rollback failures distinct.
type scopeConstructionError struct {
	cause   error
	cleanup error
}

func (e *scopeConstructionError) Error() string { return errors.Join(e.cause, e.cleanup).Error() }

func (e *scopeConstructionError) Unwrap() []error {
	if e.cleanup == nil {
		return []error{e.cause}
	}
	return []error{e.cause, e.cleanup}
}

// openScope constructs the components planned for info.Kind in declaration
// order, seeding ordinary dependencies from the supplied ancestor scopes.
func (c *composition) openScope(ctx context.Context, info ScopeInfo, ancestors []*scope) (*scope, error) {
	return c.openScopeReporting(ctx, info, ancestors, c.reportWarning)
}

func (c *composition) openScopeReporting(ctx context.Context, info ScopeInfo, ancestors []*scope, report func(string, string, string)) (*scope, error) {
	if err := validateScopeInfo(info); err != nil {
		return nil, err
	}
	scopeCtx, cancel := context.WithCancel(ctx)
	sc := &scope{info: info, ctx: scopeCtx, cancel: cancel, artifactLease: info.artifactLease}

	if len(ancestors) > 0 {
		sc.obs = ancestors[0].obs
	}
	resolved := make(map[string]*capabilityValue)
	for _, ancestor := range ancestors {
		for id, value := range ancestor.capabilities {
			resolved[id] = value
		}
	}
	own := make(map[string]*capabilityValue)
	if info.Kind == ScopeRuntime {
		for _, declaration := range standardToolDeclarations() {
			value, supplied := c.standardValues[declaration.id]
			if !supplied {
				continue
			}
			capability := &capabilityValue{standard: value, scope: sc}
			resolved[declaration.id] = capability
			own[declaration.id] = capability
		}
	}
	fail := func(err error) (*scope, error) {
		return nil, &scopeConstructionError{cause: err, cleanup: sc.cleanup()}
	}
	for _, component := range c.plan[info.Kind] {
		if err := scopeCtx.Err(); err != nil {
			return fail(err)
		}
		native, ok := c.compiled.Native(component.Declaration.Declaration.ID)
		if !ok {
			return fail(fmt.Errorf("component %q: extension is not registered: %w", component.ID, ErrComposition))
		}
		targets := make(map[string]*capabilityValue, len(component.Requires))
		for _, binding := range component.Requires {
			if binding.Host {
				continue
			}
			provider, ok := resolved[binding.Target.ID]
			if !ok {
				return fail(fmt.Errorf("component %q: dependency %q is unresolved: %w", component.ID, binding.Name, ErrComposition))
			}
			targets[binding.Name] = provider
		}
		instanceID, err := newInstanceID()
		if err != nil {
			return fail(err)
		}
		scopeIdentity := extension.ScopeIdentity{
			Kind:         info.Kind.lifetime(),
			Revision:     info.Revision,
			Workspace:    info.Workspace,
			SessionID:    info.SessionID,
			OperationID:  info.OperationID,
			InvocationID: info.InvocationID,
			Purpose:      info.Purpose,
		}
		activationSecrets := c.captureActivationSecrets(component.Declaration, info.declaredBindings)
		openValue := &capabilityValue{component: component, instanceID: instanceID, scope: sc, targets: targets, activationSecrets: activationSecrets}
		openIdentity := callIdentity{
			session:          harness.SessionIdentity{SessionID: info.SessionID},
			operationID:      info.OperationID,
			invocationID:     info.InvocationID,
			purpose:          info.Purpose,
			revision:         info.Revision,
			operationSecrets: info.operationSecrets,
		}
		view, call := c.newView(openValue, scopeCtx, openIdentity)
		instanceContext := extension.InstanceContext{
			ExtensionID:   component.Declaration.Declaration.ID,
			ComponentID:   component.ID,
			Version:       component.Declaration.Declaration.Version,
			InstanceID:    instanceID,
			Lifetime:      info.Kind.lifetime(),
			Scope:         scopeIdentity,
			Revision:      info.Revision,
			Section:       bytes.Clone(info.sections[component.Declaration.Declaration.ID]),
			DataDir:       info.DataDir,
			ReportWarning: c.bindReportWarning(scopeCtx, info, component.Declaration.Declaration.ID, report),
			Call:          call.Call,
		}
		instance, err := native.Open(view.ctx, instanceContext)
		if err != nil {
			view.end()
			return fail(fmt.Errorf("component %q: open: %w", component.ID, err))
		}
		instance = instance.Owned()
		if instance.Close != nil {
			closeInstance := instance.Close
			sc.closers = append(sc.closers, func() error {
				closeValue := &capabilityValue{component: component, instanceID: instanceID, scope: sc, targets: targets, activationSecrets: activationSecrets}
				closeIdentity := callIdentity{
					session:          harness.SessionIdentity{SessionID: info.SessionID},
					operationID:      info.OperationID,
					invocationID:     info.InvocationID,
					purpose:          info.Purpose,
					revision:         info.Revision,
					grant:            nativeGrant{lifecycle: true},
					operationSecrets: info.operationSecrets,
				}
				closeCtx, cancelClose := context.WithTimeout(context.Background(), nativeCloseDeadline)
				closeView, call := c.newView(closeValue, closeCtx, closeIdentity)
				err := closeInstance(closeView.ctx, call)
				closeView.end()
				cancelClose()
				return err
			})
		}
		view.end()
		if err := c.compiled.ValidateInstance(component.Declaration.Declaration.ID, component.ID, instance); err != nil {
			return fail(fmt.Errorf("component %q: %w: %w", component.ID, ErrComposition, err))
		}
		value := &capabilityValue{component: component, instanceID: instanceID, scope: sc, targets: targets, activationSecrets: activationSecrets}
		for _, capability := range component.Provides {
			implementation, ok := instance.Capabilities[capability.ID]
			if !ok {
				return fail(fmt.Errorf("component %q: missing capability %q: %w", component.ID, capability.ID, ErrComposition))
			}
			instanceValue := *value
			instanceValue.capability = capability
			instanceValue.implementation = implementation
			own[capability.ID] = &instanceValue
			resolved[capability.ID] = &instanceValue
		}
	}
	sc.capabilities = own
	if sc.obs != nil && info.Kind != ScopeWorkspace {
		sc.obs.publish(func() []Event {
			return []Event{scopeEvent(protocol.ScopeOpened, info)}
		})
	}
	return sc, nil
}

// nativeCloseDeadline is the fresh cooperative bound of one native Close
// callback. It is a cancellation opportunity, not forced termination: trusted
// native code that ignores its context can still block cleanup.
var nativeCloseDeadline = 5 * time.Second

// hostHandler returns one fixed host named service.
func (c *composition) hostHandler(id, method string) (extension.Handler, bool) {
	switch id {
	case extension.ContractPrompt:
		return c.promptHandler, true
	case extension.ContractChildLaunch:
		return c.childLaunchHandler, true
	case extension.ContractData:
		return c.dataHandler(method)
	case extension.ContractCredentials:
		return c.credentialsHandler, true
	}
	return nil, false
}

// captureActivationSecrets resolves one component's declared activation
// SecretRefs against the candidate binding capture and returns the values
// captured when the candidate was built. Operation references are resolved
// from the admitted Operation capture, never here, and no environment is
// sampled at Open.
func (c *composition) captureActivationSecrets(declaration *extension.CompiledDeclaration, bindings map[declaredField]declaredBinding) map[string]string {
	if declaration == nil || len(bindings) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, secret := range declaration.Declaration.Secrets {
		if secret.Lifetime != extension.SecretActivation {
			continue
		}
		binding := bindings[declaredField{extensionID: declaration.Declaration.ID, field: secret.Field}]
		if binding.value == "" {
			continue
		}
		out[secret.Field] = binding.value
	}
	return out
}

// credentialsHandler resolves one declared credential field from the calling
// component's activation capture or the preparation's Operation capture. A
// field the extension did not declare is a composition misuse; every other
// absent, unresolved or undefined value fails uniformly as unavailable
// through the private lookup, with no Operation-presence distinction.
func (c *composition) credentialsHandler(ctx context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	declaration, ok := c.compiled.Declaration(call.ExtensionID)
	if !ok {
		return extension.Result{}, fmt.Errorf("extension %q is not installed: %w", call.ExtensionID, ErrClosed)
	}
	var declared *extension.SecretRef
	for i := range declaration.Declaration.Secrets {
		if declaration.Declaration.Secrets[i].Field == request.Field {
			declared = &declaration.Declaration.Secrets[i]
			break
		}
	}
	if declared == nil {
		return extension.Result{}, fmt.Errorf("credential field %q is not declared: %w", request.Field, ErrComposition)
	}
	values := secretValuesFrom(ctx)
	var value string
	var found bool
	if declared.Lifetime == extension.SecretActivation {
		value, found = values.activation[request.Field]
	} else {
		perExtension := values.operation[call.ExtensionID]
		value, found = perExtension[request.Field]
	}
	if !found || value == "" {
		return extension.Result{}, fmt.Errorf("credential %q is unavailable: %w", request.Field, ErrClosed)
	}
	raw, err := json.Marshal(struct {
		Value string `json:"value"`
	}{Value: value})
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: raw}, nil
}

func (c *composition) promptHandler(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var input prompt.AssemblyInput
	if err := json.Unmarshal(arguments, &input); err != nil {
		return extension.Result{}, err
	}
	assembled, err := prompt.Assemble(input)
	if err != nil {
		return extension.Result{}, err
	}
	value, err := json.Marshal(assembled)
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: value}, nil
}
func (c *composition) childLaunchHandler(ctx context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		Definition    harness.DefinitionRef `json:"definition"`
		Prompt        string                `json:"prompt"`
		OperationID   string                `json:"operation_id"`
		MaxConcurrent int                   `json:"max_concurrent"`
		OutputLimit   int                   `json:"output_limit"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	grant := hostGrant(ctx)
	// The permission target is the complete canonical reference, so a frozen
	// grant for one provider's definition cannot authorize another provider's
	// same-ID definition.
	target := request.Definition.PermissionTarget()
	if !slices.Contains(grant.permissions, harness.PermissionRequest{Permission: harness.PermissionChildLaunch, Target: target}) {
		return extension.Result{}, fmt.Errorf("child launch target %q is not authorized by the prepared plan: %w", target, ErrDenied)
	}
	if call.SessionID == "" {
		return extension.Result{}, fmt.Errorf("child launch has no calling Session: %w", ErrDenied)
	}
	if c.hosts == nil || c.hosts.background == nil || c.hosts.background.h == nil {
		return extension.Result{}, ErrClosed
	}
	result, err := c.hosts.background.h.LaunchChildSession(ctx, harness.LaunchChildRequest{
		ParentSessionID: call.SessionID,
		AgentSelection:  harness.AgentSelection{Definition: &request.Definition},
		Content:         []model.ContentPart{{Kind: model.PartText, Text: request.Prompt}},
		OperationID:     request.OperationID,
		ExtensionID:     call.ExtensionID,
		MaxConcurrent:   request.MaxConcurrent,
		OutputLimit:     request.OutputLimit,
	})
	if err != nil {
		return extension.Result{}, err
	}
	value, err := json.Marshal(struct {
		ChildSessionID string `json:"child_session_id"`
	}{ChildSessionID: result.ChildSessionID})
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: value}, nil
}

// scope is one constructed instance set with its guard state.
type scope struct {
	info   ScopeInfo
	ctx    context.Context
	cancel context.CancelFunc

	obs *observation

	// artifactLease is the execution interval's borrowed call lease, shared
	// by every root view this scope opens. Standalone scopes carry nil.
	artifactLease *artifactCallLease

	capabilities map[string]*capabilityValue
	closers      []func() error

	mu      sync.Mutex
	closing bool
	closed  bool
	wg      sync.WaitGroup
}

func (s *scope) unavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailableLocked()
}

func (s *scope) unavailableLocked() bool {
	return s.closing || s.closed || s.ctx.Err() != nil
}

// enter admits one ordinary call against the Caller's context.
func (s *scope) enter(ctx context.Context) (context.Context, func(), error) {
	return s.enterFor(ctx, false)
}

// enterFor admits one call. Ordinary admission refuses a closing, closed or
// canceled scope. Lifecycle admission (Close) uses a fresh callback context
// that ignores canceled ordinary work, but still refuses a scope that is
// already closing or closed.
func (s *scope) enterFor(ctx context.Context, lifecycle bool) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closing || s.closed {
		s.mu.Unlock()
		err := ctx.Err()
		if err == nil {
			err = ErrClosed
		}
		return nil, nil, err
	}
	if !lifecycle {
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			err := ctx.Err()
			if err == nil {
				err = ErrClosed
			}
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	s.wg.Add(1)
	s.mu.Unlock()
	var callCtx context.Context
	cancelCall := func() {}
	if lifecycle {
		// Lifecycle entry ignores the provider's canceled ordinary-work
		// context but still honors the Caller's admission context. The
		// actual closing/closed state above is the provider refusal gate.
		callCtx, cancelCall = context.WithCancel(ctx)
	} else {
		callCtx, cancelCall = context.WithCancel(s.ctx)
	}
	var stopCallerWatch func() bool
	if !lifecycle {
		stopCallerWatch = context.AfterFunc(ctx, cancelCall)
	}
	var once sync.Once
	return callCtx, func() {
		once.Do(func() {
			if stopCallerWatch != nil {
				stopCallerWatch()
			}
			cancelCall()
			s.wg.Done()
		})
	}, nil
}

func (s *scope) close() error {
	commit := func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}
	if s.obs != nil {
		s.obs.publish(func() []Event {
			commit()
			return []Event{scopeEvent(protocol.ScopeClosed, s.info)}
		})
	} else {
		commit()
	}
	return s.cleanup()
}

// cleanup is the common scope cleanup: mark closing before cancellation, join
// admitted work, then dispose.
func (s *scope) cleanup() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	return s.dispose()
}

func (s *scope) dispose() error {
	var errs []error
	for i := len(s.closers) - 1; i >= 0; i-- {
		if err := s.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type workspaceAttempt struct {
	done chan struct{}
	sc   *scope
	err  error
}

type workspaceScopes struct {
	owner     context.Context
	c         *composition
	ancestors []*scope
	obs       *observation
	sections  map[string]json.RawMessage

	mu         sync.Mutex
	closed     bool
	live       map[string]*scope
	attempts   map[string]*workspaceAttempt
	building   sync.WaitGroup
	cleanupErr error
}

func newWorkspaceScopes(owner context.Context, c *composition, ancestors []*scope, obs *observation, sections map[string]json.RawMessage) *workspaceScopes {
	return &workspaceScopes{
		owner:     owner,
		c:         c,
		ancestors: ancestors,
		obs:       obs,
		sections:  sections,
		live:      make(map[string]*scope),
		attempts:  make(map[string]*workspaceAttempt),
	}
}

func (w *workspaceScopes) get(ctx context.Context, info ScopeInfo) (*scope, error) {
	if info.Kind != ScopeWorkspace {
		return nil, fmt.Errorf("workspace scope requested with kind %q: %w", info.Kind, ErrComposition)
	}
	key := info.Workspace
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	if sc := w.live[key]; sc != nil {
		w.mu.Unlock()
		return sc, nil
	}
	attempt := w.attempts[key]
	if attempt == nil {
		attempt = &workspaceAttempt{done: make(chan struct{})}
		w.attempts[key] = attempt
		w.building.Add(1)
		go w.build(key, info, attempt)
	}
	w.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.sc, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *workspaceScopes) build(key string, info ScopeInfo, attempt *workspaceAttempt) {
	defer w.building.Done()
	info.sections = w.sections
	sc, err := w.c.openScope(w.owner, info, w.ancestors)
	if err != nil {
		w.mu.Lock()
		delete(w.attempts, key)
		var construction *scopeConstructionError
		if errors.As(err, &construction) {
			w.cleanupErr = errors.Join(w.cleanupErr, construction.cleanup)
		}
		w.mu.Unlock()
		attempt.sc, attempt.err = sc, err
		close(attempt.done)
		return
	}
	var rejected error
	w.obs.publish(func() []Event {
		sc.obs = w.obs
		w.mu.Lock()
		delete(w.attempts, key)
		if w.closed || w.owner.Err() != nil {
			rejected = w.owner.Err()
			if rejected == nil {
				rejected = ErrClosed
			}
			w.mu.Unlock()
			return nil
		}
		w.live[key] = sc
		w.mu.Unlock()
		attempt.sc, attempt.err = sc, nil
		close(attempt.done)
		return []Event{scopeEvent(protocol.ScopeOpened, info)}
	})
	if rejected != nil {
		cleanup := sc.cleanup()
		w.mu.Lock()
		w.cleanupErr = errors.Join(w.cleanupErr, cleanup)
		w.mu.Unlock()
		attempt.err = &scopeConstructionError{cause: rejected, cleanup: cleanup}
		close(attempt.done)
	}
}

func (w *workspaceScopes) shutdown() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.building.Wait()
	w.mu.Lock()
	keys := make([]string, 0, len(w.live))
	for key := range w.live {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	live := make([]*scope, len(keys))
	for i, key := range keys {
		live[i] = w.live[key]
	}
	cleanupErr := w.cleanupErr
	w.mu.Unlock()
	errs := []error{cleanupErr}
	for _, sc := range live {
		errs = append(errs, sc.close())
	}
	return errors.Join(errs...)
}

// newInstanceID mints one host-issued opaque instance identity.
func newInstanceID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
