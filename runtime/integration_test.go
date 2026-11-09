package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// Assembled-phase integration: one live scenario runs the concrete production
// preparation end to end over the real owner — real transports against a test
// model server, real tool dispatch through a dependent capability, configured
// hooks, a blocked Operation, a reload published over it, the queued next
// revision, independent Workspaces, observer saturation, shutdown joining the
// parked execution, and restart repair on the same store.

func assembledConfigDocument(endpoint, tag, label string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "ASSEMBLED_TEST_KEY",
		`{"m":{"name":"M","context_window":262144}}`) + `},"extensions":{"hooks":{"enabled":true,"tag":` + fixtureString(tag) + `},"depper":{"enabled":true,"label":` + fixtureString(label) + `},"base":{"enabled":true},"probe":{"enabled":true}}}`
}

// The "leaky" definition selects the Core storage export by ID: raw storage
// is not in the ordinary capability universe, so the definition drops and the
// type is unknown at admission — raw storage is never exported into execution.
const assembledAgentsDocument = `{
  "integrated": {"model": "prov/m", "system_prompt": "simple", "tools": ["probe"], "capabilities": ["hook.first", "dep.worker"]}
}`

type assembledBase struct{ closed *atomic.Bool }

func (b assembledBase) call(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
	state := "base"
	if b.closed != nil && b.closed.Load() {
		state = "closed"
	}
	value, _ := json.Marshal(map[string]string{"state": state})
	return extension.Result{Value: value}, nil
}

type assembledWorker struct {
	label string
	ws    *workspaceOpens
}

func (w assembledWorker) call(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
	if w.ws != nil {
		w.ws.park()
	}
	if w.label == "" {
		return extension.Result{Value: json.RawMessage(`{"result":"undecodable"}`)}, nil
	}
	result, err := call.Call(ctx, "dep", "call", json.RawMessage(`{}`))
	if err != nil {
		return extension.Result{}, err
	}
	var base struct {
		State string `json:"state"`
	}
	if result.Failure != nil || json.Unmarshal(result.Value, &base) != nil {
		return extension.Result{Value: json.RawMessage(`{"result":"undecodable"}`)}, nil
	}
	value, _ := json.Marshal(map[string]string{"result": w.label + ":" + base.State})
	return extension.Result{Value: value}, nil
}

// parkingHook parks exactly one armed admission until released, ignoring its
// context: the in-flight admitted call whose presence proves Close joins
// admitted work instead of abandoning it.
type parkingHook struct {
	mu      sync.Mutex
	armed   bool
	used    bool
	release chan struct{}
	usedSig chan struct{}
}

func (h *parkingHook) Prepare(_ context.Context, _ extension.CallContext, _ extension.PreparationInput) (extension.PreparationConsequence, error) {
	h.mu.Lock()
	park := h.armed && !h.used
	h.used = h.used || h.armed
	release := h.release
	h.mu.Unlock()
	if park {
		if h.usedSig != nil {
			select {
			case h.usedSig <- struct{}{}:
			default:
			}
		}
		<-release
	}
	return extension.PreparationConsequence{}, nil
}

func (h *parkingHook) arm(release, used chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed, h.used, h.release, h.usedSig = true, false, release, used
}

// workspaceOpens records the Workspace identity of every Workspace scope
// construction, in construction order.
type workspaceOpens struct {
	mu      sync.Mutex
	keys    []string
	gate    chan struct{}
	arrived chan struct{}
}

func (w *workspaceOpens) park() {
	w.mu.Lock()
	gate, arrived := w.gate, w.arrived
	w.gate = nil
	w.mu.Unlock()
	if gate == nil {
		return
	}
	if arrived != nil {
		select {
		case arrived <- struct{}{}:
		default:
		}
	}
	<-gate
}

func (w *workspaceOpens) hold(gate, arrived chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gate, w.arrived = gate, arrived
}

func (w *workspaceOpens) opened(workspace string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys = append(w.keys, workspace)
}

func (w *workspaceOpens) constructed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.keys...)
}

// probeTool is the concrete tool capability of the assembled scenario: its
// preparation invokes the dependent service chain it is bound to, so every
// real tool dispatch records one revision-scoped dependency invocation.
type probeTool struct {
	events *traceLog
}

func (probeTool) describe(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
	return extension.ToolDescription{Definition: model.ToolDefinition{Name: "probe", Description: "records one dependency invocation", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, Available: true}, nil
}

func (probeTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t probeTool) Prepare(ctx context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	result, err := tc.Calls.Call(ctx, "dep", "call", json.RawMessage(`{}`))
	if err != nil {
		return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}}
	}
	var value struct {
		Result string `json:"result"`
	}
	if result.Failure != nil || json.Unmarshal(result.Value, &value) != nil {
		return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "undecodable dependency result"}}}
	}
	t.events.add("work:" + value.Result)
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "probed"}}}
}

func (probeTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "probed"}}
}

func assembledPlugins(e *ownerEnv, store harness.Storage, hook *recordHook, ws *workspaceOpens) []extension.Native {
	e.fixtureStore(store)
	baseCapability := extension.ServiceCapability("dep.base", "test.dep.base", extension.PureMethod("call", `{}`, `{"type":"object"}`))
	baseBinding := extension.Binding{Name: "dep", Provider: "base/base#component", CapabilityID: "dep.base", Definition: baseCapability.Definition()}
	base := testNative("base", extension.LifetimeWorkspace, []extension.Capability{baseCapability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		closed := &atomic.Bool{}
		return extension.Instance{Capabilities: map[string]extension.Implementation{"dep.base": {Service: map[string]extension.Handler{"call": assembledBase{closed: closed}.call}}}, Close: func(context.Context, extension.CallContext) error {
			closed.Store(true)
			e.events.add("close:base")
			return nil
		}}, nil
	})
	workerCapability := extension.ServiceCapability("dep.worker", "test.dep.worker", extension.PureMethod("call", `{}`, `{"type":"object"}`))
	workerBinding := extension.Binding{Name: "dep", Provider: "depper/depper#component", CapabilityID: "dep.worker", Definition: workerCapability.Definition()}
	depper := testNative("depper", extension.LifetimeWorkspace, []extension.Capability{workerCapability}, []extension.Binding{baseBinding}, func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
		ws.opened(info.Scope.Workspace)
		label := ""
		if len(info.Section) != 0 {
			var settings struct {
				Label string `json:"label"`
			}
			_ = json.Unmarshal(info.Section, &settings)
			label = settings.Label
		}
		worker := assembledWorker{label: label, ws: ws}
		return extension.Instance{Capabilities: map[string]extension.Implementation{"dep.worker": {Service: map[string]extension.Handler{"call": worker.call}}}, Close: func(context.Context, extension.CallContext) error {
			e.events.add("close:depper")
			return nil
		}}, nil
	})
	probeCapability := extension.ToolCapability("probe")
	probe := testNative("probe", extension.LifetimeWorkspace, []extension.Capability{probeCapability}, []extension.Binding{workerBinding}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"probe": {Tool: probeTool{events: e.events}}}}, nil
	})
	probe.Describe["probe"] = probeTool{}.describe
	return []extension.Native{hookPlugin(hook), base, depper, probe}
}

// lastMessageRole returns the wire role of the request body's last message.
func lastMessageRole(body string) string {
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil || len(doc.Messages) == 0 {
		return ""
	}
	return doc.Messages[len(doc.Messages)-1].Role
}

// lastUserText returns the last user message's plain text content.
func lastUserText(body string) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		if doc.Messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(doc.Messages[i].Content, &text) == nil {
			return text
		}
		return ""
	}
	return ""
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeTextTurn(w http.ResponseWriter, text string) {
	writeSSE(w, textTurnEvents(text)...)
}

// writeToolCallTurn writes one completed assistant turn publishing one tool
// call under the given call identity.
func writeToolCallTurn(w http.ResponseWriter, id, name, args string) {
	writeSSE(w, toolCallTurnEvents(id, name, args)...)
}

// operationRegisterStatus reads one Operation register's state status
// directly from the store, the only reader that works after the owner closed.
func operationRegisterStatus(t *testing.T, store harness.Storage, sessionID, operationID string) harness.OperationState {
	t.Helper()
	status, _ := operationRegisterTerminal(t, store, sessionID, operationID)
	return status
}

// operationRegisterTerminal reads one Operation's register once and returns
// its status and terminal detail, failing the test on read or decode error.
func operationRegisterTerminal(t *testing.T, store harness.Storage, sessionID, operationID string) (harness.OperationState, string) {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	testassert.Require(t, err == nil, "ReadRegister(%s): %v", operationID, err)
	var wire struct {
		State struct {
			Status   harness.OperationState `json:"status"`
			Terminal *struct {
				Detail string `json:"detail"`
			} `json:"terminal"`
		} `json:"state"`
	}
	testassert.NoError(t, json.Unmarshal(reg.Payload, &wire))
	var detail string
	if wire.State.Terminal != nil {
		detail = wire.State.Terminal.Detail
	}
	return wire.State.Status, detail
}

// awaitOperation polls one Operation's register until it reaches the wanted
// state, bounding the wait: settlement is asynchronous after Submit returns,
// and a queued delivery admits later than its submit.
func awaitOperation(t *testing.T, r *Runtime, sessionID, operationID string, want harness.OperationState) harness.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var rec harness.OperationRecord
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			var rerr error
			rec, rerr = h.ReadOperation(ctx, sessionID, operationID)
			return rerr
		})
		if err == nil && rec.State.Status == want {
			return rec
		}
		if time.Now().After(deadline) {
			var detail string
			if rec.State.Terminal != nil {
				detail = rec.State.Terminal.Detail
			}
			t.Fatalf("operation %q never settled to %v (read error %v, status %v, detail %q)", operationID, want, err, rec.State.Status, detail)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAssembledPhaseIntegration(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newOwnerEnv(t)
		server := newProductionModelServer(t, "probe", "{}")
		t.Setenv("ASSEMBLED_TEST_KEY", "assembled-key-1")
		writeServiceFile(t, e.configPath, assembledConfigDocument(server.URL, "T1", "L1"))
		writeAgentDefinitions(t, e.configPath, assembledAgentsDocument)
		hook := &recordHook{name: "hook.first", events: e.events, mutate: func(in extension.PreparationInput) extension.PreparationConsequence {
			prompt := in.Prompt + "|hooked"
			return extension.PreparationConsequence{Prompt: &prompt}
		}}
		ws := &workspaceOpens{}
		plugins := assembledPlugins(e, store, hook, ws)
		// The combined scenario runs through the public production
		// construction: concrete preparation, custom plugin slice.
		openAssembled := func() (*Runtime, error) {
			return e.open(ctx, plugins...)
		}

		// Raw storage is never exported into execution: the Core export ID is
		// outside the ordinary capability universe, so a definition selecting
		// it drops and the type is unknown at admission.
		leaky, err := openAssembled()
		testassert.NoError(t, err)
		leakySession, err := leaky.createSession(ctx, e.dataDir, "leaky")
		testassert.NoError(t, err)
		_, err = submitRawThroughRuntime(ctx, leaky, leakySession.Identity.SessionID, "op-leaky", "x", harness.MessageModeRegular)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit through a definition selecting the Core storage export = %v, want harness.ErrInvalid (raw storage must not be exported into execution)", err)
		testassert.NoError(t, leaky.Close(ctx))

		r, err := openAssembled()
		testassert.NoError(t, err)
		sat, err := r.Subscribe(1)
		testassert.NoError(t, err)
		healthy, err := r.Subscribe(64)
		testassert.NoError(t, err)

		// Two Sessions in two distinct Workspaces run over their own scopes.
		wsA := filepath.Join(e.home, "int-wsa")
		wsB := filepath.Join(e.home, "int-wsb")
		sessionA, err := r.createSession(ctx, wsA, "integrated")
		testassert.NoError(t, err)
		sessionB, err := r.createSession(ctx, wsB, "integrated")
		testassert.NoError(t, err)

		// Baseline turn on B: the composed production turn calls the real
		// probe tool through the transport — one model request for the tool
		// call and one for the follow-up — with the configured hook and the
		// dependency invoked at the tool boundary with its own revision's
		// settings.
		submitThroughRuntime(t, r, sessionB.Identity.SessionID, "op-b1", "hello")
		rec := awaitOperation(t, r, sessionB.Identity.SessionID, "op-b1", harness.OperationSuccess)
		{
			got := ws.constructed()
			testassert.Require(t, slices.Equal(got, []string{wsB}), "Workspace scope constructions after the first turn = %v, want exactly the %q identity", got, wsB)
		}
		testassert.Require(t, rec.Admission.Execution.ConfigurationRevision == "1" && rec.Admission.Execution.SystemPrompt != "" && strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "first turn record = %+v, want the hooked revision-1 success with the composed prompt", rec.Admission.Execution)
		if !slices.Equal(toolNames(rec.Admission.Execution.Tools), []string{"probe"}) {
			t.Fatalf("first turn advertisement = %v, want the composed probe tool", toolNames(rec.Admission.Execution.Tools))
		}
		if n := countEvents(e.events.all(), "work:L1:base"); n != 1 {
			t.Fatalf("dependency invocations reading revision-1 settings = %d, want 1: %v", n, e.events.all())
		}
		if server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool-call turn and the follow-up", server.requests())
		}
		testassert.Equal(t, server.authAt(0), "Bearer assembled-key-1")

		// The cleanup of the turn precedes the asynchronous Operation-close
		// publication: consume the healthy subscription until that exact
		// closure is observed, retaining every event for the final
		// complete-sequence oracle.
		var observed []Event
		for {
			event, ok := nextEvent(t, healthy)
			testassert.Require(t, ok, "the healthy subscription closed before op-b1's Operation-close publication")
			if body, closed := scopeClosedEvent(t, event); closed && operationScopeWorkspace(t, body.Scope) == wsB {
				recordCommittedEvent(t, &observed, event, sessionA.Identity.SessionID, sessionB.Identity.SessionID)
				break
			}
			recordCommittedEvent(t, &observed, event, sessionA.Identity.SessionID, sessionB.Identity.SessionID)
		}

		// Park the actual dependency call, not its model request.
		ws.mu.Lock()
		ws.gate, ws.arrived = make(chan struct{}), make(chan struct{}, 1)
		gate, arrived := ws.gate, ws.arrived
		ws.mu.Unlock()
		submitThroughRuntime(t, r, sessionA.Identity.SessionID, "op-a1", "nested")
		server.awaitRequests(3)
		<-arrived
		{
			got := ws.constructed()
			testassert.Require(t, slices.Equal(got, []string{wsB, wsA}), "Workspace scope constructions = %v, want one per distinct key in first-use order", got)
		}
		// The saturated subscriber filled on the first publication — a
		// passive fact now precedes every scope opening — and is removed at
		// the second, while the healthy one continues.
		first, ok := nextEvent(t, sat)
		testassert.Require(t, ok, "saturated subscriber closed before its first buffered event")
		switch eventKind(t, first) {
		case "configuration_changed", "scope_opened", "scope_closed", "session_changed", "text_delta", "tool_started", "tool_finished", "warning_changed":
		default:
			t.Fatalf("saturated subscriber first event = %s, outside the closed event set", eventJSON(t, first))
		}
		{
			_, ok := nextEvent(t, sat)
			testassert.Require(t, !ok, "saturated subscriber still open at the second publication, want it removed and closed")
		}
		sat.Close()

		// The queued next revision is buffered while the Operation is blocked.
		submitQueuedThroughRuntime(t, r, sessionA.Identity.SessionID, "op-a2", "queued")

		// Reload publishes the next revision over the blocked Operation.
		writeServiceFile(t, e.configPath, assembledConfigDocument(server.URL, "T2", "L2"))
		writeAgentDefinitions(t, e.configPath, assembledAgentsDocument)
		{
			revision, err := r.reload(ctx, protocolCaller())
			testassert.Require(t, err == nil && revision == "2", "Reload over the blocked Operation = (%q, %v), want 2", revision, err)
		}
		testassert.Require(t, countEvents(e.events.all(), "close:depper") == 0 && countEvents(e.events.all(), "close:base") == 0, "retired bindings closed while the nested call was parked: %v", e.events.all())
		fresh, err := r.createSession(ctx, wsA, "integrated")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, fresh.Identity.SessionID, "op-fresh", "fresh")
		freshRec := awaitOperation(t, r, fresh.Identity.SessionID, "op-fresh", harness.OperationSuccess)
		testassert.Equal(t, freshRec.Admission.Execution.ConfigurationRevision, "2")
		testassert.Require(t, slices.Equal(ws.constructed(), []string{wsB, wsA, wsA}), "resource sets did not construct independent bindings for the same Workspace: %v", ws.constructed())
		testassert.Require(t, countEvents(e.events.all(), "work:L2:base") == 1 && countEvents(e.events.all(), "close:depper") == 0, "new work did not coexist with parked old work: %v", e.events.all())
		for {
			event, ok := nextEvent(t, healthy)
			testassert.Require(t, ok, "subscription closed before fresh execution cleanup")
			recordCommittedEvent(t, &observed, event, sessionA.Identity.SessionID, sessionB.Identity.SessionID, fresh.Identity.SessionID)
			if body, closed := scopeClosedEvent(t, event); closed && operationScopeWorkspace(t, body.Scope) == wsA {
				break
			}
		}

		// Release: the blocked Operation completes on its captured revision 1.
		close(gate)
		awaitOperation(t, r, sessionA.Identity.SessionID, "op-a1", harness.OperationSuccess)
		{
			rec := readOperation(t, r, sessionA.Identity.SessionID, "op-a1")
			testassert.Require(t, rec.Admission.Execution.ConfigurationRevision == "1" && strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "blocked Operation record = %+v, want the revision-1 hooked success", rec.Admission.Execution)
		}

		// The queued delivery captures the next revision: new hook settings
		// and the dependency's own settings from the same revision, while the
		// old capture keeps its content.
		recA2 := awaitOperation(t, r, sessionA.Identity.SessionID, "op-a2", harness.OperationSuccess)
		testassert.Require(t, countEvents(e.events.all(), "work:L1:base") == 2 && orderedSubset(e.events.all(), "work:L1:base", "close:depper", "close:base"), "nested call and dependency closure order = %v", e.events.all())
		testassert.Require(t, recA2.Admission.Execution.ConfigurationRevision == "2", "queued Operation record = %+v, want the revision-2 success", recA2.Admission.Execution)
		{
			got := hook.calls()
			testassert.Require(t, slices.Equal(got, []hookCall{{tag: "T1"}, {tag: "T1"}, {tag: "T2"}, {tag: "T2"}}), "hook calls = %v, want the configured tag of each call", got)
		}
		if n := countEvents(e.events.all(), "work:L2:base"); n != 2 {
			t.Fatalf("dependency invocations reading revision-2 settings = %d, want 2: %v", n, e.events.all())
		}
		// Consume the healthy subscription through op-a2's Operation-close
		// publication so the remaining sequence is deterministic.
		closedOps := 0
		for closedOps < 2 {
			event, ok := nextEvent(t, healthy)
			testassert.Require(t, ok, "the healthy subscription closed before the queued delivery settled")
			if body, closed := scopeClosedEvent(t, event); closed && operationScopeWorkspace(t, body.Scope) == wsA {
				closedOps++
			}
			recordCommittedEvent(t, &observed, event, sessionA.Identity.SessionID, sessionB.Identity.SessionID, fresh.Identity.SessionID)
		}

		// Shutdown joins the parked execution: the third admission's model
		// request parks at the server, Close cancels it, the server observes
		// the disconnect, and the interrupted settlement commits before Close
		// converges.
		server.armGate()
		before := server.requests()
		// A submit racing the predecessor's post-terminal drain is buffered
		// and delivered by that drain, so either disposition starts this
		// execution.
		parked, err := submitRawThroughRuntime(ctx, r, sessionA.Identity.SessionID, "op-a3", "parked", harness.MessageModeRegular)
		testassert.NoError(t, err)
		testassert.Require(t, parked.Disposition == harness.DispositionAdmitted || parked.Disposition == harness.DispositionSteering, "submit op-a3 = %+v, want admitted or buffered steering", parked.Disposition)
		server.awaitRequests(before + 1)
		closeDone := make(chan error, 1)
		go func() { closeDone <- r.Close(ctx) }()
		select {
		case <-server.gone:
		case <-time.After(10 * time.Second):
			t.Fatal("shutdown cancellation never reached the parked execution")
		}
		select {
		case err := <-closeDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Close never converged after the parked execution settled")
		}
		testassert.Equal(t, operationRegisterStatus(t, store, sessionA.Identity.SessionID, "op-a3"), harness.OperationInterruption)
		server.releaseGate()

		// The healthy observer saw every committed transition in publication
		// order — scope identities exact, payloads stripped — ending with the
		// sorted Workspace closures and the Runtime. The prefix consumed up to
		// op-a2's Operation-close publication precedes the remaining drain.
		committedScope := func(kind protocol.ScopeEventKind, scopeKind ScopeKind, workspace, sessionID, operationID string) Event {
			return scopeEvent(kind, ScopeInfo{Kind: scopeKind, Workspace: workspace, SessionID: sessionID, OperationID: operationID})
		}
		sessionBID, sessionAID := sessionB.Identity.SessionID, sessionA.Identity.SessionID
		want := []Event{
			committedScope(protocol.ScopeOpened, ScopeWorkspace, wsB, "", ""),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsB, sessionBID, "op-b1"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsB, sessionBID, "op-b1"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsB, sessionBID, "op-b1"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsB, sessionBID, "op-b1"),
			committedScope(protocol.ScopeOpened, ScopeWorkspace, wsA, "", ""),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeOpened, ScopeRuntime, "", "", ""),
			configurationChangedEvent(2),
			committedScope(protocol.ScopeOpened, ScopeWorkspace, wsA, "", ""),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, fresh.Identity.SessionID, "op-fresh"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, fresh.Identity.SessionID, "op-fresh"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, fresh.Identity.SessionID, "op-fresh"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, fresh.Identity.SessionID, "op-fresh"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeClosed, ScopeWorkspace, wsA, "", ""),
			committedScope(protocol.ScopeClosed, ScopeWorkspace, wsB, "", ""),
			committedScope(protocol.ScopeClosed, ScopeRuntime, "", "", ""),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeWorkspace, wsA, "", ""),
			committedScope(protocol.ScopeClosed, ScopeRuntime, "", "", ""),
		}
		got := normalizeInvocationScopes(t, append(observed, committedTail(t, healthy, sessionAID, sessionBID, fresh.Identity.SessionID)...))
		if !slices.EqualFunc(got, want, equalEvent) {
			t.Fatalf("healthy subscriber committed events = %s, want the complete committed sequence %s", eventsJSON(t, got), eventsJSON(t, want))
		}

		// Restart repair on the same store: recovery settles the abandoned
		// running state before any execution callback, then ordinary
		// admission proceeds on the repaired state.
		seededSession, _ := seedRunningOperation(t, store, filepath.Join(e.home, "int-reseed"))
		r2, err := openAssembled()
		testassert.NoError(t, err)
		{
			rec := readOperation(t, r2, seededSession, "seed-op")
			testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "Operation interrupted by Runtime loss.", "restarted Operation state = %+v, want the terminal interruption repair", rec.State)
		}
		next, err := r2.createSession(ctx, wsB, "integrated")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r2, next.Identity.SessionID, "op-after-repair", "continue")
		awaitOperation(t, r2, next.Identity.SessionID, "op-after-repair", harness.OperationSuccess)
		testassert.NoError(t, r2.Close(ctx))
	})
}

// recordCommittedEvent appends one scope or configuration event to the
// committed-order oracle; every passive fact and warning event is accounted
// separately — its scope must name one of the real Sessions, never a foreign
// identity.
func recordCommittedEvent(t *testing.T, observed *[]Event, event Event, sessionIDs ...string) {
	t.Helper()
	switch eventKind(t, event) {
	case "configuration_changed", "scope_opened", "scope_closed":
		*observed = append(*observed, event)
	case "warning_changed":
		// The warning events the reloads' own commits and the real sessions'
		// prompt preparations require are accounted here on their
		// independent revision clock, never dropped silently: a global event
		// carries the runtime scope, a session event the real Session's
		// scope.
		body, err := event.AsWarningChangedEvent()
		testassert.NoError(t, err)
		if scopeKind(t, body.Scope) == "runtime" {
			// the global reload groups
		} else if scopeNamesRealSession(t, body.Scope, sessionIDs...) {
			// the session's own prompt group
		} else {
			t.Fatalf("warning event scope = %+v, want the runtime scope or a real session", body.Scope)
		}
		{
			_, err := strconv.ParseUint(body.WarningsRevision.Revision, 10, 64)
			testassert.Require(t, err == nil, "warning event revision %q is not a decimal integer", body.WarningsRevision.Revision)
		}
	case "session_changed":
		body, err := event.AsSessionChangedEvent()
		testassert.NoError(t, err)
		if !scopeNamesRealSession(t, body.Scope, sessionIDs...) {
			invalidation, _ := scopeSessionIdentity(t, body.Scope)
			t.Fatalf("passive invalidation for %v, want a real session", invalidation)
		}
	case "text_delta", "tool_started", "tool_finished":
		scope := progressEventScope(t, event)
		if !scopeNamesRealSession(t, scope, sessionIDs...) {
			progress, _ := scopeSessionIdentity(t, scope)
			t.Fatalf("passive progress for %v, want a real session", progress)
		}
	default:
		t.Fatalf("unexpected event kind %q", eventKind(t, event))
	}
}

// scopeClosedEvent decodes one scope event, reporting whether it is a
// closure.
func scopeClosedEvent(t *testing.T, event Event) (protocol.ScopeEvent, bool) {
	t.Helper()
	if eventKind(t, event) != "scope_closed" {
		return protocol.ScopeEvent{}, false
	}
	body, err := event.AsScopeEvent()
	testassert.NoError(t, err)
	return body, true
}

// committedTail drains the remaining closed subscription, keeping the
// committed order exact while accounting the passive facts of the real
// Sessions — a foreign identity fatals inside the recorder.
func committedTail(t *testing.T, sub *Subscription, sessionIDs ...string) []Event {
	t.Helper()
	var out []Event
	for _, event := range drainClosed(t, sub) {
		recordCommittedEvent(t, &out, event, sessionIDs...)
	}
	return out
}

// scopeNamesRealSession reports whether one event scope names one of the
// given real Sessions through its Session-carrying union branches.
func scopeNamesRealSession(t *testing.T, scope protocol.Scope, sessionIDs ...string) bool {
	t.Helper()
	sessionID, ok := scopeSessionIdentity(t, scope)
	if !ok {
		return false
	}
	for _, real := range sessionIDs {
		if sessionID == real {
			return true
		}
	}
	return false
}

// operationScopeWorkspace reads the lexical Workspace of one Operation scope;
// any other scope kind reads as the empty root.
func operationScopeWorkspace(t *testing.T, scope protocol.Scope) string {
	t.Helper()
	if scopeKind(t, scope) != "operation" {
		return ""
	}
	body, err := scope.AsOperationScope()
	testassert.NoError(t, err)
	return deref(body.Workspace)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func normalizeInvocationScopes(t *testing.T, events []Event) []Event {
	t.Helper()
	out := make([]Event, len(events))
	for i, event := range events {
		out[i] = event
		body, err := event.AsScopeEvent()
		if err != nil {
			continue
		}
		invocation, err := body.Scope.AsInvocationScope()
		if err != nil || invocation.Kind != protocol.Invocation {
			continue
		}
		testassert.Require(t, invocation.InvocationId != "" && invocation.Purpose == protocol.Conversation, "invocation scope = %+v, want the actual ID and conversation purpose", invocation)
		out[i] = scopeEvent(body.Kind, ScopeInfo{Kind: ScopeAgent, Workspace: *invocation.Workspace, SessionID: invocation.SessionId, OperationID: invocation.OperationId})
	}
	return out
}
