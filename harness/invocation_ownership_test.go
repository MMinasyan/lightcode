// Contract coverage for accepted-hook ownership: the arrays an opener returns
// and keeps referencing are copied at each acceptance boundary (the
// conversation execute entry and the maintenance model opener), so a hook
// callback running after acceptance can never replace, nil, or zero the
// accepted invocation's hook identities, modes, or configured order for the
// Harness's later consumers. At the maintenance site no hook ever runs, so
// the ownership there is preservation: the accepted invocation survives every
// post-open mutation unchanged and the procedure completes.
package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestConversationOpenedInvocationDeclarationsAreOwned drives one real turn
// through the conversation acceptance site with all three hook families on the
// opened invocation. The first pure request hook replaces the later request
// hook slot, nils the opener's argument-hook variable after zeroing its first
// element through a tampered replacement, replaces the first output hook slot,
// and the first output hook zeroes its chain's later slot: the Harness must
// still run the originally accepted identities, modes, and order for every
// family at every later use, settle the turn from them, and record only the
// original identities durably.
func TestConversationOpenedInvocationDeclarationsAreOwned(t *testing.T) {
	var trace []string
	ran := func(entry string) { trace = append(trace, entry) }

	var sharedRequest []RequestHook
	var sharedArgs []ToolArgumentsHook
	var sharedOutput []ToolOutputHook

	reqRuns := 0
	tamperOnce := func() {
		if reqRuns != 1 { // the first accepted hook of the first round tampers every family
			return
		}
		sharedRequest[1] = RequestHook{ID: "tampered.req", ExtensionID: "tamper-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			ran("tampered.req")
			return RequestConsequence{}, nil
		}}
		sharedArgs[0] = ToolArgumentsHook{ID: "tampered.arg", ExtensionID: "tamper-ext", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			ran("tampered.arg")
			return model.CloneRaw(call.Arguments), nil
		}}
		sharedArgs = nil
		sharedOutput[0] = ToolOutputHook{ID: "tampered.out", ExtensionID: "tamper-ext", Run: func(_ context.Context, in ToolOutputInput) (string, error) {
			ran("tampered.out")
			return in.Content, nil
		}}
	}

	reqPure := RequestHook{ID: "own.req.pure", ExtensionID: "own-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		reqRuns++
		ran("own.req.pure")
		tamperOnce()
		return RequestConsequence{}, nil
	}}
	reqEffectful := RequestHook{ID: "own.req.effectful", ExtensionID: "own-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		ran("own.req.effectful")
		return RequestConsequence{}, nil
	}}
	argPure := ToolArgumentsHook{ID: "own.arg.pure", ExtensionID: "own-ext", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		ran("own.arg.pure")
		return model.CloneRaw(call.Arguments), nil
	}}
	argEffectful := ToolArgumentsHook{ID: "own.arg.effectful", ExtensionID: "own-ext", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		ran("own.arg.effectful")
		return model.CloneRaw(call.Arguments), nil
	}}
	outOne := ToolOutputHook{ID: "own.out.one", ExtensionID: "own-ext", Run: func(_ context.Context, in ToolOutputInput) (string, error) {
		ran("own.out.one")
		sharedOutput[1] = ToolOutputHook{} // zero the opener-retained later slot after acceptance
		return in.Content + " shaped", nil
	}}
	outTwo := ToolOutputHook{ID: "own.out.two", ExtensionID: "own-ext", Run: func(_ context.Context, in ToolOutputInput) (string, error) {
		ran("own.out.two")
		return in.Content, nil
	}}

	rounds := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		rounds++
		if rounds == 1 {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	spy := &hookToolSpy{}
	exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		sharedRequest = []RequestHook{reqPure, reqEffectful}
		sharedArgs = []ToolArgumentsHook{argPure, argEffectful}
		sharedOutput = []ToolOutputHook{outOne, outTwo}
		inv := invocationExecution(modelFn, spy.tool)
		inv.RequestHooks = sharedRequest
		inv.ToolHooks = sharedArgs
		inv.ToolOutputHooks = sharedOutput
		return inv, nil
	}}

	h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	defer cancel()
	testassert.NoError(t, executeDirect(h, c, h.ctx, testOpID, prepared))

	// The tamper really landed on the opener-retained arrays.
	testassert.Require(t, sharedRequest[1].ID == "tampered.req", "opener request array = %+v, want the replacement to stand on the opener side", sharedRequest)
	testassert.Require(t, sharedArgs == nil, "opener argument array = %+v, want the nilled variable", sharedArgs)
	testassert.Require(t, sharedOutput[0].ID == "tampered.out", "opener output array slot 0 = %+v, want the replacement to stand on the opener side", sharedOutput[0])
	testassert.Require(t, sharedOutput[1].ID == "", "opener output array slot 1 = %+v, want the zeroed entry to stand on the opener side", sharedOutput[1])

	// The accepted families ran unchanged: original identities, modes, and
	// configured order at every later use, with no tampered entry anywhere.
	want := []string{
		"own.req.pure", "own.req.effectful", // round 1 request family
		"own.arg.pure", "own.arg.effectful", "own.out.one", "own.out.two", // the settled call's argument then output families
		"own.req.pure", "own.req.effectful", // round 2 re-runs the accepted request family
	}
	testassert.Require(t, len(trace) == len(want), "hook trace = %+v, want %+v", trace, want)
	for i := range want {
		if i < len(trace) {
			testassert.Require(t, trace[i] == want[i], "hook trace entry %d = %q, want %q", i, trace[i], want[i])
		}
	}

	// The durable evidence carries only the accepted identities.
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Equal(t, len(committed), 3) // the effectful request hook once per round plus the effectful argument hook once
	for _, entry := range committed {
		testassert.Require(t, (entry.HookID == "own.req.effectful" || entry.HookID == "own.arg.effectful") && entry.ExtensionID == "own-ext" && entry.Status == hookSucceeded,
			"committed hook result = %+v, want an accepted effectful hook settling succeeded", entry)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationSuccess && len(rec.State.PendingToolCalls) == 0, "operation state = %+v, want the turn settled from the accepted families", rec.State)

	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph validation: %v", err)
	}
}

// TestMaintenanceOpenedInvocationDeclarationsArePreserved drives the
// maintenance acceptance site: its opened invocation carries all three hook
// families, and the compact model callback — which runs strictly after the
// acceptance boundary — replaces, nils, and zeroes the opener-retained arrays.
// Maintenance never runs an accepted hook, so the ownership is preservation:
// the procedure completes over its summary pass and no hook is ever invoked,
// however the opener-retained arrays mutate afterwards.
func TestMaintenanceOpenedInvocationDeclarationsArePreserved(t *testing.T) {
	var sharedRequest []RequestHook
	var sharedArgs []ToolArgumentsHook
	var sharedOutput []ToolOutputHook

	passes := 0
	// The compact callback tampers every opener-retained array the moment it
	// runs — a replacement, a nil-out, and a zero-out — strictly after the
	// acceptance boundary copied them.
	compactFn := func(context.Context, model.Request) (model.Stream, error) {
		passes++
		sharedRequest[0] = RequestHook{ID: "tampered.req", ExtensionID: "tamper-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			t.Errorf("a tampered maintenance request hook ran")
			return RequestConsequence{}, nil
		}}
		sharedArgs = nil
		sharedOutput[0] = ToolOutputHook{}
		return summaryStream("gap summary", model.Usage{InputTokens: 1}), nil
	}
	noopTool := func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	exec := Execution{
		Strategy: &StrategyBinding{ID: gapStrategyID, Strategy: &gapStrategy{t: t}},
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			sharedRequest = []RequestHook{{ID: "own.req", ExtensionID: "own-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
				t.Errorf("a maintenance invocation request hook ran")
				return RequestConsequence{}, nil
			}}}
			sharedArgs = []ToolArgumentsHook{{ID: "own.arg", ExtensionID: "own-ext", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
				t.Errorf("a maintenance invocation argument hook ran")
				return nil, nil
			}}}
			sharedOutput = []ToolOutputHook{{ID: "own.out", ExtensionID: "own-ext", Run: func(context.Context, ToolOutputInput) (string, error) {
				t.Errorf("a maintenance invocation output hook ran")
				return "", nil
			}}}
			inv := invocationExecution(compactFn, noopTool)
			inv.RequestHooks = sharedRequest
			inv.ToolHooks = sharedArgs
			inv.ToolOutputHooks = sharedOutput
			return inv, nil
		},
	}

	h, _, c, sessionID := newGapHarness(t)
	_, _, _, _, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, gapCapture(), compactSnapshotMessages(), nil, nil)
	defer closeInvocation()
	testassert.NoError(t, err)
	testassert.Equal(t, passes, 1)

	testassert.Require(t, sharedRequest[0].ID == "tampered.req" && sharedArgs == nil && sharedOutput[0].ID == "",
		"opener-retained arrays after the procedure = %+v / %v / %+v, want the tamper to have landed on the opener side", sharedRequest, sharedArgs, sharedOutput)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.UnreportedMaintenance,
		"maintenance procedure state after the post-acceptance tamper = %+v, want the procedure to have completed over its summary pass", rec.State)
}
