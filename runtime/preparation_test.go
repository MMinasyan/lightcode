package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
)

// Preparation fixtures: one controlled prepare/opener pair drives the real
// Harness over both stores. The configured provider is credential-keyed
// against an environment variable that stays absent throughout, so every row
// here also proves controlled preparation requires no credentials or provider
// connection.

var (
	errSupply    = errors.New("test supply failure")
	errHook      = errors.New("test hook failure")
	errOpen      = errors.New("test open failure")
	errExecClose = errors.New("test execution cleanup failure")
)

var prepModelRef = model.ModelRef{Provider: "prov", Model: "m"}

func prepConfigDocument(tag string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://prov.test/v1", "api_key_env": "PREP_TEST_ABSENT_KEY"},
      "discovery": false,
      "models": {
        "m": {"name": "M", "context_window": 4096},
        "zero": {"name": "Z", "context_window": 0}
      }
    }
  },
  "plugins": {"hooks": {"tag": "` + tag + `"}}
}`
}

// prepAgentsDocument keeps duplicate configured tool names on "dup", leaves
// "solo" with no selection, gives "ghostly" no model at all, selects the
// catalog model without a positive context window on "shallow", and selects
// Operation- and Agent-scoped capabilities on "deep".
const prepAgentsDocument = `{
  "solo": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"]},
  "dual": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "cap.shared"]},
  "dup": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo", "echo", "read"], "capabilities": ["hook.second", "cap.other"]},
  "wide": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo", "read"], "capabilities": ["hook.first", "cap.shared", "hook.second"]},
  "deep": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "op.native", "ag.native"]},
  "ghostly": {"system_prompt": "simple", "tools": ["echo"]},
  "shallow": {"model": "prov/zero", "system_prompt": "simple", "tools": ["echo"]}
}`

// recordHook is one declared PreparationHook capability: it retains the tool
// slice it received, records the Invocation, and answers with its configured
// mutation or error.
type recordHook struct {
	name   string
	events *traceLog

	mu          sync.Mutex
	seen        []hookCall
	inputTools  []model.ToolDefinition
	inputDesc   string
	fail        error
	mutate      func(harness.ExecutionCapture) harness.ExecutionCapture
	block       bool
	blockSignal chan struct{}
}

type hookCall struct {
	revision string
	tag      string
}

func (h *recordHook) Prepare(ctx context.Context, in Invocation, capture harness.ExecutionCapture) (harness.ExecutionCapture, error) {
	h.events.add("hook:" + h.name)
	h.mu.Lock()
	h.inputTools = capture.Tools
	if len(capture.Tools) > 0 {
		h.inputDesc = capture.Tools[0].Description
	}
	block, blockSignal := h.block, h.blockSignal
	fail, mutate := h.fail, h.mutate
	h.mu.Unlock()
	if block {
		if blockSignal != nil {
			select {
			case blockSignal <- struct{}{}:
			default:
			}
		}
		// Answer with a valid unchanged capture only after cancellation, so the
		// Runtime's own between-hooks check is the only observer.
		<-ctx.Done()
		return capture, nil
	}
	tag := ""
	if raw := in.Config("hooks"); len(raw) > 0 {
		var settings struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return harness.ExecutionCapture{}, err
		}
		tag = settings.Tag
	}
	h.mu.Lock()
	h.seen = append(h.seen, hookCall{revision: in.Revision(), tag: tag})
	h.mu.Unlock()
	if fail != nil {
		return harness.ExecutionCapture{}, fail
	}
	next := capture
	if mutate != nil {
		next = mutate(next)
	}
	return next, nil
}

func (h *recordHook) calls() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.seen...)
}

// retainedInputs returns the tool slice and first description handed to the
// last Prepare call. The slice still aliases the array the Runtime passed, so
// later in-place mutation by any party is observable through it, while the
// description records what the input held at entry.
func (h *recordHook) retainedInputs() ([]model.ToolDefinition, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inputTools, h.inputDesc
}

func (h *recordHook) set(fail error, mutate func(harness.ExecutionCapture) harness.ExecutionCapture) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fail, h.mutate = fail, mutate
}

// blockUntilCanceled parks the next Prepare call until its call context is
// canceled, after signaling the buffered start channel.
func (h *recordHook) blockUntilCanceled(started chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.block = true
	h.blockSignal = started
}

// prepStream is one fake accepted model stream yielding a completed text turn.
type prepStream struct{ i int }

func (s *prepStream) Recv() (model.StreamDelta, error) {
	if s.i > 0 {
		return model.StreamDelta{}, io.EOF
	}
	s.i++
	return model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}},
		FinishReason:     "stop",
	}, nil
}

func (s *prepStream) Close() error { return nil }

// prepEnv wires one configurationService, composition, Runtime scope,
// Workspace registry and real Harness around a controlled preparation.
type prepEnv struct {
	t         *testing.T
	store     harness.Storage
	sh        *serviceHarness
	workspace string
	svc       *configurationService
	comp      *composition
	runtime   *scope
	ws        *workspaceScopes
	h         *harness.Harness
	events    *traceLog
	hooks     [2]*recordHook

	hookOpens, capOpens, opOpens, agOpens atomic.Int64

	ownerCancel context.CancelFunc

	// turns is signaled by the supplied execution cleanup and opCloses by the
	// Operation scope plugin's closer: deterministic completion barriers.
	turns    chan struct{}
	opCloses chan struct{}
	turnSeen int

	mu             sync.Mutex
	supplyCalls    int
	sels           []selection
	openSels       []selection
	openAdmissions []harness.OperationAdmission
	supplyErr      error
	nilOpener      bool
	hold           chan struct{}
	arrive         chan struct{}
	mutateCapture  func(harness.ExecutionCapture) harness.ExecutionCapture
	openErr        error
	invalidOpen    bool
	noCleanup      bool
	cleanupErr     error
	modelGate      chan struct{}
	modelArrived   chan struct{}
}

func newPrepEnv(t *testing.T, store harness.Storage) *prepEnv {
	t.Helper()
	sh := newServiceHarness(t)
	writeServiceFile(t, sh.configPath, prepConfigDocument("A"))
	writeServiceFile(t, agents.PathForConfig(sh.configPath), prepAgentsDocument)
	e := &prepEnv{
		t: t, store: store, sh: sh, workspace: sh.dataDir, events: &traceLog{},
		modelArrived: make(chan struct{}, 16),
		turns:        make(chan struct{}, 16),
		opCloses:     make(chan struct{}, 16),
	}
	e.hooks[0] = &recordHook{name: "hook.first", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
		c.SystemPrompt += "|first"
		return c
	}}
	e.hooks[1] = &recordHook{name: "hook.second", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
		c.SystemPrompt += "|second"
		return c
	}}
	owner, cancel := context.WithCancel(context.Background())
	e.ownerCancel = cancel
	e.comp = mustComposition(t, e.plugins()...)
	e.runtime = mustOpenScope(t, e.comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	e.ws = newWorkspaceScopes(owner, e.comp, []*scope{e.runtime})
	e.svc = newConfigurationService(owner, e.comp, sh.loader, sh.configPath)
	if _, err := e.svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	h, err := harness.New(owner, harness.Dependencies{
		Storage: store,
		Prepare: newPreparation(e.svc, e.comp, e.runtime, e.ws, e.supply).bind(),
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	e.h = h
	return e
}

func (e *prepEnv) plugins() []Plugin {
	return []Plugin{
		{
			ID:       "hooks",
			Scope:    ScopeRuntime,
			Provides: []CapabilitySpec{Spec[PreparationHook]("hook.first"), Spec[PreparationHook]("hook.second")},
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
				e.hookOpens.Add(1)
				return Instance{Values: map[string]any{"hook.first": e.hooks[0], "hook.second": e.hooks[1]}}, nil
			},
		},
		{
			ID:       "caps",
			Scope:    ScopeWorkspace,
			Provides: []CapabilitySpec{Spec[greeter]("cap.shared"), Spec[greeter]("cap.other")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				e.capOpens.Add(1)
				return Instance{Values: map[string]any{
					"cap.shared": loudGreeter{word: "shared"},
					"cap.other":  loudGreeter{word: "other"},
				}}, nil
			},
		},
		{
			ID:       "ops",
			Scope:    ScopeOperation,
			Provides: []CapabilitySpec{Spec[greeter]("op.native")},
			Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
				e.opOpens.Add(1)
				e.events.add("op-open:" + info.OperationID)
				return Instance{
					Values: map[string]any{"op.native": loudGreeter{word: "op"}},
					Close: func() error {
						e.events.add("op-close")
						select {
						case e.opCloses <- struct{}{}:
						default:
						}
						return nil
					},
				}, nil
			},
		},
		{
			ID:       "ags",
			Scope:    ScopeAgent,
			Provides: []CapabilitySpec{Spec[greeter]("ag.native")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				e.agOpens.Add(1)
				e.events.add("ag-open")
				return Instance{
					Values: map[string]any{"ag.native": loudGreeter{word: "ag"}},
					Close:  func() error { e.events.add("ag-close"); return nil },
				}, nil
			},
		},
	}
}

// supply is the controlled prepare function: it records the selection,
// optionally parks until the test's hold releases, then answers with a capture
// built from the selected view and the fixture opener.
func (e *prepEnv) supply(ctx context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	e.mu.Lock()
	e.supplyCalls++
	e.sels = append(e.sels, sel)
	hold, supplyErr, nilOpener, mutate := e.hold, e.supplyErr, e.nilOpener, e.mutateCapture
	arrive := e.arrive
	e.mu.Unlock()
	e.events.add("prepare:" + req.Session.AgentType)
	if arrive != nil {
		select {
		case arrive <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return harness.ExecutionCapture{}, nil, ctx.Err()
		}
		e.mu.Lock()
		e.hold = nil
		e.mu.Unlock()
	}
	if supplyErr != nil {
		return harness.ExecutionCapture{}, nil, supplyErr
	}
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		SystemPrompt:          "prompt-" + req.Session.AgentType,
		Tools:                 captureTools(sel.agent.Tools),
		Capabilities:          append([]string(nil), sel.agent.Capabilities...),
	}
	if mutate != nil {
		capture = mutate(capture)
	}
	if nilOpener {
		return capture, nil, nil
	}
	return capture, e.opener, nil
}

// captureTools maps configured tool names, duplicates included, onto one
// valid unique capture list in preserved first-seen order.
func captureTools(names []string) []model.ToolDefinition {
	seen := make(map[string]bool, len(names))
	var out []model.ToolDefinition
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, model.ToolDefinition{
			Name:        name,
			Description: "tool " + name,
			Parameters:  json.RawMessage(`{"type":"object"}`),
		})
	}
	return out
}

func (e *prepEnv) opener(_ context.Context, adm harness.OperationAdmission, sel selection) (harness.Execution, error) {
	e.mu.Lock()
	openErr, invalid, noCleanup, cleanupErr := e.openErr, e.invalidOpen, e.noCleanup, e.cleanupErr
	e.openAdmissions = append(e.openAdmissions, adm)
	e.openSels = append(e.openSels, sel)
	e.mu.Unlock()
	if openErr != nil {
		e.events.add("open-fail")
		return harness.Execution{}, openErr
	}
	e.events.add("open:" + adm.OperationID)
	execution := harness.Execution{
		Model: e.model(sel),
		Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
			return harness.PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no concrete tools yet"}}
		},
	}
	if invalid {
		execution.Model = nil
	}
	if !noCleanup {
		execution.Close = func() error {
			e.events.add("exec-cleanup")
			select {
			case e.turns <- struct{}{}:
			default:
			}
			e.mu.Lock()
			err := cleanupErr
			e.mu.Unlock()
			return err
		}
	}
	return execution, nil
}

func (e *prepEnv) model(sel selection) agent.ModelEffect {
	return func(ctx context.Context, _ model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		e.events.add("model")
		if _, ok := sel.bindings.entries["cap.shared"]; ok {
			worker, err := Bind[greeter](sel.bindings, "cap.shared")
			if err != nil {
				return agent.ModelSettlement{}, err
			}
			e.events.add("greet:" + worker.Greet())
		}
		select {
		case e.modelArrived <- struct{}{}:
		default:
		}
		e.mu.Lock()
		gate := e.modelGate
		e.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return agent.ModelSettlement{}, ctx.Err()
			}
		}
		out, err := assemble(prepModelRef, &prepStream{})
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &out}, nil
	}
}

// --- fixture control helpers ---

func (e *prepEnv) session(agentType string) string {
	e.t.Helper()
	rec, err := e.h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: e.workspace, AgentType: agentType})
	if err != nil {
		e.t.Fatalf("CreateSession(%q): %v", agentType, err)
	}
	return rec.Identity.SessionID
}

func (e *prepEnv) submit(ctx context.Context, sessionID, operationID, text string, mode harness.MessageMode) (harness.SubmitResult, error) {
	return e.h.Submit(ctx, harness.SubmitRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      harness.InputOriginUser,
		Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
		Mode:        mode,
	})
}

func (e *prepEnv) admit(sessionID, operationID, text string) harness.SubmitResult {
	e.t.Helper()
	res, err := e.submit(context.Background(), sessionID, operationID, text, harness.MessageModeRegular)
	if err != nil || res.Disposition != harness.DispositionAdmitted || res.Operation == nil {
		e.t.Fatalf("submit %q = %+v err %v, want admitted", operationID, res, err)
	}
	return res
}

// admitOrBuffer submits the same way but tolerates the deterministic Harness
// routing while a predecessor's post-terminal drain is in flight: an idle
// Session admits immediately, and an active one buffers the message, which
// its drain then delivers through ordinary admission exactly once.
func (e *prepEnv) admitOrBuffer(sessionID, operationID, text string) {
	e.t.Helper()
	if _, err := e.submit(context.Background(), sessionID, operationID, text, harness.MessageModeRegular); err != nil {
		e.t.Fatalf("submit %q: %v", operationID, err)
	}
}

// holdNextPrepare arms the next supply call to park; the returned channel
// releases it once.
func (e *prepEnv) holdNextPrepare() chan struct{} {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hold = make(chan struct{})
	e.arrive = make(chan struct{}, 4)
	return e.hold
}

func (e *prepEnv) prepared() <-chan struct{} {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.arrive
}

func (e *prepEnv) parkModels() <-chan struct{} {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.modelGate = make(chan struct{})
	return e.modelArrived
}

func (e *prepEnv) releaseModels() {
	e.t.Helper()
	e.mu.Lock()
	gate := e.modelGate
	e.modelGate = nil
	e.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// awaitCleanups blocks until the n-th supplied execution cleanup has run;
// every cleanup signals the buffered turns channel from inside the supplied
// close itself.
func (e *prepEnv) awaitCleanups(n int) {
	e.t.Helper()
	for e.turnSeen < n {
		select {
		case <-e.turns:
			e.turnSeen++
		case <-time.After(10 * time.Second):
			e.t.Fatalf("cleanup %d never ran; events %v", e.turnSeen+1, e.events.all())
		}
	}
}

// waitOperationClosed blocks until the Operation scope plugin's closer has
// run; the closer signals the buffered opCloses channel itself.
func (e *prepEnv) waitOperationClosed() {
	e.t.Helper()
	select {
	case <-e.opCloses:
	case <-time.After(10 * time.Second):
		e.t.Fatalf("Operation scope never closed; events %v", e.events.all())
	}
}

func (e *prepEnv) converge() error {
	e.t.Helper()
	e.ownerCancel()
	return e.h.Wait(context.Background())
}

func (e *prepEnv) counts() int {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.supplyCalls
}

func (e *prepEnv) selectionAt(i int) selection {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.sels) {
		e.t.Fatalf("selection %d missing, have %d", i, len(e.sels))
	}
	return e.sels[i]
}

func (e *prepEnv) admissionAt(i int) harness.OperationAdmission {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.openAdmissions) {
		e.t.Fatalf("admission %d missing, have %d", i, len(e.openAdmissions))
	}
	return e.openAdmissions[i]
}

func (e *prepEnv) openSelAt(i int) selection {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.openSels) {
		e.t.Fatalf("opener selection %d missing, have %d", i, len(e.openSels))
	}
	return e.openSels[i]
}

func (e *prepEnv) inputEntry(sessionID, operationID string) string {
	e.t.Helper()
	entries, err := e.store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		e.t.Fatalf("ReadEntries: %v", err)
	}
	for _, entry := range entries {
		if entry.Kind == harness.EntryInput && entry.OperationID == operationID {
			return entry.ID
		}
	}
	e.t.Fatalf("no input entry owned by %q in session %q", operationID, sessionID)
	return ""
}

func boundIDs(b Bindings) []string {
	ids := make([]string, 0, len(b.entries))
	for id := range b.entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func countEvents(events []string, want string) int {
	n := 0
	for _, event := range events {
		if event == want {
			n++
		}
	}
	return n
}

// eachPrepStore runs one fixture against memory and a temporary SQLite store.
func eachPrepStore(t *testing.T, run func(t *testing.T, store harness.Storage)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		run(t, storage.NewMemory())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		defer store.Close()
		run(t, store)
	})
}

// TestPreparationHappyPathThroughHarness drives one real admission end to end
// on both stores: ordered hooks replace only the prompt, the committed
// post-hook capture reaches the associated opener with the live execution
// selection, the Operation and Agent scopes are constructed per execution and
// disposed in reverse order with the concrete cleanup first, and the terminal
// settles success with no retained cleanup error.
func TestPreparationHappyPathThroughHarness(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("wide")
		res := e.admit(session, "op-1", "hello")

		want := harness.ExecutionCapture{
			ConfigurationRevision: "1",
			Model:                 prepModelRef,
			SystemPrompt:          "prompt-wide|first|second",
			Tools:                 captureTools([]string{"echo", "read"}),
			Capabilities:          []string{"hook.first", "cap.shared", "hook.second"},
		}
		got := res.Operation.Admission.Execution
		if !slices.Equal(got.Capabilities, want.Capabilities) || !slices.Equal(toolNames(got.Tools), toolNames(want.Tools)) ||
			got.Model != want.Model || got.ConfigurationRevision != want.ConfigurationRevision || got.SystemPrompt != want.SystemPrompt {
			t.Fatalf("committed capture = %+v, want the post-hook selection capture %+v", got, want)
		}

		e.awaitCleanups(1)
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		// The opener received the actual committed admission: its capture is the
		// final post-hook value, never a stale pre-hook one.
		adm := e.admissionAt(0)
		if adm.Execution.SystemPrompt != want.SystemPrompt || adm.OperationID != "op-1" {
			t.Fatalf("opener admission = %+v, want the committed post-hook capture for op-1", adm)
		}
		if rev := e.selectionAt(0).invocation.Revision(); rev != "1" {
			t.Fatalf("opener selection revision = %q, want 1", rev)
		}

		op, err := e.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if op.State.Status != harness.OperationSuccess {
			t.Fatalf("operation status = %q, want success", op.State.Status)
		}

		// Hooks ran in selected order inside the preparation guard; the execution
		// selection stayed live through the effect and settlement, and reverse
		// disposal ran the concrete cleanup before the scope closers.
		wantEvents := []string{
			"prepare:wide", "hook:hook.first", "hook:hook.second",
			"op-open:op-1", "ag-open", "open:op-1",
			"model", "greet:SHARED", "exec-cleanup",
			"ag-close", "op-close",
		}
		if events := e.events.all(); !slices.Equal(events, wantEvents) {
			t.Fatalf("events = %v, want %v", events, wantEvents)
		}
		if got := e.hookOpens.Load(); got != 1 {
			t.Fatalf("hook factories ran %d times, want 1", got)
		}
		if got := e.opOpens.Load(); got != 1 {
			t.Fatalf("Operation scope opened %d times, want 1", got)
		}
	})
}

func toolNames(tools []model.ToolDefinition) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// TestPreparationReloadDoesNotRecaptureInFlightPreparation holds preparation on
// revision 1, publishes revision 2, then admits and opens on revision 1, and
// verifies the next capture uses 2 — for normal, drained and Fork admission
// alike, on both stores.
func TestPreparationReloadDoesNotRecaptureInFlightPreparation(t *testing.T) {
	for _, kind := range []string{"normal", "drained", "fork"} {
		t.Run(kind, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				runReloadOrdering(t, store, kind)
			})
		})
	}
}

func runReloadOrdering(t *testing.T, store harness.Storage, kind string) {
	e := newPrepEnv(t, store)
	session := e.session("dual")

	var hold chan struct{}
	var submitErr error
	switch kind {
	case "drained":
		arrived := e.parkModels()
		e.admit(session, "op-1", "one")
		<-arrived
		if res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
			t.Fatalf("queued submit = %+v err %v, want queued", res, err)
		}
		hold = e.holdNextPrepare()
		e.releaseModels() // op-1 converges; the post-terminal drain reaches the held preparation
	default:
		e.admit(session, "op-1", "one")
		e.awaitCleanups(1)
		hold = e.holdNextPrepare()
		switch kind {
		case "normal":
			go func() {
				_, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeRegular)
				e.mu.Lock()
				submitErr = err
				e.mu.Unlock()
			}()
		case "fork":
			boundary := e.inputEntry(session, "op-1")
			go func() {
				_, err := e.h.Fork(context.Background(), harness.ForkRequest{
					SourceSessionID: session,
					BoundaryEntryID: boundary,
					OperationID:     "fork-op",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				e.mu.Lock()
				submitErr = err
				e.mu.Unlock()
			}()
		}
	}
	<-e.prepared()
	if _, err := e.svc.publish(context.Background()); err != nil {
		t.Fatalf("reload publish: %v", err)
	}
	close(hold)

	e.awaitCleanups(2)
	e.mu.Lock()
	if submitErr != nil {
		e.mu.Unlock()
		t.Fatalf("held admission failed: %v", submitErr)
	}
	e.mu.Unlock()
	// The in-flight admission committed on revision 1 and opened with the
	// same captured revision.
	if got := e.admissionAt(1).Execution.ConfigurationRevision; got != "1" {
		t.Fatalf("overlapping-reload admission committed revision %q, want the captured 1", got)
	}
	if rev := e.selectionAt(1).invocation.Revision(); rev != "1" {
		t.Fatalf("overlapping-reload preparation selection revision = %q, want 1", rev)
	}
	if rev := e.openSelAt(1).invocation.Revision(); rev != "1" {
		t.Fatalf("overlapping-reload opener selection revision = %q, want the retained 1", rev)
	}

	// The next capture uses revision 2.
	e.admitOrBuffer(session, "op-next", "next")
	e.awaitCleanups(3)
	if got := e.admissionAt(2).Execution.ConfigurationRevision; got != "2" {
		t.Fatalf("next admission revision = %q, want 2", got)
	}
	if err := e.converge(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if kind == "drained" {
		// Buffered messages captured only on delivery, and the successor
		// preparation started only after the predecessor's cleanup:
		// close-before-successor.
		if calls := e.counts(); calls != 3 {
			t.Fatalf("preparation calls = %d, want one per delivered admission", calls)
		}
		events := e.events.all()
		cleanup, secondPrepare, seen := -1, -1, 0
		for i, event := range events {
			if event == "exec-cleanup" && cleanup < 0 {
				cleanup = i
			}
			if event == "prepare:dual" {
				seen++
				if seen == 2 {
					secondPrepare = i
				}
			}
		}
		if cleanup < 0 || secondPrepare < 0 || cleanup > secondPrepare {
			t.Fatalf("events = %v, want the first cleanup before the successor preparation", events)
		}
	}
}

// TestPreparationDeliveryIdentity proves on both stores that existing IDs
// skip preparation and buffered messages capture only on delivery.
func TestPreparationDeliveryIdentity(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("dual")
		arrived := e.parkModels()
		e.admit(session, "op-1", "one")
		<-arrived

		if res, err := e.submit(context.Background(), session, "op-1", "again", harness.MessageModeRegular); err != nil || res.Disposition != harness.DispositionExisting {
			t.Fatalf("reused id = %+v err %v, want existing without preparation", res, err)
		}
		if res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
			t.Fatalf("queued = %+v err %v, want queued", res, err)
		}
		if calls := e.counts(); calls != 1 {
			t.Fatalf("preparation calls = %d, want only the admitted execution prepared so far", calls)
		}
		e.releaseModels()
		e.awaitCleanups(2)
		if calls := e.counts(); calls != 2 {
			t.Fatalf("preparation calls = %d, want the drained delivery to capture once", calls)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPreparationRejectsAdmissionAtTheBoundary proves on both stores that
// every pre-admission rejection reports its class at admission, prepares or
// opens nothing, and admits no Operation.
func TestPreparationRejectsAdmissionAtTheBoundary(t *testing.T) {
	cases := []struct {
		name        string
		agentType   string
		arm         func(e *prepEnv)
		want        error
		wantPrepars int
	}{
		{name: "unknown agent type", agentType: "ghost", want: harness.ErrInvalid},
		{name: "selected model absent from the catalog", agentType: "ghostly", want: harness.ErrInvalid},
		{name: "selected model without a positive context window", agentType: "shallow", want: harness.ErrInvalid},
		{
			name: "capture model differs from the selection", agentType: "solo",
			arm: func(e *prepEnv) {
				e.mutateCapture = func(c harness.ExecutionCapture) harness.ExecutionCapture {
					c.Model = model.ModelRef{Provider: "prov", Model: "other"}
					return c
				}
			},
			want: harness.ErrInvalid, wantPrepars: 1,
		},
		{
			name: "capture revision differs from the snapshot", agentType: "solo",
			arm: func(e *prepEnv) {
				e.mutateCapture = func(c harness.ExecutionCapture) harness.ExecutionCapture {
					c.ConfigurationRevision = "rev-9"
					return c
				}
			},
			want: harness.ErrInvalid, wantPrepars: 1,
		},
		{
			name: "capture expands beyond the selected names", agentType: "dual",
			arm: func(e *prepEnv) {
				e.mutateCapture = func(c harness.ExecutionCapture) harness.ExecutionCapture {
					c.Capabilities = append(append([]string(nil), c.Capabilities...), "cap.other")
					return c
				}
			},
			want: harness.ErrInvalid, wantPrepars: 1,
		},
		{
			name: "capture names an unselected short-scope capability", agentType: "dual",
			arm: func(e *prepEnv) {
				e.mutateCapture = func(c harness.ExecutionCapture) harness.ExecutionCapture {
					c.Capabilities = append(append([]string(nil), c.Capabilities...), "op.native")
					return c
				}
			},
			want: harness.ErrInvalid, wantPrepars: 1,
		},
		{
			name: "nil opener prevents admission", agentType: "solo",
			arm:         func(e *prepEnv) { e.nilOpener = true },
			want:        harness.ErrInvalid,
			wantPrepars: 1,
		},
		{
			name: "controlled preparation error", agentType: "solo",
			arm:         func(e *prepEnv) { e.supplyErr = errSupply },
			want:        errSupply,
			wantPrepars: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newPrepEnv(t, store)
				if tc.arm != nil {
					tc.arm(e)
				}
				session := e.session(tc.agentType)
				res, err := e.submit(context.Background(), session, "op-x", "x", harness.MessageModeRegular)
				if !errors.Is(err, tc.want) {
					t.Fatalf("submit error = %v, want %v", err, tc.want)
				}
				if res.Disposition == harness.DispositionAdmitted {
					t.Fatalf("rejected preparation admitted an Operation")
				}
				if got := e.opOpens.Load() + e.agOpens.Load(); got != 0 {
					t.Fatalf("short scopes opened %d times without admission", got)
				}
				if _, err := e.h.ReadOperation(context.Background(), session, "op-x"); !errors.Is(err, harness.ErrNotFound) {
					t.Fatalf("rejected admission left an Operation: %v", err)
				}
				if calls := e.counts(); calls != tc.wantPrepars {
					t.Fatalf("preparation calls = %d, want %d", calls, tc.wantPrepars)
				}
				if err := e.converge(); err != nil {
					t.Fatalf("Wait: %v", err)
				}
			})
		})
	}
}

// TestPreparationHooksAreOrderedAndValidated exercises the pure hook boundary
// through the real Harness on both stores: identity and selection expansion,
// invalid tool lists, non-owned capture handout, hook errors, cancellation and
// a closed binding view all abort admission with no partial consequence
// published.
func TestPreparationHooksAreOrderedAndValidated(t *testing.T) {
	t.Run("hook replacement of identity is rejected", func(t *testing.T) {
		violations := []struct {
			name   string
			mutate func(c harness.ExecutionCapture) harness.ExecutionCapture
		}{
			{"model", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Model = model.ModelRef{Provider: "prov", Model: "other"}
				return c
			}},
			{"revision", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.ConfigurationRevision = "9"
				return c
			}},
			{"capabilities", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Capabilities = []string{"hook.first"}
				return c
			}},
		}
		for _, v := range violations {
			t.Run(v.name, func(t *testing.T) {
				eachPrepStore(t, func(t *testing.T, store harness.Storage) {
					e := newPrepEnv(t, store)
					e.hooks[0].set(nil, v.mutate)
					session := e.session("wide")
					if _, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular); !errors.Is(err, harness.ErrInvalid) {
						t.Fatalf("submit error = %v, want harness.ErrInvalid", err)
					}
					events := strings.Join(e.events.all(), ",")
					if !strings.Contains(events, "hook:hook.first") || strings.Contains(events, "hook:hook.second") {
						t.Fatalf("events = %v, want the failing hook to stop the rest", e.events.all())
					}
					if got := e.opOpens.Load() + e.agOpens.Load(); got != 0 {
						t.Fatalf("short scope factories ran after a rejected hook")
					}
					if err := e.converge(); err != nil {
						t.Fatalf("Wait: %v", err)
					}
				})
			})
		}
	})
	t.Run("hook tool violations are rejected", func(t *testing.T) {
		violations := []struct {
			name   string
			mutate func(c harness.ExecutionCapture) harness.ExecutionCapture
		}{
			{"tool outside the original capture", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Tools = append(append([]model.ToolDefinition(nil), c.Tools...), model.ToolDefinition{Name: "extra", Parameters: json.RawMessage(`{}`)})
				return c
			}},
			{"invalid duplicate tool capture", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Tools = append(append([]model.ToolDefinition(nil), c.Tools...), c.Tools[0])
				return c
			}},
		}
		for _, v := range violations {
			t.Run(v.name, func(t *testing.T) {
				eachPrepStore(t, func(t *testing.T, store harness.Storage) {
					e := newPrepEnv(t, store)
					e.hooks[1].set(nil, v.mutate)
					session := e.session("wide")
					if _, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular); !errors.Is(err, harness.ErrInvalid) {
						t.Fatalf("submit error = %v, want harness.ErrInvalid", err)
					}
					if _, err := e.h.ReadOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
						t.Fatalf("rejected hook left an Operation: %v", err)
					}
					if err := e.converge(); err != nil {
						t.Fatalf("Wait: %v", err)
					}
				})
			})
		}
	})
	t.Run("hooks receive and return owned captures", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.hooks[0].set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Tools[0].Description = "hook.first" // in-place on what it received
				return c
			})
			e.hooks[1].set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Tools[0].Description = "hook.second" // in-place again
				return c
			})
			session := e.session("wide")
			res := e.admit(session, "op-1", "x")
			e.awaitCleanups(1)
			// Each hook's input carried the previous result (fresh owned copies
			// chain), and mutating the second hook's own copy cannot retroactively
			// change the array the first hook received and retained.
			if firstTools, firstDesc := e.hooks[0].retainedInputs(); firstDesc != "tool echo" || len(firstTools) == 0 || firstTools[0].Description != "hook.first" {
				t.Fatalf("hook.first input = %+v description %q, want the unaliased pre-hook capture it mutated", firstTools, firstDesc)
			}
			if _, secondDesc := e.hooks[1].retainedInputs(); secondDesc != "hook.first" {
				t.Fatalf("hook.second input description at entry = %q, want the previous owned result", secondDesc)
			}
			// The committed capture carries the final owned result.
			if got := res.Operation.Admission.Execution.Tools; len(got) == 0 || got[0].Description != "hook.second" {
				t.Fatalf("committed tools = %+v, want the final owned result", got)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
	t.Run("hook error aborts admission", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.hooks[0].set(errHook, nil)
			session := e.session("wide")
			if _, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular); !errors.Is(err, errHook) {
				t.Fatalf("submit error = %v, want %v", err, errHook)
			}
			if _, err := e.h.ReadOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("failed hook left an Operation: %v", err)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
	t.Run("cancellation during preparation aborts admission", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			session := e.session("dual")
			_ = e.holdNextPrepare()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := e.submit(ctx, session, "op-1", "x", harness.MessageModeRegular)
				done <- err
			}()
			<-e.prepared()
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled submit error = %v, want context.Canceled", err)
			}
			if _, err := e.h.ReadOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("canceled preparation left an Operation: %v", err)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
	t.Run("cancellation between hooks aborts admission", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			started := make(chan struct{}, 1)
			e.hooks[0].blockUntilCanceled(started)
			session := e.session("wide")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := e.submit(ctx, session, "op-1", "x", harness.MessageModeRegular)
				done <- err
			}()
			<-started
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled submit error = %v, want context.Canceled", err)
			}
			if slices.Contains(e.events.all(), "hook:hook.second") {
				t.Fatalf("later hook ran after cancellation: %v", e.events.all())
			}
			if _, err := e.h.ReadOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("canceled hook run left an Operation: %v", err)
			}
			_ = e.converge()
		})
	})
	t.Run("closed binding view rejects the hook call", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			session := e.session("wide")
			hold := e.holdNextPrepare()
			done := make(chan error, 1)
			go func() {
				_, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular)
				done <- err
			}()
			<-e.prepared()
			wait, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := e.runtime.close(wait); err != nil {
				t.Fatalf("runtime close: %v", err)
			}
			close(hold)
			if err := <-done; !errors.Is(err, ErrClosed) {
				t.Fatalf("submit after closed bindings error = %v, want ErrClosed", err)
			}
			if _, err := e.h.ReadOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("rejected hook admission left an Operation: %v", err)
			}
			if got := e.opOpens.Load() + e.agOpens.Load(); got != 0 {
				t.Fatalf("short scope factories ran %d times after a closed binding view", got)
			}
			_ = e.converge()
		})
	})
}

// TestPreparationOldAndNewPerCallConfiguration proves on both stores that each
// call reads its own settings from the revision its capture carries: the first
// admission's hooks see revision 1 with tag A, and after a reload the next
// hooks see revision 2 with tag B while the old committed capture keeps its
// content.
func TestPreparationOldAndNewPerCallConfiguration(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("wide")
		e.admit(session, "op-1", "one")
		e.awaitCleanups(1)

		writeServiceFile(e.t, e.sh.configPath, prepConfigDocument("B"))
		if _, err := e.svc.publish(context.Background()); err != nil {
			t.Fatalf("reload publish: %v", err)
		}
		e.admitOrBuffer(session, "op-2", "two")
		e.awaitCleanups(2)

		want := []hookCall{{revision: "1", tag: "A"}, {revision: "2", tag: "B"}}
		for _, hook := range e.hooks {
			if got := hook.calls(); !slices.Equal(got, want) {
				t.Fatalf("hook %s calls = %v, want %v", hook.name, got, want)
			}
		}
		first, err := e.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if first.Admission.Execution.ConfigurationRevision != "1" {
			t.Fatalf("the old capture changed to revision %q", first.Admission.Execution.ConfigurationRevision)
		}
		if second := e.admissionAt(1); second.Execution.ConfigurationRevision != "2" {
			t.Fatalf("new admission revision = %q, want 2", second.Execution.ConfigurationRevision)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPreparationDistinctSelectionsOverReusedInstances compares two Agent
// selections over the same constructed instances on both stores: the
// long-lived plugins open once, each selection carries its own capability set
// and configured tool names (duplicates retained in the view, unique in the
// capture), and an Agent with no selection prepares cleanly without any hook.
func TestPreparationDistinctSelectionsOverReusedInstances(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		dual := e.session("dual")
		dup := e.session("dup")
		solo := e.session("solo")

		e.admit(dual, "op-1", "one")
		e.awaitCleanups(1)
		e.admitOrBuffer(dup, "op-2", "two")
		e.awaitCleanups(2)
		e.admitOrBuffer(solo, "op-3", "three")
		e.awaitCleanups(3)

		sels := []selection{e.selectionAt(0), e.selectionAt(1), e.selectionAt(2)}
		if got := sels[0].agent.Name; got != "dual" {
			t.Fatalf("first selection agent = %q", got)
		}
		if got := boundIDs(sels[0].bindings); !slices.Equal(got, []string{"cap.shared", "hook.first"}) {
			t.Fatalf("dual selection bindings = %v", got)
		}
		if got := boundIDs(sels[1].bindings); !slices.Equal(got, []string{"cap.other", "hook.second"}) {
			t.Fatalf("dup selection bindings = %v", got)
		}
		if got := sels[1].agent.Tools; !slices.Equal(got, []string{"echo", "echo", "read"}) {
			t.Fatalf("dup selection view lost configured duplicates: %v", got)
		}
		if got := e.admissionAt(1).Execution.Tools; !slices.Equal(toolNames(got), []string{"echo", "read"}) {
			t.Fatalf("dup committed capture tools = %+v, want the valid unique list", got)
		}
		if got := sels[2].bindings; len(got.entries) != 0 {
			t.Fatalf("solo selection bindings = %v, want none", boundIDs(got))
		}
		if got := e.admissionAt(2).Execution.Capabilities; len(got) != 0 {
			t.Fatalf("solo committed capabilities = %v, want none", got)
		}
		if got := e.hookOpens.Load() + e.capOpens.Load(); got != 2 {
			t.Fatalf("long-lived factories ran %d times, want exactly one per plugin", got)
		}
		// Only the selected hook ran per admission: hook.first on dual, hook.second
		// on dup, neither on solo.
		if calls := e.hooks[0].calls(); len(calls) != 1 {
			t.Fatalf("hook.first calls = %v, want dual only", calls)
		}
		if calls := e.hooks[1].calls(); len(calls) != 1 {
			t.Fatalf("hook.second calls = %v, want dup only", calls)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPreparationSelectsShortScopeCapabilitiesAtOpen proves a selected
// Operation- or Agent-scoped capability is valid but absent from the
// preparation view, and present for the committed opener's strict full
// selection across all four scopes.
func TestPreparationSelectsShortScopeCapabilitiesAtOpen(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("deep")
		e.admit(session, "op-1", "one")
		e.awaitCleanups(1)

		if got := boundIDs(e.selectionAt(0).bindings); !slices.Equal(got, []string{"hook.first"}) {
			t.Fatalf("preparation bindings = %v, want only the currently available selected capabilities", got)
		}
		if got := boundIDs(e.openSelAt(0).bindings); !slices.Equal(got, []string{"ag.native", "hook.first", "op.native"}) {
			t.Fatalf("opener selection bindings = %v, want the full strict selection over all four scopes", got)
		}
		if got := e.admissionAt(0).Execution.Capabilities; !slices.Equal(got, []string{"hook.first", "op.native", "ag.native"}) {
			t.Fatalf("committed capabilities = %v, want the Agent's full selected names", got)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPreparationExecutionLifetimeAndCleanup covers the execution lifetime
// rows around the returned opener on both stores: a nil supplied cleanup
// still closes the scopes, a successful invalid execution closes once and
// never runs the Agent, an opener failure unwinds owned scopes, and a cleanup
// error is retained without rewriting the settled terminal.
func TestPreparationExecutionLifetimeAndCleanup(t *testing.T) {
	t.Run("nil supplied cleanup still closes scopes", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.noCleanup = true
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.waitOperationClosed()
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			events := e.events.all()
			joined := strings.Join(events, ",")
			if !strings.Contains(joined, "greet:SHARED,ag-close,op-close") || slices.Contains(events, "exec-cleanup") {
				t.Fatalf("events = %v, want both scopes closed with no concrete cleanup", events)
			}
		})
	})
	t.Run("successful invalid execution closes once and never runs the agent", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.invalidOpen = true
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.waitOperationClosed()
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			events := e.events.all()
			if closes := countEvents(events, "ag-close"); closes != 1 {
				t.Fatalf("Agent scope closed %d times, want once: %v", closes, events)
			}
			if slices.Contains(events, "model") {
				t.Fatalf("the Agent ran with an invalid execution: %v", events)
			}
		})
	})
	t.Run("opening failure unwinds owned scopes", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.openErr = errOpen
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.waitOperationClosed()
			if err := e.converge(); err != nil && !errors.Is(err, errOpen) {
				t.Fatalf("Wait: %v", err)
			}
			events := strings.Join(e.events.all(), ",")
			for _, want := range []string{"op-open:op-1", "ag-open", "open-fail", "ag-close", "op-close"} {
				if !strings.Contains(events, want) {
					t.Fatalf("events = %v, want %q", e.events.all(), want)
				}
			}
			op, err := e.h.ReadOperation(context.Background(), session, "op-1")
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if op.State.Status != harness.OperationFailure {
				t.Fatalf("operation status = %q, want failure from the opener error", op.State.Status)
			}
		})
	})
	t.Run("cleanup error is retained without rewriting the settlement", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.cleanupErr = errExecClose
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.awaitCleanups(1)
			op, err := e.h.ReadOperation(context.Background(), session, "op-1")
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if op.State.Status != harness.OperationSuccess {
				t.Fatalf("cleanup failure rewrote the settlement: status %q", op.State.Status)
			}
			if err := e.converge(); !errors.Is(err, errExecClose) {
				t.Fatalf("Wait error = %v, want the retained cleanup failure", err)
			}
		})
	})
}
