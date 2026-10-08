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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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

func (e *prepEnv) snapshotSession(ctx context.Context, sessionID string) (harness.SessionSnapshot, error) {
	var snap harness.SessionSnapshot
	err := e.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		snap, err = h.SnapshotSession(ctx, sessionID)
		return err
	})
	return snap, err
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
			e.t.Fatalf("operation %q never settled to %v (err %v, state %+v)", operationID, want, err, rec.State)
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
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
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
		if !slices.Equal(got.Capabilities, []string{"hook.first", "cap.shared", "hook.second"}) ||
			!slices.Equal(toolNames(got.Tools), []string{"echo", "read"}) ||
			got.Model != prepModelRef || got.ConfigurationRevision != "1" ||
			!strings.HasSuffix(got.SystemPrompt, "|first|second") {
			t.Fatalf("committed capture = %+v, want the post-hook selection capture", got)
		}
		wantCalls := []hookCall{{revision: "1", tag: "A"}}
		for _, hook := range e.hooks {
			if calls := hook.calls(); !slices.Equal(calls, wantCalls) {
				t.Fatalf("hook %s calls = %v, want the captured revision %v", hook.name, calls, wantCalls)
			}
		}

		e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		e.awaitEvent("ag-close", "op-close")

		// The wire request carried the committed capture: the composed system
		// prompt and the advertised tool set.
		if n := e.server.requests(); n != 1 {
			t.Fatalf("HTTP requests = %d, want the one admitted request", n)
		}
		doc := decodeWireChatBody(t, e.server.bodyAt(0))
		if doc.Model != "m" || !slices.Equal(wireToolNames(doc), []string{"echo", "read"}) {
			t.Fatalf("wire request = model %q tools %v, want the captured model and advertised tools", doc.Model, wireToolNames(doc))
		}
		if len(doc.Messages) == 0 || doc.Messages[0].Role != "system" {
			t.Fatalf("wire messages = %+v, want the leading system prompt", doc.Messages)
		}
		var systemText string
		if err := json.Unmarshal(doc.Messages[0].Content, &systemText); err != nil || systemText != got.SystemPrompt {
			t.Fatalf("wire system prompt = %q (err %v), want the committed capture's %q", systemText, err, got.SystemPrompt)
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
		if events := e.events.all(); !slices.Equal(events, wantEvents) {
			t.Fatalf("events = %v, want %v", events, wantEvents)
		}
		if got := e.hookOpens.Load(); got != 1 {
			t.Fatalf("hook factories ran %d times, want 1", got)
		}
		if got := e.opOpens.Load(); got != 1 {
			t.Fatalf("Operation scope opened %d times, want 1", got)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
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
	for _, kind := range []string{"normal", "drained", "fork"} {
		t.Run(kind, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newPrepEnv(t, store)
				session := e.session("dual")

				release := make(chan struct{})
				arrived := make(chan struct{}, 1)
				heldDone := make(chan struct{})
				var mu sync.Mutex
				var submitErr error
				var forked harness.SessionRecord

				switch kind {
				case "drained":
					hold := make(chan struct{})
					e.server.setHold(hold)
					e.admit(session, "op-1", "one")
					<-e.server.arrived
					if res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
						t.Fatalf("queued submit = %+v err %v, want queued", res, err)
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
				if _, err := e.reload(); err != nil {
					t.Fatalf("reload publish: %v", err)
				}
				close(release)
				if kind != "drained" {
					<-heldDone // the held admission's caller finished its submit
				}

				mu.Lock()
				err := submitErr
				mu.Unlock()
				if err != nil {
					t.Fatalf("held admission failed: %v", err)
				}

				// The in-flight admission committed revision 1 and the
				// captured constraints.
				var inFlight harness.OperationRecord
				if kind == "fork" {
					mu.Lock()
					destination := forked.Identity.SessionID
					mu.Unlock()
					inFlight = e.awaitTerminal(destination, "fork-op", harness.OperationSuccess)
				} else {
					inFlight = e.awaitTerminal(session, "op-2", harness.OperationSuccess)
				}
				if got := inFlight.Admission.Execution.ConfigurationRevision; got != "1" {
					t.Fatalf("overlapping-reload admission committed revision %q, want the captured 1", got)
				}
				if inFlight.Admission.Execution.Readonly || inFlight.Admission.Execution.WriteDir != "" {
					t.Fatalf("reloaded constraints leaked into the committed capture: readonly=%v write_dir=%q, want the captured false \"\"",
						inFlight.Admission.Execution.Readonly, inFlight.Admission.Execution.WriteDir)
				}

				// The next capture uses revision 2 and the reloaded
				// constraints.
				e.admitOrBuffer(session, "op-next", "next")
				next := e.awaitTerminal(session, "op-next", harness.OperationSuccess)
				if got := next.Admission.Execution.ConfigurationRevision; got != "2" {
					t.Fatalf("next admission revision = %q, want 2", got)
				}
				if !next.Admission.Execution.Readonly || next.Admission.Execution.WriteDir != "/tmp/reload" {
					t.Fatalf("next admission constraints = readonly %v write_dir %q, want the reloaded true \"/tmp/reload\"",
						next.Admission.Execution.Readonly, next.Admission.Execution.WriteDir)
				}
				if err := e.converge(); err != nil {
					t.Fatalf("Wait: %v", err)
				}

				if kind == "drained" {
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
					if cleanup < 0 || secondHook < 0 || cleanup > secondHook {
						t.Fatalf("events = %v, want the first scope close before the successor hook", events)
					}
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

		if res, err := e.submit(context.Background(), session, "op-1", "again", harness.MessageModeRegular); err != nil || res.Disposition != harness.DispositionExisting {
			t.Fatalf("reused id = %+v err %v, want existing without preparation", res, err)
		}
		if res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
			t.Fatalf("queued = %+v err %v, want queued", res, err)
		}
		if calls := len(e.hooks[0].calls()); calls != 1 {
			t.Fatalf("preparation hook calls = %d, want only the admitted execution prepared so far", calls)
		}
		if got := e.server.requests(); got != 1 {
			t.Fatalf("HTTP requests = %d, want only the admitted execution's request so far", got)
		}
		if got := e.opOpens.Load() + e.agOpens.Load(); got != 2 {
			t.Fatalf("short-scope opens = %d, want the one admitted execution's pair", got)
		}
		close(hold)
		e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		e.awaitTerminal(session, "op-2", harness.OperationSuccess)
		if calls := len(e.hooks[0].calls()); calls != 2 {
			t.Fatalf("preparation hook calls = %d, want the drained delivery to capture once", calls)
		}
		if got := e.server.requests(); got != 2 {
			t.Fatalf("HTTP requests = %d, want the drained delivery to reach the model once", got)
		}
		if got := e.opOpens.Load() + e.agOpens.Load(); got != 4 {
			t.Fatalf("short-scope opens = %d, want one pair per admitted execution", got)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
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
				if !errors.Is(err, harness.ErrInvalid) {
					t.Fatalf("submit error = %v, want harness.ErrInvalid", err)
				}
				if res.Disposition == harness.DispositionAdmitted {
					t.Fatalf("rejected preparation admitted an Operation")
				}
				if got := e.opOpens.Load() + e.agOpens.Load(); got != 0 {
					t.Fatalf("short scopes opened %d times without admission", got)
				}
				if _, err := e.readOperation(context.Background(), session, "op-x"); !errors.Is(err, harness.ErrNotFound) {
					t.Fatalf("rejected admission left an Operation: %v", err)
				}
				if calls := len(e.hooks[0].calls()) + len(e.hooks[1].calls()); calls != 0 {
					t.Fatalf("rejected admission ran %d hooks", calls)
				}
				if got := e.server.requests(); got != 0 {
					t.Fatalf("rejected admission made %d HTTP requests", got)
				}
				if err := e.converge(); err != nil {
					t.Fatalf("Wait: %v", err)
				}
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
			if res, err := e.submit(context.Background(), session, "op-2", "two", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
				t.Fatalf("queued submit = %+v err %v, want queued", res, err)
			}
			if res, err := e.submit(context.Background(), session, "op-3", "three", harness.MessageModeQueued); err != nil || res.Disposition != harness.DispositionQueued {
				t.Fatalf("queued submit = %+v err %v, want queued", res, err)
			}
			close(hold)
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			e.awaitTerminal(session, "op-3", harness.OperationSuccess)
			if _, err := e.readOperation(context.Background(), session, "op-2"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("rejected delivery left an Operation: %v", err)
			}
			entries, err := store.ReadEntries(context.Background(), session, 0)
			if err != nil {
				t.Fatalf("ReadEntries: %v", err)
			}
			for _, entry := range entries {
				if entry.OperationID == "op-2" {
					t.Fatalf("rejected delivery left entry %s (%s) behind", entry.ID, entry.Kind)
				}
			}
			if opens := countEvents(e.events.all(), "op-open:op-2"); opens != 0 {
				t.Fatalf("the rejected delivery opened its execution %d times", opens)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
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
			if err != nil {
				t.Fatalf("ListSessionIDs: %v", err)
			}
			entriesBefore, err := store.ReadEntries(context.Background(), session, 0)
			if err != nil {
				t.Fatalf("ReadEntries: %v", err)
			}
			sourceBeforeSnap, err := e.snapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("SnapshotSession: %v", err)
			}
			sourceBefore := sourceBeforeSnap.Session
			e.hooks[0].failNext(errHook)
			res, err := e.fork(context.Background(), harness.ForkRequest{
				SourceSessionID: session,
				BoundaryEntryID: boundary,
				OperationID:     "fork-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			if !errors.Is(err, errHook) {
				t.Fatalf("fork with a failing hook = %+v err %v, want the hook's own error", res, err)
			}
			sessionsAfter, err := store.ListSessionIDs(context.Background())
			if err != nil {
				t.Fatalf("ListSessionIDs: %v", err)
			}
			if !slices.Equal(sessionsAfter, sessionsBefore) {
				t.Fatalf("session listing after the failed fork = %v, want unchanged %v", sessionsAfter, sessionsBefore)
			}
			entriesAfter, err := store.ReadEntries(context.Background(), session, 0)
			if err != nil {
				t.Fatalf("ReadEntries: %v", err)
			}
			if len(entriesAfter) != len(entriesBefore) {
				t.Fatalf("source entries after the failed fork = %d, want unchanged %d", len(entriesAfter), len(entriesBefore))
			}
			sourceAfterSnap, err := e.snapshotSession(context.Background(), session)
			if err != nil {
				t.Fatalf("SnapshotSession: %v", err)
			}
			if !reflect.DeepEqual(sourceAfterSnap.Session, sourceBefore) {
				t.Fatalf("source session after the failed fork changed: %+v vs %+v", sourceAfterSnap.Session, sourceBefore)
			}
			if calls := len(e.hooks[0].calls()); calls != 2 {
				t.Fatalf("hook.first calls = %v, want the source admission plus the failed fork", calls)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
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
					if _, err := e.readOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
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
			e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			// Each hook's input carried the previous result (fresh owned copies
			// chain), and mutating the second hook's own copy cannot
			// retroactively change the array the first hook received and
			// retained.
			if firstTools, firstDesc := e.hooks[0].retainedInputs(); firstDesc != "" || len(firstTools) == 0 || firstTools[0].Description != "hook.first" {
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
			if _, err := e.readOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("failed hook left an Operation: %v", err)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
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
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled submit error = %v, want context.Canceled", err)
			}
			if _, err := e.readOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
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
			if _, err := e.readOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("canceled hook run left an Operation: %v", err)
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
			if err := e.r.runtimeScope.close(); err != nil {
				t.Fatalf("runtime scope close: %v", err)
			}
			close(release)
			if err := <-done; !errors.Is(err, ErrClosed) {
				t.Fatalf("submit after closed bindings error = %v, want ErrClosed", err)
			}
			if _, err := e.readOperation(context.Background(), session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("rejected hook admission left an Operation: %v", err)
			}
			if got := e.opOpens.Load() + e.agOpens.Load(); got != 0 {
				t.Fatalf("short scope factories ran %d times after a closed binding view", got)
			}
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

		if first.Admission.AgentType != "dual" {
			t.Fatalf("first committed agent = %q, want dual", first.Admission.AgentType)
		}
		if got := first.Admission.Execution.Capabilities; !slices.Equal(got, []string{"hook.first", "cap.shared"}) {
			t.Fatalf("dual committed capabilities = %v", got)
		}
		if got := toolNames(first.Admission.Execution.Tools); !slices.Equal(got, []string{"echo"}) {
			t.Fatalf("dual committed tools = %v", got)
		}
		if second.Admission.AgentType != "dup" {
			t.Fatalf("second committed agent = %q, want dup", second.Admission.AgentType)
		}
		if got := second.Admission.Execution.Capabilities; !slices.Equal(got, []string{"hook.second", "cap.other"}) {
			t.Fatalf("dup committed capabilities = %v", got)
		}
		if got := toolNames(second.Admission.Execution.Tools); !slices.Equal(got, []string{"echo", "read"}) {
			t.Fatalf("dup committed tools = %v, want the first-occurrence unique list", got)
		}
		if got := third.Admission.Execution.Capabilities; len(got) != 0 {
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
		if got := hookSawOp.Load(); got != 0 {
			t.Fatalf("Operation factories opened %d times during the preparation hook, want none", got)
		}
		if got := hookSawAg.Load(); got != 0 {
			t.Fatalf("Agent factories opened %d times during the preparation hook, want none", got)
		}
		if got := rec.Admission.Execution.Capabilities; !slices.Equal(got, []string{"hook.first", "op.native", "ag.native"}) {
			t.Fatalf("committed capabilities = %v, want the Agent's full selected names", got)
		}
		if got := e.opOpens.Load(); got != 1 {
			t.Fatalf("Operation scope opened %d times after admission, want one", got)
		}
		if got := e.agOpens.Load(); got != 1 {
			t.Fatalf("Agent scope opened %d times after admission, want one", got)
		}
		if err := e.converge(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
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
			if closes := countEvents(events, "ag-close"); closes != 1 {
				t.Fatalf("Agent scope closed %d times, want once: %v", closes, events)
			}
			if closes := countEvents(events, "op-close"); closes != 1 {
				t.Fatalf("Operation scope closed %d times, want once: %v", closes, events)
			}
			agIdx, opIdx := slices.Index(events, "ag-close"), slices.Index(events, "op-close")
			if agIdx > opIdx {
				t.Fatalf("events = %v, want the Agent scope closed before the Operation scope", events)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
	t.Run("opening failure unwinds owned scopes", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.failAgentFactory = true
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			rec := e.awaitTerminal(session, "op-1", harness.OperationFailure)
			if rec.State.Terminal == nil || !strings.Contains(rec.State.Terminal.Detail, errOpen.Error()) {
				t.Fatalf("operation state = %+v, want the failure carrying the opening error", rec.State)
			}
			events := e.events.all()
			if !slices.Contains(events, "op-open:op-1") {
				t.Fatalf("events = %v, want the Operation scope constructed before the failure", events)
			}
			if slices.Contains(events, "ag-open") || slices.Contains(events, "ag-close") {
				t.Fatalf("events = %v, want no Agent scope constructed through the failed opening", events)
			}
			if !slices.Contains(events, "op-close") {
				t.Fatalf("events = %v, want the owned Operation scope unwound", events)
			}
			if got := e.agOpens.Load(); got != 0 {
				t.Fatalf("Agent factories opened %d times through the failed opening", got)
			}
			if err := e.converge(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	})
	t.Run("cleanup error is retained without rewriting the settlement", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newPrepEnv(t, store)
			e.agentCloseErr = errExecClose
			session := e.session("dual")
			e.admit(session, "op-1", "one")
			rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess)
			if rec.State.Status != harness.OperationSuccess {
				t.Fatalf("cleanup failure rewrote the settlement: status %q", rec.State.Status)
			}
			if err := e.converge(); !errors.Is(err, errExecClose) {
				t.Fatalf("Wait error = %v, want the retained cleanup failure %v", err, errExecClose)
			}
		})
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
		if !rec.Admission.Execution.Readonly || rec.Admission.Execution.WriteDir != "/tmp/pad" {
			t.Fatalf("committed constraints = %v %q, want the definition's readonly true and once-trimmed %q",
				rec.Admission.Execution.Readonly, rec.Admission.Execution.WriteDir, "/tmp/pad")
		}
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
		if _, err := e.reload(); err != nil {
			t.Fatalf("policy publish: %v", err)
		}
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return nil // the default completed text turn
			}
			return toolCallTurnEvents("call-1", "echo", ` {"x": 1} `)
		})
		session := e.session("solo")
		e.admit(session, "op-1", "one")
		if rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("operation = %+v, want terminal success after the denial settled its call", rec.State)
		}
		normalized, executed, _ := e.echo.counters()
		if executed != 0 {
			t.Fatalf("concrete effects started = %d, want none for the denied call", executed)
		}
		if normalized != 1 {
			t.Fatalf("normalizations = %d, want the committed value consumed without a second pass", normalized)
		}
		entries, err := store.ReadEntries(context.Background(), session, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		var denial bool
		published := 0
		for _, entry := range entries {
			if entry.Kind != harness.EntryAssistant && entry.Kind != harness.EntryToolResult {
				continue
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(entry.Payload, &obj); err != nil {
				t.Fatalf("decode %s payload: %v", entry.ID, err)
			}
			switch entry.Kind {
			case harness.EntryAssistant:
				var calls []struct {
					ID                  string          `json:"id"`
					NormalizedArguments json.RawMessage `json:"normalized_arguments"`
				}
				if err := json.Unmarshal(obj["tool_calls"], &calls); err != nil {
					t.Fatalf("assistant tool calls: %v", err)
				}
				if len(calls) == 0 {
					continue // the continuation turn's text-only assistant
				}
				published++
				if len(calls) != 1 || calls[0].ID != "call-1" {
					t.Fatalf("assistant tool calls = %s, want the one published call", obj["tool_calls"])
				}
				if string(calls[0].NormalizedArguments) != `{"x":1}` {
					t.Fatalf("committed normalized_arguments = %s, want the producer's compacted object", calls[0].NormalizedArguments)
				}
			case harness.EntryToolResult:
				if string(obj["status"]) != `"denied"` || string(obj["content"]) != `"Permission denied."` {
					t.Fatalf("tool result = %s, want the fixed denial", entry.Payload)
				}
				if _, present := obj["metadata"]; present {
					t.Fatalf("denied result carries a metadata member")
				}
				denial = true
			}
		}
		if published != 1 || !denial {
			t.Fatalf("committed entries: %d publishing assistants and denial=%v, want 1 and true", published, denial)
		}
		if got := e.server.requests(); got != 2 {
			t.Fatalf("%d model requests, want the continuation after the settled call", got)
		}
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
		if projected != "Permission denied." {
			t.Fatalf("second projection carried tool message %q, want the committed denial", projected)
		}
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
		if rec := e.awaitTerminal(session, "op-1", harness.OperationSuccess); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("operation = %+v, want terminal success after the hook chain", rec.State)
		}

		_, executed, toolInput := e.echo.counters()
		if executed != 1 || len(toolInput) != 1 || toolInput[0] != `{"repaired":"arg.agent"}` {
			t.Fatalf("concrete tool ran %d times on %q, want once with the final committed replacement", executed, toolInput)
		}

		// The committed hook evidence: one success hook_result per argument
		// hook in configured order, under the capability IDs, each repairing
		// from its received bytes.
		entries, err := store.ReadEntries(context.Background(), session, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
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
			if err := json.Unmarshal(entry.Payload, &wire); err != nil {
				t.Fatalf("decode hook result: %v", err)
			}
			hookResults = append(hookResults, wire)
		}
		wantIDs := []string{"arg.runtime", "arg.workspace", "arg.operation", "arg.agent"}
		if len(hookResults) != len(wantIDs) {
			t.Fatalf("committed hook results = %+v, want one per argument hook", hookResults)
		}
		for i, want := range wantIDs {
			got := hookResults[i]
			if got.HookID != want || got.ToolCallID != "call-1" || got.Status != "success" {
				t.Fatalf("hook result %d = %+v, want hook %q succeeding for call-1", i, got, want)
			}
		}
		if string(hookResults[3].Arguments) != `{"repaired":"arg.agent"}` {
			t.Fatalf("last committed arguments = %s, want the final hook's replacement", hookResults[3].Arguments)
		}

		// The chain: the first hook received the invalid raw arguments, every
		// later one the preceding hook's committed replacement; the captured
		// Invocation revision reached every hook.
		for i, hook := range e.argHooks {
			seen, revs := hook.received()
			if len(seen) != 1 {
				t.Fatalf("hook %d ran %d times, want once", i, len(seen))
			}
			if i > 0 && seen[0] != string(hookResults[i-1].Arguments) {
				t.Fatalf("hook %d received %q, want the preceding committed replacement %q", i, seen[0], hookResults[i-1].Arguments)
			}
			if revs[0] == "" {
				t.Fatalf("hook %d received an empty Invocation revision", i)
			}
		}
		if first, _ := e.argHooks[0].received(); first[0] != ` [not json ` {
			t.Fatalf("first hook received %q, want the invalid raw arguments", first)
		}

		// The pure preparation hook in the same capability list ran at
		// preparation and never bound as an argument hook.
		if len(e.hooks[0].calls()) == 0 {
			t.Fatalf("preparation hook never ran")
		}
		for _, result := range hookResults {
			if result.HookID == "hook.first" {
				t.Fatalf("preparation hook bound as an argument hook")
			}
		}
	})
}
