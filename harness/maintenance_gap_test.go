// Contract coverage for the incomplete-maintenance marker on the Operation
// state: the completed maintenance model intent's empty running result sets
// it, every later transition and recovery preserves it, and only a successful
// checkpoint or terminal publication that actually records the accumulated
// facts clears it and charges them once. The read estimates stay unknown
// across the gap, and no recovery replays or reconstructs the lost work.
package harness

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

const gapStrategyID = "test.gap/v1"

// gapStrategy drives one maintenance model pass and then either ends at none
// (transferring the incurred facts to the active execution), proposes a
// checkpoint, or fails its folding stage.
type gapStrategy struct {
	t              *testing.T
	noneAfterModel bool
	consumeErr     error
}

func (s *gapStrategy) Plan(in PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "gap pass"}}}}},
	}}, nil
}

func (s *gapStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	if s.consumeErr != nil {
		return PlanOutput{}, s.consumeErr
	}
	if s.noneAfterModel {
		return PlanOutput{Kind: PlanNone}, nil
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: summaryCheckpoint(s.t, "gap summary", in.Input.BoundaryEntryID)}, nil
}

// gapCapture prices both fixture models so the estimate rows are decidable.
func gapCapture() ExecutionCapture {
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: gapStrategyID}
	capture.Pricing = conversationPricing()
	capture.Compact.Pricing = compactPricing()
	return capture
}

// newGapHarness admits one running Operation like newEffectHarness, but under
// the priced gap capture so the estimate turns from unknown back to a known
// total once the marker is retired.
func newGapHarness(t *testing.T) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := emptyStore(t)
	prepared := PreparedExecution{Capture: gapCapture(), Open: func(context.Context, OperationAdmission) (Execution, error) {
		return validExecution(), nil
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

// gapExecution opens the conversation and maintenance invocations the gap
// fixtures drive, under the given strategy binding.
func gapExecution(t *testing.T, strategy ContextStrategy, compact func(context.Context, model.Request) (model.Stream, error), conversation func(context.Context, model.Request) (model.Stream, error), tool func(context.Context, model.ToolCall) PreparedTool) Execution {
	noop := func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	return Execution{
		Strategy: &StrategyBinding{ID: gapStrategyID, Strategy: strategy},
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			if request.Purpose == InvocationPurposeConversation {
				return invocationExecution(conversation, tool), nil
			}
			return invocationExecution(compact, noop), nil
		},
	}
}

// gapCompactFn answers the maintenance model with the completed summary pass;
// a nil usage reports no usage at all on the accepted stream.
func gapCompactFn(passes *int, usage *model.Usage) func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		*passes++
		if usage == nil {
			return streamOf(turnDelta("stop", "gap summary")), nil
		}
		return summaryStream("gap summary", *usage), nil
	}
}

func assertEstimatesUnknown(t *testing.T, h *Harness, sessionID string) {
	t.Helper()
	view, err := h.ReadSessionView(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, view.EstimatedCostUSD == nil, "session estimate = %v, want unknown while the maintenance gap stands", view.EstimatedCostUSD)
	for _, summary := range view.Operations {
		testassert.Require(t, summary.EstimatedCostUSD == nil, "operation %s estimate = %v, want unknown while the maintenance gap stands", summary.OperationID, summary.EstimatedCostUSD)
	}
}

func assertEstimateKnownUSD(t *testing.T, h *Harness, sessionID string, want float64) {
	t.Helper()
	view, err := h.ReadSessionView(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, view.EstimatedCostUSD != nil && math.Abs(*view.EstimatedCostUSD-want) <= 1e-9, "session estimate = %v, want %v after the facts were recorded", view.EstimatedCostUSD, want)
}

// TestMaintenanceCompletedResponseGap drives the actual maintenance procedure
// through the completed model pass and its empty running result: the writer
// sets the marker, the gap keeps both estimates unknown, a checkpoint commit
// records the facts exactly once, clears the marker, and prices them, while an
// unreported fact stays independently unknown.
func TestMaintenanceCompletedResponseGap(t *testing.T) {
	factUsage := model.Usage{InputTokens: 1000000} // compact model at the captured input rate 0.5: a 0.5 USD producer

	t.Run("the completed maintenance intent sets the marker and a checkpoint commit clears it", func(t *testing.T) {
		h, _, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, &factUsage), nil, nil)
		_, _, _, facts, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, compactSnapshotMessages(), nil, nil)
		defer closeInvocation()
		testassert.NoError(t, err)
		testassert.Equal(t, passes, 1)

		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && rec.State.UnreportedMaintenance,
			"operation state after the completed maintenance model intent = %+v, want the running marker set by the empty running result", rec.State)
		assertEstimatesUnknown(t, h, sessionID)

		inputID := fixtureInputEntryID(c)
		err = h.commitMaintenance(context.Background(), c, testOpID, capture, PlanInput{BoundaryEntryID: inputID}, facts, summaryCheckpoint(t, "gap summary", inputID), false)
		testassert.NoError(t, err)

		rec, err = h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationRunning && !rec.State.UnreportedMaintenance,
			"operation state after the committed checkpoint = %+v, want the marker cleared by the publication that recorded the facts", rec.State)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 1000000}}}}
		testassert.Require(t, usageTotalsEqual(rec.State.Usage, want), "operation usage = %+v, want the recorded facts charged once", rec.State.Usage)
		// The one reported fact priced by the captured compact input rate.
		assertEstimateKnownUSD(t, h, sessionID, 0.5)
	})

	t.Run("a reportless completed pass leaves the estimate unknown even after publication", func(t *testing.T) {
		h, _, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, nil), nil, nil)
		_, _, _, facts, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, compactSnapshotMessages(), nil, nil)
		defer closeInvocation()
		testassert.NoError(t, err)
		testassert.Require(t, len(facts.facts) == 1 && facts.facts[0].Usage == nil, "facts = %+v, want the incurred producer with its usage report absent", facts.facts)
		inputID := fixtureInputEntryID(c)
		err = h.commitMaintenance(context.Background(), c, testOpID, capture, PlanInput{BoundaryEntryID: inputID}, facts, summaryCheckpoint(t, "gap summary", inputID), false)
		testassert.NoError(t, err)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, !rec.State.UnreportedMaintenance, "operation state = %+v, want the marker cleared with the facts recorded", rec.State)
		// The published fact reports nothing: it stays independently unknown.
		assertEstimatesUnknown(t, h, sessionID)
	})
}

// TestMaintenanceGapAcrossTransitionsAndRecovery proves preservation and the
// publications that retire the marker: model work ending at none transfers
// the facts, ordinary model and tool transitions preserve the marker, the
// success terminal records the accumulated facts exactly once and clears it,
// a failed maintenance settlement records and clears, and a cold recovery
// without the process-local facts settles the ordinary interruption, preserves
// the marker on the terminal Operation, fabricates no facts, replays nothing,
// and leaves both estimates unknown.
func TestMaintenanceGapAcrossTransitionsAndRecovery(t *testing.T) {
	t.Run("model ending at none preserves the marker through later transitions until the fact publication", func(t *testing.T) {
		h, store, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		strategy := &gapStrategy{t: t, noneAfterModel: true}
		conversation := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		spy := &hookToolSpy{}
		exec := gapExecution(t, strategy, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), conversation, spy.tool)

		// The procedure runs manual so no conversation request is published
		// behind it: the ordinary model round below owns its own request.
		_, frozenCheckpoint, frozenBoundary, err := h.projectCanonical(c, testOpID)
		testassert.NoError(t, err)
		input := PlanInput{
			Trigger:           triggerManual,
			Messages:          cloneRequestMessages(compactSnapshotMessages()),
			CheckpointEntryID: frozenCheckpoint,
			BoundaryEntryID:   frozenBoundary,
			Instructions:      capture.Compact.SystemPrompt,
			Tools:             cloneToolDefinitions(capture.Tools),
			ContextWindow:     capture.ContextWindow,
			OutputReserve:     capture.OutputReserve,
		}
		_, _, closeInvocation, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, input, nil)
		defer closeInvocation()
		testassert.NoError(t, err)
		testassert.Require(t, len(c.pendingFacts) == 1, "pending facts = %+v, want the incurred facts transferred to the active execution", c.pendingFacts)

		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationRunning && rec.State.UnreportedMaintenance, "operation state after model->none = %+v, want the marker preserved", rec.State)
		assertEstimatesUnknown(t, h, sessionID)

		// The ordinary model transition of the next conversation round.
		publishCalls(t, h, c, sessionID, testToolCall("call-1"))
		{
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.UnreportedMaintenance && len(rec.State.PendingToolCalls) == 1, "operation state after the model transition = %+v, want the marker preserved with the published call", rec.State)
		}

		// The ordinary tool transition.
		_, err = h.toolEffect(c, testOpID, exec, openedConversation(t, exec), capture)(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		{
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.UnreportedMaintenance, "operation state after the tool settlement = %+v, want the marker preserved", rec.State)
		}

		// The success terminal publishes the accumulated facts once and clears it.
		_, err = h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
		testassert.NoError(t, err)
		testassert.Equal(t, passes, 1) // no replay of the maintenance model
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && settlement.Facts[0].Usage.InputTokens == 1000000,
			"settlement = %+v, want the transferred maintenance fact recorded exactly once", settlement)
		state := operationStateOf(t, graph, testOpID)
		testassert.Require(t, !state.UnreportedMaintenance, "terminal operation state = %+v, want the marker cleared by the fact publication", state)
		want := UsageTotals{ByModel: []ModelUsage{
			{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 1000000}},
			{Model: testModelRef(), Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}}, // the published round's reported assistant usage
		}}
		testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want the fact charged once beside the round's own usage", state.Usage)
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the fact charged once beside the round's own usage", graph.Session.State.Usage)
		// With the marker retired, the priced-out producers become decidable again.
		assertEstimateKnownUSD(t, h, sessionID, 0.5+0.000011) // compact 1M input at 0.5/M plus the round's 3/1/2 at 1/4/2 per M
	})

	t.Run("a failed maintenance settlement records the facts, clears the marker, and charges once", func(t *testing.T) {
		h, store, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		strategy := &gapStrategy{t: t, consumeErr: errors.New("folding stage failed")}
		exec := gapExecution(t, strategy, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), nil, nil)
		_, _, _, _, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, compactSnapshotMessages(), nil, nil)
		defer closeInvocation()
		testassert.Require(t, err != nil, "the procedure settled past the folding failure")
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationFailure && len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil,
			"settlement = %+v, want the incurred facts recorded on the failure terminal", settlement)
		state := operationStateOf(t, graph, testOpID)
		testassert.Require(t, !state.UnreportedMaintenance, "operation state = %+v, want the failure publication to clear the marker", state)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 1000000}}}}
		testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want the fact charged once", state.Usage)
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the fact charged once", graph.Session.State.Usage)
	})

	t.Run("a failed checkpoint publication retains the marker and recovery preserves it", func(t *testing.T) {
		h, store, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), nil, nil)
		_, _, _, facts, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, compactSnapshotMessages(), nil, nil)
		defer closeInvocation()
		testassert.NoError(t, err)
		store.txHook = func(string) error { return errors.New("injected checkpoint failure") }
		inputID := fixtureInputEntryID(c)
		err = h.commitMaintenance(context.Background(), c, testOpID, capture, PlanInput{BoundaryEntryID: inputID}, facts, summaryCheckpoint(t, "gap summary", inputID), false)
		testassert.Require(t, err != nil, "commitCheckpoint succeeded past an injected failure")
		testassert.Require(t, isPublicationFailure(err), "commitCheckpoint error = %v, want the marked publication failure", err)
		store.txHook = nil
		{
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.Status == OperationRunning && rec.State.UnreportedMaintenance,
				"operation state after the failed publication = %+v, want the rollback to retain the marker", rec.State)
		}
		{
			testassert.NoError(t, Recover(context.Background(), store))
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			testassert.Equal(t, passes, 1) // recovery replays nothing
			state := operationStateOf(t, graph, testOpID)
			testassert.Require(t, state.Status == OperationInterruption && state.UnreportedMaintenance,
				"recovered operation = %+v, want the terminal to preserve the marker without the lost facts", state)
			settlement := settlementEntryOf(t, graph)
			testassert.Require(t, len(settlement.Facts) == 0, "recovered settlement = %+v, want no reconstructed maintenance facts", settlement)
			for _, entry := range graph.Entries {
				testassert.Require(t, entry.Compaction == nil, "recovery published a checkpoint entry %s", entry.Envelope.ID)
			}
			reader := newTestHarness(t, store, nil)
			assertEstimatesUnknown(t, reader, sessionID)
		}
	})

	t.Run("a cold recovery inside the gap leaves both estimates unknown with no replay", func(t *testing.T) {
		h, store, c, sessionID := newGapHarness(t)
		capture := gapCapture()
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), nil, nil)
		_, _, _, _, closeInvocation, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, compactSnapshotMessages(), nil, nil)
		defer closeInvocation()
		testassert.NoError(t, err) // the procedure ends at its uncommitted checkpoint; no commit runs
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.UnreportedMaintenance, "operation state inside the gap = %+v, want the marker set", rec.State)

		testassert.NoError(t, Recover(context.Background(), store))
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		testassert.Equal(t, passes, 1) // no replay
		state := operationStateOf(t, graph, testOpID)
		testassert.Require(t, state.Status == OperationInterruption && state.UnreportedMaintenance,
			"recovered operation = %+v, want the quiet recovery interruption preserving the marker", state)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, len(settlement.Facts) == 0, "recovered settlement = %+v, want no fabricated success or facts", settlement)
		reader := newTestHarness(t, store, nil)
		assertEstimatesUnknown(t, reader, sessionID)
	})
}
