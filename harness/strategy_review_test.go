package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Review rows for the context strategy contract: the consumed evidence names
// the frozen input, none after model work settles its facts exactly once, the
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

func (s *fixedCutStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	s.planCall++
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{BoundaryEntryID: s.boundary, Items: authoredItems(s.messages...)}}, nil
}

func (s *fixedCutStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// noneStrategy plans nothing at all.
type noneStrategy struct{ planCall int }

func (s *noneStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	s.planCall++
	return PlanOutput{Kind: PlanNone}, nil
}

func (s *noneStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// noneAfterModelStrategy runs one model pass and then folds to none.
type noneAfterModelStrategy struct{}

func (noneAfterModelStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}},
	}}, nil
}

func (noneAfterModelStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// errStrategy fails its first Plan with the given cause.
type errStrategy struct{ cause error }

func (s errStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{}, s.cause
}
func (s errStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{}, s.cause
}

// malformedStrategy returns a plan outside the closed union.
type malformedStrategy struct{}

func (malformedStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel}, nil // no variants at all
}
func (malformedStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
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
	graph, err := runCheckpointProposal(t, partialCutFixture(), func(PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
	})
	testassert.NoError(t, err)
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

// TestReviewNoneAfterModelWork proves one rule through real runs: a strategy
// that incurred model work and then planned none succeeds — manual maintenance
// settles its actual ordered facts on its existing success terminal, and
// automatic maintenance continues the waiting conversation with its
// uncommitted facts accounted exactly once at the next checkpoint. An initial
// none with no model work stays a clean non-event.
func TestReviewNoneAfterModelWork(t *testing.T) {
	onePass := func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	}

	t.Run("manual model work then none settles the facts on the success terminal", func(t *testing.T) {
		exec := purposeExecution(forbiddenConversationModel(t), onePass)
		exec.Strategy = fixtureStrategyBinding(noneAfterModelStrategy{})
		store := emptyStore(t)
		prepared := compactPrepared(captureForHandoff(), exec)
		h := newCompactHarness(t, store, prepared)
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		testassert.NoError(t, runManualCompact(t, h, c, session.Identity.SessionID, "compact-1"))

		graph, err := validateFixture(t, store, session.Identity.SessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationSuccess && settlement.Detail == "", "settlement = %+v, want the success terminal", settlement)
		testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
			"settlement facts = %+v, want the one incurred fact with its reported usage", settlement.Facts)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, OutputTokens: 3}}}}
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the incurred usage charged exactly once", graph.Session.State.Usage)
		testassert.Require(t, usageTotalsEqual(operationStateOf(t, graph, "compact-1").Usage, want), "operation usage = %+v, want the incurred usage charged exactly once", operationStateOf(t, graph, "compact-1").Usage)
	})

	t.Run("automatic model work then none continues and the later checkpoint accounts the facts once", func(t *testing.T) {
		strategy := &noneThenCheckpointStrategy{}
		conversation := func() func(context.Context, model.Request) (model.Stream, error) {
			rounds := 0
			return func(context.Context, model.Request) (model.Stream, error) {
				rounds++
				if rounds == 1 {
					return streamOf(turnDelta("tool_calls", "work", testToolCall("call-1"))), nil
				}
				return streamOf(turnDelta("stop", "quiet")), nil
			}
		}()
		tool := func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		exec := purposeExecution(conversation, onePass)
		opened := exec.OpenInvocation
		exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
			inv, err := opened(ctx, request)
			if err != nil {
				return inv, err
			}
			if request.Purpose == InvocationPurposeConversation {
				inv.Tool = tool
			}
			return inv, nil
		}
		exec.Strategy = fixtureStrategyBinding(strategy)
		h, store, c, sessionID := strategyHarness(t, exec)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))

		// The waiting conversation continued through its tool round; the next
		// checkpoint publication consumed the uncommitted facts exactly once.
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Facts) == 1 && comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
			"checkpoint facts = %+v, want the earlier none's incurred fact accounted once", comp.Facts)
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, OutputTokens: 3}}}}
		testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the incurred usage charged exactly once", graph.Session.State.Usage)
	})

	t.Run("initial none without model work stays a clean non-event", func(t *testing.T) {
		exec := purposeExecution(quietConversation(), forbiddenMaintenanceModel(t))
		exec.Strategy = fixtureStrategyBinding(&noneStrategy{})
		h, store, c, sessionID := strategyHarness(t, exec)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil, "initial none wrote a checkpoint")
		}
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationSuccess && len(settlement.Facts) == 0 && settlement.Usage == nil,
			"settlement = %+v, want the ordinary clean terminal with no maintenance facts", settlement)
	})
}

// noneThenCheckpointStrategy runs one model pass and folds to none on the
// first request round, then proposes the checkpoint on the next round.
type noneThenCheckpointStrategy struct{ plans int }

func (s *noneThenCheckpointStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.plans++
	if s.plans == 1 {
		return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
			Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}},
		}}, nil
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.BoundaryEntryID,
		Items:           authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("later summary")}),
	}}, nil
}

func (s *noneThenCheckpointStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// TestReviewEarlyFailuresSettleWithProducerWitness proves every failure
// before any model plan still settles normally through the real automatic
// round: the failure terminal carries the strategy's own detail, the consumed
// evidence, and a valid producer request identity — never a stuck running
// Operation behind a rolled-back settlement.
func TestReviewEarlyFailuresSettleWithProducerWitness(t *testing.T) {
	rows := []struct {
		name     string
		strategy ContextStrategy
		detail   string
	}{
		{"plan error before any model plan", errStrategy{cause: errors.New("strategy plan exploded")}, "strategy plan exploded"},
		{"malformed initial plan", malformedStrategy{}, "model plan requires exactly its model variant"},
		{"rejected immediate checkpoint", &fixedCutStrategy{boundary: hexID(9), messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("kept")}}}, "names no in-session entry"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			exec := purposeExecution(quietConversation(), forbiddenMaintenanceModel(t))
			exec.Strategy = fixtureStrategyBinding(row.strategy)
			h, store, c, sessionID := strategyHarness(t, exec)
			err := runAutomaticMaintenance(t, h, c, exec)
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
// entry through the real automatic round — before any strategy callback runs —
// even for a model-free strategy.
func TestReviewBindingAgreementBeforeStrategyRuns(t *testing.T) {
	strategy := &fixedCutStrategy{boundary: hexID(1), messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("kept")}}}
	exec := purposeExecution(quietConversation(), forbiddenMaintenanceModel(t))
	exec.Strategy = &StrategyBinding{ID: "other.strategy/v2", Strategy: strategy}
	h, store, c, sessionID := strategyHarness(t, exec)
	err := runAutomaticMaintenance(t, h, c, exec)
	testassert.Require(t, err != nil, "a mismatched binding reached the maintenance procedure")
	testassert.Equal(t, strategy.planCall, 0)
	graph, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationFailure, "operation = %+v, want the binding-mismatch failure", operationStateOf(t, graph, testOpID))
}

// TestReviewStripsOnlyInjectedPrompt proves the canonical projection strips
// exactly the injected system prompt and never a checkpoint's own leading
// system message — with and without a captured prompt — through the real
// manual compact owner.
func TestReviewStripsOnlyInjectedPrompt(t *testing.T) {
	for _, capturedPrompt := range []string{"", "system prompt"} {
		name := "without a captured prompt"
		if capturedPrompt != "" {
			name = "with a captured prompt"
		}
		t.Run(name, func(t *testing.T) {
			store := idleFixture(t, validTestGraph(), hexID(20))
			capture := captureForHandoff()
			capture.SystemPrompt = capturedPrompt
			exec := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
			exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
				return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(
					model.Message{Role: model.RoleSystem, Content: admissionContent("checkpoint system message")},
					model.Message{Role: model.RoleUser, Content: admissionContent("kept")},
				)}
			}})
			prepared := compactPrepared(capture, exec)
			h := newCompactHarness(t, store, prepared)
			c := leaseCoordinator(t, h, testSessionID)
			testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))

			canonical, _, _, err := h.projectCanonical(c, "compact-1")
			testassert.NoError(t, err)
			testassert.Require(t, len(canonical) == 2 && canonical[0].Role == model.RoleSystem && canonical[0].TextContent() == "checkpoint system message" && canonical[1].TextContent() == "kept",
				"canonical projection = %+v, want the checkpoint's own leading system message intact", canonical)
		})
	}
}

// TestReviewRetainAndContextSources proves the proposal source-resolution
// domain through real automatic rounds carrying actual request-hook
// contributions: the identical canonical and request-only values permit only
// the canonical source reference, explicitly authored context stays authored
// context and never an input event, and a source position can never address a
// request-only contribution.
func TestReviewRetainAndContextSources(t *testing.T) {
	t.Run("identical canonical and contribution values permit the canonical reference", func(t *testing.T) {
		hook := RequestHook{ID: "req.contribute", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{Messages: []model.Message{in.CanonicalMessages[0]}}, nil
		}}
		exec := requestHookExecution(purposeExecution(quietConversation(), forbiddenMaintenanceModel(t)), hook)
		exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(0)}
		}})
		h, store, c, sessionID := strategyHarness(t, exec)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "canonical retain corrupts: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "hello",
			"checkpoint messages = %+v, want the retained canonical message", comp.Messages)
	})

	t.Run("explicit authored context equal to a contribution stays authored", func(t *testing.T) {
		hook := RequestHook{ID: "req.contribute", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(_ context.Context, in RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{Messages: []model.Message{in.CanonicalMessages[0]}}, nil
		}}
		exec := requestHookExecution(purposeExecution(quietConversation(), forbiddenMaintenanceModel(t)), hook)
		exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(in.Messages[0])}
		}})
		h, store, c, sessionID := strategyHarness(t, exec)
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "authored context corrupts: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "hello",
			"checkpoint messages = %+v, want the authored context message", comp.Messages)
	})

	t.Run("a source position can never address a request-only contribution", func(t *testing.T) {
		hook := RequestHook{ID: "req.contribute", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("steered addition")}}}, nil
		}}
		exec := requestHookExecution(purposeExecution(quietConversation(), forbiddenMaintenanceModel(t)), hook)
		exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(len(in.Messages))}
		}})
		h, store, c, sessionID := strategyHarness(t, exec)
		err := runAutomaticMaintenance(t, h, c, exec)
		testassert.Require(t, err != nil, "an uncovered source position was accepted")
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
}

// TestReviewRetainResolution proves the Retain source resolution against the
// Harness-owned covered view through real automatic rounds: index zero and
// prior-checkpoint message positions resolve, positions are unique and
// strictly increasing and stay inside the covered view, and the Harness
// resolves them from its own frozen copy even when a callback mutated the
// values it received.
func TestReviewRetainResolution(t *testing.T) {
	proposal := func(fn func(PlanInput) *CheckpointPlan) Execution {
		exec := purposeExecution(quietConversation(), forbiddenMaintenanceModel(t))
		exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: fn})
		return exec
	}
	t.Run("index zero resolves the covered view's first message", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, validTestGraph(), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(0)}
		})
		testassert.NoError(t, err)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "hello",
			"checkpoint messages = %+v, want the retained first message", comp.Messages)
	})

	t.Run("prior-checkpoint message positions resolve", func(t *testing.T) {
		fixture := partialCutFixture()
		fixture.ops[0].Admission.Execution.Strategy = fixtureStrategyCapture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		frozen, _, _, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		prior := validCompactionEntry(testOpID)
		testassert.Require(t, len(frozen) > len(prior.Messages), "frozen view = %+v, want the prior checkpoint messages and the tail", frozen)
		exec := proposal(func(PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: retainItems(0, len(prior.Messages))}
		})
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
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
		graph, err := runCheckpointProposal(t, partialCutFixture(), func(PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: retainItems(0, 1, 2, 3)}
		})
		testassert.Require(t, err != nil, "a retained tail message past the covered cut was accepted")
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil || graph.Entries[i].Envelope.ID == hexID(3), "the rejected proposal committed a checkpoint entry")
		}
		testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationFailure, "operation state = %+v, want the strategy failure terminal", operationStateOf(t, graph, testOpID))
	})

	t.Run("the matching covered source commits with one rebuilt tail message", func(t *testing.T) {
		fixture := partialCutFixture()
		fixture.ops[0].Admission.Execution.Strategy = fixtureStrategyCapture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		frozen, _, _, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		exec := proposal(func(PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: retainItems(0, 1, 2)}
		})
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
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
		testassert.Require(t, len(after) == 5 && after[3].TextContent() == frozen[3].TextContent() && after[4].TextContent() == "quiet",
			"rebuilt view = %+v, want the covered source, one rebuilt tail message and the quiet conversation turn", after)
	})

	t.Run("duplicate and decreasing positions are rejected", func(t *testing.T) {
		for _, items := range [][]CheckpointItem{retainItems(0, 0), retainItems(1, 0)} {
			graph, err := runCheckpointProposal(t, validTestGraph(), func(in PlanInput) *CheckpointPlan {
				return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: items}
			})
			testassert.Require(t, err != nil, "positions %v were accepted", items)
			assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
		}
	})

	t.Run("a mutating callback cannot rewrite the frozen source copy", func(t *testing.T) {
		strategy := &mutatingStrategy{}
		addition := model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "addition", Extra: model.Extra{"raw": json.RawMessage(` { "n" : 1 } `)}}}}
		hook := RequestHook{ID: "req.contribute", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			return RequestConsequence{Messages: []model.Message{addition}}, nil
		}}
		exec := requestHookExecution(purposeExecution(quietConversation(), forbiddenMaintenanceModel(t)), hook)
		exec.Strategy = fixtureStrategyBinding(strategy)
		h, store, c, sessionID := strategyHarness(t, exec)
		frozen, _, _, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		pristine := frozen[0].TextContent()
		testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))

		graph, gerr := validateFixture(t, store, sessionID)
		testassert.Require(t, gerr == nil, "the mutation-corrupted checkpoint committed: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == pristine,
			"checkpoint message = %+v, want the frozen source copy resolved unchanged (pristine %q)", comp.Messages, pristine)
		testassert.Require(t, frozen[0].TextContent() == pristine,
			"the frozen input copy was rewritten to %q", frozen[0].TextContent())
		testassert.Require(t, comp.Evidence != nil && comp.Evidence.Prompt == captureForHandoff().SystemPrompt &&
			reflect.DeepEqual(comp.Evidence.ToolNames, []string{"echo"}) &&
			len(comp.Evidence.Messages) == 1 && comp.Evidence.Messages[0].TextContent() == "addition" &&
			string(comp.Evidence.Messages[0].Content[0].Extra["raw"]) == ` { "n" : 1 } `,
			"strategy changed the accepted contribution: evidence=%+v", comp.Evidence)
	})
}

// mutatingStrategy rewrites every frozen input value it receives and returns
// plans aliasing its outputs, exercising the callback composition isolation:
// the Harness resolves Retain and persists facts from its own frozen copies.
type mutatingStrategy struct{ passes int }

func (s *mutatingStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.passes++
	if in.Contributions != nil {
		in.Contributions.Prompt = "mutated"
		in.Contributions.ToolNames[0] = "mutated"
		in.Contributions.Messages[0].Content[0].Text = "mutated"
		in.Contributions.Messages[0].Content[0].Extra["raw"][0] = 'x'
	}
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

func (s *mutatingStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}
