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
	"github.com/MMinasyan/lightcode/internal/testassert"
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
			{ID: "cap.hook.one", ExtensionID: "cap-a", Mode: harness.HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				mu.Lock()
				hookSeen = append(hookSeen, string(call.Arguments))
				mu.Unlock()
				return json.RawMessage(`{ "x": "<" }`), nil
			}},
			{ID: "cap.hook.two", ExtensionID: "cap-a", Mode: harness.HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
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
					return harness.Execution{OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
						return harness.InvocationExecution{
							Model:         script.effect,
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
					}}, nil
				},
			}, nil
		}
		session := createSession(t, f.h)
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the published call's turn
		<-script.arrived // the completing turn
		// A queued submit proves op-1's terminal committed: the drain admits
		// it only after the terminal settlement.
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}
		<-f.prepare // the drain admitted op-2: op-1's terminal success committed
		<-script.arrived
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		mu.Lock()
		seen := append([]string{}, hookSeen...)
		toolInput := append([]string{}, prepared...)
		mu.Unlock()
		testassert.Require(t, len(seen) == 2 && seen[0] == `{"x":1}` && seen[1] == `{"x":"\u003c"}`, "hook chain received %q, want the producer's committed normalization then the first hook's committed replacement", seen)
		testassert.Require(t, len(toolInput) == 1 && toolInput[0] == `{"y":"\u0026\u003e"}`, "concrete tool received %q, want the final committed replacement bytes", toolInput)

		rec, err := f.h.ReadOperation(ctx, session, "op-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "hook turn = %+v err %v, want terminal success with every effect settled", rec, err)

		// The committed evidence: one success hook_result per hook, in
		// configured order, carrying the durable replacement bytes.
		entries, err := store.ReadEntries(ctx, session, 0)
		testassert.NoError(t, err)
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
			{
				err := json.Unmarshal(entry.Payload, &wire)
				testassert.NoError(t, err)
			}
			hookResults = append(hookResults, wire)
		}
		testassert.Require(t, len(hookResults) == 2 && hookResults[0].HookID == "cap.hook.one" && hookResults[0].Status == "success" && string(hookResults[0].Arguments) == `{"x":"\u003c"}` && hookResults[1].HookID == "cap.hook.two" && hookResults[1].Status == "success" && string(hookResults[1].Arguments) == `{"y":"\u0026\u003e"}`, "committed hook results = %+v, want one success per hook with the durable bytes", hookResults)

		// The evidence never projects as conversation: the completing turn's
		// projection carries the call result, not the hook replacements.
		projection := texts(script.seen()[1])
		for _, text := range projection {
			testassert.Require(t, text != `{"y":"\u0026\u003e"}`, "hook evidence projected as conversation content")
		}

		// A restart revalidates the Session and Operation without any plugin:
		// the closed decoder interprets the hook evidence itself.
		{
			err := harness.Recover(ctx, store)
			testassert.NoError(t, err)
		}
		restarted, err := harness.New(ctx, harness.Dependencies{Storage: store, Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{}, errors.New("no preparation after restart")
		}})
		testassert.NoError(t, err)
		{
			_, err := restarted.ReadSessionHeader(ctx, session)
			testassert.NoError(t, err)
		}
		rec, err = restarted.ReadOperation(ctx, session, "op-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess, "restarted ReadOperation = %+v err %v, want the revalidated settled Operation", rec, err)

		// A Fork copies no hook effect: the fork boundary is op-2's input, so
		// the copied prefix actually contains op-1's committed hook evidence —
		// asserted present in the source prefix and absent from the fork.
		boundary := forkEntryOf(t, store, session, harness.EntryInput, "op-2")
		before := snapshotSession(t, store, session)
		forkScript := newScriptModel(publicFail("fork turn settled"))
		f2 := newPublicFixture(t, store, forkScript, nil)
		defer f2.close()
		res, err := f2.h.Fork(ctx, harness.ForkRequest{SourceSessionID: session, BoundaryEntryID: boundary.ID, OperationID: "fork-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "fork input"}}})
		testassert.NoError(t, err)
		<-forkScript.arrived
		forkScript.releaseGate()
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}
		assertForkSourceUnchanged(t, store, session, before)
		var sourceHooks int
		for _, entry := range entries {
			if entry.Kind == harness.EntryHookResult && entry.Sequence < boundary.Sequence {
				sourceHooks++
			}
		}
		testassert.Equal(t, sourceHooks, 2)
		copied, err := store.ReadEntries(ctx, res.Session.Identity.SessionID, 0)
		testassert.NoError(t, err)
		hookEffects, acceptedSelections := 0, 0
		for _, entry := range copied {
			if entry.Kind == harness.EntryHookResult {
				hookEffects++
				continue
			}
			if entry.Kind != harness.EntryToolResult {
				continue
			}
			var wire struct {
				NormalizedArguments json.RawMessage `json:"normalized_arguments"`
				HookIDs             []struct {
					ExtensionID string `json:"extension_id"`
					HookID      string `json:"hook_id"`
				} `json:"hook_ids"`
			}
			testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
			testassert.Require(t, string(wire.NormalizedArguments) == `{"y":"\u0026\u003e"}` && len(wire.HookIDs) == 2 &&
				wire.HookIDs[0].HookID == "cap.hook.one" && wire.HookIDs[1].HookID == "cap.hook.two",
				"copied tool result %s lost its accepted selection: %s", entry.ID, entry.Payload)
			acceptedSelections++
		}
		testassert.Require(t, hookEffects == 0, "the fork copied %d hook effects, want none", hookEffects)
		// The copied accepted selection is operationless provenance: it stays
		// valid without its excluded hook effect entries and revalidates cold.
		testassert.Require(t, acceptedSelections == 1, "copied accepted tool selections = %d, want the settled call carried whole", acceptedSelections)
		{
			_, err := f2.h.ReadHistoryView(ctx, res.Session.Identity.SessionID)
			testassert.Require(t, err == nil, "destination history view: %v, want the copied accepted selection to revalidate with hook effects excluded", err)
		}
	})
}

// TestPublicForkRemapsRequestEvidenceAndExcludesHookEffects proves the fork
// rows for request evidence: the copied assistant's consumed evidence survives
// as the same closed delta with its canonical boundary rewritten to the copied
// entry identity, the hook provenance stays recorded unchanged, no hook effect
// or hook Operation state crosses the boundary, and the destination graph
// revalidates through a cold read.
func TestPublicForkRemapsRequestEvidenceAndExcludesHookEffects(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		netPrompt := "fork-test net prompt"
		purePrompt := "fork-test pure prompt"
		addition, aerr := model.NewMessage(model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "contributed"}}})
		testassert.NoError(t, aerr)
		hooks := []harness.RequestHook{
			{ID: "req.pure", ExtensionID: "req-ext", Mode: harness.HookModePure, Run: func(context.Context, harness.RequestHookInput) (harness.RequestConsequence, error) {
				p := purePrompt
				return harness.RequestConsequence{Prompt: &p}, nil
			}},
			{ID: "req.effectful", ExtensionID: "req-ext", Mode: harness.HookModeEffectful, Run: func(context.Context, harness.RequestHookInput) (harness.RequestConsequence, error) {
				p := netPrompt
				return harness.RequestConsequence{Prompt: &p, Messages: []model.Message{addition}}, nil
			}},
		}
		script := newScriptModel(publicTurn(), publicTurn(), publicTurn())
		f := newPublicFixture(t, store, script, nil)
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
						return harness.InvocationExecution{
							Model:         script.effect,
							NormalizeTool: hookNormalize,
							RequestHooks:  hooks,
							Tool:          func(context.Context, model.ToolCall) harness.PreparedTool { return harness.PreparedTool{} },
						}, nil
					}}, nil
				},
			}, nil
		}
		session := createSession(t, f.h)
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}
		<-f.prepare
		<-script.arrived
		{
			_, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queued-2")
			testassert.NoError(t, err)
		}
		<-f.prepare
		<-script.arrived
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		entries, err := store.ReadEntries(ctx, session, 0)
		testassert.NoError(t, err)
		type evidenceWire struct {
			BoundaryEntryID string            `json:"boundary_entry_id"`
			Prompt          *string           `json:"prompt"`
			Messages        []json.RawMessage `json:"messages"`
			HookIDs         []struct {
				ExtensionID string `json:"extension_id"`
				HookID      string `json:"hook_id"`
			} `json:"hook_ids"`
		}
		type assistantWire struct {
			RequestID string        `json:"request_id"`
			Evidence  *evidenceWire `json:"evidence"`
		}
		var sourceAssistant *assistantWire
		var sourceHookResults int
		for _, entry := range entries {
			switch entry.Kind {
			case harness.EntryAssistant:
				var wire assistantWire
				testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
				sourceAssistant = &wire // op-3's assistant is the last committed
			case harness.EntryHookResult:
				sourceHookResults++
			}
		}
		testassert.Require(t, sourceAssistant != nil && sourceAssistant.Evidence != nil, "source assistant carries no request evidence")
		testassert.Require(t, sourceHookResults == 3, "source hook results = %d, want one effectful request hook per dispatched request", sourceHookResults)
		testassert.Require(t, len(sourceAssistant.Evidence.HookIDs) == 2 && sourceAssistant.Evidence.HookIDs[0].HookID == "req.pure" &&
			sourceAssistant.Evidence.HookIDs[1].HookID == "req.effectful",
			"source assistant provenance = %+v, want the changed pure contributor and the effectful setter in hook order", sourceAssistant.Evidence.HookIDs)

		boundary := forkEntryOf(t, store, session, harness.EntryInput, "op-2")
		before := snapshotSession(t, store, session)
		forkScript := newScriptModel(publicFail("fork turn settled"))
		forkScript.gate = make(chan struct{})
		f2 := newPublicFixture(t, store, forkScript, nil)
		defer f2.close()
		res, err := f2.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: session,
			BoundaryEntryID: boundary.ID,
			OperationID:     "fork-1",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		dest := res.Session.Identity.SessionID
		<-forkScript.arrived
		forkScript.releaseGate()
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}
		assertForkSourceUnchanged(t, store, session, before)

		copied, err := store.ReadEntries(ctx, dest, 0)
		testassert.NoError(t, err)
		destIDs := map[string]bool{}
		var copiedAssistants []*assistantWire
		copiedHooks := 0
		for _, entry := range copied {
			destIDs[entry.ID] = true
			switch entry.Kind {
			case harness.EntryAssistant:
				var wire assistantWire
				testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
				copiedAssistants = append(copiedAssistants, &wire)
			case harness.EntryHookResult:
				copiedHooks++
			}
		}
		testassert.Require(t, copiedHooks == 0, "the fork copied %d hook effects, want none", copiedHooks)
		// The copied prefix carries op-1's evidence-bearing assistant (op-3's
		// assistant sits past the boundary and is not copied).
		testassert.Equal(t, len(copiedAssistants), 1)
		copiedEvidence := copiedAssistants[0].Evidence
		testassert.Require(t, copiedEvidence != nil, "copied assistant carries no request evidence")
		testassert.Require(t, destIDs[copiedEvidence.BoundaryEntryID], "copied evidence boundary %q names no destination entry", copiedEvidence.BoundaryEntryID)
		testassert.Require(t, copiedEvidence.Prompt != nil && *copiedEvidence.Prompt == netPrompt,
			"copied evidence prompt = %v, want the net contribution the assistant consumed", copiedEvidence.Prompt)
		testassert.Require(t, len(copiedEvidence.Messages) == 1 && len(copiedEvidence.HookIDs) == 2 &&
			copiedEvidence.HookIDs[0].HookID == "req.pure" && copiedEvidence.HookIDs[1].HookID == "req.effectful",
			"copied evidence = %+v, want the retained delta and recorded provenance unchanged", copiedEvidence)
		{
			_, err := f2.h.ReadHistoryView(ctx, dest)
			testassert.Require(t, err == nil, "destination history view: %v, want the copied evidence-carrying prefix to revalidate", err)
		}
	})
}
