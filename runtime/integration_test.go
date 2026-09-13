package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
)

// Assembled-phase integration: one live scenario combines the phase's
// mechanisms over the real owner — a blocked Operation, a reload published
// over it, the queued next revision, independent Workspaces, configured
// hooks, nested dependency invocation, observer saturation, shutdown joining
// the blocked work, and restart repair on the same store.

func integratedConfigDocument(tag, label string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://prov.test/v1", "api_key_env": "PREP_TEST_ABSENT_KEY"},
      "discovery": false,
      "models": {"m": {"name": "M", "context_window": 4096}}
    }
  },
  "plugins": {"hooks": {"tag": "` + tag + `"}, "depper": {"label": "` + label + `"}}
}`
}

// The "leaky" definition selects the Core storage export by ID: raw storage
// is not in the ordinary capability universe, so the definition drops and the
// type is unknown at admission — raw storage is never exported into execution.
const integratedAgentsDocument = `{
  "integrated": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "dep.worker"]},
  "leaky": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["session_store"]}
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

// combinedPrep is the controlled prepare/opener pair for the combined
// scenario: it records the revision of every selection, and its model effect
// invokes the selected dependency, parks while gated, and can hold after
// observing cancellation so a test proves shutdown joins it.
type combinedPrep struct {
	events *traceLog

	mu       sync.Mutex
	calls    int
	opens    int
	gate     chan struct{}
	hold     chan struct{}
	callsRev []string
	openRevs []string
	arrived  chan struct{}
	arrivedN int
	canceled chan struct{}
	cleanups chan struct{}
	cleanupN int
}

func newCombinedPrep(events *traceLog) *combinedPrep {
	return &combinedPrep{
		events:   events,
		arrived:  make(chan struct{}, 16),
		canceled: make(chan struct{}, 16),
		cleanups: make(chan struct{}, 16),
	}
}

func (p *combinedPrep) prepare(_ context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	p.mu.Lock()
	p.calls++
	p.callsRev = append(p.callsRev, sel.invocation.Revision())
	p.mu.Unlock()
	return harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		SystemPrompt:          "prompt-" + req.Session.AgentType,
		Tools:                 captureTools(sel.agent.Tools),
		Capabilities:          append([]string(nil), sel.agent.Capabilities...),
	}, p.opener, nil
}

func (p *combinedPrep) opener(_ context.Context, adm harness.OperationAdmission, sel selection) (harness.Execution, error) {
	p.mu.Lock()
	p.opens++
	p.openRevs = append(p.openRevs, sel.invocation.Revision())
	gate, hold := p.gate, p.hold
	p.mu.Unlock()
	p.events.add("open:" + adm.OperationID)
	return harness.Execution{
		NormalizeTool: runtimeNormalize,
		Model: func(ctx context.Context, _ model.Request) (model.Stream, error) {
			if worker, err := Bind[combinedWorker](sel.bindings, "dep.worker"); err == nil {
				p.events.add("work:" + worker.Work(sel.invocation))
			}
			signalBuffered(p.arrived)
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					signalBuffered(p.canceled)
					if hold != nil {
						<-hold
					}
					// A canceled physical request under the dead execution
					// context settles the Harness-owned interruption.
					return nil, ctx.Err()
				}
			}
			return &prepStream{}, nil
		},
		Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
			return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no concrete tools yet"}}}
		},
		Close: func() error {
			signalBuffered(p.cleanups)
			return nil
		},
	}, nil
}

func signalBuffered(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (p *combinedPrep) armGate() {
	p.mu.Lock()
	p.gate = make(chan struct{})
	p.mu.Unlock()
}

func (p *combinedPrep) releaseGate() {
	p.mu.Lock()
	gate := p.gate
	p.gate = nil
	p.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (p *combinedPrep) holdAfterCancel(release chan struct{}) {
	p.mu.Lock()
	p.hold = release
	p.mu.Unlock()
}

func (p *combinedPrep) counts() (calls, opens, cleanups int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.opens, p.cleanupN
}

func (p *combinedPrep) revisions() (callsRev, openRevs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.callsRev...), append([]string(nil), p.openRevs...)
}

func (p *combinedPrep) awaitCleanups(n int) {
	p.mu.Lock()
	seen := p.cleanupN
	p.mu.Unlock()
	for seen < n {
		select {
		case <-p.cleanups:
			p.mu.Lock()
			p.cleanupN++
			seen = p.cleanupN
			p.mu.Unlock()
		case <-time.After(10 * time.Second):
			panic(fmt.Sprintf("combined cleanup %d never ran; events %v", seen+1, p.events.all()))
		}
	}
}

// awaitArrival waits until the n-th execution reached its model effect. Each
// execution signals exactly once, so the count identifies the execution
// without consuming a stale buffered signal from an earlier turn.
func (p *combinedPrep) awaitArrival(n int) {
	p.mu.Lock()
	seen := p.arrivedN
	p.mu.Unlock()
	for seen < n {
		<-p.arrived
		p.mu.Lock()
		p.arrivedN++
		seen = p.arrivedN
		p.mu.Unlock()
	}
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

func combinedPlugins(e *ownerEnv, store harness.Storage, hook *recordHook, ws *workspaceOpens) []Plugin {
	return []Plugin{
		e.storagePlugin(store),
		{
			ID:       "hooks",
			Scope:    ScopeRuntime,
			Provides: []CapabilitySpec{Spec[PreparationHook]("hook.first")},
			ValidateConfig: func(raw json.RawMessage) error {
				var settings struct {
					Tag string `json:"tag"`
				}
				if err := json.Unmarshal(raw, &settings); err != nil || settings.Tag == "" {
					return errors.New("hooks require a non-empty tag")
				}
				return nil
			},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				return Instance{Values: map[string]any{"hook.first": hook}}, nil
			},
		},
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
	}
}

// operationRegisterStatus reads one Operation register's state status
// directly from the store, the only reader that works after the owner closed.
func operationRegisterStatus(t *testing.T, store harness.Storage, sessionID, operationID string) harness.OperationState {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	if err != nil {
		t.Fatalf("ReadRegister(%s): %v", operationID, err)
	}
	var wire struct {
		State struct {
			Status harness.OperationState `json:"status"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode operation register payload: %v", err)
	}
	return wire.State.Status
}

func TestAssembledPhaseIntegration(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, integratedConfigDocument("T1", "L1"))
		writeServiceFile(t, agents.PathForConfig(e.configPath), integratedAgentsDocument)
		hook := &recordHook{name: "hook.first", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.SystemPrompt += "|hooked"
			return c
		}}
		prep := newCombinedPrep(e.events)
		ws := &workspaceOpens{}
		plugins := combinedPlugins(e, store, hook, ws)
		// The combined scenario runs on its own prepare/opener pair: the
		// ownerEnv option set with the controlled preparation replaced.
		openWith := func() (*Runtime, error) {
			opts := e.options(plugins...)
			opts.prepare = prep.prepare
			return open(ctx, opts)
		}

		// Raw storage is never exported into execution: the Core export ID is
		// outside the ordinary capability universe, so a definition selecting
		// it drops and the type is unknown at admission.
		leaky, err := openWith()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		leakySession, err := leaky.createSession(ctx, e.dataDir, "leaky")
		if err != nil {
			t.Fatalf("createSession(leaky): %v", err)
		}
		if err := leaky.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: leakySession.Identity.SessionID, OperationID: "op-leaky", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, Mode: harness.MessageModeRegular,
			})
			return err
		}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("submit through a definition selecting the Core storage export = %v, want harness.ErrInvalid (raw storage must not be exported into execution)", err)
		}
		if err := leaky.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}

		r, err := openWith()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		sat, err := r.Subscribe(1)
		if err != nil {
			t.Fatalf("Subscribe(saturated): %v", err)
		}
		healthy, err := r.Subscribe(64)
		if err != nil {
			t.Fatalf("Subscribe(healthy): %v", err)
		}

		// Two Sessions in two distinct Workspaces run over their own scopes.
		wsA := filepath.Join(e.home, "int-wsa")
		wsB := filepath.Join(e.home, "int-wsb")
		sessionA, err := r.createSession(ctx, wsA, "integrated")
		if err != nil {
			t.Fatalf("createSession(A): %v", err)
		}
		sessionB, err := r.createSession(ctx, wsB, "integrated")
		if err != nil {
			t.Fatalf("createSession(B): %v", err)
		}

		// Baseline turn on B: configured hook, dependency invocation, and one
		// Workspace scope for the one key used so far.
		submitThroughRuntime(t, r, sessionB.Identity.SessionID, "op-b1", "hello")
		prep.awaitCleanups(1)
		if got := ws.constructed(); !slices.Equal(got, []string{wsB}) {
			t.Fatalf("Workspace scope constructions after the first turn = %v, want exactly the %q identity", got, wsB)
		}
		if calls, opens, _ := prep.counts(); calls != 1 || opens != 1 {
			t.Fatalf("preparation/opener calls after the first turn = %d/%d, want 1/1", calls, opens)
		}
		if rec := readOperation(t, r, sessionB.Identity.SessionID, "op-b1"); rec.State.Status != harness.OperationSuccess ||
			rec.Admission.Execution.ConfigurationRevision != "1" || rec.Admission.Execution.SystemPrompt != "prompt-integrated|hooked" {
			t.Fatalf("first turn record = %+v, want the hooked revision-1 success", rec.Admission.Execution)
		}

		// The cleanup signal fires inside the Agent-scope disposal, so it does
		// not order op-b1's asynchronous Operation-close publication: consume
		// the healthy subscription until that exact closure is observed,
		// retaining every event for the final complete-sequence oracle.
		var observed []Event
		for {
			event, ok := nextEvent(t, healthy)
			if !ok {
				t.Fatal("the healthy subscription closed before op-b1's Operation-close publication")
			}
			observed = append(observed, event)
			if event.Kind == EventScopeClosed && event.Scope.Kind == ScopeOperation && event.Scope.Workspace == wsB {
				break
			}
		}

		// Blocked Operation on A: the model effect parks mid-execution.
		prep.armGate()
		submitThroughRuntime(t, r, sessionA.Identity.SessionID, "op-a1", "blocked")
		prep.awaitArrival(2)
		if got := ws.constructed(); !slices.Equal(got, []string{wsB, wsA}) {
			t.Fatalf("Workspace scope constructions = %v, want one per distinct key in first-use order", got)
		}
		// The saturated subscriber filled on the first publication and is
		// removed at the second (the /wsa scope open), while the healthy one
		// continues.
		first, ok := nextEvent(t, sat)
		if !ok || first.Kind != EventScopeOpened || first.Scope.Kind != ScopeWorkspace || first.Scope.Workspace != wsB {
			t.Fatalf("saturated subscriber first event = %+v (ok=%v), want the /wsb workspace open", first, ok)
		}
		if _, ok := nextEvent(t, sat); ok {
			t.Fatal("saturated subscriber still open at the second publication, want it removed and closed")
		}
		sat.Close()

		// The queued next revision is buffered while the Operation is blocked.
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: sessionA.Identity.SessionID, OperationID: "op-a2", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "queued"}}, Mode: harness.MessageModeQueued,
			})
			if err != nil {
				return err
			}
			if res.Disposition != harness.DispositionQueued {
				return fmt.Errorf("queued submit = %+v, want queued", res.Disposition)
			}
			return nil
		}); err != nil {
			t.Fatalf("queued submit: %v", err)
		}

		// Reload publishes the next revision over the blocked Operation.
		writeServiceFile(t, e.configPath, integratedConfigDocument("T2", "L2"))
		if revision, err := r.Reload(ctx); err != nil || revision != "2" {
			t.Fatalf("Reload over the blocked Operation = (%q, %v), want 2", revision, err)
		}

		// Release: the blocked Operation completes on its captured revision 1.
		prep.releaseGate()
		prep.awaitCleanups(2)
		// The queued delivery may already be preparing, so only the in-flight
		// pair is pinned here; the full sequence is asserted after it converges.
		if callsRev, openRevs := prep.revisions(); len(callsRev) < 2 || len(openRevs) < 2 ||
			callsRev[0] != "1" || callsRev[1] != "1" || openRevs[0] != "1" || openRevs[1] != "1" {
			t.Fatalf("revisions after release = prep %v open %v, want the in-flight pair retained on 1", callsRev, openRevs)
		}
		if rec := readOperation(t, r, sessionA.Identity.SessionID, "op-a1"); rec.State.Status != harness.OperationSuccess ||
			rec.Admission.Execution.ConfigurationRevision != "1" || rec.Admission.Execution.SystemPrompt != "prompt-integrated|hooked" {
			t.Fatalf("blocked Operation record = %+v, want the revision-1 hooked success", rec.Admission.Execution)
		}

		// The queued delivery captures the next revision: new hook settings
		// and the dependency's own settings from the same revision, while the
		// old capture keeps its content.
		prep.awaitCleanups(3)
		if callsRev, openRevs := prep.revisions(); !slices.Equal(callsRev, []string{"1", "1", "2"}) || !slices.Equal(openRevs, []string{"1", "1", "2"}) {
			t.Fatalf("revisions after the queued delivery = prep %v open %v, want the next admission on 2", callsRev, openRevs)
		}
		if got := hook.calls(); !slices.Equal(got, []hookCall{{revision: "1", tag: "T1"}, {revision: "1", tag: "T1"}, {revision: "2", tag: "T2"}}) {
			t.Fatalf("hook calls = %v, want the configured tag of each call's own revision", got)
		}
		events := e.events.all()
		if n := countEvents(events, "work:L1:base"); n != 2 {
			t.Fatalf("dependency invocations reading revision-1 settings = %d, want 2: %v", n, events)
		}
		if n := countEvents(events, "work:L2:base"); n != 1 {
			t.Fatalf("dependency invocations reading revision-2 settings = %d, want 1: %v", n, events)
		}
		if rec := readOperation(t, r, sessionA.Identity.SessionID, "op-a2"); rec.State.Status != harness.OperationSuccess ||
			rec.Admission.Execution.ConfigurationRevision != "2" {
			t.Fatalf("queued Operation record = %+v, want the revision-2 success", rec.Admission.Execution)
		}

		// Shutdown joins the blocked work: the parked model observes the
		// shutdown cancellation, holds the settlement, and Close cannot
		// converge until the execution and its required terminal commit are
		// done.
		prep.armGate()
		release := make(chan struct{})
		var once sync.Once
		doRelease := func() { once.Do(func() { close(release) }) }
		defer doRelease()
		prep.holdAfterCancel(release)
		// A submit racing the predecessor's post-terminal drain is buffered
		// and delivered by that drain (the Harness routing guarantee), so
		// either disposition starts this execution.
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: sessionA.Identity.SessionID, OperationID: "op-a3", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "parked"}}, Mode: harness.MessageModeRegular,
			})
			if err != nil {
				return err
			}
			if res.Disposition != harness.DispositionAdmitted && res.Disposition != harness.DispositionSteering {
				return fmt.Errorf("submit op-a3 = %+v, want admitted or buffered steering", res.Disposition)
			}
			return nil
		}); err != nil {
			t.Fatalf("Submit(op-a3): %v", err)
		}
		prep.awaitArrival(4)
		closeDone := make(chan error, 1)
		go func() { closeDone <- r.Close(ctx) }()
		select {
		case <-prep.canceled:
		case <-time.After(10 * time.Second):
			t.Fatal("shutdown cancellation never reached the parked execution")
		}
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned while the canceled execution was still settling (%v); shutdown joined nothing", err)
		case <-time.After(200 * time.Millisecond):
		}
		doRelease()
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close never converged after the joined execution settled")
		}
		// Close returned only after the joined execution settled and its
		// resource cleanup ran.
		prep.awaitCleanups(4)
		prep.releaseGate()
		if status := operationRegisterStatus(t, store, sessionA.Identity.SessionID, "op-a3"); status != harness.OperationInterruption {
			raw, _ := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionA.Identity.SessionID, Kind: harness.RegisterOperation, OperationID: "op-a3"})
			t.Fatalf("parked Operation terminal after Close = %q, want the committed interruption (payload %s)", status, raw.Payload)
		}

		// The healthy observer saw every committed transition in publication
		// order — scope identities exact, payloads stripped — ending with the
		// sorted Workspace closures and the Runtime. The prefix consumed up to
		// op-b1's Operation-close publication precedes the remaining drain.
		scopeEvent := func(kind EventKind, scopeKind ScopeKind, workspace string) Event {
			return Event{Kind: kind, Scope: ScopeInfo{Kind: scopeKind, Workspace: workspace}}
		}
		want := []Event{
			scopeEvent(EventScopeOpened, ScopeWorkspace, wsB),
			scopeEvent(EventScopeOpened, ScopeOperation, wsB),
			scopeEvent(EventScopeOpened, ScopeAgent, wsB),
			scopeEvent(EventScopeClosed, ScopeAgent, wsB),
			scopeEvent(EventScopeClosed, ScopeOperation, wsB),
			scopeEvent(EventScopeOpened, ScopeWorkspace, wsA),
			scopeEvent(EventScopeOpened, ScopeOperation, wsA),
			scopeEvent(EventScopeOpened, ScopeAgent, wsA),
			{Kind: EventConfiguration, ConfigurationRevision: "2"},
			scopeEvent(EventScopeClosed, ScopeAgent, wsA),
			scopeEvent(EventScopeClosed, ScopeOperation, wsA),
			scopeEvent(EventScopeOpened, ScopeOperation, wsA),
			scopeEvent(EventScopeOpened, ScopeAgent, wsA),
			scopeEvent(EventScopeClosed, ScopeAgent, wsA),
			scopeEvent(EventScopeClosed, ScopeOperation, wsA),
			scopeEvent(EventScopeOpened, ScopeOperation, wsA),
			scopeEvent(EventScopeOpened, ScopeAgent, wsA),
			scopeEvent(EventScopeClosed, ScopeAgent, wsA),
			scopeEvent(EventScopeClosed, ScopeOperation, wsA),
			scopeEvent(EventScopeClosed, ScopeWorkspace, wsA),
			scopeEvent(EventScopeClosed, ScopeWorkspace, wsB),
			scopeEvent(EventScopeClosed, ScopeRuntime, ""),
		}
		got := append(observed, drainClosed(t, healthy)...)
		if !slices.Equal(got, want) {
			t.Fatalf("healthy subscriber events = %+v, want the complete committed sequence %+v", got, want)
		}

		// Restart repair on the same store: recovery settles the abandoned
		// running state before any execution callback, then ordinary
		// admission proceeds on the repaired state.
		callsBefore, opensBefore, _ := prep.counts()
		seededSession, _ := seedRunningOperation(t, store, filepath.Join(e.home, "int-reseed"))
		r2, err := openWith()
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if calls, opens, _ := prep.counts(); calls != callsBefore || opens != opensBefore {
			t.Fatalf("preparation/opener calls across restart recovery = %d/%d, want recovery to invoke no execution callback", calls, opens)
		}
		if rec := readOperation(t, r2, seededSession, "seed-op"); rec.State.Status != harness.OperationInterruption ||
			rec.State.Terminal == nil || rec.State.Terminal.Detail != "Operation interrupted by Runtime loss." {
			t.Fatalf("restarted Operation state = %+v, want the terminal interruption repair", rec.State)
		}
		next, err := r2.createSession(ctx, wsB, "integrated")
		if err != nil {
			t.Fatalf("createSession after repair: %v", err)
		}
		submitThroughRuntime(t, r2, next.Identity.SessionID, "op-after-repair", "continue")
		prep.awaitCleanups(5)
		if rec := readOperation(t, r2, next.Identity.SessionID, "op-after-repair"); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("post-repair Operation = %+v, want success", rec.State)
		}
		if err := r2.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
