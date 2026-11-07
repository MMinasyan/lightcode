package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// implementationProvenance resolves every actual implementing extension root:
// the selected tools/hooks/strategy, the named-definition owner when used, and
// their transitively declared dependency closure, excluding the host baseline.
func implementationProvenance(c *composition, sel selection) []harness.ImplementationCapture {
	roots := make(map[string]bool)
	seen := make(map[string]bool)
	var addCapability func(*extension.CompiledCapability)
	addCapability = func(capability *extension.CompiledCapability) {
		if capability == nil || seen[capability.ID] {
			return
		}
		seen[capability.ID] = true
		roots[capability.Declaration.Declaration.ID] = true
		for _, binding := range capability.Component.Requires {
			if binding.Host || binding.Target == nil {
				continue
			}
			addCapability(binding.Target)
		}
	}
	add := func(id string) {
		if capability, ok := c.compiled.Capability(id); ok {
			addCapability(capability)
		}
	}
	for _, id := range sel.selected {
		add(id)
	}
	if sel.inputs.Strategy.ID != "" {
		add(sel.inputs.Strategy.ID)
	}
	if sel.definition != nil {
		add(sel.definition.CapabilityID)
	}
	var out []harness.ImplementationCapture
	for _, declaration := range c.compiled.Declarations() {
		id := declaration.Declaration.ID
		if !roots[id] {
			continue
		}
		out = append(out, harness.ImplementationCapture{ExtensionID: id, Version: declaration.Declaration.Version, Settings: sel.invocation.Config(id)})
	}
	return out
}

func selectedCapability(scopes []*scope, id string) (*capabilityValue, bool) {
	for _, scope := range scopes {
		if value, ok := scope.capabilities[id]; ok {
			return value, true
		}
	}
	return nil, false
}

// resolveStrategyBinding selects the execution's strategy.
func resolveStrategyBinding(c *composition, scopes []*scope, selected harness.StrategySelection) (*harness.StrategyBinding, error) {
	if selected.ID == "" {
		return productionStrategy, nil
	}
	capability, ok := c.compiled.Capability(selected.ID)
	if !ok || capability.Kind != extension.KindStrategy {
		return nil, fmt.Errorf("unknown strategy %q: %w", selected.ID, ErrComposition)
	}
	value, ok := selectedCapability(scopes, selected.ID)
	if !ok {
		return nil, fmt.Errorf("strategy %q is not open in this execution: %w", selected.ID, ErrClosed)
	}
	if value.implementation.Strategy == nil {
		return nil, fmt.Errorf("strategy %q has no implementation: %w", selected.ID, ErrComposition)
	}
	return &harness.StrategyBinding{ID: capability.ID, RequiresContextWindow: capability.Strategy.RequiresContextWindow, Strategy: value.implementation.Strategy}, nil
}

func (c *composition) toolSpec(id string) (toolDeclaration, bool) {
	export, ok := c.exports[id]
	if !ok {
		return toolDeclaration{}, false
	}
	if export.standard {
		return toolDeclaration{id: id, standardDescribe: export.standardDescribe}, true
	}
	return toolDeclaration{id: id, capability: export.capability, native: export.native}, true
}

func (c *composition) toolDeclarations(names []string) ([]toolDeclaration, error) {
	declarations := make([]toolDeclaration, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		declaration, ok := c.toolSpec(name)
		if !ok {
			return nil, fmt.Errorf("tool %q has no composed declaration: %w", name, ErrComposition)
		}
		declarations = append(declarations, declaration)
	}
	return declarations, nil
}

func (c *composition) bindTool(sel selection, id string) (Tool, error) {
	export, ok := c.exports[id]
	if !ok {
		return nil, fmt.Errorf("tool %q has no composed declaration: %w", id, ErrComposition)
	}
	if export.standard {
		value := c.standardValues[id]
		if value == nil {
			return nil, fmt.Errorf("standard tool %q has no value: %w", id, ErrComposition)
		}
		return value, nil
	}
	value, ok := selectedCapability(sel.scopes, id)
	if !ok || value.capability == nil || value.capability.Kind != extension.KindTool || value.scope.unavailable() {
		return nil, fmt.Errorf("tool %q is not open in this invocation: %w", id, ErrClosed)
	}
	if value.implementation.Tool == nil {
		return nil, fmt.Errorf("tool %q has no implementation: %w", id, ErrComposition)
	}
	return extensionTool{composition: c, value: value}, nil
}

// extensionTool adapts one native Tool contribution to the host's baseline
// tool interface and builds the same Harness PreparedTool gate.
type extensionTool struct {
	composition *composition
	value       *capabilityValue
}

func (t extensionTool) baseIdentity(tc ToolContext, callID string, grant nativeGrant) callIdentity {
	return callIdentity{
		session:      tc.Session,
		operationID:  tc.OperationID,
		invocationID: tc.InvocationID,
		purpose:      string(tc.Purpose),
		toolCallID:   callID,
		revision:     tc.Invocation.Revision(),
		grant:        grant,
	}
}

func (t extensionTool) context(tc ToolContext, calls extension.CallContext) extension.ToolContext {
	return extension.ToolContext{
		Session:       tc.Session,
		Model:         tc.Model,
		Revision:      tc.Invocation.Revision(),
		Workspace:     tc.Workspace,
		AdmittedEntry: tc.AdmittedEntry,
		Constraints:   tc.Constraints,
		Config:        tc.Invocation.Config(t.value.capability.Declaration.Declaration.ID),
		AgentTypes:    tc.Invocation.AgentTypes(),
		Calls:         calls,
	}
}

func (t extensionTool) Normalize(ctx context.Context, tc ToolContext, call model.ToolCall) (json.RawMessage, error) {
	carrier := harness.CallbackIdentityFromContext(ctx)
	identity := t.baseIdentity(tc, call.ID, nativeGrant{pure: true})
	identity.requestID = carrier.RequestID
	identity.toolCallID = carrier.ToolCallID
	if identity.toolCallID == "" {
		identity.toolCallID = call.ID
	}
	identity.effectID = carrier.EffectID
	view, calls := t.composition.newView(t.value, ctx, identity)
	defer view.end()
	return t.value.implementation.Tool.Normalize(t.context(tc, calls), call)
}

func (t extensionTool) Prepare(ctx context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	carrier := harness.CallbackIdentityFromContext(ctx)
	identity := t.baseIdentity(tc, call.ID, nativeGrant{pure: true})
	identity.requestID = carrier.RequestID
	identity.toolCallID = carrier.ToolCallID
	if identity.toolCallID == "" {
		identity.toolCallID = call.ID
	}
	view, calls := t.composition.newView(t.value, ctx, identity)
	plan := t.value.implementation.Tool.Prepare(view.ctx, t.context(tc, calls), call)
	view.end()
	return t.prepared(tc, call, plan)
}

func (t extensionTool) prepared(tc ToolContext, call model.ToolCall, plan extension.ToolPlan) harness.PreparedTool {
	owned := plan.Owned()
	// Three independent owned arrays: the public prepared plan the Harness
	// authorizes, the private frozen action grant, and the plan handed back to
	// the native executor. Author mutation of any one cannot alter another.
	actionPermissions := slices.Clone(owned.Permissions)
	prepared := harness.PreparedTool{Permissions: slices.Clone(owned.Permissions), CanonicalWorkspace: owned.CanonicalWorkspace, CanonicalWriteDir: owned.CanonicalWriteDir, Immediate: owned.Immediate}
	if owned.Immediate != nil {
		return prepared
	}
	prepared.Execute = func(ctx context.Context) harness.ToolOutcome {
		carrier := harness.CallbackIdentityFromContext(ctx)
		identity := t.baseIdentity(tc, call.ID, nativeGrant{permissions: actionPermissions})
		identity.requestID = carrier.RequestID
		identity.toolCallID = carrier.ToolCallID
		if identity.toolCallID == "" {
			identity.toolCallID = call.ID
		}
		identity.effectID = carrier.EffectID
		view, calls := t.composition.newView(t.value, ctx, identity)
		defer view.end()
		return t.value.implementation.Tool.Execute(view.ctx, t.context(tc, calls), owned)
	}
	return prepared
}

func hookBoundary(value *capabilityValue, boundary extension.HookBoundary) (extension.Hook, bool) {
	if value == nil || value.capability == nil || value.capability.Kind != extension.KindHook {
		return extension.Hook{}, false
	}
	for _, hook := range value.capability.Hooks {
		if hook.Boundary == boundary {
			return hook, true
		}
	}
	return extension.Hook{}, false
}

// bindToolArgumentHooks binds every selected before_tool hook in selected
// capability order.
func bindToolArgumentHooks(sel selection, base callIdentity) []harness.ToolArgumentsHook {
	composition := sel.invocation.snapshot.composition
	base.revision = sel.invocation.Revision()
	var hooks []harness.ToolArgumentsHook
	for _, id := range sel.inputs.Capabilities {
		value, ok := selectedCapability(sel.scopes, id)
		if !ok {
			continue
		}
		hook, ok := hookBoundary(value, extension.BoundaryBeforeTool)
		if !ok {
			continue
		}
		implementation := value.implementation.BeforeTool
		if implementation == nil {
			continue
		}
		mode := harness.HookModeEffectful
		pure := false
		if hook.Mode == extension.HookModePure {
			mode = harness.HookModePure
			pure = true
		}
		extensionID := value.capability.Declaration.Declaration.ID
		hookID := id
		hooks = append(hooks, harness.ToolArgumentsHook{
			ID:          hookID,
			ExtensionID: extensionID,
			Mode:        mode,
			Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
				carrier := harness.CallbackIdentityFromContext(ctx)
				identity := base
				identity.requestID = carrier.RequestID
				identity.toolCallID = call.ID
				identity.effectID = carrier.EffectID
				identity.grant = nativeGrant{pure: pure}
				view, calls := composition.newView(value, ctx, identity)
				defer view.end()
				return implementation(view.ctx, calls, call)
			},
		})
	}
	return hooks
}

// bindRequestHooks binds every selected request hook in selected capability order.
func bindRequestHooks(sel selection, base callIdentity) []harness.RequestHook {
	composition := sel.invocation.snapshot.composition
	base.revision = sel.invocation.Revision()
	var hooks []harness.RequestHook
	for _, id := range sel.inputs.Capabilities {
		value, ok := selectedCapability(sel.scopes, id)
		if !ok {
			continue
		}
		hook, ok := hookBoundary(value, extension.BoundaryRequest)
		if !ok {
			continue
		}
		implementation := value.implementation.Request
		if implementation == nil {
			continue
		}
		mode := harness.HookModeEffectful
		pure := false
		if hook.Mode == extension.HookModePure {
			mode = harness.HookModePure
			pure = true
		}
		extensionID := value.capability.Declaration.Declaration.ID
		hookID := id
		hooks = append(hooks, harness.RequestHook{
			ID:          hookID,
			ExtensionID: extensionID,
			Mode:        mode,
			Run: func(ctx context.Context, input harness.RequestHookInput) (harness.RequestConsequence, error) {
				carrier := harness.CallbackIdentityFromContext(ctx)
				identity := base
				identity.requestID = input.RequestID
				identity.effectID = carrier.EffectID
				identity.grant = nativeGrant{pure: pure}
				view, calls := composition.newView(value, ctx, identity)
				defer view.end()
				return implementation(view.ctx, calls, input)
			},
		})
	}
	return hooks
}

// bindToolOutputHooks binds every selected after_tool hook in selected order.
func bindToolOutputHooks(sel selection, base callIdentity) []harness.ToolOutputHook {
	composition := sel.invocation.snapshot.composition
	base.revision = sel.invocation.Revision()
	var hooks []harness.ToolOutputHook
	for _, id := range sel.inputs.Capabilities {
		value, ok := selectedCapability(sel.scopes, id)
		if !ok {
			continue
		}
		if _, ok := hookBoundary(value, extension.BoundaryAfterTool); !ok {
			continue
		}
		implementation := value.implementation.AfterTool
		if implementation == nil {
			continue
		}
		extensionID := value.capability.Declaration.Declaration.ID
		hookID := id
		hooks = append(hooks, harness.ToolOutputHook{
			ID:          hookID,
			ExtensionID: extensionID,
			Run: func(ctx context.Context, input harness.ToolOutputInput) (string, error) {
				carrier := harness.CallbackIdentityFromContext(ctx)
				identity := base
				identity.requestID = carrier.RequestID
				identity.toolCallID = input.Outcome.Result.CallID
				identity.effectID = carrier.EffectID
				identity.grant = nativeGrant{pure: true}
				view, calls := composition.newView(value, ctx, identity)
				defer view.end()
				return implementation(view.ctx, calls, input)
			},
		})
	}
	return hooks
}
