package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// native returns the extension.Native binding that enters the ordinary
// composition path for one executable extension.
func (s *executableSupervisor) native(executable executableExtension) (extension.Native, error) {
	declaration := executable.Declaration
	var compiled *extension.Compiled
	describes := make(map[string]extension.ToolDescribe)
	for _, capability := range declaration.Capabilities {
		if capability.Kind != extension.KindTool {
			continue
		}
		capabilityID := capability.ID
		describes[capabilityID] = func(ctx context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
			return s.describe(declaration, compiled, capabilityID, ctx, input)
		}
	}
	native := extension.Native{
		Declaration: declaration,
		Describe:    describes,
		Open: func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			return s.open(executable, compiled, ctx, instance)
		},
	}
	compiled, err := extension.CompileNative(native)
	if err != nil {
		return extension.Native{}, err
	}
	return native, nil
}

// closedCall is the metadata callback's context: pure description has no
// binding authority, so a worker hostcall from it is closed.
func closedCall(context.Context, string, string, json.RawMessage) (extension.Result, error) {
	return extension.Result{}, extension.ErrClosed
}

// describe serves one tool's activation-level metadata request.
func (s *executableSupervisor) describe(declaration extension.Declaration, compiled *extension.Compiled, capabilityID string, ctx context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
	activation, err := s.live(declaration.ID)
	if err != nil {
		return extension.ToolDescription{}, err
	}
	capability, ok := compiled.Capability(capabilityID)
	if !ok {
		return extension.ToolDescription{}, fmt.Errorf("executable extension %q: capability %q is not compiled: %w", declaration.ID, capabilityID, extension.ErrClosed)
	}
	params, err := worker.EncodeDescribeParams(input)
	if err != nil {
		return extension.ToolDescription{}, err
	}
	call := extension.CallContext{
		ExtensionID: declaration.ID,
		Lifetime:    extension.LifetimeHarness,
		Revision:    input.Revision,
		Section:     input.Config,
		SessionID:   input.Session.SessionID,
	}
	validator := fixedResult(capability.Methods, "describe", typed(worker.DecodeDescribeResult))
	outcome := activation.request(ctx, worker.MethodCall, func(token string) (any, error) {
		return worker.CallParams{
			Target:    worker.Target{Kind: worker.TargetMetadata, CapabilityID: capabilityID},
			Context:   worker.NewContext(token, call),
			Method:    "describe",
			Arguments: worker.JSONBytes(params),
		}, nil
	}, closedCall, validator)
	if outcome.err != nil {
		return extension.ToolDescription{}, outcome.err
	}
	if outcome.failure != nil {
		return extension.ToolDescription{}, outcome.failure
	}
	return outcome.value.(extension.ToolDescription), nil
}

// open confirms one component's worker instance and supplies the fixed typed
// implementations for exactly its declared capabilities.
func (s *executableSupervisor) open(executable executableExtension, compiled *extension.Compiled, ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
	activation, err := s.live(executable.Declaration.ID)
	if err != nil {
		return extension.Instance{}, err
	}
	outcome := activation.request(ctx, worker.MethodOpen, func(token string) (any, error) {
		return worker.OpenParams{
			InstanceID:  instance.InstanceID,
			ComponentID: instance.ComponentID,
			Scope:       instance.Scope,
			Context:     worker.NewInstanceContext(token, instance),
			Section:     worker.JSONBytes(instance.Section),
		}, nil
	}, executableCall(instance.Call), validateNull)
	if err := requestError(outcome); err != nil {
		return extension.Instance{}, err
	}
	component, ok := componentFor(executable.Declaration, instance.ComponentID)
	if !ok {
		return extension.Instance{}, fmt.Errorf("executable extension %q: open component %q is not declared: %w", executable.Declaration.ID, instance.ComponentID, extension.ErrClosed)
	}
	binding := &executableBinding{
		supervisor:  s,
		declaration: executable.Declaration,
		compiled:    compiled,
		instanceID:  instance.InstanceID,
		componentID: instance.ComponentID,
		lifetime:    instance.Lifetime,
		scope:       instance.Scope,
		revision:    instance.Revision,
		section:     append(json.RawMessage(nil), instance.Section...),
	}
	capabilities := make(map[string]extension.Implementation, len(component.Provides))
	for _, id := range component.Provides {
		capability, ok := capabilityFor(executable.Declaration, id)
		if !ok {
			return extension.Instance{}, fmt.Errorf("executable extension %q: capability %q is not declared: %w", executable.Declaration.ID, id, extension.ErrClosed)
		}
		capabilities[id] = binding.implementation(capability)
	}
	return extension.Instance{Capabilities: capabilities, Close: binding.dispose}, nil
}

// validateNull accepts only the protocol-level null result of open/dispose.
func validateNull(raw json.RawMessage) (any, error) {
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("result must be null")
	}
	return nil, nil
}

// fixedResult validates one fixed method's bare success bytes against the
// compiled method schema, then decodes the semantic value.
func fixedResult(methods extension.MethodSet, method string, decode func(json.RawMessage) (any, error)) func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		if err := extension.ValidateResult(methods, method, extension.Result{Value: raw}); err != nil {
			return nil, err
		}
		return decode(raw)
	}
}

// typed adapts one typed codec to the request validator signature.
func typed[T any](decode func(json.RawMessage) (T, error)) func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		value, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return value, nil
	}
}

// envelopeResult validates one named-service or effectful-hook result envelope
// against the compiled method schema, then decodes it.
func envelopeResult(methods extension.MethodSet, method string, decode func(extension.Result) (any, error)) func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		result, err := worker.DecodeServiceResult(raw)
		if err != nil {
			return nil, err
		}
		if err := extension.ValidateResult(methods, method, result); err != nil {
			return nil, err
		}
		return decode(result)
	}
}

func componentFor(declaration extension.Declaration, componentID string) (extension.Component, bool) {
	for _, component := range declaration.Components {
		if component.ID == componentID {
			return component, true
		}
	}
	return extension.Component{}, false
}

func capabilityFor(declaration extension.Declaration, capabilityID string) (extension.Capability, bool) {
	for _, capability := range declaration.Capabilities {
		if capability.ID == capabilityID {
			return capability, true
		}
	}
	return extension.Capability{}, false
}

// executableBinding is one opened instance's host-side binding.
type executableBinding struct {
	supervisor  *executableSupervisor
	declaration extension.Declaration
	compiled    *extension.Compiled
	instanceID  string
	componentID string
	lifetime    extension.Lifetime
	scope       extension.ScopeIdentity
	revision    string
	section     json.RawMessage
}

func (b *executableBinding) callContext() extension.CallContext {
	return extension.CallContext{
		ExtensionID: b.declaration.ID,
		ComponentID: b.componentID,
		InstanceID:  b.instanceID,
		Lifetime:    b.lifetime,
		Scope:       b.scope,
		Revision:    b.revision,
		Section:     append(json.RawMessage(nil), b.section...),
	}
}

// methods returns one capability's compiled method set.
func (b *executableBinding) methods(capabilityID string) extension.MethodSet {
	capability, ok := b.compiled.Capability(capabilityID)
	if !ok {
		return nil
	}
	return capability.Methods
}

// implementation returns the fixed typed binding for one declared capability.
func (b *executableBinding) implementation(capability extension.Capability) extension.Implementation {
	methods := b.methods(capability.ID)
	switch capability.Kind {
	case extension.KindService:
		handlers := make(map[string]extension.Handler, len(capability.Methods))
		for _, method := range capability.Methods {
			methodName := method.Name
			handlers[methodName] = func(ctx context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
				validator := envelopeResult(methods, methodName, func(result extension.Result) (any, error) { return result, nil })
				outcome := b.instanceCall(ctx, call, capability.ID, methodName, arguments, validator)
				if err := requestError(outcome); err != nil {
					return extension.Result{}, err
				}
				return outcome.value.(extension.Result), nil
			}
		}
		return extension.Implementation{Service: handlers}
	case extension.KindTool:
		return extension.Implementation{Tool: &executableTool{binding: b, capabilityID: capability.ID, methods: methods}}
	case extension.KindHook:
		implementation := extension.Implementation{}
		for _, hook := range capability.Hooks {
			switch hook.Boundary {
			case extension.BoundaryPrepare:
				implementation.Preparation = func(ctx context.Context, call extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
					params, err := worker.EncodePrepareHookParams(input)
					if err != nil {
						return extension.PreparationConsequence{}, err
					}
					validator := fixedResult(methods, string(extension.BoundaryPrepare), typed(worker.DecodePrepareHookResult))
					outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryPrepare), params, validator)
					if err := requestError(outcome); err != nil {
						return extension.PreparationConsequence{}, err
					}
					return outcome.value.(extension.PreparationConsequence), nil
				}
			case extension.BoundaryRequest:
				effectful := hook.Mode == extension.HookModeEffectful
				implementation.Request = func(ctx context.Context, call extension.CallContext, input harness.RequestHookInput) (harness.RequestConsequence, error) {
					params, err := worker.EncodeRequestHookParams(input)
					if err != nil {
						return harness.RequestConsequence{}, err
					}
					if effectful {
						validator := func(raw json.RawMessage) (any, error) {
							result, err := worker.DecodeServiceResult(raw)
							if err != nil {
								return nil, err
							}
							if err := extension.ValidateResult(methods, string(extension.BoundaryRequest), result); err != nil {
								return nil, err
							}
							consequence, failure, err := worker.DecodeRequestHookOutcome(raw)
							if err != nil {
								return nil, err
							}
							if failure != nil {
								return *failure, nil
							}
							return consequence, nil
						}
						outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryRequest), params, validator)
						if err := effectfulError(outcome); err != nil {
							return harness.RequestConsequence{}, err
						}
						if failure, ok := outcome.value.(worker.HookFailure); ok {
							return harness.RequestConsequence{}, hookFailureError(&failure)
						}
						return outcome.value.(harness.RequestConsequence), nil
					}
					validator := fixedResult(methods, string(extension.BoundaryRequest), typed(worker.DecodeRequestHookResult))
					outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryRequest), params, validator)
					if err := requestError(outcome); err != nil {
						return harness.RequestConsequence{}, err
					}
					return outcome.value.(harness.RequestConsequence), nil
				}
			case extension.BoundaryBeforeTool:
				effectful := hook.Mode == extension.HookModeEffectful
				implementation.BeforeTool = func(ctx context.Context, call extension.CallContext, toolCall model.ToolCall) (json.RawMessage, error) {
					params, err := worker.EncodeBeforeToolHookParams(toolCall)
					if err != nil {
						return nil, err
					}
					if effectful {
						validator := func(raw json.RawMessage) (any, error) {
							result, err := worker.DecodeServiceResult(raw)
							if err != nil {
								return nil, err
							}
							if err := extension.ValidateResult(methods, string(extension.BoundaryBeforeTool), result); err != nil {
								return nil, err
							}
							arguments, failure, err := worker.DecodeBeforeToolHookOutcome(raw)
							if err != nil {
								return nil, err
							}
							if failure != nil {
								return *failure, nil
							}
							return arguments, nil
						}
						outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryBeforeTool), params, validator)
						if err := effectfulError(outcome); err != nil {
							return nil, err
						}
						if failure, ok := outcome.value.(worker.HookFailure); ok {
							return nil, hookFailureError(&failure)
						}
						return outcome.value.(json.RawMessage), nil
					}
					validator := fixedResult(methods, string(extension.BoundaryBeforeTool), typed(worker.DecodeArguments))
					outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryBeforeTool), params, validator)
					if err := requestError(outcome); err != nil {
						return nil, err
					}
					return outcome.value.(json.RawMessage), nil
				}
			case extension.BoundaryAfterTool:
				implementation.AfterTool = func(ctx context.Context, call extension.CallContext, input harness.ToolOutputInput) (string, error) {
					params, err := worker.EncodeAfterToolHookParams(input)
					if err != nil {
						return "", err
					}
					validator := fixedResult(methods, string(extension.BoundaryAfterTool), typed(worker.DecodeAfterToolHookResult))
					outcome := b.instanceCall(ctx, call, capability.ID, string(extension.BoundaryAfterTool), params, validator)
					if err := requestError(outcome); err != nil {
						return "", err
					}
					return outcome.value.(string), nil
				}
			}
		}
		return implementation
	case extension.KindStrategy:
		return extension.Implementation{Strategy: &executableStrategy{binding: b, capabilityID: capability.ID, methods: methods}}
	}
	return extension.Implementation{}
}

// instanceCall sends one method call to the binding's opened instance.
func (b *executableBinding) instanceCall(ctx context.Context, call extension.CallContext, capabilityID, method string, arguments json.RawMessage, validate func(json.RawMessage) (any, error)) requestOutcome {
	activation, err := b.supervisor.live(b.declaration.ID)
	if err != nil {
		return requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
	}
	return activation.request(ctx, worker.MethodCall, func(token string) (any, error) {
		return worker.CallParams{
			Target:    worker.Target{Kind: worker.TargetInstance, InstanceID: b.instanceID, CapabilityID: capabilityID},
			Context:   worker.NewContext(token, call),
			Method:    method,
			Arguments: worker.JSONBytes(arguments),
		}, nil
	}, executableCall(call.Call), validate)
}

// dispose acknowledges one instance's cleanup.
func (b *executableBinding) dispose(ctx context.Context, call extension.CallContext) error {
	activation, err := b.supervisor.live(b.declaration.ID)
	if err != nil {
		if errors.Is(err, extension.ErrClosed) {
			return nil
		}
		return err
	}
	outcome := activation.request(ctx, worker.MethodDispose, func(token string) (any, error) {
		return worker.DisposeParams{InstanceID: b.instanceID, Context: worker.NewContext(token, call)}, nil
	}, executableCall(call.Call), validateNull)
	err = requestError(outcome)
	if errors.Is(err, extension.ErrClosed) {
		return nil
	}
	return err
}

// executableTool is one tool capability's fixed typed binding.
type executableTool struct {
	binding      *executableBinding
	capabilityID string
	methods      extension.MethodSet
}

func (t *executableTool) Normalize(context extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return t.NormalizeContext(executableBackground(), context, call)
}

func (t *executableTool) NormalizeContext(ctx context.Context, context extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	params, err := worker.EncodeNormalizeParams(context, call)
	if err != nil {
		return nil, err
	}
	validator := fixedResult(t.methods, "normalize", typed(worker.DecodeArguments))
	outcome := t.binding.instanceCall(ctx, context.Calls, t.capabilityID, "normalize", params, validator)
	if err := requestError(outcome); err != nil {
		return nil, err
	}
	return outcome.value.(json.RawMessage), nil
}

func (t *executableTool) Prepare(ctx context.Context, context extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	params, err := worker.EncodePrepareParams(context, call)
	if err != nil {
		return t.failedPlan(context, err, false)
	}
	validator := fixedResult(t.methods, "prepare", typed(worker.DecodePrepareResult))
	outcome := t.binding.instanceCall(ctx, context.Calls, t.capabilityID, "prepare", params, validator)
	if err := requestError(outcome); err != nil {
		return t.failedPlan(context, err, outcome.cancelled)
	}
	return outcome.value.(extension.ToolPlan)
}

// failedPlan turns one pure preparation transport failure into its not-started
// immediate outcome.
func (t *executableTool) failedPlan(context extension.ToolContext, err error, cancelled bool) extension.ToolPlan {
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{
		Result:   bindingToolResult(context.Calls.ToolCallID, err, cancelled),
		Evidence: harness.ExecutionNotStarted,
	}}
}

func (t *executableTool) Execute(ctx context.Context, context extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	params, err := worker.EncodeExecuteParams(context, plan)
	if err != nil {
		return failedExecution(context, harness.ExecutionNotStarted, err, false)
	}
	validator := fixedResult(t.methods, "execute", typed(worker.DecodeExecuteResult))
	outcome := t.binding.instanceCall(ctx, context.Calls, t.capabilityID, "execute", params, validator)
	if err := requestError(outcome); err != nil {
		return failedExecution(context, outcome.evidence, err, outcome.cancelled)
	}
	return outcome.value.(harness.ToolOutcome)
}

// failedExecution renders one binding failure as the tool's own outcome with
// its actual delivery knowledge. Cancellation-caused failures are interrupted,
// never ordinary errors.
func failedExecution(context extension.ToolContext, evidence harness.ExecutionEvidence, err error, cancelled bool) harness.ToolOutcome {
	if evidence == "" {
		evidence = harness.ExecutionUnknown
	}
	return harness.ToolOutcome{Result: bindingToolResult(context.Calls.ToolCallID, err, cancelled), Evidence: evidence}
}

func bindingToolResult(callID string, err error, cancelled bool) model.ToolResult {
	status := model.ResultError
	if cancelled {
		status = model.ResultInterrupted
	}
	content := "Tool call interrupted."
	if err != nil && !cancelled {
		content = err.Error()
	}
	return model.ToolResult{CallID: callID, Status: status, Content: content}
}

// executableStrategy is one context strategy capability's fixed typed binding.
type executableStrategy struct {
	binding      *executableBinding
	capabilityID string
	methods      extension.MethodSet
	ctx          context.Context
}

func (s *executableStrategy) Plan(input harness.PlanInput) (harness.PlanOutput, error) {
	params, err := worker.EncodePlanParams(input)
	if err != nil {
		return harness.PlanOutput{}, err
	}
	validator := fixedResult(s.methods, "plan", typed(worker.DecodePlanResult))
	outcome := s.binding.instanceCall(s.context(), s.binding.callContext(), s.capabilityID, "plan", params, validator)
	if err := requestError(outcome); err != nil {
		if outcome.failure == nil && (outcome.cancelled || isCancellation(outcome.err)) {
			return harness.PlanOutput{}, &harness.EffectError{Interrupted: true, Err: err}
		}
		return harness.PlanOutput{}, err
	}
	return outcome.value.(harness.PlanOutput), nil
}

func (s *executableStrategy) Consume(input harness.ConsumeInput) (harness.PlanOutput, error) {
	params, err := worker.EncodeConsumeParams(input)
	if err != nil {
		return harness.PlanOutput{}, err
	}
	validator := fixedResult(s.methods, "consume", typed(worker.DecodePlanResult))
	outcome := s.binding.instanceCall(s.context(), s.binding.callContext(), s.capabilityID, "consume", params, validator)
	if err := requestError(outcome); err != nil {
		if outcome.failure == nil && (outcome.cancelled || isCancellation(outcome.err)) {
			return harness.PlanOutput{}, &harness.EffectError{Interrupted: true, Err: err}
		}
		return harness.PlanOutput{}, err
	}
	return outcome.value.(harness.PlanOutput), nil
}

func (s *executableStrategy) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// executableBackground is the context for callbacks whose native signature
// carries no context.
func executableBackground() context.Context { return context.Background() }

// requestError returns the request's failure or nil.
func requestError(outcome requestOutcome) error {
	if outcome.err != nil {
		return outcome.err
	}
	if outcome.failure != nil {
		return outcome.failure
	}
	return nil
}

// effectfulError carries one effectful callback's binding failure as the
// shared typed knowledge value.
func effectfulError(outcome requestOutcome) error {
	err := requestError(outcome)
	if err == nil {
		return nil
	}
	return &harness.EffectError{Evidence: outcome.evidence, Err: err, Interrupted: outcome.cancelled || isCancellation(err)}
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// hookFailureError maps one accepted inbound hook failure arm.
func hookFailureError(failure *worker.HookFailure) error {
	if failure == nil {
		return nil
	}
	return &harness.EffectError{
		Evidence:    failure.Evidence,
		Err:         errors.New(failure.Message),
		Interrupted: failure.Status == worker.HookStatusInterrupted,
	}
}
