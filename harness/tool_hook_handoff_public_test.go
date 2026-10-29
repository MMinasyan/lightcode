// Public-surface coverage for the managed shutdown at the pending-call effect
// intent handoffs on both real stores: a Harness lifetime cancellation
// injected exactly when a tool or hook effect intent transaction is handed to
// the store — after the intent owner's pre-Transact cancellation check has
// already admitted the transition — never aborts the admitted intent's durable
// write, the execution gate then settles the ordinary interruption without
// ever invoking the external executor or hook callback, and Wait retains no
// publication failure, so the fixture leaves no running Operation behind.
package harness_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// armIntentHandoffShutdown chains the probe's single-shot hooks so the
// Harness lifetime cancellation lands at the entry of the nth transaction of
// the tool turn: admission (1), model intent (2), model result (3), and the
// pending-call effect intent (4).
func armIntentHandoffShutdown(probe *publicationCancelStore, f *publicFixture, handoff chan struct{}) {
	probe.afterCommit = func() {
		probe.afterCommit = func() {
			probe.afterCommit = func() {
				probe.atEntry = func() {
					close(handoff)
					f.cancel()
				}
			}
		}
	}
}

// TestPublicExecutionCancellationAtToolIntentHandoff mirrors the model-effect
// handoff row at the tool effect's intent: the committed intent is followed
// by the interrupted-before-execution settlement, the external executor never
// runs, and Wait converges clean.
func TestPublicExecutionCancellationAtToolIntentHandoff(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(publicTurn("call-1"), publicTurn())
		probe := &publicationCancelStore{Storage: store}
		f := newPublicFixture(t, probe, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		var executed int
		intentHandoff := make(chan struct{})
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			prep := f.defaultPrep
			prep.Open = func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
				return harness.Execution{OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
					return harness.InvocationExecution{
						Model:         script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return harness.PreparedTool{Permissions: publicPermission, Execute: func(context.Context) harness.ToolOutcome {
								executed++
								return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
							}}
						},
					}, nil
				}}, nil
			}
			armIntentHandoffShutdown(probe, f, intentHandoff)
			return prep, nil
		}
		res, err := submit(t, f.h, session, "op-tool-intent", harness.MessageModeRegular, "hello")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted && res.Operation != nil, "submit = %+v err %v, want the admitted operation", res, err)
		select { // the tool intent transaction is at the store handoff: the pre-Transact check already passed
		case <-intentHandoff:
		case <-time.After(10 * time.Second):
			t.Fatalf("the tool effect intent never reached the store handoff")
		}
		{
			err := converge(t, f)
			testassert.Require(t, err == nil, "Wait after the tool-intent-handoff shutdown = %v, want no retained publication or storage failure", err)
		}
		testassert.Require(t, executed == 0, "the canceled effect executed the tool %d times, want zero after the committed intent", executed)
		testassert.Require(t, len(script.seen()) == 1, "the canceled execution dispatched %d model requests, want only the publishing turn", len(script.seen()))
		rec, err := f.h.ReadOperation(context.Background(), session, "op-tool-intent")
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted" && len(rec.State.PendingToolCalls) == 0,
			"operation after the tool-intent-handoff shutdown = %+v, want the committed ordinary interruption with the call settled", rec.State)
	})
}

// TestPublicExecutionCancellationAtHookIntentHandoff mirrors the same row at
// the argument-hook effect's intent: the committed hook intent is followed by
// the interrupted hook settlement, the hook callback never runs, and Wait
// converges clean.
func TestPublicExecutionCancellationAtHookIntentHandoff(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(publicTurn("call-1"), publicTurn())
		probe := &publicationCancelStore{Storage: store}
		f := newPublicFixture(t, probe, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		var hookRan, prepared int
		intentHandoff := make(chan struct{})
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			prep := f.defaultPrep
			prep.Open = func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
				return harness.Execution{OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
					return harness.InvocationExecution{
						Model:         script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							prepared++
							return harness.PreparedTool{Permissions: publicPermission, Execute: func(context.Context) harness.ToolOutcome {
								return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
							}}
						},
						ToolHooks: []harness.ToolArgumentsHook{{ID: "hook.handoff", ExtensionID: "handoff-ext", Mode: harness.HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
							hookRan++
							return model.CloneRaw(call.Arguments), nil
						}}},
					}, nil
				}}, nil
			}
			armIntentHandoffShutdown(probe, f, intentHandoff)
			return prep, nil
		}
		res, err := submit(t, f.h, session, "op-hook-intent", harness.MessageModeRegular, "hello")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted && res.Operation != nil, "submit = %+v err %v, want the admitted operation", res, err)
		select { // the hook intent transaction is at the store handoff: the pre-Transact check already passed
		case <-intentHandoff:
		case <-time.After(10 * time.Second):
			t.Fatalf("the argument hook effect intent never reached the store handoff")
		}
		{
			err := converge(t, f)
			testassert.Require(t, err == nil, "Wait after the hook-intent-handoff shutdown = %v, want no retained publication or storage failure", err)
		}
		testassert.Require(t, hookRan == 0, "the canceled effect ran the hook %d times, want zero after the committed intent", hookRan)
		testassert.Require(t, prepared == 0, "the canceled chain prepared the call %d times, want zero", prepared)
		testassert.Require(t, len(script.seen()) == 1, "the canceled execution dispatched %d model requests, want only the publishing turn", len(script.seen()))
		rec, err := f.h.ReadOperation(context.Background(), session, "op-hook-intent")
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted" && len(rec.State.PendingToolCalls) == 0,
			"operation after the hook-intent-handoff shutdown = %+v, want the committed ordinary interruption with the call settled", rec.State)
	})
}
