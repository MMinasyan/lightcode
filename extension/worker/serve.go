package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
)

var errStopped = errors.New("worker: stopped")

// Serve runs one Go authoring adapter over the supplied streams until the
// activation ends. The first host request must be an activation with the exact
// extension contract version. The native's own declaration and metadata
// callbacks are compiled and validated before serving.
func Serve(ctx context.Context, native extension.Native, in io.Reader, out io.Writer) error {
	if in == nil || out == nil {
		return errors.New("worker: serve requires an input and an output stream")
	}
	compiled, err := extension.CompileNative(native)
	if err != nil {
		return err
	}
	owned, ok := compiled.Native(native.Declaration.ID)
	if !ok {
		return fmt.Errorf("worker: compiled declaration %q is missing", native.Declaration.ID)
	}
	s := &server{
		ctx:       ctx,
		native:    owned,
		compiled:  compiled,
		reader:    NewReader(in),
		writer:    NewWriter(out),
		instances: make(map[string]*openInstance),
		reserved:  make(map[string]bool),
		requests:  make(map[string]context.CancelFunc),
		pending:   make(map[string]chan hostcallOutcome),
		done:      make(chan struct{}),
		fatal:     make(chan error, 1),
	}
	return s.run()
}

type server struct {
	ctx      context.Context
	native   extension.Native
	compiled *extension.Compiled
	reader   *Reader
	writer   *Writer

	next atomic.Uint64

	mu        sync.Mutex
	stopped   bool
	done      chan struct{}
	instances map[string]*openInstance
	reserved  map[string]bool
	requests  map[string]context.CancelFunc
	pending   map[string]chan hostcallOutcome
	fatal     chan error
	handlers  sync.WaitGroup
}

type openInstance struct {
	component string
	value     extension.Instance
	ctx       context.Context
	cancel    context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

type hostcallOutcome struct {
	result extension.Result
	err    error
}

type frameResult struct {
	frame Frame
	err   error
}

type protocolFailure struct {
	err error
}

func (e protocolFailure) Error() string { return e.err.Error() }

func badRoute(format string, args ...any) error {
	return protocolFailure{err: fmt.Errorf(format, args...)}
}

func decodeFailure(what string, err error) error {
	return protocolFailure{err: fmt.Errorf("worker: %s: %w", what, err)}
}

func (s *server) run() error {
	frames := make(chan frameResult)
	go s.readLoop(frames)
	first := true
	for {
		select {
		case <-s.ctx.Done():
			s.shutdown()
			return s.ctx.Err()
		case err := <-s.fatal:
			s.shutdown()
			return err
		case event := <-frames:
			if event.err != nil {
				s.shutdown()
				if errors.Is(event.err, io.EOF) {
					return nil
				}
				return event.err
			}
			if first {
				first = false
				if err := s.activate(event.frame); err != nil {
					s.shutdown()
					return err
				}
				continue
			}
			if err := s.handleFrame(event.frame); err != nil {
				s.shutdown()
				return err
			}
		}
	}
}

func (s *server) readLoop(frames chan<- frameResult) {
	for {
		frame, err := s.reader.ReadFrame()
		select {
		case frames <- frameResult{frame: frame, err: err}:
		case <-s.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (s *server) shutdown() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.done)
	for _, cancel := range s.requests {
		cancel()
	}
	for _, instance := range s.instances {
		instance.cancel()
	}
	s.mu.Unlock()
	s.handlers.Wait()
}

func (s *server) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *server) activate(frame Frame) error {
	if frame.Kind != KindRequest {
		return fmt.Errorf("worker: first frame kind %q is not an activation request", frame.Kind)
	}
	if frame.ID == "" {
		return errors.New("worker: activation request without id")
	}
	if frame.Method != MethodActivate {
		return s.rejectActivation(frame.ID, fmt.Errorf("worker: first request method %q is not %q", frame.Method, MethodActivate))
	}
	var params ActivateParams
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		return s.rejectActivation(frame.ID, fmt.Errorf("worker: activation params are invalid: %w", err))
	}
	if params.ContractVersion != extension.ContractVersion {
		return s.rejectActivation(frame.ID, fmt.Errorf("worker: contract version %q is not %q", params.ContractVersion, extension.ContractVersion))
	}
	encoded, err := json.Marshal(ActivateResult{Declaration: NewDeclaration(s.native.Declaration)})
	if err != nil {
		s.writeError(frame.ID, &Error{Class: ClassInternal, Message: "activation result could not be encoded"})
		return err
	}
	if err := s.write(Frame{Kind: KindResponse, ID: frame.ID, Result: encoded}); err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			s.writeError(frame.ID, &Error{Class: ClassInternal, Message: "activation result exceeds the frame limit"})
		}
		return err
	}
	return nil
}

func (s *server) rejectActivation(id string, cause error) error {
	failure := &Error{Class: ClassIncompatible, Message: cause.Error()}
	s.writeError(id, failure)
	return failure
}

func (s *server) handleFrame(frame Frame) error {
	switch frame.Kind {
	case KindRequest:
		return s.admitRequest(frame)
	case KindResponse:
		return s.routeResponse(frame)
	case KindCancel:
		s.cancelRequest(frame.ID)
		return nil
	case KindProgress:
		return nil
	default:
		return fmt.Errorf("worker: unknown frame kind %q", frame.Kind)
	}
}

// admitRequest performs request admission and instance lookup in frame order,
// then runs the callback concurrently. A decode or routing failure ends the
// activation; an author callback error is the call's ordinary error.
func (s *server) admitRequest(frame Frame) error {
	if frame.ID == "" {
		return errors.New("worker: request without id")
	}
	if frame.Method == "" {
		return fmt.Errorf("worker: request %q without a method", frame.ID)
	}
	s.mu.Lock()
	_, duplicated := s.requests[frame.ID]
	s.mu.Unlock()
	if duplicated {
		return fmt.Errorf("worker: duplicate request id %q", frame.ID)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	switch frame.Method {
	case MethodActivate:
		cancel()
		return badRoute("worker: activation already completed")
	case MethodOpen:
		var request OpenParams
		if err := json.Unmarshal(frame.Params, &request); err != nil {
			cancel()
			return decodeFailure("open params", err)
		}
		if request.InstanceID == "" {
			cancel()
			return badRoute("worker: open requires an instance id")
		}
		declaration, ok := s.compiled.Declaration(s.native.Declaration.ID)
		if !ok {
			cancel()
			return badRoute("worker: declaration %q is missing", s.native.Declaration.ID)
		}
		if _, ok := declaration.Components[request.ComponentID]; !ok {
			cancel()
			return badRoute("worker: open names undeclared component %q", request.ComponentID)
		}
		s.mu.Lock()
		_, exists := s.instances[request.InstanceID]
		reserved := s.reserved[request.InstanceID]
		if !exists && !reserved {
			s.reserved[request.InstanceID] = true
		}
		s.mu.Unlock()
		if exists || reserved {
			cancel()
			return badRoute("worker: duplicate instance id %q", request.InstanceID)
		}
		s.launch(frame.ID, ctx, cancel, func(ctx context.Context) (json.RawMessage, error) {
			return s.open(ctx, request)
		})
	case MethodCall:
		var request CallParams
		if err := json.Unmarshal(frame.Params, &request); err != nil {
			cancel()
			return decodeFailure("call params", err)
		}
		if request.Method == "" {
			cancel()
			return badRoute("worker: call without a method")
		}
		switch request.Target.Kind {
		case TargetMetadata:
			capability, describe, err := s.admitMetadata(request)
			if err != nil {
				cancel()
				return err
			}
			s.launch(frame.ID, ctx, cancel, func(ctx context.Context) (json.RawMessage, error) {
				return s.metadata(ctx, request, capability, describe)
			})
		case TargetInstance:
			instance, capability, implementation, err := s.admitInstanceCall(request)
			if err != nil {
				cancel()
				return err
			}
			s.launch(frame.ID, ctx, cancel, func(ctx context.Context) (json.RawMessage, error) {
				defer instance.wg.Done()
				return s.instanceCall(ctx, request, instance, capability, implementation)
			})
		default:
			cancel()
			return badRoute("worker: unknown call target kind %q", request.Target.Kind)
		}
	case MethodDispose:
		var request DisposeParams
		if err := json.Unmarshal(frame.Params, &request); err != nil {
			cancel()
			return decodeFailure("dispose params", err)
		}
		instance, err := s.admitDispose(request)
		if err != nil {
			cancel()
			return err
		}
		s.launch(frame.ID, ctx, cancel, func(ctx context.Context) (json.RawMessage, error) {
			return s.dispose(ctx, instance, request)
		})
	default:
		cancel()
		return badRoute("worker: unknown request method %q", frame.Method)
	}
	return nil
}

func (s *server) launch(id string, ctx context.Context, cancel context.CancelFunc, run func(context.Context) (json.RawMessage, error)) {
	s.mu.Lock()
	s.requests[id] = cancel
	s.handlers.Add(1)
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.requests, id)
			s.mu.Unlock()
			cancel()
			s.handlers.Done()
		}()
		result, err := run(ctx)
		var failure protocolFailure
		if errors.As(err, &failure) {
			s.fail(failure.err)
			return
		}
		s.respond(id, result, err)
	}()
}

func (s *server) cancelRequest(id string) {
	s.mu.Lock()
	cancel := s.requests[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) admitMetadata(request CallParams) (*extension.CompiledCapability, extension.ToolDescribe, error) {
	capability, ok := s.compiled.Capability(request.Target.CapabilityID)
	if !ok {
		return nil, nil, badRoute("worker: metadata names undeclared capability %q", request.Target.CapabilityID)
	}
	if capability.Kind != extension.KindTool {
		return nil, nil, badRoute("worker: metadata capability %q is not a tool", request.Target.CapabilityID)
	}
	describe, ok := s.native.Describe[request.Target.CapabilityID]
	if !ok {
		return nil, nil, badRoute("worker: capability %q has no metadata callback", request.Target.CapabilityID)
	}
	if request.Method != "describe" {
		return nil, nil, badRoute("worker: metadata method %q is not describe", request.Method)
	}
	return capability, describe, nil
}

func (s *server) admitInstanceCall(request CallParams) (*openInstance, *extension.CompiledCapability, extension.Implementation, error) {
	s.mu.Lock()
	instance := s.instances[request.Target.InstanceID]
	s.mu.Unlock()
	if instance == nil {
		return nil, nil, extension.Implementation{}, badRoute("worker: call names unknown instance %q", request.Target.InstanceID)
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.closed {
		return nil, nil, extension.Implementation{}, badRoute("worker: call names a disposing or disposed instance %q", request.Target.InstanceID)
	}
	capability, ok := s.compiled.Capability(request.Target.CapabilityID)
	if !ok {
		return nil, nil, extension.Implementation{}, badRoute("worker: call names undeclared capability %q", request.Target.CapabilityID)
	}
	if capability.Component == nil || capability.Component.ID != instance.component {
		return nil, nil, extension.Implementation{}, badRoute("worker: capability %q is not provided by instance %q", capability.ID, request.Target.InstanceID)
	}
	implementation, ok := instance.value.Capabilities[capability.ID]
	if !ok {
		return nil, nil, extension.Implementation{}, badRoute("worker: instance %q does not supply capability %q", request.Target.InstanceID, capability.ID)
	}
	instance.wg.Add(1)
	return instance, capability, implementation, nil
}

func (s *server) admitDispose(request DisposeParams) (*openInstance, error) {
	s.mu.Lock()
	instance := s.instances[request.InstanceID]
	s.mu.Unlock()
	if instance == nil {
		return nil, badRoute("worker: dispose names unknown instance %q", request.InstanceID)
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.closed {
		return nil, badRoute("worker: dispose names a disposing or disposed instance %q", request.InstanceID)
	}
	instance.closed = true
	return instance, nil
}

func (s *server) open(ctx context.Context, request OpenParams) (json.RawMessage, error) {
	openCtx, cancel := context.WithCancel(ctx)
	instanceContext := extension.InstanceContext{
		ExtensionID: s.native.Declaration.ID,
		ComponentID: request.ComponentID,
		Version:     s.native.Declaration.Version,
		InstanceID:  request.InstanceID,
		Lifetime:    request.Scope.Kind,
		Scope:       request.Scope,
		Revision:    request.Context.Revision,
		Section:     append(json.RawMessage(nil), request.Section...),
		Call:        s.callFunc(openCtx, request.Context),
	}
	instance, err := s.native.Open(openCtx, instanceContext)
	cancel()
	if err != nil {
		s.releaseReservation(request.InstanceID)
		return nil, err
	}
	instance = instance.Owned()
	declaration, ok := s.compiled.Declaration(s.native.Declaration.ID)
	if !ok {
		s.closeInvalid(ctx, instance, request)
		s.releaseReservation(request.InstanceID)
		return nil, fmt.Errorf("%w: declaration %q is missing", harness.ErrInvalid, s.native.Declaration.ID)
	}
	if err := declaration.ValidateInstance(request.ComponentID, instance); err != nil {
		s.closeInvalid(ctx, instance, request)
		s.releaseReservation(request.InstanceID)
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalid, err)
	}
	if instance.Capabilities == nil {
		instance.Capabilities = map[string]extension.Implementation{}
	}
	instanceCtx, instanceCancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	delete(s.reserved, request.InstanceID)
	if s.stopped {
		s.mu.Unlock()
		instanceCancel()
		return nil, errStopped
	}
	if _, exists := s.instances[request.InstanceID]; exists {
		s.mu.Unlock()
		instanceCancel()
		return nil, badRoute("worker: duplicate instance id %q", request.InstanceID)
	}
	s.instances[request.InstanceID] = &openInstance{
		component: request.ComponentID,
		value:     instance,
		ctx:       instanceCtx,
		cancel:    instanceCancel,
	}
	s.mu.Unlock()
	return json.RawMessage("null"), nil
}

func (s *server) closeInvalid(ctx context.Context, instance extension.Instance, request OpenParams) {
	if instance.Close == nil {
		return
	}
	closeCtx, cancel := context.WithCancel(ctx)
	callContext := request.Context.callContext()
	callContext.Call = s.callFunc(closeCtx, request.Context)
	_ = instance.Close(closeCtx, callContext)
	cancel()
}

func (s *server) releaseReservation(id string) {
	s.mu.Lock()
	delete(s.reserved, id)
	s.mu.Unlock()
}

func (s *server) metadata(ctx context.Context, request CallParams, capability *extension.CompiledCapability, describe extension.ToolDescribe) (json.RawMessage, error) {
	input, err := DecodeDescribeParams(json.RawMessage(request.Arguments))
	if err != nil {
		return nil, decodeFailure("describe arguments", err)
	}
	description, err := describe(ctx, input)
	if err != nil {
		return nil, err
	}
	value, err := EncodeDescribeResult(description)
	if err != nil {
		return nil, err
	}
	return s.bareResult(capability, request.Method, value)
}

func (s *server) instanceCall(ctx context.Context, request CallParams, instance *openInstance, capability *extension.CompiledCapability, implementation extension.Implementation) (json.RawMessage, error) {
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(instance.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	return s.dispatch(callCtx, capability, implementation, request)
}

func (s *server) dispatch(ctx context.Context, capability *extension.CompiledCapability, implementation extension.Implementation, request CallParams) (json.RawMessage, error) {
	switch capability.Kind {
	case extension.KindService:
		if _, declared := capability.Methods[request.Method]; !declared {
			return nil, badRoute("worker: service %q has no method %q", capability.ID, request.Method)
		}
		handler, ok := implementation.Service[request.Method]
		if !ok {
			return nil, badRoute("worker: service %q has no method %q", capability.ID, request.Method)
		}
		scopeCtx, endScope := context.WithCancel(ctx)
		callContext := request.Context.callContext()
		callContext.Call = s.callFunc(scopeCtx, request.Context)
		result, err := handler(scopeCtx, callContext, json.RawMessage(request.Arguments))
		endScope()
		if err != nil {
			return nil, err
		}
		return s.callResult(capability, request.Method, result)
	case extension.KindTool:
		return s.dispatchTool(ctx, capability, implementation.Tool, request)
	case extension.KindHook:
		return s.dispatchHook(ctx, capability, implementation, request)
	case extension.KindStrategy:
		return s.dispatchStrategy(capability, implementation.Strategy, request)
	default:
		return nil, badRoute("worker: capability %q has unknown kind %q", capability.ID, capability.Kind)
	}
}

// callResult validates one named-service or effectful-hook result against its
// fixed method schema and encodes the common result envelope.
func (s *server) callResult(capability *extension.CompiledCapability, method string, result extension.Result) (json.RawMessage, error) {
	if err := extension.ValidateResult(capability.Methods, method, result); err != nil {
		return nil, err
	}
	return EncodeServiceResult(result)
}

// bareResult validates one fixed method result against its schema and returns
// its semantic value without the named-service result envelope.
func (s *server) bareResult(capability *extension.CompiledCapability, method string, value json.RawMessage) (json.RawMessage, error) {
	if err := extension.ValidateResult(capability.Methods, method, extension.Result{Value: value}); err != nil {
		return nil, err
	}
	return value, nil
}

func (s *server) dispatchTool(ctx context.Context, capability *extension.CompiledCapability, tool extension.Tool, request CallParams) (json.RawMessage, error) {
	if tool == nil {
		return nil, badRoute("worker: tool %q has no implementation", request.Target.CapabilityID)
	}
	var value json.RawMessage
	switch request.Method {
	case "normalize":
		toolContext, call, err := DecodeNormalizeParams(json.RawMessage(request.Arguments))
		if err != nil {
			return nil, decodeFailure("normalize arguments", err)
		}
		scopeCtx, endScope := context.WithCancel(ctx)
		toolContext.Calls.Call = s.callFunc(scopeCtx, request.Context)
		arguments, err := tool.Normalize(toolContext, call)
		endScope()
		if err != nil {
			return nil, err
		}
		value, err = EncodeArguments(arguments)
		if err != nil {
			return nil, err
		}
	case "prepare":
		toolContext, call, err := DecodePrepareParams(json.RawMessage(request.Arguments))
		if err != nil {
			return nil, decodeFailure("prepare arguments", err)
		}
		scopeCtx, endScope := context.WithCancel(ctx)
		toolContext.Calls.Call = s.callFunc(scopeCtx, request.Context)
		plan := tool.Prepare(scopeCtx, toolContext, call)
		endScope()
		value, err = EncodePrepareResult(plan)
		if err != nil {
			return nil, err
		}
	case "execute":
		toolContext, plan, err := DecodeExecuteParams(json.RawMessage(request.Arguments))
		if err != nil {
			return nil, decodeFailure("execute arguments", err)
		}
		scopeCtx, endScope := context.WithCancel(ctx)
		toolContext.Calls.Call = s.callFunc(scopeCtx, request.Context)
		outcome := tool.Execute(scopeCtx, toolContext, plan)
		endScope()
		value, err = EncodeExecuteResult(outcome)
		if err != nil {
			return nil, err
		}
	default:
		return nil, badRoute("worker: tool %q has no method %q", request.Target.CapabilityID, request.Method)
	}
	return s.bareResult(capability, request.Method, value)
}

func (s *server) dispatchHook(ctx context.Context, capability *extension.CompiledCapability, implementation extension.Implementation, request CallParams) (json.RawMessage, error) {
	boundary := extension.HookBoundary(request.Method)
	scopeCtx, endScope := context.WithCancel(ctx)
	callContext := request.Context.callContext()
	callContext.Call = s.callFunc(scopeCtx, request.Context)
	var value json.RawMessage
	switch boundary {
	case extension.BoundaryPrepare:
		if implementation.Preparation == nil {
			endScope()
			return nil, badRoute("worker: hook %q has no preparation callback", capability.ID)
		}
		input, err := DecodePrepareHookParams(json.RawMessage(request.Arguments))
		if err != nil {
			endScope()
			return nil, decodeFailure("prepare hook arguments", err)
		}
		consequence, err := implementation.Preparation(scopeCtx, callContext, input)
		endScope()
		if err != nil {
			return nil, err
		}
		value, err = EncodePrepareHookResult(consequence)
		if err != nil {
			return nil, err
		}
	case extension.BoundaryRequest:
		if implementation.Request == nil {
			endScope()
			return nil, badRoute("worker: hook %q has no request callback", capability.ID)
		}
		input, err := DecodeRequestHookParams(json.RawMessage(request.Arguments))
		if err != nil {
			endScope()
			return nil, decodeFailure("request hook arguments", err)
		}
		consequence, err := implementation.Request(scopeCtx, callContext, input)
		endScope()
		if err != nil {
			return nil, err
		}
		value, err = EncodeRequestHookResult(consequence)
		if err != nil {
			return nil, err
		}
	case extension.BoundaryBeforeTool:
		if implementation.BeforeTool == nil {
			endScope()
			return nil, badRoute("worker: hook %q has no before-tool callback", capability.ID)
		}
		call, err := DecodeBeforeToolHookParams(json.RawMessage(request.Arguments))
		if err != nil {
			endScope()
			return nil, decodeFailure("before-tool hook arguments", err)
		}
		arguments, err := implementation.BeforeTool(scopeCtx, callContext, call)
		endScope()
		if err != nil {
			return nil, err
		}
		value, err = EncodeArguments(arguments)
		if err != nil {
			return nil, err
		}
	case extension.BoundaryAfterTool:
		if implementation.AfterTool == nil {
			endScope()
			return nil, badRoute("worker: hook %q has no after-tool callback", capability.ID)
		}
		input, err := DecodeAfterToolHookParams(json.RawMessage(request.Arguments))
		if err != nil {
			endScope()
			return nil, decodeFailure("after-tool hook arguments", err)
		}
		content, err := implementation.AfterTool(scopeCtx, callContext, input)
		endScope()
		if err != nil {
			return nil, err
		}
		value, err = EncodeAfterToolHookResult(content)
		if err != nil {
			return nil, err
		}
	default:
		endScope()
		return nil, badRoute("worker: hook %q has unknown boundary %q", capability.ID, request.Method)
	}
	return s.bareResult(capability, request.Method, value)
}

func (s *server) dispatchStrategy(capability *extension.CompiledCapability, strategy harness.ContextStrategy, request CallParams) (json.RawMessage, error) {
	if strategy == nil {
		return nil, badRoute("worker: strategy %q has no implementation", request.Target.CapabilityID)
	}
	var value json.RawMessage
	switch request.Method {
	case "plan":
		input, err := DecodePlanParams(json.RawMessage(request.Arguments))
		if err != nil {
			return nil, decodeFailure("plan arguments", err)
		}
		output, err := strategy.Plan(input)
		if err != nil {
			return nil, err
		}
		value, err = EncodePlanResult(output)
		if err != nil {
			return nil, err
		}
	case "consume":
		input, err := DecodeConsumeParams(json.RawMessage(request.Arguments))
		if err != nil {
			return nil, decodeFailure("consume arguments", err)
		}
		output, err := strategy.Consume(input)
		if err != nil {
			return nil, err
		}
		value, err = EncodePlanResult(output)
		if err != nil {
			return nil, err
		}
	default:
		return nil, badRoute("worker: strategy %q has no method %q", request.Target.CapabilityID, request.Method)
	}
	return s.bareResult(capability, request.Method, value)
}

func (s *server) dispose(ctx context.Context, instance *openInstance, request DisposeParams) (json.RawMessage, error) {
	instance.cancel()
	instance.wg.Wait()
	closeCtx, cancel := context.WithCancel(ctx)
	callContext := request.Context.callContext()
	callContext.Call = s.callFunc(closeCtx, request.Context)
	var closeErr error
	if instance.value.Close != nil {
		closeErr = instance.value.Close(closeCtx, callContext)
	}
	cancel()
	s.mu.Lock()
	delete(s.instances, request.InstanceID)
	s.mu.Unlock()
	if closeErr != nil {
		return nil, closeErr
	}
	return json.RawMessage("null"), nil
}

func (s *server) callFunc(scope context.Context, wire Context) func(context.Context, string, string, json.RawMessage) (extension.Result, error) {
	return func(ctx context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
		return s.hostcall(ctx, scope, wire, binding, method, arguments)
	}
}

func (s *server) hostcall(ctx context.Context, scope context.Context, wire Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
	if binding == "" || method == "" {
		return extension.Result{}, &Error{Class: ClassInvalid, Message: "hostcall requires a binding and method"}
	}
	if err := scope.Err(); err != nil {
		return extension.Result{}, extension.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return extension.Result{}, err
	}
	combined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(scope, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if err := combined.Err(); err != nil {
		return extension.Result{}, err
	}
	encoded, err := json.Marshal(HostcallParams{Context: wire, Binding: binding, Method: method, Arguments: JSONBytes(arguments)})
	if err != nil {
		return extension.Result{}, err
	}
	id := s.nextRequestID()
	outcomes := make(chan hostcallOutcome, 1)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return extension.Result{}, extension.ErrClosed
	}
	s.pending[id] = outcomes
	s.mu.Unlock()
	if err := s.write(Frame{Kind: KindRequest, ID: id, Method: MethodHostcall, Params: encoded}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		if errors.Is(err, ErrFrameTooLarge) {
			return extension.Result{}, &Error{Class: ClassInternal, Message: "hostcall exceeds the frame limit"}
		}
		return extension.Result{}, err
	}
	cancelled := combined.Done()
	for {
		select {
		case outcome := <-outcomes:
			return outcome.result, serviceCallError(outcome.err)
		case <-s.done:
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			select {
			case outcome := <-outcomes:
				return outcome.result, serviceCallError(outcome.err)
			default:
			}
			return extension.Result{}, extension.ErrClosed
		case <-cancelled:
			cancelled = nil
			_ = s.write(Frame{Kind: KindCancel, ID: id})
		}
	}
}

// serviceCallError exposes one hostcall failure to the author: a failure with
// a produced partial becomes the same typed CallError native authors receive,
// wrapping the classed worker error so errors.Is keeps the cause.
func serviceCallError(err error) error {
	if err == nil {
		return nil
	}
	var failure *Error
	if errors.As(err, &failure) && len(failure.Partial) != 0 {
		return &extension.CallError{Err: failure, Partial: append(json.RawMessage(nil), failure.Partial...)}
	}
	return err
}

func (s *server) nextRequestID() string {
	return "w" + strconv.FormatUint(s.next.Add(1), 10)
}

func (s *server) routeResponse(frame Frame) error {
	if frame.ID == "" {
		return errors.New("worker: response without an id")
	}
	s.mu.Lock()
	outcomes, known := s.pending[frame.ID]
	if known {
		delete(s.pending, frame.ID)
	}
	s.mu.Unlock()
	if !known {
		return fmt.Errorf("worker: response to unknown request %q", frame.ID)
	}
	hasResult := frame.Result != nil
	hasError := frame.Error != nil
	if hasResult == hasError {
		return fmt.Errorf("worker: response %q must carry exactly one of result or error", frame.ID)
	}
	if hasError {
		if !frame.Error.Class.valid() {
			return fmt.Errorf("worker: response %q carries unknown error class %q", frame.ID, frame.Error.Class)
		}
		if len(frame.Error.Partial) != 0 && !validPresentJSON(json.RawMessage(frame.Error.Partial)) {
			return fmt.Errorf("worker: response %q carries an invalid error partial", frame.ID)
		}
		outcomes <- hostcallOutcome{err: frame.Error}
		return nil
	}
	result, err := DecodeServiceResult(frame.Result)
	if err != nil {
		return fmt.Errorf("worker: response %q: %w", frame.ID, err)
	}
	if err := extension.ValidateResultEnvelope(result); err != nil {
		return fmt.Errorf("worker: response %q: %w", frame.ID, err)
	}
	if result.Failure != nil {
		if !validPresentJSON(result.Failure.Data) || !validPresentJSON(result.Failure.Partial) {
			return fmt.Errorf("worker: response %q carries invalid failure payload", frame.ID)
		}
	} else if !validPresentJSON(result.Value) {
		return fmt.Errorf("worker: response %q carries an invalid success value", frame.ID)
	}
	outcomes <- hostcallOutcome{result: result}
	return nil
}

func validPresentJSON(raw json.RawMessage) bool {
	return raw == nil || json.Valid(raw)
}

func (s *server) respond(id string, result json.RawMessage, callErr error) {
	if callErr != nil {
		s.writeError(id, classifyError(callErr))
		return
	}
	if err := s.write(Frame{Kind: KindResponse, ID: id, Result: result}); err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			s.writeError(id, &Error{Class: ClassInternal, Message: "result exceeds the frame limit"})
			return
		}
		if !errors.Is(err, errStopped) {
			s.fail(err)
		}
	}
}

func (s *server) writeError(id string, failure *Error) {
	if failure == nil {
		failure = &Error{Class: ClassInternal}
	}
	if err := s.write(Frame{Kind: KindResponse, ID: id, Error: failure}); err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			small := &Error{Class: ClassInternal, Message: "error response exceeds the frame limit"}
			if err := s.write(Frame{Kind: KindResponse, ID: id, Error: small}); err != nil && !errors.Is(err, errStopped) {
				s.fail(err)
			}
			return
		}
		if !errors.Is(err, errStopped) {
			s.fail(err)
		}
	}
}

func (s *server) write(frame Frame) error {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return errStopped
	}
	return s.writer.WriteFrame(frame)
}
