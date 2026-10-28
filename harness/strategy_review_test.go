package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Review rows for the context strategy contract: the consumed evidence names
// the frozen input, none-after-model-work is an algorithm failure, the
// tool-free request gate accepts the empty tools slice, early failures settle
// with a valid producer witness, the binding/capture agreement holds before
// any strategy runs, only the injected system prompt is stripped, and
// request-only contribution messages never become checkpoint history.

// fixedCutStrategy is the replacement policy returning one immediate
// checkpoint at its configured covered boundary with the given messages.
type fixedCutStrategy struct {
	boundary string
	messages []model.Message
	planCall int
}

func (s *fixedCutStrategy) Plan(PlanInput) (PlanOutput, error) {
	s.planCall++
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{BoundaryEntryID: s.boundary, Items: authoredItems(s.messages...)}}, nil
}

func (s *fixedCutStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// noneStrategy plans nothing at all.
type noneStrategy struct{ planCall int }

func (s *noneStrategy) Plan(PlanInput) (PlanOutput, error) {
	s.planCall++
	return PlanOutput{Kind: PlanNone}, nil
}

func (s *noneStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// noneAfterModelStrategy runs one model pass and then folds to none.
type noneAfterModelStrategy struct{}

func (noneAfterModelStrategy) Plan(PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}},
	}}, nil
}

func (noneAfterModelStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// errStrategy fails its first Plan with the given cause.
type errStrategy struct{ cause error }

func (s errStrategy) Plan(PlanInput) (PlanOutput, error) { return PlanOutput{}, s.cause }
func (s errStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{}, s.cause
}

// malformedStrategy returns a plan outside the closed union.
type malformedStrategy struct{}

func (malformedStrategy) Plan(PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel}, nil // no variants at all
}
func (malformedStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel}, nil
}

// partialCutFixture builds the fixture Session whose frozen projection names
// the prior checkpoint hexID(3) and the frozen boundary hexID(6): the prior
// checkpoint covers up to hexID(1), and the tail carries one assistant and
// two operationless inputs — the partial cut hexID(5) sits between the prior
// checkpoint and the frozen boundary.
func partialCutFixture() *testGraph {
	fixture := validTestGraph()
	cutSignal := validSignalEntry(testOpID)
	cutSignal.EntryID = hexID(5)
	tailSignal := validSignalEntry(testOpID)
	tailSignal.EntryID = hexID(6)
	prior := validCompactionEntry(testOpID)
	prior.EntryID = hexID(3)
	prior.BoundaryEntryID = hexID(1)
	fixture.entries = append(fixture.entries,
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntrySignal, Sequence: 3, CommittedAt: testTime}, signal: &cutSignal},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 4, CommittedAt: testTime}, compaction: &prior},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntrySignal, Sequence: 5, CommittedAt: testTime}, signal: &tailSignal},
	)
	fixture.session.State.CheckpointEntryID = hexID(3)
	return fixture
}

// TestReviewConsumedEvidenceNamesFrozenInput proves the committed checkpoint
// evidence names the frozen input references — never the proposed cut: a
// replacement strategy's partial coverage cut pairs no boundary before its
// checkpoint entry and the committed graph validates.
func TestReviewConsumedEvidenceNamesFrozenInput(t *testing.T) {
	fixture := partialCutFixture()
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	strategy := &fixedCutStrategy{
		boundary: hexID(5), // the partial cut: strictly between the prior checkpoint and the frozen boundary
		messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("kept")}},
	}
	exec := compactionExecution(t, forbiddenConversationModel(t), nil)
	exec.Strategy = fixtureStrategyBinding(strategy)

	frozen, frozenCheckpoint, frozenBoundary, err := h.projectCanonical(c, testOpID)
	testassert.NoError(t, err)
	planInput := PlanInput{
		Trigger:           triggerManual,
		Messages:          frozen,
		CheckpointEntryID: frozenCheckpoint,
		BoundaryEntryID:   frozenBoundary,
	}
	checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
		&RunningRequest{RequestID: "request-conversation", CheckpointEntryID: hexID(3), BoundaryEntryID: hexID(6)})
	defer closeMaintenance()
	testassert.NoError(t, err)
	testassert.Require(t, checkpoint != nil, "the strategy planned no checkpoint")

	err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
	testassert.NoError(t, err)

	graph, err := validateFixture(t, store, testSessionID)
	testassert.Require(t, err == nil, "committed graph corrupts: %v", err)
	var comp *contextCheckpointEntry
	for i := range graph.Entries {
		if e := graph.Entries[i].Compaction; e != nil && e.EntryID != hexID(3) {
			comp = e
		}
	}
	testassert.Require(t, comp != nil, "the partial-cut checkpoint did not commit")
	testassert.Require(t, comp.Evidence != nil && comp.Evidence.CheckpointEntryID == hexID(3) && comp.Evidence.BoundaryEntryID == hexID(6),
		"committed evidence = %+v, want the frozen input pair (%q, %q)", comp.Evidence, hexID(3), hexID(6))
	testassert.Equal(t, comp.BoundaryEntryID, hexID(5)) // the proposed partial cut still commits as the covered boundary
}

// TestReviewNoneAfterModelWork proves one rule: a strategy that incurred
// model work and then planned none succeeds — manual maintenance settles its
// actual ordered facts on its existing success terminal, and automatic
// maintenance continues the waiting conversation while its uncommitted facts
// stay owned process-locally until the next checkpoint or terminal
// publication accounts them exactly once. An initial none with no model work
// stays a clean non-event.
func TestReviewNoneAfterModelWork(t *testing.T) {
	onePass := func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	}

	t.Run("manual model work then none settles the facts on the success terminal", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		exec := compactionExecution(t, onePass, nil)
		exec.Strategy = fixtureStrategyBinding(noneAfterModelStrategy{})

		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
			&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
		defer closeMaintenance()
		testassert.NoError(t, err)
		testassert.Require(t, checkpoint == nil, "none after model work produced a checkpoint")

		// The manual commit is the procedure's terminal: the actual ordered
		// facts settle on the success terminal exactly once.
		testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, nil, true))

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationSuccess && settlement.Detail == "", "settlement = %+v, want the success terminal", settlement)
		testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
			"settlement facts = %+v, want the one incurred fact with its reported usage", settlement.Facts)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, OutputTokens: 3}}}}
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the incurred usage charged exactly once", graph.Session.State.Usage)
		testassert.Require(t, usageTotalsEqual(operationStateOf(t, graph, testOpID).Usage, want), "operation usage = %+v, want the incurred usage charged exactly once", operationStateOf(t, graph, testOpID).Usage)
	})

	t.Run("automatic model work then none continues and the later checkpoint accounts the facts once", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		exec := compactionExecution(t, onePass, nil)
		exec.Strategy = fixtureStrategyBinding(noneAfterModelStrategy{})

		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
			&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
		defer closeMaintenance()
		testassert.NoError(t, err)
		testassert.Require(t, checkpoint == nil, "none after model work produced a checkpoint")

		// The waiting conversation continues: no settlement, no checkpoint,
		// the Operation keeps running.
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Settlement == nil && graph.Entries[i].Compaction == nil, "automatic none wrote durable work")
		}
		testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation must stay running after an automatic none")

		// The next checkpoint publication consumes the uncommitted facts
		// exactly once.
		inputID := fixtureInputEntryID(c)
		testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, summaryCheckpoint(t, "later summary", inputID), false))
		graph, err = validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Facts) == 1 && comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
			"checkpoint facts = %+v, want the earlier none's incurred fact accounted once", comp.Facts)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, OutputTokens: 3}}}}
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the incurred usage charged exactly once", graph.Session.State.Usage)
	})

	t.Run("initial none without model work stays a clean non-event", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		exec := compactionExecution(t, forbiddenConversationModel(t), nil)
		exec.Strategy = fixtureStrategyBinding(&noneStrategy{})

		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		checkpoint, _, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput, nil)
		defer closeMaintenance()
		testassert.NoError(t, err)
		testassert.Require(t, checkpoint == nil, "initial none produced a checkpoint")

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Settlement == nil && graph.Entries[i].Compaction == nil, "initial none wrote durable work")
		}
		testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation must stay running after an initial none")
	})
}

// TestReviewEarlyFailuresSettleWithProducerWitness proves every failure
// before any model plan still settles normally: the failure terminal carries
// the strategy's own detail, the consumed evidence, and a valid producer
// request identity — never a stuck running Operation behind a rolled-back
// settlement.
func TestReviewEarlyFailuresSettleWithProducerWitness(t *testing.T) {
	rows := []struct {
		name    string
		strategy ContextStrategy
		detail  string
	}{
		{"plan error before any model plan", errStrategy{cause: errors.New("strategy plan exploded")}, "strategy plan exploded"},
		{"malformed initial plan", malformedStrategy{}, "model plan requires exactly its model variant"},
		{"rejected immediate checkpoint", &fixedCutStrategy{boundary: hexID(9), messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("kept")}}}, "names no in-session entry"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := testCapture()
			capture.Strategy = fixtureStrategyCapture()
			exec := compactionExecution(t, forbiddenConversationModel(t), nil)
			exec.Strategy = fixtureStrategyBinding(row.strategy)

			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{
				Trigger:           triggerManual,
				Messages:          frozen,
				CheckpointEntryID: frozenCheckpoint,
				BoundaryEntryID:   frozenBoundary,
				Contributions:     &RequestEvidence{CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary},
			}
			checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
				&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
			defer closeMaintenance()
			if err == nil && checkpoint != nil {
				err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
			}
			testassert.Require(t, err != nil, "early failure returned success")

			graph, gerr := validateFixture(t, store, sessionID)
			testassert.Require(t, gerr == nil, "early failure left a corrupt graph: %v", gerr)
			settlement := settlementEntryOf(t, graph)
			testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, row.detail),
				"settlement = %+v, want the failure terminal carrying %q", settlement, row.detail)
			testassert.Require(t, settlement.Evidence != nil, "settlement = %+v, want the consumed evidence retained", settlement)
			testassert.Require(t, settlement.RequestID != "", "settlement = %+v, want the procedure producer identity", settlement)
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationFailure, "operation must be terminal, not stuck running")
		})
	}
}

// TestReviewToolFreeRequestGate proves the tool-free request rule accepts
// the nil and the non-nil empty tools slice identically and still rejects an
// advertised tool.
func TestReviewToolFreeRequestGate(t *testing.T) {
	request, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}})
	testassert.NoError(t, err)
	testassert.Require(t, request.Tools != nil, "fixture requires a non-nil empty tools slice, got %v", request.Tools)
	testassert.NoError(t, validatePlanOutput(PlanOutput{Kind: PlanModel, Model: &ModelPlan{Request: request}}))
	testassert.NoError(t, validatePlanOutput(PlanOutput{Kind: PlanModel, Model: &ModelPlan{Request: model.Request{Messages: request.Messages}}}))
	withTool := request
	withTool.Tools = []model.ToolDefinition{testToolDefinition()}
	testassert.Require(t, validatePlanOutput(PlanOutput{Kind: PlanModel, Model: &ModelPlan{Request: withTool}}) != nil, "an advertised tool must stay outside the tool-free maintenance request")
}

// TestReviewBindingAgreementBeforeStrategyRuns proves the selected binding
// and the capture's resolved strategy inputs agree once at the maintenance
// entry — before any strategy callback runs — even for a model-free strategy.
func TestReviewBindingAgreementBeforeStrategyRuns(t *testing.T) {
	h, _, c, _ := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture() // "test.strategy/v1"
	strategy := &fixedCutStrategy{boundary: fixtureInputEntryID(c), messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("kept")}}}
	exec := compactionExecution(t, forbiddenConversationModel(t), nil)
	exec.Strategy = &StrategyBinding{ID: "other.strategy/v2", Strategy: strategy}

	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	_, _, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput, nil)
	defer closeMaintenance()
	testassert.Require(t, err != nil, "a mismatched binding reached the maintenance procedure")
	testassert.Equal(t, strategy.planCall, 0)
}

// TestReviewStripsOnlyInjectedPrompt proves the canonical projection strips
// exactly the injected system prompt and never a checkpoint's own leading
// system message — with and without a captured prompt.
func TestReviewStripsOnlyInjectedPrompt(t *testing.T) {
	for _, capturedPrompt := range []string{"", "system prompt"} {
		name := "without a captured prompt"
		if capturedPrompt != "" {
			name = "with a captured prompt"
		}
		t.Run(name, func(t *testing.T) {
			fixture := validTestGraph()
			fixture.ops[0].Admission.Execution.SystemPrompt = capturedPrompt
			h := newTestHarness(t, fixture.storage(t), nil)
			c := leaseCoordinator(t, h, testSessionID)
			capture := fixture.ops[0].Admission.Execution
			inputID := fixtureInputEntryID(c)
			checkpoint := &CheckpointPlan{BoundaryEntryID: inputID, Items: authoredItems(
				model.Message{Role: model.RoleSystem, Content: admissionContent("checkpoint system message")},
				model.Message{Role: model.RoleUser, Content: admissionContent("kept")},
			)}
			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
			err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
			testassert.NoError(t, err)

			canonical, _, _, err := h.projectCanonical(c, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, len(canonical) == 3 && canonical[0].Role == model.RoleSystem && canonical[0].TextContent() == "checkpoint system message" && canonical[1].TextContent() == "kept",
				"canonical projection = %+v, want the checkpoint's own leading system message intact", canonical)
		})
	}
}

// TestReviewRetainAndContextSources proves the proposal source-resolution
// domain: the identical canonical and request-only values permit only the
// canonical source reference, explicitly authored context stays authored
// context and never an input event, and a source position can never address
// a request-only contribution.
func TestReviewRetainAndContextSources(t *testing.T) {
	t.Run("identical canonical and contribution values permit the canonical reference", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		testassert.Require(t, len(frozen) != 0, "the frozen view is empty")
		// The request contribution repeats the canonical message's exact
		// bytes; the canonical source position stays retainable.
		contribution := frozen[0]
		contributions := RequestEvidence{CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Messages: []model.Message{contribution}}
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Contributions: &contributions}
		checkpoint := &CheckpointPlan{BoundaryEntryID: inputID, Items: retainItems(0)}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "canonical retain corrupts: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == frozen[0].TextContent(),
			"checkpoint messages = %+v, want the retained canonical message", comp.Messages)
	})

	t.Run("explicit authored context equal to a contribution stays authored", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		contribution := frozen[0]
		contributions := RequestEvidence{CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Messages: []model.Message{contribution}}
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Contributions: &contributions}
		checkpoint := &CheckpointPlan{BoundaryEntryID: inputID, Items: authoredItems(contribution)}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "authored context corrupts: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == contribution.TextContent(),
			"checkpoint messages = %+v, want the authored context message", comp.Messages)
	})

	t.Run("a source position can never address a request-only contribution", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		contributions := RequestEvidence{CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("steered addition")}}}
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary, Contributions: &contributions}
		// The position just past the covered view would address the
		// contribution region; it resolves nothing.
		checkpoint := &CheckpointPlan{BoundaryEntryID: inputID, Items: retainItems(len(frozen))}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
		testassert.Require(t, err != nil, "an uncovered source position was accepted")
		assertNoCheckpointSettledFailure(t, h, store, c, sessionID)
	})
}

// TestReviewRetainResolution proves the Retain source resolution against the
// Harness-owned covered view: index zero and prior-checkpoint message
// positions resolve, positions are unique and strictly increasing and stay
// inside the covered view, and the Harness resolves them from its own frozen
// copy even when a callback mutated the values it received.
func TestReviewRetainResolution(t *testing.T) {
	t.Run("index zero resolves the covered view's first message", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID),
			&CheckpointPlan{BoundaryEntryID: inputID, Items: retainItems(0)}, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == frozen[0].TextContent(),
			"checkpoint messages = %+v, want the retained first message", comp.Messages)
	})

	t.Run("prior-checkpoint message positions resolve", func(t *testing.T) {
		fixture := partialCutFixture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		// The frozen view's head messages are the prior checkpoint's own.
		prior := validCompactionEntry(testOpID)
		testassert.Require(t, len(frozen) > len(prior.Messages), "frozen view = %+v, want the prior checkpoint messages and the tail", frozen)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		// Retain the prior checkpoint's messages and one tail message at the
		// partial cut.
		items := retainItems(0, len(prior.Messages))
		priorFacts := newMaintenanceFacts("request-conversation", frozenBoundary)
		priorFacts.maintenanceID = "maintenance-two"
		priorFacts.facts[0].RequestID = "maintenance-two"
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, priorFacts,
			&CheckpointPlan{BoundaryEntryID: hexID(5), Items: items}, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, testSessionID)
		testassert.Require(t, gerr == nil, "prior-checkpoint retain corrupts: %v", gerr)
		var comp *contextCheckpointEntry
		for i := range graph.Entries {
			if e := graph.Entries[i].Compaction; e != nil && e.EntryID != hexID(3) {
				comp = e
			}
		}
		testassert.Require(t, comp != nil && len(comp.Messages) == 2 &&
			comp.Messages[0].TextContent() == frozen[0].TextContent() &&
			comp.Messages[1].TextContent() == frozen[len(prior.Messages)].TextContent(),
			"checkpoint messages = %+v, want the retained prior-checkpoint and tail messages", comp.Messages)
	})

	t.Run("a retained message past the covered cut is rejected without a rebuilt-tail duplicate", func(t *testing.T) {
		fixture := partialCutFixture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := captureForHandoff()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		// The early cut at hexID(5) covers the first three frozen messages;
		// the fourth belongs to the kept tail and retaining it would
		// duplicate it there.
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		coveredFacts := newMaintenanceFacts("request-conversation", frozenBoundary)
		coveredFacts.maintenanceID = "maintenance-two"
		coveredFacts.facts[0].RequestID = "maintenance-two"
		err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, coveredFacts,
			&CheckpointPlan{BoundaryEntryID: hexID(5), Items: retainItems(0, 1, 2, 3)}, false)
		testassert.Require(t, err != nil, "a retained tail message past the covered cut was accepted")
		{
			graph, gerr := validateFixture(t, store, testSessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Compaction == nil || graph.Entries[i].Envelope.ID == hexID(3), "the rejected proposal committed a checkpoint entry")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationFailure, "operation state = %+v, want the strategy failure terminal", operationStateOf(t, graph, testOpID))
		}
		after, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, len(after) == len(before), "the rejected proposal changed the projection")
	})

	t.Run("the matching covered source commits with one rebuilt tail message", func(t *testing.T) {
		fixture := partialCutFixture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := captureForHandoff()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		coveredFacts := newMaintenanceFacts("request-conversation", frozenBoundary)
		coveredFacts.maintenanceID = "maintenance-two"
		coveredFacts.facts[0].RequestID = "maintenance-two"
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, coveredFacts,
			&CheckpointPlan{BoundaryEntryID: hexID(5), Items: retainItems(0, 1, 2)}, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, testSessionID)
		testassert.Require(t, gerr == nil, "the covered-source commit corrupts: %v", gerr)
		var comp *contextCheckpointEntry
		for i := range graph.Entries {
			if e := graph.Entries[i].Compaction; e != nil && e.EntryID != hexID(3) {
				comp = e
			}
		}
		testassert.Require(t, comp != nil && len(comp.Messages) == 3, "checkpoint messages = %+v, want exactly the covered source", comp)
		after, _, _, _, aerr := h.projectContextRefs(c, testOpID)
		testassert.NoError(t, aerr)
		after = after[1:] // the frozen view shape: the injected prompt stripped
		testassert.Require(t, len(after) == 4 && after[3].TextContent() == frozen[3].TextContent(), "rebuilt view = %+v, want the covered source and one rebuilt tail message", after)
	})

	t.Run("duplicate and decreasing positions are rejected", func(t *testing.T) {
		for _, items := range [][]CheckpointItem{retainItems(0, 0), retainItems(1, 0)} {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := testCapture()
			capture.Strategy = fixtureStrategyCapture()
			inputID := fixtureInputEntryID(c)
			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
			err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID),
				&CheckpointPlan{BoundaryEntryID: inputID, Items: items}, false)
			testassert.Require(t, err != nil, "positions %v were accepted", items)
			assertNoCheckpointSettledFailure(t, h, store, c, sessionID)
		}
	})

	t.Run("a mutating callback cannot rewrite the frozen source copy", func(t *testing.T) {
		strategy := &mutatingStrategy{}
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		exec := compactionExecution(t, forbiddenConversationModel(t), nil)
		exec.Strategy = fixtureStrategyBinding(strategy)

		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		pristine := frozen[0].TextContent()
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
			&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
		defer closeMaintenance()
		testassert.NoError(t, err)
		testassert.Require(t, checkpoint != nil, "the mutating strategy planned no checkpoint")
		err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
		testassert.NoError(t, err)

		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "the mutation-corrupted checkpoint committed: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == pristine,
			"checkpoint message = %+v, want the frozen source copy resolved unchanged (pristine %q)", comp.Messages, pristine)
		testassert.Require(t, planInput.Messages[0].TextContent() == pristine,
			"the frozen input copy was rewritten to %q", planInput.Messages[0].TextContent())
	})
}

// mutatingStrategy rewrites every frozen input value it receives and returns
// plans aliasing its outputs, exercising the callback composition isolation:
// the Harness resolves Retain and persists facts from its own frozen copies.
type mutatingStrategy struct{ passes int }

func (s *mutatingStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.passes++
	for i := range in.Messages {
		for j := range in.Messages[i].Content {
			in.Messages[i].Content[j].Text = "mutated by the callback"
		}
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.BoundaryEntryID,
		Items:           retainItems(0),
	}}, nil
}

func (s *mutatingStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}
