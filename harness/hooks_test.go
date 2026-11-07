package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: testSelection("coder")})
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
		return InvocationExecution{
			Model: func(context.Context, model.Request) (model.Stream, error) {
				return completedTurnStream(), nil
			},
			Tool: toolFn, NormalizeTool: normalize, ToolHooks: hooks,
		}, nil
	}}
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
		{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			received = append(received, string(call.Arguments))
			mu.Unlock()
			return json.RawMessage(`{ "x": "<" }`), nil
		}},
		{ID: "cap.hook.two", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			received = append(received, string(call.Arguments))
			mu.Unlock()
			return json.RawMessage(`{"y":"&>"}`), nil
		}},
	}
	spy := &hookToolSpy{}
	exec := hookFixtureExecution(spy.tool, normalize, hooks...)
	h, store, c, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)})
	testassert.NoError(t, err)
	got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
	testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}))
	// Callback counts: one normalization for the valid original at the
	// producer plus exactly one per hook replacement.
	mu.Lock()
	count, chain := normalizations, append([]string{}, received...)
	mu.Unlock()
	testassert.Equal(t, count, 3)
	testassert.Require(t, len(chain) == 2 && chain[0] == ` {"x": 1} ` && chain[1] == `{ "x": "<" }`, "hook chain received %q, want the raw arguments then the committed replacement bytes", chain)
	prepared := spy.received()
	testassert.Require(t, len(prepared) == 1 && prepared[0].ID == "call-1" && prepared[0].Name == "echo" && string(prepared[0].Arguments) == `{"y":"&>"}`, "preparer received %+v, want the final committed replacement bytes under the bound identity", prepared)

	// The committed evidence: one success hook_result per hook, whose stored
	// arguments are exactly the bytes the continuation consumed.
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Equal(t, len(committed), 2)
	testassert.Require(t, committed[0].HookID == "cap.hook.one" && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{ "x": "<" }` && committed[0].Error == "", "first hook result = %+v, want the committed replacement", committed[0])
	testassert.Require(t, committed[1].HookID == "cap.hook.two" && committed[1].Status == hookSucceeded && string(committed[1].Arguments) == `{"y":"&>"}`, "second hook result = %+v, want the committed replacement", committed[1])

	// The assistant entry never mutated: original raw bytes retained, the
	// producer's own committed normalization untouched.
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.Assistant == nil || len(entry.Assistant.ToolCalls) == 0 {
			continue
		}
		record := entry.Assistant.ToolCalls[0]
		{
			raw, derr := base64.StdEncoding.DecodeString(record.ArgumentsBase64)
			testassert.Require(t, derr == nil && string(raw) == ` {"x": 1} `, "assistant raw arguments = %q err %v, want the original bytes retained", raw, derr)
		}
		testassert.Require(t, string(record.NormalizedArguments) == ` {"x": 1} `, "assistant normalized_arguments = %q, want the producer's committed value unchanged", record.NormalizedArguments)
	}

	// Hook results are execution evidence, not conversation messages.
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0 && rec.State.Terminal != nil, "operation state = %+v, want the effect cleared, the call settled and the run retired", rec.State)
	messages, err := fixtureProjection(h, c)(context.Background())
	testassert.NoError(t, err)
	for _, msg := range messages {
		for _, part := range msg.Content {
			testassert.Require(t, part.Text != `{"y":"\u0026\u003e"}`, "hook evidence projected as conversation content")
		}
	}
}

// TestToolArgumentHookInterruptionRules proves the interruption rows fixed
// for every hook: an explicitly interrupted callback and cancellation between
// the committed intent and Run interrupt, while a
// successful normalized output is a real result even when cancellation
// arrives before its commit — interruption then appears at the next boundary.
func TestToolArgumentHookInterruptionRules(t *testing.T) {
	t.Run("explicit interruption during run interrupts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run
			return nil, &EffectError{Evidence: ExecutionObserved, Err: ctx.Err(), Interrupted: true}
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		_, store, _, sessionID, results, err := freshExecutionToolCallsCtx(t, ctx, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
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
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			ran++
			return json.RawMessage(`{}`), nil
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		ctx, cancel := context.WithCancel(context.Background())
		_, store, _, sessionID, results, err := freshExecutionToolCallsConfig(t, ctx, testCapture(), exec, func(store *graphStorage) {
			failEffectRegister(store,
				func(ae *ActiveEffect) bool { return ae.Kind == EffectHook && ae.HookID == "cap.hook.one" },
				false, nil, cancel) // the context dies between the committed hook intent and Run
		}, testToolCall("call-1"))
		testassert.NoError(t, err)
		got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
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
		ctx, cancel := context.WithCancel(context.Background())
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize,
			ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				cancel() // the execution context dies during the successful hook's Run
				return json.RawMessage(`{"first":true}`), nil
			}},
			ToolArgumentsHook{ID: "cap.hook.two", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				secondRan++
				return json.RawMessage(`{}`), nil
			}},
		)
		_, store, _, sessionID, results, err := freshExecutionToolCallsCtx(t, ctx, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
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
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, context.Canceled // no done execution context: an ordinary hook error
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
		testassert.Require(t, got.Status == model.ResultError && got.Content == "context canceled", "tool result = %+v, want the validation error with the hook's diagnostic", got)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Error == "context canceled", "hook results = %+v, want one error result carrying the diagnostic", committed)
	})

	t.Run("real hook result commits before the next boundary observes cancellation", func(t *testing.T) {
		executorTool := func(context.Context, model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		ctx, cancel := context.WithCancel(context.Background())
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(ctx context.Context, _ model.ToolCall) (json.RawMessage, error) {
			cancel() // the execution context dies during Run, before the output commits
			return json.RawMessage(`{"fixed":true}`), nil
		}}
		exec := hookFixtureExecution(executorTool, objectNormalize, hook)
		_, store, _, sessionID, results, err := freshExecutionToolCallsCtx(t, ctx, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		got := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
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
		run        func(context.Context, model.ToolCall) (json.RawMessage, error)
		normalize  func(model.ToolCall) (json.RawMessage, error)
		want       model.ToolResult
		wantStatus hookResultStatus
		wantError  string // the hook_result error member; empty asserts only a non-empty diagnostic
	}{
		{
			name:       "returned error carries the bounded diagnostic",
			run:        func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, errors.New("hook broke") },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "hook broke"},
			wantStatus: hookFailed,
			wantError:  "hook broke",
		},
		{
			name: "cancellation-shaped failure leaves later sibling eligible",
			run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return nil, &EffectError{Evidence: ExecutionObserved, Err: context.Canceled}
			},
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "context canceled"},
			wantStatus: hookFailed,
			wantError:  "context canceled",
		},
		{
			name:       "an empty-text error keeps valid evidence and settles the call",
			run:        func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, errors.New("") },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
			wantError:  invalidToolResultContent,
		},
		{
			name:       "invalid returned value is the internal-validation error",
			run:        func(context.Context, model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`[1,2]`), nil },
			normalize:  objectNormalize,
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
		},
		{
			name: "normalizer rejection keeps its useful diagnostic",
			run:  func(context.Context, model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`{"a":1}`), nil },
			normalize: func(model.ToolCall) (json.RawMessage, error) {
				return nil, errors.New("no default for a")
			},
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "no default for a"},
			wantStatus: hookFailed,
			wantError:  "no default for a",
		},
		{
			name:       "malformed normalizer outcome is the internal-validation error",
			run:        func(context.Context, model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`{"a":1}`), nil },
			normalize:  func(model.ToolCall) (json.RawMessage, error) { return json.RawMessage(`[1,2]`), nil },
			want:       model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantStatus: hookFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var later int
			hooks := []ToolArgumentsHook{
				{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
					if call.ID == "call-1" { // only the first call's chain fails; a later call runs its own chain
						return tc.run(ctx, call)
					}
					return json.RawMessage(`{}`), nil
				}},
				{ID: "cap.hook.two", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
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
			_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"), testToolCall("call-2"))
			testassert.NoError(t, err)
			first := model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}
			testassert.Equal(t, first, tc.want)
			testassert.Equal(t, later, 0)
			committed := hookGraphHookResults(t, store, sessionID)
			var firstResults []hookResultEntry
			for _, hook := range committed {
				if hook.ToolCallID == "call-1" {
					firstResults = append(firstResults, hook)
				}
			}
			testassert.Require(t, len(firstResults) == 1 && firstResults[0].Status == tc.wantStatus && firstResults[0].HookID == "cap.hook.one" && len(firstResults[0].Arguments) == 0 && firstResults[0].Error != "", "hook results = %+v, want one %s result with a non-empty diagnostic", firstResults, tc.wantStatus)
			testassert.Require(t, tc.wantError == "" || firstResults[0].Error == tc.wantError, "hook result error = %q, want %q", firstResults[0].Error, tc.wantError)
			// Later calls still run: call-2 executes after the failed chain.
			laterResult := model.ToolResult{CallID: results[1].ToolCallID, Status: results[1].Status, Content: results[1].Content}
			testassert.Require(t, laterResult.Status == model.ResultSuccess, "later call result = %+v, want success", laterResult)
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
		{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			return json.RawMessage(`{}`), nil
		}},
		{ID: "cap.hook.two", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			mu.Lock()
			second = true
			mu.Unlock()
			return json.RawMessage(`{}`), nil
		}},
	}
	exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hooks...)
	failed := false
	_, store, _, sessionID, err := freshExecutionRunConfig(t, context.Background(), testCapture(), exec, func(store *graphStorage) {
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryHookResult && !failed { // recovery's own hook_result insert succeeds
				failed = true
				return fmt.Errorf("%w: injected hook result failure", ErrStorage)
			}
			return nil
		}
	}, testToolCall("call-1"))
	testassert.Require(t, errors.Is(err, ErrStorage), "tool effect = %v, want the injected storage failure", err)
	mu.Lock()
	ran, ranSecond := runs, second
	mu.Unlock()
	testassert.Require(t, ran == 1 && !ranSecond, "hooks ran %d/%v, want the first only: a failed result transaction starts no later effect", ran, ranSecond)
	h := newTestHarness(t, store, nil)
	rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, rerr)
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
	hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
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
					return ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				},
			}
		})
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Equal(t, model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}, (model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent}))
		testassert.Equal(t, executed, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookSucceeded && string(committed[0].Arguments) == `{"repaired":true}`, "hook results = %+v, want the settled hook effect left intact", committed)
	})

	t.Run("allowed sibling executes with the final committed value", func(t *testing.T) {
		spy := &hookToolSpy{}
		exec := buildExec(spy.tool)
		_, _, _, _, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultSuccess, "tool result = %+v, want success", results[0])
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
	hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}}
	prepared := 0
	exec := hookFixtureExecution(func(_ context.Context, call model.ToolCall) PreparedTool {
		prepared++
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}}
	}, objectNormalize, hook)
	ghost := model.ToolCall{ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`)}

	// The producing request advertises only echo: the ghost call settles the
	// unavailable-tool error and never runs a hook or preparer.
	_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, ghost)
	testassert.NoError(t, err)
	testassert.Equal(t, model.ToolResult{CallID: results[0].ToolCallID, Status: results[0].Status, Content: results[0].Content}, (model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: `Tool "ghost" is not available.`}))
	testassert.Equal(t, ran, 0)
	testassert.Equal(t, prepared, 0)
	testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results after the gated call, want none")

	// A capture whose request advertises ghost lets the same call past the gate
	// into its hook chain and plan.
	ghostCapture := testCapture()
	ghostCapture.Tools = append(ghostCapture.Tools, model.ToolDefinition{Name: "ghost", Description: "ghost tool", Parameters: json.RawMessage(`{"type":"object"}`)})
	_, store, _, sessionID, results, err = freshExecutionToolCalls(t, ghostCapture, exec, ghost)
	testassert.NoError(t, err)
	testassert.Require(t, results[0].Status == model.ResultSuccess && results[0].Content == "ran", "names-complete call = %+v, want it past the gate into its hook chain and plan", results[0])
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
			{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
			{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
		}},
		{"empty extension identity", []ToolArgumentsHook{
			{ID: "cap.hook.one", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
		}},
		{"invalid mode", []ToolArgumentsHook{
			{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: "observer", Run: func(context.Context, model.ToolCall) (json.RawMessage, error) { return nil, nil }},
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
	hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		ran++
		return json.RawMessage(`{}`), nil
	}}
	spy := &hookToolSpy{}
	exec := hookFixtureExecution(spy.tool, objectNormalize, hook)
	injected := fmt.Errorf("%w: injected hook intent failure", ErrStorage)
	_, store, _, sessionID, err := freshExecutionRunConfig(t, context.Background(), testCapture(), exec, func(store *graphStorage) {
		failEffectRegister(store,
			func(ae *ActiveEffect) bool {
				return ae.Kind == EffectHook && ae.HookID == "cap.hook.one" && ae.ToolCallID == "call-1"
			},
			false, injected, nil)
	}, testToolCall("call-1"))
	testassert.Require(t, errors.Is(err, ErrStorage), "tool effect = %v, want the injected storage failure", err)
	testassert.Equal(t, ran, 0)
	testassert.Require(t, len(spy.received()) == 0, "preparer ran for %v, want zero preparations", spy.received())
	h := newTestHarness(t, store, nil)
	rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, rerr)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 1, "operation state = %+v, want the quiet running state with the call still pending", rec.State)
	testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results, want none without a committed reservation")

	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	rec = graph.Operations[0]
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "recovered operation = %+v, want the terminal interruption", rec.State)
	testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results after recovery, want none: no reservation, no replay")
}

// requestHookFixtureExecution supplies one opened conversation invocation
// carrying the given request hooks over the fixture model and tool callbacks.
func requestHookFixtureExecution(modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool, hooks ...RequestHook) Execution {
	if modelFn == nil {
		modelFn = func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
	}
	if toolFn == nil {
		toolFn = func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	}
	return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{Model: modelFn, Tool: toolFn, NormalizeTool: objectNormalize, RequestHooks: hooks}, nil
	}}
}

// runRequestHookRound drives one real pipeline round over the already-admitted
// Operation and returns the first conversation model request it dispatched.
func runRequestHookRound(t *testing.T, h *Harness, c *coordinator, exec Execution, capture ExecutionCapture) (model.Request, error) {
	t.Helper()
	return runAdmittedExecution(t, h, c, context.Background(), testOpID, capture, exec)
}

// hookGraphHookResultsByStatus returns the committed hook_result payloads in
// committed order filtered by nothing — fixtures inspect statuses themselves.
func graphSettlementOf(t *testing.T, store *graphStorage, sessionID string) *operationSettlementEntry {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var out *operationSettlementEntry
	count := 0
	for _, entry := range graph.Entries {
		if entry.Settlement != nil {
			count++
			out = entry.Settlement
		}
	}
	testassert.Equal(t, count, 1)
	return out
}

// TestRequestHookEffectfulChainSettlement proves the request-boundary positive
// pipeline row: an effectful request hook settles its own durable effect whose
// consumed input evidence names the frozen canonical projection it actually
// received, the continuation advances from the codec-adopted committed
// consequence, the dispatch consumes the contribution, and the answering
// assistant records the identity and the final effective delta in contribution
// order beside the pure contributor.
func TestRequestHookEffectfulChainSettlement(t *testing.T) {
	var saw RequestHookInput
	changed := "effectful prompt"
	addition, aerr := model.NewMessage(model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "contributed"}}})
	testassert.NoError(t, aerr)
	pure := RequestHook{ID: "req.pure", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		prompt := "pure prompt " + in.Prompt
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	effectful := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		saw = in
		return RequestConsequence{Prompt: &changed, Messages: []model.Message{addition}}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}, nil, pure, effectful)
	h, store, c, sessionID := newHookHarness(t, exec)
	sent, err := runRequestHookRound(t, h, c, exec, testCapture())
	testassert.NoError(t, err)

	// The hook input carried the committed intent's adopted evidence and the
	// view the earlier pure contribution produced, never a reread projection.
	testassert.Require(t, saw.RequestID != "" && saw.BoundaryEntryID != "", "hook input = %+v, want the bound request identity and canonical boundary", saw)
	testassert.Require(t, saw.Prompt == "pure prompt system", "hook input prompt = %q, want the view after the earlier pure contribution", saw.Prompt)

	// The dispatch request consumed both contributions exactly once.
	testassert.Require(t, len(sent.Messages) >= 2 && sent.Messages[0].TextContent() == "effectful prompt", "dispatch prompt = %q, want the effectful replacement", sent.Messages[0].TextContent())
	last := sent.Messages[len(sent.Messages)-1]
	testassert.Require(t, last.TextContent() == "contributed", "dispatch tail = %q, want the committed contribution applied to the rebuilt view", last.TextContent())

	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.effectful" && committed[0].ExtensionID == "req-ext" &&
		committed[0].RequestID != "" && committed[0].ToolCallID == "" && committed[0].Status == hookSucceeded &&
		committed[0].Evidence == ExecutionObserved && committed[0].InputEvidence != nil && committed[0].Consequence != nil,
		"committed hook result = %+v, want the request-bound effect with consumed evidence and consequence", committed[0])
	testassert.Require(t, committed[0].Consequence.Prompt != nil && *committed[0].Consequence.Prompt == "effectful prompt" && len(committed[0].Consequence.Messages) == 1,
		"committed consequence = %+v, want the durable replacement the continuation adopted", committed[0].Consequence)

	// The consumed input evidence retains the earlier pure intermediate (an
	// actual effect boundary consumed it); the final assistant evidence keeps
	// only the net prompt setter.
	testassert.Require(t, len(committed[0].InputEvidence.HookIDs) == 1 &&
		committed[0].InputEvidence.HookIDs[0].HookID == "req.pure" &&
		committed[0].InputEvidence.Prompt == "pure prompt system",
		"consumed input evidence = %+v, want the retained pure intermediate and its prompt delta at the effect boundary", committed[0].InputEvidence)
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.HookIDs) == 2 &&
		assistant.Evidence.HookIDs[0].HookID == "req.pure" && assistant.Evidence.HookIDs[1].HookID == "req.effectful",
		"assistant evidence hook identities = %+v, want the changed pure contributor and the effectful setter in hook order", assistant.Evidence.HookIDs)
	testassert.Require(t, assistant.Evidence.Prompt == "effectful prompt" && len(assistant.Evidence.Messages) == 1,
		"assistant evidence = %+v, want the final effective prompt and contribution", assistant.Evidence)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestRequestHookIntentFailureLeavesRecovery proves the request family's
// intent publication-failure row through the real pipeline: the failed intent
// transaction runs no hook callback and dispatches no conversation request,
// the source returns the original storage cause (never the steering token),
// the Operation stays exactly as committed for recovery, and recovery settles
// the interruption with no hook effect and no replay.
func TestRequestHookIntentFailureLeavesRecovery(t *testing.T) {
	ran := 0
	modelCalls := 0
	hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		ran++
		return RequestConsequence{}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		modelCalls++
		return completedTurnStream(), nil
	}, nil, hook)
	h, store, c, sessionID := newHookHarness(t, exec)
	failEffectRegister(store,
		func(ae *ActiveEffect) bool {
			return ae.Kind == EffectHook && ae.HookID == "req.effectful" && ae.RequestID != ""
		},
		false, fmt.Errorf("%w: injected hook intent failure", ErrStorage), nil)
	{
		_, err := runRequestHookRound(t, h, c, exec, testCapture())
		testassert.Require(t, errors.Is(err, ErrStorage), "pipeline = %v, want the injected storage failure verbatim", err)
	}
	testassert.Equal(t, ran, 0)
	testassert.Equal(t, modelCalls, 0)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && rec.State.Request == nil,
		"operation state = %+v, want the unchanged running state after the rolled-back intent", rec.State)
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
	testassert.Require(t, graph.Operations[0].State.Status == OperationInterruption && graph.Operations[0].State.Terminal != nil, "recovered operation = %+v, want the terminal interruption", graph.Operations[0].State)
	{
		got := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(got) == 0, "hook results after recovery = %+v, want none: no reservation, no replay", got)
	}
}

// TestRequestHookResultFailureLeavesRecovery proves the request family's
// result publication-failure row through the real pipeline: the hook ran and
// its consequence commit failed, the committed intent and its published
// running request stay durable, no conversation request is dispatched, and
// recovery settles the reserved hook effect as its own interrupted result
// carrying the consumed input evidence with unknown knowledge, while the
// settlement witnesses the published request identity exactly once.
func TestRequestHookResultFailureLeavesRecovery(t *testing.T) {
	ran := 0
	modelCalls := 0
	prompt := "committed by the hook"
	hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		ran++
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		modelCalls++
		return completedTurnStream(), nil
	}, nil, hook)
	h, store, c, sessionID := newHookHarness(t, exec)
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind != EntryHookResult {
			return nil
		}
		entry, derr := decodeHookResultEntry(Entry{SessionID: draft.SessionID, ID: draft.ID, OperationID: draft.OperationID, Kind: draft.Kind, Payload: draft.Payload})
		if derr == nil && entry.HookID == "req.effectful" && entry.RequestID != "" && entry.Status == hookSucceeded {
			return fmt.Errorf("%w: injected hook result failure", ErrStorage)
		}
		return nil
	}
	{
		_, err := runRequestHookRound(t, h, c, exec, testCapture())
		testassert.Require(t, errors.Is(err, ErrStorage), "pipeline = %v, want the injected storage failure verbatim", err)
	}
	testassert.Equal(t, ran, 1)
	testassert.Equal(t, modelCalls, 0)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil && rec.State.ActiveEffect.Kind == EffectHook &&
		rec.State.ActiveEffect.RequestID != "" && rec.State.Request != nil && rec.State.Request.RequestID == rec.State.ActiveEffect.RequestID,
		"operation state = %+v, want the retained hook intent with its published running request", rec.State)
	publishedRequestID := rec.State.Request.RequestID
	reservedID := rec.State.ActiveEffect.ResultEntryID
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].EntryID == reservedID && committed[0].Status == hookInterrupted &&
		committed[0].Evidence == ExecutionUnknown && committed[0].RequestID == publishedRequestID &&
		committed[0].InputEvidence != nil && committed[0].Consequence == nil,
		"recovered hook result = %+v, want the interrupted unknown-knowledge effect carrying the consumed evidence", committed[0])
	settlement := graphSettlementOf(t, store, sessionID)
	testassert.Require(t, settlement.RequestID == publishedRequestID && settlement.Status == OperationInterruption && settlement.Evidence == nil,
		"settlement = %+v, want the published request identity witnessed exactly once", settlement)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestRequestHookFailureSettlesTerminalOnce proves the failed-hook
// settlement owner through the full execution pipeline: the hook error
// settles one failure terminal carrying its nonempty diagnostic, the
// conversation request is never dispatched, the effect's error hook_result
// settles before any further hook or dispatch, and no second terminal write
// or replayed effect survives a recovery pass.
func TestRequestHookFailureSettlesTerminalOnce(t *testing.T) {
	ran := 0
	hookErr := errors.New("request binding broke")
	hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		ran++
		return RequestConsequence{}, hookErr
	}}
	h, store, c, sessionID, prepared, cancel := newExecutionHarness(t, func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("a failed request hook dispatched a conversation request")
		return nil, nil
	}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
	defer cancel()
	// The opened invocation carries the failing request hook.
	prepared.Open = func(_ context.Context, _ OperationAdmission) (Execution, error) {
		return requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("a failed request hook dispatched a conversation request")
			return nil, nil
		}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, hook), nil
	}
	before, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, err != nil && errors.Is(err, hookErr), "execute = %v, want the hook cause returned verbatim", err)
	}
	testassert.Equal(t, ran, 1)
	settlement := graphSettlementOf(t, store, sessionID)
	testassert.Require(t, settlement.Status == OperationFailure && settlement.Detail == "request binding broke" &&
		settlement.RequestID != "" && settlement.Evidence == nil && len(settlement.Facts) == 0,
		"settlement = %+v, want exactly one failure terminal with the hook diagnostic and the request witness", settlement)
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Evidence == ExecutionObserved &&
		committed[0].Error == "request binding broke", "hook results = %+v, want the single settled failed effect", committed)
	after, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == len(before)+2, "entry tail = %d new entries, want exactly the failed hook result plus the one failure settlement: %d -> %d", len(after)-len(before), len(before), len(after))
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	after2, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	testassert.Equal(t, len(after2), len(after))
}

// TestRequestHookDeclaredKnowledgeRows proves the shared hook-knowledge rule
// on the request family: a typed EffectError carries its declared knowledge
// through the failure unchanged to the durable hook_result, and an invalid
// declaration is rejected at persistence, retains the committed intent for
// recovery, and is never coerced — recovery settles it as unknown.
func TestRequestHookDeclaredKnowledgeRows(t *testing.T) {
	t.Run("declared not_started survives unchanged", func(t *testing.T) {
		hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{}, &EffectError{Evidence: ExecutionNotStarted, Err: errors.New("binding refused before entry")}
		}}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("a failed request hook dispatched a conversation request")
			return nil, nil
		}, nil, hook)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			_, err := runRequestHookRound(t, h, c, exec, testCapture())
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "binding refused before entry"), "pipeline = %v, want the declared cause", err)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Evidence == ExecutionNotStarted,
			"hook results = %+v, want the declared not_started knowledge unchanged", committed)
		settlement := graphSettlementOf(t, store, sessionID)
		testassert.Require(t, settlement.Status == OperationFailure && settlement.RequestID != "",
			"settlement = %+v, want the execution's own settlement to witness the published request once after the settled declared failure", settlement)
	})
	t.Run("invalid declaration rejects at persistence and recovery settles unknown", func(t *testing.T) {
		hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{}, &EffectError{Evidence: ExecutionEvidence("all"), Err: errors.New("binding broke")}
		}}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("a retained hook intent dispatched a conversation request")
			return nil, nil
		}, nil, hook)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			_, err := runRequestHookRound(t, h, c, exec, testCapture())
			testassert.Require(t, err != nil && errors.Is(err, ErrInvalid), "pipeline = %v, want the persistence rejection of the invalid declaration", err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil && rec.State.ActiveEffect.Kind == EffectHook,
			"operation state = %+v, want the committed hook intent retained", rec.State)
		{
			err := Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted && committed[0].Evidence == ExecutionUnknown,
			"recovered hook results = %+v, want recovery settling the retained effect as unknown", committed)
	})
}

// TestArgumentHookPureChainRepairRestoreAndRefusal proves the argument family's
// pure rows through the real tool effect: a pure repair followed by an exact
// restore adds zero hook transactions and dispatches the assistant's committed
// normalization unchanged while keeping every accepted identity, a pure refusal
// settles the call durably with no effect transaction and never reaches the
// preparer, and an effectful no-op in a net-restored chain still records its
// own result, retains the consumed pure intermediate bytes as its input, and
// every accepted hook stays in the tool settlement's provenance in configured
// order.
// The repair row adds the settled D2 positive: a pure-only chain repairs a
// producer-rejected invalid original, normalizing the original exactly once
// (rejected) and the replacement exactly once with no re-normalization at
// dispatch, and adds zero durable mutation steps over the same hookless
// valid-call settlement.
func TestArgumentHookPureChainRepairRestoreAndRefusal(t *testing.T) {
	t.Run("pure repair then exact restore adds no transactions", func(t *testing.T) {
		spy := &hookToolSpy{}
		exec := hookFixtureExecution(spy.tool, objectNormalize,
			ToolArgumentsHook{ID: "cap.pure.one", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":9}`), nil
			}},
			ToolArgumentsHook{ID: "cap.pure.two", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":1}`), nil // the exact original bytes
			}},
		)
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultSuccess, "tool result = %+v, want the restored chain to run the call", results[0])
		testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results = %+v, want zero durable transactions for a net-restored pure chain", hookGraphHookResults(t, store, sessionID))
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 1 && string(prepared[0].Arguments) == `{"x":1}`, "preparer received %+v, want the assistant's committed normalization restored", prepared)
		testassert.Require(t, string(results[0].NormalizedArguments) == `{"x":1}` &&
			len(results[0].HookIDs) == 2 && results[0].HookIDs[0].HookID == "cap.pure.one" && results[0].HookIDs[1].HookID == "cap.pure.two",
			"tool result provenance = %+v %s, want the original consumed value and every accepted pure identity in order", results[0].NormalizedArguments, results[0].HookIDs)
	})
	t.Run("pure refusal settles the call with no durable effect", func(t *testing.T) {
		refusal := errors.New("pure hook refused")
		spy := &hookToolSpy{}
		exec := hookFixtureExecution(spy.tool, objectNormalize,
			ToolArgumentsHook{ID: "cap.pure.refuse", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return nil, refusal
			}},
			ToolArgumentsHook{ID: "cap.pure.after", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				t.Fatal("a hook ran after the chain's refusal")
				return nil, nil
			}},
		)
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultError && strings.Contains(results[0].Content, "pure hook refused"), "tool result = %+v, want the validation failure carrying the refusal", results[0])
		testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results, want none: a pure refusal has no effect transaction")
		testassert.Require(t, len(spy.received()) == 0, "preparer ran for %+v, want no preparation after the refusal", spy.received())
		testassert.Require(t, results[0].Status == model.ResultError && strings.Contains(results[0].Content, "pure hook refused") && len(results[0].HookIDs) == 0,
			"committed tool result = %+v, want the refusal diagnostic settled once with no hook provenance", results[0])
	})
	t.Run("effectful no-op in a restored chain keeps its result and consumed input", func(t *testing.T) {
		spy := &hookToolSpy{}
		exec := hookFixtureExecution(spy.tool, objectNormalize,
			ToolArgumentsHook{ID: "cap.pure.one", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":9}`), nil
			}},
			ToolArgumentsHook{ID: "cap.effectful.noop", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				return model.CloneRaw(call.Arguments), nil // the value it received, unchanged
			}},
			ToolArgumentsHook{ID: "cap.pure.restore", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":1}`), nil
			}},
		)
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultSuccess, "tool result = %+v, want the settled no-op chain to run the call", results[0])
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "cap.effectful.noop" && committed[0].Status == hookSucceeded &&
			string(committed[0].Arguments) == `{"x":9}`, "committed hook results = %+v, want the effectful no-op's own result", committed)
		consumed, derr := base64.StdEncoding.DecodeString(committed[0].InputArgumentsBase64)
		testassert.Require(t, derr == nil && string(consumed) == `{"x":9}`, "consumed input = %q, want the pure intermediate bytes the no-op consumed", consumed)
		testassert.Require(t, len(results[0].HookIDs) == 3 &&
			results[0].HookIDs[0].HookID == "cap.pure.one" && results[0].HookIDs[1].HookID == "cap.effectful.noop" && results[0].HookIDs[2].HookID == "cap.pure.restore" &&
			string(results[0].NormalizedArguments) == `{"x":1}`, "tool result provenance = %+v %s, want every accepted identity in configured order with the restored consumed value", results[0].HookIDs, results[0].NormalizedArguments)
	})
	t.Run("pure-only repair of an invalid original normalizes exactly twice and adds no transactions", func(t *testing.T) {
		var (
			mu            sync.Mutex
			attempts      []string // normalizer argument bytes per attempt in call order
			failures      int
			repairSteps   int
			hooklessSteps int
		)
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized, err := objectNormalize(call)
			mu.Lock()
			attempts = append(attempts, string(call.Arguments))
			if err != nil {
				failures++
			}
			mu.Unlock()
			return normalized, err
		}
		broken := testToolCall("call-1")
		broken.Arguments = json.RawMessage("not json")
		var preparedCalls []model.ToolCall
		prepareArrived := make(chan struct{}, 1)
		prepareGate := make(chan struct{})
		plan := func(ctx context.Context, call model.ToolCall) PreparedTool {
			preparedCalls = append(preparedCalls, call)
			prepareArrived <- struct{}{}
			<-prepareGate
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		exec := hookFixtureExecution(plan, normalize,
			ToolArgumentsHook{ID: "cap.pure.repair", ExtensionID: "cap-a", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				testassert.Equal(t, string(call.Arguments), "not json")
				return json.RawMessage(`{"x":2}`), nil
			}},
		)
		store := emptyStore(t)
		admission := preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return Execution{}, nil
		})
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return admission, nil
		})
		session, serr := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: testSelection("coder")})
		testassert.NoError(t, serr)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		sessionID := session.Identity.SessionID
		mustAdmitWithoutExecution(t, h, sessionID, testOpID, admissionContent("hello"))
		store.txHook = func(string) error { mu.Lock(); repairSteps++; mu.Unlock(); return nil }
		open := exec.OpenInvocation
		first := true
		exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
			inv, ierr := open(ctx, request)
			if ierr != nil {
				return inv, ierr
			}
			if request.Purpose == InvocationPurposeConversation {
				inv.Model = func(context.Context, model.Request) (model.Stream, error) {
					if first {
						first = false
						return completedTurnStream(broken), nil
					}
					return completedTurnStream(), nil
				}
			}
			return inv, nil
		}
		done := make(chan error, 1)
		go func() {
			done <- executeDirect(h, c, h.ctx, testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
				return exec, nil
			}))
		}()
		select {
		case <-prepareArrived:
		case err := <-done:
			t.Fatalf("execution ended before preparation: %v", err)
		}
		{
			mu.Lock()
			testassert.Require(t, len(attempts) == 2 && attempts[0] == "not json" && attempts[1] == `{"x":2}` && failures == 1,
				"normalization attempts = %v with %d failures, want the rejected original plus one accepted replacement", attempts, failures)
			mu.Unlock()
			assistant := compositionAssistant(t, store, sessionID)
			testassert.Require(t, len(assistant.ToolCalls) == 1 && assistant.ToolCalls[0].NormalizedArguments == nil,
				"assistant call record = %+v, want no committed normalization before the repair", assistant.ToolCalls)
			testassert.Require(t, assistant.ToolCalls[0].NormalizationFailure != "",
				"assistant call record = %+v, want the saved normalization-failure diagnostic on the invalid original", assistant.ToolCalls)
			raw, derr := base64.StdEncoding.DecodeString(assistant.ToolCalls[0].ArgumentsBase64)
			testassert.Require(t, derr == nil && string(raw) == "not json", "assistant raw bytes = %q, want the source arguments preserved verbatim", raw)
		}
		close(prepareGate)
		testassert.NoError(t, <-done)
		{
			mu.Lock()
			testassert.Require(t, len(attempts) == 2 && failures == 1 && attempts[1] == `{"x":2}`,
				"normalization attempts = %v with %d failures, want the rejected original plus one accepted replacement and no re-normalization at dispatch", attempts, failures)
			mu.Unlock()
		}
		testassert.Require(t, len(preparedCalls) == 1 && string(preparedCalls[0].Arguments) == `{"x":2}`, "preparer received %+v, want the accepted normalized replacement", preparedCalls)
		testassert.Require(t, len(hookGraphHookResults(t, store, sessionID)) == 0, "hook results, want zero durable hook transactions for a pure-only repair")
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		settled := 0
		for _, entry := range graph.Entries {
			if entry.ToolResult == nil {
				continue
			}
			settled++
			testassert.Require(t, string(entry.ToolResult.NormalizedArguments) == `{"x":2}` &&
				len(entry.ToolResult.HookIDs) == 1 && entry.ToolResult.HookIDs[0].HookID == "cap.pure.repair" &&
				entry.ToolResult.Content == "done" && entry.ToolResult.Evidence == ExecutionObserved,
				"tool result = %+v, want the repaired value and the pure contributor identity over the executed outcome", entry.ToolResult)
		}
		testassert.Equal(t, settled, 1)
		// The same fixture without hooks settles the valid original; equal
		// durable mutation-step counts show the pure repair added no transaction.
		baseline := testToolCall("call-1")
		spy2 := &hookToolSpy{}
		exec2 := hookFixtureExecution(spy2.tool, objectNormalize)
		_, _, _, _, bres, berr := freshExecutionToolCallsConfig(t, context.Background(), testCapture(), exec2, func(store *graphStorage) {
			store.txHook = func(string) error { mu.Lock(); hooklessSteps++; mu.Unlock(); return nil }
		}, baseline)
		testassert.NoError(t, berr)
		testassert.Require(t, bres[0].Status == model.ResultSuccess, "hookless tool result = %+v, want the valid call settled", bres[0])
		mu.Lock()
		testassert.Require(t, repairSteps > 0 && repairSteps == hooklessSteps,
			"durable mutation steps: repaired pure chain %d, hookless valid settlement %d, want equal counts", repairSteps, hooklessSteps)
		mu.Unlock()
	})
}

// TestArgumentHookDeclaredKnowledgeRows proves the shared hook-knowledge rule
// on the argument family: declared not_started and unknown knowledge reach the
// durable hook_result unchanged through the binding failure, and an invalid
// declaration rejects at persistence, retains the committed hook intent with
// its pending call, and is settled unknown only by recovery.
func TestArgumentHookDeclaredKnowledgeRows(t *testing.T) {
	for _, knowledge := range []ExecutionEvidence{ExecutionNotStarted, ExecutionUnknown} {
		t.Run("declared "+string(knowledge), func(t *testing.T) {
			cause := errors.New("argument binding broke")
			hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return nil, &EffectError{Evidence: knowledge, Err: cause}
			}}
			exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
			_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
			testassert.NoError(t, err) // the settled error is a result, not a pipeline failure
			committed := hookGraphHookResults(t, store, sessionID)
			testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Evidence == knowledge,
				"hook results = %+v, want the declared %s knowledge unchanged", committed, knowledge)
			wantContent, wantEvidence := "argument binding broke", ExecutionNotStarted
			if knowledge == ExecutionUnknown {
				wantContent = "Pre-tool hook outcome unknown; the hook may have taken effect. The tool was not executed.\n\nargument binding broke"
			}
			testassert.Require(t, results[0].Status == model.ResultError && results[0].Content == wantContent && results[0].Evidence == wantEvidence,
				"tool result = %+v, want the %s hook knowledge rendered into content %q with concrete %s evidence", results[0], knowledge, wantContent, wantEvidence)
		})
	}
	t.Run("invalid declaration retains the intent for recovery", func(t *testing.T) {
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			return nil, &EffectError{Evidence: ExecutionEvidence("all"), Err: errors.New("argument binding broke")}
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		_, store, _, sessionID, err := freshExecutionRunConfig(t, context.Background(), testCapture(), exec, nil, testToolCall("call-1"))
		testassert.Require(t, errors.Is(err, ErrInvalid), "tool effect = %v, want the persistence rejection of the invalid declaration", err)
		h := newTestHarness(t, store, nil)
		rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, rerr)
		testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil && rec.State.ActiveEffect.Kind == EffectHook &&
			len(rec.State.PendingToolCalls) == 1, "operation state = %+v, want the retained hook intent with its pending call", rec.State)
		{
			err := Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted && committed[0].Evidence == ExecutionUnknown,
			"recovered hook results = %+v, want recovery settling the retained effect as unknown", committed)
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})
}

// TestRequestHookInvalidOpenedExecutionRejectsHooks closes the opened
// invocation's request-hook contract: every request hook requires a non-empty
// unique ID, a non-empty extension identity, a valid mode, and a non-nil Run;
// an invalid invocation closes before the rejection and settles the ordinary
// failure.
func TestRequestHookInvalidOpenedExecutionRejectsHooks(t *testing.T) {
	closed := 0
	for _, tc := range []struct {
		name  string
		hooks []RequestHook
	}{
		{"empty id", []RequestHook{{Run: func(context.Context, RequestHookInput) (RequestConsequence, error) { return RequestConsequence{}, nil }}}},
		{"nil run", []RequestHook{{ID: "req.hook.one"}}},
		{"duplicate id", []RequestHook{
			{ID: "req.hook.one", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) { return RequestConsequence{}, nil }},
			{ID: "req.hook.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) { return RequestConsequence{}, nil }},
		}},
		{"empty extension identity", []RequestHook{{ID: "req.hook.one", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) { return RequestConsequence{}, nil }}}},
		{"invalid mode", []RequestHook{{ID: "req.hook.one", ExtensionID: "req-ext", Mode: "observer", Run: func(context.Context, RequestHookInput) (RequestConsequence, error) { return RequestConsequence{}, nil }}}},
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
						RequestHooks:  tc.hooks,
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

// TestRequestHookEmptyErrorTextSettlesWithGenericDetail proves the terminal
// detail floor through the full execution pipeline: a hook failure whose own
// error text is empty still settles exactly one failure terminal carrying the
// fixed nonempty standard detail, while the pipeline returns the original
// (empty-text) cause verbatim.
func TestRequestHookEmptyErrorTextSettlesWithGenericDetail(t *testing.T) {
	empty := errors.New("")
	hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		return RequestConsequence{}, empty
	}}
	h, store, c, sessionID, prepared, cancel := newExecutionHarness(t, func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("a failed request hook dispatched a conversation request")
		return nil, nil
	}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
	defer cancel()
	prepared.Open = func(_ context.Context, _ OperationAdmission) (Execution, error) {
		return requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("a failed request hook dispatched a conversation request")
			return nil, nil
		}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, hook), nil
	}
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, err != nil && err.Error() == "", "execute = %v, want the original empty-text cause verbatim", err)
	}
	settlement := graphSettlementOf(t, store, sessionID)
	testassert.Require(t, settlement.Status == OperationFailure && settlement.Detail == genericExecutionFailureDetail,
		"settlement = %+v, want the failure settled with the fixed nonempty standard detail", settlement)
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].Status == hookFailed && committed[0].Error == genericExecutionFailureDetail,
		"hook results = %+v, want the hook's own failure settled with the same standard detail", committed)
}

func TestRequestHookFailureAfterCancellationExecutesAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cause := fmt.Errorf("hook failure: %w", context.Canceled)
	hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		cancel()
		return RequestConsequence{}, cause
	}}
	h, store, c, sessionID, prepared, cleanup := newExecutionHarness(t, func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("failed hook dispatched model")
		return nil, nil
	}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
	defer cleanup()
	prepared.Open = func(context.Context, OperationAdmission) (Execution, error) {
		return requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("failed hook dispatched model")
			return nil, nil
		}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, hook), nil
	}
	err := executeDirect(h, c, ctx, testOpID, prepared)
	if err != cause {
		t.Fatalf("execute error = %v, want original %v", err, cause)
	}
	settlement := graphSettlementOf(t, store, sessionID)
	if settlement.Status != OperationFailure || settlement.Detail != cause.Error() {
		t.Fatalf("settlement = %+v, want failure with original cause", settlement)
	}
	committed := hookGraphHookResults(t, store, sessionID)
	if len(committed) != 1 || committed[0].Status != hookFailed {
		t.Fatalf("hook results = %+v, want failure", committed)
	}
}

func TestRequestHookOutcomesThroughExecute(t *testing.T) {
	for _, mode := range []HookMode{HookModePure, HookModeEffectful} {
		for _, interrupted := range []bool{false, true} {
			for _, cancelDuring := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/interrupted=%t/canceled=%t", mode, interrupted, cancelDuring), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					cause := &EffectError{Evidence: ExecutionUnknown, Err: context.Canceled, Interrupted: interrupted}
					calls, models := 0, 0
					h, store, c, sessionID, prepared, stop := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
						hook := RequestHook{ID: "req.hook", ExtensionID: "req-ext", Mode: mode, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
							calls++
							if cancelDuring {
								cancel()
							}
							return RequestConsequence{}, cause
						}}
						return requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
							models++
							return completedTurnStream(), nil
						}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, hook), nil
					})
					defer stop()
					err := executeDirect(h, c, ctx, testOpID, prepared)
					want := OperationFailure
					if interrupted {
						want = OperationInterruption
					}
					if (!interrupted && err != cause) || (interrupted && err != nil) || calls != 1 || models != 0 {
						t.Fatalf("execute=%v calls=%d model=%d", err, calls, models)
					}
					settlement := graphSettlementOf(t, store, sessionID)
					if settlement.Status != want {
						t.Fatalf("settlement=%+v want %s", settlement, want)
					}
					hooks := hookGraphHookResults(t, store, sessionID)
					if mode == HookModePure {
						if len(hooks) != 0 {
							t.Fatalf("pure hook recorded effect: %+v", hooks)
						}
					} else {
						wantHook := hookFailed
						if interrupted {
							wantHook = hookInterrupted
						}
						if len(hooks) != 1 || hooks[0].Status != wantHook || hooks[0].Evidence != ExecutionUnknown {
							t.Fatalf("hooks=%+v", hooks)
						}
					}
				})
			}
		}
	}
}

// TestArgumentHookCancellationKnowledgeRows keeps status independent of evidence.
func TestArgumentHookCancellationKnowledgeRows(t *testing.T) {
	runCancellation := func(t *testing.T, knowledge ExecutionEvidence, cause error, typed, interrupted bool) {
		var cancel context.CancelFunc
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			cancel() // the callback was already entered: the execution ends cooperatively
			if typed {
				return nil, &EffectError{Evidence: knowledge, Err: cause, Interrupted: interrupted}
			}
			return nil, cause
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		ctx, cancelFn := context.WithCancel(context.Background())
		cancel = cancelFn
		defer cancelFn()
		_, store, _, sessionID, results, err := freshExecutionToolCallsCtx(t, ctx, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		want := model.ResultError
		if interrupted {
			want = model.ResultInterrupted
		}
		testassert.Require(t, results[0].Status == want, "tool result = %+v, want %s", results[0], want)
		committed := hookGraphHookResults(t, store, sessionID)
		wantHook := hookFailed
		if interrupted {
			wantHook = hookInterrupted
		}
		testassert.Require(t, len(committed) == 1 && committed[0].Status == wantHook && committed[0].Evidence == knowledge,
			"hook results = %+v, want %s with declared knowledge %s", committed, wantHook, knowledge)
		{
			if _, err := validateFixture(t, store, sessionID); err != nil {
				t.Fatalf("graph validation: %v", err)
			}
		}
	}
	for _, knowledge := range []ExecutionEvidence{ExecutionNotStarted, ExecutionUnknown, ExecutionObserved} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/interrupted=%t", knowledge, interrupted), func(t *testing.T) {
				runCancellation(t, knowledge, context.Canceled, true, interrupted)
			})
		}
	}
	t.Run("bare cancellation is failure", func(t *testing.T) { runCancellation(t, ExecutionObserved, context.Canceled, false, false) })
	t.Run("cancellation between the intent and the callback settles not_started", func(t *testing.T) {
		ran := 0
		hook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
			ran++
			return json.RawMessage(`{"x":1}`), nil
		}}
		exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, store, _, sessionID, results, err := freshExecutionToolCallsConfig(t, ctx, testCapture(), exec, func(store *graphStorage) {
			failEffectRegister(store,
				func(ae *ActiveEffect) bool { return ae.Kind == EffectHook && ae.HookID == "cap.hook.one" },
				false, nil, cancel) // the intent commits, then the execution is canceled before the callback starts
		}, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultInterrupted, "tool result = %+v, want the settled interruption", results[0])
		testassert.Equal(t, ran, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted && committed[0].Evidence == ExecutionNotStarted,
			"hook results = %+v, want the provably unstarted interrupted effect", committed)
	})
}

func TestPureArgumentHookFailureDoesNotCreateUncertainty(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			hook := ToolArgumentsHook{ID: "pure", ExtensionID: "ext", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return nil, &EffectError{Evidence: ExecutionUnknown, Err: context.Canceled, Interrupted: interrupted}
			}}
			exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, objectNormalize, hook)
			_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
			want := model.ResultError
			if interrupted {
				want = model.ResultInterrupted
			}
			if err != nil || results[0].Status != want || strings.Contains(results[0].Content, unknownHookToolResultPrefix) {
				t.Fatalf("result=%+v error=%v", results[0], err)
			}
			if hooks := hookGraphHookResults(t, store, sessionID); len(hooks) != 0 {
				t.Fatalf("pure hook published evidence: %+v", hooks)
			}
		})
	}
}

// TestRequestHookCancellationKnowledgeRows keeps request status and knowledge independent.
func TestRequestHookCancellationKnowledgeRows(t *testing.T) {
	for _, tc := range []struct {
		name        string
		knowledge   ExecutionEvidence
		cause       error
		typed       bool
		interrupted bool
	}{
		{"not started failure", ExecutionNotStarted, context.Canceled, true, false},
		{"not started interrupted", ExecutionNotStarted, context.Canceled, true, true},
		{"unknown failure", ExecutionUnknown, context.Canceled, true, false},
		{"unknown interrupted", ExecutionUnknown, context.Canceled, true, true},
		{"observed failure", ExecutionObserved, context.Canceled, true, false},
		{"observed interrupted", ExecutionObserved, context.Canceled, true, true},
		{"bare cancellation is failure", ExecutionObserved, context.Canceled, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				cancel() // the callback was already entered: the execution ends cooperatively
				if tc.typed {
					return RequestConsequence{}, &EffectError{Evidence: tc.knowledge, Err: tc.cause, Interrupted: tc.interrupted}
				}
				return RequestConsequence{}, tc.cause
			}}
			exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
				t.Fatal("a canceled request hook dispatched a conversation request")
				return nil, nil
			}, nil, hook)
			h, store, c, sessionID := newHookHarness(t, exec)
			_, err := runAdmittedExecution(t, h, c, ctx, testOpID, testCapture(), exec)
			if tc.interrupted {
				testassert.Require(t, err == nil, "execution = %v, want the settled interruption", err)
			} else {
				testassert.Require(t, errors.Is(err, tc.cause), "execution = %v, want the original cause", err)
			}
			committed := hookGraphHookResults(t, store, sessionID)
			want := hookFailed
			if tc.interrupted {
				want = hookInterrupted
			}
			testassert.Require(t, len(committed) == 1 && committed[0].Status == want && committed[0].Evidence == tc.knowledge,
				"hook results = %+v, want %s carrying knowledge %s", committed, want, tc.knowledge)
			{
				if _, err := validateFixture(t, store, sessionID); err != nil {
					t.Fatalf("graph validation: %v", err)
				}
			}
		})
	}
	t.Run("cancellation between the intent and the callback settles not_started", func(t *testing.T) {
		ran := 0
		hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			ran++
			return RequestConsequence{}, nil
		}}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			t.Fatal("a canceled request hook dispatched a conversation request")
			return nil, nil
		}, nil, hook)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, store, _, sessionID, err := freshExecutionRunConfig(t, ctx, testCapture(), exec, func(store *graphStorage) {
			failEffectRegister(store,
				func(ae *ActiveEffect) bool {
					return ae.Kind == EffectHook && ae.HookID == "req.effectful" && ae.RequestID != ""
				},
				false, nil, cancel) // the intent commits, then the execution is canceled before the callback starts
		})
		testassert.Require(t, err == nil, "execution = %v, want the settled interruption", err)
		testassert.Equal(t, ran, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].Status == hookInterrupted && committed[0].Evidence == ExecutionNotStarted,
			"hook results = %+v, want the provably unstarted interrupted effect", committed)
	})
}

// TestArgumentHookAcceptedSelectionSurvivesEffectExits proves that the chain's
// accepted normalized value and ordered provenance survive concrete-effect
// exits before execution: an invalid prepared plan settles the call carrying
// the accepted selection, and a cancellation-aborted tool effect intent settles
// the call interrupted carrying the same selection without a started effect
// while the terminal helper interrupts the unstarted sibling as not_started
// and leaves the sibling's chain uncommitted.
func TestArgumentHookAcceptedSelectionSurvivesEffectExits(t *testing.T) {
	chainHooks := func() []ToolArgumentsHook {
		return []ToolArgumentsHook{
			{ID: "cap.pure.repair", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":9}`), nil
			}},
			{ID: "cap.effectful.replace", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":7}`), nil
			}},
		}
	}
	assertAccepted := func(t *testing.T, store *graphStorage, sessionID string) {
		t.Helper()
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settled := 0
		for _, entry := range graph.Entries {
			if entry.ToolResult == nil {
				continue
			}
			settled++
			testassert.Require(t, string(entry.ToolResult.NormalizedArguments) == `{"x":7}` &&
				len(entry.ToolResult.HookIDs) == 2 && entry.ToolResult.HookIDs[0].HookID == "cap.pure.repair" && entry.ToolResult.HookIDs[1].HookID == "cap.effectful.replace",
				"tool result provenance = %+v %s, want the accepted value and the ordered pure-then-effectful chain", entry.ToolResult.NormalizedArguments, entry.ToolResult.HookIDs)
		}
		testassert.Require(t, settled >= 1, "no committed tool result carried the accepted selection")
	}
	t.Run("an invalid prepared plan settles with the accepted selection", func(t *testing.T) {
		spy := &hookToolSpy{plan: func(model.ToolCall) PreparedTool { return PreparedTool{} }} // shape-invalid: neither immediate nor executable
		exec := hookFixtureExecution(spy.tool, objectNormalize, chainHooks()...)
		_, store, _, sessionID, results, err := freshExecutionToolCalls(t, testCapture(), exec, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultError, "tool result = %+v, want the invalid-plan settlement", results[0])
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 1 && string(prepared[0].Arguments) == `{"x":7}`, "preparer received %+v, want the chain's accepted value", prepared)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "cap.effectful.replace" && committed[0].Status == hookSucceeded,
			"hook results = %+v, want the settled chain effect", committed)
		assertAccepted(t, store, sessionID)
	})
	t.Run("a cancellation-aborted tool effect intent settles carrying the accepted selection", func(t *testing.T) {
		executed := 0
		spy := &hookToolSpy{plan: func(call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				executed++
				return ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
			}}
		}}
		exec := hookFixtureExecution(spy.tool, objectNormalize, chainHooks()...)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, store, _, sessionID, results, err := freshExecutionToolCallsConfig(t, ctx, testCapture(), exec, func(store *graphStorage) {
			failEffectRegister(store,
				func(ae *ActiveEffect) bool { return ae.Kind == EffectTool && ae.ToolCallID == "call-1" },
				false, nil, cancel) // cancellation lands on the concrete tool effect intent's own lifetime
		}, testToolCall("call-1"), testToolCall("call-2"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultInterrupted && string(results[0].NormalizedArguments) == `{"x":7}` &&
			len(results[0].HookIDs) == 2 && results[0].HookIDs[0].HookID == "cap.pure.repair" && results[0].HookIDs[1].HookID == "cap.effectful.replace",
			"tool result = %+v, want the settled interruption carrying the accepted selection and ordered provenance", results[0])
		testassert.Require(t, results[1].Status == model.ResultInterrupted && results[1].Evidence == ExecutionNotStarted,
			"sibling tool result = %+v, want the terminal helper's proven-unstarted interruption", results[1])
		testassert.Equal(t, executed, 0)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "cap.effectful.replace" && committed[0].Status == hookSucceeded,
			"hook results = %+v, want the settled chain effect of the interrupted call only", committed)
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 1 && string(prepared[0].Arguments) == `{"x":7}`,
			"preparer received %+v, want the accepted value for the call that reached preparation", prepared)
	})
}

// TestRequestHookRunningRequestHookResults proves the running-request state
// through the actual pipeline: after two committed request-hook effects, an
// aborted model effect intent leaves the published request state recording
// both hook results in exact committed order with full session identity, and
// recovery witnesses the retained request settling the operation with those
// effects kept as their own durable rows.
func TestRequestHookRunningRequestHookResults(t *testing.T) {
	e1 := RequestHook{ID: "req.effectful.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		prompt := "first effect"
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	e2 := RequestHook{ID: "req.effectful.two", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		prompt := "second effect"
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("the aborted model effect dispatched a conversation request")
		return nil, nil
	}, nil, e1, e2)
	h, store, c, sessionID := newHookHarness(t, exec)
	store.replaceHook = func(key RegisterKey, payload json.RawMessage) error {
		if key.Kind != RegisterOperation {
			return nil
		}
		op, derr := decodeOperationRegister(Register{Key: key, Payload: payload})
		// The model effect intent transaction that follows both committed hook
		// results aborts; the hook effects themselves stay durable.
		if derr == nil && op.State.ActiveEffect != nil && op.State.ActiveEffect.Kind == EffectModel &&
			op.State.Request != nil && len(op.State.Request.HookResults) == 2 {
			return fmt.Errorf("%w: injected model intent failure", ErrStorage)
		}
		return nil
	}
	{
		_, err := runRequestHookRound(t, h, c, exec, testCapture())
		testassert.Require(t, errors.Is(err, ErrStorage), "pipeline = %v, want the injected storage failure verbatim", err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.Request != nil && rec.State.ActiveEffect == nil,
		"operation state = %+v, want the published request retained without a model effect", rec.State)
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 2 && committed[0].HookID == "req.effectful.one" && committed[1].HookID == "req.effectful.two",
		"hook results = %+v, want both effects in committed order", committed)
	testassert.Require(t, len(rec.State.Request.HookResults) == 2 && rec.State.Request.RequestID != "",
		"running request state = %+v, want both committed hook results recorded", rec.State.Request)
	for i, ref := range rec.State.Request.HookResults {
		testassert.Require(t, ref.SessionID == sessionID && ref.EntryID == committed[i].EntryID,
			"hook result reference %d = %+v, want the full SessionID and identity of committed effect %s", i, ref, committed[i].EntryID)
	}
	{
		err := Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, graph.Operations[0].State.Status == OperationInterruption && graph.Operations[0].State.Request == nil,
		"recovered operation = %+v, want the settled interruption clearing the request state", graph.Operations[0].State)
	settlement := graphSettlementOf(t, store, sessionID)
	testassert.Require(t, settlement.RequestID == rec.State.Request.RequestID,
		"settlement = %+v, want the retained request identity witnessed once", settlement)
	after := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(after) == 2, "hook results after recovery = %+v, want the two committed effects kept", after)
}

// TestArgumentHookPureRestoreMatchesBaselineTransactions counts the actual
// transaction mutation steps: the pure repair-and-restore chain settles the
// call through exactly the same durable steps as the identical hookless call
// — no hook effects, no extra register writes.
func TestArgumentHookPureRestoreMatchesBaselineTransactions(t *testing.T) {
	driveArgs := func(t *testing.T, hooks ...ToolArgumentsHook) (int, int) {
		spy := &hookToolSpy{}
		exec := hookFixtureExecution(spy.tool, objectNormalize, hooks...)
		steps := 0
		_, store, _, sessionID, results, err := freshExecutionToolCallsConfig(t, context.Background(), testCapture(), exec, func(store *graphStorage) {
			store.txHook = func(string) error { steps++; return nil }
		}, testToolCall("call-1"))
		testassert.NoError(t, err)
		testassert.Require(t, results[0].Status == model.ResultSuccess, "tool result = %+v, want the call run", results[0])
		return steps, len(hookGraphHookResults(t, store, sessionID))
	}
	baseSteps, baseHooks := driveArgs(t)
	var capturedOriginal json.RawMessage
	pureA := ToolArgumentsHook{ID: "cap.pure.one", ExtensionID: "cap-a", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		capturedOriginal = model.CloneRaw(call.Arguments)
		return json.RawMessage(`{"x":9}`), nil
	}}
	pureB := ToolArgumentsHook{ID: "cap.pure.two", ExtensionID: "cap-a", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		return model.CloneRaw(capturedOriginal), nil
	}}
	pureSteps, pureHooks := driveArgs(t, pureA, pureB)
	testassert.Require(t, baseHooks == 0 && pureHooks == 0, "hook entries = baseline %d pure %d, want none", baseHooks, pureHooks)
	testassert.Require(t, pureSteps == baseSteps, "pure restore chain performed %d durable mutation steps, want the baseline %d", pureSteps, baseSteps)
}

// TestRequestHookCodecAdoptedValueContinuesTheLiveView proves the continuation
// rule on the request boundary: a pure contribution whose raw prompt bytes are
// not canonical text reaches the effectful hook only through the codec-adopted
// committed evidence — never resurrected raw bytes — and every later pure
// hook, the continuing view, the durable rows, and the actual dispatch all
// carry the same adopted value.
func TestRequestHookCodecAdoptedValueContinuesTheLiveView(t *testing.T) {
	rawPrompt := "bad \xff bytes here" // a raw prompt carrying invalid UTF-8
	var effectfulSaw string
	var nextPureSaw string
	pure := RequestHook{ID: "req.pure", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		adopted := rawPrompt
		return RequestConsequence{Prompt: &adopted}, nil
	}}
	noop := RequestHook{ID: "req.effectful.noop", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		effectfulSaw = in.Prompt
		reasserted := in.Prompt
		return RequestConsequence{Prompt: &reasserted}, nil
	}}
	final := RequestHook{ID: "req.pure.final", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		nextPureSaw = in.Prompt
		return RequestConsequence{}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}, nil, pure, noop, final)
	h, store, c, sessionID := newHookHarness(t, exec)
	{
		sent, err := runRequestHookRound(t, h, c, exec, testCapture())
		testassert.NoError(t, err)
		dispatchPrompt := ""
		for _, m := range sent.Messages {
			if m.Role == model.RoleSystem {
				dispatchPrompt = m.TextContent()
			}
		}
		// The effectful hook received the codec-adopted committed evidence,
		// the continuing view never resurrects the raw bytes, and the model
		// dispatch carries the adopted text.
		testassert.Require(t, effectfulSaw != rawPrompt && strings.Contains(effectfulSaw, "\ufffd"),
			"effectful input prompt = %q, want the codec-adopted replacement of the invalid raw bytes %q", effectfulSaw, rawPrompt)
		testassert.Require(t, nextPureSaw == effectfulSaw && dispatchPrompt == effectfulSaw,
			"continuing view = %q dispatch = %q, want the adopted value %q everywhere after the effect boundary", nextPureSaw, dispatchPrompt, effectfulSaw)
	}
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].Status == hookSucceeded &&
		committed[0].InputEvidence != nil && committed[0].InputEvidence.Prompt == effectfulSaw &&
		committed[0].Consequence != nil && committed[0].Consequence.Prompt != nil && *committed[0].Consequence.Prompt == effectfulSaw,
		"hook results = %+v, want the durable rows carrying the adopted text", committed)
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && assistant.Evidence.Prompt == effectfulSaw,
		"assistant evidence prompt = %v, want the adopted final effective value", assistant.Evidence.Prompt)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestRequestHookEmptyToolSelectionStaysHidden proves the present-empty view
// through the effect boundary: a pure hook hiding every advertised tool hands
// the effectful hook an empty tool view from its committed consumed evidence,
// the hiding survives into the later pure hook and the actual dispatch as the
// recorded no-advertisement selection, and the pure hook itself wrote nothing
// durable.
func TestRequestHookEmptyToolSelectionStaysHidden(t *testing.T) {
	var effectfulTools int
	var nextPureTools int
	hide := RequestHook{ID: "req.pure.hide", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		hidden := []string{}
		return RequestConsequence{ToolNames: hidden}, nil
	}}
	noop := RequestHook{ID: "req.effectful.noop", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		effectfulTools = len(in.Tools)
		return RequestConsequence{}, nil
	}}
	final := RequestHook{ID: "req.pure.final", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		nextPureTools = len(in.Tools)
		return RequestConsequence{}, nil
	}}
	exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}, nil, hide, noop, final)
	h, store, c, sessionID := newHookHarness(t, exec)
	{
		sent, err := runRequestHookRound(t, h, c, exec, testCapture())
		testassert.NoError(t, err)
		testassert.Require(t, len(sent.Tools) == 0, "dispatch advertises %d tools, want the recorded no-advertisement selection", len(sent.Tools))
	}
	testassert.Require(t, effectfulTools == 0, "effectful hook received %d tools, want none after the pure hiding", effectfulTools)
	testassert.Require(t, nextPureTools == 0, "later pure hook received %d tools, want the hiding continuing", nextPureTools)
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.effectful.noop" &&
		committed[0].InputEvidence != nil && committed[0].InputEvidence.ToolNames != nil && len(committed[0].InputEvidence.ToolNames) == 0,
		"hook results = %+v, want the present-empty selection recorded as consumed", committed)
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && assistant.Evidence.ToolNames != nil && len(assistant.Evidence.ToolNames) == 0,
		"assistant evidence = %v, want the recorded empty view carried to the boundary", assistant.Evidence)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestRequestHookPureContributorsFollowEffectiveChange proves the settled
// request provenance policy through the actual pipeline: every successfully
// accepted pure or effectful hook identity is recorded in configured order,
// including reassertions and no-ops; whole-baseline restoration keeps the
// identities while the recorded value deltas return to the frozen capture;
// and no per-dimension last-setter attribution survives anywhere.
func TestRequestHookPureContributorsFollowEffectiveChange(t *testing.T) {
	capture := testCapture()
	basePrompt := capture.SystemPrompt
	fullNames := make([]string, 0, len(capture.Tools))
	for _, tool := range capture.Tools {
		fullNames = append(fullNames, tool.Name)
	}
	drive := func(t *testing.T, hooks ...RequestHook) (*RequestEvidence, model.Request) {
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil, hooks...)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			sent, err := runRequestHookRound(t, h, c, exec, capture)
			testassert.NoError(t, err)
			return compositionAssistant(t, store, sessionID).Evidence, sent
		}
	}
	promptOf := func(req model.Request) string {
		for _, m := range req.Messages {
			if m.Role == model.RoleSystem {
				return m.TextContent()
			}
		}
		return ""
	}
	hook := func(id string, run func(context.Context, RequestHookInput) (RequestConsequence, error)) RequestHook {
		return RequestHook{ID: id, ExtensionID: "req-ext", Mode: HookModePure, Run: run}
	}
	t.Run("sequential appends both contribute", func(t *testing.T) {
		var first string
		ev, sent := drive(t,
			hook("req.pure.one", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				changed := in.Prompt + " one"
				first = changed
				return RequestConsequence{Prompt: &changed}, nil
			}),
			hook("req.pure.two", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				testassert.Equal(t, in.Prompt, first)
				changed := in.Prompt + " two"
				return RequestConsequence{Prompt: &changed}, nil
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 2 && ev.HookIDs[0].HookID == "req.pure.one" && ev.HookIDs[1].HookID == "req.pure.two",
			"contributions = %+v, want both appenders in hook order", ev.HookIDs)
		testassert.Require(t, ev.Prompt == basePrompt+" one two" && promptOf(sent) == basePrompt+" one two",
			"prompt delta = %v dispatch = %q, want the whole accumulated replacement", ev.Prompt, promptOf(sent))
	})
	t.Run("reassertion after a change is also a contributor", func(t *testing.T) {
		ev, sent := drive(t,
			hook("req.pure.one", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				changed := in.Prompt + " one"
				return RequestConsequence{Prompt: &changed}, nil
			}),
			hook("req.pure.reassert", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				reasserted := in.Prompt // the exact value the hook received: no effective change
				return RequestConsequence{Prompt: &reasserted}, nil
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 2 && ev.HookIDs[0].HookID == "req.pure.one" && ev.HookIDs[1].HookID == "req.pure.reassert",
			"contributions = %+v, want both accepted hooks in configured order", ev.HookIDs)
		testassert.Require(t, promptOf(sent) == basePrompt+" one", "dispatch = %q, want the changed value", promptOf(sent))
	})
	t.Run("pure no-op before a changed hook is also a contributor", func(t *testing.T) {
		ev, _ := drive(t,
			hook("req.pure.noop", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				same := in.Prompt
				return RequestConsequence{Prompt: &same}, nil
			}),
			hook("req.pure.change", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				changed := in.Prompt + " tail"
				return RequestConsequence{Prompt: &changed}, nil
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 2 && ev.HookIDs[0].HookID == "req.pure.noop" && ev.HookIDs[1].HookID == "req.pure.change",
			"contributions = %+v, want every accepted hook in configured order", ev.HookIDs)
	})
	t.Run("change then restore then later change keeps every accepted contributor", func(t *testing.T) {
		var original string
		ev, sent := drive(t,
			hook("req.pure.change", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				original = in.Prompt
				changed := in.Prompt + " mid"
				return RequestConsequence{Prompt: &changed}, nil
			}),
			hook("req.pure.restore", func(context.Context, RequestHookInput) (RequestConsequence, error) {
				restored := original // the entire frozen prompt returns
				return RequestConsequence{Prompt: &restored}, nil
			}),
			hook("req.pure.later", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				changed := in.Prompt + " later"
				return RequestConsequence{Prompt: &changed}, nil
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 3 && ev.HookIDs[0].HookID == "req.pure.change" && ev.HookIDs[1].HookID == "req.pure.restore" && ev.HookIDs[2].HookID == "req.pure.later",
			"contributions = %+v, want every accepted hook in configured order", ev.HookIDs)
		testassert.Require(t, promptOf(sent) == original+" later", "dispatch = %q, want restoration then the later change", promptOf(sent))
	})
	t.Run("mixed prompt and tool changes keep intermediate contributors", func(t *testing.T) {
		ev, sent := drive(t,
			hook("req.pure.prompt", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
				changed := in.Prompt + " prompt"
				return RequestConsequence{Prompt: &changed}, nil
			}),
			hook("req.pure.hide", func(context.Context, RequestHookInput) (RequestConsequence, error) {
				hidden := []string{}
				return RequestConsequence{ToolNames: hidden}, nil
			}),
			hook("req.pure.show", func(context.Context, RequestHookInput) (RequestConsequence, error) {
				shown := append([]string(nil), fullNames...) // the full catalog selection: a genuine change from hidden
				return RequestConsequence{ToolNames: shown}, nil
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 3 && ev.HookIDs[0].HookID == "req.pure.prompt" &&
			ev.HookIDs[1].HookID == "req.pure.hide" && ev.HookIDs[2].HookID == "req.pure.show",
			"contributions = %+v, want every hook that actually changed the live view, in hook order", ev.HookIDs)
		testassert.Require(t, reflect.DeepEqual(ev.ToolNames, fullNames), "names = %v, want the full catalog", ev.ToolNames)
		testassert.Require(t, len(sent.Tools) == len(capture.Tools), "dispatch advertises %d tools, want the restored full catalog", len(sent.Tools))
	})
	t.Run("hide-all then full baseline restoration keeps identities with no deltas", func(t *testing.T) {
		ev, sent := drive(t,
			hook("req.pure.hide", func(context.Context, RequestHookInput) (RequestConsequence, error) {
				hidden := []string{}
				return RequestConsequence{ToolNames: hidden}, nil
			}),
			hook("req.pure.restore", func(context.Context, RequestHookInput) (RequestConsequence, error) {
				shown := append([]string(nil), fullNames...)
				prompt := basePrompt
				return RequestConsequence{Prompt: &prompt, ToolNames: shown}, nil // the entire frozen baseline returns
			}),
		)
		testassert.Require(t, len(ev.HookIDs) == 2 && ev.HookIDs[0].HookID == "req.pure.hide" && ev.HookIDs[1].HookID == "req.pure.restore",
			"contributions = %+v, want both accepted hooks kept in configured order", ev.HookIDs)
		testassert.Require(t, ev.Prompt == basePrompt && reflect.DeepEqual(ev.ToolNames, fullNames), "evidence = prompt %v names %v, want full captured view", ev.Prompt, ev.ToolNames)
		testassert.Require(t, promptOf(sent) == basePrompt && len(sent.Tools) == len(capture.Tools), "dispatch = %q with %d tools, want the unchanged baseline view", promptOf(sent), len(sent.Tools))
	})
	t.Run("effectful no-op keeps the pure intermediate and its own fact", func(t *testing.T) {
		pure := hook("req.pure.one", func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
			changed := in.Prompt + " one"
			return RequestConsequence{Prompt: &changed}, nil
		})
		noop := RequestHook{ID: "req.effectful.noop", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
			reasserted := in.Prompt
			return RequestConsequence{Prompt: &reasserted}, nil
		}}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil, pure, noop)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			sent, err := runRequestHookRound(t, h, c, exec, capture)
			testassert.NoError(t, err)
			testassert.Require(t, promptOf(sent) == basePrompt+" one", "dispatch = %q, want the pure intermediate adopted by the no-op", promptOf(sent))
		}
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.effectful.noop" && committed[0].Status == hookSucceeded &&
			committed[0].Consequence != nil && committed[0].Consequence.Prompt != nil && *committed[0].Consequence.Prompt == basePrompt+" one",
			"hook results = %+v, want the no-op's own committed fact", committed)
		testassert.Require(t, committed[0].InputEvidence != nil &&
			committed[0].InputEvidence.Prompt == basePrompt+" one" &&
			len(committed[0].InputEvidence.HookIDs) == 1 && committed[0].InputEvidence.HookIDs[0].HookID == "req.pure.one",
			"consumed evidence = %+v, want the pure intermediate provenance at the effect boundary", committed[0].InputEvidence)
		assistant := compositionAssistant(t, store, sessionID)
		testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.HookIDs) == 2 &&
			assistant.Evidence.HookIDs[0].HookID == "req.pure.one" && assistant.Evidence.HookIDs[1].HookID == "req.effectful.noop",
			"assistant contributions = %+v, want the changed pure hook and the effectful no-op in order", assistant.Evidence.HookIDs)
	})
}

// TestRequestHookInputFrozenDomainsCarryHiddenCatalogAndRestoresThroughCatalogNames
// proves the frozen input domains of RequestHookInput through the actual
// ordered pipeline for both successor modes: a pure hook changes the prompt,
// adds a role-indistinguishable user message, and hides every tool; the
// successor identifies exactly the round-frozen canonical messages
// independently of the effective Messages, sees the captured prompt
// independently of the current Prompt, and discovers the hidden eligible
// tool names by iterating the captured Catalog — then restores exactly those
// names. A third hook mutates the successor's owned CanonicalMessages content
// and Catalog parameter bytes; the barrier proves neither mutation reaches the
// live view, a later hook, the model dispatch, or the captured catalog, and
// the dispatch still carries the full restored catalog and the addition
// exactly once.
func TestRequestHookInputFrozenDomainsCarryHiddenCatalogAndRestoresThroughCatalogNames(t *testing.T) {
	capture := testCapture()
	basePrompt := capture.SystemPrompt
	fullNames := make([]string, 0, len(capture.Tools))
	for _, tool := range capture.Tools {
		fullNames = append(fullNames, tool.Name)
	}
	parametersOf := func(tools []model.ToolDefinition) []string {
		out := make([]string, 0, len(tools))
		for _, tool := range tools {
			out = append(out, string(tool.Parameters))
		}
		return out
	}
	baseParameters := parametersOf(capture.Tools)
	textsOf := func(messages []model.Message) []string {
		out := make([]string, 0, len(messages))
		for _, message := range messages {
			out = append(out, string(message.Role)+"|"+message.TextContent())
		}
		return out
	}
	namesOf := func(tools []model.ToolDefinition) []string {
		out := make([]string, 0, len(tools))
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return out
	}
	addition, aerr := model.NewMessage(model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "contributed"}}})
	testassert.NoError(t, aerr)
	var base []string
	firstChange := func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		// At the first boundary the frozen domains and the evolving view agree.
		testassert.Require(t, reflect.DeepEqual(textsOf(in.CanonicalMessages), textsOf(in.Messages)),
			"first input frozen canonical = %v effective = %v, want agreement at the opening boundary", textsOf(in.CanonicalMessages), textsOf(in.Messages))
		testassert.Equal(t, in.CapturedPrompt, basePrompt)
		testassert.Require(t, reflect.DeepEqual(namesOf(in.Catalog), namesOf(in.Tools)),
			"first catalog = %v tools = %v, want the unchanged captured view at the opening boundary", namesOf(in.Catalog), namesOf(in.Tools))
		testassert.Require(t, reflect.DeepEqual(parametersOf(in.Catalog), baseParameters), "first catalog parameters mismatch")
		base = textsOf(in.CanonicalMessages)
		prompt := in.Prompt + " modified"
		hidden := []string{}
		return RequestConsequence{Prompt: &prompt, Messages: []model.Message{addition}, ToolNames: hidden}, nil
	}
	frozenSuccessor := func(mode HookMode) RequestHook {
		return RequestHook{ID: "req.successor", ExtensionID: "req-ext", Mode: mode, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
			// The frozen canonical excludes the pure addition; the effective
			// Messages carry it.
			testassert.Require(t, reflect.DeepEqual(textsOf(in.CanonicalMessages), base), "successor frozen canonical = %v, want the round-frozen base", textsOf(in.CanonicalMessages))
			testassert.Require(t, len(in.Messages) == len(base)+1 && textsOf(in.Messages)[len(base)] == "user|contributed",
				"effective messages = %v, want the frozen canonical plus the addition", textsOf(in.Messages))
			// The captured prompt stands independently of the current prompt.
			testassert.Equal(t, in.CapturedPrompt, basePrompt)
			testassert.Equal(t, in.Prompt, basePrompt+" modified")
			// Hidden tools are discoverable only through the Catalog.
			testassert.Require(t, len(in.Tools) == 0, "visible tools = %v, want every eligible tool hidden", namesOf(in.Tools))
			testassert.Require(t, reflect.DeepEqual(namesOf(in.Catalog), fullNames), "successor catalog = %v, want the full eligible catalog while hidden", namesOf(in.Catalog))
			testassert.Require(t, reflect.DeepEqual(parametersOf(in.Catalog), baseParameters), "successor catalog parameters mismatch")
			// Mutation barrier: nested owned values the hook may scribble on.
			if len(in.CanonicalMessages) > 0 && len(in.CanonicalMessages[0].Content) > 0 {
				in.CanonicalMessages[0].Content[0] = model.ContentPart{Kind: model.PartText, Text: "MUTATED"}
			}
			if len(in.Catalog) > 0 {
				in.Catalog[0].Parameters = json.RawMessage(`{"mutated":true}`)
			}
			shown := namesOf(in.Catalog) // restored strictly from the discovered catalog
			return RequestConsequence{ToolNames: shown}, nil
		}}
	}
	barrierCheck := RequestHook{ID: "req.barrier", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		testassert.Require(t, reflect.DeepEqual(textsOf(in.CanonicalMessages), base), "barrier canonical = %v, want the mutation to have reached nothing", textsOf(in.CanonicalMessages))
		testassert.Require(t, reflect.DeepEqual(parametersOf(in.Catalog), baseParameters), "barrier catalog parameters mismatch: the mutation reached no later input")
		testassert.Require(t, reflect.DeepEqual(namesOf(in.Tools), fullNames), "barrier tools = %v, want the restored full catalog for the next boundary", namesOf(in.Tools))
		return RequestConsequence{}, nil
	}}
	promptOf := func(req model.Request) string {
		for _, m := range req.Messages {
			if m.Role == model.RoleSystem {
				return m.TextContent()
			}
		}
		return ""
	}
	t.Run("pure successor chain", func(t *testing.T) {
		hooks := []RequestHook{
			{ID: "req.first", ExtensionID: "req-ext", Mode: HookModePure, Run: firstChange},
			frozenSuccessor(HookModePure),
			barrierCheck,
		}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil, hooks...)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			sent, err := runRequestHookRound(t, h, c, exec, capture)
			testassert.NoError(t, err)
			testassert.Equal(t, promptOf(sent), basePrompt+" modified")
			contributed := 0
			conversation := make([]string, 0, len(sent.Messages))
			for _, m := range sent.Messages {
				if m.Role == model.RoleSystem { // the rendered prompt is carried apart from the canonical view
					continue
				}
				conversation = append(conversation, string(m.Role)+"|"+m.TextContent())
				if m.TextContent() == "contributed" {
					contributed++
				}
			}
			testassert.Require(t, contributed == 1 && reflect.DeepEqual(conversation, append(append([]string(nil), base...), "user|contributed")),
				"dispatch conversation = %v, want the frozen canonical with the addition exactly once", conversation)
			testassert.Require(t, reflect.DeepEqual(namesOf(sent.Tools), fullNames), "dispatch tools = %v, want the full restored catalog", namesOf(sent.Tools)) // full restored catalog
			testassert.Require(t, reflect.DeepEqual(parametersOf(sent.Tools), baseParameters), "dispatch catalog parameters mismatch")
		}
		{
			got := hookGraphHookResults(t, store, sessionID)
			testassert.Require(t, len(got) == 0, "hook results = %+v, want no pure transactions", got)
		}
		{
			assistant := compositionAssistant(t, store, sessionID)
			testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.HookIDs) == 3 &&
				assistant.Evidence.HookIDs[0].HookID == "req.first" && assistant.Evidence.HookIDs[1].HookID == "req.successor" && assistant.Evidence.HookIDs[2].HookID == "req.barrier",
				"assistant contributions = %+v, want every accepted hook in configured order (the no-op barrier included)", assistant.Evidence.HookIDs)
			testassert.Require(t, reflect.DeepEqual(assistant.Evidence.ToolNames, fullNames), "names = %v, want the full catalog", assistant.Evidence.ToolNames)
		}
		testassert.Require(t, reflect.DeepEqual(parametersOf(capture.Tools), baseParameters), "the caller's capture must stand untouched") // the caller's capture stands untouched
	})
	t.Run("effectful successor chain", func(t *testing.T) {
		hooks := []RequestHook{
			{ID: "req.first", ExtensionID: "req-ext", Mode: HookModePure, Run: firstChange},
			frozenSuccessor(HookModeEffectful),
			barrierCheck,
		}
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil, hooks...)
		h, store, c, sessionID := newHookHarness(t, exec)
		{
			sent, err := runRequestHookRound(t, h, c, exec, capture)
			testassert.NoError(t, err)
			testassert.Equal(t, promptOf(sent), basePrompt+" modified")
			contributed := 0
			conversation := make([]string, 0, len(sent.Messages))
			for _, m := range sent.Messages {
				if m.Role == model.RoleSystem { // the rendered prompt is carried apart from the canonical view
					continue
				}
				conversation = append(conversation, string(m.Role)+"|"+m.TextContent())
				if m.TextContent() == "contributed" {
					contributed++
				}
			}
			testassert.Require(t, contributed == 1 && reflect.DeepEqual(conversation, append(append([]string(nil), base...), "user|contributed")),
				"dispatch conversation = %v, want the frozen canonical with the addition exactly once", conversation)
			testassert.Require(t, reflect.DeepEqual(namesOf(sent.Tools), fullNames), "dispatch tools = %v, want the full restored catalog", namesOf(sent.Tools))
			testassert.Require(t, reflect.DeepEqual(parametersOf(sent.Tools), baseParameters), "dispatch catalog parameters mismatch")
		}
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.successor",
			"hook results = %+v, want exactly the effectful successor's own transaction", committed)
		{
			input := committed[0].InputEvidence
			testassert.Require(t, input != nil && input.Prompt == basePrompt+" modified" &&
				input.ToolNames != nil && len(input.ToolNames) == 0 && len(input.Messages) == 1,
				"consumed input evidence = %+v, want the hidden-view prompt, names, and addition the effect actually received", input)
			testassert.Require(t, len(input.HookIDs) == 1 && input.HookIDs[0].HookID == "req.first",
				"consumed provenance = %+v, want the pure contributor at the effect boundary", input.HookIDs)
		}
		testassert.Require(t, committed[0].Consequence != nil && reflect.DeepEqual(committed[0].Consequence.ToolNames, fullNames),
			"committed consequence names = %v, want the codec-adopted restored full selection", committed[0].Consequence)
		{
			assistant := compositionAssistant(t, store, sessionID)
			testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.HookIDs) == 3 &&
				assistant.Evidence.HookIDs[0].HookID == "req.first" && assistant.Evidence.HookIDs[1].HookID == "req.successor" && assistant.Evidence.HookIDs[2].HookID == "req.barrier",
				"assistant contributions = %+v, want the pure contributor, the effectful successor, and the accepted no-op barrier in configured order", assistant.Evidence.HookIDs)
		}
		testassert.Require(t, reflect.DeepEqual(parametersOf(capture.Tools), baseParameters), "the caller's capture must stand untouched")
	})
}
