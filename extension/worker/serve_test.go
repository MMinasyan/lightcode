package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

const serveTimeout = 5 * time.Second

type peer struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *io.PipeReader
	writer *worker.Writer
	reader *worker.Reader
	errs   chan error
	stash  []worker.Frame
}

func startPeer(t *testing.T, native extension.Native) *peer {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	errs := make(chan error, 1)
	go func() {
		errs <- worker.Serve(context.Background(), native, inReader, outWriter)
	}()
	p := &peer{t: t, in: inWriter, out: outReader, writer: worker.NewWriter(inWriter), reader: worker.NewReader(outReader), errs: errs}
	t.Cleanup(func() {
		inWriter.Close()
		outReader.Close()
		outWriter.Close()
	})
	return p
}

func (p *peer) send(frame worker.Frame) {
	p.t.Helper()
	if err := p.writer.WriteFrame(frame); err != nil {
		p.t.Fatalf("writing frame: %v", err)
	}
}

func (p *peer) next() worker.Frame {
	p.t.Helper()
	if len(p.stash) != 0 {
		frame := p.stash[0]
		p.stash = p.stash[1:]
		return frame
	}
	frame, err := p.reader.ReadFrame()
	if err != nil {
		p.t.Fatalf("reading frame: %v", err)
	}
	return frame
}

func (p *peer) readResponse(id string) worker.Frame {
	p.t.Helper()
	for i, frame := range p.stash {
		if frame.Kind == worker.KindResponse && frame.ID == id {
			p.stash = append(p.stash[:i], p.stash[i+1:]...)
			return frame
		}
	}
	for {
		frame, err := p.reader.ReadFrame()
		if err != nil {
			p.t.Fatalf("reading response %q: %v", id, err)
		}
		if frame.Kind == worker.KindResponse && frame.ID == id {
			return frame
		}
		p.stash = append(p.stash, frame)
	}
}

func (p *peer) readRequest(method string) worker.Frame {
	p.t.Helper()
	for {
		frame := p.next()
		if frame.Kind == worker.KindRequest && frame.Method == method {
			return frame
		}
		p.stash = append(p.stash, frame)
	}
}

func (p *peer) readCancel(id string) worker.Frame {
	p.t.Helper()
	for {
		frame := p.next()
		if frame.Kind == worker.KindCancel && frame.ID == id {
			return frame
		}
		p.stash = append(p.stash, frame)
	}
}

func (p *peer) sendRaw(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		p.t.Fatalf("writing raw frame: %v", err)
	}
}

func (p *peer) finish() error {
	p.t.Helper()
	p.in.Close()
	select {
	case err := <-p.errs:
		return err
	case <-time.After(serveTimeout):
		p.t.Fatal("Serve did not return")
		return nil
	}
}

func (p *peer) wait() error {
	p.t.Helper()
	select {
	case err := <-p.errs:
		return err
	case <-time.After(serveTimeout):
		p.t.Fatal("Serve did not return")
		return nil
	}
}

// drain consumes every remaining output frame so a completed handler never
// blocks a stream write while the test waits for Serve's exit.
func (p *peer) drain() {
	go func() {
		for {
			if _, err := p.reader.ReadFrame(); err != nil {
				return
			}
		}
	}()
}

func (p *peer) activate() worker.ActivateResult {
	p.t.Helper()
	params, err := json.Marshal(worker.ActivateParams{ContractVersion: extension.ContractVersion})
	if err != nil {
		p.t.Fatalf("encoding activation params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "activate", Method: worker.MethodActivate, Params: params})
	frame := p.readResponse("activate")
	if frame.Error != nil {
		p.t.Fatalf("activation failed: %v", frame.Error)
	}
	var result worker.ActivateResult
	if err := json.Unmarshal(frame.Result, &result); err != nil {
		p.t.Fatalf("decoding activation result: %v", err)
	}
	return result
}

// assertBareResult proves a fixed method answered with its semantic value, not
// the named-service Result envelope.
func assertBareResult(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decoding fixed method result: %v", err)
	}
	if _, wrapped := members["value"]; wrapped {
		t.Fatalf("fixed method answered with the Result envelope: %s", raw)
	}
	if _, wrapped := members["failure"]; wrapped {
		t.Fatalf("fixed method answered with the Result envelope: %s", raw)
	}
}

// decodeFixed asserts one bare fixed method result and decodes its semantic
// value.
func decodeFixed[T any](t *testing.T, raw json.RawMessage, decode func(json.RawMessage) (T, error)) T {
	t.Helper()
	assertBareResult(t, raw)
	value, err := decode(raw)
	if err != nil {
		t.Fatalf("decoding fixed method result: %v", err)
	}
	return value
}

func testScope() extension.ScopeIdentity {
	return extension.ScopeIdentity{Kind: extension.LifetimeHarness, Revision: "rev-1", Workspace: "/workspace"}
}

func testRoutingContext(token string) worker.Context {
	return worker.NewContext(token, extension.CallContext{
		ExtensionID:  "ext",
		ComponentID:  "component",
		InstanceID:   "instance",
		Lifetime:     extension.LifetimeHarness,
		Scope:        testScope(),
		Revision:     "rev-1",
		Section:      json.RawMessage("{\n  \"section\": true\n}"),
		SessionID:    "session-1",
		OperationID:  "op-1",
		InvocationID: "inv-1",
		Purpose:      "conversation",
		RequestID:    "request-1",
		ToolCallID:   "call-1",
		EffectID:     "effect-1",
		Caller:       extension.CallerIdentity{ExtensionID: "caller", ComponentID: "caller-component", InstanceID: "caller-instance"},
	})
}

func (p *peer) open(token string) {
	p.t.Helper()
	params, err := json.Marshal(worker.OpenParams{
		InstanceID:  "instance",
		ComponentID: "component",
		Scope:       testScope(),
		Context:     testRoutingContext(token),
		Section:     worker.JSONBytes("{\n  \"captured\": true\n}"),
	})
	if err != nil {
		p.t.Fatalf("encoding open params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open", Method: worker.MethodOpen, Params: params})
	frame := p.readResponse("open")
	if frame.Error != nil {
		p.t.Fatalf("open failed: %v", frame.Error)
	}
	if string(frame.Result) != "null" {
		p.t.Fatalf("open result = %s, want null", frame.Result)
	}
}

func (p *peer) call(id string, target worker.Target, token, method string, arguments json.RawMessage) worker.Frame {
	p.t.Helper()
	params, err := json.Marshal(worker.CallParams{Target: target, Context: testRoutingContext(token), Method: method, Arguments: worker.JSONBytes(arguments)})
	if err != nil {
		p.t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: id, Method: worker.MethodCall, Params: params})
	return p.readResponse(id)
}

func metadataTarget(capability string) worker.Target {
	return worker.Target{Kind: worker.TargetMetadata, CapabilityID: capability}
}

func instanceTarget(capability string) worker.Target {
	return worker.Target{Kind: worker.TargetInstance, CapabilityID: capability, InstanceID: "instance"}
}

func baseDeclaration(capabilities ...extension.Capability) extension.Declaration {
	provides := make([]string, len(capabilities))
	for i, capability := range capabilities {
		provides[i] = capability.ID
	}
	return extension.Declaration{
		ID:           "ext",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Components: []extension.Component{{
			ID:       "component",
			Lifetime: extension.LifetimeHarness,
			Provides: provides,
		}},
		Capabilities: capabilities,
	}
}

func singleNative(declaration extension.Declaration, describe map[string]extension.ToolDescribe, open func(context.Context, extension.InstanceContext) (extension.Instance, error)) extension.Native {
	if open == nil {
		open = func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		}
	}
	return extension.Native{Declaration: declaration, Describe: describe, Open: open}
}

func testDescribeCallback() extension.ToolDescribe {
	return func(_ context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{
			Definition: model.ToolDefinition{Name: "tool-1", Description: "described", Parameters: json.RawMessage(`{"type":"object"}`)},
			Available:  true,
		}, nil
	}
}

type stubTool struct{}

func (stubTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (stubTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "/workspace/file"}}}
}

func (stubTool) Execute(_ context.Context, _ extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: string(plan.Data)}, Evidence: harness.ExecutionObserved}
}

func TestServeActivationDescribeThenOpen(t *testing.T) {
	var opened atomic.Int32
	capability := extension.ToolCapability("tool-1")
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		opened.Add(1)
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"tool-1": {Tool: stubTool{}},
		}}, nil
	})
	p := startPeer(t, native)
	activation := p.activate()
	if activation.Declaration.ID != "ext" {
		t.Fatalf("activation = %+v", activation)
	}

	input := extension.DescribeInput{
		Session:     testSession(),
		Model:       model.ModelRef{Provider: "provider", Model: "model"},
		Constraints: harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
		Revision:    "rev-1",
		Config:      json.RawMessage("{\n  \"x\": 1\n}"),
		AgentTypes:  []protocol.Agent{testAgent()},
		Inputs: harness.AgentInputs{
			Model:        model.ModelRef{Provider: "provider", Model: "model"},
			Instructions: "instructions",
			AssemblyMode: "full",
			Tools:        []string{"tool-a"},
			Capabilities: []string{"cap-a"},
			Constraints:  harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
			Strategy:     harness.StrategySelection{ID: "strategy"},
		},
	}
	encoded, err := worker.EncodeDescribeParams(input)
	if err != nil {
		t.Fatalf("encoding describe input: %v", err)
	}
	response := p.call("describe-1", metadataTarget("tool-1"), "metadata-token", "describe", encoded)
	if response.Error != nil {
		t.Fatalf("describe failed: %v", response.Error)
	}
	description := decodeFixed(t, response.Result, worker.DecodeDescribeResult)
	if description.Definition.Name != "tool-1" || !description.Available {
		t.Fatalf("description = %+v", description)
	}
	if opened.Load() != 0 {
		t.Fatalf("metadata describe opened %d instances, want none", opened.Load())
	}

	p.open("open-token")
	if opened.Load() != 1 {
		t.Fatalf("open count = %d, want 1", opened.Load())
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeNormalizePreservesOpaqueArgumentBytes(t *testing.T) {
	capability := extension.ToolCapability("tool-1")
	var received atomic.Value
	tool := recordingTool{received: &received}
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"tool-1": {Tool: tool}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	call := testToolCall()
	encoded, err := worker.EncodeNormalizeParams(testToolContext(), call)
	if err != nil {
		t.Fatalf("encoding normalize input: %v", err)
	}
	response := p.call("normalize-1", instanceTarget("tool-1"), "call-token", "normalize", encoded)
	if response.Error != nil {
		t.Fatalf("normalize failed: %v", response.Error)
	}
	stored, ok := received.Load().(json.RawMessage)
	if !ok || !bytes.Equal(stored, call.Arguments) {
		t.Fatalf("normalize received %q, want %q", stored, call.Arguments)
	}
	arguments := decodeFixed(t, response.Result, worker.DecodeArguments)
	if !bytes.Equal(arguments, call.Arguments) {
		t.Fatalf("normalize result = %q, want %q", arguments, call.Arguments)
	}
	if bytes.Contains(response.Result, []byte("\n")) {
		t.Fatalf("normalize result wire contains a literal newline: %s", response.Result)
	}

	plan := testToolPlan()
	plan.Data = []byte("data\nline\nwith\nnewlines")
	encoded, err = worker.EncodePrepareParams(testToolContext(), call)
	if err != nil {
		t.Fatalf("encoding prepare input: %v", err)
	}
	response = p.call("prepare-1", instanceTarget("tool-1"), "call-token", "prepare", encoded)
	if response.Error != nil {
		t.Fatalf("prepare failed: %v", response.Error)
	}
	prepared := decodeFixed(t, response.Result, worker.DecodePrepareResult)
	if prepared.CanonicalWorkspace != "" || len(prepared.Permissions) == 0 {
		t.Fatalf("prepared plan = %+v", prepared)
	}
	encoded, err = worker.EncodeExecuteParams(testToolContext(), plan)
	if err != nil {
		t.Fatalf("encoding execute input: %v", err)
	}
	response = p.call("execute-1", instanceTarget("tool-1"), "call-token", "execute", encoded)
	if response.Error != nil {
		t.Fatalf("execute failed: %v", response.Error)
	}
	outcome := decodeFixed(t, response.Result, worker.DecodeExecuteResult)
	if outcome.Result.Content != string(plan.Data) {
		t.Fatalf("execute content = %q, want preserved plan data %q", outcome.Result.Content, plan.Data)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type recordingTool struct {
	received *atomic.Value
}

func (t recordingTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	t.received.Store(append(json.RawMessage(nil), call.Arguments...))
	return call.Arguments, nil
}

func (t recordingTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "/workspace/file"}}}
}

func (t recordingTool) Execute(_ context.Context, _ extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: string(plan.Data)}, Evidence: harness.ExecutionObserved}
}

func TestServeHostcallAndCallbackScopes(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("echo",
		`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`,
		`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`))
	calls := make(chan extension.CallContext, 1)
	instanceCalls := make(chan extension.InstanceContext, 1)
	handler := func(ctx context.Context, callContext extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
		calls <- callContext
		return callContext.Call(ctx, "dep", "ping", arguments)
	}
	native := singleNative(baseDeclaration(capability), nil, func(_ context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		instanceCalls <- instance
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"echo": handler}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	arguments := json.RawMessage("{\"text\":\"line one\\nline two\"}")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(arguments)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})

	hostcall := p.readRequest(worker.MethodHostcall)
	var hostcallParams worker.HostcallParams
	if err := json.Unmarshal(hostcall.Params, &hostcallParams); err != nil {
		t.Fatalf("decoding hostcall params: %v", err)
	}
	if hostcallParams.Context.Token != "call-token" || hostcallParams.Binding != "dep" || hostcallParams.Method != "ping" {
		t.Fatalf("hostcall params = %+v", hostcallParams)
	}
	if !bytes.Equal(hostcallParams.Arguments, arguments) {
		t.Fatalf("hostcall arguments = %q, want %q", hostcallParams.Arguments, arguments)
	}
	value := json.RawMessage("{\n  \"text\": \"returned\\nvalue\"\n}")
	result, err := worker.EncodeServiceResult(extension.Result{Value: value})
	if err != nil {
		t.Fatalf("encoding hostcall result: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Result: result})

	response := p.readResponse("call-1")
	if response.Error != nil {
		t.Fatalf("call failed: %v", response.Error)
	}
	decoded, err := worker.DecodeServiceResult(response.Result)
	if err != nil {
		t.Fatalf("decoding call result: %v", err)
	}
	if !bytes.Equal(decoded.Value, value) {
		t.Fatalf("call result value = %q, want %q", decoded.Value, value)
	}

	callContext := <-calls
	if callContext.ExtensionID != "ext" || callContext.InstanceID != "instance" {
		t.Fatalf("call context = %+v", callContext)
	}
	if _, err := callContext.Call(context.Background(), "dep", "ping", json.RawMessage(`{}`)); !errors.Is(err, extension.ErrClosed) {
		t.Fatalf("call context after callback returned = %v, want extension.ErrClosed", err)
	}

	instanceCall := <-instanceCalls
	if instanceCall.InstanceID != "instance" || instanceCall.ExtensionID != "ext" {
		t.Fatalf("instance call context = %+v", instanceCall)
	}
	if _, err := instanceCall.Call(context.Background(), "dep", "ping", json.RawMessage(`{}`)); !errors.Is(err, extension.ErrClosed) {
		t.Fatalf("instance call after Open returned = %v, want extension.ErrClosed", err)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeInstanceCallDuringOpen(t *testing.T) {
	capability := extension.ToolCapability("tool-1")
	var received atomic.Value
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
		result, err := instance.Call(ctx, "dep", "ping", json.RawMessage(`{"during":"open"}`))
		if err != nil {
			return extension.Instance{}, err
		}
		received.Store(result.Value)
		return extension.Instance{Capabilities: map[string]extension.Implementation{"tool-1": {Tool: stubTool{}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()

	params, err := json.Marshal(worker.OpenParams{InstanceID: "instance", ComponentID: "component", Scope: testScope(), Context: testRoutingContext("open-token"), Section: worker.JSONBytes("{}")})
	if err != nil {
		t.Fatalf("encoding open params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open", Method: worker.MethodOpen, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	var hostcallParams worker.HostcallParams
	if err := json.Unmarshal(hostcall.Params, &hostcallParams); err != nil {
		t.Fatalf("decoding hostcall params: %v", err)
	}
	if hostcallParams.Context.Token != "open-token" || hostcallParams.Binding != "dep" {
		t.Fatalf("hostcall params = %+v", hostcallParams)
	}
	result, err := worker.EncodeServiceResult(extension.Result{Value: json.RawMessage(`{"answer":true}`)})
	if err != nil {
		t.Fatalf("encoding hostcall result: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Result: result})
	response := p.readResponse("open")
	if response.Error != nil {
		t.Fatalf("open failed: %v", response.Error)
	}
	if value, ok := received.Load().(json.RawMessage); !ok || string(value) != `{"answer":true}` {
		t.Fatalf("open hostcall value = %q", value)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeCancelInFlightAndLateCancel(t *testing.T) {
	started := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("wait",
		`{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	toolCapability := extension.ToolCapability("tool-1")
	handler := func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		close(started)
		<-ctx.Done()
		return extension.Result{}, ctx.Err()
	}
	native := singleNative(baseDeclaration(capability, toolCapability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"wait": handler}},
			"tool-1":    {Tool: stubTool{}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "wait", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	<-started
	p.send(worker.Frame{Kind: worker.KindCancel, ID: "call-1"})
	response := p.readResponse("call-1")
	if response.Error == nil || response.Error.Class != worker.ClassCancelled {
		t.Fatalf("cancelled response = %+v, want the cancelled class", response)
	}

	p.send(worker.Frame{Kind: worker.KindCancel, ID: "call-1"})
	p.send(worker.Frame{Kind: worker.KindCancel, ID: "never-sent"})

	encoded, err := worker.EncodeDescribeParams(extension.DescribeInput{Session: testSession(), Model: model.ModelRef{Provider: "provider", Model: "model"}, Inputs: harness.AgentInputs{Model: model.ModelRef{Provider: "provider", Model: "model"}}})
	if err != nil {
		t.Fatalf("encoding describe input: %v", err)
	}
	describe := p.call("describe-1", metadataTarget("tool-1"), "describe-token", "describe", encoded)
	if describe.Error != nil {
		t.Fatalf("describe after late cancel failed: %v", describe.Error)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeConcurrentRequests(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("work",
		`{"type":"object","additionalProperties":false}`,
		`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`))
	handler := func(ctx context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		}
		return extension.Result{Value: arguments}, nil
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"work": handler}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	for _, id := range []string{"call-1", "call-2"} {
		params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("token-" + id), Method: "work", Arguments: worker.JSONBytes(`{"value":"` + id + `"}`)})
		if err != nil {
			t.Fatalf("encoding call params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: id, Method: worker.MethodCall, Params: params})
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(serveTimeout):
			t.Fatal("host requests did not run concurrently")
		}
	}
	close(release)
	for _, id := range []string{"call-1", "call-2"} {
		response := p.readResponse(id)
		if response.Error != nil {
			t.Fatalf("%s failed: %v", id, response.Error)
		}
		result, err := worker.DecodeServiceResult(response.Result)
		if err != nil {
			t.Fatalf("decoding %s: %v", id, err)
		}
		want := `{"value":"` + id + `"}`
		if string(result.Value) != want {
			t.Fatalf("%s value = %s, want %s", id, result.Value, want)
		}
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeDisposeCancelsAndJoinsInstanceCalls(t *testing.T) {
	started := make(chan struct{})
	var closed atomic.Bool
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("wait",
		`{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handler := func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		close(started)
		<-ctx.Done()
		return extension.Result{}, ctx.Err()
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{
			Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"wait": handler}}},
			Close: func(context.Context, extension.CallContext) error {
				closed.Store(true)
				return nil
			},
		}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "wait", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	<-started
	dispose, err := json.Marshal(worker.DisposeParams{InstanceID: "instance", Context: testRoutingContext("dispose-token")})
	if err != nil {
		t.Fatalf("encoding dispose params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-1", Method: worker.MethodDispose, Params: dispose})

	seen := map[string]worker.Frame{}
	for len(seen) != 2 {
		frame := p.next()
		if frame.Kind == worker.KindResponse {
			seen[frame.ID] = frame
			continue
		}
		t.Fatalf("unexpected frame while disposing: %+v", frame)
	}
	if call := seen["call-1"]; call.Error == nil || call.Error.Class != worker.ClassCancelled {
		t.Fatalf("call response = %+v, want cancelled", call)
	}
	if disposeResponse := seen["dispose-1"]; disposeResponse.Error != nil || string(disposeResponse.Result) != "null" {
		t.Fatalf("dispose response = %+v", disposeResponse)
	}
	if !closed.Load() {
		t.Fatal("dispose did not close the instance")
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeOversizedResultFailsOnlyItsCall(t *testing.T) {
	capability := extension.ToolCapability("tool-1")
	tool := largeResultTool{result: json.RawMessage(strings.Repeat("a", worker.FrameLimit))}
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"tool-1": {Tool: tool}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	encoded, err := worker.EncodeNormalizeParams(testToolContext(), model.ToolCall{ID: "call-1", Name: "tool-1", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("encoding normalize input: %v", err)
	}
	response := p.call("normalize-1", instanceTarget("tool-1"), "call-token", "normalize", encoded)
	if response.Error == nil || response.Error.Class != worker.ClassInternal || !strings.Contains(response.Error.Message, "frame limit") {
		t.Fatalf("oversized response = %+v, want an internal frame-limit error", response)
	}
	describeInput, err := worker.EncodeDescribeParams(extension.DescribeInput{
		Session: testSession(),
		Model:   model.ModelRef{Provider: "provider", Model: "model"},
		Inputs:  harness.AgentInputs{Model: model.ModelRef{Provider: "provider", Model: "model"}},
	})
	if err != nil {
		t.Fatalf("encoding describe input: %v", err)
	}
	describe := p.call("describe-1", metadataTarget("tool-1"), "describe-token", "describe", describeInput)
	if describe.Error != nil {
		t.Fatalf("describe after oversized result failed: %v", describe.Error)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type largeResultTool struct {
	result json.RawMessage
}

func (t largeResultTool) Normalize(extension.ToolContext, model.ToolCall) (json.RawMessage, error) {
	return t.result, nil
}

func (t largeResultTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{}}
}

func (t largeResultTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{}
}

func TestServeEOFCancelsOutstandingWork(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("wait",
		`{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handler := func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return extension.Result{}, ctx.Err()
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"wait": handler}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "wait", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	<-started
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(serveTimeout):
		t.Fatal("outstanding call did not observe cancellation at EOF")
	}
}

func TestServeProtocolFailures(t *testing.T) {
	t.Run("unknown frame kind", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		p.activate()
		p.send(worker.Frame{Kind: "bogus", ID: "x"})
		if err := p.wait(); err == nil || !strings.Contains(err.Error(), "unknown frame kind") {
			t.Fatalf("Serve = %v, want an unknown-kind failure", err)
		}
	})
	t.Run("response to unknown id", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		p.activate()
		p.send(worker.Frame{Kind: worker.KindResponse, ID: "missing", Result: json.RawMessage("null")})
		if err := p.wait(); err == nil || !strings.Contains(err.Error(), "unknown request") {
			t.Fatalf("Serve = %v, want an unknown-response failure", err)
		}
	})
	for _, tc := range []struct {
		name  string
		frame func(id string) worker.Frame
	}{
		{"both result and error", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id, Result: json.RawMessage("null"), Error: &worker.Error{Class: worker.ClassInternal, Message: "x"}}
		}},
		{"neither result nor error", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id}
		}},
		{"unknown error class", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id, Error: &worker.Error{Class: "bogus", Message: "x"}}
		}},
		{"invalid service result", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id, Result: json.RawMessage(`{"value":"e30=","failure":{"code":"x","message":"y"}}`)}
		}},
		{"invalid success bytes", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id, Result: json.RawMessage(`{"value":"e30"}`)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("echo",
				`{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
			native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"echo": func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
					close(started)
					_, err := callContext.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
					return extension.Result{}, err
				}}}}}, nil
			})
			p := startPeer(t, native)
			p.activate()
			p.open("open-token")
			params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(`{}`)})
			if err != nil {
				t.Fatalf("encoding call params: %v", err)
			}
			p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
			<-started
			hostcall := p.readRequest(worker.MethodHostcall)
			p.send(tc.frame(hostcall.ID))
			if err := p.wait(); err == nil {
				t.Fatal("Serve accepted an invalid terminal payload")
			}
		})
	}
}

func TestServeFrameBounds(t *testing.T) {
	if worker.FrameLimit != 4<<20 {
		t.Fatalf("FrameLimit = %d, want the 4 MiB default", worker.FrameLimit)
	}
	t.Run("over-limit input ends Serve", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		go func() {
			_, _ = p.in.Write(bytes.Repeat([]byte("x"), 4<<20+1))
		}()
		err := p.wait()
		if !errors.Is(err, worker.ErrFrameTooLarge) {
			t.Fatalf("Serve = %v, want ErrFrameTooLarge", err)
		}
	})
	t.Run("writer rejects over-limit envelope", func(t *testing.T) {
		var buffer bytes.Buffer
		writer := worker.NewWriter(&buffer)
		err := writer.WriteFrame(worker.Frame{Kind: worker.KindRequest, ID: "x", Method: worker.MethodCall, Params: json.RawMessage(`"` + strings.Repeat("a", worker.FrameLimit) + `"`)})
		if !errors.Is(err, worker.ErrFrameTooLarge) {
			t.Fatalf("WriteFrame = %v, want ErrFrameTooLarge", err)
		}
		if buffer.Len() != 0 {
			t.Fatalf("writer wrote %d bytes of an over-limit frame", buffer.Len())
		}
	})
}

func TestServeHandshakeFailures(t *testing.T) {
	t.Run("version mismatch", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		params, _ := json.Marshal(worker.ActivateParams{ContractVersion: "2"})
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "activate", Method: worker.MethodActivate, Params: params})
		response := p.readResponse("activate")
		if response.Error == nil || response.Error.Class != worker.ClassIncompatible {
			t.Fatalf("activation response = %+v, want incompatible", response)
		}
		err := p.wait()
		var typed *worker.Error
		if !errors.As(err, &typed) || typed.Class != worker.ClassIncompatible {
			t.Fatalf("Serve = %v, want an incompatible error", err)
		}
	})
	t.Run("first frame is not activation", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "open", Method: worker.MethodOpen, Params: json.RawMessage(`{}`)})
		response := p.readResponse("open")
		if response.Error == nil || response.Error.Class != worker.ClassIncompatible {
			t.Fatalf("first request response = %+v, want incompatible", response)
		}
		if err := p.wait(); err == nil || !strings.Contains(err.Error(), "not") {
			t.Fatalf("Serve = %v, want a handshake failure", err)
		}
	})
	t.Run("first frame is not a request", func(t *testing.T) {
		p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
		p.send(worker.Frame{Kind: worker.KindProgress, ID: "x", Value: json.RawMessage("null")})
		if err := p.wait(); err == nil {
			t.Fatal("Serve accepted progress as the first frame")
		}
	})
}

func TestServeBadRouting(t *testing.T) {
	capability := extension.ToolCapability("tool-1")
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"tool-1": {Tool: stubTool{}}}}, nil
	})
	for _, tc := range []struct {
		name      string
		target    worker.Target
		method    string
		arguments json.RawMessage
	}{
		{"unknown metadata capability", worker.Target{Kind: worker.TargetMetadata, CapabilityID: "missing"}, "describe", json.RawMessage(`{}`)},
		{"unknown instance", worker.Target{Kind: worker.TargetInstance, CapabilityID: "tool-1", InstanceID: "missing"}, "normalize", json.RawMessage(`{}`)},
		{"unknown method", instanceTarget("tool-1"), "bogus", json.RawMessage(`{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startPeer(t, native)
			p.activate()
			p.open("open-token")
			params, err := json.Marshal(worker.CallParams{Target: tc.target, Context: testRoutingContext("token"), Method: tc.method, Arguments: worker.JSONBytes(tc.arguments)})
			if err != nil {
				t.Fatalf("encoding call params: %v", err)
			}
			p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
			if err := p.wait(); err == nil {
				t.Fatal("Serve accepted bad routing")
			}
		})
	}
}

func TestServeProgressDropped(t *testing.T) {
	p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
	p.activate()
	p.send(worker.Frame{Kind: worker.KindProgress, ID: "unknown", Value: json.RawMessage(`{"x":1}`)})
	p.send(worker.Frame{Kind: worker.KindProgress, Value: json.RawMessage(`"not json routing"`)})
	p.send(worker.Frame{Kind: worker.KindCancel, ID: "unknown"})
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeSingleProgressIsNotFatal(t *testing.T) {
	p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
	p.activate()
	p.send(worker.Frame{Kind: worker.KindProgress, ID: "unknown", Value: json.RawMessage(`null`)})
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeOpenFailureReturnsError(t *testing.T) {
	capability := extension.ToolCapability("tool-1")
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{}, harness.ErrInvalid
	})
	p := startPeer(t, native)
	p.activate()
	params, err := json.Marshal(worker.OpenParams{InstanceID: "instance", ComponentID: "component", Scope: testScope(), Context: testRoutingContext("open-token")})
	if err != nil {
		t.Fatalf("encoding open params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open", Method: worker.MethodOpen, Params: params})
	response := p.readResponse("open")
	if response.Error == nil || response.Error.Class != worker.ClassInvalid {
		t.Fatalf("open response = %+v, want the invalid class", response)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeOneMiBMultilineMetadataFitsFrame(t *testing.T) {
	metadata := json.RawMessage(`{"data":"` + strings.Repeat("a", 1<<20) + `"}`)
	capability := extension.ToolCapability("tool-1")
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"tool-1": {Tool: metadataTool{metadata: metadata}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	plan := extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
	encoded, err := worker.EncodeExecuteParams(testToolContext(), plan)
	if err != nil {
		t.Fatalf("encoding execute input: %v", err)
	}
	response := p.call("execute-1", instanceTarget("tool-1"), "call-token", "execute", encoded)
	if response.Error != nil {
		t.Fatalf("execute failed: %v", response.Error)
	}
	outcome := decodeFixed(t, response.Result, worker.DecodeExecuteResult)
	if !bytes.Equal(outcome.Metadata, metadata) {
		t.Fatalf("metadata bytes changed: got %d bytes, want %d", len(outcome.Metadata), len(metadata))
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type metadataTool struct {
	metadata json.RawMessage
}

func (t metadataTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (t metadataTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{}}
}

func (t metadataTool) Execute(_ context.Context, _ extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ok"}, Metadata: t.metadata, Evidence: harness.ExecutionObserved}
}

type stubStrategy struct{}

func (stubStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func (stubStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func TestServeFixedMethodsUseBareResults(t *testing.T) {
	hooks := extension.HookCapability("hook-extra",
		extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure),
		extension.SingleHook(extension.BoundaryAfterTool, extension.HookModePure))
	strategy := extension.StrategyCapability("strategy-1", true)
	native := singleNative(baseDeclaration(hooks, strategy), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"hook-extra": {
				Preparation: func(context.Context, extension.CallContext, extension.PreparationInput) (extension.PreparationConsequence, error) {
					return extension.PreparationConsequence{Prompt: stringPointer("hook prompt")}, nil
				},
				AfterTool: func(context.Context, extension.CallContext, harness.ToolOutputInput) (string, error) {
					return "shaped", nil
				},
			},
			"strategy-1": {Strategy: stubStrategy{}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	prepareInput, err := worker.EncodePrepareHookParams(testPreparationInput())
	if err != nil {
		t.Fatalf("encoding prepare hook input: %v", err)
	}
	prepare := p.call("prepare-1", instanceTarget("hook-extra"), "call-token", "prepare", prepareInput)
	if prepare.Error != nil {
		t.Fatalf("prepare hook failed: %v", prepare.Error)
	}
	consequence := decodeFixed(t, prepare.Result, worker.DecodePrepareHookResult)
	if consequence.Prompt == nil || *consequence.Prompt != "hook prompt" {
		t.Fatalf("prepare consequence = %+v", consequence)
	}

	afterInput, err := worker.EncodeAfterToolHookParams(harness.ToolOutputInput{Outcome: testToolOutcome(), Content: "content"})
	if err != nil {
		t.Fatalf("encoding after-tool hook input: %v", err)
	}
	after := p.call("after-1", instanceTarget("hook-extra"), "call-token", "after_tool", afterInput)
	if after.Error != nil {
		t.Fatalf("after-tool hook failed: %v", after.Error)
	}
	if content := decodeFixed(t, after.Result, worker.DecodeAfterToolHookResult); content != "shaped" {
		t.Fatalf("after-tool content = %q", content)
	}

	planInput, err := worker.EncodePlanParams(testPlanInput())
	if err != nil {
		t.Fatalf("encoding plan input: %v", err)
	}
	plan := p.call("plan-1", instanceTarget("strategy-1"), "call-token", "plan", planInput)
	if plan.Error != nil {
		t.Fatalf("strategy plan failed: %v", plan.Error)
	}
	if output := decodeFixed(t, plan.Result, worker.DecodePlanResult); output.Kind != harness.PlanNone {
		t.Fatalf("plan output = %+v", output)
	}

	consumeInput, err := worker.EncodeConsumeParams(harness.ConsumeInput{Input: testPlanInput(), Output: testModelOutput()})
	if err != nil {
		t.Fatalf("encoding consume input: %v", err)
	}
	consume := p.call("consume-1", instanceTarget("strategy-1"), "call-token", "consume", consumeInput)
	if consume.Error != nil {
		t.Fatalf("strategy consume failed: %v", consume.Error)
	}
	if output := decodeFixed(t, consume.Result, worker.DecodePlanResult); output.Kind != harness.PlanNone {
		t.Fatalf("consume output = %+v", output)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestProgressFrameWithMalformedFieldsIsDropped(t *testing.T) {
	p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
	p.activate()
	p.sendRaw(`{"kind":"progress","id":123}`)
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestFrameRejectsNullMembers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame worker.Frame
	}{
		{
			name:  "failure data null",
			frame: worker.Frame{Kind: worker.KindResponse, ID: "missing", Result: json.RawMessage(`{"failure":{"code":"x","message":"y","data":null}}`)},
		},
		{
			name:  "failure partial null",
			frame: worker.Frame{Kind: worker.KindResponse, ID: "missing", Result: json.RawMessage(`{"failure":{"code":"x","message":"y","partial":null}}`)},
		},
		{
			name: "optional context section null",
			frame: worker.Frame{
				Kind:   worker.KindRequest,
				ID:     "open",
				Method: worker.MethodOpen,
				Params: json.RawMessage(`{"instance_id":"instance","component_id":"component","scope":{"kind":"harness","revision":"rev-1"},"context":{"token":"token","section":null},"section":""}`),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
			p.activate()
			p.send(tc.frame)
			err := p.wait()
			if err == nil || !strings.Contains(err.Error(), "null") {
				t.Fatalf("Serve = %v, want the null-member failure", err)
			}
		})
	}
}

func TestReaderAllowsProtocolNullResult(t *testing.T) {
	var buffer bytes.Buffer
	buffer.WriteString(`{"kind":"response","id":"x","result":null}` + "\n")
	frame, err := worker.NewReader(&buffer).ReadFrame()
	if err != nil {
		t.Fatalf("protocol-level null result rejected: %v", err)
	}
	if frame.Kind != worker.KindResponse || string(frame.Result) != "null" {
		t.Fatalf("frame = %+v", frame)
	}
}

// TestServeHostcallPartialDecodesCallError proves a hostcall error carrying a
// produced partial reaches the worker author as extension.CallError wrapping
// the classed worker error, with the exact owned bytes and cause preserved.
func TestServeHostcallPartialDecodesCallError(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("call", `{}`, `{}`))
	received := make(chan error, 1)
	handler := func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		_, err := call.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
		received <- err
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"call": handler}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "call", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	partial := json.RawMessage("{\n  \"restored\": [\"a.go\"],\n  \"big\": 9007199254740993\n}")
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Error: &worker.Error{
		Class: worker.ClassConflict, Message: "partial failure", Partial: worker.JSONBytes(partial),
	}})
	response := p.readResponse("call-1")
	if response.Error != nil {
		t.Fatalf("author call failed: %v", response.Error)
	}
	var callErr *extension.CallError
	if err := <-received; !errors.As(err, &callErr) {
		t.Fatalf("author error = %v, want extension.CallError", err)
	} else if !errors.Is(callErr, harness.ErrConflict) {
		t.Fatalf("author error cause = %v, want the conflict class", callErr.Err)
	} else if !bytes.Equal(callErr.Partial, partial) {
		t.Fatalf("author partial = %q, want %q", callErr.Partial, partial)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

// TestServeHostcallMalformedPartialFailsActivation proves an invalid present
// partial is rejected by the existing activation-invalid-terminal rule rather
// than delivered to the author.
func TestServeHostcallMalformedPartialFailsActivation(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1", extension.ServiceIOMethod("call", `{}`, `{}`))
	received := make(chan error, 1)
	handler := func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		_, err := call.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
		received <- err
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"call": handler}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "call", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Error: &worker.Error{
		Class: worker.ClassInternal, Message: "malformed", Partial: worker.JSONBytes(`not json`),
	}})
	if err := p.wait(); err == nil || !strings.Contains(err.Error(), "invalid error partial") {
		t.Fatalf("Serve error = %v, want the invalid error partial terminal", err)
	}
}
