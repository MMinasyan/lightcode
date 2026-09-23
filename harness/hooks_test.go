package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// byteNormalize is the byte-preserving fixture normalizer: it accepts exactly
// one non-null JSON object and returns the caller's exact argument bytes, so
// every compaction or escaping visible downstream is attributable to the
// durable codec rather than the normalizer.
func byteNormalize(call model.ToolCall) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, errors.New("arguments must be one non-null JSON object")
	}
	return model.CloneRaw(call.Arguments), nil
}

// hookToolSpy records each prepared call's identity and argument bytes.
type hookToolSpy struct {
	mu    sync.Mutex
	calls []model.ToolCall
	plan  func(model.ToolCall) PreparedTool
}

func (s *hookToolSpy) tool(_ context.Context, call model.ToolCall) PreparedTool {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	if s.plan != nil {
		return s.plan(call)
	}
	return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
}

func (s *hookToolSpy) received() []model.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.ToolCall{}, s.calls...)
}

// newHookHarness admits one running Operation ("op-1") whose prepared
// execution is exactly the given value.
func newHookHarness(t *testing.T, exec Execution) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := emptyStore(t)
	prepared := PreparedExecution{Capture: testCapture(), Open: func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	})
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
		t.Fatalf("disposition %q, want admitted", disposition)
	}
	c, err := h.coordinatorFor(context.Background(), session.Identity.SessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return h, store, c, session.Identity.SessionID
}

// publishHookCalls publishes the given calls as pending through one model
// effect over the given execution, so the producer normalization runs through
// that execution's own normalizer.
func publishHookCalls(t *testing.T, h *Harness, c *coordinator, exec Execution, calls ...model.ToolCall) {
	t.Helper()
	exec.Model = func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(calls...), nil
	}
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, testCapture()), nil); err != nil {
		t.Fatalf("model effect: %v", err)
	}
}

// hookGraphHookResults returns the committed hook_result payloads in
// committed order.
func hookGraphHookResults(t *testing.T, store *graphStorage, sessionID string) []hookResultEntry {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	var out []hookResultEntry
	for _, entry := range graph.Entries {
		if entry.HookResult != nil {
			out = append(out, *entry.HookResult)
		}
	}
	return out
}

// TestToolArgumentHookChainSettlement proves the complete ordered hook chain:
// the first hook receives the original raw call arguments and every later one
// the preceding committed replacement bytes; each successful replacement is
// normalized exactly once and its committed consequence feeds the next hook
// and then the preparer byte-identically; call identity and tool name stay
// bound; the assistant entry keeps its original raw bytes and committed
// normalization untouched; and the committed hook_result entries are the
// durable evidence the projector never projects as conversation.
func TestToolArgumentHookChainSettlement(t *testing.T) {
	var (
		mu             sync.Mutex
		normalizations int
		received       []string // hook-received argument bytes in chain order
	)
	normalize := func(call model.ToolCall) (json.RawMessage, error) {
		mu.Lock()
		normalizations++
		mu.Unlock()
		return byteNormalize(call)
	}
	hooks := []ToolArgumentsHook{
		{ID: "cap.hook.one", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			received = append(received, string(call.Arguments))
			mu.Unlock()
			return json.RawMessage(`{ "x": "<" }`), nil
		}},
		{ID: "cap.hook.two", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			received = append(received, string(call.Arguments))
			mu.Unlock()
			return json.RawMessage(`{"y":"&>"}`), nil
		}},
	}
	spy := &hookToolSpy{}
	exec := Execution{
		Tool:          spy.tool,
		NormalizeTool: normalize,
		ToolHooks:     hooks,
	}
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)})

	te := h.toolEffect(c, testOpID, exec, testCapture())
	got, err := te(context.Background(), testToolCall("call-1"))
	if err != nil {
		t.Fatalf("tool effect: %v", err)
	}
	if got != (model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}) {
		t.Fatalf("tool result = %+v, want the prepared success", got)
	}
	// Callback counts: one normalization for the valid original at the
	// producer plus exactly one per hook replacement.
	mu.Lock()
	count, chain := normalizations, append([]string{}, received...)
	mu.Unlock()
	if count != 3 {
		t.Fatalf("normalization callback ran %d times, want one per producer replacement (3)", count)
	}
	if len(chain) != 2 || chain[0] != ` {"x": 1} ` || chain[1] != `{"x":"\u003c"}` {
		t.Fatalf("hook chain received %q, want the raw arguments then the committed replacement bytes", chain)
	}
	prepared := spy.received()
	if len(prepared) != 1 || prepared[0].ID != "call-1" || prepared[0].Name != "echo" || string(prepared[0].Arguments) != `{"y":"\u0026\u003e"}` {
		t.Fatalf("preparer received %+v, want the final committed replacement bytes under the bound identity", prepared)
	}

	// The committed evidence: one success hook_result per hook, whose stored
	// arguments are exactly the bytes the continuation consumed.
	committed := hookGraphHookResults(t, store, sessionID)
	if len(committed) != 2 {
		t.Fatalf("%d hook_result entries, want one per hook", len(committed))
	}
	if committed[0].HookID != "cap.hook.one" || committed[0].Status != hookSucceeded || string(committed[0].Arguments) != `{"x":"\u003c"}` || committed[0].Error != "" {
		t.Fatalf("first hook result = %+v, want the committed compacted replacement", committed[0])
	}
	if committed[1].HookID != "cap.hook.two" || committed[1].Status != hookSucceeded || string(committed[1].Arguments) != `{"y":"\u0026\u003e"}` {
		t.Fatalf("second hook result = %+v, want the committed escaped replacement", committed[1])
	}

	// The assistant entry never mutated: original raw bytes retained, the
	// producer's own committed normalization untouched.
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the chain: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Assistant == nil {
			continue
		}
		record := entry.Assistant.ToolCalls[0]
		if raw, derr := base64.StdEncoding.DecodeString(record.ArgumentsBase64); derr != nil || string(raw) != ` {"x": 1} ` {
			t.Fatalf("assistant raw arguments = %q err %v, want the original bytes retained", raw, derr)
		}
		if string(record.NormalizedArguments) != `{"x":1}` {
			t.Fatalf("assistant normalized_arguments = %q, want the producer's committed value unchanged", record.NormalizedArguments)
		}
	}

	// Hook results are execution evidence, not conversation messages.
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
		t.Fatalf("operation state = %+v, want a quiet running Operation with the call settled", rec.State)
	}
	messages, err := h.contextSource(c, testOpID)(context.Background())
	if err != nil {
		t.Fatalf("context source: %v", err)
	}
	for _, msg := range messages {
		for _, part := range msg.Content {
			if part.Text == `{"y":"\u0026\u003e"}` {
				t.Fatalf("hook evidence projected as conversation content")
			}
		}
	}
}

// TestToolArgumentHookInterruptionRules proves the interruption rows fixed
// for every hook: a cancellation-shaped error with a done execution context is
// interruption (during Run and between the committed intent and Run), while a
// successful normalized output is a real result even when cancellation
// arrives before its commit — interruption then appears at the next boundary.
func TestToolArgumentHookInterruptionRules(t *testing.T) {
	t.Run("cancellation-shaped error during run interrupts", func(t *testing.T) {
		exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize}
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run
			return nil, ctx.Err()
		}}
		exec.ToolHooks = []ToolArgumentsHook{hook}
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(ctx, testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got != (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}) {
			t.Fatalf("tool result = %+v, want the interrupted-before-execution result", got)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].Status != hookInterrupted || committed[0].HookID != "cap.hook.one" || committed[0].ToolCallID != "call-1" || committed[0].Error == "" || len(committed[0].Arguments) != 0 {
			t.Fatalf("hook results = %+v, want one interrupted result under the reserved identity", committed)
		}
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the interruption: %v", err)
		}
	})

	t.Run("cancellation between intent and run never runs the hook", func(t *testing.T) {
		ran := 0
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			ran++
			return json.RawMessage(`{}`), nil
		}}
		exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize, ToolHooks: []ToolArgumentsHook{hook}}
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		replaces := 0
		baselineReplaces := 0
		store.txHook = func(step string) error {
			if step == "replace_register" {
				replaces++
				if replaces == baselineReplaces+1 { // the hook intent's register replacement
					cancel() // the context dies between the committed intent and Run
				}
			}
			return nil
		}
		baselineReplaces = replaces
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(ctx, testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if ran != 0 {
			t.Fatalf("hook ran %d times after the intent, want zero", ran)
		}
		if got != (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}) {
			t.Fatalf("tool result = %+v, want the interrupted-before-execution result", got)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].Status != hookInterrupted {
			t.Fatalf("hook results = %+v, want one interrupted result under the intent's reserved identity", committed)
		}
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the interruption: %v", err)
		}
	})

	t.Run("cancellation aborting the next hook intent keeps the real result and interrupts", func(t *testing.T) {
		secondRan := 0
		exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize}
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		exec.ToolHooks = []ToolArgumentsHook{
			{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				cancel() // the execution context dies during the successful hook's Run
				return json.RawMessage(`{"first":true}`), nil
			}},
			{ID: "cap.hook.two", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				secondRan++
				return json.RawMessage(`{}`), nil
			}},
		}
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(ctx, testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got != (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}) {
			t.Fatalf("tool result = %+v, want the interrupted-before-execution result", got)
		}
		if secondRan != 0 {
			t.Fatalf("second hook ran %d times, want later starts prevented", secondRan)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].HookID != "cap.hook.one" || committed[0].Status != hookSucceeded || string(committed[0].Arguments) != `{"first":true}` {
			t.Fatalf("hook results = %+v, want the earlier real hook result preserved", committed)
		}
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the interrupted chain: %v", err)
		}
	})

	t.Run("cancellation-shaped error without a done context is a hook error", func(t *testing.T) {
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, context.Canceled // no done execution context: an ordinary hook error
		}}
		exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize, ToolHooks: []ToolArgumentsHook{hook}}
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got.Status != model.ResultError || got.Content != "context canceled" {
			t.Fatalf("tool result = %+v, want the validation error with the hook's diagnostic", got)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].Status != hookFailed || committed[0].Error != "context canceled" {
			t.Fatalf("hook results = %+v, want one error result carrying the diagnostic", committed)
		}
	})

	t.Run("real hook result commits before the next boundary observes cancellation", func(t *testing.T) {
		exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
			}}
		}, NormalizeTool: objectNormalize}
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run, before the output commits
			return json.RawMessage(`{"fixed":true}`), nil
		}}
		exec.ToolHooks = []ToolArgumentsHook{hook}
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(ctx, testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		// The successful replacement committed as a real result; the
		// interruption appears at the next boundary (the tool intent).
		if got != (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}) {
			t.Fatalf("tool result = %+v, want the interrupted-before-execution result at the next boundary", got)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].Status != hookSucceeded || string(committed[0].Arguments) != `{"fixed":true}` {
			t.Fatalf("hook results = %+v, want the committed real result", committed)
		}
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the boundary interruption: %v", err)
		}
	})
}

// TestToolArgumentHookFailureRules proves the hook-error rows fixed for every
// hook: a returned error settles the ordinary validation-error result carrying
// the bounded diagnostic; an invalid returned value and a malformed
// normalizer outcome settle the fixed internal-validation error; remaining
// hooks are skipped; and later tool calls continue.
func TestToolArgumentHookFailureRules(t *testing.T) {
	cases := []struct {
		name       string
		run        func(model.ToolCall) (json.RawMessage, error)
		normalize  func(model.ToolCall) (json.RawMessage, error)
		want       model.ToolResult
		wantStatus hookResultStatus
		wantError  string // the hook_result error member; empty asserts only a non-empty diagnostic
	}{
		{
			name:       "returned error carries the bounded diagnostic",
			run:        func(model.ToolCall) (json.RawMessage, error) { return nil, errors.New("hook broke") },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "hook broke"},
			wantStatus: hookFailed,
			wantError:  "hook broke",
		},
		{
			name:       "an empty-text error keeps valid evidence and settles the call",
			run:        func(model.ToolCall) (json.RawMessage, error) { return nil, errors.New("") },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
			wantError:  invalidToolResultContent,
		},
		{
			name:       "invalid returned value is the internal-validation error",
			run:        func(model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`[1,2]`), nil },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
		},
		{
			name:       "normalizer rejection keeps its useful diagnostic",
			run:        func(model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`{"a":1}`), nil },
			normalize:  func(model.ToolCall) (json.RawMessage, error) { return nil, errors.New("no default for a") },
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "no default for a"},
			wantStatus: hookFailed,
			wantError:  "no default for a",
		},
		{
			name:       "malformed normalizer outcome is the internal-validation error",
			run:        func(model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`{"a":1}`), nil },
			normalize:  func(model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`[1,2]`), nil },
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var later int
			hooks := []ToolArgumentsHook{
				{ID: "cap.hook.one", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
					if call.ID == "call-1" { // only the first call's chain fails; a later call runs its own chain
						return tc.run(call)
					}
					return json.RawMessage(`{}`), nil
				}},
				{ID: "cap.hook.two", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
					if call.ID == "call-1" {
						later++
					}
					return json.RawMessage(`{}`), nil
				}},
			}
			spy := &hookToolSpy{}
			normalize := tc.normalize
			normalizeFor := func(call model.ToolCall) (json.RawMessage, error) { // the case's normalizer only ever judges the failing call
				if call.ID == "call-1" {
					return normalize(call)
				}
				return objectNormalize(call)
			}
			exec := Execution{Tool: spy.tool, NormalizeTool: normalizeFor, ToolHooks: hooks}
			h, store, c, sessionID := newHookHarness(t, exec)
			publishHookCalls(t, h, c, exec, testToolCall("call-1"), testToolCall("call-2"))

			got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1"))
			if err != nil {
				t.Fatalf("tool effect: %v", err)
			}
			if got != tc.want {
				t.Fatalf("tool result = %+v, want %+v", got, tc.want)
			}
			if later != 0 {
				t.Fatalf("remaining hook ran %d times, want zero after the failure", later)
			}
			committed := hookGraphHookResults(t, store, sessionID)
			if len(committed) != 1 || committed[0].Status != tc.wantStatus || committed[0].HookID != "cap.hook.one" || len(committed[0].Arguments) != 0 || committed[0].Error == "" {
				t.Fatalf("hook results = %+v, want one %s result with a non-empty diagnostic", committed, tc.wantStatus)
			}
			if tc.wantError != "" && committed[0].Error != tc.wantError {
				t.Fatalf("hook result error = %q, want %q", committed[0].Error, tc.wantError)
			}
			// Later calls still run: call-2 executes after the failed chain.
			got, err = h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-2"))
			if err != nil {
				t.Fatalf("later tool effect: %v", err)
			}
			if got.Status != model.ResultSuccess {
				t.Fatalf("later call result = %+v, want success", got)
			}
			prepareCalls := spy.received()
			if len(prepareCalls) != 1 || prepareCalls[0].ID != "call-2" {
				t.Fatalf("preparer calls = %+v, want only the later call", prepareCalls)
			}
			if _, err := validateFixture(t, store, sessionID); err != nil {
				t.Fatalf("graph after the failure: %v", err)
			}
		})
	}
}

// TestToolArgumentHookFailedResultCommitLeavesRecovery proves a failed hook
// result transaction returns no usable consequence, starts no later effect,
// leaves the valid running intent for recovery, and that recovery writes the
// interrupted hook_result under the intent's reserved identity and settles the
// pending tools and Operation through the existing interruption helpers —
// replaying nothing.
func TestToolArgumentHookFailedResultCommitLeavesRecovery(t *testing.T) {
	var (
		mu     sync.Mutex
		runs   int
		second bool
	)
	hooks := []ToolArgumentsHook{
		{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			return json.RawMessage(`{}`), nil
		}},
		{ID: "cap.hook.two", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			second = true
			mu.Unlock()
			return json.RawMessage(`{}`), nil
		}},
	}
	exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize, ToolHooks: hooks}
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, testToolCall("call-1"))

	hookResultsFailed := false
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryHookResult && !hookResultsFailed { // only the first hook result transaction fails; recovery's own hook_result insert succeeds
			hookResultsFailed = true
			return fmt.Errorf("%w: injected hook result failure", ErrStorage)
		}
		return nil
	}
	if _, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1")); !errors.Is(err, ErrStorage) {
		t.Fatalf("tool effect = %v, want the injected storage failure", err)
	}
	mu.Lock()
	ran, ranSecond := runs, second
	mu.Unlock()
	if ran != 1 || ranSecond {
		t.Fatalf("hooks ran %d/%v, want the first only: a failed result transaction starts no later effect", ran, ranSecond)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect == nil || rec.State.ActiveEffect.Kind != EffectHook ||
		rec.State.ActiveEffect.HookID != "cap.hook.one" || rec.State.ActiveEffect.ToolCallID != "call-1" {
		t.Fatalf("operation state = %+v, want the committed running hook intent left for recovery", rec.State)
	}
	reserved := rec.State.ActiveEffect.ResultEntryID
	if len(rec.State.PendingToolCalls) != 1 || rec.State.PendingToolCalls[0].CallID != "call-1" {
		t.Fatalf("pending calls = %+v, want the first call still pending", rec.State.PendingToolCalls)
	}

	// Recovery interrupts the reserved active hook effect through the common
	// terminal helper: the interrupted hook_result under the reserved
	// identity, the interrupted tool result, and the terminal Operation.
	if err := Recover(context.Background(), store); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after recovery: %v", err)
	}
	var hookResults []hookResultEntry
	for _, entry := range graph.Entries {
		if entry.HookResult != nil {
			hookResults = append(hookResults, *entry.HookResult)
		}
	}
	if len(hookResults) != 1 || hookResults[0].EntryID != reserved || hookResults[0].HookID != "cap.hook.one" ||
		hookResults[0].ToolCallID != "call-1" || hookResults[0].Status != hookInterrupted || hookResults[0].Error != recoveryInterruptedDetail {
		t.Fatalf("recovered hook results = %+v, want one interrupted result under the reserved identity %q", hookResults, reserved)
	}
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation after recovery: %v", err)
	}
	// The live harness holds its stale coordinator view; the durable truth is
	// the recovered store's own validated graph.
	rec = graph.Operations[0]
	if rec.State.Status != OperationInterruption || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
		t.Fatalf("recovered operation = %+v, want the terminal interruption", rec.State)
	}
	// No hook or tool replays: the store's next recovery run writes nothing.
	before := snapshotHookStore(t, store, sessionID)
	if err := Recover(context.Background(), store); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	after := snapshotHookStore(t, store, sessionID)
	if before != after {
		t.Fatalf("second recovery changed the record count (%d -> %d): replay is forbidden", before, after)
	}
}

// snapshotHookStore counts one Session's committed entries and registers.
func snapshotHookStore(t *testing.T, store *graphStorage, sessionID string) int {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	registers, err := store.ReadRegisters(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read registers: %v", err)
	}
	return len(entries) + len(registers)
}

// TestToolArgumentHookDenialSettlesAfterHooks proves argument hooks may have
// settled before a final tool permission denial: the denial forbids the
// concrete tool and never rolls back the separate hook effects, while the
// allowed sibling executes with the final committed value.
func TestToolArgumentHookDenialSettlesAfterHooks(t *testing.T) {
	hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`{"repaired":true}`), nil
	}}
	buildExec := func(plan func(context.Context, model.ToolCall) PreparedTool) Execution {
		return Execution{Tool: plan, NormalizeTool: objectNormalize, ToolHooks: []ToolArgumentsHook{hook}}
	}

	t.Run("denial forbids the concrete start and keeps hook evidence", func(t *testing.T) {
		var executed int
		exec := buildExec(func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{ // an executor plan with no declarations is denied
				Execute: func(context.Context) ToolOutcome {
					executed++
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				},
			}
		})
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got != (model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: permissionDeniedToolResultContent}) {
			t.Fatalf("tool result = %+v, want the permission denial", got)
		}
		if executed != 0 {
			t.Fatalf("denied executor ran %d times, want the forbidden concrete start prevented", executed)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		if len(committed) != 1 || committed[0].Status != hookSucceeded || string(committed[0].Arguments) != `{"repaired":true}` {
			t.Fatalf("hook results = %+v, want the settled hook effect left intact", committed)
		}
	})

	t.Run("allowed sibling executes with the final committed value", func(t *testing.T) {
		spy := &hookToolSpy{}
		exec := buildExec(spy.tool)
		h, _, c, _ := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got.Status != model.ResultSuccess {
			t.Fatalf("tool result = %+v, want success", got)
		}
		prepared := spy.received()
		if len(prepared) != 1 || string(prepared[0].Arguments) != `{"repaired":true}` {
			t.Fatalf("preparer received %+v, want the final committed replacement", prepared)
		}
	})
}

// TestToolArgumentHookAdvertisementGate proves the one advertisement gate
// precedes every hook: a committed call whose tool name is outside the
// advertised set settles the unavailable-tool error and never runs a hook.
func TestToolArgumentHookAdvertisementGate(t *testing.T) {
	ran := 0
	hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}}
	exec := Execution{Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize, ToolHooks: []ToolArgumentsHook{hook}}
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, model.ToolCall{ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`)})

	got, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), model.ToolCall{ID: "call-1", Name: "ghost"})
	if err != nil {
		t.Fatalf("tool effect: %v", err)
	}
	if got != (model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: `Tool "ghost" is not available.`}) {
		t.Fatalf("tool result = %+v, want the unavailable-tool error", got)
	}
	if ran != 0 {
		t.Fatalf("hook ran %d times behind the gate, want zero", ran)
	}
	if got := hookGraphHookResults(t, store, sessionID); len(got) != 0 {
		t.Fatalf("hook results = %+v, want none", got)
	}
}

// TestToolArgumentHookInvalidOpenedExecutionRejectsHooks proves the opened
// execution requires a non-empty unique ID and a non-nil run function for
// every argument hook, closing before rejection and settling the ordinary
// failure.
func TestToolArgumentHookInvalidOpenedExecutionRejectsHooks(t *testing.T) {
	closed := 0
	for _, tc := range []struct {
		name  string
		hooks []ToolArgumentsHook
	}{
		{"empty id", []ToolArgumentsHook{{Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }}}},
		{"nil run", []ToolArgumentsHook{{ID: "cap.hook.one"}}},
		{"duplicate id", []ToolArgumentsHook{
			{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
			{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, c, sessionID, prepared, _ := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
				modelFn := func(context.Context, model.Request) (model.Stream, error) {
					return completedTurnStream(), nil
				}
				return Execution{Model: modelFn, Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, NormalizeTool: objectNormalize, ToolHooks: tc.hooks, Close: func() error {
					closed++
					return nil
				}}, nil
			})
			if err := h.execute(c, testOpID, prepared, h.ctx); err == nil {
				t.Fatalf("execute = nil, want the invalid-hook rejection")
			}
			if closed != 1 {
				t.Fatalf("close ran %d times, want exactly one before rejection", closed)
			}
			closed = 0
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if rec.State.Status != OperationFailure {
				t.Fatalf("operation status = %s, want the ordinary failure settlement", rec.State.Status)
			}
		})
	}
}

// TestToolArgumentHookIntentFailureLeavesRecovery proves the failed hook-intent
// transaction row: the hook never runs, no preparer runs, no hook evidence
// exists, and the quiet running state survives for recovery — which interrupts
// the Operation through the common terminal helper without replaying the hook
// or committing any hook result (none was ever reserved).
func TestToolArgumentHookIntentFailureLeavesRecovery(t *testing.T) {
	ran := 0
	hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}}
	spy := &hookToolSpy{}
	exec := Execution{Tool: spy.tool, NormalizeTool: objectNormalize, ToolHooks: []ToolArgumentsHook{hook}}
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, testToolCall("call-1"))

	replaces := 0
	store.txHook = func(step string) error {
		if step == "replace_register" {
			replaces++
			if replaces == 1 { // the hook intent's register replacement fails
				return fmt.Errorf("%w: injected hook intent failure", ErrStorage)
			}
		}
		return nil
	}
	if _, err := h.toolEffect(c, testOpID, exec, testCapture())(context.Background(), testToolCall("call-1")); !errors.Is(err, ErrStorage) {
		t.Fatalf("tool effect = %v, want the injected storage failure", err)
	}
	if ran != 0 {
		t.Fatalf("hook ran %d times, want zero after the failed intent", ran)
	}
	if prepared := spy.received(); len(prepared) != 0 {
		t.Fatalf("preparer ran for %v, want zero preparations", prepared)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 1 {
		t.Fatalf("operation state = %+v, want the quiet running state with the call still pending", rec.State)
	}
	if got := hookGraphHookResults(t, store, sessionID); len(got) != 0 {
		t.Fatalf("hook results = %+v, want none without a committed reservation", got)
	}

	if err := Recover(context.Background(), store); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after recovery: %v", err)
	}
	rec = graph.Operations[0]
	if rec.State.Status != OperationInterruption || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
		t.Fatalf("recovered operation = %+v, want the terminal interruption", rec.State)
	}
	if got := hookGraphHookResults(t, store, sessionID); len(got) != 0 {
		t.Fatalf("hook results after recovery = %+v, want none: no reservation, no replay", got)
	}
}
