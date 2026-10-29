package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// emptyText returns a pointer to the empty string for present-empty
// consequence members.
func emptyText() *string {
	text := ""
	return &text
}

// compositionFixtureRequest is the request-hook round driver: one full
// pipeline round (request source with the opened invocation's request hooks,
// then the model effect) over one frozen round, recording the dispatch
// request the model callback received.
func compositionFixtureRequest(t *testing.T, h *Harness, c *coordinator, exec Execution, inv InvocationExecution, capture ExecutionCapture) (agent.ModelSettlement, model.Request, error) {
	t.Helper()
	var sent model.Request
	round := &conversationRound{}
	inv.Model = func(_ context.Context, r model.Request) (model.Stream, error) {
		sent = r
		return completedTurnStream(), nil
	}
	source := h.requestSource(c, testOpID, capture, exec, inv, round)
	if _, err := source(context.Background()); err != nil {
		return agent.ModelSettlement{}, model.Request{}, err
	}
	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, capture, round), defaultModelRequest(t), nil)
	if err != nil {
		return agent.ModelSettlement{}, model.Request{}, err
	}
	return set, sent, nil
}

// compositionModelEffect returns the committed assistant payload of one
// settled conversation request.
func compositionAssistant(t *testing.T, store *graphStorage, sessionID string) *assistantEntry {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var out *assistantEntry
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			out = entry.Assistant
		}
	}
	testassert.Require(t, out != nil, "no committed assistant entry")
	return out
}

// TestRequestHookPurePromptRemoval proves the pure request-boundary rows: a
// present-empty Prompt removes the captured prompt from the actual dispatch,
// the contribution is recorded as durable evidence on the answering
// assistant, and the pure hook commits no durable effect of its own.
func TestRequestHookPurePromptRemoval(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	remover := RequestHook{ID: "req.prompt.remove", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		return RequestConsequence{Prompt: emptyText()}, nil
	}}
	inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{remover}}
	set, sent, err := compositionFixtureRequest(t, h, c, effectExecution(nil, nil), inv, testCapture())
	testassert.NoError(t, err)
	testassert.Equal(t, set.Disposition, agent.DispoReady)
	testassert.Require(t, len(sent.Messages) > 0 && sent.Messages[0].Role != model.RoleSystem, "dispatch request = %+v, want no system prompt message after a present-empty prompt", sent.Messages)

	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && assistant.Evidence.Prompt != nil && *assistant.Evidence.Prompt == "", "assistant evidence = %+v, want the recorded empty-prompt contribution", assistant.Evidence)
	testassert.Require(t, len(assistant.Evidence.HookIDs) == 1 && assistant.Evidence.HookIDs[0].HookID == "req.prompt.remove" && assistant.Evidence.HookIDs[0].ExtensionID == "req-ext", "assistant evidence hook identities = %+v, want the pure contributor recorded", assistant.Evidence.HookIDs)

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.HookResult == nil, "pure request hook committed a durable hook result: %+v", entry.HookResult)
	}
}

// TestRequestHookPureToolView proves the pure tool-view rows: a present
// selection narrows the advertised catalog, a present-empty selection
// advertises none while the actual captured catalog rides the request, an
// out-of-catalog name rejects the round before any model callback, and a
// later selection restoring the full catalog keeps every accepted identity
// while recording no durable tool-view delta.
func TestRequestHookPureToolView(t *testing.T) {
	t.Run("narrowed view advertised", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		empty := RequestHook{ID: "req.tools.empty", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{ToolNames: []string{}}, nil
		}}
		inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{empty}}
		_, sent, err := compositionFixtureRequest(t, h, c, effectExecution(nil, nil), inv, testCapture())
		testassert.NoError(t, err)
		testassert.Require(t, len(sent.Tools) == 0, "dispatch tools = %+v, want the actual request narrowed to the empty advertised view", sent.Tools)
		assistant := compositionAssistant(t, store, sessionID)
		testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.ToolNames) == 0 && assistant.Evidence.ToolNames != nil, "assistant evidence = %+v, want the recorded empty advertised view", assistant.Evidence)
		testassert.Require(t, len(assistant.Evidence.HookIDs) == 1 && assistant.Evidence.HookIDs[0].HookID == "req.tools.empty", "assistant evidence hook identities = %+v, want the narrowing pure contributor retained", assistant.Evidence.HookIDs)
	})
	t.Run("outside catalog rejects before dispatch", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		calls := 0
		bad := RequestHook{ID: "req.tools.bad", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{ToolNames: []string{"not-captured"}}, nil
		}}
		inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{bad}}
		exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
			calls++
			return completedTurnStream(), nil
		}, nil)
		_, _, err := compositionFixtureRequest(t, h, c, exec, inv, testCapture())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "outside the captured tool catalog"), "round = %v, want the out-of-catalog rejection", err)
		testassert.Equal(t, calls, 0)
	})
	t.Run("restored view keeps accepted identities with no delta", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		narrow := RequestHook{ID: "req.tools.narrow", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{ToolNames: []string{}}, nil
		}}
		restore := RequestHook{ID: "req.tools.restore", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{ToolNames: []string{"echo"}}, nil // a selection covering the captured catalog restores the full view
		}}
		inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{narrow, restore}}
		_, sent, err := compositionFixtureRequest(t, h, c, effectExecution(nil, nil), inv, testCapture())
		testassert.NoError(t, err)
		testassert.Require(t, len(sent.Tools) == 1 && sent.Tools[0].Name == "echo", "dispatch tools = %+v, want the restored full catalog", sent.Tools)
		assistant := compositionAssistant(t, store, sessionID)
		testassert.Require(t, assistant.Evidence != nil && assistant.Evidence.ToolNames == nil, "assistant evidence = %+v, want no net tool-view delta", assistant.Evidence)
		testassert.Require(t, len(assistant.Evidence.HookIDs) == 2 && assistant.Evidence.HookIDs[0].HookID == "req.tools.narrow" && assistant.Evidence.HookIDs[1].HookID == "req.tools.restore", "assistant evidence hook identities = %+v, want both accepted hooks in configured order", assistant.Evidence.HookIDs)
	})
}

// TestRequestHookFrozenInputOwnership proves the input-ownership rows: the
// hook receives the frozen canonical projection with its canonical
// references, and mutating the delivered views cannot reach the dispatch or
// the durable state.
func TestRequestHookFrozenInputOwnership(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	var saw RequestHookInput
	observer := RequestHook{ID: "req.observe", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		saw = in
		for i := range in.Messages {
			for j := range in.Messages[i].Content {
				in.Messages[i].Content[j].Text = "mutated"
			}
		}
		in.Tools = append(in.Tools, model.ToolDefinition{Name: "extra", Parameters: json.RawMessage(`{"type":"object"}`)})
		in.Prompt += " mutated"
		return RequestConsequence{}, nil
	}}
	inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{observer}}
	_, sent, err := compositionFixtureRequest(t, h, c, effectExecution(nil, nil), inv, testCapture())
	testassert.NoError(t, err)
	testassert.Require(t, saw.RequestID != "" && saw.BoundaryEntryID != "", "hook input = %+v, want the bound request identity and canonical boundary", saw)
	testassert.Require(t, len(saw.Messages) == 1 && saw.Messages[0].TextContent() == "mutated", "hook mutation did not reach the delivered copy")
	testassert.Require(t, len(sent.Messages) == 2 && sent.Messages[1].TextContent() == "hello", "dispatch messages = %+v, want the frozen canonical input unchanged by hook mutation", sent.Messages)
	testassert.Require(t, len(sent.Tools) == 1 && sent.Tools[0].Name == "echo", "dispatch tools = %+v, want the captured catalog unchanged by hook mutation", sent.Tools)
	testassert.Require(t, strings.Contains(sent.Messages[0].TextContent(), "system") && !strings.Contains(sent.Messages[0].TextContent(), "mutated"), "dispatch prompt = %q, want the captured prompt unchanged", sent.Messages[0].TextContent())
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, len(assistant.Evidence.HookIDs) == 1 && assistant.Evidence.HookIDs[0].HookID == "req.observe" && assistant.Evidence.Prompt == nil && assistant.Evidence.Messages == nil, "assistant evidence = %+v, want the accepted observer identity with no contribution from an empty consequence", assistant.Evidence)
}

// TestHookFamiliesDisjointForSameIdentity proves the binding-isolation rows:
// a request-bound hook and a tool-bound hook configured with identical
// extension and hook identities never cross-bind — the request family records
// consumed evidence with no argument members, the tool family records consumed
// bytes with no evidence, and each family's provenance lands only on its own
// record.
func TestHookFamiliesDisjointForSameIdentity(t *testing.T) {
	sharedID := "shared.hook"
	sharedExt := "shared-ext"
	hookToolCall := model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}
	spy := &hookToolSpy{}
	exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		prompt := "shared request prompt"
		return InvocationExecution{
			Tool:          spy.tool,
			NormalizeTool: objectNormalize,
			RequestHooks: []RequestHook{{ID: sharedID, ExtensionID: sharedExt, Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
				return RequestConsequence{Prompt: &prompt}, nil
			}}},
			ToolHooks: []ToolArgumentsHook{{ID: sharedID, ExtensionID: sharedExt, Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
				return json.RawMessage(`{"x":2}`), nil
			}}},
		}, nil
	}}
	h, store, c, sessionID := newHookHarness(t, exec)
	inv, err := exec.OpenInvocation(context.Background(), InvocationRequest{ID: "inv-1", Purpose: InvocationPurposeConversation, Model: testCapture().Model})
	testassert.NoError(t, err)
	round := &conversationRound{}
	source := h.requestSource(c, testOpID, testCapture(), exec, inv, round)
	if _, err := source(context.Background()); err != nil {
		t.Fatalf("request source: %v", err)
	}
	inv.Model = func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(hookToolCall), nil
	}
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, testCapture(), round), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	{
		_, err := h.toolEffect(c, testOpID, exec, inv, testCapture())(context.Background(), hookToolCall, []string{"echo"})
		testassert.NoError(t, err)
	}

	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && len(assistant.Evidence.HookIDs) == 1 && assistant.Evidence.HookIDs[0].HookID == sharedID && assistant.Evidence.HookIDs[0].ExtensionID == sharedExt, "assistant evidence hook identities = %+v, want only the request-bound family record", assistant.Evidence.HookIDs)
	testassert.Require(t, assistant.Evidence.Prompt != nil && *assistant.Evidence.Prompt == "shared request prompt", "assistant evidence = %+v, want the request family's consumed prompt contribution", assistant.Evidence)

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var hookResult *hookResultEntry
	var toolResult *toolResultEntry
	for _, entry := range graph.Entries {
		if entry.HookResult != nil {
			hookResult = entry.HookResult
		}
		if entry.ToolResult != nil {
			toolResult = entry.ToolResult
		}
	}
	testassert.Require(t, hookResult != nil && hookResult.RequestID == "" && hookResult.ToolCallID == "call-1" && hookResult.InputEvidence == nil, "tool-bound hook result carries a request binding or consumed evidence: %+v", hookResult)
	testassert.Require(t, hookResult.InputArgumentsBase64 != "", "tool-bound hook result carries no consumed input bytes: %+v", hookResult)
	testassert.Require(t, toolResult != nil && len(toolResult.HookIDs) == 1 && toolResult.HookIDs[0].HookID == sharedID && toolResult.HookIDs[0].ExtensionID == sharedExt, "tool result hook provenance = %+v, want only the tool-bound family record", toolResult.HookIDs)
}

// TestArgumentHookChainPureRestoreKeepsAcceptedIdentities proves the chain
// provenance rule: every successfully accepted hook identity — pure
// replacements and an effectful hook alike, changed or restored — stays in the
// tool settlement's provenance in configured order, while an effectful hook
// always records its own result and retains the consumed intermediate bytes
// it actually consumed.
func TestArgumentHookChainPureRestoreKeepsAcceptedIdentities(t *testing.T) {
	original := json.RawMessage(`{"x":1}`)
	spy := &hookToolSpy{}
	exec := hookFixtureExecution(spy.tool, objectNormalize,
		ToolArgumentsHook{ID: "cap.pure.one", ExtensionID: "cap-a", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			return json.RawMessage(`{"x":9}`), nil
		}},
		ToolArgumentsHook{ID: "cap.pure.two", ExtensionID: "cap-a", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			return model.CloneRaw(original), nil // restore the original, not the preceding hook's replacement
		}},
		ToolArgumentsHook{ID: "cap.effectful", ExtensionID: "cap-a", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
			return json.RawMessage(`{"x":7}`), nil
		}},
	)
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, model.ToolCall{ID: "call-1", Name: "echo", Arguments: original})
	inv := openedConversation(t, exec)
	{
		_, err := h.toolEffect(c, testOpID, exec, inv, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
	}

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var hookResults []hookResultEntry
	var toolResult *toolResultEntry
	for _, entry := range graph.Entries {
		if entry.HookResult != nil {
			hookResults = append(hookResults, *entry.HookResult)
		}
		if entry.ToolResult != nil {
			toolResult = entry.ToolResult
		}
	}
	testassert.Require(t, len(hookResults) == 1 && hookResults[0].HookID == "cap.effectful", "committed hook results = %+v, want only the effectful hook's own result", hookResults)
	consumed, derr := base64.StdEncoding.DecodeString(hookResults[0].InputArgumentsBase64)
	testassert.Require(t, derr == nil && string(consumed) == string(original), "effectful hook result consumed input = %q, want the restored original bytes", consumed)
	testassert.Require(t, toolResult != nil && len(toolResult.HookIDs) == 3 && toolResult.HookIDs[0].HookID == "cap.pure.one" && toolResult.HookIDs[1].HookID == "cap.pure.two" && toolResult.HookIDs[2].HookID == "cap.effectful", "tool result hook provenance = %+v, want every accepted identity in configured order", toolResult.HookIDs)
	prepared := spy.received()
	testassert.Require(t, len(prepared) == 1 && string(prepared[0].Arguments) == `{"x":7}`, "preparer received %+v, want the effectful final value", prepared)
}

// TestMaintenanceFactsSettleOnce proves the maintenance provenance rows: a
// committed checkpoint carries the actual summary request input and the
// consumed evidence under the maintenance identity, and its Operation then
// carries no settlement-carried facts; a failed maintenance settles the same
// facts exactly once on the terminal instead.
func TestMaintenanceFactsSettleOnce(t *testing.T) {
	t.Run("committed checkpoint carries the facts", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := triggerUsageCapture(4)
		seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
		var summaryRequest model.Request
		exec := purposeExecution(
			func(context.Context, model.Request) (model.Stream, error) {
				return completedTurnStream(), nil
			},
			func(_ context.Context, r model.Request) (model.Stream, error) {
				summaryRequest = r
				return summaryStream("maintained", model.Usage{InputTokens: 5, OutputTokens: 2}), nil
			},
		)
		set, _, err := runTriggerRound(t, h, c, testOpID, exec, capture)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		var compaction *contextCheckpointEntry
		settlements := 0
		for _, entry := range graph.Entries {
			if entry.Compaction != nil {
				compaction = entry.Compaction
			}
			if entry.Settlement != nil {
				settlements++
			}
		}
		testassert.Require(t, compaction != nil, "no committed compaction entry")
		testassert.Require(t, len(compaction.Facts) == 1 && compaction.Facts[0].RequestID != "" && compaction.Facts[0].Input != nil && compaction.Evidence != nil && compaction.Evidence.BoundaryEntryID != "", "compaction entry provenance = %+v, want the maintenance identity, actual input, and consumed evidence", compaction)
		testassert.Require(t, reflect.DeepEqual(compaction.Facts[0].Input, summaryRequest.Messages), "compaction fact input = %+v, want the exact tool-free summary request the effect consumed", compaction.Facts[0].Input)
		for _, message := range compaction.Facts[0].Input {
			testassert.Require(t, len(message.ToolCalls) == 0, "compaction input carries tool members: %+v", message)
		}
		testassert.Equal(t, settlements, 0)
	})
	t.Run("failed maintenance settles facts on the terminal", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := triggerUsageCapture(4)
		seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
		exec := purposeExecution(
			func(context.Context, model.Request) (model.Stream, error) {
				t.Fatal("maintenance failure dispatched the conversation request")
				return nil, nil
			},
			func(context.Context, model.Request) (model.Stream, error) {
				return nil, errMaintenanceSummary
			},
		)
		_, _, err := runTriggerRound(t, h, c, testOpID, exec, capture)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "summary transport failure"), "round error = %v, want the summary failure's original cause", err)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		var settlement *operationSettlementEntry
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Compaction == nil, "maintenance failure committed a checkpoint: %+v", entry.Compaction)
			if entry.Settlement != nil {
				settlement = entry.Settlement
			}
		}
		testassert.Require(t, settlement != nil && len(settlement.Facts) > 0 && settlement.RequestID != "" && settlement.Evidence != nil, "settlement = %+v, want the maintenance facts settled on the terminal", settlement)
	})
	t.Run("checkpointed maintenance then failed maintenance attributes each producer once", func(t *testing.T) {
		// One Operation carries two maintenance producers: the first
		// checkpoint's facts and usage stay on the compaction entry, the
		// second maintenance settles its own facts once on the failure
		// terminal, and the cold graph read stays valid.
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := triggerUsageCapture(4)
		seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
		var summaryInputs [][]model.Message
		summaryCalls, dispatchCalls := 0, 0
		exec := purposeExecution(
			func(context.Context, model.Request) (model.Stream, error) {
				dispatchCalls++
				return completedTurnStream(), nil // reports {3,1,2}: above threshold 4
			},
			func(_ context.Context, r model.Request) (model.Stream, error) {
				summaryCalls++
				summaryInputs = append(summaryInputs, append([]model.Message(nil), r.Messages...))
				if summaryCalls == 1 {
					return summaryStream("M1", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
				}
				return nil, errMaintenanceSummary
			},
		)
		{
			set, _, err := runTriggerRound(t, h, c, testOpID, exec, capture)
			testassert.Require(t, err == nil && set.Disposition == agent.DispoReady, "first request: settlement %+v err %v, want the checkpointed maintenance ready", set, err)
		}
		{
			_, _, err := runTriggerRound(t, h, c, testOpID, exec, capture)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "summary transport failure"), "second request = %v, want the maintenance failure's original cause", err)
		}
		testassert.Equal(t, summaryCalls, 2)
		testassert.Equal(t, dispatchCalls, 1) // the failed maintenance dispatched no conversation request
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		var compaction *contextCheckpointEntry
		var settlement *operationSettlementEntry
		compactions, settlements := 0, 0
		for i := range graph.Entries {
			if graph.Entries[i].Compaction != nil {
				compaction = graph.Entries[i].Compaction
				compactions++
			}
			if graph.Entries[i].Settlement != nil {
				settlement = graph.Entries[i].Settlement
				settlements++
			}
		}
		testassert.Require(t, compactions == 1 && settlements == 1, "facts carriers = %d checkpoints, %d terminals, want each maintenance settled once", compactions, settlements)
		testassert.Require(t, compaction.Facts[0].RequestID != "" && settlement.RequestID != "" && compaction.Facts[0].RequestID != settlement.RequestID,
			"producers = checkpoint %q terminal %q, want two distinct maintenance identities", compaction.Facts[0].RequestID, settlement.RequestID)
		testassert.Require(t, reflect.DeepEqual(compaction.Facts[0].Input, summaryInputs[0]) && reflect.DeepEqual(settlement.Facts[0].Input, summaryInputs[1]),
			"facts carry summary inputs of the wrong producers: checkpoint %+v terminal %+v", compaction.Facts[0].Input, settlement.Facts[0].Input)
		testassert.Require(t, compaction.Facts[0].Usage != nil && compaction.Facts[0].Usage.InputTokens == 1,
			"checkpoint fact usage = %+v, want the producing summary's usage billed on its own entry", compaction.Facts[0].Usage)
		testassert.Require(t, graph.Operations[0].State.Status == OperationFailure && graph.Operations[0].State.Terminal != nil,
			"operation = %+v, want the cold graph terminal on the maintenance failure", graph.Operations[0].State)
	})
}

var (
	errMaintenanceSummary = errors.New("summary transport failure")
	errMaintenanceOpener  = errors.New("maintenance opener broke")
)

// TestRunningRequestSuspendAndClear proves the running-request lifecycle:
// the first conversation intent publishes the frozen request state with its
// canonical references, a pending tool batch keeps that request suspended
// across its tool settlement, and the quiet conversation result clears it.
func TestRunningRequestSuspendAndClear(t *testing.T) {
	exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Tool: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
			},
			NormalizeTool: objectNormalize,
		}, nil
	}}
	h, store, c, sessionID := newHookHarness(t, exec)
	inv := openedConversation(t, exec)
	inv.Model = func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}), nil
	}
	round := &conversationRound{}
	source := h.requestSource(c, testOpID, testCapture(), exec, inv, round)
	if _, err := source(context.Background()); err != nil {
		t.Fatalf("request source: %v", err)
	}
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, testCapture(), round), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(rec.State.PendingToolCalls) == 1, "operation state = %+v, want the published call reservation", rec.State)
	testassert.Require(t, rec.State.Request != nil && rec.State.Request.RequestID == round.requestID && rec.State.Request.BoundaryEntryID != "", "running request state = %+v, want the published frozen request", rec.State)

	{
		_, err := h.toolEffect(c, testOpID, exec, inv, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
	}
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(rec.State.PendingToolCalls) == 0, "operation state after the tool settlement = %+v, want the batch drained", rec.State)
	testassert.Require(t, rec.State.Request == nil, "running request state after the batch drained = %+v, want the request closed with no batch to suspend", rec.State)

	inv2 := openedConversation(t, exec)
	inv2.Model = func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(model.ToolCall{ID: "call-2", Name: "echo", Arguments: json.RawMessage(`{"x":2}`)}), nil
	}
	round2 := fixtureRound(h, c)
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv2, testCapture(), round2), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Request != nil && rec.State.Request.RequestID == round2.requestID && len(rec.State.PendingToolCalls) == 1, "operation state with a new pending batch = %+v, want the request published for its suspended batch", rec.State)
	{
		_, err := h.toolEffect(c, testOpID, exec, inv2, testCapture())(context.Background(), testToolCall("call-2"), []string{"echo"})
		testassert.NoError(t, err)
	}
	rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Request == nil && len(rec.State.PendingToolCalls) == 0, "operation state after the final batch = %+v, want the request cleared at the last settlement", rec.State)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestModelAttemptDeepOwnedRequestCopies proves the per-attempt ownership
// rule through the real effect pipeline: every physical attempt receives its
// own deep-owned copy of the logical dispatch request, a callback that
// mutates its copy (message text, tool list, extras) cannot reach the next
// attempt's view or the retained evidence, and the settled assistant still
// answers the untouched logical request.
func TestModelAttemptDeepOwnedRequestCopies(t *testing.T) {
	var seen []string
	calls := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		calls++
		if len(seen) > 0 {
			// A later attempt must observe the original request exactly.
			testassert.Require(t, seen[0] == seen[len(seen)-1], "attempt %d received a mutated request:\nwant %q\ngot  %q", calls, seen[0], seen[len(seen)-1])
		}
		if calls == 1 {
			testassert.Equal(t, len(seen), 1)
			return nil, errors.New("first attempt failed")
		}
		return completedTurnStream(), nil
	}
	exec := effectExecution(modelFn, nil)
	exec.Retry = (&retrySpy{limit: 1}).retry
	h, store, c, sessionID := newEffectHarness(t, nil)
	inv := InvocationExecution{Model: modelFn, NormalizeTool: objectNormalize}
	round := &conversationRound{}
	source := h.requestSource(c, testOpID, testCapture(), exec, inv, round)
	req, err := source(context.Background())
	testassert.NoError(t, err)
	// Record a fingerprint and mutate the callback-owned view at attempt one.
	inv.Model = func(_ context.Context, r model.Request) (model.Stream, error) {
		fingerprint := fmt.Sprintf("%d/%d/%s", len(r.Messages), len(r.Tools), r.Messages[len(r.Messages)-1].TextContent())
		seen = append(seen, fingerprint)
		r.Messages[len(r.Messages)-1].Content[0].Text = "mutated"
		r.Tools = append(r.Tools, model.ToolDefinition{Name: "extra", Parameters: json.RawMessage(`{"type":"object"}`)})
		return modelFn(context.TODO(), r)
	}
	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, testCapture(), round), req, nil)
	testassert.NoError(t, err)
	testassert.Equal(t, set.Disposition, agent.DispoReady)
	testassert.Equal(t, calls, 2)
	testassert.Require(t, len(seen) == 2 && seen[0] == seen[1], "attempt views differ after mutation: %v", seen)
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Evidence != nil && assistant.Evidence.BoundaryEntryID != "", "assistant evidence = %+v, want the untouched logical request evidence", assistant.Evidence)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// TestRequestHookConsequenceToolMembersRejectBeforeDispatch proves the shared
// message rule at the hook consequence gate: a consequence carrying tool
// execution members rejects before any view advance, the pure chain commits
// nothing, and the round dispatches nothing.
func TestRequestHookConsequenceToolMembersRejectBeforeDispatch(t *testing.T) {
	sneaky := model.ToolCall{ID: "call-9", Name: "echo", Arguments: json.RawMessage(`{}`)}
	hook := RequestHook{ID: "req.bad", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		return RequestConsequence{Messages: []model.Message{{Role: model.RoleAssistant, Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, ToolCalls: []model.ToolCall{sneaky}}}}, nil
	}}
	inv := InvocationExecution{NormalizeTool: objectNormalize, RequestHooks: []RequestHook{hook}}
	calls := 0
	exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
		calls++
		return completedTurnStream(), nil
	}, nil)
	h, store, c, sessionID := newEffectHarness(t, nil)
	_, _, err := compositionFixtureRequest(t, h, c, exec, inv, testCapture())
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "tool calls"), "round = %v, want the consequence rejection", err)
	testassert.Equal(t, calls, 0)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.Assistant == nil, "a rejected consequence still committed an assistant entry")
	}
}

func TestRequestMessageCopyOwnsHistoricalToolCalls(t *testing.T) {
	original := model.Message{
		Role: model.RoleAssistant, Source: testModelRef(),
		ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage("not json"), Extra: model.Extra{"meta": json.RawMessage(`{"n":1e0}`)}}},
	}
	owned := cloneMessageValue(original)
	owned.ToolCalls[0].Name = "changed"
	owned.ToolCalls[0].Arguments[0] = 'X'
	owned.ToolCalls[0].Extra["meta"][0] = '['
	owned.ToolCalls[0].Extra["injected"] = json.RawMessage(`true`)
	testassert.Check(t, original.ToolCalls[0].Name == "echo", "copied call scalar aliases the original")
	testassert.Check(t, string(original.ToolCalls[0].Arguments) == "not json", "copied raw arguments alias the original")
	testassert.Check(t, string(original.ToolCalls[0].Extra["meta"]) == `{"n":1e0}`, "copied extra bytes alias the original")
	testassert.Check(t, len(original.ToolCalls[0].Extra) == 1, "copied extra map aliases the original")
}

func TestMaterializedEmptyToolSelectionStaysEmpty(t *testing.T) {
	capture := testCapture()
	testassert.Require(t, len(capture.Tools) > 0, "the captured catalog must be nonempty")
	view := materializeRequestView(capture, nil, RequestEvidence{BoundaryEntryID: testEntryID, ToolNames: []string{}})
	testassert.Require(t, view.names != nil && len(view.effectiveTools()) == 0, "present-empty selection became the full catalog")
}

func TestAcceptedPureRequestIdentitiesRecordInOrder(t *testing.T) {
	capture := testCapture()
	for _, restored := range []bool{false, true} {
		view := newRequestView(capture, "", testEntryID, nil)
		reasserted := capture.SystemPrompt
		view.recordPure(HookIdentity{ExtensionID: "ext.test", HookID: "pure.noop"}, RequestConsequence{Prompt: &reasserted})
		if restored {
			changed, baseline := "changed", capture.SystemPrompt
			view.recordPure(HookIdentity{ExtensionID: "ext.test", HookID: "pure.change"}, RequestConsequence{Prompt: &changed})
			view.recordPure(HookIdentity{ExtensionID: "ext.test", HookID: "pure.restore"}, RequestConsequence{Prompt: &baseline})
		}
		evidence := view.evidenceFor()
		want := []HookIdentity{{ExtensionID: "ext.test", HookID: "pure.noop"}}
		if restored {
			want = append(want, HookIdentity{ExtensionID: "ext.test", HookID: "pure.change"}, HookIdentity{ExtensionID: "ext.test", HookID: "pure.restore"})
		}
		testassert.Check(t, reflect.DeepEqual(evidence.HookIDs, want), "restored=%v contributions = %+v, want every accepted identity in order", restored, evidence.HookIDs)
		testassert.Check(t, evidence.Prompt == nil && evidence.Messages == nil && evidence.ToolNames == nil, "restored=%v deltas = %+v, want none", restored, evidence)
	}
}

func TestArgumentRefusalDoesNotAcceptInvalidOriginal(t *testing.T) {
	spy := &hookToolSpy{}
	normalize := func(call model.ToolCall) (json.RawMessage, error) {
		if string(call.Arguments) == "not json" {
			return nil, errors.New("invalid original")
		}
		return objectNormalize(call)
	}
	hook := ToolArgumentsHook{ID: "pure.refuse", ExtensionID: "ext.test", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		if call.ID == "call-1" {
			return nil, errors.New("refused")
		}
		return model.CloneRaw(call.Arguments), nil
	}}
	exec := hookFixtureExecution(spy.tool, normalize, hook)
	h, store, c, sessionID := newHookHarness(t, exec)
	first, second := testToolCall("call-1"), testToolCall("call-2")
	first.Arguments = json.RawMessage("not json")
	publishHookCalls(t, h, c, exec, first, second)
	inv := openedConversation(t, exec)
	result, err := h.toolEffect(c, testOpID, exec, inv, testCapture())(context.Background(), first, []string{"echo"})
	testassert.NoError(t, err)
	testassert.Require(t, result.Status == model.ResultError, "refusal = %+v", result)
	result, err = h.toolEffect(c, testOpID, exec, inv, testCapture())(context.Background(), second, []string{"echo"})
	testassert.NoError(t, err)
	testassert.Require(t, result.Status == model.ResultSuccess, "later sibling = %+v", result)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil && entry.ToolResult.ToolCallID == first.ID {
			testassert.Require(t, len(entry.ToolResult.NormalizedArguments) == 0, "rejected raw bytes became accepted arguments")
		}
	}
	testassert.Require(t, len(spy.received()) == 1, "only the valid sibling may reach preparation")
}

func TestFinalArgumentHookCancellationPreventsPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spy := &hookToolSpy{}
	hook := ToolArgumentsHook{ID: "pure.cancel", ExtensionID: "ext.test", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(`{"x":9}`), nil
	}}
	exec := hookFixtureExecution(spy.tool, objectNormalize, hook)
	h, store, c, sessionID := newHookHarness(t, exec)
	call := testToolCall("call-1")
	publishHookCalls(t, h, c, exec, call)
	result, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(ctx, call, []string{"echo"})
	testassert.NoError(t, err)
	testassert.Check(t, len(spy.received()) == 0, "preparer ran after cancellation")
	testassert.Check(t, result.Status == model.ResultInterrupted, "result = %+v, want interruption", result)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			testassert.Require(t, string(entry.ToolResult.NormalizedArguments) == `{"x":9}`, "accepted repair was discarded")
		}
	}
}

func TestEffectfulNoopKeepsSurvivingPureArgumentIdentity(t *testing.T) {
	spy := &hookToolSpy{}
	pure := ToolArgumentsHook{ID: "pure.repair", ExtensionID: "ext.test", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`{"x":9}`), nil
	}}
	effect := ToolArgumentsHook{ID: "effect.noop", ExtensionID: "ext.test", Mode: HookModeEffectful, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		return model.CloneRaw(call.Arguments), nil
	}}
	exec := hookFixtureExecution(spy.tool, objectNormalize, pure, effect)
	h, store, c, sessionID := newHookHarness(t, exec)
	call := testToolCall("call-1")
	publishHookCalls(t, h, c, exec, call)
	_, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), call, []string{"echo"})
	testassert.NoError(t, err)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			want := []HookIdentity{{ExtensionID: pure.ExtensionID, HookID: pure.ID}, {ExtensionID: effect.ExtensionID, HookID: effect.ID}}
			testassert.Require(t, reflect.DeepEqual(entry.ToolResult.HookIDs, want), "surviving contributors = %+v, want %+v", entry.ToolResult.HookIDs, want)
		}
	}
}

func TestRequestEvidenceRequiresOrderedEffectfulIdentities(t *testing.T) {
	a := HookIdentity{ExtensionID: "ext.test", HookID: "effect.a"}
	b := HookIdentity{ExtensionID: "ext.test", HookID: "effect.b"}
	pure := HookIdentity{ExtensionID: "ext.test", HookID: "pure"}
	results := []graphEntry{
		{Envelope: Entry{ID: hexID(10), Sequence: 2}, HookResult: &hookResultEntry{ExtensionID: a.ExtensionID, HookID: a.HookID}},
		{Envelope: Entry{ID: hexID(11), Sequence: 3}, HookResult: &hookResultEntry{ExtensionID: b.ExtensionID, HookID: b.HookID}},
	}
	for _, tc := range []struct {
		name string
		ids  []HookIdentity
		seq  int64
		bad  bool
	}{
		{"complete", []HookIdentity{a, b}, 4, false},
		{"pure identities may interleave", []HookIdentity{pure, a, b}, 4, false},
		{"missing all", nil, 4, true},
		{"missing earlier", []HookIdentity{b}, 4, true},
		{"reversed", []HookIdentity{b, a}, 4, true},
		{"consumed prefix", []HookIdentity{a}, 3, false},
		{"future identity", []HookIdentity{a, b}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &graphValidation{}
			err := v.validateEvidenceHookOrder(tc.ids, results, graphEntry{Envelope: Entry{Sequence: tc.seq}})
			testassert.Require(t, (err != nil) == tc.bad, "order validation = %v, want bad=%v", err, tc.bad)
		})
	}
}

func TestContributionMessageEmptyNameIsUnnamed(t *testing.T) {
	m, err := decodeMessageValue(json.RawMessage(`{"role":"user","name":"","content":[{"kind":"text","text":"hello"}]}`))
	testassert.NoError(t, err)
	testassert.Equal(t, m.Name, "")
}

func TestEvidenceBoundaryDoesNotPredateItsCheckpoint(t *testing.T) {
	inputID, checkpointID, ownerID := hexID(1), hexID(2), hexID(3)
	v := &graphValidation{sessionID: testSessionID, entryByID: map[string]graphEntry{
		inputID:      {Envelope: Entry{ID: inputID, Sequence: 1}, Input: &inputEntry{}},
		checkpointID: {Envelope: Entry{ID: checkpointID, Sequence: 2}, Compaction: &contextCheckpointEntry{}},
	}}
	owner := graphEntry{Envelope: Entry{ID: ownerID, Sequence: 3}}
	evidence := &RequestEvidence{CheckpointEntryID: checkpointID, BoundaryEntryID: inputID}
	testassert.Require(t, v.validateEvidenceRefs("request", evidence, owner) != nil, "a covered pre-checkpoint boundary was accepted")
	evidence.BoundaryEntryID = checkpointID
	testassert.NoError(t, v.validateEvidenceRefs("request", evidence, owner))
}

// historicalProbe renders one stable fingerprint of a message list: role and
// text per message plus each tool call's raw argument bytes and marshalled
// tool-call extras, so a fixture can compare views byte-for-byte.
func historicalProbe(messages []model.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(string(m.Role))
		sb.WriteString("/")
		sb.WriteString(m.TextContent())
		for _, tc := range m.ToolCalls {
			extra, _ := json.Marshal(tc.Extra)
			fmt.Fprintf(&sb, " [%s %s %s]", tc.ID, string(tc.Arguments), extra)
		}
	}
	return sb.String()
}

// TestRequestHookHistoricalToolCallMutationStaysLocal proves the actual
// pipeline's deep ownership of historical conversation state: a pure request
// hook that rewrites the nested ToolCalls (arguments, extras) and text of the
// messages it receives cannot reach the later effectful hook's materialized
// view, the actual model dispatch request, or the canonical-only maintenance
// summary input, and the committed assistant record keeps its original bytes.
func TestRequestHookHistoricalToolCallMutationStaysLocal(t *testing.T) {
	originalPair := string(model.RoleAssistant) + "/done [call-1 " + `{"x":1}` + ` {"keep":1}` + "]"
	zero := 0
	pairStream := streamOf(model.StreamDelta{
		HasChoice: true,
		Role:      "assistant",
		ContentFragments: []model.ContentFragment{{
			Position: zero,
			Kind:     model.PartText,
			Text:     "done",
		}},
		ToolFragments: []model.ToolCallFragment{{
			Position:         &zero,
			ID:               "call-1",
			Name:             "echo",
			ArgumentFragment: `{"x":1}`,
			Extra:            model.Extra{"keep": json.RawMessage(`1`)},
		}},
		FinishReason: "tool_calls",
	}, usageDelta(model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}))
	roundA := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Model: func(context.Context, model.Request) (model.Stream, error) { return pairStream, nil },
			Tool: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "tool done"}}}
			},
			NormalizeTool: objectNormalize,
		}, nil
	}}
	h, store, c, sessionID := newHookHarness(t, roundA)
	capture := triggerUsageCapture(8)
	{
		set, _, err := runTriggerRound(t, h, c, testOpID, roundA, capture)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
	}
	{
		_, err := h.toolEffect(c, testOpID, roundA, openedConversation(t, roundA), capture)(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
	}

	var mutatorSawOriginal string
	var checkerView string
	var dispatchView string
	var summaryInput []model.Message
	mutator := RequestHook{ID: "req.mutator", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		mutatorSawOriginal = historicalProbe(in.Messages)
		for i := range in.Messages {
			m := &in.Messages[i]
			for j := range m.ToolCalls {
				m.ToolCalls[j].Arguments = json.RawMessage(`{"mutated":true}`)
				m.ToolCalls[j].Extra = model.Extra{"poisoned": json.RawMessage(`"leak"`)}
			}
			for k := range m.Content {
				m.Content[k].Text = "content mutated"
			}
		}
		return RequestConsequence{}, nil
	}}
	checker := RequestHook{ID: "req.checker", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		checkerView = historicalProbe(in.Messages)
		return RequestConsequence{}, nil
	}}
	roundB := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Model: func(_ context.Context, r model.Request) (model.Stream, error) {
				dispatchView = historicalProbe(r.Messages)
				return streamOf(turnDelta("stop", "mid"), usageDelta(model.Usage{InputTokens: 13})), nil
			},
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
			RequestHooks:  []RequestHook{mutator, checker},
		}, nil
	}}
	{
		set, _, err := runTriggerRound(t, h, c, testOpID, roundB, capture)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
	}
	// The mutating hook really received the seeded pair, then poisoned only
	// its own copy; the effectful hook's materialized view and the actual
	// dispatch still carry the committed bytes exactly.
	testassert.Require(t, strings.Contains(mutatorSawOriginal, originalPair), "mutator view = %q, want the seeded pair before mutating", mutatorSawOriginal)
	testassert.Require(t, strings.Contains(checkerView, originalPair), "checker view = %q, want the seeded pair byte-identical after the earlier hook's mutation", checkerView)
	testassert.Require(t, strings.Contains(dispatchView, originalPair), "dispatch view = %q, want the seeded pair byte-identical in the actual model request", dispatchView)

	// A later maintenance round summarizes the canonical-only projection of
	// the same pair; the summary input stays original for every field the
	// maintenance snapshot carries (tool execution members are stripped).
	roundC := Execution{OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "later"), usageDelta(model.Usage{InputTokens: 13})), nil
		}
		if request.Purpose == InvocationPurposeMaintenance {
			modelFn = func(_ context.Context, r model.Request) (model.Stream, error) {
				summaryInput = append([]model.Message(nil), r.Messages...)
				return summaryStream("M-hist", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
		}
		return InvocationExecution{
			Model:         modelFn,
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
		}, nil
	}}
	{
		set, _, err := runTriggerRound(t, h, c, testOpID, roundC, capture)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
	}
	testassert.Require(t, len(summaryInput) >= 2, "summary input = %+v, want the canonical projection of the seeded conversation", summaryInput)
	var transcript strings.Builder
	for _, m := range summaryInput {
		testassert.Require(t, len(m.ToolCalls) == 0, "summary input carries tool members: %+v", m)
		transcript.WriteString(m.TextContent())
	}
	// The canonical transcript carries the seeded rows in their stored form:
	// original texts and the original tool-call extra key, with none of the
	// hook's mutations.
	testassert.Require(t, strings.Contains(transcript.String(), `"done"`) && strings.Contains(transcript.String(), `"tool done"`) && strings.Contains(transcript.String(), `keep`),
		"summary transcript = %q, want the seeded conversation's original bytes", transcript.String())
	testassert.Require(t, !strings.Contains(transcript.String(), "mutated") && !strings.Contains(transcript.String(), "poisoned") && !strings.Contains(transcript.String(), "leak"),
		"summary transcript leaked a hook mutation: %q", transcript.String())

	// The durable rows are untouched: the committed assistant record keeps
	// the seeded argument bytes and tool-call extras.
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	seededKept := false
	for i := range graph.Entries {
		assistant := graph.Entries[i].Assistant
		if assistant == nil || len(assistant.ToolCalls) == 0 {
			continue
		}
		call := assistant.ToolCalls[0]
		raw, derr := base64.StdEncoding.DecodeString(call.ArgumentsBase64)
		testassert.NoError(t, derr)
		testassert.Require(t, string(raw) == `{"x":1}` && string(call.Extra["keep"]) == `1` &&
			len(assistant.Content) > 0 && assistant.Content[0].Text == "done",
			"committed tool call = %+v, want the original bytes and extras preserved", call)
		seededKept = true
	}
	testassert.Require(t, seededKept, "the graph carries no committed assistant pair")
	{
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.checker" && committed[0].Status == hookSucceeded,
			"hook results = %+v, want only the effectful checker's own committed effect", committed)
	}
}

// TestRequestHookPureRestoreMatchesBaselineTransactions proves the D2 pure
// request boundary through the actual pipeline: a pure change restored to the
// captured original by a later pure hook performs exactly the durable mutation
// steps of the identical hookless round — no hook_result entries, no extra
// register or entry transactions — and the dispatch keeps the original prompt.
func TestRequestHookPureRestoreMatchesBaselineTransactions(t *testing.T) {
	drive := func(t *testing.T, hooks ...RequestHook) (int, int, string) {
		exec := requestHookFixtureExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil, hooks...)
		h, store, c, sessionID := newHookHarness(t, exec)
		steps := 0
		store.txHook = func(string) error { steps++; return nil }
		{
			set, sent, err := runRequestHookPipeline(t, h, c, exec, testCapture())
			testassert.NoError(t, err)
			testassert.Equal(t, set.Disposition, agent.DispoReady)
			prompt := ""
			for _, m := range sent.Messages {
				if m.Role == model.RoleSystem {
					prompt = m.TextContent()
				}
			}
			return steps, len(hookGraphHookResults(t, store, sessionID)), prompt
		}
	}
	baseSteps, baseHooks, basePrompt := drive(t)
	var capturedOriginal string
	pureA := RequestHook{ID: "req.pure.one", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		capturedOriginal = in.Prompt
		changed := "pure changed " + in.Prompt
		return RequestConsequence{Prompt: &changed}, nil
	}}
	pureB := RequestHook{ID: "req.pure.two", ExtensionID: "req-ext", Mode: HookModePure, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
		restored := capturedOriginal // the captured original bytes, not an echo of the previous replacement
		return RequestConsequence{Prompt: &restored}, nil
	}}
	pureSteps, pureHooks, purePrompt := drive(t, pureA, pureB)
	testassert.Require(t, baseHooks == 0 && pureHooks == 0, "hook entries = baseline %d pure %d, want none", baseHooks, pureHooks)
	testassert.Require(t, pureSteps == baseSteps, "pure restore round performed %d durable mutation steps, want the baseline %d", pureSteps, baseSteps)
	testassert.Require(t, purePrompt == basePrompt && basePrompt != "", "dispatch prompt = %q, want the restored original %q", purePrompt, basePrompt)
}

// TestRequestHooksRunBeforeCanonicalOnlyMaintenance proves the installed-hook
// ordering through the full trigger pipeline: request hooks settle before the
// maintenance procedure runs, the maintenance input stays canonical-only
// without any hook contribution, the effectful hook's consumed evidence keeps
// the pure intermediate it actually consumed, the committed consequence is
// applied to the rebuilt projection exactly once, and the durable entries
// commit in hook → checkpoint → assistant order.
func TestRequestHooksRunBeforeCanonicalOnlyMaintenance(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	var events []string
	var consumedPrompt string
	var adopted string
	var summarySerialized string
	var dispatchPrompt string
	var dispatchHooked int
	pure := RequestHook{ID: "req.pure", ExtensionID: "req-ext", Mode: HookModePure, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		events = append(events, "pure")
		changed := "hooked " + in.Prompt
		return RequestConsequence{Prompt: &changed}, nil
	}}
	noop := RequestHook{ID: "req.effectful.noop", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		events = append(events, "effectful")
		consumedPrompt = in.Prompt
		reasserted := in.Prompt // the exact value the hook received: an effectful no-op
		return RequestConsequence{Prompt: &reasserted}, nil
	}}
	exec := Execution{OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
		modelFn := func(_ context.Context, r model.Request) (model.Stream, error) {
			events = append(events, "model")
			for _, m := range r.Messages {
				if m.Role == model.RoleSystem {
					dispatchPrompt = m.TextContent()
				}
				dispatchHooked += strings.Count(m.TextContent(), "hooked")
			}
			return completedTurnStream(), nil
		}
		if request.Purpose == InvocationPurposeMaintenance {
			modelFn = func(_ context.Context, r model.Request) (model.Stream, error) {
				events = append(events, "maintenance")
				var sb strings.Builder
				for _, m := range r.Messages {
					sb.WriteString(m.TextContent())
				}
				summarySerialized = sb.String()
				return summaryStream("M-hooks", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
		}
		return InvocationExecution{
			Model:         modelFn,
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
			RequestHooks:  []RequestHook{pure, noop},
		}, nil
	}}
	{
		set, _, err := runTriggerRound(t, h, c, testOpID, exec, capture)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
	}
	testassert.Require(t, len(events) == 4 && events[0] == "pure" && events[1] == "effectful" && events[2] == "maintenance" && events[3] == "model",
		"pipeline events = %v, want hooks before maintenance before rebuild-and-model", events)
	// The maintenance procedure ran over the canonical-only input: no hook
	// contribution reached the summary request.
	testassert.Require(t, !strings.Contains(summarySerialized, "hooked"), "maintenance input = %q, want the canonical-only projection without hook contributions", summarySerialized)
	// The effectful no-op consumed the pure intermediate and reasserted it;
	// the durable effect keeps that consumed evidence.
	committed := hookGraphHookResults(t, store, sessionID)
	testassert.Require(t, len(committed) == 1 && committed[0].HookID == "req.effectful.noop" && committed[0].Status == hookSucceeded,
		"hook results = %+v, want only the effectful no-op's durable effect", committed)
	testassert.Require(t, committed[0].InputEvidence != nil && committed[0].InputEvidence.Prompt != nil && *committed[0].InputEvidence.Prompt == consumedPrompt &&
		len(committed[0].InputEvidence.HookIDs) == 1 && committed[0].InputEvidence.HookIDs[0].HookID == "req.pure",
		"consumed evidence = %+v, want the pure intermediate prompt and identity retained at the effect boundary", committed[0].InputEvidence)
	adopted = *committed[0].Consequence.Prompt
	testassert.Require(t, adopted == consumedPrompt, "committed consequence = %q, want the codec-adopted reasserted value", adopted)
	// The frozen contribution applies exactly once to the rebuilt projection.
	testassert.Require(t, dispatchPrompt == adopted, "dispatch prompt = %q, want the adopted contribution", dispatchPrompt)
	testassert.Require(t, dispatchHooked == 1, "dispatch carries the contribution %d times, want exactly once", dispatchHooked)
	// Durable committed order: hook effect, checkpoint, then the answering assistant.
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	hookSeq, compactionSeq, assistantSeq := -1, -1, -1
	for _, entry := range graph.Entries {
		switch {
		case entry.HookResult != nil && entry.HookResult.RequestID != "":
			hookSeq = int(entry.Envelope.Sequence)
		case entry.Compaction != nil:
			compactionSeq = int(entry.Envelope.Sequence)
		case entry.Assistant != nil:
			if int(entry.Envelope.Sequence) > assistantSeq {
				assistantSeq = int(entry.Envelope.Sequence) // the round's answering assistant is the last
			}
		}
	}
	testassert.Require(t, hookSeq >= 0 && compactionSeq >= 0 && assistantSeq >= 0 && hookSeq < compactionSeq && compactionSeq < assistantSeq,
		"committed sequences = hook %d checkpoint %d assistant %d, want hooks before the checkpoint before the assistant", hookSeq, compactionSeq, assistantSeq)
	{
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	}
}

// maintenanceHookedRound runs one hooked automatic-maintenance round with two
// effectful request hooks and returns the storage the durable rows live in
// plus how many times the conversation transport ran. A non-nil summaryErr
// fails the maintenance summary; a non-nil openerErr fails the maintenance
// OpenInvocation before the summary effect is ever attempted.
func maintenanceHookedRound(t *testing.T, summaryErr, openerErr error) (*Harness, *graphStorage, string, int) {
	t.Helper()
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	e1 := RequestHook{ID: "req.effectful.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		prompt := "effect one " + in.Prompt
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	e2 := RequestHook{ID: "req.effectful.two", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
		prompt := "effect two " + in.Prompt
		return RequestConsequence{Prompt: &prompt}, nil
	}}
	conversationCalls := 0
	exec := Execution{OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		}
		if request.Purpose == InvocationPurposeMaintenance {
			if openerErr != nil {
				return InvocationExecution{}, openerErr
			}
			modelFn = func(context.Context, model.Request) (model.Stream, error) {
				if summaryErr != nil {
					return nil, summaryErr
				}
				return summaryStream("M-reconcile", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
		}
		return InvocationExecution{
			Model:         modelFn,
			Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			NormalizeTool: objectNormalize,
			RequestHooks:  []RequestHook{e1, e2},
		}, nil
	}}
	inv := openedConversation(t, exec)
	round := &conversationRound{}
	source := h.requestSource(c, testOpID, capture, exec, inv, round)
	req, err := source(context.Background())
	testassert.Require(t, err == nil || summaryErr != nil || openerErr != nil, "request source: %v", err)
	if summaryErr != nil || openerErr != nil {
		return h, store, sessionID, conversationCalls
	}
	{
		set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, inv, capture, round), req, nil)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
	}
	return h, store, sessionID, conversationCalls
}

// mutateStoredEvidenceHookIDs rewrites the hook identities recorded in one
// stored entry payload's consumed evidence, simulating durable evidence that
// no longer matches the committed effect executions it names its view from.
func mutateStoredEvidenceHookIDs(t *testing.T, store *graphStorage, sessionID string, kind EntryKind, hookIDs json.RawMessage) {
	t.Helper()
	entries := store.entries[sessionID]
	for i := range entries {
		if entries[i].Kind != kind {
			continue
		}
		var obj map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(entries[i].Payload, &obj))
		var evidence map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(obj["evidence"], &evidence))
		evidence["hook_ids"] = hookIDs
		evidenceRaw, err := json.Marshal(evidence)
		testassert.NoError(t, err)
		obj["evidence"] = evidenceRaw
		payload, err := json.Marshal(obj)
		testassert.NoError(t, err)
		entries[i].Payload = payload
		return
	}
	t.Fatalf("no stored %s entry to mutate", kind)
}

// removeStoredMember drops one member from a stored entry payload of the
// given kind, simulating a durable record written without it.
func removeStoredMember(t *testing.T, store *graphStorage, sessionID string, kind EntryKind, member string) {
	t.Helper()
	entries := store.entries[sessionID]
	for i := range entries {
		if entries[i].Kind != kind {
			continue
		}
		var obj map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(entries[i].Payload, &obj))
		if _, ok := obj[member]; !ok {
			t.Fatalf("stored %s entry carries no %s member", kind, member)
		}
		delete(obj, member)
		payload, err := json.Marshal(obj)
		testassert.NoError(t, err)
		entries[i].Payload = payload
		return
	}
	t.Fatalf("no stored %s entry", kind)
}

// TestAutomaticMaintenanceEvidenceReconcilesEffectfulIdentities proves the
// maintenance evidence is reconciled against the committed effectful hook
// executions of the conversation request whose view it actually consumed:
// omitting or reversing the recorded effectful identities in the durable
// checkpoint or failure-terminal evidence is corruption under cold validation.
func TestAutomaticMaintenanceEvidenceReconcilesEffectfulIdentities(t *testing.T) {
	t.Run("checkpoint evidence omitting an effectful identity corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, nil)
		mutateStoredEvidenceHookIDs(t, store, sessionID, EntryCompaction, json.RawMessage(`[]`))
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the omission to corrupt", err)
	})
	t.Run("checkpoint evidence reversing effectful identities corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, nil)
		mutateStoredEvidenceHookIDs(t, store, sessionID, EntryCompaction, json.RawMessage(`[{"extension_id":"req-ext","hook_id":"req.effectful.two"},{"extension_id":"req-ext","hook_id":"req.effectful.one"}]`))
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the reversal to corrupt", err)
	})
	t.Run("failure terminal evidence omitting an effectful identity corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, errMaintenanceSummary, nil)
		mutateStoredEvidenceHookIDs(t, store, sessionID, EntryOperationSettlement, json.RawMessage(`[]`))
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the omission to corrupt", err)
	})
	t.Run("checkpoint and failure terminal carry the conversation source apart from the producer", func(t *testing.T) {
		committedSource := func(entries map[string][]Entry, want string) string {
			for _, entry := range entries[want] {
				if entry.Kind != EntryHookResult {
					continue
				}
				h, err := decodeHookResultEntry(entry)
				testassert.NoError(t, err)
				if h.RequestID != "" {
					return h.RequestID
				}
			}
			t.Fatal("no committed request-bound hook result names the conversation request")
			return ""
		}
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, nil)
		sourceID := committedSource(store.entries, sessionID)
		records := 0
		for _, entry := range store.entries[sessionID] {
			if entry.Kind != EntryCompaction {
				continue
			}
			comp, err := decodeContextCheckpointEntry(entry)
			testassert.NoError(t, err)
			testassert.Require(t, comp.ConsumedRequestID == sourceID && comp.Facts[0].RequestID != "" && comp.Facts[0].RequestID != sourceID,
				"checkpoint identity = producer %q consumed %q, want the source %q kept distinct from the maintenance producer", comp.Facts[0].RequestID, comp.ConsumedRequestID, sourceID)
			records++
		}
		testassert.Equal(t, records, 1)
		_, failStore, failSession, _ := maintenanceHookedRound(t, errMaintenanceSummary, nil)
		failSource := committedSource(failStore.entries, failSession)
		settlements := 0
		for _, entry := range failStore.entries[failSession] {
			if entry.Kind != EntryOperationSettlement {
				continue
			}
			sett, err := decodeOperationSettlementEntry(entry)
			testassert.NoError(t, err)
			testassert.Require(t, len(sett.Facts) > 0 && sett.Evidence != nil, "maintenance failure settlement = %+v, want the retained facts", sett)
			testassert.Require(t, sett.ConsumedRequestID == failSource && sett.RequestID != "" && sett.RequestID != failSource,
				"settlement identity = producer %q consumed %q, want the source %q kept distinct from the maintenance producer", sett.RequestID, sett.ConsumedRequestID, failSource)
			settlements++
		}
		testassert.Equal(t, settlements, 1)
	})
	t.Run("stored checkpoint without its consumed source corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, nil)
		removeStoredMember(t, store, sessionID, EntryCompaction, "consumed_request_id")
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the missing automatic source to corrupt", err)
	})
	t.Run("opener-failed maintenance retains the source evidence without actual input", func(t *testing.T) {
		_, store, sessionID, dispatchCalls := maintenanceHookedRound(t, nil, errMaintenanceOpener)
		testassert.Equal(t, dispatchCalls, 0)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err) // the original graph stays valid
		sourceID := ""
		for _, entry := range store.entries[sessionID] {
			if entry.Kind != EntryHookResult {
				continue
			}
			h, herr := decodeHookResultEntry(entry)
			testassert.NoError(t, herr)
			if h.RequestID != "" {
				sourceID = h.RequestID
			}
		}
		testassert.Require(t, sourceID != "", "no committed request-bound hook result names the conversation request")
		settlements := 0
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Compaction == nil, "an opener-failed maintenance committed a checkpoint: %+v", entry.Compaction)
			if entry.Settlement == nil {
				continue
			}
			sett := entry.Settlement
			settlements++
			testassert.Require(t, len(sett.Facts) == 0, "opener-failed settlement invented actual input: %+v", sett.Facts)
			testassert.Require(t, sett.CompactionUsage && sett.Evidence != nil && len(sett.Evidence.HookIDs) == 2 &&
				sett.Evidence.HookIDs[0].HookID == "req.effectful.one" && sett.Evidence.HookIDs[1].HookID == "req.effectful.two",
				"opener-failed settlement = %+v, want the retained maintenance evidence provenance with both effectful contributors", sett)
			testassert.Require(t, sett.ConsumedRequestID == sourceID && sett.RequestID != "" && sett.RequestID != sourceID,
				"opener-failed settlement identity = producer %q consumed %q, want the source %q retained apart from the producer", sett.RequestID, sett.ConsumedRequestID, sourceID)
		}
		testassert.Equal(t, settlements, 1)
	})
	t.Run("opener-failed terminal evidence omitting its effectful provenance corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, errMaintenanceOpener)
		mutateStoredEvidenceHookIDs(t, store, sessionID, EntryOperationSettlement, json.RawMessage(`[]`))
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the opener-failure omission to corrupt", err)
	})
	t.Run("opener-failed terminal evidence reversing its effectful provenance corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, errMaintenanceOpener)
		mutateStoredEvidenceHookIDs(t, store, sessionID, EntryOperationSettlement, json.RawMessage(`[{"extension_id":"req-ext","hook_id":"req.effectful.two"},{"extension_id":"req-ext","hook_id":"req.effectful.one"}]`))
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the opener-failure reversal to corrupt", err)
	})
	t.Run("opener-failed terminal without its consumed source corrupts", func(t *testing.T) {
		_, store, sessionID, _ := maintenanceHookedRound(t, nil, errMaintenanceOpener)
		removeStoredMember(t, store, sessionID, EntryOperationSettlement, "consumed_request_id")
		_, err := validateFixture(t, store, sessionID)
		testassert.Require(t, err != nil && errors.Is(err, ErrCorrupt), "cold validation = %v, want the opener-failure missing source to corrupt", err)
	})
}
