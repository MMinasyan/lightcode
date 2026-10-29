// Contract coverage for publication-origin classification and origin rules at
// the pending-call effect boundaries: once an intent transaction is invoked it
// runs on a cancellation-free context, so every error from it — including a
// cancellation-shaped store error under a genuinely canceled execution — is a
// real store failure and a marked publication-origin failure that propagates
// with its original cause and the durable state retained for recovery, never
// followed by a guessed compensating write. Only a cancellation observed
// before the transaction was ever attempted may use a caller's ordinary
// interruption settlement. The same marking owns the result publications and
// their ErrNotFound guards.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// publicationFailureCauses are the two shapes one invoked publication
// transaction can return: an ordinary storage failure and a cancellation-
// shaped store error under a live execution.
func publicationFailureCauses() []error {
	return []error{
		fmt.Errorf("%w: store down", ErrStorage),
		fmt.Errorf("publication aborted mid-transaction: %w", context.Canceled),
	}
}

// executorTurnFixture builds one executor tool turn: round one publishes
// call-1, round two ends the turn, and the tool plan runs through the
// executor path that commits the pending-call effect intent.
func executorTurnFixture(t *testing.T, hook bool) (*Harness, *graphStorage, *coordinator, string, PreparedExecution, context.CancelFunc, *int, *int, *int) {
	t.Helper()
	var rounds, preparedCalls, executed int
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		rounds++
		if rounds == 1 {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	planFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		preparedCalls++
		return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
			executed++
			return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
		}}
	}
	if !hook {
		h, store, c, sessionID, prepared, cancel := newExecutionHarness(t, modelFn, planFn)
		return h, store, c, sessionID, prepared, cancel, &rounds, &preparedCalls, &executed
	}
	argHook := ToolArgumentsHook{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		return model.CloneRaw(call.Arguments), nil
	}}
	h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
		return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
			inv := invocationExecution(modelFn, planFn)
			inv.ToolHooks = []ToolArgumentsHook{argHook}
			return inv, nil
		}}, nil
	})
	return h, store, c, sessionID, prepared, cancel, &rounds, &preparedCalls, &executed
}

// injectAtNthReplace arms one store hook to fail the transaction that writes
// the nth replace_register step of the fixture turn and disarm itself.
func injectAtNthReplace(store *graphStorage, n int, cause error, onFire func()) {
	replaces := 0
	store.txHook = func(step string) error {
		if step != "replace_register" {
			return nil
		}
		replaces++
		if replaces == n {
			if onFire != nil {
				onFire()
			}
			return cause
		}
		return nil
	}
}

// assertRetainsRunningState proves the failed-publication durable shape: the
// Operation stays exactly Running with the given active effect, and no
// settlement, tool result, or hook result write was fabricated.
func assertRetainsRunningState(t *testing.T, h *Harness, store *graphStorage, sessionID string, wantActiveEffect func(*ActiveEffect) bool) {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", rec.State)
	testassert.Require(t, wantActiveEffect(rec.State.ActiveEffect), "operation active effect = %+v, want the shape the failed publication left committed", rec.State.ActiveEffect)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.Settlement == nil, "the failed publication fabricated a compensating settlement write")
		testassert.Require(t, entry.ToolResult == nil, "the failed publication fabricated a tool result write")
		testassert.Require(t, entry.HookResult == nil, "the failed publication fabricated a hook result write")
	}
}

// TestToolEffectIntentPublicationFailureRetainsRunningState proves the tool
// effect intent's origin boundary: an injected failure of the invoked intent
// transaction — plain storage or cancellation-shaped, under a live execution
// or a concurrent execution cancellation — propagates marked with its original
// cause, the executor never runs, no follow-up write lands, and the Operation
// stays Running for recovery.
func TestToolEffectIntentPublicationFailureRetainsRunningState(t *testing.T) {
	for _, concurrentCancel := range []bool{false, true} {
		for _, cause := range publicationFailureCauses() {
			name := cause.Error()
			if concurrentCancel {
				name += " under concurrent execution cancellation"
			}
			t.Run(name, func(t *testing.T) {
				h, store, c, sessionID, prepared, cancel, rounds, preparedCalls, executed := executorTurnFixture(t, false)
				defer cancel()
				injectAtNthReplace(store, 4, cause, func() { // the tool effect intent's Operation register write
					if concurrentCancel {
						cancel()
					}
				})
				err := executeDirect(h, c, h.ctx, testOpID, prepared)
				store.txHook = nil
				testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
				testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
				testassert.Equal(t, *executed, 0) // no execution was started past the failed intent
				testassert.Equal(t, *preparedCalls, 1)
				testassert.Equal(t, *rounds, 1) // no compensating later dispatch
				assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool { return ae == nil })
			})
		}
	}
}

// TestToolResultPublicationFailureRetainsActiveEffect proves the tool result
// publication's marking on the executor path: after the committed intent and
// one real execution, a failed result transaction propagates marked with its
// cause, never replays the executor, and leaves the durable Running state
// carrying the tool effect's active effect for recovery.
func TestToolResultPublicationFailureRetainsActiveEffect(t *testing.T) {
	for _, cause := range publicationFailureCauses() {
		t.Run(cause.Error(), func(t *testing.T) {
			h, store, c, sessionID, prepared, cancel, _, _, executed := executorTurnFixture(t, false)
			defer cancel()
			injectAtNthReplace(store, 5, cause, nil) // the tool result's Operation register write
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			store.txHook = nil
			testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
			testassert.Equal(t, *executed, 1) // the executor ran exactly once; a failed result never replays it
			assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool {
				return ae != nil && ae.Kind == EffectTool && ae.ToolCallID == "call-1"
			})
		})
	}
}

// TestImmediateToolResultPublicationFailureRetainsNoActiveEffect proves the
// same result-publication marking on the immediate plan shape: no effect
// intent was ever committed, so the retained Running state carries no active
// effect, and the failed result transaction fabricates nothing.
func TestImmediateToolResultPublicationFailureRetainsNoActiveEffect(t *testing.T) {
	for _, cause := range publicationFailureCauses() {
		t.Run(cause.Error(), func(t *testing.T) {
			var rounds int
			modelFn := func(context.Context, model.Request) (model.Stream, error) {
				rounds++
				if rounds == 1 {
					return completedTurnStream(testToolCall("call-1")), nil
				}
				return completedTurnStream(), nil
			}
			planFn := func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
			}
			h, store, c, sessionID, prepared, cancel := newExecutionHarness(t, modelFn, planFn)
			defer cancel()
			inserts := 0
			store.txHook = func(step string) error { // the second entry write of the turn is the tool result entry
				if step == "insert_entry" {
					inserts++
					if inserts == 2 {
						return cause
					}
				}
				return nil
			}
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			store.txHook = nil
			testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
			testassert.Equal(t, rounds, 1) // no compensating later dispatch
			assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool { return ae == nil })
		})
	}
}

// TestHookEffectPublicationFailuresAtIntentAndResult proves the argument-hook
// effect's two publication boundaries with the same rule.
func TestHookEffectPublicationFailuresAtIntentAndResult(t *testing.T) {
	t.Run("the failed hook intent never runs the callback", func(t *testing.T) {
		for _, cause := range publicationFailureCauses() {
			t.Run(cause.Error(), func(t *testing.T) {
				h, store, c, sessionID, prepared, cancel, _, preparedCalls, executed := executorTurnFixture(t, true)
				defer cancel()
				injectAtNthReplace(store, 4, cause, nil) // the hook effect intent's Operation register write
				err := executeDirect(h, c, h.ctx, testOpID, prepared)
				store.txHook = nil
				testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
				testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
				testassert.Equal(t, *preparedCalls, 0) // the chain never reached preparation
				testassert.Equal(t, *executed, 0)
				assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool { return ae == nil })
			})
		}
	})
	t.Run("the failed hook result publication retains the hook intent", func(t *testing.T) {
		for _, cause := range publicationFailureCauses() {
			t.Run(cause.Error(), func(t *testing.T) {
				h, store, c, sessionID, prepared, cancel, _, preparedCalls, executed := executorTurnFixture(t, true)
				defer cancel()
				injectAtNthReplace(store, 5, cause, nil) // the hook result's Operation register write
				err := executeDirect(h, c, h.ctx, testOpID, prepared)
				store.txHook = nil
				testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
				testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
				testassert.Equal(t, *preparedCalls, 0) // the chain stopped at the failed hook publication
				testassert.Equal(t, *executed, 0)
				assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool {
					return ae != nil && ae.Kind == EffectHook && ae.HookID == "cap.hook.one" && ae.ToolCallID == "call-1"
				})
			})
		}
	})
}

// TestRequestHookPublicationFailuresAtIntentAndResult proves the request-bound
// hook effect's intent and result publications through the real pipeline.
func TestRequestHookPublicationFailuresAtIntentAndResult(t *testing.T) {
	run := func(t *testing.T, cause error, replaceOrdinal int, wantHookRan int) {
		t.Helper()
		hookRan := 0
		hook := RequestHook{ID: "req.effectful", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			hookRan++
			return RequestConsequence{}, nil
		}}
		modelFn := func(context.Context, model.Request) (model.Stream, error) { return completedTurnStream(), nil }
		exec := requestHookFixtureExecution(modelFn, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, hook)
		h, store, c, sessionID := newHookHarness(t, exec)
		injectAtNthReplace(store, replaceOrdinal, cause, nil)
		_, _, err := runRequestHookPipeline(t, h, c, exec, testCapture())
		store.txHook = nil
		testassert.Require(t, err != nil, "the pipeline completed past an injected publication failure")
		testassert.Require(t, errors.Is(err, cause), "error = %v, want the original publication cause %v preserved", err, cause)
		testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
		testassert.Equal(t, hookRan, wantHookRan)
		assertRetainsRunningState(t, h, store, sessionID, func(ae *ActiveEffect) bool {
			if replaceOrdinal == 1 {
				return ae == nil // the failed intent committed nothing
			}
			return ae != nil && ae.Kind == EffectHook && ae.HookID == "req.effectful"
		})
		{
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			for _, entry := range graph.Entries {
				testassert.Require(t, entry.Assistant == nil, "the failed hook publication dispatched a conversation request")
			}
		}
	}
	t.Run("the failed request hook intent never runs the callback", func(t *testing.T) {
		for _, cause := range publicationFailureCauses() {
			run(t, cause, 1, 0)
		}
	})
	t.Run("the failed request hook result retains the hook intent and never dispatches", func(t *testing.T) {
		for _, cause := range publicationFailureCauses() {
			run(t, cause, 2, 1)
		}
	})
}

// TestCommitResultNotFoundBoundariesAreMarked proves the ErrNotFound guards of
// the result publications carry the publication-origin classification with
// the original identity.
func TestCommitResultNotFoundBoundariesAreMarked(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	_, err := h.commitToolResult(context.Background(), c, "op-foreign", PendingToolCall{CallID: "call-1", AssistantEntry: EntryRef{SessionID: sessionID, EntryID: "00000000000000000000000000000001"}, ResultEntryID: "00000000000000000000000000000002"},
		ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}}, nil, false, toolSelection{}, ExecutionNotStarted, false)
	testassert.Require(t, errors.Is(err, ErrNotFound), "commitToolResult = %v, want the original ErrNotFound cause", err)
	testassert.Require(t, isPublicationFailure(err), "commitToolResult error = %v, want the publication-origin classification", err)
	_, err = h.commitHookResult(context.Background(), c, "op-foreign", hookEffectBinding{hookID: "cap.hook.one", callID: "call-1"}, "00000000000000000000000000000003", hookResultEntry{Status: hookSucceeded, Evidence: ExecutionObserved})
	testassert.Require(t, errors.Is(err, ErrNotFound), "commitHookResult = %v, want the original ErrNotFound cause", err)
	testassert.Require(t, isPublicationFailure(err), "commitHookResult error = %v, want the publication-origin classification", err)
}

// TestEffectHookInvalidDeclaredKnowledgeRejects proves an effectful hook's
// invalid EffectError declaration reaches the shared accepting validator at
// its result publication: the declaration is rejected with the original
// identity through a marked publication failure, the hook intent stays
// committed for recovery, and no guessed execution knowledge is recorded.
func TestEffectHookInvalidDeclaredKnowledgeRejects(t *testing.T) {
	call := model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}
	exec := hookFixtureExecution(func(context.Context, model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}}}
	}, objectNormalize, ToolArgumentsHook{ID: "cap.hook.bogus", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		return nil, &EffectError{Evidence: ExecutionEvidence("bogus"), Err: errors.New("binding down")}
	}})
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, call)
	_, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), call, []string{"echo"})
	testassert.Require(t, err != nil, "the invalid declaration settled without rejection")
	testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
	rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, rerr)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil && rec.State.ActiveEffect.HookID == "cap.hook.bogus",
		"operation state = %+v, want the hook intent retained for recovery", rec.State)
	graph, verr := validateFixture(t, store, sessionID)
	testassert.NoError(t, verr)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.HookResult == nil, "a rejected declaration recorded a hook result: %+v", entry.HookResult)
		testassert.Require(t, entry.ToolResult == nil && entry.Settlement == nil, "the rejection fabricated a follow-up write: %+v", entry.Envelope.ID)
	}
}

// TestWaitRetainsPublicationFailureAtPendingCallBoundaries proves the Wait
// consequence of the classification: a marked pending-call publication failure
// is retained for Wait, while a hook's ordinary domain failure and a
// synchronous invalid admission never contaminate it.
func TestWaitRetainsPublicationFailureAtPendingCallBoundaries(t *testing.T) {
	t.Run("a marked tool-intent publication failure is retained", func(t *testing.T) {
		h, store, c, _, prepared, cancel, _, _, _ := executorTurnFixture(t, false)
		cause := fmt.Errorf("intent aborted: %w", context.DeadlineExceeded) // cancellation-shaped, live execution
		injectAtNthReplace(store, 4, cause, nil)
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		store.txHook = nil
		testassert.Require(t, err != nil, "executeDirect = %v, want the publication failure", err)
		cancel()
		waitErr := h.Wait(context.Background())
		testassert.Require(t, errors.Is(waitErr, cause), "Wait = %v, want the retained publication cause", waitErr)
	})
	t.Run("a hook domain failure settles the call and never contaminates Wait", func(t *testing.T) {
		var rounds, preparedCalls int
		domainErr := errors.New("the hook could not inspect the call")
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil
			}
			return completedTurnStream(), nil
		}
		planFn := func(_ context.Context, call model.ToolCall) PreparedTool {
			preparedCalls++
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
			return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
				inv := invocationExecution(modelFn, planFn)
				inv.ToolHooks = []ToolArgumentsHook{{ID: "cap.hook.domain", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
					return nil, domainErr
				}}}
				return inv, nil
			}}, nil
		})
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err) // the domain failure settled the call as an ordinary error result
		_ = store
		cancel()
		waitErr := h.Wait(context.Background())
		testassert.Require(t, waitErr == nil, "Wait = %v, want no retained failure after a settled domain hook failure", waitErr)
		_ = sessionID
	})
	t.Run("a synchronous invalid admission never contaminates Wait", func(t *testing.T) {
		h, _, c, sessionID, _, cancel, _, _, _ := executorTurnFixture(t, false)
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: sessionID, OperationID: "op-invalid", Origin: InputOriginUser, Content: admissionContent("hello"), Mode: MessageMode("urgent")})
		testassert.Require(t, errors.Is(err, ErrInvalid), "invalid Submit = %v, want the synchronous rejection", err)
		_ = c
		cancel()
		waitErr := h.Wait(context.Background())
		testassert.Require(t, waitErr == nil, "Wait = %v, want no retained failure after a synchronous invalid admission", waitErr)
	})
}
