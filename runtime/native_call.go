package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
)

// nativeGrant is the private authority one callback view carries. It never
// appears in an authoring value.
type nativeGrant struct {
	permissions []harness.PermissionRequest
	pure        bool
	lifecycle   bool
}

// callIdentity is the immutable attribution one native callback view carries.
type callIdentity struct {
	session      harness.SessionIdentity
	operationID  string
	invocationID string
	purpose      string
	requestID    string
	toolCallID   string
	effectID     string
	revision     string
	caller       extension.CallerIdentity
	grant        nativeGrant
}

// nativeView is one callback's private admission and cancellation owner. End
// closes admission, cancels the view origin, joins every nested call, then
// releases the admission it borrowed.
type nativeView struct {
	composition *composition
	component   *extension.CompiledComponent
	targets     map[string]*capabilityValue
	auth        extension.CallContext
	grant       nativeGrant

	origin  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	release func()

	mu   sync.Mutex
	done bool
	wg   sync.WaitGroup
}

// newView creates one callback view over value and returns its authoring call
// context. A guest-supplied context may narrow the view's origin cancellation
// or deadline but can never detach it.
func (c *composition) newView(value *capabilityValue, parent context.Context, identity callIdentity) (*nativeView, extension.CallContext) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	view := &nativeView{
		composition: c,
		component:   value.component,
		targets:     value.targets,
		grant:       identity.grant,
		origin:      parent,
		ctx:         ctx,
		cancel:      cancel,
	}
	sessionID := identity.session.SessionID
	if sessionID == "" {
		sessionID = value.scope.info.SessionID
	}
	operationID := identity.operationID
	if operationID == "" {
		operationID = value.scope.info.OperationID
	}
	invocationID := identity.invocationID
	if invocationID == "" {
		invocationID = value.scope.info.InvocationID
	}
	purpose := identity.purpose
	if purpose == "" {
		purpose = value.scope.info.Purpose
	}
	registration := value.component.Declaration.Declaration
	view.auth = extension.CallContext{
		ExtensionID:  registration.ID,
		ComponentID:  value.component.ID,
		InstanceID:   value.instanceID,
		Lifetime:     value.scope.info.Kind.lifetime(),
		Scope:        extension.ScopeIdentity{Kind: value.scope.info.Kind.lifetime(), Revision: value.scope.info.Revision, Workspace: value.scope.info.Workspace, SessionID: value.scope.info.SessionID, OperationID: value.scope.info.OperationID, InvocationID: value.scope.info.InvocationID, Purpose: value.scope.info.Purpose},
		Revision:     identity.revision,
		Section:      append(json.RawMessage(nil), value.scope.info.sections[registration.ID]...),
		SessionID:    sessionID,
		OperationID:  operationID,
		InvocationID: invocationID,
		Purpose:      purpose,
		RequestID:    identity.requestID,
		ToolCallID:   identity.toolCallID,
		EffectID:     identity.effectID,
		Caller:       identity.caller,
	}
	view.auth.Call = view.call
	return view, view.auth
}

// call invokes one declared binding through this view. The host origin and the
// supplied context are both cancellation sources: the supplied context may
// narrow the call with its own deadline or cancellation, but Background can
// never detach the host origin. The merge links are stopped on return.
func (v *nativeView) call(supplied context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
	v.mu.Lock()
	if v.done {
		v.mu.Unlock()
		return extension.Result{}, extension.ErrClosed
	}
	v.wg.Add(1)
	v.mu.Unlock()
	defer v.wg.Done()
	if supplied == nil {
		supplied = context.Background()
	}
	if err := v.ctx.Err(); err != nil {
		return extension.Result{}, err
	}
	if err := supplied.Err(); err != nil {
		return extension.Result{}, err
	}
	merged, cancel := context.WithCancel(supplied)
	stop := context.AfterFunc(v.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if err := v.ctx.Err(); err != nil {
		return extension.Result{}, err
	}
	if err := supplied.Err(); err != nil {
		return extension.Result{}, err
	}
	return v.composition.dispatch(merged, v.auth, v.grant, v.component, v.targets, binding, method, arguments)
}

// end closes admission, cancels the view origin, joins nested calls, then
// releases borrowed admission.
func (v *nativeView) end() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.done = true
	v.mu.Unlock()
	v.cancel()
	v.wg.Wait()
	if v.release != nil {
		v.release()
	}
}

type grantContextKey struct{}

func hostGrant(ctx context.Context) nativeGrant {
	grant, _ := ctx.Value(grantContextKey{}).(nativeGrant)
	return grant
}

// dispatch resolves, validates, admits and runs one nested named-service call.
func (c *composition) dispatch(ctx context.Context, caller extension.CallContext, grant nativeGrant, component *extension.CompiledComponent, targets map[string]*capabilityValue, bindingName, methodName string, arguments json.RawMessage) (extension.Result, error) {
	var binding *extension.CompiledBinding
	for i := range component.Requires {
		if component.Requires[i].Name == bindingName {
			binding = component.Requires[i]
			break
		}
	}
	if binding == nil {
		return extension.Result{}, fmt.Errorf("component %q: binding %q is not declared: %w", component.ID, bindingName, ErrComposition)
	}
	method, ok := binding.Methods[methodName]
	if !ok {
		return extension.Result{}, fmt.Errorf("binding %q: method %q is not declared: %w", bindingName, methodName, ErrComposition)
	}
	if grant.pure && method.Mode != extension.ModePure {
		return extension.Result{}, fmt.Errorf("binding %q: pure callback cannot invoke %s method %q: %w", bindingName, method.Mode, methodName, ErrComposition)
	}
	arguments = append(json.RawMessage(nil), arguments...)
	if err := extension.ValidateArguments(binding.Methods, methodName, arguments); err != nil {
		return extension.Result{}, fmt.Errorf("%w: %w", ErrComposition, err)
	}
	if binding.Host {
		handler, ok := c.hostHandler(binding.CapabilityID)
		if !ok {
			return extension.Result{}, fmt.Errorf("unknown host service %q: %w", binding.CapabilityID, ErrComposition)
		}
		if err := ctx.Err(); err != nil {
			return extension.Result{}, err
		}
		hostCaller := caller
		hostCaller.Call = nil
		hostCtx := context.WithValue(ctx, grantContextKey{}, nativeGrant{permissions: append([]harness.PermissionRequest(nil), grant.permissions...)})
		result, err := handler(hostCtx, hostCaller, arguments)
		if err != nil {
			return extension.Result{}, err
		}
		result = ownResult(result)
		if err := extension.ValidateResult(binding.Methods, methodName, result); err != nil {
			return extension.Result{}, fmt.Errorf("host service %q: %w", binding.CapabilityID, err)
		}
		return result, nil
	}
	provider := targets[bindingName]
	if provider == nil {
		return extension.Result{}, fmt.Errorf("binding %q: provider is unavailable: %w", bindingName, ErrClosed)
	}
	pure := grant.pure || method.Mode == extension.ModePure
	childIdentity := callIdentity{
		session:      harness.SessionIdentity{SessionID: caller.SessionID},
		operationID:  caller.OperationID,
		invocationID: caller.InvocationID,
		purpose:      caller.Purpose,
		requestID:    caller.RequestID,
		toolCallID:   caller.ToolCallID,
		effectID:     caller.EffectID,
		revision:     caller.Revision,
		caller:       extension.CallerIdentity{ExtensionID: caller.ExtensionID, ComponentID: caller.ComponentID, InstanceID: caller.InstanceID},
		grant:        nativeGrant{permissions: grant.permissions, pure: pure, lifecycle: grant.lifecycle},
	}
	return c.invokeProvider(ctx, provider, binding.Methods, methodName, arguments, nil, childIdentity, grant.lifecycle)
}

// invokeProvider runs one declared provider method under the provider scope's
// guard: the same argument/result validation, private pure view and result
// ownership as a nested declared call. Host-initiated resolver and
// configuration-edit calls use this one path with the provider's own compiled
// methods; a direct caller may override the captured section, never the
// authority.
func (c *composition) invokeProvider(ctx context.Context, provider *capabilityValue, methods extension.MethodSet, method string, arguments, section json.RawMessage, identity callIdentity, lifecycle bool) (extension.Result, error) {
	if provider == nil {
		return extension.Result{}, ErrClosed
	}
	contract, ok := provider.capability.Methods[method]
	if !ok {
		return extension.Result{}, fmt.Errorf("capability %q: method %q is not declared: %w", provider.capability.ID, method, ErrComposition)
	}
	if identity.grant.pure && contract.Mode != extension.ModePure {
		return extension.Result{}, fmt.Errorf("capability %q: pure callback cannot invoke %s method %q: %w", provider.capability.ID, contract.Mode, method, ErrComposition)
	}
	arguments = append(json.RawMessage(nil), arguments...)
	if err := extension.ValidateArguments(methods, method, arguments); err != nil {
		return extension.Result{}, fmt.Errorf("%w: %w", ErrComposition, err)
	}
	callCtx, release, err := provider.scope.enterFor(ctx, lifecycle)
	if err != nil {
		return extension.Result{}, err
	}
	if identity.revision == "" {
		identity.revision = provider.scope.info.Revision
	}
	child, childCall := c.newView(provider, callCtx, identity)
	child.release = release
	defer child.end()
	if section != nil {
		childCall.Section = append(json.RawMessage(nil), section...)
	}
	handler := provider.implementation.Service[method]
	if handler == nil {
		return extension.Result{}, fmt.Errorf("capability %q: method %q has no implementation: %w", provider.capability.ID, method, ErrComposition)
	}
	result, err := handler(child.ctx, childCall, arguments)
	if err != nil {
		return extension.Result{}, err
	}
	result = ownResult(result)
	if err := extension.ValidateResult(methods, method, result); err != nil {
		return extension.Result{}, fmt.Errorf("capability %q: %w", provider.capability.ID, err)
	}
	return result, nil
}

func ownResult(result extension.Result) extension.Result {
	out := extension.Result{Value: append(json.RawMessage(nil), result.Value...)}
	if result.Failure != nil {
		failure := *result.Failure
		failure.Data = append(json.RawMessage(nil), result.Failure.Data...)
		failure.Partial = append(json.RawMessage(nil), result.Failure.Partial...)
		out.Failure = &failure
	}
	return out
}
