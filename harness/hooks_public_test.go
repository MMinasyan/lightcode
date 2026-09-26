// External-package public Harness suite: the argument-hook rows through
// public operations only, over both storage implementations.
package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// hookNormalize is the public hook rows' byte-preserving normalizer: it
// accepts exactly one non-null JSON object and returns the caller's exact
// argument bytes, so every difference between consumed and committed bytes is
// attributable to the durable codec.
func hookNormalize(call model.ToolCall) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, errors.New("arguments must be one non-null JSON object")
	}
	return model.CloneRaw(call.Arguments), nil
}

// TestPublicToolArgumentHookTurn proves the complete hook path through the
// public Harness without the Runtime: selected argument hooks settle as their
// own durable effects before concrete preparation, the first hook receives the
// original raw call arguments and every later one the preceding committed
// replacement bytes, the final committed value reaches the concrete tool
// byte-identically, the hook evidence never projects as conversation, a
// restart revalidates the Session without any plugin, and a Fork copies no
// hook effect or hook Operation state.
func TestPublicToolArgumentHookTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		var (
			mu       sync.Mutex
			hookSeen []string // argument bytes each hook received, in chain order
			prepared []string // argument bytes the concrete tool received
		)
		hooks := []harness.ToolArgumentsHook{
			{ID: "cap.hook.one", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				mu.Lock()
				hookSeen = append(hookSeen, string(call.Arguments))
				mu.Unlock()
				return json.RawMessage(`{ "x": "<" }`), nil
			}},
			{ID: "cap.hook.two", Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				mu.Lock()
				hookSeen = append(hookSeen, string(call.Arguments))
				mu.Unlock()
				return json.RawMessage(`{"y":"&>"}`), nil
			}},
		}
		script := newScriptModel(
			publicTurn("call-1"),               // the turn publishes the call
			publicTurn(),                       // the turn completes after the result
			publicFail("drained turn settled"), // the drained item's own terminal
		)
		f := newPublicFixture(t, store, script, nil)
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: hookNormalize,
						ToolHooks:     hooks,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							mu.Lock()
							prepared = append(prepared, string(call.Arguments))
							mu.Unlock()
							return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
								Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"},
							}}
						},
					}, nil
				},
			}, nil
		}
		session := createSession(t, f.h)
		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived // the published call's turn
		<-script.arrived // the completing turn
		// A queued submit proves op-1's terminal committed: the drain admits
		// it only after the terminal settlement.
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		<-f.prepare // the drain admitted op-2: op-1's terminal success committed
		<-script.arrived
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		mu.Lock()
		seen := append([]string{}, hookSeen...)
		toolInput := append([]string{}, prepared...)
		mu.Unlock()
		if len(seen) != 2 || seen[0] != `{"x":1}` || seen[1] != `{"x":"\u003c"}` {
			t.Fatalf("hook chain received %q, want the producer's committed normalization then the first hook's committed replacement", seen)
		}
		if len(toolInput) != 1 || toolInput[0] != `{"y":"\u0026\u003e"}` {
			t.Fatalf("concrete tool received %q, want the final committed replacement bytes", toolInput)
		}

		rec, err := f.h.ReadOperation(ctx, session, "op-1")
		if err != nil || rec.State.Status != harness.OperationSuccess || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
			t.Fatalf("hook turn = %+v err %v, want terminal success with every effect settled", rec, err)
		}

		// The committed evidence: one success hook_result per hook, in
		// configured order, carrying the durable replacement bytes.
		entries, err := store.ReadEntries(ctx, session, 0)
		if err != nil {
			t.Fatalf("read entries: %v", err)
		}
		type hookWire struct {
			HookID     string          `json:"hook_id"`
			ToolCallID string          `json:"tool_call_id"`
			Status     string          `json:"status"`
			Arguments  json.RawMessage `json:"arguments"`
			Error      string          `json:"error"`
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
		if len(hookResults) != 2 ||
			hookResults[0].HookID != "cap.hook.one" || hookResults[0].Status != "success" || string(hookResults[0].Arguments) != `{"x":"\u003c"}` ||
			hookResults[1].HookID != "cap.hook.two" || hookResults[1].Status != "success" || string(hookResults[1].Arguments) != `{"y":"\u0026\u003e"}` {
			t.Fatalf("committed hook results = %+v, want one success per hook with the durable bytes", hookResults)
		}

		// The evidence never projects as conversation: the completing turn's
		// projection carries the call result, not the hook replacements.
		projection := texts(script.seen()[1])
		for _, text := range projection {
			if text == `{"y":"\u0026\u003e"}` {
				t.Fatalf("hook evidence projected as conversation content")
			}
		}

		// A restart revalidates the Session and Operation without any plugin:
		// the closed decoder interprets the hook evidence itself.
		if err := harness.Recover(ctx, store); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		restarted, err := harness.New(ctx, harness.Dependencies{Storage: store, Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{}, errors.New("no preparation after restart")
		}})
		if err != nil {
			t.Fatalf("restarted Harness: %v", err)
		}
		if _, err := restarted.ReadSession(ctx, session); err != nil {
			t.Fatalf("restarted ReadSession: %v", err)
		}
		rec, err = restarted.ReadOperation(ctx, session, "op-1")
		if err != nil || rec.State.Status != harness.OperationSuccess {
			t.Fatalf("restarted ReadOperation = %+v err %v, want the revalidated settled Operation", rec, err)
		}

		// A Fork copies no hook effect: the fork boundary is op-2's input, so
		// the copied prefix actually contains op-1's committed hook evidence —
		// asserted present in the source prefix and absent from the fork.
		boundary := forkEntryOf(t, store, session, harness.EntryInput, "op-2")
		before := snapshotSession(t, store, session)
		forkScript := newScriptModel(publicFail("fork turn settled"))
		f2 := newPublicFixture(t, store, forkScript, nil)
		defer f2.close()
		res, err := f2.h.Fork(ctx, harness.ForkRequest{SourceSessionID: session, BoundaryEntryID: boundary.ID, OperationID: "fork-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "fork input"}}})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		<-forkScript.arrived
		forkScript.releaseGate()
		if err := converge(t, f2); err != nil {
			t.Fatalf("fork Wait: %v", err)
		}
		assertForkSourceUnchanged(t, store, session, before)
		var sourceHooks int
		for _, entry := range entries {
			if entry.Kind == harness.EntryHookResult && entry.Sequence < boundary.Sequence {
				sourceHooks++
			}
		}
		if sourceHooks != 2 {
			t.Fatalf("source prefix hook evidence = %d entries, want the two committed hook results before the boundary", sourceHooks)
		}
		copied, err := store.ReadEntries(ctx, res.Session.Identity.SessionID, 0)
		if err != nil {
			t.Fatalf("read fork entries: %v", err)
		}
		for _, entry := range copied {
			if entry.Kind == harness.EntryHookResult {
				t.Fatalf("fork copied a hook effect: entry %s", entry.ID)
			}
		}
	})
}
