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
	observe        func() // runs at the completed-model folding boundary, before publication
}

func (s *gapStrategy) Plan(in PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "gap pass"}}}}},
	}}, nil
}

func (s *gapStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	if s.observe != nil {
		s.observe()
	}
	if s.consumeErr != nil {
		return PlanOutput{}, s.consumeErr
	}
	if s.noneAfterModel {
		return PlanOutput{Kind: PlanNone}, nil
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: summaryCheckpoint(s.t, "gap summary", in.Input.BoundaryEntryID)}, nil
}

// gapOnceStrategy runs one maintenance model pass on the first request round
// and plans none on every later round, so the incurred facts stay uncommitted
// while the conversation continues. The optional callbacks observe the real
// marker at the folding and next-request boundaries.
type gapOnceStrategy struct {
	t            *testing.T
	plans        int
	atConsume    func()
	atSecondPlan func()
}

func (s *gapOnceStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.plans++
	if s.plans == 1 {
		return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
			Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "gap pass"}}}}},
		}}, nil
	}
	if s.atSecondPlan != nil {
		s.atSecondPlan()
	}
	return PlanOutput{Kind: PlanNone}, nil
}

func (s *gapOnceStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	if s.atConsume != nil {
		s.atConsume()
	}
	return PlanOutput{Kind: PlanNone}, nil
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

// zeroUsageConversation answers the conversation model with a completed turn
// that reports an all-zero usage: the provider report is present, so it is a
// known priced-out zero and never leaves the estimate unknown.
func zeroUsageConversation() func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "quiet"), usageDelta(model.Usage{})), nil
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

	t.Run("the completed maintenance intent sets the marker and its publication clears it", func(t *testing.T) {
		h, _, c, sessionID := newGapHarness(t)
		passes := 0
		strategy := &gapStrategy{t: t}
		// The folding boundary is the real gap: the completed model effect has
		// published its marker and no checkpoint exists yet.
		strategy.observe = func() {
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && rec.State.UnreportedMaintenance,
				"operation state at the completed maintenance input = %+v, want the running marker set by the empty running result", rec.State)
			assertEstimatesUnknown(t, h, sessionID)
		}
		exec := gapExecution(t, strategy, gapCompactFn(&passes, &factUsage), zeroUsageConversation(), nil)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
		testassert.Equal(t, passes, 1)

		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationSuccess && !rec.State.UnreportedMaintenance,
			"operation state after the committed checkpoint = %+v, want the ordinary terminal with the marker cleared by the publication that recorded the facts", rec.State)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 1000000}}}}
		testassert.Require(t, usageTotalsEqual(UsageByModelForTest(rec.State.Usage, testCompactCapture().Model), want), "operation compact usage = %+v, want the recorded facts charged once", rec.State.Usage)
		// The one reported fact priced by the captured compact input rate.
		assertEstimateKnownUSD(t, h, sessionID, 0.5)
	})

	t.Run("a reportless completed pass leaves the estimate unknown even after publication", func(t *testing.T) {
		h, _, c, sessionID := newGapHarness(t)
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, nil), zeroUsageConversation(), nil)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
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
		passes := 0
		atConsume := func() {
			// The folding boundary is the real gap: the completed model effect
			// has published its marker and no checkpoint exists yet.
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && rec.State.UnreportedMaintenance,
				"operation state at the completed maintenance input = %+v, want the running marker set by the empty running result", rec.State)
			assertEstimatesUnknown(t, h, sessionID)
		}
		atSecondPlan := func() {
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.UnreportedMaintenance && len(rec.State.PendingToolCalls) == 0,
				"operation state at the next request boundary = %+v, want the marker preserved through the model and tool transitions", rec.State)
		}
		strategy := &gapOnceStrategy{t: t, atConsume: atConsume, atSecondPlan: atSecondPlan}
		rounds := 0
		conversation := func(context.Context, model.Request) (model.Stream, error) {
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil
			}
			return streamOf(turnDelta("stop", "quiet"), usageDelta(model.Usage{})), nil
		}
		tool := func(_ context.Context, call model.ToolCall) PreparedTool {
			// The ordinary model transition has already committed the call.
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.UnreportedMaintenance && len(rec.State.PendingToolCalls) == 1,
				"operation state after the model transition = %+v, want the marker preserved with the published call", rec.State)
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Evidence: ExecutionObserved, Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
			}}
		}
		exec := gapExecution(t, strategy, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), conversation, tool)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
		testassert.Equal(t, passes, 1) // no replay of the maintenance model
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && settlement.Facts[0].Usage.InputTokens == 1000000,
			"settlement = %+v, want the transferred maintenance fact recorded exactly once", settlement)
		state := operationStateOf(t, graph, testOpID)
		testassert.Require(t, state.Status == OperationSuccess && !state.UnreportedMaintenance, "terminal operation state = %+v, want the success terminal with the marker cleared by the fact publication", state)
		roundUsage := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}}}}
		testassert.Require(t, usageTotalsEqual(UsageByModelForTest(state.Usage, testCompactCapture().Model), UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 1000000}}}}), "operation usage = %+v, want the fact charged once", state.Usage)
		testassert.Require(t, usageTotalsEqual(UsageByModelForTest(state.Usage, testModelRef()), roundUsage), "operation usage = %+v, want the round's own reported usage", state.Usage)
		testassert.Require(t, usageTotalsEqual(UsageByModelForTest(graph.Session.State.Usage, testModelRef()), roundUsage), "session usage = %+v, want the round's own reported usage", graph.Session.State.Usage)
		// With the marker retired, the priced-out producers become decidable again.
		assertEstimateKnownUSD(t, h, sessionID, 0.5+0.000011) // compact 1M input at 0.5/M plus the round's 3/1/2 at 1/4/2 per M
	})

	t.Run("a failed maintenance settlement records the facts, clears the marker, and charges once", func(t *testing.T) {
		h, store, c, sessionID := newGapHarness(t)
		passes := 0
		strategy := &gapStrategy{t: t, consumeErr: errors.New("folding stage failed")}
		exec := gapExecution(t, strategy, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), zeroUsageConversation(), nil)
		err := runAutomaticMaintenance(t, h, c, exec)
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
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), zeroUsageConversation(), nil)
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryCompaction {
				return errors.New("injected checkpoint failure")
			}
			return nil
		}
		err := runAutomaticMaintenance(t, h, c, exec)
		testassert.Require(t, err != nil, "commitCheckpoint succeeded past an injected failure")
		testassert.Require(t, isPublicationFailure(err), "commitCheckpoint error = %v, want the marked publication failure", err)
		store.entryHook = nil
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
		passes := 0
		exec := gapExecution(t, &gapStrategy{t: t}, gapCompactFn(&passes, &model.Usage{InputTokens: 1000000}), zeroUsageConversation(), nil)
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryCompaction { // the checkpoint publication is exactly the gap boundary
				return errors.New("injected checkpoint failure")
			}
			return nil
		}
		err := runAutomaticMaintenance(t, h, c, exec)
		testassert.Require(t, err != nil, "the checkpoint publication succeeded past the injected failure")
		store.entryHook = nil
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.UnreportedMaintenance, "operation state inside the gap = %+v, want the marker set", rec.State)
		assertEstimatesUnknown(t, h, sessionID)

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
