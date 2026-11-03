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
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
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
		`{"m":{"name":"M","context_window":262144}}`) + `},"plugins":{"hooks":{"tag":` + fixtureString(tag) + `},"depper":{"label":` + fixtureString(label) + `}}}`
}

// The "leaky" definition selects the Core storage export by ID: raw storage
// is not in the ordinary capability universe, so the definition drops and the
// type is unknown at admission — raw storage is never exported into execution.
const assembledAgentsDocument = `{
  "integrated": {"model": "prov/m", "system_prompt": "simple", "tools": ["probe"], "capabilities": ["hook.first", "dep.worker"]},
  "leaky": {"model": "prov/m", "system_prompt": "simple", "capabilities": ["session_store"]}
}`

type combinedBase struct{}

func (combinedBase) Base() string { return "base" }

// combinedWorker is one dependent capability: its factory receives its
// declared dependency, and every call reads its own settings from the
// Invocation the caller passed, never from ambient state.
type combinedWorker struct{ base combinedBase }

func (w combinedWorker) Work(inv Invocation) string {
	label := ""
	if raw := inv.Config("depper"); len(raw) > 0 {
		var settings struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return "undecodable:" + err.Error()
		}
		label = settings.Label
	}
	return label + ":" + w.base.Base()
}

// workspaceOpens records the Workspace identity of every Workspace scope
// construction, in construction order.
type workspaceOpens struct {
	mu   sync.Mutex
	keys []string
}

func (w *workspaceOpens) opened(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys = append(w.keys, key)
}

func (w *workspaceOpens) constructed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.keys...)
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

func (h *parkingHook) Prepare(_ context.Context, _ Invocation, capture harness.ExecutionCapture) (harness.ExecutionCapture, error) {
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
	return capture, nil
}

func (h *parkingHook) arm(release, used chan struct{}) {
	h.mu.Lock()
	h.armed, h.release, h.usedSig = true, release, used
	h.mu.Unlock()
}

// probeTool is the concrete tool capability of the assembled scenario: its
// preparation invokes the dependent capability it was constructed with, so
// every real tool dispatch records one revision-scoped dependency invocation.
type probeTool struct {
	worker combinedWorker
	events *traceLog
}

func (t probeTool) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "probe",
		Description: "records one dependency invocation",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (t probeTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t probeTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	t.events.add("work:" + t.worker.Work(tc.Invocation))
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "probed"}}}
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

func assembledPlugins(e *ownerEnv, store harness.Storage, hook *recordHook, ws *workspaceOpens) []Plugin {
	e.fixtureStore(store)
	return []Plugin{
		hookPlugin(hook),
		{
			ID:       "base",
			Scope:    ScopeWorkspace,
			Provides: []CapabilitySpec{Spec[combinedBase]("dep.base")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				return Instance{Values: map[string]any{"dep.base": combinedBase{}}}, nil
			},
		},
		{
			ID:       "depper",
			Scope:    ScopeWorkspace,
			Provides: []CapabilitySpec{Spec[combinedWorker]("dep.worker")},
			Requires: []CapabilitySpec{Spec[combinedBase]("dep.base")},
			ValidateConfig: func(raw json.RawMessage) error {
				var settings struct {
					Label string `json:"label"`
				}
				if err := json.Unmarshal(raw, &settings); err != nil || settings.Label == "" {
					return errors.New("depper requires a non-empty label")
				}
				return nil
			},
			Open: func(_ context.Context, info ScopeInfo, bindings Bindings) (Instance, error) {
				ws.opened(info.Workspace)
				base, err := Bind[combinedBase](bindings, "dep.base")
				if err != nil {
					return Instance{}, err
				}
				return Instance{Values: map[string]any{"dep.worker": combinedWorker{base: base}}}, nil
			},
		},
		{
			ID:       "probe",
			Scope:    ScopeWorkspace,
			Provides: []CapabilitySpec{ToolSpec("probe", probeTool{}.describe)},
			Requires: []CapabilitySpec{Spec[combinedWorker]("dep.worker")},
			Open: func(_ context.Context, _ ScopeInfo, bindings Bindings) (Instance, error) {
				worker, err := Bind[combinedWorker](bindings, "dep.worker")
				if err != nil {
					return Instance{}, err
				}
				return Instance{Values: map[string]any{"probe": probeTool{worker: worker, events: e.events}}}, nil
			},
		},
	}
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
		writeServiceFile(t, agents.PathForConfig(e.configPath), assembledAgentsDocument)
		hook := &recordHook{name: "hook.first", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.SystemPrompt += "|hooked"
			return c
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

		// Blocked Operation on A: the production model request parks at the
		// test server until the gate releases.
		server.armGate()
		submitThroughRuntime(t, r, sessionA.Identity.SessionID, "op-a1", "parked")
		server.awaitRequests(3)
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
		{
			revision, err := r.Reload(ctx)
			testassert.Require(t, err == nil && revision == "2", "Reload over the blocked Operation = (%q, %v), want 2", revision, err)
		}

		// Release: the blocked Operation completes on its captured revision 1.
		server.releaseGate()
		awaitOperation(t, r, sessionA.Identity.SessionID, "op-a1", harness.OperationSuccess)
		{
			rec := readOperation(t, r, sessionA.Identity.SessionID, "op-a1")
			testassert.Require(t, rec.Admission.Execution.ConfigurationRevision == "1" && strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "blocked Operation record = %+v, want the revision-1 hooked success", rec.Admission.Execution)
		}

		// The queued delivery captures the next revision: new hook settings
		// and the dependency's own settings from the same revision, while the
		// old capture keeps its content.
		recA2 := awaitOperation(t, r, sessionA.Identity.SessionID, "op-a2", harness.OperationSuccess)
		testassert.Require(t, recA2.Admission.Execution.ConfigurationRevision == "2", "queued Operation record = %+v, want the revision-2 success", recA2.Admission.Execution)
		{
			got := hook.calls()
			testassert.Require(t, slices.Equal(got, []hookCall{{revision: "1", tag: "T1"}, {revision: "1", tag: "T1"}, {revision: "2", tag: "T2"}}), "hook calls = %v, want the configured tag of each call's own revision", got)
		}
		if n := countEvents(e.events.all(), "work:L2:base"); n != 1 {
			t.Fatalf("dependency invocations reading revision-2 settings = %d, want 1: %v", n, e.events.all())
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
			recordCommittedEvent(t, &observed, event, sessionA.Identity.SessionID, sessionB.Identity.SessionID)
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
			configurationChangedEvent(2),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a1"),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a2"),
			committedScope(protocol.ScopeOpened, ScopeOperation, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeOpened, ScopeAgent, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeAgent, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeOperation, wsA, sessionAID, "op-a3"),
			committedScope(protocol.ScopeClosed, ScopeWorkspace, wsA, "", ""),
			committedScope(protocol.ScopeClosed, ScopeWorkspace, wsB, "", ""),
			committedScope(protocol.ScopeClosed, ScopeRuntime, "", "", ""),
		}
		got := append(observed, committedTail(t, healthy, sessionAID, sessionBID)...)
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
