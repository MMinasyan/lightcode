package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// awaitResponse reads one response without hanging forever when Serve exits on
// a protocol failure instead of answering the call.
func (p *peer) awaitResponse(id string) (worker.Frame, error) {
	p.t.Helper()
	for i, frame := range p.stash {
		if frame.Kind == worker.KindResponse && frame.ID == id {
			p.stash = append(p.stash[:i], p.stash[i+1:]...)
			return frame, nil
		}
	}
	type result struct {
		frame worker.Frame
		err   error
	}
	results := make(chan result, 1)
	go func() {
		for {
			frame, err := p.reader.ReadFrame()
			if err != nil {
				results <- result{err: err}
				return
			}
			if frame.Kind == worker.KindResponse && frame.ID == id {
				results <- result{frame: frame}
				return
			}
			p.stash = append(p.stash, frame)
		}
	}()
	select {
	case got := <-results:
		return got.frame, got.err
	case err := <-p.errs:
		return worker.Frame{}, err
	case <-time.After(serveTimeout):
		return worker.Frame{}, errors.New("no response")
	}
}

func (p *peer) openInstance(id string) worker.Frame {
	p.t.Helper()
	params, err := json.Marshal(worker.OpenParams{
		InstanceID:  id,
		ComponentID: "component",
		Scope:       testScope(),
		Context:     testRoutingContext("open-token"),
		Section:     worker.JSONBytes("{}"),
	})
	if err != nil {
		p.t.Fatalf("encoding open params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open-" + id, Method: worker.MethodOpen, Params: params})
	return p.readResponse("open-" + id)
}

func TestNamedServiceArgumentsByteExact(t *testing.T) {
	arguments := []byte("{\n  \"text\": \"a<b>&c\\nd\"\n}")
	value := []byte("{\n  \"result\": \"x<y>&z\\nw\"\n}")
	captured := make(chan []byte, 1)
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("echo", `{"type":"object"}`, `{"type":"object"}`))
	handler := func(ctx context.Context, callContext extension.CallContext, args json.RawMessage) (extension.Result, error) {
		captured <- append([]byte(nil), args...)
		return callContext.Call(ctx, "dep", "echo", args)
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"echo": handler}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")

	params, err := json.Marshal(worker.CallParams{
		Target:    instanceTarget("service-1"),
		Context:   testRoutingContext("call-token"),
		Method:    "echo",
		Arguments: worker.JSONBytes(arguments),
	})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	var hostcallParams worker.HostcallParams
	if err := json.Unmarshal(hostcall.Params, &hostcallParams); err != nil {
		t.Fatalf("decoding hostcall params: %v", err)
	}
	if !bytes.Equal(hostcallParams.Arguments, arguments) {
		t.Fatalf("hostcall arguments = %q, want the exact %q", hostcallParams.Arguments, arguments)
	}
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
		t.Fatalf("result value = %q, want the exact %q", decoded.Value, value)
	}
	if got := <-captured; !bytes.Equal(got, arguments) {
		t.Fatalf("handler arguments = %q, want the exact %q", got, arguments)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestRoutingContextCallerWireShape(t *testing.T) {
	routing := testRoutingContext("token")
	raw, err := json.Marshal(routing)
	if err != nil {
		t.Fatalf("encoding routing context: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decoding routing context: %v", err)
	}
	var caller map[string]json.RawMessage
	if err := json.Unmarshal(members["caller"], &caller); err != nil {
		t.Fatalf("decoding caller: %v", err)
	}
	for _, member := range []string{"extension_id", "component_id", "instance_id"} {
		if _, ok := caller[member]; !ok {
			t.Fatalf("caller is missing %q: %s", member, members["caller"])
		}
	}
	for _, member := range []string{"ExtensionID", "ComponentID", "InstanceID"} {
		if _, ok := caller[member]; ok {
			t.Fatalf("caller carries Go field name %q: %s", member, members["caller"])
		}
	}
	var decoded worker.Context
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding routing context: %v", err)
	}
	if decoded.Caller == nil || *decoded.Caller != *routing.Caller {
		t.Fatalf("decoded caller = %+v, want %+v", decoded.Caller, routing.Caller)
	}
}

func TestExactlyFrameLimitEnvelope(t *testing.T) {
	const limit = 4 << 20
	frame := worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: json.RawMessage(`""`)}
	base, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding frame base: %v", err)
	}
	padding := limit - len(base)
	frame.Params = json.RawMessage(`"` + strings.Repeat("a", padding) + `"`)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding exact frame: %v", err)
	}
	if len(encoded) != limit {
		t.Fatalf("exact frame is %d bytes, want %d", len(encoded), limit)
	}
	var buffer bytes.Buffer
	if err := worker.NewWriter(&buffer).WriteFrame(frame); err != nil {
		t.Fatalf("writing the exact-limit frame: %v", err)
	}
	got, err := worker.NewReader(bytes.NewReader(buffer.Bytes())).ReadFrame()
	if err != nil {
		t.Fatalf("reading the exact-limit frame: %v", err)
	}
	if got.Kind != worker.KindRequest || got.ID != "call-1" || len(got.Params) != len(frame.Params) {
		t.Fatalf("exact-limit frame round trip = kind %q id %q params %d", got.Kind, got.ID, len(got.Params))
	}
	frame.Params = json.RawMessage(`"` + strings.Repeat("a", padding+1) + `"`)
	if err := worker.NewWriter(io.Discard).WriteFrame(frame); !errors.Is(err, worker.ErrFrameTooLarge) {
		t.Fatalf("one byte over the limit = %v, want ErrFrameTooLarge", err)
	}
}

func TestOversizedActivationEndsServe(t *testing.T) {
	declaration := baseDeclaration()
	declaration.Version = strings.Repeat("v", 5<<20)
	native := singleNative(declaration, nil, nil)
	p := startPeer(t, native)
	params, err := json.Marshal(worker.ActivateParams{ContractVersion: extension.ContractVersion})
	if err != nil {
		t.Fatalf("encoding activation params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "activate", Method: worker.MethodActivate, Params: params})
	response := p.readResponse("activate")
	if response.Error == nil || response.Error.Class != worker.ClassInternal {
		t.Fatalf("activation response = %+v, want the small internal error", response)
	}
	if err := p.wait(); err == nil {
		t.Fatal("Serve continued after an activation failure")
	}
}

func TestBadParamsPayloadEndsServe(t *testing.T) {
	p := startPeer(t, singleNative(baseDeclaration(), nil, nil))
	p.activate()
	params := json.RawMessage(`{"instance_id":"instance","component_id":"component","scope":{"kind":"harness","revision":"rev-1"},"context":{"token":"token"},"section":"@@@"}`)
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open", Method: worker.MethodOpen, Params: params})
	err := p.wait()
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("Serve = %v, want the malformed payload failure", err)
	}
}

func TestOversizedErrorMessageIsCallLocal(t *testing.T) {
	huge := strings.Repeat("e", worker.FrameLimit)
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("fail", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`),
		extension.ServiceIOMethod("ok", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handlers := map[string]extension.Handler{
		"fail": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{}, errors.New(huge)
		},
		"ok": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{Value: json.RawMessage("null")}, nil
		},
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: handlers}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "fail", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding fail call: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-fail", Method: worker.MethodCall, Params: params})
	response, err := p.awaitResponse("call-fail")
	if err != nil {
		t.Fatalf("Serve ended instead of answering the oversized error: %v", err)
	}
	if response.Error == nil || response.Error.Class != worker.ClassInternal {
		t.Fatalf("fail response = %+v, want the small internal error", response)
	}
	if len(response.Error.Message) > 200 || !strings.Contains(response.Error.Message, "frame limit") {
		t.Fatalf("fail response message = %q, want the small frame-limit fallback", response.Error.Message)
	}
	ok := p.call("call-ok", instanceTarget("service-1"), "call-token", "ok", json.RawMessage(`{}`))
	if ok.Error != nil {
		t.Fatalf("follow-up call failed: %v", ok.Error)
	}
	if result, err := worker.DecodeServiceResult(ok.Result); err != nil || string(result.Value) != "null" {
		t.Fatalf("follow-up call result = %+v, %v", result, err)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestCallThenDisposeOrderingRepeated(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
			"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage("null")}, nil
			},
		}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	for i := 0; i < 200; i++ {
		instance := fmt.Sprintf("instance-%d", i)
		if response := p.openInstance(instance); response.Error != nil {
			t.Fatalf("open %d failed: %+v", i, response)
		}
		callParams, err := json.Marshal(worker.CallParams{Target: worker.Target{Kind: worker.TargetInstance, CapabilityID: "service-1", InstanceID: instance}, Context: testRoutingContext("call-token"), Method: "ping", Arguments: worker.JSONBytes(`{}`)})
		if err != nil {
			t.Fatalf("encoding call params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-" + instance, Method: worker.MethodCall, Params: callParams})
		disposeParams, err := json.Marshal(worker.DisposeParams{InstanceID: instance, Context: testRoutingContext("dispose-token")})
		if err != nil {
			t.Fatalf("encoding dispose params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-" + instance, Method: worker.MethodDispose, Params: disposeParams})
		callResponse, err := p.awaitResponse("call-" + instance)
		if err != nil {
			t.Fatalf("call %d: Serve ended: %v", i, err)
		}
		if callResponse.Error != nil && callResponse.Error.Class != worker.ClassCancelled {
			t.Fatalf("call %d response = %+v", i, callResponse)
		}
		disposeResponse, err := p.awaitResponse("dispose-" + instance)
		if err != nil {
			t.Fatalf("dispose %d: Serve ended: %v", i, err)
		}
		if disposeResponse.Error != nil || string(disposeResponse.Result) != "null" {
			t.Fatalf("dispose %d response = %+v", i, disposeResponse)
		}
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestDuplicateOpenFailsBeforeNativeOpen(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-release
		}
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
			"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage("null")}, nil
			},
		}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.drain()
	params, err := json.Marshal(worker.OpenParams{InstanceID: "instance", ComponentID: "component", Scope: testScope(), Context: testRoutingContext("open-token")})
	if err != nil {
		t.Fatalf("encoding open params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open-1", Method: worker.MethodOpen, Params: params})
	<-started
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "open-2", Method: worker.MethodOpen, Params: params})
	close(release)
	err = p.wait()
	if err == nil || !strings.Contains(err.Error(), "duplicate instance id") {
		t.Fatalf("Serve = %v, want the duplicate-instance failure", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Native.Open ran %d times, want 1", calls.Load())
	}
}

func TestDisposeCancelReachesClose(t *testing.T) {
	closeStarted := make(chan struct{})
	closeObserved := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{
			Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
				"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage("null")}, nil
				},
			}}},
			Close: func(ctx context.Context, _ extension.CallContext) error {
				close(closeStarted)
				<-ctx.Done()
				close(closeObserved)
				return ctx.Err()
			},
		}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.DisposeParams{InstanceID: "instance", Context: testRoutingContext("dispose-token")})
	if err != nil {
		t.Fatalf("encoding dispose params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-1", Method: worker.MethodDispose, Params: params})
	<-closeStarted
	p.send(worker.Frame{Kind: worker.KindCancel, ID: "dispose-1"})
	response, err := p.awaitResponse("dispose-1")
	if err != nil {
		t.Fatalf("dispose cancellation did not answer: %v", err)
	}
	if response.Error == nil || response.Error.Class != worker.ClassCancelled {
		t.Fatalf("dispose response = %+v, want cancelled", response)
	}
	select {
	case <-closeObserved:
	case <-time.After(serveTimeout):
		t.Fatal("dispose cancellation did not reach Close")
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestRetainedCallDuringResultEncodingFails(t *testing.T) {
	captured := make(chan func(context.Context, string, string, json.RawMessage) (extension.Result, error), 1)
	huge := strings.Repeat("a", 16<<20)
	value := json.RawMessage(`"` + huge + `"`)
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("echo", `{"type":"object"}`, `{"type":"object"}`))
	handler := func(_ context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		captured <- callContext.Call
		return extension.Result{Value: value}, nil
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"echo": handler}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	call := <-captured
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := call(ctx, "dep", "ping", json.RawMessage(`{}`)); !errors.Is(err, extension.ErrClosed) {
		t.Fatalf("retained Call during result encoding = %v, want extension.ErrClosed", err)
	}
	response := p.readResponse("call-1")
	if response.Error == nil || response.Error.Class != worker.ClassInternal {
		t.Fatalf("oversized result response = %+v, want the small internal error", response)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestInvalidNativeDeclarationEndsServe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native extension.Native
	}{
		{
			name:   "missing config schema",
			native: extension.Native{Declaration: extension.Declaration{ID: "ext", Version: "1.0.0"}},
		},
		{
			name: "nil describe callback",
			native: extension.Native{
				Declaration: extension.Declaration{
					ID:           "ext",
					Version:      "1.0.0",
					ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
					Components: []extension.Component{{
						ID:       "component",
						Lifetime: extension.LifetimeHarness,
						Provides: []string{"tool-1"},
					}},
					Capabilities: []extension.Capability{extension.ToolCapability("tool-1")},
				},
				Describe: map[string]extension.ToolDescribe{"tool-1": nil},
				Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
					return extension.Instance{}, nil
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := worker.Serve(context.Background(), tc.native, strings.NewReader(""), io.Discard); err == nil {
				t.Fatal("Serve accepted an invalid native declaration")
			}
		})
	}
}

func TestIncompleteInstanceClosedAndRefused(t *testing.T) {
	var closed atomic.Bool
	capability := extension.ToolCapability("tool-1")
	native := singleNative(baseDeclaration(capability), map[string]extension.ToolDescribe{"tool-1": testDescribeCallback()}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{
			Close: func(context.Context, extension.CallContext) error {
				closed.Store(true)
				return nil
			},
		}, nil
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
		t.Fatalf("open response = %+v, want invalid", response)
	}
	if !closed.Load() {
		t.Fatal("incomplete instance was not closed")
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestDeclarationBindingAnotherExtensionActivates(t *testing.T) {
	foreign := extension.ServiceCapability("other.service", "other.service/v1",
		extension.ServiceIOMethod("call", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	local := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	declaration := extension.Declaration{
		ID:           "ext",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Components: []extension.Component{{
			ID:       "component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"service-1"},
			Requires: []extension.Binding{{
				Name:         "foreign",
				Provider:     "other-extension",
				CapabilityID: "other.service",
				Definition:   foreign.Definition(),
			}},
		}},
		Capabilities: []extension.Capability{local},
	}
	native := extension.Native{
		Declaration: declaration,
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"service-1": {Service: map[string]extension.Handler{
					"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
						return extension.Result{Value: json.RawMessage("null")}, nil
					},
				}},
			}}, nil
		},
	}
	if _, err := extension.Compile([]extension.Native{native}); err == nil {
		t.Fatal("full compilation unexpectedly resolved a provider outside the native set")
	}
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	response := p.call("call-1", instanceTarget("service-1"), "call-token", "ping", json.RawMessage(`{}`))
	if response.Error != nil {
		t.Fatalf("call to the opened instance failed: %v", response.Error)
	}
	result, err := worker.DecodeServiceResult(response.Result)
	if err != nil || string(result.Value) != "null" {
		t.Fatalf("call result = %+v, %v", result, err)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestErrClosedClassBothDirections(t *testing.T) {
	if !errors.Is(&worker.Error{Class: worker.ClassClosed}, extension.ErrClosed) {
		t.Fatal("closed class does not unwrap to extension.ErrClosed")
	}
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("direct", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`),
		extension.ServiceIOMethod("nested", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handlers := map[string]extension.Handler{
		"direct": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{}, extension.ErrClosed
		},
		"nested": func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			_, err := callContext.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
			return extension.Result{}, err
		},
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: handlers}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	direct := p.call("call-direct", instanceTarget("service-1"), "call-token", "direct", json.RawMessage(`{}`))
	if direct.Error == nil || direct.Error.Class != worker.ClassClosed {
		t.Fatalf("direct closed error = %+v, want the closed class", direct)
	}
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "nested", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding nested call: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-nested", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Error: &worker.Error{Class: worker.ClassClosed, Message: "gone"}})
	nested := p.readResponse("call-nested")
	if nested.Error == nil || nested.Error.Class != worker.ClassClosed {
		t.Fatalf("nested closed error = %+v, want the closed class", nested)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeHookErrorsCarryEffectKnowledge(t *testing.T) {
	effectful := extension.HookCapability("hook-effectful",
		extension.SingleHook(extension.BoundaryRequest, extension.HookModeEffectful),
		extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModeEffectful))
	pure := extension.HookCapability("hook-pure", extension.SingleHook(extension.BoundaryRequest, extension.HookModePure))
	var mode atomic.Int32
	requestConsequence := harness.RequestConsequence{Prompt: stringPointer("request prompt"), Messages: []model.Message{}}
	requestHook := func(context.Context, extension.CallContext, harness.RequestHookInput) (harness.RequestConsequence, error) {
		switch mode.Load() {
		case 0:
			return requestConsequence, nil
		case 1:
			return harness.RequestConsequence{}, errors.New("plain hook failure")
		case 2:
			return harness.RequestConsequence{}, &harness.EffectError{Interrupted: true, Err: errors.New("interrupted hook failure")}
		case 3:
			return harness.RequestConsequence{}, &harness.EffectError{Interrupted: true, Evidence: harness.ExecutionUnknown, Err: errors.New("interrupted hook failure")}
		}
		return harness.RequestConsequence{}, nil
	}
	beforeToolHook := func(context.Context, extension.CallContext, model.ToolCall) (json.RawMessage, error) {
		switch mode.Load() {
		case 1:
			return nil, errors.New("plain before-tool failure")
		case 2:
			return nil, &harness.EffectError{Interrupted: true, Err: errors.New("interrupted before-tool failure")}
		}
		return json.RawMessage(`{"repaired":true}`), nil
	}
	pureHook := func(context.Context, extension.CallContext, harness.RequestHookInput) (harness.RequestConsequence, error) {
		switch mode.Load() {
		case 1:
			return harness.RequestConsequence{}, errors.New("plain pure failure")
		case 2:
			return harness.RequestConsequence{}, &harness.EffectError{Interrupted: true, Err: errors.New("interrupted pure failure")}
		}
		return harness.RequestConsequence{Prompt: stringPointer("pure prompt")}, nil
	}
	native := singleNative(baseDeclaration(effectful, pure), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"hook-effectful": {Request: requestHook, BeforeTool: beforeToolHook},
			"hook-pure":      {Request: pureHook},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	requestInput, err := worker.EncodeRequestHookParams(testRequestHookInput())
	if err != nil {
		t.Fatalf("encoding request hook input: %v", err)
	}
	beforeToolInput, err := worker.EncodeBeforeToolHookParams(testToolCall())
	if err != nil {
		t.Fatalf("encoding before-tool hook input: %v", err)
	}

	mode.Store(0)
	success := p.call("request-success", instanceTarget("hook-effectful"), "call-token", "request", requestInput)
	if success.Error != nil {
		t.Fatalf("successful request hook failed: %v", success.Error)
	}
	assertBareResult(t, success.Result)
	decoded, err := worker.DecodeRequestHookResult(success.Result)
	if err != nil || !reflect.DeepEqual(decoded, requestConsequence) {
		t.Fatalf("request hook success = %#v / %v", decoded, err)
	}

	mode.Store(1)
	plain := p.call("request-plain", instanceTarget("hook-effectful"), "call-token", "request", requestInput)
	if plain.Error == nil || plain.Error.Class != worker.ClassInternal || plain.Error.Message != "plain hook failure" || plain.Error.Interrupted || plain.Error.Evidence != "" {
		t.Fatalf("plain hook failure = %+v, want the ordinary error frame", plain.Error)
	}
	if len(plain.Result) != 0 {
		t.Fatalf("plain hook failure carried a result: %s", plain.Result)
	}

	mode.Store(2)
	interrupted := p.call("request-interrupted", instanceTarget("hook-effectful"), "call-token", "request", requestInput)
	if interrupted.Error == nil || !interrupted.Error.Interrupted || interrupted.Error.Evidence != "" {
		t.Fatalf("interrupted hook failure = %+v, want declared interruption", interrupted.Error)
	}
	pureInterrupted := p.call("pure-interrupted", instanceTarget("hook-pure"), "call-token", "request", requestInput)
	if pureInterrupted.Error == nil || !pureInterrupted.Error.Interrupted || pureInterrupted.Error.Evidence != "" {
		t.Fatalf("interrupted pure hook failure = %+v, want declared interruption", pureInterrupted.Error)
	}
	beforeInterrupted := p.call("before-interrupted", instanceTarget("hook-effectful"), "call-token", "before_tool", beforeToolInput)
	if beforeInterrupted.Error == nil || !beforeInterrupted.Error.Interrupted || beforeInterrupted.Error.Evidence != "" {
		t.Fatalf("interrupted before-tool failure = %+v, want declared interruption", beforeInterrupted.Error)
	}

	mode.Store(3)
	known := p.call("request-known", instanceTarget("hook-effectful"), "call-token", "request", requestInput)
	if known.Error == nil || !known.Error.Interrupted || known.Error.Evidence != harness.ExecutionUnknown {
		t.Fatalf("effectful hook failure = %+v, want unknown execution knowledge", known.Error)
	}

	mode.Store(1)
	beforePlain := p.call("before-plain", instanceTarget("hook-effectful"), "call-token", "before_tool", beforeToolInput)
	if beforePlain.Error == nil || beforePlain.Error.Class != worker.ClassInternal || beforePlain.Error.Message != "plain before-tool failure" || beforePlain.Error.Interrupted {
		t.Fatalf("plain before-tool failure = %+v, want the ordinary error frame", beforePlain.Error)
	}

	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestRepeatedDisposeEndsServe(t *testing.T) {
	closeStarted := make(chan struct{}, 2)
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{
			Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
				"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage("null")}, nil
				},
			}}},
			Close: func(ctx context.Context, _ extension.CallContext) error {
				closeStarted <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.DisposeParams{InstanceID: "instance", Context: testRoutingContext("dispose-token")})
	if err != nil {
		t.Fatalf("encoding dispose params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-1", Method: worker.MethodDispose, Params: params})
	select {
	case <-closeStarted:
	case <-time.After(serveTimeout):
		t.Fatal("first dispose did not close the instance")
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-2", Method: worker.MethodDispose, Params: params})
	serveErr := p.wait()
	if serveErr == nil || !strings.Contains(serveErr.Error(), "disposing or disposed instance") {
		t.Fatalf("Serve = %v, want the bad-route failure", serveErr)
	}
}

func TestEmptyHostcallArgumentsStayEmpty(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("echo", `{"type":"object"}`, `{"type":"null"}`))
	handler := func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		return callContext.Call(ctx, "dep", "ping", nil)
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"echo": handler}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	var hostcallParams worker.HostcallParams
	if err := json.Unmarshal(hostcall.Params, &hostcallParams); err != nil {
		t.Fatalf("decoding hostcall params: %v", err)
	}
	if len(hostcallParams.Arguments) != 0 {
		t.Fatalf("empty hostcall arguments became %q", hostcallParams.Arguments)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(hostcall.Params, &members); err != nil {
		t.Fatalf("decoding hostcall members: %v", err)
	}
	if got := string(members["arguments"]); got != `""` {
		t.Fatalf("hostcall arguments wire = %s, want the empty base64 string", got)
	}
	result, err := worker.EncodeServiceResult(extension.Result{Value: json.RawMessage("null")})
	if err != nil {
		t.Fatalf("encoding hostcall result: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Result: result})
	response := p.readResponse("call-1")
	if response.Error != nil {
		t.Fatalf("call failed: %v", response.Error)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestProgressFloodKeepsOutstandingCall(t *testing.T) {
	release := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("wait", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handler := func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		select {
		case <-release:
			return extension.Result{Value: json.RawMessage("null")}, nil
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		}
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"wait": handler}},
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
	for i := 0; i < 500; i++ {
		p.send(worker.Frame{Kind: worker.KindProgress, ID: fmt.Sprintf("progress-%d", i), Value: json.RawMessage(`{"n":1}`)})
	}
	close(release)
	response := p.readResponse("call-1")
	if response.Error != nil {
		t.Fatalf("outstanding call failed after the progress flood: %v", response.Error)
	}
	if result, err := worker.DecodeServiceResult(response.Result); err != nil || string(result.Value) != "null" {
		t.Fatalf("outstanding call result = %+v, %v", result, err)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestPendingHostcallSettlesOnTerminalFault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(id string) worker.Frame
	}{
		{"malformed terminal", func(id string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: id, Result: json.RawMessage(`{"value":"bnVsbA=="}`), Error: &worker.Error{Class: worker.ClassInternal, Message: "both arms"}}
		}},
		{"unknown terminal", func(string) worker.Frame {
			return worker.Frame{Kind: worker.KindResponse, ID: "w-unknown", Result: json.RawMessage(`{"value":"bnVsbA=="}`)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan error, 1)
			capability := extension.ServiceCapability("service-1", "service/v1",
				extension.ServiceIOMethod("echo", `{"type":"object"}`, `{"type":"null"}`))
			handler := func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				_, err := callContext.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
				observed <- err
				return extension.Result{}, err
			}
			native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{
					"service-1": {Service: map[string]extension.Handler{"echo": handler}},
				}}, nil
			})
			p := startPeer(t, native)
			p.activate()
			p.open("open-token")
			params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(`{}`)})
			if err != nil {
				t.Fatalf("encoding call params: %v", err)
			}
			p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
			hostcall := p.readRequest(worker.MethodHostcall)
			p.send(tc.respond(hostcall.ID))
			if err := p.wait(); err == nil {
				t.Fatal("Serve accepted a terminal fault")
			}
			select {
			case err := <-observed:
				if err == nil {
					t.Fatal("pending hostcall author callback observed no error")
				}
			case <-time.After(serveTimeout):
				t.Fatal("pending hostcall author callback did not settle")
			}
		})
	}
}

func disposeCapabilityNative(close func(context.Context, extension.CallContext) error) extension.Native {
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	return singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{
			Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
				"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage("null")}, nil
				},
			}}},
			Close: close,
		}, nil
	})
}

func TestCallAfterDisposeFrameEndsServe(t *testing.T) {
	t.Run("while disposing", func(t *testing.T) {
		started := make(chan struct{}, 2)
		closeStarted := make(chan struct{})
		capability := extension.ServiceCapability("service-1", "service/v1",
			extension.ServiceIOMethod("wait", `{}`, `{"type":"null"}`))
		native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{
				Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{
					"wait": func(ctx context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
						started <- struct{}{}
						<-ctx.Done()
						return extension.Result{}, ctx.Err()
					},
				}}},
				Close: func(ctx context.Context, _ extension.CallContext) error {
					close(closeStarted)
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		})
		p := startPeer(t, native)
		p.activate()
		p.open("open-token")

		for i := 0; i < 2; i++ {
			params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "wait", Arguments: worker.JSONBytes(`{}`)})
			if err != nil {
				t.Fatalf("encoding call params: %v", err)
			}
			p.send(worker.Frame{Kind: worker.KindRequest, ID: fmt.Sprintf("call-%d", i), Method: worker.MethodCall, Params: params})
		}
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(serveTimeout):
				t.Fatal("call was not admitted before the dispose frame")
			}
		}
		dispose, err := json.Marshal(worker.DisposeParams{InstanceID: "instance", Context: testRoutingContext("dispose-token")})
		if err != nil {
			t.Fatalf("encoding dispose params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-1", Method: worker.MethodDispose, Params: dispose})
		select {
		case <-closeStarted:
		case <-time.After(serveTimeout):
			t.Fatal("dispose did not close the instance")
		}
		// Both calls written before the dispose frame were admitted, cancelled
		// by the dispose and answered exactly once.
		for i := 0; i < 2; i++ {
			response := p.readResponse(fmt.Sprintf("call-%d", i))
			if response.Error == nil || response.Error.Class != worker.ClassCancelled {
				t.Fatalf("call %d response = %+v, want one cancelled answer", i, response)
			}
		}
		violating, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "wait", Arguments: worker.JSONBytes(`{}`)})
		if err != nil {
			t.Fatalf("encoding violating call params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-after-dispose", Method: worker.MethodCall, Params: violating})
		serveErr := p.wait()
		if serveErr == nil || !strings.Contains(serveErr.Error(), "disposing or disposed instance") {
			t.Fatalf("Serve = %v, want the bad-route failure", serveErr)
		}
	})

	t.Run("after disposal", func(t *testing.T) {
		p := startPeer(t, disposeCapabilityNative(func(context.Context, extension.CallContext) error { return nil }))
		p.activate()
		p.open("open-token")
		dispose, err := json.Marshal(worker.DisposeParams{InstanceID: "instance", Context: testRoutingContext("dispose-token")})
		if err != nil {
			t.Fatalf("encoding dispose params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "dispose-1", Method: worker.MethodDispose, Params: dispose})
		if frame := p.readResponse("dispose-1"); frame.Error != nil {
			t.Fatalf("dispose failed: %v", frame.Error)
		}
		params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "ping", Arguments: worker.JSONBytes(`{}`)})
		if err != nil {
			t.Fatalf("encoding call params: %v", err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-after-dispose", Method: worker.MethodCall, Params: params})
		serveErr := p.wait()
		if serveErr == nil || !strings.Contains(serveErr.Error(), "unknown instance") {
			t.Fatalf("Serve = %v, want the unknown-instance failure", serveErr)
		}
	})
}

func TestHostcallCancellationWaitsForResponse(t *testing.T) {
	control := make(chan struct{})
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("echo", `{"type":"object"}`, `{"type":"null"}`))
	handler := func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		child, cancel := context.WithCancel(ctx)
		go func() {
			<-control
			cancel()
		}()
		result, err := callContext.Call(child, "dep", "ping", json.RawMessage(`{}`))
		cancel()
		return result, err
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: map[string]extension.Handler{"echo": handler}},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "echo", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding call params: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-1", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	close(control)
	p.readCancel(hostcall.ID)

	// The worker must still accept exactly one response for the cancelled
	// hostcall and let the author callback observe it.
	result, err := worker.EncodeServiceResult(extension.Result{Value: json.RawMessage("null")})
	if err != nil {
		t.Fatalf("encoding hostcall result: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Result: result})
	response := p.readResponse("call-1")
	if response.Error != nil {
		t.Fatalf("cancelled hostcall did not wait for its response: %v", response.Error)
	}
	decoded, err := worker.DecodeServiceResult(response.Result)
	if err != nil || string(decoded.Value) != "null" {
		t.Fatalf("call result = %+v, %v", decoded, err)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestOpenedInstanceOwnsCapabilityAndServiceMaps(t *testing.T) {
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceMethod("ping", extension.ModeServiceIO, `{}`,
			`{"type":"object","required":["n"],"properties":{"n":{"type":"integer"}},"additionalProperties":false}`, "", ""))
	opens := 0
	shared := map[string]extension.Handler{
		"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
			return extension.Result{Value: json.RawMessage(`{"n":1}`)}, nil
		},
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		opens++
		if opens == 2 {
			shared["ping"] = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
				return extension.Result{Value: json.RawMessage(`{"n":2}`)}, nil
			}
		}
		return extension.Instance{Capabilities: map[string]extension.Implementation{
			"service-1": {Service: shared},
		}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.openInstance("one")
	p.openInstance("two")

	call := func(instance, id string) string {
		t.Helper()
		params, err := json.Marshal(worker.CallParams{
			Target:    worker.Target{Kind: worker.TargetInstance, CapabilityID: "service-1", InstanceID: instance},
			Context:   testRoutingContext("call-token"),
			Method:    "ping",
			Arguments: worker.JSONBytes(`{}`),
		})
		if err != nil {
			t.Fatalf("encoding %s call: %v", id, err)
		}
		p.send(worker.Frame{Kind: worker.KindRequest, ID: id, Method: worker.MethodCall, Params: params})
		response := p.readResponse(id)
		if response.Error != nil {
			t.Fatalf("%s call failed: %v", id, response.Error)
		}
		result, err := worker.DecodeServiceResult(response.Result)
		if err != nil {
			t.Fatalf("decoding %s result: %v", id, err)
		}
		return string(result.Value)
	}
	if got := call("one", "call-one"); got != `{"n":1}` {
		t.Fatalf("first instance handler changed to %s, want the accepted {\"n\":1}", got)
	}
	if got := call("two", "call-two"); got != `{"n":2}` {
		t.Fatalf("second instance handler = %s, want {\"n\":2}", got)
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestCancelledClassUnwrapsContext(t *testing.T) {
	if !errors.Is(&worker.Error{Class: worker.ClassCancelled}, context.Canceled) {
		t.Fatal("cancelled class does not unwrap to context.Canceled")
	}
	observed := make(chan bool, 1)
	capability := extension.ServiceCapability("service-1", "service/v1",
		extension.ServiceIOMethod("nested", `{"type":"object","additionalProperties":false}`, `{"type":"null"}`))
	handler := func(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		_, err := callContext.Call(ctx, "dep", "ping", json.RawMessage(`{}`))
		observed <- errors.Is(err, context.Canceled)
		return extension.Result{}, err
	}
	native := singleNative(baseDeclaration(capability), nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"service-1": {Service: map[string]extension.Handler{"nested": handler}}}}, nil
	})
	p := startPeer(t, native)
	p.activate()
	p.open("open-token")
	params, err := json.Marshal(worker.CallParams{Target: instanceTarget("service-1"), Context: testRoutingContext("call-token"), Method: "nested", Arguments: worker.JSONBytes(`{}`)})
	if err != nil {
		t.Fatalf("encoding nested call: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindRequest, ID: "call-nested", Method: worker.MethodCall, Params: params})
	hostcall := p.readRequest(worker.MethodHostcall)
	p.send(worker.Frame{Kind: worker.KindResponse, ID: hostcall.ID, Error: &worker.Error{Class: worker.ClassCancelled, Message: "cancelled"}})
	response := p.readResponse("call-nested")
	if response.Error == nil || response.Error.Class != worker.ClassCancelled {
		t.Fatalf("nested cancelled error = %+v, want the cancelled class", response)
	}
	if !<-observed {
		t.Fatal("hostcall cancelled error does not satisfy errors.Is(err, context.Canceled)")
	}
	if err := p.finish(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}
