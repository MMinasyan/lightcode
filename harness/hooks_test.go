package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	{
		_, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello"))
		testassert.Equal(t, disposition, DispositionAdmitted)
	}
	c := leaseCoordinator(t, h, session.Identity.SessionID)
	return h, store, c, session.Identity.SessionID
}

// hookFixtureExecution returns the hook fixtures' execution: its opener answers
// every Engine request with an invocation carrying the given preparation
// callback, normalizer, and selected argument hooks.
func hookFixtureExecution(toolFn func(context.Context, model.ToolCall) PreparedTool, normalize func(model.ToolCall) (json.RawMessage, error), hooks ...ToolArgumentsHook) Execution {
	return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{Tool: toolFn, NormalizeTool: normalize, ToolHooks: hooks}, nil
	}}
}

// publishHookCalls publishes the given calls as pending through one model
// effect over the given execution's opened invocation, so the producer
// normalization runs through that execution's own normalizer.
func publishHookCalls(t *testing.T, h *Harness, c *coordinator, exec Execution, calls ...model.ToolCall) {
	t.Helper()
	inv := openedConversation(t, exec)
	inv.Model = func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(calls...), nil
	}
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, testCapture()), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
}

// hookGraphHookResults returns the committed hook_result payloads in
// committed order.
func hookGraphHookResults(t *testing.T, store *graphStorage, sessionID string) []hookResultEntry {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
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
	exec := hookFixtureExecution(spy.tool, normalize, hooks...)
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)})

	te := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())
	got, err := te(context.Background(), testToolCall("call-1"), []string{"echo"})
	testassert.NoError(t, err)
	testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}))
	// Callback counts: one normalization for the valid original at the
	// producer plus exactly one per hook replacement.
	mu.Lock()
	count, chain := normalizations, append([]string{}, received...)
	mu.Unlock()
	testassert.Equal(t, count, 3)
	testassert.Require(t, len(chain) == 2 && chain[0] == ` {"x": 1} ` && chain[1] == `{"x":"\u003c"}`, "hook chain received %q, want the raw arguments then the committed replacement bytes", chain)
	prepared := spy.received()
	testassert.Require(t, len(prepared) == 1 && prepared[0].ID == "call-1" && prepared[0].Name == "echo" && string(prepared[0].Arguments) == `{"y":"\u0026\u003e"}`, "preparer received %+v, want the final committed replacement bytes under the bound identity", prepared)

	// The committed evidence: one success hook_result per hook, whose stored
	// arguments are exactly the bytes the continuation consumed.
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Equal(t, len(committed), 2)
	testassert.Require(t, committed[0].HookID == "cap.hook.one" && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{"x":"\u003c"}` && committed[0].Error == "", "first hook result = %+v, want the committed compacted replacement", committed[0])
	testassert.Require(t, committed[1].HookID == "cap.hook.two" && committed[1].Status == hookSucceeded && string(committed[1].Arguments) == `{"y":"\u0026\u003e"}`, "second hook result = %+v, want the committed escaped replacement", committed[1])

	// The assistant entry never mutated: original raw bytes retained, the
	// producer's own committed normalization untouched.
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.Assistant == nil {
			continue
		}
		record := entry.Assistant.ToolCalls[0]
		{
			raw, derr := base64.StdEncoding.DecodeString(record.ArgumentsBase64)
			testassert.Require(t, derr == nil && string(raw) == ` {"x": 1} `, "assistant raw arguments = %q err %v, want the original bytes retained", raw, derr)
		}
		testassert.Require(t, string(record.NormalizedArguments) == `{"x":1}`, "assistant normalized_arguments = %q, want the producer's committed value unchanged", record.NormalizedArguments)
	}

	// Hook results are execution evidence, not conversation messages.
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "operation state = %+v, want a quiet running Operation with the call settled", rec.State)
	messages, err := fixtureProjection(h, c)(context.Background())
	testassert.NoError(t, err)
	for _, msg := range messages {
		for _, part := range msg.Content {
			testassert.Require(t, part.Text != `{"y":"\u0026\u003e"}`, "hook evidence projected as conversation content")
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
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize)
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run
			return nil, ctx.Err()
		}}
		exec = hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}))
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted && committed[0].HookID == "cap.hook.one" && committed[0].ToolCallID == "call-1" && committed[0].Error != "" && len(committed[0].Arguments) == 0, "hook results = %+v, want one interrupted result under the reserved identity", committed)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("cancellation between intent and run never runs the hook", func(t *testing.T) {
		ran := 0
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			ran++
			return json.RawMessage(`{}`), nil
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
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
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, ran, 0)
		testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}))
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted, "hook results = %+v, want one interrupted result under the intent's reserved identity", committed)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("cancellation aborting the next hook intent keeps the real result and interrupts", func(t *testing.T) {
		secondRan := 0
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize)
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		exec = hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize,
			ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				cancel() // the execution context dies during the successful hook's Run
				return json.RawMessage(`{"first":true}`), nil
			}},
			ToolArgumentsHook{ID: "cap.hook.two", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				secondRan++
				return json.RawMessage(`{}`), nil
			}},
		)
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}))
		testassert.Equal(t, secondRan, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "cap.hook.one" && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{"first":true}`, "hook results = %+v, want the earlier real hook result preserved", committed)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("cancellation-shaped error without a done context is a hook error", func(t *testing.T) {
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, context.Canceled // no done execution context: an ordinary hook error
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultError && got.Content == "context canceled", "tool result = %+v, want the validation error with the hook's diagnostic", got)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Error == "context canceled", "hook results = %+v, want one error result carrying the diagnostic", committed)
	})

	t.Run("real hook result commits before the next boundary observes cancellation", func(t *testing.T) {
		executorTool := func(context.Context, model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		exec := hookFixtureExecution(executorTool, objectNormalize)
		h, store, c, sessionID := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run, before the output commits
			return json.RawMessage(`{"fixed":true}`), nil
		}}
		exec = hookFixtureExecution(executorTool, objectNormalize, hook)
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		// The successful replacement committed as a real result; the
		// interruption appears at the next boundary (the tool intent).
		testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}))
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{"fixed":true}`, "hook results = %+v, want the committed real result", committed)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
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
			exec := hookFixtureExecution(spy.tool, normalizeFor, hooks...)
			h, store, c, sessionID := newHookHarness(t, exec)
			publishHookCalls(t, h, c, exec, testToolCall("call-1"), testToolCall("call-2"))

			got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
			testassert.Equal(t, got, tc.want)
			testassert.Equal(t, later, 0)
			committed := hookGraphHookResults(t, store, sessionID)
			testassert.Require(t, len(committed) == 1 && committed[0].Status == tc.wantStatus && committed[0].HookID == "cap.hook.one" && len(committed[0].Arguments) == 0 && committed[0].Error != "", "hook results = %+v, want one %s result with a non-empty diagnostic", committed, tc.wantStatus)
			testassert.Require(t, tc.wantError == "" || committed[0].Error == tc.wantError, "hook result error = %q, want %q", committed[0].Error, tc.wantError)
			// Later calls still run: call-2 executes after the failed chain.
			got, err = h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-2"), []string{"echo"})
			testassert.NoError(t, err)
			testassert.Require(t, got.Status == model.ResultSuccess, "later call result = %+v, want success", got)
			prepareCalls := spy.received()
			testassert.Require(t, len(prepareCalls) == 1 && prepareCalls[0].ID == "call-2", "preparer calls = %+v, want only the later call", prepareCalls)
			{
				_, err := validateFixture(t, store, sessionID)
				testassert.NoError(t, err)
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
	exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hooks...)
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
	{
		_, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.Require(t, errors.Is(err, ErrStorage), "tool effect = %v, want the injected storage failure", err)
	}
	mu.Lock()
	ran, ranSecond := runs, second
	mu.Unlock()
	testassert.Require(t, ran == 1 && !ranSecond, "hooks ran %d/%v, want the first only: a failed result transaction starts no later effect", ran, ranSecond)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil && rec.State.ActiveEffect.Kind == EffectHook && rec.State.ActiveEffect.HookID == "cap.hook.one" && rec.State.ActiveEffect.ToolCallID == "call-1", "operation state = %+v, want the committed running hook intent left for recovery", rec.State)
	reserved := rec.State.ActiveEffect.ResultEntryID
	testassert.Require(t, len(rec.State.PendingToolCalls) == 1 && rec.State.PendingToolCalls[0].CallID == "call-1", "pending calls = %+v, want the first call still pending", rec.State.PendingToolCalls)

	// Recovery interrupts the reserved active hook effect through the common
	// terminal helper: the interrupted hook_result under the reserved
	// identity, the interrupted tool result, and the terminal Operation.
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var hookResults []hookResultEntry
	for _, entry := range graph.Entries {
		if entry.HookResult != nil {
			hookResults = append(hookResults, *entry.HookResult)
		}
	}
	testassert.Require(t, len(hookResults) == 1 && hookResults[0].EntryID == reserved && hookResults[0].HookID == "cap.hook.one" && hookResults[0].ToolCallID == "call-1" && hookResults[0].Status == hookInterrupted && hookResults[0].Error == recoveryInterruptedDetail, "recovered hook results = %+v, want one interrupted result under the reserved identity %q", hookResults, reserved)
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	// The live harness holds its stale coordinator view; the durable truth is
	// the recovered store's own validated graph.
	rec = graph.Operations[0]
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "recovered operation = %+v, want the terminal interruption", rec.State)
	// No hook or tool replays: the store's next recovery run writes nothing.
	before := snapshotHookStore(t, store, sessionID)
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	after := snapshotHookStore(t, store, sessionID)
	testassert.Equal(t, before, after)
}

// snapshotHookStore counts one Session's committed entries and registers.
func snapshotHookStore(t *testing.T, store *graphStorage, sessionID string) int {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	registers, err := store.ReadRegisters(context.Background(), sessionID)
	testassert.NoError(t, err)
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
		return hookFixtureExecution(plan, objectNormalize, hook)
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
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent}))
		testassert.Equal(t, executed, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{"repaired":true}`, "hook results = %+v, want the settled hook effect left intact", committed)
	})

	t.Run("allowed sibling executes with the final committed value", func(t *testing.T) {
		spy := &hookToolSpy{}
		exec := buildExec(spy.tool)
		h, _, c, _ := newHookHarness(t, exec)
		publishHookCalls(t, h, c, exec, testToolCall("call-1"))
		got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultSuccess, "tool result = %+v, want success", got)
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 1 && string(prepared[0].Arguments) == `{"repaired":true}`, "preparer received %+v, want the final committed replacement", prepared)
	})
}

// TestToolArgumentHookAdvertisementGate proves the one advertisement gate
// precedes every hook: a committed call whose tool name is absent from the
// Engine-supplied per-call names of the producing request settles the
// unavailable-tool error and never runs a hook — a capture advertising that
// name grants it nothing — while a names-complete call proceeds past the gate
// into its hook chain and plan.
func TestToolArgumentHookAdvertisementGate(t *testing.T) {
	ran := 0
	hook := ToolArgumentsHook{ID: "cap.hook.one", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}}
	prepared := 0
	exec := hookFixtureExecution(func(_ context.Context, call model.ToolCall) PreparedTool {
		prepared++
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}}
	}, objectNormalize, hook)
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec,
		model.ToolCall{ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`)},
		model.ToolCall{ID: "call-2", Name: "ghost", Arguments: json.RawMessage(`{}`)},
	)

	ghostCapture := testCapture()
	ghostCapture.Tools = append(ghostCapture.Tools, model.ToolDefinition{Name: "ghost", Description: "ghost tool", Parameters: json.RawMessage(`{"type":"object"}`)})
	got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), ghostCapture)(context.Background(), model.ToolCall{ID: "call-1", Name: "ghost"}, []string{"echo"})
	testassert.NoError(t, err)
	testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: `Tool "ghost" is not available.`}))
	testassert.Equal(t, ran, 0)
	testassert.Equal(t, prepared, 0)
	{
		got := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(got) == 0, "hook results after the gated call = %+v, want none", got)
	}
	got, err = h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), model.ToolCall{ID: "call-2", Name: "ghost"}, []string{"ghost"})
	testassert.NoError(t, err)
	testassert.Require(t, got.CallID == "call-2" && got.Status == model.ResultSuccess && got.Content == "ran", "names-complete call = %+v, want it past the gate into its hook chain and plan", got)
	testassert.Equal(t, ran, 1)
	testassert.Equal(t, prepared, 1)
	{
		got := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(got) == 1 && got[0].HookID == "cap.hook.one", "hook results after the admitted call = %+v, want the one settled hook execution", got)
	}
}

// TestToolArgumentHookInvalidOpenedExecutionRejectsHooks proves the opened
// invocation requires a non-empty unique ID and a non-nil run function for
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
				return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
					return InvocationExecution{
						Model:         modelFn,
						Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
						NormalizeTool: objectNormalize,
						ToolHooks:     tc.hooks,
						Close: func() error {
							closed++
							return nil
						},
					}, nil
				}}, nil
			})
			{
				err := executeDirect(h, c, h.ctx, testOpID, prepared)
				testassert.Require(t, err != nil, "execute = nil, want the invalid-hook rejection")
			}
			testassert.Equal(t, closed, 1)
			closed = 0
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Equal(t, rec.State.Status, OperationFailure)
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
	exec := hookFixtureExecution(spy.tool, objectNormalize, hook)
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
	{
		_, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.Require(t, errors.Is(err, ErrStorage), "tool effect = %v, want the injected storage failure", err)
	}
	testassert.Equal(t, ran, 0)
	{
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 0, "preparer ran for %v, want zero preparations", prepared)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 1, "operation state = %+v, want the quiet running state with the call still pending", rec.State)
	{
		got := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(got) == 0, "hook results = %+v, want none without a committed reservation", got)
	}

	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	rec = graph.Operations[0]
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "recovered operation = %+v, want the terminal interruption", rec.State)
	{
		got := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(got) == 0, "hook results after recovery = %+v, want none: no reservation, no replay", got)
	}
}
