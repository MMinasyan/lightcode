package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

const executableTestTimeout = 10 * time.Second

// testExecutablePeer is one in-process worker speaking the protocol over pipes.
type testExecutablePeer struct {
	t       *testing.T
	auto    bool
	handler func(*testExecutablePeer, worker.Frame)
	input   *io.PipeReader
	output  *io.PipeWriter
	reader  *worker.Reader
	writer  *worker.Writer
	frames  chan worker.Frame
	allow   chan struct{}
	done    chan struct{}
	finish  sync.Once
}

// testExecutableProcess is one peer's host-visible process.
type testExecutableProcess struct {
	stdin  io.WriteCloser
	stdout io.Reader
	peer   *testExecutablePeer
}

func (p *testExecutableProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *testExecutableProcess) Stdout() io.Reader     { return p.stdout }
func (p *testExecutableProcess) Wait() error           { <-p.peer.done; return nil }
func (p *testExecutableProcess) Kill() error           { p.peer.exit(); return nil }

// closeSignal closes the peer when the host closes its stdin end.
type closeSignal struct {
	io.WriteCloser
	once sync.Once
	exit func()
}

func (w *closeSignal) Close() error {
	w.once.Do(w.exit)
	return w.WriteCloser.Close()
}

func newTestExecutablePeer(t *testing.T, auto bool, handler func(*testExecutablePeer, worker.Frame)) *testExecutablePeer {
	return &testExecutablePeer{
		t:       t,
		auto:    auto,
		handler: handler,
		frames:  make(chan worker.Frame, 64),
		allow:   make(chan struct{}, 64),
		done:    make(chan struct{}),
	}
}

func (p *testExecutablePeer) process() *testExecutableProcess {
	peerIn, hostIn := io.Pipe()
	hostOut, peerOut := io.Pipe()
	p.input = peerIn
	p.output = peerOut
	p.reader = worker.NewReader(peerIn)
	p.writer = worker.NewWriter(peerOut)
	go p.run()
	return &testExecutableProcess{
		stdin:  &closeSignal{WriteCloser: hostIn, exit: p.exit},
		stdout: hostOut,
		peer:   p,
	}
}

func (p *testExecutablePeer) run() {
	defer p.exit()
	for {
		if !p.auto {
			select {
			case <-p.allow:
			case <-p.done:
				return
			}
		}
		frame, err := p.reader.ReadFrame()
		if err != nil {
			return
		}
		if p.handler != nil {
			p.handler(p, frame)
			continue
		}
		select {
		case p.frames <- frame:
		case <-p.done:
			return
		}
	}
}

// exit ends the simulated process. It is idempotent.
func (p *testExecutablePeer) exit() {
	p.finish.Do(func() {
		close(p.done)
		if p.input != nil {
			_ = p.input.Close()
		}
		if p.output != nil {
			_ = p.output.Close()
		}
	})
}

// allowOne permits one inbound frame in manual mode.
func (p *testExecutablePeer) allowOne() {
	select {
	case p.allow <- struct{}{}:
	default:
		p.t.Fatal("peer allow queue is full")
	}
}

// next returns the next frame in manual mode after permitting it.
func (p *testExecutablePeer) next() worker.Frame {
	p.t.Helper()
	p.allowOne()
	return p.await()
}

// await returns the next frame without permitting a read.
func (p *testExecutablePeer) await() worker.Frame {
	p.t.Helper()
	select {
	case frame := <-p.frames:
		return frame
	case <-time.After(executableTestTimeout):
		p.t.Fatal("timed out waiting for a worker frame")
		return worker.Frame{}
	}
}

// expectRequest asserts the next frame is a request with the given method.
func (p *testExecutablePeer) expectRequest(method string) worker.Frame {
	p.t.Helper()
	frame := p.next()
	if frame.Kind != worker.KindRequest || frame.Method != method {
		p.t.Fatalf("frame = %q/%q, want request/%q", frame.Kind, frame.Method, method)
	}
	return frame
}

func (p *testExecutablePeer) send(frame worker.Frame) {
	p.t.Helper()
	if err := p.writer.WriteFrame(frame); err != nil {
		p.t.Fatalf("writing %q frame: %v", frame.Kind, err)
	}
}

func (p *testExecutablePeer) sendValue(id string, value any) {
	p.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		p.t.Fatalf("marshaling response: %v", err)
	}
	p.send(worker.Frame{Kind: worker.KindResponse, ID: id, Result: raw})
}

func (p *testExecutablePeer) sendNull(id string) {
	p.t.Helper()
	p.send(worker.Frame{Kind: worker.KindResponse, ID: id, Result: json.RawMessage("null")})
}

// testPeerSpawner replaces the process seam with a factory of in-process
// peers, one per activation.
type testPeerSpawner struct {
	t       *testing.T
	factory func() *testExecutablePeer
	mu      sync.Mutex
	spawned []*testExecutablePeer
}

func (s *testPeerSpawner) install() {
	old := spawnExecutable
	spawnExecutable = func(_ context.Context, _ string, environment []string) (executableProcess, error) {
		s.mu.Lock()
		peer := s.factory()
		s.spawned = append(s.spawned, peer)
		s.mu.Unlock()
		return peer.process(), nil
	}
	s.t.Cleanup(func() { spawnExecutable = old })
}

func (s *testPeerSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spawned)
}

func (s *testPeerSpawner) last() *testExecutablePeer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spawned) == 0 {
		s.t.Fatal("no peer was spawned")
	}
	return s.spawned[len(s.spawned)-1]
}

// executableTestDeclaration is one executable extension carrying every fixed
// contribution kind and one declared credential binding.
func executableTestDeclaration() extension.Declaration {
	credentials, _ := extension.HostCapability(extension.ContractCredentials)
	return extension.Declaration{
		ID:           "peer",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"token":{"type":"string"}},"additionalProperties":false}`),
		Secrets:      []extension.SecretRef{{Field: "/token", Lifetime: extension.SecretActivation}},
		Components: []extension.Component{{
			ID:       "peer.component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"peer.tool", "peer.service", "peer.hooks", "peer.strategy"},
			Requires: []extension.Binding{{
				Name:         "credentials",
				Provider:     extension.HostProvider,
				CapabilityID: extension.ContractCredentials,
				Definition:   credentials,
			}},
		}},
		Capabilities: []extension.Capability{
			extension.ToolCapability("peer.tool"),
			extension.ServiceCapability("peer.service", "peer.service/v1",
				extension.ServiceMethod("echo", extension.ModeServiceIO, `{"type":"object"}`, `{}`, "", "")),
			extension.HookCapability("peer.hooks",
				extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure),
				extension.SingleHook(extension.BoundaryRequest, extension.HookModeEffectful),
				extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModePure),
				extension.SingleHook(extension.BoundaryAfterTool, extension.HookModePure)),
			extension.StrategyCapability("peer.strategy", false),
		},
	}
}

func executableTestToolContext(instanceID string) extension.ToolContext {
	return extension.ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Revision:      "1",
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Calls: extension.CallContext{
			ExtensionID: "peer",
			ComponentID: "peer.component",
			InstanceID:  instanceID,
			Lifetime:    extension.LifetimeHarness,
			Revision:    "1",
		},
	}
}

func executableTestCallContext(instanceID string) extension.CallContext {
	return extension.CallContext{
		ExtensionID: "peer",
		ComponentID: "peer.component",
		InstanceID:  instanceID,
		Lifetime:    extension.LifetimeHarness,
		Revision:    "1",
	}
}

func activatePeer(t *testing.T, peer *testExecutablePeer, declaration extension.Declaration) {
	t.Helper()
	var described []string
	for _, capability := range declaration.Capabilities {
		if capability.Kind == extension.KindTool {
			described = append(described, capability.ID)
		}
	}
	sort.Strings(described)
	frame := peer.expectRequest(worker.MethodActivate)
	peer.sendValue(frame.ID, worker.ActivateResult{Declaration: worker.NewDeclaration(declaration), Described: described})
}

func respondNull(peer *testExecutablePeer, method string) worker.Frame {
	frame := peer.expectRequest(method)
	peer.sendNull(frame.ID)
	return frame
}

func acquireTestActivation(t *testing.T, supervisor *executableSupervisor, executable executableExtension, peer *testExecutablePeer) {
	t.Helper()
	acquired := make(chan error, 1)
	go func() { acquired <- supervisor.acquire(context.Background(), executable, nil) }()
	activatePeer(t, peer, executable.Declaration)
	if err := <-acquired; err != nil {
		t.Fatalf("acquire: %v", err)
	}
}

func promptAssembly() prompt.AssemblyInput {
	return prompt.AssemblyInput{Mode: prompt.ModeNone}
}

// encodeValue runs one worker encoder with a single-value result for tests.
func encodeValue[T any](t *testing.T, encode func(T) (json.RawMessage, error), value T) json.RawMessage {
	t.Helper()
	raw, err := encode(value)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return raw
}

// toolOnlyDeclaration is one executable extension providing a single tool.
func toolOnlyDeclaration() extension.Declaration {
	return extension.Declaration{
		ID:           "peer",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		Components: []extension.Component{{
			ID:       "peer.component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"peer.tool"},
		}},
		Capabilities: []extension.Capability{extension.ToolCapability("peer.tool")},
	}
}

func startTestExecutable(t *testing.T, supervisor *executableSupervisor, executable executableExtension, peer *testExecutablePeer) extension.Instance {
	t.Helper()
	acquireTestActivation(t, supervisor, executable, peer)
	native, err := supervisor.native(executable)
	if err != nil {
		t.Fatalf("native: %v", err)
	}
	instanceContext := extension.InstanceContext{
		ExtensionID: executable.Declaration.ID,
		ComponentID: "peer.component",
		Version:     executable.Declaration.Version,
		InstanceID:  "instance-1",
		Lifetime:    extension.LifetimeHarness,
		Scope:       extension.ScopeIdentity{Kind: extension.LifetimeHarness, Revision: "1"},
		Revision:    "1",
		Section:     json.RawMessage(`{"enabled":true}`),
	}
	type openResult struct {
		instance extension.Instance
		err      error
	}
	opened := make(chan openResult, 1)
	go func() {
		value, err := native.Open(context.Background(), instanceContext)
		opened <- openResult{instance: value, err: err}
	}()
	frame := peer.next()
	if frame.Method != worker.MethodOpen {
		t.Fatalf("open frame method = %q", frame.Method)
	}
	peer.sendNull(frame.ID)
	open := <-opened
	if open.err != nil {
		t.Fatalf("open: %v", open.err)
	}
	return open.instance
}

func eventually(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(executableTestTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func activationPending(activation *executableActivation) (sent int, total int) {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	for _, request := range activation.pending {
		request.mu.Lock()
		if request.sent {
			sent++
		}
		request.mu.Unlock()
		total++
	}
	return sent, total
}

func runToolExecute(t *testing.T, tool extension.Tool, toolContext extension.ToolContext, plan extension.ToolPlan) chan harness.ToolOutcome {
	outcome := make(chan harness.ToolOutcome, 1)
	go func() { outcome <- tool.Execute(context.Background(), toolContext, plan) }()
	return outcome
}

func TestExecutableCallEvidence(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	activation, err := supervisor.live(declaration.ID)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	tool := instance.Capabilities["peer.tool"].Tool
	toolContext := executableTestToolContext("instance-1")
	plan := extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}

	// The writer is blocked on the first frame, the second frame never leaves
	// the queue, and the activation then ends.
	first := runToolExecute(t, tool, toolContext, plan)
	peer.allowOne()
	eventually(t, "first call to leave the writer queue", func() bool {
		sent, _ := activationPending(activation)
		return sent == 1
	})
	second := runToolExecute(t, tool, toolContext, plan)
	eventually(t, "second call to start its write", func() bool {
		sent, _ := activationPending(activation)
		return sent == 2
	})
	third := runToolExecute(t, tool, toolContext, plan)
	eventually(t, "third call to queue behind the blocked writer", func() bool {
		_, total := activationPending(activation)
		return total == 3 && len(activation.out) >= 1
	})
	peer.exit()
	firstOutcome := <-first
	secondOutcome := <-second
	thirdOutcome := <-third
	testassert.Equal(t, firstOutcome.Evidence, harness.ExecutionUnknown)
	testassert.Equal(t, secondOutcome.Evidence, harness.ExecutionUnknown)
	testassert.Equal(t, thirdOutcome.Evidence, harness.ExecutionNotStarted)
}

func TestExecutableLostResult(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool

	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	peer.next() // the call left the queue
	peer.exit()
	got := <-outcome
	testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
}

func TestExecutableMalformedTerminalFailsActivationWithoutRestart(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawned := 0
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer {
		spawned++
		if spawned == 1 {
			return peer
		}
		return newTestExecutablePeer(t, false, nil)
	}}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peer.next()
	peer.send(worker.Frame{Kind: worker.KindResponse, ID: call.ID, Result: json.RawMessage(`[]`)})
	got := <-outcome
	testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)

	// The failed activation is retained without respawn while the set holds it.
	if err := supervisor.acquire(context.Background(), executable, nil); err == nil {
		t.Fatal("acquiring a failed live activation succeeded")
	}
	if count := spawner.count(); count != 1 {
		t.Fatalf("failed activation spawned %d processes, want 1", count)
	}
	supervisor.release(declaration.ID) // still retained
	if err := supervisor.acquire(context.Background(), executable, nil); err == nil {
		t.Fatal("acquiring a failed activation succeeded after one of two holds released")
	}
	supervisor.release(declaration.ID)
	supervisor.release(declaration.ID) // last hold: the next acquisition starts fresh
	fresh := newTestExecutablePeer(t, false, nil)
	spawner.factory = func() *testExecutablePeer {
		spawned++
		return fresh
	}
	acquireTestActivation(t, supervisor, executable, fresh)
	if count := spawner.count(); count != 2 {
		t.Fatalf("fresh activation spawned %d processes in total, want 2", count)
	}
	supervisor.release(declaration.ID)
}

func TestExecutableDeclarationMismatchFailsActivation(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	acquired := make(chan error, 1)
	go func() { acquired <- supervisor.acquire(context.Background(), executable, nil) }()
	frame := peer.expectRequest(worker.MethodActivate)
	other := declaration
	other.Version = "2.0.0"
	peer.sendValue(frame.ID, worker.ActivateResult{Declaration: worker.NewDeclaration(other), Described: []string{"peer.tool"}})
	err := <-acquired
	if err == nil || !strings.Contains(err.Error(), "declaration") {
		t.Fatalf("mismatched declaration error = %v", err)
	}
	supervisor.release(declaration.ID)
}

func TestExecutableHostcallTokenAndClosedRefusal(t *testing.T) {
	declaration := executableTestDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)

	type invocation struct {
		binding string
		method  string
		args    []byte
	}
	invoked := make(chan invocation, 2)
	call := extension.CallContext{
		ExtensionID: "peer", ComponentID: "peer.component", InstanceID: "instance-1",
		Lifetime: extension.LifetimeHarness, Revision: "1",
		Call: func(_ context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
			invoked <- invocation{binding: binding, method: method, args: append([]byte(nil), arguments...)}
			return extension.Result{Value: json.RawMessage(`"secret-value"`)}, nil
		},
	}
	serviceResult := make(chan extension.Result, 1)
	go func() {
		result, err := instance.Capabilities["peer.service"].Service["echo"](context.Background(), call, json.RawMessage(`{"query":1}`))
		if err != nil {
			t.Errorf("service: %v", err)
		}
		serviceResult <- result
	}()
	serviceFrame := peer.next()
	var serviceParams worker.CallParams
	if err := json.Unmarshal(serviceFrame.Params, &serviceParams); err != nil {
		t.Fatalf("service params: %v", err)
	}
	hostcall := worker.Frame{Kind: worker.KindRequest, ID: "w1", Method: worker.MethodHostcall}
	hostcall.Params, _ = json.Marshal(worker.HostcallParams{
		Context:   worker.Context{Token: serviceParams.Context.Token},
		Binding:   "credentials",
		Method:    "get",
		Arguments: worker.JSONBytes(`{"field":"/token"}`),
	})
	peer.send(hostcall)
	response := peer.next()
	if response.Kind != worker.KindResponse || response.ID != "w1" || response.Error != nil {
		t.Fatalf("hostcall response = %+v", response)
	}
	decoded, err := worker.DecodeServiceResult(response.Result)
	if err != nil {
		t.Fatalf("hostcall result: %v", err)
	}
	if !bytes.Equal(decoded.Value, json.RawMessage(`"secret-value"`)) {
		t.Fatalf("hostcall value = %q", decoded.Value)
	}
	seen := <-invoked
	if seen.binding != "credentials" || seen.method != "get" || !bytes.Equal(seen.args, []byte(`{"field":"/token"}`)) {
		t.Fatalf("hostcall invocation = %+v", seen)
	}

	// The service's own terminal ends its token; later hostcalls are closed.
	peer.sendValue(serviceFrame.ID, encodeValue(t, worker.EncodeServiceResult, extension.Result{Value: json.RawMessage(`"done"`)}))
	<-serviceResult
	late := worker.Frame{Kind: worker.KindRequest, ID: "w2", Method: worker.MethodHostcall}
	late.Params, _ = json.Marshal(worker.HostcallParams{
		Context: worker.Context{Token: serviceParams.Context.Token},
		Binding: "credentials", Method: "get", Arguments: worker.JSONBytes(`{"field":"/token"}`),
	})
	peer.send(late)
	lateResponse := peer.next()
	if lateResponse.Error == nil || lateResponse.Error.Class != worker.ClassClosed {
		t.Fatalf("late hostcall response = %+v", lateResponse)
	}
	forged := worker.Frame{Kind: worker.KindRequest, ID: "w3", Method: worker.MethodHostcall}
	forged.Params, _ = json.Marshal(worker.HostcallParams{
		Context: worker.Context{Token: "forged"}, Binding: "credentials", Method: "get", Arguments: worker.JSONBytes(`{}`),
	})
	peer.send(forged)
	forgedResponse := peer.next()
	if forgedResponse.Error == nil || forgedResponse.Error.Class != worker.ClassClosed {
		t.Fatalf("forged hostcall response = %+v", forgedResponse)
	}
	select {
	case extra := <-invoked:
		t.Fatalf("closed hostcall still invoked %+v", extra)
	default:
	}
}

func TestExecutableUnrelatedActivationSurvivesFailure(t *testing.T) {
	first := toolOnlyDeclaration()
	second := toolOnlyDeclaration()
	second.ID = "other"
	second.Components[0].ID = "other.component"
	second.Components[0].Provides = []string{"other.tool"}
	second.Capabilities = []extension.Capability{extension.ToolCapability("other.tool")}
	peerOne := newTestExecutablePeer(t, false, nil)
	peerTwo := newTestExecutablePeer(t, false, nil)
	peers := []*testExecutablePeer{peerOne, peerTwo}
	next := 0
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer {
		peer := peers[next]
		next++
		return peer
	}}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	one := executableExtension{Declaration: first, EntryPoint: "one"}
	two := executableExtension{Declaration: second, EntryPoint: "two"}
	instanceOne := startTestExecutable(t, supervisor, one, peerOne)
	acquireTestActivation(t, supervisor, two, peerTwo)
	nativeTwo, err := supervisor.native(two)
	if err != nil {
		t.Fatalf("native: %v", err)
	}
	described := make(chan extension.ToolDescription, 1)
	go func() {
		description, err := nativeTwo.Describe["other.tool"](context.Background(), extension.DescribeInput{
			Session: harness.SessionIdentity{SessionID: "s", Workspace: "/w"},
			Model:   model.ModelRef{Provider: "p", Model: "m"},
		})
		if err != nil {
			t.Errorf("describe other: %v", err)
		}
		described <- description
	}()
	describeFrame := peerTwo.next()
	if describeFrame.Method != worker.MethodCall {
		t.Fatalf("other describe frame = %q", describeFrame.Method)
	}
	peerTwo.sendValue(describeFrame.ID, encodeValue(t, worker.EncodeDescribeResult, extension.ToolDescription{
		Definition: model.ToolDefinition{Name: "other.tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		Available:  true,
	}))
	if got := <-described; got.Definition.Name != "other.tool" {
		t.Fatalf("other description = %+v", got)
	}
	defer supervisor.release(second.ID)

	tool := instanceOne.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peerOne.next()
	peerOne.send(worker.Frame{Kind: worker.KindResponse, ID: call.ID, Result: json.RawMessage(`[]`)})
	<-outcome
	if _, err := supervisor.live(first.ID); err == nil {
		t.Fatal("broken activation is still live")
	}
	if _, err := supervisor.live(second.ID); err != nil {
		t.Fatalf("unrelated activation failed: %v", err)
	}
	supervisor.release(first.ID)
}

func TestExecutableEnvironmentUsesSecretExcludedProducer(t *testing.T) {
	declaration := executableTestDeclaration()
	t.Setenv("EXEC_PROVIDER_KEY", "provider-secret")
	t.Setenv("EXEC_DECLARED_SECRET", "declared-secret")
	t.Setenv("EXEC_UNRELATED", "kept")
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterSimpleNative(declaration, simpleTool{}, nil), nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", "EXEC_PROVIDER_KEY", `{"m":{"context_window":4096}}`)+`},"extensions":{"peer":{"enabled":true,"token":"EXEC_DECLARED_SECRET"}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	environment := spawner.environment(0)
	for _, name := range []string{"EXEC_PROVIDER_KEY", "EXEC_DECLARED_SECRET"} {
		testassert.Require(t, !slices.ContainsFunc(environment, func(entry string) bool { return strings.HasPrefix(entry, name+"=") }), "worker environment inherited %s", name)
	}
	testassert.Require(t, slices.Contains(environment, "EXEC_UNRELATED=kept"), "worker environment dropped an ordinary entry")
}

// newExecutableConfigurationService builds one configuration service whose
// registration owns the supplied executable extension.
func newExecutableConfigurationService(t *testing.T, h *serviceHarness, declaration extension.Declaration, spawner interface{ install() }) (*configurationService, *executableSupervisor) {
	t.Helper()
	var service *configurationService
	supervisor := newExecutableSupervisor(func(ctx context.Context, excluded []string) ([]string, error) {
		return service.executableEnvironment(ctx, excluded)
	})
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	native, err := supervisor.native(executable)
	if err != nil {
		t.Fatalf("native: %v", err)
	}
	composition, err := newNativeComposition([]extension.Native{native})
	if err != nil {
		t.Fatalf("composition: %v", err)
	}
	service = newConfigurationService(context.Background(), composition, h.loader,
		NewFileConfigurationSource(h.configPath),
		NewEnvironmentCredentialSource(filepath.Join(h.home, ".lightcode", ".env")),
		h.locations(), harness.IncludedStrategyBinding(), newObservation())
	service.composition = composition
	service.dataDir = t.TempDir()
	service.executables = []executableExtension{executable}
	service.supervisor = supervisor
	spawner.install()
	return service, supervisor
}

func TestExecutableSectionChangeReusesProcess(t *testing.T) {
	declaration := toolOnlyDeclaration()
	opens := 0
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		native := adapterToolNative(declaration, simpleTool{})
		original := native.Open
		native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			opens++
			return original(ctx, instance)
		}
		return native, nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	first := service.current().resources
	captured, err := service.capturePreparation(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	writeServiceFile(t, h.configPath, `{ "extensions" : { "peer" : { "enabled" : true } } }`)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if service.current().resources == first {
		t.Fatal("section change reused the resource set")
	}
	if count := spawner.count(); count != 1 {
		t.Fatalf("section change spawned %d processes, want one reused activation", count)
	}
	testassert.Equal(t, opens, 2)
	captured.snapshot.resources.release()
	captured.releaseHeldNames()
	service.current().resources.release()
	select {
	case <-spawner.last().leaderDone:
	case <-time.After(executableTestTimeout):
		t.Fatal("worker did not stop after the last release")
	}
}

func TestExecutableDisableKeepsParkedCall(t *testing.T) {
	declaration := toolOnlyDeclaration()
	blocking := newAdapterBlockingTool()
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterToolNative(declaration, blocking), blocking.close
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, supervisor := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	captured, err := service.capturePreparation(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	snapshot := service.current()
	toolContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Invocation:    Invocation{snapshot: snapshot},
	}
	outcome := startPreparedTool(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "peer.tool", toolContext, model.ToolCall{ID: "call-1", Name: "peer.tool", Arguments: json.RawMessage(`{}`)}, context.Background())
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("parked call did not start")
	}
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":false}}}`)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("disable publish: %v", err)
	}
	select {
	case got := <-outcome:
		t.Fatalf("parked call settled during disable: %+v", got)
	default:
	}
	select {
	case <-spawner.last().leaderDone:
		t.Fatal("worker stopped while a captured set still held it")
	default:
	}
	if _, err := supervisor.live(declaration.ID); err != nil {
		t.Fatalf("activation unavailable while held: %v", err)
	}
	blocking.close()
	select {
	case got := <-outcome:
		if got.Result.Content != "released" || got.Evidence != harness.ExecutionObserved {
			t.Fatalf("parked call outcome = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked call did not finish")
	}
	captured.snapshot.resources.release()
	captured.releaseHeldNames()
	select {
	case <-spawner.last().leaderDone:
	case <-time.After(executableTestTimeout):
		t.Fatal("worker did not stop after the last release")
	}
	service.current().resources.release()
}

func TestExecutableWorkerCredentialHostService(t *testing.T) {
	const name = "EXEC_WORKER_SECRET"
	t.Setenv(name, "activation-secret")
	declaration := executableTestDeclaration()
	handler := func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
		return call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/token"}`))
	}
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterSimpleNative(declaration, simpleTool{}, map[string]map[string]extension.Handler{
			"peer.service": {"echo": handler},
		}), nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true,"token":"`+name+`"}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	value, ok := service.current().resources.runtime.capabilities["peer.service"]
	if !ok {
		t.Fatal("service capability is not open")
	}
	result, err := service.current().composition.invokeProvider(context.Background(), value, value.capability.Methods, "echo", json.RawMessage(`{"query":1}`), nil, callIdentity{revision: "1"}, false)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if string(result.Value) != `{"value":"activation-secret"}` {
		t.Fatalf("credential value = %q", result.Value)
	}
}

func TestExecutableDisposeTimeoutDoesNotBlockPublication(t *testing.T) {
	deadline := nativeCloseDeadline
	nativeCloseDeadline = 100 * time.Millisecond
	grace := executableShutdownGrace
	executableShutdownGrace = 200 * time.Millisecond
	defer func() {
		nativeCloseDeadline = deadline
		executableShutdownGrace = grace
	}()

	declaration := toolOnlyDeclaration()
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		release := make(chan struct{})
		native := adapterToolNative(declaration, simpleTool{})
		original := native.Open
		native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			value, err := original(ctx, instance)
			if err != nil {
				return value, err
			}
			value.Close = func(context.Context, extension.CallContext) error {
				<-release
				return nil
			}
			return value, nil
		}
		return native, func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	writeServiceFile(t, h.configPath, `{ "extensions" : { "peer" : { "enabled" : true } } }`)
	published := make(chan error, 1)
	go func() {
		_, err := service.publish(context.Background())
		published <- err
	}()
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("reload publish: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reload publication blocked on an unanswered dispose")
	}
	released := make(chan error, 1)
	go func() { released <- service.current().resources.release() }()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("resource release blocked on an unanswered dispose")
	}
}

func TestExecutableOwnerCloseBoundedUnresponsiveCall(t *testing.T) {
	grace := executableShutdownGrace
	executableShutdownGrace = 200 * time.Millisecond
	defer func() { executableShutdownGrace = grace }()

	declaration := toolOnlyDeclaration()
	blocking := newAdapterBlockingTool()
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterToolNative(declaration, blocking), blocking.close
	}}
	spawner.install()
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	installExecutablePackage(t, dataDir, executableExtension{Declaration: declaration, EntryPoint: "peer"})
	owner, err := open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	set := owner.config.current().resources
	snapshot := owner.config.current()
	toolContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Invocation:    Invocation{snapshot: snapshot},
	}
	tool, err := snapshot.composition.bindTool(selection{scopes: []*scope{set.runtime}, invocation: toolContext.Invocation}, "peer.tool")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	call := model.ToolCall{ID: "call-1", Name: "peer.tool", Arguments: json.RawMessage(`{}`)}
	if _, err := tool.Normalize(context.Background(), toolContext, call); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	prepared := tool.Prepare(context.Background(), toolContext, call)
	callCtx, release, err := set.runtime.enter(context.Background())
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	outcome := make(chan harness.ToolOutcome, 1)
	go func() {
		defer release()
		outcome <- prepared.Execute(callCtx)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- owner.Close(context.Background()) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("owner close did not complete within the grace window")
	}
	select {
	case got := <-outcome:
		testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
		testassert.Equal(t, got.Result.Status, model.ResultInterrupted)
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call did not settle")
	}
}

// adapterWorker is one in-process author-side worker.Serve process over pipes.
type adapterWorker struct {
	stdin      *io.PipeWriter
	stdout     *io.PipeReader
	serveIn    *io.PipeReader
	serveOut   *io.PipeWriter
	cancel     context.CancelFunc
	leaderDone chan struct{}
	leaderOnce sync.Once
	release    func()
}

func newAdapterWorker(t *testing.T, native extension.Native, release func()) *adapterWorker {
	t.Helper()
	serveIn, hostIn := io.Pipe()
	hostOut, serveOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	adapter := &adapterWorker{
		stdin:      hostIn,
		stdout:     hostOut,
		serveIn:    serveIn,
		serveOut:   serveOut,
		cancel:     cancel,
		leaderDone: make(chan struct{}),
		release:    release,
	}
	go func() {
		_ = worker.Serve(ctx, native, serveIn, serveOut)
		_ = serveOut.Close()
		_ = serveIn.Close()
		adapter.finishLeader()
	}()
	return adapter
}

func (w *adapterWorker) finishLeader() {
	w.leaderOnce.Do(func() { close(w.leaderDone) })
}

func (w *adapterWorker) Stdin() io.WriteCloser { return w.stdin }
func (w *adapterWorker) Stdout() io.Reader     { return w.stdout }
func (w *adapterWorker) Wait() error           { <-w.leaderDone; return nil }
func (w *adapterWorker) Kill() error {
	w.cancel()
	w.finishLeader()
	_ = w.stdin.Close()
	_ = w.stdout.Close()
	_ = w.serveIn.Close()
	_ = w.serveOut.Close()
	if w.release != nil {
		w.release()
	}
	return nil
}

// adapterSpawner replaces the process seam with in-process adapter workers.
type adapterSpawner struct {
	t       *testing.T
	factory func() (extension.Native, func())
	mu      sync.Mutex
	workers []*adapterWorker
	envs    [][]string
}

func (s *adapterSpawner) install() {
	old := spawnExecutable
	spawnExecutable = func(_ context.Context, _ string, environment []string) (executableProcess, error) {
		native, release := s.factory()
		adapter := newAdapterWorker(s.t, native, release)
		s.mu.Lock()
		s.workers = append(s.workers, adapter)
		s.envs = append(s.envs, append([]string(nil), environment...))
		s.mu.Unlock()
		return adapter, nil
	}
	s.t.Cleanup(func() { spawnExecutable = old })
}

func (s *adapterSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workers)
}

func (s *adapterSpawner) last() *adapterWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.workers) == 0 {
		s.t.Fatal("no adapter worker was spawned")
	}
	return s.workers[len(s.workers)-1]
}

func (s *adapterSpawner) environment(index int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.envs[index]
}

func adapterDescribe(id string) extension.ToolDescribe {
	return func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{Definition: model.ToolDefinition{Name: id, Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
	}
}

// adapterToolNative builds the ordinary adapter worker for one tool-only
// declaration.
func adapterToolNative(declaration extension.Declaration, tool extension.Tool) extension.Native {
	describes := map[string]extension.ToolDescribe{}
	for _, capability := range declaration.Capabilities {
		if capability.Kind == extension.KindTool {
			describes[capability.ID] = adapterDescribe(capability.ID)
		}
	}
	return extension.Native{
		Declaration: declaration,
		Describe:    describes,
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			capabilities := map[string]extension.Implementation{}
			for _, capability := range declaration.Capabilities {
				if capability.Kind == extension.KindTool {
					capabilities[capability.ID] = extension.Implementation{Tool: tool}
				}
			}
			return extension.Instance{Capabilities: capabilities}, nil
		},
	}
}

// adapterBlockingTool blocks Execute until release, ignoring cancellation.
type adapterBlockingTool struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newAdapterBlockingTool() *adapterBlockingTool {
	return &adapterBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
}

func (t *adapterBlockingTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (t *adapterBlockingTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
}
func (t *adapterBlockingTool) Execute(_ context.Context, context extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	t.once.Do(func() { close(t.entered) })
	<-t.release
	return harness.ToolOutcome{Result: model.ToolResult{CallID: context.Calls.ToolCallID, Status: model.ResultSuccess, Content: "released"}, Evidence: harness.ExecutionObserved}
}
func (t *adapterBlockingTool) close() {
	select {
	case <-t.release:
	default:
		close(t.release)
	}
}

// preparedToolOutcome runs one selected tool through the shared
// extensionTool -> PreparedTool gate, not the raw capability implementation.
func preparedToolOutcome(t *testing.T, composition *composition, scopes []*scope, name string, toolContext ToolContext, call model.ToolCall) harness.ToolOutcome {
	t.Helper()
	tool, err := composition.bindTool(selection{scopes: scopes, invocation: toolContext.Invocation}, name)
	if err != nil {
		t.Fatalf("bind tool %q: %v", name, err)
	}
	if _, err := tool.Normalize(context.Background(), toolContext, call); err != nil {
		t.Fatalf("normalize tool %q: %v", name, err)
	}
	prepared := tool.Prepare(context.Background(), toolContext, call)
	if prepared.Immediate != nil {
		return *prepared.Immediate
	}
	if prepared.Execute == nil {
		t.Fatalf("tool %q has no executor or immediate outcome", name)
	}
	return prepared.Execute(context.Background())
}

// adapterSimpleNative builds one in-process adapter worker with a simple
// implementation for every declared capability kind.
func adapterSimpleNative(declaration extension.Declaration, tool extension.Tool, services map[string]map[string]extension.Handler) extension.Native {
	describes := map[string]extension.ToolDescribe{}
	for _, capability := range declaration.Capabilities {
		if capability.Kind == extension.KindTool {
			describes[capability.ID] = adapterDescribe(capability.ID)
		}
	}
	return extension.Native{
		Declaration: declaration,
		Describe:    describes,
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			capabilities := map[string]extension.Implementation{}
			for _, capability := range declaration.Capabilities {
				switch capability.Kind {
				case extension.KindService:
					handlers := map[string]extension.Handler{}
					for _, method := range capability.Methods {
						if provided := services[capability.ID]; provided != nil {
							if handler := provided[method.Name]; handler != nil {
								handlers[method.Name] = handler
								continue
							}
						}
						handlers[method.Name] = func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
							return extension.Result{Value: json.RawMessage(`"ok"`)}, nil
						}
					}
					capabilities[capability.ID] = extension.Implementation{Service: handlers}
				case extension.KindTool:
					capabilities[capability.ID] = extension.Implementation{Tool: tool}
				case extension.KindHook:
					implementation := extension.Implementation{
						Preparation: func(context.Context, extension.CallContext, extension.PreparationInput) (extension.PreparationConsequence, error) {
							return extension.PreparationConsequence{}, nil
						},
						Request: func(context.Context, extension.CallContext, harness.RequestHookInput) (harness.RequestConsequence, error) {
							return harness.RequestConsequence{}, nil
						},
						BeforeTool: func(_ context.Context, _ extension.CallContext, call model.ToolCall) (json.RawMessage, error) {
							return call.Arguments, nil
						},
						AfterTool: func(_ context.Context, _ extension.CallContext, input harness.ToolOutputInput) (string, error) {
							return input.Content, nil
						},
					}
					capabilities[capability.ID] = implementation
				case extension.KindStrategy:
					capabilities[capability.ID] = extension.Implementation{Strategy: simpleStrategy{}}
				}
			}
			return extension.Instance{Capabilities: capabilities}, nil
		},
	}
}

type simpleTool struct{}

func (simpleTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (simpleTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
}
func (simpleTool) Execute(_ context.Context, context extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: context.Calls.ToolCallID, Status: model.ResultSuccess, Content: "ok"}, Evidence: harness.ExecutionObserved}
}

type simpleStrategy struct{}

func (simpleStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}
func (simpleStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

// startPreparedTool runs one selected tool through the shared PreparedTool
// gate and returns its asynchronous execution outcome.
func startPreparedTool(t *testing.T, composition *composition, scopes []*scope, name string, toolContext ToolContext, call model.ToolCall, ctx context.Context) chan harness.ToolOutcome {
	t.Helper()
	tool, err := composition.bindTool(selection{scopes: scopes, invocation: toolContext.Invocation}, name)
	if err != nil {
		t.Fatalf("bind tool %q: %v", name, err)
	}
	if _, err := tool.Normalize(context.Background(), toolContext, call); err != nil {
		t.Fatalf("normalize tool %q: %v", name, err)
	}
	prepared := tool.Prepare(context.Background(), toolContext, call)
	outcome := make(chan harness.ToolOutcome, 1)
	if prepared.Immediate != nil {
		outcome <- *prepared.Immediate
		return outcome
	}
	if prepared.Execute == nil {
		t.Fatalf("tool %q has no executor", name)
	}
	go func() { outcome <- prepared.Execute(ctx) }()
	return outcome
}

func TestExecutableInvalidFixedResultFailsActivation(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peer.next()
	peer.send(worker.Frame{Kind: worker.KindResponse, ID: call.ID, Result: json.RawMessage(`{"result":{"call_id":"call-1","status":"success","content":"ok"}}`)})
	if got := <-outcome; got.Evidence != harness.ExecutionUnknown {
		t.Fatalf("invalid fixed terminal outcome = %+v", got)
	}
	if _, err := supervisor.live(declaration.ID); err == nil {
		t.Fatal("invalid fixed terminal did not fail the activation")
	}
}

func TestExecutableInvalidServiceResultFailsActivation(t *testing.T) {
	declaration := executableTestDeclaration()
	for i := range declaration.Capabilities {
		if declaration.Capabilities[i].Kind == extension.KindService {
			declaration.Capabilities[i].Methods[0].Success = json.RawMessage(`{"type":"string"}`)
		}
	}
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	result := make(chan error, 1)
	go func() {
		_, err := instance.Capabilities["peer.service"].Service["echo"](context.Background(), executableTestCallContext("instance-1"), json.RawMessage(`{}`))
		result <- err
	}()
	call := peer.next()
	peer.sendValue(call.ID, encodeValue(t, worker.EncodeServiceResult, extension.Result{Value: json.RawMessage(`123`)}))
	<-result
	if _, err := supervisor.live(declaration.ID); err == nil {
		t.Fatal("invalid service terminal did not fail the activation")
	}
}

func TestExecutableReorderedDescribedActivates(t *testing.T) {
	declaration := toolOnlyDeclaration()
	declaration.Components[0].Provides = []string{"peer.alpha", "peer.zeta"}
	declaration.Capabilities = []extension.Capability{extension.ToolCapability("peer.alpha"), extension.ToolCapability("peer.zeta")}
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	acquired := make(chan error, 1)
	go func() { acquired <- supervisor.acquire(context.Background(), executable, nil) }()
	frame := peer.expectRequest(worker.MethodActivate)
	peer.sendValue(frame.ID, worker.ActivateResult{Declaration: worker.NewDeclaration(declaration), Described: []string{"peer.zeta", "peer.alpha"}})
	if err := <-acquired; err != nil {
		t.Fatalf("reordered described list did not activate: %v", err)
	}
	supervisor.release(declaration.ID)
}

func TestExecutableTerminalBeforeFailureKeepsOutcome(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	plan := extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
	firstContext := executableTestToolContext("instance-1")
	firstContext.Calls.ToolCallID = "first"
	secondContext := executableTestToolContext("instance-1")
	secondContext.Calls.ToolCallID = "second"
	first := runToolExecute(t, tool, firstContext, plan)
	second := runToolExecute(t, tool, secondContext, plan)
	frames := map[string]worker.Frame{}
	for i := 0; i < 2; i++ {
		frame := peer.next()
		var params worker.CallParams
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			t.Fatalf("call params: %v", err)
		}
		frames[params.Context.ToolCallID] = frame
	}
	firstFrame, ok := frames["first"]
	if !ok {
		t.Fatalf("first call frame missing: %v", frames)
	}
	secondFrame, ok := frames["second"]
	if !ok {
		t.Fatalf("second call frame missing: %v", frames)
	}
	peer.sendValue(firstFrame.ID, encodeValue(t, worker.EncodeExecuteResult, harness.ToolOutcome{
		Result:   model.ToolResult{CallID: "first", Status: model.ResultSuccess, Content: "accepted"},
		Evidence: harness.ExecutionObserved,
	}))
	peer.send(worker.Frame{Kind: worker.KindResponse, ID: secondFrame.ID, Result: json.RawMessage(`[]`)})
	if got := <-first; got.Result.Content != "accepted" || got.Evidence != harness.ExecutionObserved {
		t.Fatalf("accepted terminal was overwritten: %+v", got)
	}
	if got := <-second; got.Evidence != harness.ExecutionUnknown {
		t.Fatalf("malformed terminal outcome = %+v", got)
	}
}

// leaderExitProcess reports leader exit independently of the stdout pipe and
// takes the descendant down with Kill.
type leaderExitProcess struct {
	inner  *testExecutableProcess
	leader chan struct{}
	killed chan struct{}
	once   sync.Once
}

func (p *leaderExitProcess) Stdin() io.WriteCloser { return p.inner.Stdin() }
func (p *leaderExitProcess) Stdout() io.Reader     { return p.inner.Stdout() }
func (p *leaderExitProcess) Wait() error           { <-p.leader; return nil }
func (p *leaderExitProcess) Kill() error {
	p.once.Do(func() { close(p.killed) })
	return p.inner.Kill()
}

func TestExecutableLeaderExitWithDescendantHoldingStdout(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	process := &leaderExitProcess{inner: peer.process(), leader: make(chan struct{}), killed: make(chan struct{})}
	old := spawnExecutable
	spawnExecutable = func(context.Context, string, []string) (executableProcess, error) { return process, nil }
	t.Cleanup(func() { spawnExecutable = old })

	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	peer.next() // the call was written
	activation, err := supervisor.live(declaration.ID)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	eventually(t, "call write acknowledgement", func() bool {
		sent, _ := activationPending(activation)
		return sent == 1
	})
	close(process.leader)
	select {
	case <-process.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("descendant process group was not killed after leader exit")
	}
	select {
	case got := <-outcome:
		testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
	case <-time.After(2 * time.Second):
		t.Fatal("lost-result call did not settle after leader exit")
	}
}

// gatedKillProcess delays physical termination so tests can observe that
// callers are not released before the process group is gone.
type gatedKillProcess struct {
	inner       *testExecutableProcess
	killStarted chan struct{}
	releaseKill chan struct{}
	once        sync.Once
}

func (p *gatedKillProcess) Stdin() io.WriteCloser { return p.inner.Stdin() }
func (p *gatedKillProcess) Stdout() io.Reader     { return p.inner.Stdout() }
func (p *gatedKillProcess) Wait() error           { return p.inner.Wait() }
func (p *gatedKillProcess) Kill() error {
	p.once.Do(func() { close(p.killStarted) })
	<-p.releaseKill
	return p.inner.Kill()
}

func TestExecutableFailureReleasesAfterProcessGone(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	process := &gatedKillProcess{inner: peer.process(), killStarted: make(chan struct{}), releaseKill: make(chan struct{})}
	old := spawnExecutable
	spawnExecutable = func(context.Context, string, []string) (executableProcess, error) { return process, nil }
	t.Cleanup(func() { spawnExecutable = old })

	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peer.next()
	peer.send(worker.Frame{Kind: worker.KindResponse, ID: call.ID, Result: json.RawMessage(`[]`)})
	select {
	case <-process.killStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("failure did not start process-group termination")
	}
	select {
	case got := <-outcome:
		t.Fatalf("caller released before the process group was gone: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(process.releaseKill)
	got := <-outcome
	testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
}

func TestExecutableHostcallResponseDoesNotBlockTerminalIntake(t *testing.T) {
	declaration := executableTestDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)

	call := extension.CallContext{
		ExtensionID: "peer", ComponentID: "peer.component", InstanceID: "instance-1",
		Lifetime: extension.LifetimeHarness, Revision: "1",
		Call: func(context.Context, string, string, json.RawMessage) (extension.Result, error) {
			return extension.Result{Value: json.RawMessage(`"hostcall"`)}, nil
		},
	}
	result := make(chan extension.Result, 1)
	go func() {
		value, err := instance.Capabilities["peer.service"].Service["echo"](context.Background(), call, json.RawMessage(`{}`))
		if err != nil {
			t.Errorf("service: %v", err)
		}
		result <- value
	}()
	serviceFrame := peer.next()
	var params worker.CallParams
	if err := json.Unmarshal(serviceFrame.Params, &params); err != nil {
		t.Fatalf("service params: %v", err)
	}
	hostcall := worker.Frame{Kind: worker.KindRequest, ID: "w1", Method: worker.MethodHostcall}
	hostcall.Params, _ = json.Marshal(worker.HostcallParams{
		Context: worker.Context{Token: params.Context.Token}, Binding: "credentials", Method: "get", Arguments: worker.JSONBytes(`{"field":"/token"}`),
	})
	peer.send(hostcall)
	// The peer no longer reads stdin, so the queued hostcall response blocks
	// the writer. The service terminal on stdout must still be consumed.
	peer.sendValue(serviceFrame.ID, encodeValue(t, worker.EncodeServiceResult, extension.Result{Value: json.RawMessage(`"done"`)}))
	select {
	case got := <-result:
		if string(got.Value) != `"done"` {
			t.Fatalf("service result = %q", got.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal intake blocked behind a hostcall response")
	}
}

func TestExecutableCancelledCallUnresponsiveWorker(t *testing.T) {
	grace := executableShutdownGrace
	executableShutdownGrace = 200 * time.Millisecond
	defer func() { executableShutdownGrace = grace }()

	declaration := toolOnlyDeclaration()
	blocking := newAdapterBlockingTool()
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterToolNative(declaration, blocking), blocking.close
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	snapshot := service.current()
	toolContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Invocation:    Invocation{snapshot: snapshot},
	}
	ctx, cancel := context.WithCancel(context.Background())
	outcome := startPreparedTool(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "peer.tool", toolContext, model.ToolCall{ID: "call-1", Name: "peer.tool", Arguments: json.RawMessage(`{}`)}, ctx)
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never entered Execute")
	}
	cancel()
	select {
	case got := <-outcome:
		testassert.Equal(t, got.Result.Status, model.ResultInterrupted)
		testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled call did not settle after the grace window")
	}
}

// adapterCancelTool blocks one execution until its callback context ends and
// can be switched to an immediate result for the late-cancellation case.
type adapterCancelTool struct {
	block   bool
	entered chan struct{}
	once    sync.Once
}

func (t *adapterCancelTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (t *adapterCancelTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
}
func (t *adapterCancelTool) Execute(ctx context.Context, context extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	if t.block {
		t.once.Do(func() { close(t.entered) })
		<-ctx.Done()
	}
	return harness.ToolOutcome{Result: model.ToolResult{CallID: context.Calls.ToolCallID, Status: model.ResultSuccess, Content: "done"}, Evidence: harness.ExecutionObserved}
}

func TestExecutableCooperativeAndLateCancellation(t *testing.T) {
	declaration := toolOnlyDeclaration()
	tool := &adapterCancelTool{block: true, entered: make(chan struct{})}
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterToolNative(declaration, tool), nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	snapshot := service.current()
	toolContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Invocation:    Invocation{snapshot: snapshot},
	}
	call := model.ToolCall{ID: "call-1", Name: "peer.tool", Arguments: json.RawMessage(`{}`)}
	ctx, cancel := context.WithCancel(context.Background())
	outcome := startPreparedTool(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "peer.tool", toolContext, call, ctx)
	select {
	case <-tool.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cooperative call did not start")
	}
	cancel()
	select {
	case got := <-outcome:
		if got.Result.Content != "done" || got.Evidence != harness.ExecutionObserved {
			t.Fatalf("cooperative cancellation outcome = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cooperative cancellation did not settle")
	}

	tool.block = false
	lateCtx, lateCancel := context.WithCancel(context.Background())
	late := startPreparedTool(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "peer.tool", toolContext, call, lateCtx)
	var lateOutcome harness.ToolOutcome
	select {
	case lateOutcome = <-late:
	case <-time.After(2 * time.Second):
		t.Fatal("late-cancellation call did not settle")
	}
	lateCancel()
	if lateOutcome.Result.Content != "done" || lateOutcome.Evidence != harness.ExecutionObserved {
		t.Fatalf("late cancellation outcome = %+v", lateOutcome)
	}
}

func TestExecutableOversizeCallFailsItselfOnly(t *testing.T) {
	declaration := toolOnlyDeclaration()
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterToolNative(declaration, simpleTool{}), nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	snapshot := service.current()
	value, ok := snapshot.resources.runtime.capabilities["peer.tool"]
	if !ok {
		t.Fatal("tool is not open")
	}
	tool := value.implementation.Tool
	toolContext := executableTestToolContext("instance-1")
	oversize := extension.ToolPlan{
		Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}},
		Data:        make([]byte, worker.FrameLimit),
	}
	outcome := tool.Execute(context.Background(), toolContext, oversize)
	testassert.Equal(t, outcome.Evidence, harness.ExecutionNotStarted)
	testassert.Equal(t, outcome.Result.Status, model.ResultError)

	preparedContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Invocation:    Invocation{snapshot: snapshot},
	}
	got := preparedToolOutcome(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "peer.tool", preparedContext, model.ToolCall{ID: "call-1", Name: "peer.tool", Arguments: json.RawMessage(`{}`)})
	if got.Result.Content != "ok" || got.Evidence != harness.ExecutionObserved {
		t.Fatalf("follow-up outcome = %+v", got)
	}
}

func TestExecutableTerminalRacingActivationFailure(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	process := &leaderExitProcess{inner: peer.process(), leader: make(chan struct{}), killed: make(chan struct{})}
	old := spawnExecutable
	spawnExecutable = func(context.Context, string, []string) (executableProcess, error) { return process, nil }
	t.Cleanup(func() { spawnExecutable = old })

	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peer.next()
	peer.sendValue(call.ID, encodeValue(t, worker.EncodeExecuteResult, harness.ToolOutcome{
		Result:   model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "accepted"},
		Evidence: harness.ExecutionObserved,
	}))
	// The leader exits as the terminal is in flight; termination must drain the
	// reader before it settles anything.
	close(process.leader)
	select {
	case got := <-outcome:
		if got.Result.Content != "accepted" || got.Evidence != harness.ExecutionObserved {
			t.Fatalf("terminal racing failure was overwritten: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal racing failure did not settle")
	}
}

// toggleFailWriter fails writes after the activation is established.
type toggleFailWriter struct {
	io.WriteCloser
	fail atomic.Bool
}

func (w *toggleFailWriter) Write(data []byte) (int, error) {
	if w.fail.Load() {
		return 0, errors.New("write failed")
	}
	return w.WriteCloser.Write(data)
}

type writeFailProcess struct {
	inner  *testExecutableProcess
	writer *toggleFailWriter
}

func (p *writeFailProcess) Stdin() io.WriteCloser { return p.writer }
func (p *writeFailProcess) Stdout() io.Reader     { return p.inner.Stdout() }
func (p *writeFailProcess) Wait() error           { return p.inner.Wait() }
func (p *writeFailProcess) Kill() error           { return p.inner.Kill() }

func TestExecutableWriteFailureTerminatesWithoutEarlySettlement(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	process := &writeFailProcess{inner: peer.process(), writer: &toggleFailWriter{WriteCloser: nil}}
	process.writer.WriteCloser = process.inner.Stdin()
	old := spawnExecutable
	spawnExecutable = func(context.Context, string, []string) (executableProcess, error) { return process, nil }
	t.Cleanup(func() { spawnExecutable = old })

	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	process.writer.fail.Store(true)
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	select {
	case got := <-outcome:
		testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
	case <-time.After(2 * time.Second):
		t.Fatal("write failure did not settle the call")
	}
	if _, err := supervisor.live(declaration.ID); err == nil {
		t.Fatal("write failure did not terminate the activation")
	}
}

func TestExecutableUnsentCancelSettlesNotStarted(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	activation, err := supervisor.live(declaration.ID)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	tool := instance.Capabilities["peer.tool"].Tool
	toolContext := executableTestToolContext("instance-1")
	plan := extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}}
	first := runToolExecute(t, tool, toolContext, plan)
	eventually(t, "first call to block the writer", func() bool {
		sent, _ := activationPending(activation)
		return sent == 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan harness.ToolOutcome, 1)
	go func() { second <- tool.Execute(ctx, toolContext, plan) }()
	eventually(t, "second call to queue unclaimed", func() bool {
		_, total := activationPending(activation)
		return total == 2
	})
	cancel()
	select {
	case got := <-second:
		testassert.Equal(t, got.Evidence, harness.ExecutionNotStarted)
		testassert.Equal(t, got.Result.Status, model.ResultInterrupted)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unsent cancellation did not settle immediately")
	}
	if _, err := supervisor.live(declaration.ID); err != nil {
		t.Fatalf("unsent cancellation terminated the activation: %v", err)
	}
	peer.exit()
	<-first
}

func TestExecutableCancelledClassTerminal(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	spawner := &testPeerSpawner{t: t, factory: func() *testExecutablePeer { return peer }}
	spawner.install()
	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	instance := startTestExecutable(t, supervisor, executable, peer)
	defer supervisor.release(declaration.ID)
	tool := instance.Capabilities["peer.tool"].Tool
	outcome := runToolExecute(t, tool, executableTestToolContext("instance-1"), extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: "*"}}})
	call := peer.next()
	peer.send(worker.Frame{Kind: worker.KindResponse, ID: call.ID, Error: &worker.Error{Class: worker.ClassCancelled, Message: "cancelled"}})
	got := <-outcome
	testassert.Equal(t, got.Result.Status, model.ResultInterrupted)
	testassert.Equal(t, got.Evidence, harness.ExecutionUnknown)
}

// closeRecordingProcess records the parent-pipe close.
type closeRecordingProcess struct {
	inner  *testExecutableProcess
	closed chan struct{}
	once   sync.Once
}

func (p *closeRecordingProcess) Stdin() io.WriteCloser { return p.inner.Stdin() }
func (p *closeRecordingProcess) Stdout() io.Reader     { return p.inner.Stdout() }
func (p *closeRecordingProcess) Wait() error           { return p.inner.Wait() }
func (p *closeRecordingProcess) Kill() error           { return p.inner.Kill() }
func (p *closeRecordingProcess) CloseParent() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func TestExecutableTerminationClosesParentPipes(t *testing.T) {
	declaration := toolOnlyDeclaration()
	peer := newTestExecutablePeer(t, false, nil)
	process := &closeRecordingProcess{inner: peer.process(), closed: make(chan struct{})}
	old := spawnExecutable
	spawnExecutable = func(context.Context, string, []string) (executableProcess, error) { return process, nil }
	t.Cleanup(func() { spawnExecutable = old })

	supervisor := newExecutableSupervisor(func(context.Context, []string) ([]string, error) { return nil, nil })
	executable := executableExtension{Declaration: declaration, EntryPoint: "peer"}
	startTestExecutable(t, supervisor, executable, peer)
	supervisor.release(declaration.ID)
	select {
	case <-process.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("termination did not close the parent pipe ends")
	}
}

func TestExecutableInvalidDeclarationRejectedAtInstall(t *testing.T) {
	declaration := toolOnlyDeclaration()
	declaration.Components = nil
	dataDir := t.TempDir()
	source := executablePackageSource(t, executableExtension{Declaration: declaration, EntryPoint: "peer"})
	_, err := newPackageManager(dataDir).install(source)
	testassert.Require(t, errors.Is(err, harness.ErrInvalid), "install invalid declaration = %v, want invalid", err)
	mustNotExist(t, filepath.Join(dataDir, "extensions", extensionDataHash(declaration.ID), "selected"), "an invalid declaration created a selection")
}

func TestExecutableOwnerCloseRunsWorkerClose(t *testing.T) {
	declaration := toolOnlyDeclaration()
	closed := make(chan struct{})
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		native := adapterToolNative(declaration, simpleTool{})
		original := native.Open
		native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			value, err := original(ctx, instance)
			if err != nil {
				return value, err
			}
			value.Close = func(context.Context, extension.CallContext) error {
				select {
				case <-closed:
				default:
					close(closed)
				}
				return nil
			}
			return value, nil
		}
		return native, nil
	}}
	spawner.install()
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	installExecutablePackage(t, dataDir, executableExtension{Declaration: declaration, EntryPoint: "peer"})
	owner, err := open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("owner close skipped the worker Close callback")
	}
}

func TestWorkerHookFailureMatchesNativeSettlement(t *testing.T) {
	for _, evidence := range []harness.ExecutionEvidence{harness.ExecutionNotStarted, harness.ExecutionUnknown} {
		native := &harness.EffectError{Interrupted: true, Evidence: evidence, Err: errors.New("hook interrupted")}
		failure := worker.HookFailureFromError(native)
		if failure == nil {
			t.Fatalf("%s: HookFailureFromError returned nil", evidence)
		}
		mapped := hookFailureError(failure)
		testassert.Equal(t, harness.EffectInterrupted(mapped), harness.EffectInterrupted(native))
		testassert.Equal(t, harness.DeclaredExecutionKnowledge(mapped), harness.DeclaredExecutionKnowledge(native))
	}
}

func TestWorkerServiceErrorClassReachesClassifier(t *testing.T) {
	declaration := executableTestDeclaration()
	handler := func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{}, &worker.Error{Class: worker.ClassDenied, Message: "denied by worker"}
	}
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return adapterSimpleNative(declaration, simpleTool{}, map[string]map[string]extension.Handler{
			"peer.service": {"echo": handler},
		}), nil
	}}
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"peer":{"enabled":true}}}`)
	service, _ := newExecutableConfigurationService(t, h, declaration, spawner)
	if _, err := service.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer service.current().resources.release()
	value, ok := service.current().resources.runtime.capabilities["peer.service"]
	if !ok {
		t.Fatal("service capability is not open")
	}
	_, err := service.current().composition.invokeProvider(context.Background(), value, value.capability.Methods, "echo", json.RawMessage(`{}`), nil, callIdentity{revision: "1"}, false)
	if err == nil {
		t.Fatal("worker service denial returned no error")
	}
	testassert.Equal(t, ErrorCode(err), protocol.ErrorCodeDenied)
}
