package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Preparation fixtures: the public Open over the existing owner environment
// drives the real Runtime and its one concrete preparation over both stores
// through a keyless local model endpoint, real selected hooks, and declared
// test tools with observable behavior. HTTP scripts dispatch on actual wire
// inputs only.

var (
	errHook      = errors.New("test hook failure")
	errOpen      = errors.New("test open failure")
	errExecClose = errors.New("test execution cleanup failure")
)

var prepModelRef = model.ModelRef{Provider: "prov", Model: "m"}

// runtimeNormalize is the fixtures' required pure normalizer: it accepts
// exactly one non-null JSON object and returns its compact encoding.
func runtimeNormalize(call model.ToolCall) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, errors.New("arguments must be one non-null JSON object")
	}
	return json.Marshal(obj)
}

// prepConfigDocument points the configured provider at the fixture's keyless
// local endpoint with discovery disabled and a generous window, so ordinary
// rows never accidentally test compaction. The "zero" model keeps the
// zero-window rejection row.
func prepConfigDocument(endpoint, tag string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {
        "m": {"name": "M", "context_window": 262144, "max_output_tokens": 4096},
        "zero": {"name": "Z", "context_window": 0}
      }
    }
  },
  "plugins": {"hooks": {"tag": "` + tag + `"}
  }
}`
}

// prepPolicyConfigDocument adds one global deny rule for the declared
// file.write pair of the fixture's echo tool.
func prepPolicyConfigDocument(endpoint, tag string) string {
	return strings.TrimSuffix(prepConfigDocument(endpoint, tag), "}") + `,"permissions":{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}}`
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
  "hooky": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "arg.runtime", "arg.workspace", "op.native", "arg.operation", "ag.native", "arg.agent"]},
  "locked": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "readonly": true, "write_dir": "  /tmp/pad  "},
  "ghostly": {"system_prompt": "simple", "tools": ["echo"]},
  "shallow": {"model": "prov/zero", "system_prompt": "simple", "tools": ["echo"]}
}`

// recordHook is one declared PreparationHook capability: it retains the tool
// slice it received, records the Invocation, and answers with its configured
// mutation or error. One armed call may park until its test releases it, and
// one armed call may fail with a one-shot error.
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
	parkRelease chan struct{}
	parkArrived chan struct{}
	nextFail    error
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
	parkRelease, parkArrived := h.parkRelease, h.parkArrived
	h.parkRelease, h.parkArrived = nil, nil
	nextFail := h.nextFail
	h.nextFail = nil
	h.mu.Unlock()
	if parkRelease != nil {
		if parkArrived != nil {
			select {
			case parkArrived <- struct{}{}:
			default:
			}
		}
		select {
		case <-parkRelease:
		case <-ctx.Done():
			return harness.ExecutionCapture{}, ctx.Err()
		}
	}
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
	if nextFail != nil {
		return harness.ExecutionCapture{}, nextFail
	}
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

// failNext arms exactly the next Prepare call to fail with err after
// recording it; every later call answers normally again.
func (h *recordHook) failNext(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextFail = err
}

// parkNext arms exactly the next Prepare call to park after recording its
// input, until the release channel closes; the arrived channel signals the
// parked call once.
func (h *recordHook) parkNext(release chan struct{}, arrived chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.parkRelease, h.parkArrived = release, arrived
}

// blockUntilCanceled parks the next Prepare call until its call context is
// canceled, after signaling the buffered start channel.
func (h *recordHook) blockUntilCanceled(started chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.block = true
	h.blockSignal = started
}

// recordArgumentHook is one declared ToolArgumentsHook capability: it records
// every received call's argument bytes and Invocation revision and answers
// with its fixed replacement.
type recordArgumentHook struct {
	replace json.RawMessage

	mu   sync.Mutex
	seen []string
	revs []string
}

func (h *recordArgumentHook) BeforeTool(ctx context.Context, in Invocation, call model.ToolCall) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.seen = append(h.seen, string(call.Arguments))
	h.revs = append(h.revs, in.Revision())
	h.mu.Unlock()
	return h.replace, nil
}

func (h *recordArgumentHook) received() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.seen...), append([]string{}, h.revs...)
}

// prepTool is the fixture's declared "echo" tool: pure object normalization,
// one declared file.write pair on the fixed canonical target, and observable
// counters, so the shared boundary's normalization, denial and argument-hook
// rows read real effects through the concrete opener. The failure flag is the
// one deliberate error-outcome row: false keeps the ordinary success result,
// true settles the executed call with a real error result.
type prepTool struct {
	mu         sync.Mutex
	normalized int
	executed   int
	input      []string
	fail       bool
}

func (t *prepTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	t.mu.Lock()
	t.normalized++
	t.mu.Unlock()
	return runtimeNormalize(call)
}

func (t *prepTool) Prepare(_ context.Context, _ ToolContext, call model.ToolCall) harness.PreparedTool {
	t.mu.Lock()
	t.input = append(t.input, string(call.Arguments))
	fail := t.fail
	t.mu.Unlock()
	return harness.PreparedTool{
		Permissions:        []harness.PermissionRequest{{Permission: "file.write", Target: "/w/a.txt"}},
		CanonicalWorkspace: "/w",
		Execute: func(context.Context) harness.ToolOutcome {
			t.mu.Lock()
			t.executed++
			t.mu.Unlock()
			if fail {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "echo failed"}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
		},
	}
}

func (t *prepTool) counters() (int, int, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.normalized, t.executed, append([]string(nil), t.input...)
}

// availableToolDescription returns one pure describe function for a
// declared, always-available tool with a minimal valid definition.
func availableToolDescription(id string) func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	return func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{Definition: model.ToolDefinition{Name: id, Parameters: json.RawMessage(`{}`)}, Available: true}, nil
	}
}

// textTurnEvents returns the SSE events of one completed text turn.
func textTurnEvents(text string) []string {
	return []string{
		`{"choices":[{"delta":{"role":"assistant","content":"` + text + `"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
}

// toolCallTurnEvents returns the SSE events of one completed tool-call turn.
func toolCallTurnEvents(id, name, args string) []string {
	return []string{
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + strconv.Quote(args) + `}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// prepEnv hosts the preparation oracles on one real owner: the existing
// ownerEnv's isolated HOME/paths/store/server, the fixture's declared hooks,
// tools and four-scope factories supplied to the public Open, and the
// constructed Runtime's actual Harness, Reload and Close. Every preparation,
// capture and admission observation runs through that owner; no second
// composition, scope registry or configuration service is assembled here.
type prepEnv struct {
	*ownerEnv
	r         *Runtime
	store     harness.Storage
	workspace string
	echo      *prepTool
	hooks     [2]*recordHook
	argHooks  [4]*recordArgumentHook // runtime, workspace, operation, agent scopes

	hookOpens, capOpens, opOpens, agOpens atomic.Int64

	// failAgentFactory arms the Agent-scope factory to fail every
	// construction, and agentCloseErr makes the Agent scope's Instance.Close
	// return the armed error: the concrete opening-failure and
	// cleanup-error-retention rows. Both arm the per-admission Agent scope,
	// so a test may set them after construction.
	failAgentFactory bool
	agentCloseErr    error

	// agGateArrived and agGateRelease arm the Agent-scope factory to park its
	// opening: the factory signals arrival, then waits for release with no
	// cancellation exit, immediately before returning its valid Instance. A
	// test sets both after newPrepEnv and before its single Submit.
	agGateArrived chan struct{}
	agGateRelease chan struct{}
}

func newPrepEnv(t *testing.T, store harness.Storage) *prepEnv {
	t.Helper()
	e := &prepEnv{ownerEnv: newOwnerEnv(t), store: store, echo: &prepTool{}}
	e.workspace = e.dataDir
	writeServiceFile(t, e.configPath, prepConfigDocument(e.server.URL, "A"))
	writeServiceFile(t, agents.PathForConfig(e.configPath), prepAgentsDocument)
	e.hooks[0] = &recordHook{name: "hook.first", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
		c.SystemPrompt += "|first"
		return c
	}}
	e.hooks[1] = &recordHook{name: "hook.second", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
		c.SystemPrompt += "|second"
		return c
	}}
	for i, name := range []string{"arg.runtime", "arg.workspace", "arg.operation", "arg.agent"} {
		e.argHooks[i] = &recordArgumentHook{replace: json.RawMessage(`{"repaired":"` + name + `"}`)}
	}
	r, err := Open(context.Background(), Options{
		DataDir:    e.dataDir,
		ConfigPath: e.configPath,
		Plugins:    append(e.plugins(), e.storagePlugin(store)),
	})
	testassert.NoError(t, err)
	e.r = r
	t.Cleanup(func() { _ = e.converge() })
	return e
}

func (e *prepEnv) plugins() []Plugin {
	return []Plugin{
		{
			ID:       "hooks",
			Scope:    ScopeRuntime,
			Provides: []CapabilitySpec{Spec[PreparationHook]("hook.first"), Spec[PreparationHook]("hook.second"), Spec[ToolArgumentsHook]("arg.runtime"), ToolSpec("echo", availableToolDescription("echo")), ToolSpec("read", availableToolDescription("read"))},
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
				return Instance{Values: map[string]any{"hook.first": e.hooks[0], "hook.second": e.hooks[1], "arg.runtime": e.argHooks[0], "echo": e.echo, "read": noopTool{}}}, nil
			},
		},
		{
			ID:       "caps",
			Scope:    ScopeWorkspace,
			Provides: []CapabilitySpec{Spec[greeter]("cap.shared"), Spec[greeter]("cap.other"), Spec[ToolArgumentsHook]("arg.workspace")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				e.capOpens.Add(1)
				return Instance{Values: map[string]any{
					"cap.shared":    loudGreeter{word: "shared"},
					"cap.other":     loudGreeter{word: "other"},
					"arg.workspace": e.argHooks[1],
				}}, nil
			},
		},
		{
			ID:       "ops",
			Scope:    ScopeOperation,
			Provides: []CapabilitySpec{Spec[greeter]("op.native"), Spec[ToolArgumentsHook]("arg.operation")},
			Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
				e.opOpens.Add(1)
				e.events.add("op-open:" + info.OperationID)
				return Instance{
					Values: map[string]any{"op.native": loudGreeter{word: "op"}, "arg.operation": e.argHooks[2]},
					Close: func() error {
						e.events.add("op-close")
						return nil
					},
				}, nil
			},
		},
		{
			ID:       "ags",
			Scope:    ScopeAgent,
			Provides: []CapabilitySpec{Spec[greeter]("ag.native"), Spec[ToolArgumentsHook]("arg.agent")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				if e.failAgentFactory {
					return Instance{}, errOpen
				}
				e.agOpens.Add(1)
				e.events.add("ag-open")
				if e.agGateArrived != nil {
					select {
					case e.agGateArrived <- struct{}{}:
					default:
					}
					<-e.agGateRelease
				}
				return Instance{
					Values: map[string]any{"ag.native": loudGreeter{word: "ag"}, "arg.agent": e.argHooks[3]},
					Close: func() error {
						e.events.add("ag-close")
						return e.agentCloseErr
					},
				}, nil
			},
		},
	}
}

// --- fixture control helpers ---

// session creates one root Session of the given agent type through the
// Runtime's admitted-call gate.
func (e *prepEnv) session(agentType string) string {
	e.t.Helper()
	rec, err := e.r.createSession(context.Background(), e.workspace, agentType)
	if err != nil {
		e.t.Fatalf("createSession(%q): %v", agentType, err)
	}
	return rec.Identity.SessionID
}

// submit runs one public Submit through the Runtime's actual Harness,
// returning the reported result even on error exactly as the Harness does.
func (e *prepEnv) submit(ctx context.Context, sessionID, operationID, text string, mode harness.MessageMode) (harness.SubmitResult, error) {
	var res harness.SubmitResult
	err := e.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		res, err = h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        mode,
		})
		return err
	})
	return res, err
}

// readOperation, snapshotSession and fork run their public Harness
// operations through the Runtime's actual Harness.
func (e *prepEnv) readOperation(ctx context.Context, sessionID, operationID string) (harness.OperationRecord, error) {
	var rec harness.OperationRecord
	err := e.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.ReadOperation(ctx, sessionID, operationID)
		return err
	})
	return rec, err
}

func (e *prepEnv) sessionView(ctx context.Context, sessionID string) (harness.SessionReadView, error) {
	var view harness.SessionReadView
	err := e.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		view, err = h.ReadSessionView(ctx, sessionID)
		return err
	})
	return view, err
}

func (e *prepEnv) fork(ctx context.Context, req harness.ForkRequest) (harness.ForkResult, error) {
	var res harness.ForkResult
	err := e.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		res, err = h.Fork(ctx, req)
		return err
	})
	return res, err
}

// reload publishes the next configuration revision through the Runtime's
// actual Reload.
func (e *prepEnv) reload() (string, error) {
	return e.r.Reload(context.Background())
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

// awaitTerminal polls one Operation until it reaches the wanted terminal
// state.
func (e *prepEnv) awaitTerminal(sessionID, operationID string, want harness.OperationState) harness.OperationRecord {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := e.readOperation(context.Background(), sessionID, operationID)
		if err == nil && rec.State.Status == want {
			return rec
		}
		if time.Now().After(deadline) {
			var detail string
			if rec.State.Terminal != nil {
				detail = rec.State.Terminal.Detail
			}
			e.t.Fatalf("operation %q never settled to %v (err %v, status %v, detail %q)", operationID, want, err, rec.State.Status, detail)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitEvent polls the ordered event log until every named event has run.
func (e *prepEnv) awaitEvent(want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := e.events.all()
		complete := true
		for _, name := range want {
			if !slices.Contains(events, name) {
				complete = false
			}
		}
		if complete {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("events %v never reached %v", events, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// converge joins the owner's one shared shutdown through its actual Close.
func (e *prepEnv) converge() error {
	e.t.Helper()
	return e.r.Close(context.Background())
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
		testassert.NoError(t, err)
		defer store.Close()
		run(t, store)
	})
}

// eachPrepStoreOnce runs one fixture against the memory store alone: the
// scenarios that perform no Session admission, recovery, durable, or mounted
// work have no store axis to multiply.
func eachPrepStoreOnce(t *testing.T, run func(t *testing.T, store harness.Storage)) {
	t.Helper()
	run(t, storage.NewMemory())
}

// TestPreparationHappyPathThroughHarness drives one real admission end to end
// on both stores: the selected hooks run in order and replace only the
// prompt, the committed capture matches the HTTP request's system prompt and
// tool advertisement with the hook-recorded revision, and the Operation and
// Agent scopes are constructed per execution and disposed in reverse order.
func TestPreparationHappyPathThroughHarness(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("wide")
		res := e.admit(session, "op-1", "hello")

		got := res.Operation.Admission.Execution
		testassert.Require(t, slices.Equal(got.Capabilities, []string{"hook.first", "cap.shared", "hook.second"}) && slices.Equal(toolNames(got.Tools), []string{"echo", "read"}) && got.Model == prepModelRef && got.ConfigurationRevision == "1" && strings.HasSuffix(got.SystemPrompt, "|first|second"), "committed capture = %+v, want the post-hook selection capture", got)
		wantCalls := []hookCall{{revision: "1", tag: "A"}}
		for _, hook := range e.hooks {
			{
				calls := hook.calls()
				testassert.Require(t, slices.Equal(calls, wantCalls), "hook %s calls = %v, want the captured revision %v", hook.name, calls, wantCalls)
			}
		}

		e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		e.awaitEvent("ag-close", "op-close")

		// The wire request carried the committed capture: the composed system
		// prompt and the advertised tool set.
		testassert.Equal(t, e.server.requests(), 1)
		doc := decodeWireChatBody(t, e.server.bodyAt(0))
		if doc.Model != "m" || !slices.Equal(wireToolNames(doc), []string{"echo", "read"}) {
			t.Fatalf("wire request = model %q tools %v, want the captured model and advertised tools", doc.Model, wireToolNames(doc))
		}
		testassert.Require(t, len(doc.Messages) != 0 && doc.Messages[0].Role == "system", "wire messages = %+v, want the leading system prompt", doc.Messages)
		var systemText string
		{
			err := json.Unmarshal(doc.Messages[0].Content, &systemText)
			testassert.Require(t, err == nil && systemText == got.SystemPrompt, "wire system prompt = %q (err %v), want the committed capture's %q", systemText, err, got.SystemPrompt)
		}

		// Hooks ran in selected order inside the preparation guard; the owner's
		// storage plugin opened first at public Open; reverse disposal closed
		// the Agent scope before the Operation scope.
		wantEvents := []string{
			"open:core",
			"hook:hook.first", "hook:hook.second",
			"op-open:op-1", "ag-open",
			"ag-close", "op-close",
		}
		{
			events := e.events.all()
			testassert.Require(t, slices.Equal(events, wantEvents), "events = %v, want %v", events, wantEvents)
		}
		testassert.Equal(t, e.hookOpens.Load(), 1)
		testassert.Equal(t, e.opOpens.Load(), 1)
		testassert.NoError(t, e.converge())
	})
}

// wireToolNames returns the advertised function names of one wire request.
func wireToolNames(doc wireChatBody) []string {
	out := make([]string, 0, len(doc.Tools))
	for _, tool := range doc.Tools {
		out = append(out, tool.Function.Name)
	}
	return out
}

// toolNames returns one tool list's names in order.
func toolNames(tools []model.ToolDefinition) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// TestPreparationReloadDoesNotRecaptureInFlightPreparation parks a real
// selected PreparationHook after its capture, publishes revision 2, releases
// it, and verifies the in-flight admission commits revision 1 with the old
// constraints while the next capture uses revision 2 — for normal, drained
// and Fork admission alike, on both stores.
func TestPreparationReloadDoesNotRecaptureInFlightPreparation(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		mode harness.MessageMode
		want harness.SubmitDisposition
	}{
		{name: "normal", kind: "normal"},
		{name: "drained queued delivery", kind: "drained", mode: harness.MessageModeQueued, want: harness.DispositionQueued},
		{name: "drained regular delivery", kind: "drained", mode: harness.MessageModeRegular, want: harness.DispositionSteering},
		{name: "fork", kind: "fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newPrepEnv(t, store)
				session := e.session("dual")

				release := make(chan struct{})
				arrived := make(chan struct{}, 1)
				heldDone := make(chan struct{})
				var mu sync.Mutex
				var submitErr error
				var forked harness.SessionRecord

				switch tc.kind {
				case "drained":
					hold := make(chan struct{})
					e.server.setHold(hold)
					e.admit(session, "op-1", "one")
					<-e.server.arrived
					{
						res, err := e.submit(context.Background(), session, "op-2", "two", tc.mode)
						testassert.Require(t, err == nil && res.Disposition == tc.want, "buffered submit = %+v err %v, want disposition %s", res, err, tc.want)
					}
					e.hooks[0].parkNext(release, arrived)
					close(hold) // op-1 converges; the post-terminal drain reaches the parked hook
				case "normal":
					e.admit(session, "op-1", "one")
					e.awaitTerminal(session, "op-1", harness.OperationSuccess)
					e.awaitEvent("op-close")
					e.hooks[0].parkNext(release, arrived)
					go func() {
						_, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeRegular)
						mu.Lock()
						submitErr = err
						mu.Unlock()
						close(heldDone)
					}()
				case "fork":
					e.admit(session, "op-1", "one")
					e.awaitTerminal(session, "op-1", harness.OperationSuccess)
					e.awaitEvent("op-close")
					e.hooks[0].parkNext(release, arrived)
					boundary := e.inputEntry(session, "op-1")
					go func() {
						res, err := e.fork(context.Background(), harness.ForkRequest{
							SourceSessionID: session,
							BoundaryEntryID: boundary,
							OperationID:     "fork-op",
							Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
						})
						mu.Lock()
						if err == nil {
							forked = res.Session
						}
						submitErr = err
						mu.Unlock()
						close(heldDone)
					}()
				}
				<-arrived
				// The reload varies the selected definition's permission
				// constraints, so the revision-2 capture differs from the
				// revision-1 one on both members.
				reloadAgents := strings.Replace(prepAgentsDocument,
					`"dual": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "cap.shared"]}`,
					`"dual": {"model": "prov/m", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.first", "cap.shared"], "readonly": true, "write_dir": "/tmp/reload"}`, 1)
				writeServiceFile(t, agents.PathForConfig(e.configPath), reloadAgents)
				{
					_, err := e.reload()
					testassert.NoError(t, err)
				}
				close(release)
				if tc.kind != "drained" {
					<-heldDone // the held admission's caller finished its submit
				}

				mu.Lock()
				err := submitErr
				mu.Unlock()
				testassert.NoError(t, err)

				// The in-flight admission committed revision 1 and the
				// captured constraints.
				var inFlight harness.OperationRecord
				if tc.kind == "fork" {
					mu.Lock()
					destination := forked.Identity.SessionID
					mu.Unlock()
					inFlight = e.awaitTerminal(destination, "fork-op", harness.OperationSuccess)
				} else {
					inFlight = e.awaitTerminal(session, "op-2", harness.OperationSuccess)
				}
				testassert.Require(t, inFlight.Admission.Execution.ConfigurationRevision == "1" && !inFlight.Admission.Execution.Readonly && inFlight.Admission.Execution.WriteDir == "", "got %v, want %v; reloaded constraints leaked into the committed capture: readonly=%v write_dir=%q, want the captured false \"\"", inFlight.Admission.Execution.ConfigurationRevision, "1", inFlight.Admission.Execution.Readonly, inFlight.Admission.Execution.WriteDir)

				// The next capture uses revision 2 and the reloaded
				// constraints.
				e.admitOrBuffer(session, "op-next", "next")
				next := e.awaitTerminal(session, "op-next", harness.OperationSuccess)
				testassert.Require(t, next.Admission.Execution.ConfigurationRevision == "2" && next.Admission.Execution.Readonly && next.Admission.Execution.WriteDir == "/tmp/reload", "got %v, want %v; next admission constraints = readonly %v write_dir %q, want the reloaded true \"/tmp/reload\"", next.Admission.Execution.ConfigurationRevision, "2", next.Admission.Execution.Readonly, next.Admission.Execution.WriteDir)
				testassert.NoError(t, e.converge())

				if tc.kind == "drained" {
					// Buffered messages captured only on delivery, and the
					// successor preparation started only after the
					// predecessor's scope closed: close-before-successor.
					events := e.events.all()
					cleanup, secondHook, seen := -1, -1, 0
					for i, event := range events {
						if event == "op-close" && cleanup < 0 {
							cleanup = i
						}
						if event == "hook:hook.first" {
							seen++
							if seen == 2 {
								secondHook = i
							}
						}
					}
					testassert.Require(t, cleanup >= 0 && secondHook >= 0 && cleanup <= secondHook, "events = %v, want the first scope close before the successor hook", events)
				}
			})
		})
	}
}

// TestPreparationDeliveryIdentity proves on both stores that existing IDs skip
// preparation and buffered messages capture only on delivery: the counts are
// a real selected preparation hook, the HTTP requests and the short-scope
// factory events.
func TestPreparationDeliveryIdentity(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		session := e.session("dual")
		hold := make(chan struct{})
		e.server.setHold(hold)
		e.admit(session, "op-1", "one")
		<-e.server.arrived

		{
			res, err := e.submit(context.Background(), session, "op-1", "again", harness.MessageModeRegular)
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionExisting, "reused id = %+v err %v, want existing without preparation", res, err)
		}
		{
			res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued)
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionQueued, "queued = %+v err %v, want queued", res, err)
		}
		testassert.Equal(t, len(e.hooks[0].calls()), 1)
		testassert.Equal(t, e.server.requests(), 1)
		testassert.Equal(t, e.opOpens.Load()+e.agOpens.Load(), 2)
		close(hold)
		e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		e.awaitTerminal(session, "op-2", harness.OperationSuccess)
		testassert.Equal(t, len(e.hooks[0].calls()), 2)
		testassert.Equal(t, e.server.requests(), 2)
		testassert.Equal(t, e.opOpens.Load()+e.agOpens.Load(), 4)
		testassert.NoError(t, e.converge())
	})
}

// TestPreparationRejectsAdmissionAtTheBoundary proves on both stores that
// every pre-admission rejection reports its class at admission through the
// concrete Open path, prepares or opens nothing, and admits no Operation.
func TestPreparationRejectsAdmissionAtTheBoundary(t *testing.T) {
	cases := []struct {
		name      string
		agentType string
	}{
		{"unknown agent type", "ghost"},
		{"no model configured", "ghostly"},
		{"selected model without a positive context window", "shallow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newPrepEnv(t, store)
				t.Cleanup(func() { e.converge() })
				session := e.session(tc.agentType)
				res, err := e.submit(context.Background(), session, "op-x", "x", harness.MessageModeRegular)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit error = %v, want harness.ErrInvalid", err)
				testassert.Require(t, res.Disposition != harness.DispositionAdmitted, "rejected preparation admitted an Operation")
				testassert.Equal(t, e.opOpens.Load()+e.agOpens.Load(), 0)
				{
					_, err := e.readOperation(context.Background(), session, "op-x")
					testassert.Require(t, errors.Is(err, harness.ErrNotFound), "rejected admission left an Operation: %v", err)
				}
				testassert.Equal(t, len(e.hooks[0].calls())+len(e.hooks[1].calls()), 0)
				testassert.Equal(t, e.server.requests(), 0)
				testassert.NoError(t, e.converge())
			})
		})
	}
}

// TestPreparationPreparationHookErrorOnDrainedAndForkDelivery proves on both
// stores that the queued-drain and Fork admission paths surface one
// preparation-hook error with no partial consequence: the rejected delivery
// leaves no Operation or input entry while the next delivery still admits,
// and the failed Fork publishes no destination Session or prefix copy and
// leaves the source unchanged.
func TestPreparationPreparationHookErrorOnDrainedAndForkDelivery(t *testing.T) {
	t.Run("rejected queued delivery adds nothing and the next still admits", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			t.Cleanup(func() { e.converge() })
			session := e.session("dual")
			hold := make(chan struct{})
			e.server.setHold(hold)
			e.admit(session, "op-1", "one")
			<-e.server.arrived
			e.hooks[0].failNext(errHook)
			{
				res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued)
				testassert.Require(t, err == nil && res.Disposition == harness.DispositionQueued, "queued submit = %+v err %v, want queued", res, err)
			}
			{
				res, err := e.submit(context.Background(), session, "op-3", "three", harness.MessageModeQueued)
				testassert.Require(t, err == nil && res.Disposition == harness.DispositionQueued, "queued submit = %+v err %v, want queued", res, err)
			}
			close(hold)
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			e.awaitTerminal(session, "op-3", harness.OperationSuccess)
			{
				_, err := e.readOperation(context.Background(), session, "op-2")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "rejected delivery left an Operation: %v", err)
			}
			entries, err := store.ReadEntries(context.Background(), session, 0)
			testassert.NoError(t, err)
			for _, entry := range entries {
				testassert.Require(t, entry.OperationID != "op-2", "rejected delivery left entry %s (%s) behind", entry.ID, entry.Kind)
			}
			testassert.Equal(t, countEvents(e.events.all(), "op-open:op-2"), 0)
			testassert.NoError(t, e.converge())
		})
	})
	t.Run("failed Fork publishes no destination and leaves the source unchanged", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			t.Cleanup(func() { e.converge() })
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			boundary := e.inputEntry(session, "op-1")
			sessionsBefore, err := store.ListSessionIDs(context.Background())
			testassert.NoError(t, err)
			entriesBefore, err := store.ReadEntries(context.Background(), session, 0)
			testassert.NoError(t, err)
			sourceBeforeView, err := e.sessionView(context.Background(), session)
			testassert.NoError(t, err)
			e.hooks[0].failNext(errHook)
			res, err := e.fork(context.Background(), harness.ForkRequest{
				SourceSessionID: session,
				BoundaryEntryID: boundary,
				OperationID:     "fork-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, errHook), "fork with a failing hook = %+v err %v, want the hook's own error", res, err)
			sessionsAfter, err := store.ListSessionIDs(context.Background())
			testassert.NoError(t, err)
			testassert.Require(t, slices.Equal(sessionsAfter, sessionsBefore), "session listing after the failed fork = %v, want unchanged %v", sessionsAfter, sessionsBefore)
			entriesAfter, err := store.ReadEntries(context.Background(), session, 0)
			testassert.NoError(t, err)
			testassert.Equal(t, len(entriesAfter), len(entriesBefore))
			sourceAfterView, err := e.sessionView(context.Background(), session)
			testassert.NoError(t, err)
			// The run's own retirement publications advance the local counter
			// between the two reads; the durable metadata and the register
			// totals must stay exactly as they were.
			afterHeader, beforeHeader := sourceAfterView.Header, sourceBeforeView.Header
			afterHeader.Revision.LocalRevision, beforeHeader.Revision.LocalRevision = 0, 0
			testassert.Require(t, reflect.DeepEqual(afterHeader, beforeHeader) && reflect.DeepEqual(sourceAfterView.TotalUsage, sourceBeforeView.TotalUsage), "source session after the failed fork changed: %+v vs %+v", sourceAfterView, sourceBeforeView)
			testassert.Equal(t, len(e.hooks[0].calls()), 2)
			testassert.NoError(t, e.converge())
		})
	})
}

// TestPreparationHooksAreOrderedAndValidated exercises the pure hook boundary
// through the real Harness on both stores: identity and selection expansion,
// invalid tool lists, non-owned capture handout, hook errors, cancellation
// while a hook is parked, cancellation between hooks, and a closed supplying
// scope all abort admission with no partial consequence published.
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
			{"readonly", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Readonly = !c.Readonly
				return c
			}},
			{"write_dir", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.WriteDir = "/tmp/hook-elsewhere"
				return c
			}},
		}
		for _, v := range violations {
			t.Run(v.name, func(t *testing.T) {
				eachPrepStore(t, func(t *testing.T, store harness.Storage) {
					e := newPrepEnv(t, store)
					e.hooks[0].set(nil, v.mutate)
					session := e.session("wide")
					{
						_, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular)
						testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit error = %v, want harness.ErrInvalid", err)
					}
					events := strings.Join(e.events.all(), ",")
					if !strings.Contains(events, "hook:hook.first") || strings.Contains(events, "hook:hook.second") {
						t.Fatalf("events = %v, want the failing hook to stop the rest", e.events.all())
					}
					testassert.Equal(t, e.opOpens.Load()+e.agOpens.Load(), 0)
					testassert.NoError(t, e.converge())
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
					{
						_, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular)
						testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit error = %v, want harness.ErrInvalid", err)
					}
					{
						_, err := e.readOperation(context.Background(), session, "op-1")
						testassert.Require(t, errors.Is(err, harness.ErrNotFound), "rejected hook left an Operation: %v", err)
					}
					testassert.NoError(t, e.converge())
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
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			// Each hook's input carried the previous result (fresh owned copies
			// chain), and mutating the second hook's own copy cannot
			// retroactively change the array the first hook received and
			// retained.
			{
				firstTools, firstDesc := e.hooks[0].retainedInputs()
				testassert.Require(t, firstDesc == "" && len(firstTools) != 0 && firstTools[0].Description == "hook.first", "hook.first input = %+v description %q, want the unaliased pre-hook capture it mutated", firstTools, firstDesc)
			}
			{
				_, secondDesc := e.hooks[1].retainedInputs()
				testassert.Equal(t, secondDesc, "hook.first")
			}
			// The committed capture carries the final owned result.
			{
				got := res.Operation.Admission.Execution.Tools
				testassert.Require(t, len(got) != 0 && got[0].Description == "hook.second", "committed tools = %+v, want the final owned result", got)
			}
			testassert.NoError(t, e.converge())
		})
	})
	t.Run("hook error aborts admission", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.hooks[0].set(errHook, nil)
			session := e.session("wide")
			{
				_, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular)
				testassert.Require(t, errors.Is(err, errHook), "submit error = %v, want %v", err, errHook)
			}
			{
				_, err := e.readOperation(context.Background(), session, "op-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "failed hook left an Operation: %v", err)
			}
			testassert.NoError(t, e.converge())
		})
	})
	t.Run("cancellation while a hook is parked aborts admission", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			started := make(chan struct{}, 1)
			e.hooks[0].blockUntilCanceled(started)
			session := e.session("dual")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := e.submit(ctx, session, "op-1", "x", harness.MessageModeRegular)
				done <- err
			}()
			<-started
			cancel()
			{
				err := <-done
				testassert.Require(t, errors.Is(err, context.Canceled), "canceled submit error = %v, want context.Canceled", err)
			}
			{
				_, err := e.readOperation(context.Background(), session, "op-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canceled preparation left an Operation: %v", err)
			}
			testassert.NoError(t, e.converge())
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
			{
				err := <-done
				testassert.Require(t, errors.Is(err, context.Canceled), "canceled submit error = %v, want context.Canceled", err)
			}
			if slices.Contains(e.events.all(), "hook:hook.second") {
				t.Fatalf("later hook ran after cancellation: %v", e.events.all())
			}
			{
				_, err := e.readOperation(context.Background(), session, "op-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canceled hook run left an Operation: %v", err)
			}
			_ = e.converge()
		})
	})
	t.Run("closed supplying scope rejects the next hook call", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			session := e.session("wide")
			release := make(chan struct{})
			arrived := make(chan struct{}, 1)
			e.hooks[0].parkNext(release, arrived)
			done := make(chan error, 1)
			go func() {
				_, err := e.submit(context.Background(), session, "op-1", "x", harness.MessageModeRegular)
				done <- err
			}()
			<-arrived
			testassert.NoError(t, e.r.runtimeScope.close())
			close(release)
			{
				err := <-done
				testassert.Require(t, errors.Is(err, ErrClosed), "submit after closed bindings error = %v, want ErrClosed", err)
			}
			{
				_, err := e.readOperation(context.Background(), session, "op-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "rejected hook admission left an Operation: %v", err)
			}
			testassert.Equal(t, e.opOpens.Load()+e.agOpens.Load(), 0)
			_ = e.converge()
		})
	})
}

// TestPreparationDistinctSelectionsOverReusedInstances compares two Agent
// selections over the same constructed instances on both stores: the
// long-lived plugins open once, each committed capture carries its own
// capability set and the valid unique tool list, and an Agent with no
// selection prepares cleanly without any hook.
func TestPreparationDistinctSelectionsOverReusedInstances(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		dual := e.session("dual")
		dup := e.session("dup")
		solo := e.session("solo")

		e.admit(dual, "op-1", "one")
		first := e.awaitTerminal(dual, "op-1", harness.OperationSuccess)
		e.admitOrBuffer(dup, "op-2", "two")
		second := e.awaitTerminal(dup, "op-2", harness.OperationSuccess)
		e.admitOrBuffer(solo, "op-3", "three")
		third := e.awaitTerminal(solo, "op-3", harness.OperationSuccess)

		testassert.Equal(t, first.Admission.AgentType, "dual")
		{
			got := first.Admission.Execution.Capabilities
			testassert.Require(t, slices.Equal(got, []string{"hook.first", "cap.shared"}), "dual committed capabilities = %v", got)
		}
		{
			got := toolNames(first.Admission.Execution.Tools)
			testassert.Require(t, slices.Equal(got, []string{"echo"}), "dual committed tools = %v", got)
		}
		testassert.Equal(t, second.Admission.AgentType, "dup")
		{
			got := second.Admission.Execution.Capabilities
			testassert.Require(t, slices.Equal(got, []string{"hook.second", "cap.other"}), "dup committed capabilities = %v", got)
		}
		{
			got := toolNames(second.Admission.Execution.Tools)
			testassert.Require(t, slices.Equal(got, []string{"echo", "read"}), "dup committed tools = %v, want the first-occurrence unique list", got)
		}
		{
			got := third.Admission.Execution.Capabilities
			testassert.Require(t, len(got) == 0, "solo committed capabilities = %v, want none", got)
		}
		testassert.Equal(t, e.hookOpens.Load()+e.capOpens.Load(), 2)
		// Only the selected hook ran per admission: hook.first on dual, hook.second
		// on dup, neither on solo.
		{
			calls := e.hooks[0].calls()
			testassert.Require(t, len(calls) == 1, "hook.first calls = %v, want dual only", calls)
		}
		{
			calls := e.hooks[1].calls()
			testassert.Require(t, len(calls) == 1, "hook.second calls = %v, want dup only", calls)
		}
		testassert.NoError(t, e.converge())
	})
}

// TestPreparationSelectsShortScopeCapabilitiesAtOpen proves a selected
// Operation- or Agent-scoped capability stays out of the preparation view —
// the selected preparation hook observes no short-scope construction — and
// reaches the committed capture, with one Operation and Agent factory per
// admitted execution.
func TestPreparationSelectsShortScopeCapabilitiesAtOpen(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		var hookSawOp, hookSawAg atomic.Int64
		e.hooks[0].set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			hookSawOp.Store(e.opOpens.Load())
			hookSawAg.Store(e.agOpens.Load())
			return c
		})
		session := e.session("deep")
		e.admit(session, "op-1", "one")
		rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		testassert.Equal(t, hookSawOp.Load(), 0)
		testassert.Equal(t, hookSawAg.Load(), 0)
		{
			got := rec.Admission.Execution.Capabilities
			testassert.Require(t, slices.Equal(got, []string{"hook.first", "op.native", "ag.native"}), "committed capabilities = %v, want the Agent's full selected names", got)
		}
		testassert.Equal(t, e.opOpens.Load(), 1)
		testassert.Equal(t, e.agOpens.Load(), 1)
		testassert.NoError(t, e.converge())
	})
}

// TestPreparationExecutionLifetimeAndCleanup covers the execution lifetime
// rows around the concrete opener on both stores: the execution's scopes
// close once and in reverse order, an Agent-scope factory failure is the
// real opening failure and unwinds the owned Operation scope, and an
// Instance.Close error is retained without rewriting the settled terminal.
func TestPreparationExecutionLifetimeAndCleanup(t *testing.T) {
	t.Run("execution scopes close once and in reverse order", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			e.awaitEvent("ag-close", "op-close")
			events := e.events.all()
			{
				closes := countEvents(events, "ag-close")
				testassert.Require(t, closes == 1, "Agent scope closed %d times, want once: %v", closes, events)
			}
			{
				closes := countEvents(events, "op-close")
				testassert.Require(t, closes == 1, "Operation scope closed %d times, want once: %v", closes, events)
			}
			agIdx, opIdx := slices.Index(events, "ag-close"), slices.Index(events, "op-close")
			testassert.Require(t, agIdx <= opIdx, "events = %v, want the Agent scope closed before the Operation scope", events)
			testassert.NoError(t, e.converge())
		})
	})
	t.Run("opening failure unwinds owned scopes", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.failAgentFactory = true
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			rec := e.awaitTerminal(session, "op-1", harness.OperationFailure)
			testassert.Require(t, rec.State.Terminal != nil && strings.Contains(rec.State.Terminal.Detail, errOpen.Error()), "operation state = %+v, want the failure carrying the opening error", rec.State)
			events := e.events.all()
			testassert.Require(t, slices.Contains(events, "op-open:op-1") && !slices.Contains(events, "ag-open") && !slices.Contains(events, "ag-close") && slices.Contains(events, "op-close"), "events = %v, want the Operation scope constructed before the failure; events = %v, want no Agent scope constructed through the failed opening; events = %v, want the owned Operation scope unwound", events, events, events)
			testassert.Equal(t, e.agOpens.Load(), 0)
			testassert.NoError(t, e.converge())
		})
	})
	t.Run("cleanup error is retained without rewriting the settlement", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.agentCloseErr = errExecClose
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
			{
				err := e.converge()
				testassert.Require(t, errors.Is(err, errExecClose), "Wait error = %v, want the retained cleanup failure %v", err, errExecClose)
			}
		})
	})
}

// TestPreparationInterruptBeforeAgentScopeEntrySettlesInterruption proves on
// both stores that an interrupt landing after the admission commit and before
// the Agent-scope guard admits its call settles the durable Operation as the
// interruption: the parked Agent-scope factory holds the opening open, the
// real Runtime interrupt cancels the execution context and the Agent scope
// derived from it, and the released opening reaches enter with a canceled
// caller on an unavailable derived scope.
func TestPreparationInterruptBeforeAgentScopeEntrySettlesInterruption(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		arrived := make(chan struct{}, 1)
		release := make(chan struct{})
		releaseGate := sync.OnceFunc(func() { close(release) })
		defer releaseGate() // a failing assertion never parks the opening or the owner's shutdown
		e.agGateArrived, e.agGateRelease = arrived, release
		session := e.session("deep")
		e.admit(session, "op-int", "interrupt me")
		select {
		case <-arrived: // the opening is parked inside the Agent-scope factory
		case <-time.After(10 * time.Second):
			t.Fatal("the Agent-scope opening never parked")
		}
		testassert.NoError(t, e.r.interruptSession(context.Background(), session))
		releaseGate()
		rec := e.awaitTerminal(session, "op-int", harness.OperationInterruption)
		if rec.State.Terminal != nil && strings.Contains(rec.State.Terminal.Detail, ErrClosed.Error()) {
			t.Fatalf("terminal detail = %q, want the interruption identity, not the closed-scope failure", rec.State.Terminal.Detail)
		}
		e.awaitEvent("ag-close", "op-close") // the parked opening still disposed both scopes
		testassert.NoError(t, e.converge())
	})
}

// TestPreparationCapturesPermissionConstraints proves the durable permission
// capability capture through the real Harness: the Runtime projection trims
// write_dir once and the admitted capture carries the one definition's
// readonly and trimmed write_dir values through commit. The reload axis on
// these constraint members is proven by the reload-ordering suite.
func TestPreparationCapturesPermissionConstraints(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		t.Cleanup(func() { e.converge() })
		session := e.session("locked")
		e.admit(session, "op-1", "one")
		rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		testassert.Require(t, rec.Admission.Execution.Readonly && rec.Admission.Execution.WriteDir == "/tmp/pad", "committed constraints = %v %q, want the definition's readonly true and once-trimmed %q", rec.Admission.Execution.Readonly, rec.Admission.Execution.WriteDir, "/tmp/pad")
	})
}

// TestPreparationForwardsCapturedPolicyAndNormalizer proves the opened
// execution's permission capture and required normalizer reach the shared
// Harness boundary through the concrete opener on both stores: the
// advertised call's normalization commits at the assistant producer and runs
// exactly once, the configured policy denies the declared file.write pair so
// the call settles the fixed denial without starting the concrete effect, the
// denial is visible in the next model projection on the wire, and the
// Operation still reaches terminal success.
func TestPreparationForwardsCapturedPolicyAndNormalizer(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		t.Cleanup(func() { e.converge() })
		writeServiceFile(t, e.configPath, prepPolicyConfigDocument(e.server.URL, "A"))
		{
			_, err := e.reload()
			testassert.NoError(t, err)
		}
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return nil // the default completed text turn
			}
			return toolCallTurnEvents("call-1", "echo", ` {"x": 1} `)
		})
		session := e.session("solo")
		e.admit(session, "op-1", "one")
		{
			rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			testassert.Require(t, rec.State.Status == harness.OperationSuccess, "operation = %+v, want terminal success after the denial settled its call", rec.State)
		}
		normalized, executed, _ := e.echo.counters()
		testassert.Equal(t, executed, 0)
		testassert.Equal(t, normalized, 1)
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		var denial bool
		published := 0
		for _, entry := range entries {
			if entry.Kind != harness.EntryAssistant && entry.Kind != harness.EntryToolResult {
				continue
			}
			var obj map[string]json.RawMessage
			{
				err := json.Unmarshal(entry.Payload, &obj)
				testassert.Require(t, err == nil, "decode %s payload: %v", entry.ID, err)
			}
			switch entry.Kind {
			case harness.EntryAssistant:
				var calls []struct {
					ID                  string          `json:"id"`
					NormalizedArguments json.RawMessage `json:"normalized_arguments"`
				}
				testassert.NoError(t, json.Unmarshal(obj["tool_calls"], &calls))
				if len(calls) == 0 {
					continue // the continuation turn's text-only assistant
				}
				published++
				testassert.Require(t, len(calls) == 1 && calls[0].ID == "call-1", "assistant tool calls = %s, want the one published call", obj["tool_calls"])
				testassert.Require(t, string(calls[0].NormalizedArguments) == `{"x":1}`, "committed normalized_arguments = %s, want the producer's compacted object", calls[0].NormalizedArguments)
			case harness.EntryToolResult:
				testassert.Require(t, string(obj["status"]) == `"denied"` && string(obj["content"]) == `"Permission denied."`, "tool result = %s, want the fixed denial", entry.Payload)
				{
					_, present := obj["metadata"]
					testassert.Require(t, !present, "denied result carries a metadata member")
				}
				denial = true
			}
		}
		testassert.Require(t, published == 1 && denial, "committed entries: %d publishing assistants and denial=%v, want 1 and true", published, denial)
		testassert.Equal(t, e.server.requests(), 2)
		var projected string
		doc := decodeWireChatBody(t, e.server.bodyAt(1))
		for _, msg := range doc.Messages {
			if msg.Role == "tool" {
				var text string
				if json.Unmarshal(msg.Content, &text) == nil {
					projected = text
				}
			}
		}
		testassert.Equal(t, projected, "Permission denied.")
	})
}

// TestPreparationForwardsToolArgumentHooksAcrossScopes proves the argument
// hook row through the real preparation, Harness and Agent on both stores:
// the selected argument hooks bind in configured order after all execution
// scopes open, from any of the four scopes, with capability IDs as the
// durable hook identities and the captured Invocation reaching every hook; a
// pure preparation hook in the same capability list never binds as an
// argument hook; the first hook receives the invalid raw arguments and
// repairs them; every replacement commits as its own durable hook_result
// effect; the final committed value reaches the concrete tool
// byte-identically; and the Operation settles success.
func TestPreparationForwardsToolArgumentHooksAcrossScopes(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		t.Cleanup(func() { e.converge() })
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return nil // the default completed text turn
			}
			return toolCallTurnEvents("call-1", "echo", ` [not json `)
		})
		session := e.session("hooky")
		e.admit(session, "op-1", "one")
		{
			rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			testassert.Require(t, rec.State.Status == harness.OperationSuccess, "operation = %+v, want terminal success after the hook chain", rec.State)
		}

		_, executed, toolInput := e.echo.counters()
		testassert.Require(t, executed == 1 && len(toolInput) == 1 && toolInput[0] == `{"repaired":"arg.agent"}`, "concrete tool ran %d times on %q, want once with the final committed replacement", executed, toolInput)

		// The committed hook evidence: one success hook_result per argument
		// hook in configured order, under the capability IDs, each repairing
		// from its received bytes.
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		type hookWire struct {
			HookID     string          `json:"hook_id"`
			ToolCallID string          `json:"tool_call_id"`
			Status     string          `json:"status"`
			Arguments  json.RawMessage `json:"arguments"`
		}
		var hookResults []hookWire
		for _, entry := range entries {
			if entry.Kind != harness.EntryHookResult {
				continue
			}
			var wire hookWire
			testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
			hookResults = append(hookResults, wire)
		}
		wantIDs := []string{"arg.runtime", "arg.workspace", "arg.operation", "arg.agent"}
		testassert.Require(t, len(hookResults) == len(wantIDs), "committed hook results = %+v, want one per argument hook", hookResults)
		for i, want := range wantIDs {
			got := hookResults[i]
			testassert.Require(t, got.HookID == want && got.ToolCallID == "call-1" && got.Status == "success", "hook result %d = %+v, want hook %q succeeding for call-1", i, got, want)
		}
		testassert.Require(t, string(hookResults[3].Arguments) == `{"repaired":"arg.agent"}`, "last committed arguments = %s, want the final hook's replacement", hookResults[3].Arguments)

		// The chain: the first hook received the invalid raw arguments, every
		// later one the preceding hook's committed replacement; the captured
		// Invocation revision reached every hook.
		for i, hook := range e.argHooks {
			seen, revs := hook.received()
			testassert.Require(t, len(seen) == 1, "hook %d ran %d times, want once", i, len(seen))
			if i > 0 && seen[0] != string(hookResults[i-1].Arguments) {
				t.Fatalf("hook %d received %q, want the preceding committed replacement %q", i, seen[0], hookResults[i-1].Arguments)
			}
			testassert.Require(t, revs[0] != "", "hook %d received an empty Invocation revision", i)
		}
		{
			first, _ := e.argHooks[0].received()
			testassert.Require(t, first[0] == ` [not json `, "first hook received %q, want the invalid raw arguments", first)
		}

		// The pure preparation hook in the same capability list ran at
		// preparation and never bound as an argument hook.
		testassert.Require(t, len(e.hooks[0].calls()) != 0, "preparation hook never ran")
		for _, result := range hookResults {
			testassert.Require(t, result.HookID != "hook.first", "preparation hook bound as an argument hook")
		}
	})
}
