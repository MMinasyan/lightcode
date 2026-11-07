package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// The replacement-strategy fixture: one test binding exercising the context
// strategy contract end to end — two model plans folding into one
// non-summary checkpoint, the ordered maintenance facts carrying both
// producers exactly once, the metadata gate's unknown-window split, the
// coverage and message rejections before commit, the publication failure
// retaining the running state with no follow-up terminal write, the
// non-summary checkpoint round-tripping and projecting as ordinary messages,
// and the raw plan State threading through Consume without ever becoming
// durable.

// fixtureStrategy is the replacement policy: Plan requests one model pass,
// each completed output folds back through Consume — the first answer plans
// the second pass, the second commits one non-summary checkpoint — while
// recording every frozen input, raw State, and completed output it observed.
type fixtureStrategy struct {
	seenInputs []PlanInput
	seenStates []json.RawMessage
	seenOutput int
	planErr    error
	consumeErr error
	onFailure  func()
}

func (s *fixtureStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.seenInputs = append(s.seenInputs, in)
	if s.planErr != nil {
		if s.onFailure != nil {
			s.onFailure()
		}
		return PlanOutput{}, s.planErr
	}
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("strategy pass " + strconv.Itoa(len(s.seenInputs)))}}},
		State:   json.RawMessage(`{"step":` + strconv.Itoa(len(s.seenInputs)) + `}`),
	}}, nil
}

func (s *fixtureStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	s.seenStates = append(s.seenStates, in.State)
	s.seenOutput++
	if s.consumeErr != nil {
		if s.onFailure != nil {
			s.onFailure()
		}
		return PlanOutput{}, s.consumeErr
	}
	if s.seenOutput == 1 {
		return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
			Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("strategy pass 2")}}},
			State:   json.RawMessage(`{"step":2}`),
		}}, nil
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items: authoredItems(
			model.Message{Role: model.RoleUser, Content: admissionContent("kept turn one")},
			model.Message{Role: model.RoleAssistant, Source: in.Output.Source, Content: admissionContent("kept turn two")},
		),
	}}, nil
}

// fixtureStrategyBinding returns the replacement binding over the given
// policy.
func fixtureStrategyBinding(strategy ContextStrategy) *StrategyBinding {
	return &StrategyBinding{ID: "test.strategy/v1", RequiresContextWindow: false, Strategy: strategy}
}

// fixtureStrategyCapture is the resolved capture input of the replacement
// binding.
func fixtureStrategyCapture() StrategyCapture {
	return StrategyCapture{ID: "test.strategy/v1", RequiresContextWindow: false}
}

// twoPassExecution returns the real automatic-maintenance execution: the
// maintenance transport answers the first request with the given usage and the
// second with no reported usage at all, while the conversation transport
// completes quietly after the maintenance stage.
func twoPassExecution(t *testing.T, first model.Usage, strategy *StrategyBinding) (Execution, *int) {
	calls := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		calls++
		if calls == 1 {
			return streamOf(turnDelta("stop", "first output"), usageDelta(first)), nil
		}
		return streamOf(turnDelta("stop", "second output")), nil
	}
	exec := purposeExecution(quietConversation(), compact)
	exec.Strategy = strategy
	return exec, &calls
}

// strategyHarness admits one running Operation ("op-1") under the strategy
// capture and opens the given real execution.
func strategyHarness(t *testing.T, exec Execution) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	return newAdmittedHarness(t, context.Background(), PreparedExecution{
		Capture: captureForHandoff(),
		Open: func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		},
	})
}

// idleFixture settles the fixture's running Operation success in a new
// settlement entry and clears the current Operation, so the Session is idle
// with its history, checkpoint and capture intact and manual Compact can
// admit its own dedicated Operation.
func idleFixture(t *testing.T, fixture *testGraph, settlementID string) *graphStorage {
	t.Helper()
	op := &fixture.ops[0]
	settlement := validSettlementEntry()
	settlement.EntryID = settlementID
	settlement.OperationID = op.Admission.OperationID
	sequence := int64(0)
	for i := range fixture.entries {
		if fixture.entries[i].env.Sequence > sequence {
			sequence = fixture.entries[i].env.Sequence
		}
	}
	fixture.entries = append(fixture.entries, testEntry{
		env:        Entry{SessionID: testSessionID, ID: settlementID, OperationID: op.Admission.OperationID, Kind: EntryOperationSettlement, Sequence: sequence + 1, CommittedAt: testTime},
		settlement: &settlement,
	})
	stamp := testTime
	op.State.Status, op.State.SettledAt = OperationSuccess, &stamp
	op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: settlementID}}
	fixture.session.State.CurrentOperationID = ""
	return fixture.storage(t)
}

// quietConversation answers the conversation model after the request source
// with an empty completed stream: the automatic maintenance under test is the
// only effect, and no conversation content or usage reaches the graph.
func quietConversation() func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "quiet")), nil
	}
}

// forbiddenMaintenanceModel is the tripwire for rows whose strategy never
// plans a maintenance model pass.
func forbiddenMaintenanceModel(t *testing.T) func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("the strategy planned a maintenance model pass")
		return nil, nil
	}
}

// proposalStrategy is the ordinary data strategy for checkpoint rows: Plan
// returns exactly the checkpoint derived from its frozen input and Consume
// never runs.
type proposalStrategy struct {
	checkpoint func(PlanInput) *CheckpointPlan
}

func (s proposalStrategy) Plan(in PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: s.checkpoint(in)}, nil
}

func (s proposalStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// runCheckpointProposal drives one real automatic maintenance round over the
// fixture's running Operation with a strategy proposing the given checkpoint,
// returning the validated graph and the combined run/validation error.
func runCheckpointProposal(t *testing.T, fixture *testGraph, proposal func(PlanInput) *CheckpointPlan) (*sessionGraph, error) {
	t.Helper()
	fixture.ops[0].Admission.Execution.Strategy = fixtureStrategyCapture()
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	exec := purposeExecution(quietConversation(), forbiddenMaintenanceModel(t))
	exec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: proposal})
	err := runAutomaticMaintenance(t, h, c, exec)
	graph, gerr := validateFixture(t, store, testSessionID)
	return graph, errors.Join(err, gerr)
}

// assertNoCheckpointSettledFailureGraph asserts the failed strategy attempt
// left no checkpoint entry and settled the named Operation as the failure
// terminal.
func assertNoCheckpointSettledFailureGraph(t *testing.T, graph *sessionGraph, operationID string) {
	t.Helper()
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "rejected checkpoint committed an entry")
	}
	state := operationStateOf(t, graph, operationID)
	testassert.Require(t, state.Status == OperationFailure, "operation state = %+v, want the strategy failure terminal", state)
}

func TestStrategyOutcomesThroughConversationEngine(t *testing.T) {
	for _, stage := range []string{"plan", "consume"} {
		for _, interrupted := range []bool{false, true} {
			for _, cancelDuring := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/interrupted=%t/canceled=%t", stage, interrupted, cancelDuring), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var cause error = context.Canceled
					if interrupted {
						cause = &EffectError{Err: context.Canceled, Interrupted: true}
					}
					strategy := &fixtureStrategy{}
					if stage == "plan" {
						strategy.planErr = cause
					} else {
						strategy.consumeErr = cause
					}
					if cancelDuring {
						strategy.onFailure = cancel
					}
					exec, calls := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(strategy))
					prepared := modelPrepared(nil)
					prepared.Capture = captureForHandoff()
					prepared.Open = func(context.Context, OperationAdmission) (Execution, error) { return exec, nil }
					h, store, c, sessionID := newAdmittedHarness(t, context.Background(), prepared)
					err := executeDirect(h, c, ctx, testOpID, prepared)
					want := OperationFailure
					if interrupted {
						want = OperationInterruption
					}
					if (!interrupted && err != cause) || (interrupted && err != nil) {
						t.Fatalf("execute=%v", err)
					}
					settlement := graphSettlementOf(t, store, sessionID)
					if settlement.Status != want {
						t.Fatalf("settlement=%+v want %s", settlement, want)
					}
					wantCalls := 0
					if stage == "consume" {
						wantCalls = 1
					}
					if *calls != wantCalls || len(settlement.Facts) != wantCalls {
						t.Fatalf("calls=%d facts=%+v", *calls, settlement.Facts)
					}
					if wantCalls == 1 && (settlement.Facts[0].Usage == nil || settlement.Facts[0].Usage.InputTokens != 3) {
						t.Fatalf("facts=%+v", settlement.Facts)
					}
				})
			}
		}
	}
}

// TestStrategyContractTwoFactCheckpoint proves the replacement policy's
// complete checkpoint through the real automatic maintenance round: two model
// plans run through their own maintenance invocations and producer identities,
// the ordered facts carry both requests with distinct identities and inputs,
// only the reporting producer carries usage, the checkpoint messages are the
// policy's non-summary messages, and the reported usage is charged exactly
// once on the checkpoint and both registers.
func TestStrategyContractTwoFactCheckpoint(t *testing.T) {
	strategy := &fixtureStrategy{}
	exec, calls := twoPassExecution(t, model.Usage{InputTokens: 10, OutputTokens: 5}, fixtureStrategyBinding(strategy))
	h, store, c, sessionID := strategyHarness(t, exec)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, *calls, 2)

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, len(comp.Messages) == 2, "checkpoint messages = %+v, want the two non-summary items", comp.Messages)
	testassert.Require(t, comp.StrategyID == "test.strategy/v1", "checkpoint strategy = %q, want the selected binding identity", comp.StrategyID)
	testassert.Require(t, len(comp.Facts) == 2, "checkpoint facts = %+v, want both producers in order", comp.Facts)
	testassert.Require(t, comp.Facts[0].RequestID != "" && comp.Facts[1].RequestID != "" && comp.Facts[0].RequestID != comp.Facts[1].RequestID, "fact producers = %q %q, want distinct request identities", comp.Facts[0].RequestID, comp.Facts[1].RequestID)
	testassert.Require(t, !bytes.Equal(mustJSON(t, comp.Facts[0].Input), mustJSON(t, comp.Facts[1].Input)), "fact inputs are not the two distinct model requests")
	testassert.Require(t, comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == (UsageCount{InputTokens: 10, OutputTokens: 5}), "first fact usage = %+v, want the reporting producer's counts", comp.Facts[0].Usage)
	testassert.Require(t, comp.Facts[1].Usage == nil, "second fact usage = %+v, want the missing report kept absent, never a zero", comp.Facts[1].Usage)
	// The reported usage is charged exactly once on the checkpoint: the
	// registers carry the single total keyed by the producing model.
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 10, OutputTokens: 5}}}}
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want %+v", graph.Session.State.Usage, want)
	testassert.Require(t, usageTotalsEqual(operationStateOf(t, graph, testOpID).Usage, want), "operation usage = %+v, want %+v", operationStateOf(t, graph, testOpID).Usage, want)

	// The non-summary checkpoint projects as ordinary messages with no
	// custom decoder.
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 4 && after[1].Role == model.RoleUser && after[1].TextContent() == "kept turn one" && after[2].Role == model.RoleAssistant && after[2].TextContent() == "kept turn two" && after[3].TextContent() == "quiet", "projection = %+v, want the system message, the two ordinary checkpoint messages and the quiet conversation turn", after)

	// The raw plan State threaded through Consume and stayed process-local:
	// the durable payload carries no state member at all.
	testassert.Require(t, len(strategy.seenStates) == 2 && string(strategy.seenStates[0]) == `{"step":1}` && string(strategy.seenStates[1]) == `{"step":2}`, "consumed states = %s, want each plan's own raw State threaded through", strategy.seenStates)
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			testassert.Require(t, !bytes.Contains(graph.Entries[i].Envelope.Payload, []byte(`"state"`)), "checkpoint payload persisted strategy state: %s", graph.Entries[i].Envelope.Payload)
		}
	}
}

// TestStrategyContractMetadataGate proves the declared context-window split:
// a binding that does not require a known window admits the unknown (zero)
// window, while the included policy's requirement refuses it — pre-admission,
// through the capture validation the preparation gate runs.
func TestStrategyContractMetadataGate(t *testing.T) {
	unknown := testCapture()
	unknown.ContextWindow = 0
	unknown.Strategy = fixtureStrategyCapture()
	err := validateExecutionCapture(unknown)
	testassert.Require(t, err == nil, "a strategy not requiring a window must accept the unknown window; error %v", err)

	included := testCapture()
	included.ContextWindow = 0
	included.Strategy = StrategyCapture{ID: IncludedStrategyID, RequiresContextWindow: true}
	err = validateExecutionCapture(included)
	testassert.Require(t, err != nil, "the included policy must refuse the unknown window pre-admission; error %v", err)

	// The closed relation is enforced without loading strategy code: a
	// required window with a zero capture is invalid on the codec side too.
	_, encErr := encodeExecutionCapture(included)
	testassert.Require(t, encErr != nil, "the capture codec accepted a required strategy with an unknown window")
}

// TestStrategyContractCoverageRejections proves the pre-commit gates through
// the real automatic maintenance round: a boundary outside the frozen
// snapshot's covered range, fabricated tool-call checkpoint messages, and a
// cut between a tool call and its result are all rejected before the
// checkpoint commit, the operation settles as the strategy failure, and no
// checkpoint entry appears.
func TestStrategyContractCoverageRejections(t *testing.T) {
	t.Run("boundary outside the covered range", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, validTestGraph(), func(PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: hexID(9), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
		})
		testassert.Require(t, err != nil, "an out-of-range boundary was accepted")
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})

	t.Run("fabricated tool-call checkpoint messages", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, validTestGraph(), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(model.Message{
				Role:      model.RoleAssistant,
				Source:    testCompactCapture().Model,
				ToolCalls: []model.ToolCall{{ID: "call-fabricated", Name: "echo", Arguments: json.RawMessage(`{}`)}},
			})}
		})
		testassert.Require(t, err != nil, "a fabricated tool-call checkpoint message was accepted")
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})

	t.Run("boundary between a tool call and its result", func(t *testing.T) {
		// The frozen snapshot covers an assistant publishing call-1 and its
		// committed result; the cut at the assistant splits the pair.
		fixture := validTestGraph()
		callAssistant := validAssistantEntry(testOpID)
		callAssistant.EntryID = hexID(5)
		callAssistant.RequestID = hexID(5)
		callAssistant.ToolCalls = []toolCallRecord{validToolCallRecord()}
		callAssistant.ToolCalls[0].ResultEntryID = hexID(6)
		callAssistant.Evidence = &RequestEvidence{BoundaryEntryID: hexID(2)}
		result := validToolResultEntry(testOpID)
		result.EntryID = hexID(6)
		result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
		fixture.entries = append(fixture.entries,
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime}, assistant: &callAssistant},
			testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
		)
		// The proposed boundary at the assistant cuts between call-1 and
		// its result even though the checkpoint messages are self-contained.
		graph, err := runCheckpointProposal(t, fixture, func(PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
		})
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "cuts between tool call"), "straddling cut error = %v, want the call/result rejection", err)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
}

// TestStrategyContractPublicationFailure proves the publication failure split
// through the real manual compact owner: the injected checkpoint transaction
// failure leaves the running state for recovery with no follow-up terminal
// write, the previous checkpoint stays current, and the projection is
// unchanged.
func TestStrategyContractPublicationFailure(t *testing.T) {
	fixture := validTestGraph()
	store := idleFixture(t, fixture, hexID(20))
	priorExec := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
	priorExec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
		return summaryCheckpoint(t, "prior summary", in.BoundaryEntryID)
	}})
	nextExec, _ := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
	priorPrepared := compactPrepared(captureForHandoff(), priorExec)
	nextPrepared := compactPrepared(captureForHandoff(), nextExec)
	queue := []PreparedExecution{priorPrepared, nextPrepared}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		next := queue[0]
		queue = queue[1:]
		return next, nil
	})
	c := leaseCoordinator(t, h, testSessionID)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))

	graph0, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, summaryText(t, compactedEntryOf(t, graph0)) == "prior summary", "previous checkpoint = %+v, want the committed prior summary", compactedEntryOf(t, graph0))
	priorID := compactedEntryOf(t, graph0).EntryID
	before, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	beforeEntries, _ := storedSessionState(store, testSessionID)

	injected := errors.New("injected checkpoint publication failure")
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryCompaction {
			return injected
		}
		return nil
	}
	err = runManualCompact(t, h, c, testSessionID, "compact-2")
	testassert.Require(t, err != nil && errors.Is(err, injected), "run = %v, want the injected publication cause preserved", err)
	testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

	store.entryHook = nil
	graph, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	afterEntries, _ := storedSessionState(store, testSessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries))
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil || graph.Entries[i].Envelope.OperationID != "compact-2", "publication failure fabricated a follow-up terminal write")
	}
	testassert.Require(t, summaryText(t, compactedEntryOf(t, graph)) == "prior summary", "previous checkpoint no longer current: %+v", compactedEntryOf(t, graph))
	testassert.Equal(t, compactedEntryOf(t, graph).EntryID, priorID)
	testassert.Require(t, operationStateOf(t, graph, "compact-2").Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, "compact-2"))
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(after, before), "projection changed through the publication failure:\nbefore %+v\nafter  %+v", before, after)
}

// TestStrategyContractCheckpointRoundTrip proves the non-summary checkpoint
// round-trips through the durable codec with its messages intact — repeated
// opaque call IDs across exchanges included — and no custom canonical
// decoder.
func TestStrategyContractCheckpointRoundTrip(t *testing.T) {
	entry := validCompactionEntry(testOpID)
	entry.StrategyID = "test.strategy/v1"
	entry.Messages = []model.Message{
		{Role: model.RoleUser, Content: admissionContent("kept turn one")},
		{Role: model.RoleAssistant, Source: testModelRef(), Content: admissionContent("kept turn two")},
		{Role: model.RoleAssistant, Source: testModelRef(), ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}}},
		{Role: model.RoleTool, ToolCallID: "call-1", Content: admissionContent("done")},
		{Role: model.RoleAssistant, Source: testModelRef(), ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":2}`)}}},
		{Role: model.RoleTool, ToolCallID: "call-1", Content: admissionContent("done again")},
	}
	raw, err := encodeContextCheckpointEntry(entry)
	testassert.NoError(t, err)
	got, err := decodeContextCheckpointEntry(Entry{SessionID: testSessionID, ID: testEntryID, OperationID: testOpID, Kind: EntryCompaction, Sequence: 1, CommittedAt: testTime, Payload: raw})
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(got.Messages, entry.Messages), "round-tripped messages = %+v, want %+v", got.Messages, entry.Messages)
}

// mustJSON renders one value's JSON form for value comparison.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	testassert.NoError(t, err)
	return raw
}

// toolPairFixture builds the fixture Session whose frozen projection carries
// one assistant publishing call-1 with opaque raw argument bytes and its
// committed tool result, after the plain input and assistant entries.
func toolPairFixture(arguments []byte) *testGraph {
	fixture := validTestGraph()
	callAssistant := validAssistantEntry(testOpID)
	callAssistant.EntryID = hexID(5)
	callAssistant.RequestID = hexID(5)
	callAssistant.ToolCalls = []toolCallRecord{validToolCallRecord()}
	callAssistant.ToolCalls[0].ResultEntryID = hexID(6)
	callAssistant.ToolCalls[0].ArgumentsBase64 = base64.StdEncoding.EncodeToString(arguments)
	callAssistant.Evidence = &RequestEvidence{BoundaryEntryID: hexID(2)}
	result := validToolResultEntry(testOpID)
	result.EntryID = hexID(6)
	result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
	fixture.entries = append(fixture.entries,
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime}, assistant: &callAssistant},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &result},
	)
	return fixture
}

// TestStrategyContractToolExchangeSources proves the checkpoint message
// domain over real tool exchanges: a retained valid call/result pair commits
// with its raw argument bytes preserved exactly, while an unpaired retained
// exchange and an authored — fabricated — tool exchange fail the real
// automatic maintenance round before commit.
func TestStrategyContractToolExchangeSources(t *testing.T) {
	t.Run("retained valid tool pairs commit with byte-preserving arguments", func(t *testing.T) {
		arguments := []byte{0x7b, 0xff, 0x00, 0x7d} // not valid JSON
		graph, err := runCheckpointProposal(t, toolPairFixture(arguments), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(2, 3)}
		})
		testassert.NoError(t, err)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 2 && len(comp.Messages[0].ToolCalls) == 1 && comp.Messages[1].ToolCallID == comp.Messages[0].ToolCalls[0].ID,
			"checkpoint messages = %+v, want the retained call/result pair", comp.Messages)
		testassert.Require(t, bytes.Equal(comp.Messages[0].ToolCalls[0].Arguments, arguments),
			"retained arguments = %v, want the exact raw bytes %v", comp.Messages[0].ToolCalls[0].Arguments, arguments)
	})

	t.Run("an unpaired retained exchange fails before commit", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, toolPairFixture([]byte(`{}`)), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(2)}
		})
		testassert.Require(t, err != nil, "an unpaired retained call was accepted")
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})

	t.Run("an authored tool exchange is fabricated and fails before commit", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, toolPairFixture([]byte(`{}`)), func(in PlanInput) *CheckpointPlan {
			fabricated := model.Message{
				Role:      model.RoleAssistant,
				Source:    testCompactCapture().Model,
				ToolCalls: []model.ToolCall{{ID: "call-fabricated", Name: "echo", Arguments: json.RawMessage(`{}`)}},
			}
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(fabricated, model.Message{Role: model.RoleTool, ToolCallID: "call-fabricated", Content: admissionContent("result")})}
		})
		testassert.Require(t, err != nil, "an authored fabricated tool exchange was accepted")
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
}

// recordSecondMaintenanceInvocation wraps the execution opener and records
// the second maintenance invocation's identity before its model effect
// publishes, so a fault can name that actual request instead of a write
// ordinal.
func recordSecondMaintenanceInvocation(exec *Execution) *string {
	id := new(string)
	opened := exec.OpenInvocation
	opens := 0
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose == InvocationPurposeMaintenance {
			opens++
			if opens == 2 {
				*id = request.ID
			}
		}
		return inv, nil
	}
	return id
}

// TestStrategyContractPublicationFailureNoFollowUp proves the publication
// failure split by the boundary where it occurred: a quiet failed second-model
// intent and a failed second-model result publication propagate the original
// cause with the durable running state retained and no guessed follow-up
// terminal write. The fault names the second maintenance invocation's own
// request identity, never a write ordinal.
func TestStrategyContractPublicationFailureNoFollowUp(t *testing.T) {
	for _, row := range []struct {
		name       string
		failResult bool
		message    string
	}{
		{"second-model intent publication failure", false, "injected intent publication failure"},
		{"second-model result publication failure", true, "injected result publication failure"},
	} {
		t.Run(row.name, func(t *testing.T) {
			exec, _ := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
			secondID := recordSecondMaintenanceInvocation(&exec)
			h, store, c, sessionID := strategyHarness(t, exec)
			injected := errors.New(row.message)
			failEffectRegister(store,
				func(ae *ActiveEffect) bool {
					return ae.Kind == EffectModel && ae.Purpose == InvocationPurposeMaintenance && ae.RequestID == *secondID
				},
				row.failResult, injected, nil)
			err := runAutomaticMaintenance(t, h, c, exec)
			testassert.Require(t, err != nil && errors.Is(err, injected), "error = %v, want the original publication cause preserved", err)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Settlement == nil, "publication failure fabricated a follow-up terminal write")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
		})
	}
}

// TestStrategyContractLaterMaintenanceIntentUnderCanceledExecution proves the
// attempted publication's owner boundary on a later maintenance model: the
// second model plan's intent transaction cancels the maintenance execution
// and returns a cancellation-shaped cause, the marked failure propagates with
// that original cause, the running state and the incurred facts are retained,
// and the later model is never dispatched. The fault names the second
// maintenance invocation's request identity.
func TestStrategyContractLaterMaintenanceIntentUnderCanceledExecution(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			exec, calls := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
			secondID := recordSecondMaintenanceInvocation(&exec)
			h, store, c, sessionID := strategyHarness(t, exec)
			execCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := fmt.Errorf("intent publication aborted: %w", cause)
			failEffectRegister(store,
				func(ae *ActiveEffect) bool {
					return ae.Kind == EffectModel && ae.Purpose == InvocationPurposeMaintenance && ae.RequestID == *secondID
				},
				false, injected, cancel)
			err := runAutomaticMaintenanceCtx(t, h, c, execCtx, exec)
			testassert.Require(t, errors.Is(err, cause), "error = %v, want the original %v publication cause preserved", err, cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Settlement == nil, "attempted maintenance intent publication fabricated a follow-up terminal write")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
			testassert.Equal(t, *calls, 1) // the later model is never dispatched
		})
	}
}

// TestStrategyContractInvocationLifecycle proves the maintenance invocation
// ownership across multiple model plans in one real automatic round: an
// intermediate maintenance invocation disposes once its validated successor
// model plan is about to open, before that successor opens, and the final one
// disposes only after the checkpoint consumption.
func TestStrategyContractInvocationLifecycle(t *testing.T) {
	events := []string{}
	record := func(event string) { events = append(events, event) }
	exec, _ := twoPassExecution(t, model.Usage{InputTokens: 1}, fixtureStrategyBinding(&fixtureStrategy{}))
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose != InvocationPurposeMaintenance {
			return inv, nil
		}
		record("open")
		inner := inv.Close
		inv.Close = func() error {
			record("close")
			if inner != nil {
				return inner()
			}
			return nil
		}
		return inv, nil
	}

	h, store, c, sessionID := strategyHarness(t, exec)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	// The intermediate invocation disposed before its successor opened; the
	// automatic final one disposed after its own checkpoint consumption.
	testassert.Require(t, reflect.DeepEqual(events, []string{"open", "close", "open", "close"}), "events = %v, want the final disposal after the checkpoint consumption", events)
	graph, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	testassert.Require(t, compactedEntryOf(t, graph) != nil, "the strategy published no checkpoint")
}

// malformedConsumeStrategy runs one valid model pass and then folds its
// completed output into a successor plan outside the closed union.
type malformedConsumeStrategy struct{}

func (malformedConsumeStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}},
	}}, nil
}

func (malformedConsumeStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel}, nil // no variants at all
}

// TestStrategyContractMalformedConsumeKeepsFinalInvocationThroughSettlement
// proves the disposal order after a real model pass: only a validated
// successor model plan makes the completed invocation intermediate, so a
// malformed Consume plan settles the terminal failure — with its reported
// facts and usage durable — while the invocation is still owned, and the
// retained closer disposes it only after that settlement.
func TestStrategyContractMalformedConsumeKeepsFinalInvocationThroughSettlement(t *testing.T) {
	exec := purposeExecution(quietConversation(), func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	})
	exec.Strategy = fixtureStrategyBinding(malformedConsumeStrategy{})
	closes := 0
	var closedStatus OperationState
	var h *Harness
	var sessionID string
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose != InvocationPurposeMaintenance {
			return inv, nil
		}
		inner := inv.Close
		inv.Close = func() error {
			closes++
			record, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, rerr)
			closedStatus = record.State.Status
			if inner != nil {
				return inner()
			}
			return nil
		}
		return inv, nil
	}

	var store *graphStorage
	var c *coordinator
	h, store, c, sessionID = strategyHarness(t, exec)
	err := runAutomaticMaintenance(t, h, c, exec)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "model plan requires exactly its model variant"), "error = %v, want the closed-union rejection of the successor plan", err)
	testassert.Equal(t, closes, 1)
	testassert.Require(t, closedStatus == OperationFailure, "operation status at the final disposal = %v, want the terminal failure already durable when the retained closer ran", closedStatus)

	graph, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationFailure, "settlement status = %v, want the malformed-plan failure", settlement.Status)
	testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
		"settlement facts = %+v, want the incurred maintenance fact with its reported usage durable at the disposal", settlement.Facts)
}

// TestStrategyContractNoneThenOrdinaryTerminal proves the additive
// accounting combination through one real conversation execution: the facts an
// earlier model->none left uncommitted settle exactly once beside the ordinary
// terminal usage on one settlement.
func TestStrategyContractNoneThenOrdinaryTerminal(t *testing.T) {
	ordinary := UsageCount{InputTokens: 2, OutputTokens: 1}
	exec := purposeExecution(func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
	}, func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	})
	exec.Strategy = fixtureStrategyBinding(noneAfterModelStrategy{})
	h, store, c, sessionID := strategyHarness(t, exec)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))

	graph, gerr := validateFixture(t, store, sessionID)
	testassert.Require(t, gerr == nil, "the additive combination corrupts: %v", gerr)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Usage != nil && *settlement.Usage == ordinary, "settlement usage = %+v, want the ordinary no-output counts", settlement.Usage)
	testassert.Require(t, len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 7, OutputTokens: 3}),
		"settlement facts = %+v, want the accumulated maintenance fact beside the ordinary usage", settlement.Facts)
	want := UsageTotals{ByModel: []ModelUsage{
		{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, OutputTokens: 3}},
		{Model: testModelRef(), Usage: ordinary},
	}}
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want the additive ordinary + maintenance totals", graph.Session.State.Usage)
}

// ownedValuesStrategy mutates every frozen input value it receives, rewrites
// the aliased raw State its returned plan still points at, and keeps one
// shared authored message its returned checkpoint items alias — exercising
// the owned handoff of settings, plan inputs and returned values.
type ownedValuesStrategy struct {
	sharedState      json.RawMessage
	sharedMessage    model.Message
	seenStates       []string
	seenMessages     []string
	seenSettings     []string
	seenMeasurements []string
	seenTools        []string
}

func (s *ownedValuesStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.seenSettings = append(s.seenSettings, string(in.Settings))
	s.seenMessages = append(s.seenMessages, in.Messages[0].TextContent())
	s.seenMeasurements = append(s.seenMeasurements, strconv.FormatInt(in.Measurement.InputTokens, 10))
	s.seenTools = append(s.seenTools, in.Tools[0].Name)
	for i := range in.Messages {
		for j := range in.Messages[i].Content {
			in.Messages[i].Content[j].Text = "mutated input"
		}
	}
	in.Settings = append(in.Settings[:0], `"rewritten"`...)
	if in.Measurement != nil {
		in.Measurement.InputTokens = 999
	}
	for i := range in.Tools {
		in.Tools[i].Name = "mutated"
	}
	s.sharedState = json.RawMessage(`{"step":1}`)
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("owned pass")}}},
		State:   s.sharedState,
	}}, nil
}

func (s *ownedValuesStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	// Rewrite the alias the returned plan still points at before observing
	// the delivered value: a shared State would surface the rewrite.
	s.sharedState[2] = 'X'
	s.seenStates = append(s.seenStates, string(in.State))
	s.seenSettings = append(s.seenSettings, string(in.Input.Settings))
	s.seenMessages = append(s.seenMessages, in.Input.Messages[0].TextContent())
	s.seenMeasurements = append(s.seenMeasurements, strconv.FormatInt(in.Input.Measurement.InputTokens, 10))
	s.seenTools = append(s.seenTools, in.Input.Tools[0].Name)
	s.sharedMessage = model.Message{Role: model.RoleUser, Content: admissionContent("authored context")}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           []CheckpointItem{{Context: &s.sharedMessage}},
	}}, nil
}

// TestStrategyContractOwnedCallbackValues proves the composition isolation
// through a real automatic round: the captured settings are delivered as owned
// strategy input, every frozen input reaches each callback as an owned deep
// copy, and returned plan values are owned before the Harness retains them —
// so a mutating strategy can never rewrite the Harness's frozen source copy
// used to resolve Retain, persisted facts, the admission capture or a later
// callback.
func TestStrategyContractOwnedCallbackValues(t *testing.T) {
	strategy := &ownedValuesStrategy{}
	capture := captureForHandoff()
	capture.Strategy.Settings = json.RawMessage(`{"setting":1}`)
	exec := purposeExecution(quietConversation(), func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "owned pass"), usageDelta(model.Usage{InputTokens: 1})), nil
	})
	exec.Strategy = fixtureStrategyBinding(strategy)
	fixture := validTestGraph()
	fixture.ops[0].Admission.Execution = capture
	fixture.entries[1].assistant.Usage = &UsageCount{InputTokens: 11}
	measured := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 11}}}}
	fixture.ops[0].State.Usage, fixture.session.State.Usage = measured, measured
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	sessionID := testSessionID
	frozen, _, _, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	pristine := frozen[0].TextContent()
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))

	// The accepted checkpoint messages were owned before retention:
	// rewriting the strategy's shared authored message afterwards must not
	// reach the committed entry.
	strategy.sharedMessage.Content[0].Text = "mutated after return"

	// Each callback observed the pristine frozen values and the delivered
	// captured settings, and no mutation of a delivered copy reached the
	// owner's own input for the next callback.
	testassert.Require(t, reflect.DeepEqual(strategy.seenMessages, []string{pristine, pristine}), "callback messages = %v, want pristine frozen input at every callback", strategy.seenMessages)
	testassert.Require(t, reflect.DeepEqual(strategy.seenSettings, []string{`{"setting":1}`, `{"setting":1}`}), "callback settings = %v, want the captured settings delivered owned", strategy.seenSettings)
	testassert.Require(t, reflect.DeepEqual(strategy.seenStates, []string{`{"step":1}`}), "consumed states = %v, want the plan's raw State owned before retention", strategy.seenStates)
	testassert.Require(t, reflect.DeepEqual(strategy.seenMeasurements, []string{"11", "11"}),
		"callback measurements = %v, want the owner's measurement unchanged across callbacks", strategy.seenMeasurements)
	testassert.Require(t, reflect.DeepEqual(strategy.seenTools, []string{"echo", "echo"}), "callback tools = %v, want the captured tools unchanged across callbacks", strategy.seenTools)
	testassert.Require(t, string(capture.Strategy.Settings) == `{"setting":1}`, "the capture settings were rewritten to %s", capture.Strategy.Settings)
	testassert.Require(t, frozen[0].TextContent() == pristine, "the frozen input copy was rewritten to %q", frozen[0].TextContent())

	graph, gerr := validateFixture(t, store, sessionID)
	testassert.Require(t, gerr == nil, "the owned commit corrupts: %v", gerr)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "authored context",
		"checkpoint messages = %+v, want the authored context at its returned value", comp.Messages)
	// The persisted fact keeps the actual tool-free input it consumed.
	testassert.Require(t, len(comp.Facts) == 1 && comp.Facts[0].Input[0].TextContent() == "owned pass", "persisted fact input = %+v, want the actual model request", comp.Facts[0].Input)
}

// TestStrategyContractRepeatedPartialCheckpoints proves repeated partial
// checkpoints through two real manual compact Operations: after each commit the
// projection names the new checkpoint and the actual kept tail boundary, and
// the committed graph validates with the complete checkpoint-and-tail pair.
func TestStrategyContractRepeatedPartialCheckpoints(t *testing.T) {
	fixture := partialCutFixture()
	store := idleFixture(t, fixture, hexID(20))
	firstExec := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
	firstExec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: hexID(5), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("first cut")})}
	}})
	secondExec := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
	secondExec.Strategy = fixtureStrategyBinding(proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(0)}
	}})
	firstPrepared := compactPrepared(captureForHandoff(), firstExec)
	secondPrepared := compactPrepared(captureForHandoff(), secondExec)
	queue := []PreparedExecution{firstPrepared, secondPrepared}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		next := queue[0]
		queue = queue[1:]
		return next, nil
	})
	c := leaseCoordinator(t, h, testSessionID)

	// First partial checkpoint: the cut at hexID(5) keeps the hexID(6) tail.
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	after1, checkpoint1, boundary1, err := h.projectCanonical(c, "compact-1")
	testassert.NoError(t, err)
	testassert.Require(t, checkpoint1 != "" && boundary1 == hexID(6), "rebuilt refs = (%q, %q), want the new checkpoint and the kept tail's %q", checkpoint1, boundary1, hexID(6))
	testassert.Require(t, len(after1) == 2 && after1[0].TextContent() == "first cut", "rebuilt view = %+v, want the checkpoint message and the kept tail", after1)

	// Second partial checkpoint over the rebuilt view: the cut at the frozen
	// boundary keeps no entry tail.
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-2"))
	after2, checkpoint2, boundary2, err := h.projectCanonical(c, "compact-2")
	testassert.NoError(t, err)
	testassert.Require(t, boundary2 == checkpoint2, "rebuilt refs = (%q, %q), want the checkpoint as its own tail boundary", checkpoint2, boundary2)
	testassert.Require(t, len(after2) == 1 && after2[0].TextContent() == "first cut", "rebuilt view = %+v, want the retained checkpoint message", after2)

	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.Require(t, gerr == nil, "repeated partial checkpoints corrupt: %v", gerr)
	var first, second *contextCheckpointEntry
	for i := range graph.Entries {
		if e := graph.Entries[i].Compaction; e != nil && e.EntryID != hexID(3) {
			if first == nil {
				first = e
			} else {
				second = e
			}
		}
	}
	testassert.Require(t, first != nil && second != nil && first.BoundaryEntryID == hexID(5) && second.BoundaryEntryID == hexID(6),
		"committed cuts = %+v, %+v, want the two partial boundaries", first, second)
}

// TestPublicationHandoffAcrossEngineContract proves the composition handoff
// through the fixed Engine contract: a cancellation-shaped publication-origin
// error returned by a supplied callback while the run context stays live
// keeps its original cause and classification at the owning settler, the
// actual running state stays unchanged for recovery, and no compensating
// settlement or waiting dispatch follows. Every fault names the intent or
// result publication of the invocation whose real identity it targets.
func TestPublicationHandoffAcrossEngineContract(t *testing.T) {
	rows := []struct {
		name       string
		vehicle    string
		failResult bool
		cause      error
	}{
		{"conversation intent under cancellation", "conversation", false, context.Canceled},
		{"conversation result under cancellation", "conversation", true, context.Canceled},
		{"conversation intent under deadline", "conversation", false, context.DeadlineExceeded},
		{"conversation result under deadline", "conversation", true, context.DeadlineExceeded},
		{"later maintenance intent under cancellation", "maintenance", false, context.Canceled},
		{"later maintenance result under cancellation", "maintenance", true, context.Canceled},
		{"later maintenance intent under deadline", "maintenance", false, context.DeadlineExceeded},
		{"later maintenance result under deadline", "maintenance", true, context.DeadlineExceeded},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			injected := fmt.Errorf("publication aborted: %w", row.cause)
			var err error
			var store *graphStorage
			var sessionID string
			var modelCalls *int
			if row.vehicle == "conversation" {
				calls := 0
				modelCalls = &calls
				exec := purposeExecution(func(context.Context, model.Request) (model.Stream, error) {
					calls++
					return streamOf(turnDelta("stop", "answer")), nil
				}, forbiddenMaintenanceModel(t))
				exec.Strategy = fixtureStrategyBinding(&noneStrategy{})
				h, s, _, sid := newAdmittedHarness(t, context.Background(), PreparedExecution{
					Capture: captureForHandoff(),
					Open: func(context.Context, OperationAdmission) (Execution, error) {
						return exec, nil
					},
				})
				store, sessionID = s, sid
				c := leaseCoordinator(t, h, sid)
				failEffectRegister(store,
					func(ae *ActiveEffect) bool {
						return ae.Kind == EffectModel && ae.Purpose == InvocationPurposeConversation
					},
					row.failResult, injected, nil)
				err = executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(captureForHandoff(), func(context.Context, OperationAdmission) (Execution, error) {
					return exec, nil
				}))
			} else {
				exec, calls := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
				modelCalls = calls
				secondID := recordSecondMaintenanceInvocation(&exec)
				h, s, c, sid := strategyHarness(t, exec)
				store, sessionID = s, sid
				failEffectRegister(store,
					func(ae *ActiveEffect) bool {
						return ae.Kind == EffectModel && ae.Purpose == InvocationPurposeMaintenance && ae.RequestID == *secondID
					},
					row.failResult, injected, nil)
				err = runAutomaticMaintenance(t, h, c, exec)
			}
			testassert.Require(t, err != nil && errors.Is(err, row.cause), "error = %v, want the original %v cause preserved", err, row.cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Settlement == nil, "publication failure fabricated a follow-up terminal write")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
			// The failed intent precedes its model; the failed result follows
			// it. The conversation transport never runs on the maintenance
			// vehicle.
			wantCalls := 1
			if row.vehicle == "conversation" && !row.failResult {
				wantCalls = 0
			}
			if row.vehicle == "maintenance" && row.failResult {
				wantCalls = 2
			}
			testassert.Equal(t, *modelCalls, wantCalls)
		})
	}
}

// TestPublicationHandoffRetainsCheckpointPublicationAcrossDoneContext proves
// the request-source handoff: a checkpoint publication failure returned by
// the conversation request source while the run context dies concurrently
// keeps its original cause and classification instead of being relabeled
// into a compensating interruption settlement, and the waiting dispatch
// never runs.
func TestPublicationHandoffRetainsCheckpointPublicationAcrossDoneContext(t *testing.T) {
	admitted := modelPrepared(nil)
	admitted.Capture.Strategy = fixtureStrategyCapture()
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), admitted)
	execCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	injected := fmt.Errorf("checkpoint publication aborted: %w", context.Canceled)
	failed := false
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryCompaction && !failed {
			failed = true
			cancel() // the run context dies concurrently with the failed checkpoint publication
			return injected
		}
		return nil
	}
	binding := fixtureStrategyBinding(&immediateCutStrategy{})
	admitted.Open = func(ctx context.Context, admission OperationAdmission) (Execution, error) {
		exec := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
			return invocationExecution(func(context.Context, model.Request) (model.Stream, error) {
				t.Fatal("the conversation model must never run")
				return nil, nil
			}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
		}}
		exec.Strategy = binding
		return exec, nil
	}
	err := executeDirect(h, c, execCtx, testOpID, admitted)
	testassert.Require(t, err != nil && errors.Is(err, context.Canceled), "error = %v, want the original context canceled cause preserved", err)
	testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

	store.entryHook = nil
	graph, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil, "the relabeled publication fabricated a compensating settlement")
	}
	testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
}

// immediateCutStrategy plans one immediate checkpoint at the frozen covered
// boundary with one authored context message.
type immediateCutStrategy struct{ planCall int }

func (s *immediateCutStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.planCall++
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}}, nil
}

func (s *immediateCutStrategy) Consume(_ ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// captureForHandoff returns the handoff fixture capture with the replacement
// strategy's captured inputs.
func captureForHandoff() ExecutionCapture {
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	return capture
}

// requestShapeStrategy plans one model pass carrying the given request
// messages and folds its completed output into the configured outcome.
type requestShapeStrategy struct {
	request []model.Message
	outcome string
}

func (s requestShapeStrategy) Plan(_ PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{Request: model.Request{Messages: s.request}}}, nil
}

func (s requestShapeStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	switch s.outcome {
	case "failure":
		return PlanOutput{}, errors.New("strategy consume failure")
	case "none-success":
		return PlanOutput{Kind: PlanNone}, nil
	default:
		return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
			BoundaryEntryID: in.Input.BoundaryEntryID,
			Items:           authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")}),
		}}, nil
	}
}

// TestMaintenanceRequestMessageDomain proves the tool-free maintenance
// request rule through real runs: zero advertised tools, never a restriction
// on its historical messages. An empty accepted request and one carrying an
// authentic paired historical tool exchange run through a real maintenance
// model and publish their facts — with the actual inputs, empty included — on
// the checkpoint, the failure terminal and the manual none-success terminal.
func TestMaintenanceRequestMessageDomain(t *testing.T) {
	exchange := []model.Message{
		{Role: model.RoleUser, Content: admissionContent("turn")},
		{Role: model.RoleAssistant, Source: testCompactCapture().Model, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}}},
		{Role: model.RoleTool, ToolCallID: "call-1", Content: admissionContent("done")},
	}
	shapes := []struct {
		name    string
		request []model.Message
	}{
		{"empty request", nil},
		{"historical tool exchange", exchange},
		{"opaque bytes and typed text", []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: string([]byte{'a', 0xff, 'b'}), Extra: model.Extra{"raw": json.RawMessage(` { "n" : 1e999, "s" : "<" } `)}}}}}},
	}
	outcomes := []string{"checkpoint", "failure", "none-success"}
	for _, shape := range shapes {
		for _, outcome := range outcomes {
			t.Run(shape.name+" on "+outcome, func(t *testing.T) {
				var sent model.Request
				captureModel := func(_ context.Context, req model.Request) (model.Stream, error) {
					sent = req
					return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 4, OutputTokens: 2})), nil
				}
				strategy := requestShapeStrategy{request: shape.request, outcome: outcome}
				var (
					graph     *sessionGraph
					runErr    error
					published []maintenanceFact
				)
				if outcome == "none-success" {
					exec := purposeExecution(forbiddenConversationModel(t), captureModel)
					exec.Strategy = fixtureStrategyBinding(strategy)
					store := emptyStore(t)
					prepared := compactPrepared(captureForHandoff(), exec)
					h := newCompactHarness(t, store, prepared)
					session, serr := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: testSelection("coder")})
					testassert.NoError(t, serr)
					c := leaseCoordinator(t, h, session.Identity.SessionID)
					runErr = runManualCompact(t, h, c, session.Identity.SessionID, "compact-1")
					graph, verr := validateFixture(t, store, session.Identity.SessionID)
					testassert.NoError(t, verr)
					published = settlementEntryOf(t, graph).Facts
				} else {
					exec := purposeExecution(quietConversation(), captureModel)
					exec.Strategy = fixtureStrategyBinding(strategy)
					h, store, c, sessionID := strategyHarness(t, exec)
					runErr = runAutomaticMaintenance(t, h, c, exec)
					var verr error
					graph, verr = validateFixture(t, store, sessionID)
					testassert.NoError(t, verr)
					if outcome == "checkpoint" {
						published = compactedEntryOf(t, graph).Facts
					} else {
						published = settlementEntryOf(t, graph).Facts
					}
				}
				if outcome == "failure" {
					testassert.Require(t, runErr != nil, "the strategy failure returned success")
				} else {
					testassert.NoError(t, runErr)
				}
				testassert.Require(t, len(published) == 1, "published facts = %+v, want the one effect fact, empty input included", published)
				testassert.Require(t, reflect.DeepEqual(sent.Messages, published[0].Input) || len(sent.Messages) == 0 && len(published[0].Input) == 0, "physical request and stored fact differ: %+v / %+v", sent.Messages, published[0].Input)
				testassert.Require(t, len(published[0].Input) == len(shape.request), "fact input = %+v, want the actual request messages", published[0].Input)
				for i := range shape.request {
					if shape.name == "opaque bytes and typed text" {
						testassert.Require(t, published[0].Input[i].TextContent() == "a\ufffdb" && string(published[0].Input[i].Content[0].Extra["raw"]) == ` { "n" : 1e999, "s" : "<" } `, "accepted input = %+v", published[0].Input[i])
						continue
					}
					testassert.Require(t, published[0].Input[i].TextContent() == shape.request[i].TextContent() && published[0].Input[i].Role == shape.request[i].Role,
						"fact input[%d] = %+v, want %+v", i, published[0].Input[i], shape.request[i])
				}
				if len(shape.request) == 3 {
					testassert.Require(t, len(published[0].Input[1].ToolCalls) == 1 && published[0].Input[2].ToolCallID == published[0].Input[1].ToolCalls[0].ID,
						"fact input = %+v, want the historical tool exchange preserved", published[0].Input)
				}
				testassert.Require(t, published[0].Usage != nil, "fact usage = %+v, want the reported counts", published[0].Usage)
			})
		}
	}
}

// TestMaintenanceMalformedRequestAfterOpeningCancellation proves a malformed
// maintenance request fails at the request boundary after the invocation
// opened, through the real automatic and manual owners alike: the model is
// never dispatched, the invocation disposes exactly once, and the Operation
// settles failure — or interruption when the opener cancels the execution.
func TestMaintenanceMalformedRequestAfterOpeningCancellation(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, cancelOnOpen := range []bool{false, true} {
			t.Run(fmt.Sprintf("manual=%v/cancel=%v", manual, cancelOnOpen), func(t *testing.T) {
				dispatches, closes := 0, 0
				execCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				exec := purposeExecution(forbiddenConversationModel(t), func(context.Context, model.Request) (model.Stream, error) {
					dispatches++
					return completedTurnStream(), nil
				})
				original := exec.OpenInvocation
				exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
					inv, err := original(ctx, request)
					if err != nil {
						return inv, err
					}
					if request.Purpose != InvocationPurposeMaintenance {
						return inv, nil
					}
					inner := inv.Close
					inv.Close = func() error {
						closes++
						if inner != nil {
							return inner()
						}
						return nil
					}
					if cancelOnOpen {
						cancel()
					}
					return inv, nil
				}
				exec.Strategy = fixtureStrategyBinding(requestShapeStrategy{request: []model.Message{{Role: "bad"}}})

				var (
					store   *graphStorage
					session string
					opID    string
					err     error
				)
				if manual {
					store = emptyStore(t)
					prepared := compactPrepared(captureForHandoff(), exec)
					h := newCompactHarness(t, store, prepared)
					created, serr := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: testSelection("coder")})
					testassert.NoError(t, serr)
					c := leaseCoordinator(t, h, created.Identity.SessionID)
					session, opID = created.Identity.SessionID, "compact-1"
					err = runManualCompactCtx(t, h, c, execCtx, session, opID)
				} else {
					var h *Harness
					var c *coordinator
					h, store, c, session = strategyHarness(t, exec)
					opID = testOpID
					err = runAutomaticMaintenanceCtx(t, h, c, execCtx, exec)
				}
				graph, verr := validateFixture(t, store, session)
				testassert.NoError(t, verr)
				testassert.Equal(t, dispatches, 0)
				testassert.Equal(t, closes, 1)
				want := OperationFailure
				if cancelOnOpen {
					want = OperationInterruption
				}
				state := operationStateOf(t, graph, opID)
				testassert.Require(t, state.Status == want, "status = %s, want %s", state.Status, want)
				if !cancelOnOpen {
					settlement := settlementEntryOf(t, graph)
					testassert.Require(t, strings.Contains(settlement.Detail, "invalid role") && strings.Contains(settlement.Detail, "request"),
						"settlement detail = %q, want the malformed-request boundary", settlement.Detail)
					testassert.Require(t, err != nil && strings.Contains(err.Error(), "invalid role"), "error = %v, want the malformed request cause", err)
				}
			})
		}
	}
}

// repeatedCallIDFixture builds the fixture Session whose frozen projection
// carries two authentic completed tool exchanges from different Operations
// sharing the same opaque call ID: the admitted input, assistant and result
// of the running Operation in front of a settled follow-up Operation's own
// input, assistant and result.
func repeatedCallIDFixture() *testGraph {
	fixture := validTestGraph()
	callOne := validAssistantEntry(testOpID)
	callOne.EntryID = hexID(5)
	callOne.RequestID = hexID(5)
	callOne.ToolCalls = []toolCallRecord{validToolCallRecord()}
	callOne.ToolCalls[0].ResultEntryID = hexID(6)
	callOne.Evidence = &RequestEvidence{BoundaryEntryID: hexID(2)}
	resultOne := validToolResultEntry(testOpID)
	resultOne.EntryID = hexID(6)
	resultOne.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}

	inputTwo := validInputEntry("op-2")
	inputTwo.EntryID = hexID(7)
	callTwo := validAssistantEntry("op-2")
	callTwo.EntryID = hexID(8)
	callTwo.RequestID = hexID(8)
	callTwo.ToolCalls = []toolCallRecord{validToolCallRecord()} // the same opaque call ID
	callTwo.ToolCalls[0].ResultEntryID = hexID(9)
	callTwo.Evidence = &RequestEvidence{BoundaryEntryID: hexID(7)}
	resultTwo := validToolResultEntry("op-2")
	resultTwo.EntryID = hexID(9)
	resultTwo.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(8)}

	stamped := testTime
	settlement := validSettlementEntry()
	settlement.EntryID = hexID(10)
	settlement.OperationID = "op-2"
	settlement.Status = OperationSuccess
	settlement.RequestID = hexID(8)
	opTwo := validOperationRecord()
	opTwo.Admission.OperationID = "op-2"
	opTwo.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(7)}
	opTwo.State.Status = OperationSuccess
	opTwo.State.SettledAt = &stamped
	opTwo.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(10)}}

	fixture.entries = append(fixture.entries,
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime}, assistant: &callOne},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &resultOne},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(7), OperationID: "op-2", Kind: EntryInput, Sequence: 5, CommittedAt: testTime}, input: &inputTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(8), OperationID: "op-2", Kind: EntryAssistant, Sequence: 6, CommittedAt: testTime}, assistant: &callTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(9), OperationID: "op-2", Kind: EntryToolResult, Sequence: 7, CommittedAt: testTime}, toolResult: &resultTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(10), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 8, CommittedAt: testTime}, settlement: &settlement},
	)
	fixture.ops = append(fixture.ops, opTwo)
	return fixture
}

// TestRepeatedOpaqueCallIDsAcrossOperations proves the checkpoint message
// pairing tracks outstanding calls instead of imposing session-wide unique
// call IDs through real automatic maintenance rounds: two authentic completed
// exchanges from different Operations sharing one opaque call ID retain,
// commit, round-trip the codec and project as ordinary messages, while a
// duplicate still-outstanding ID stays invalid.
func TestRepeatedOpaqueCallIDsAcrossOperations(t *testing.T) {
	graph, err := runCheckpointProposal(t, repeatedCallIDFixture(), func(in PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(2, 3, 5, 6)}
	})
	testassert.NoError(t, err)
	// Retain both authentic completed exchanges sharing the opaque call ID.
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, len(comp.Messages) == 4, "checkpoint messages = %+v, want both exchanges", comp.Messages)
	testassert.Require(t, len(comp.Messages[0].ToolCalls) == 1 && comp.Messages[0].ToolCalls[0].ID == "call-1" && comp.Messages[1].ToolCallID == "call-1" &&
		len(comp.Messages[2].ToolCalls) == 1 && comp.Messages[2].ToolCalls[0].ID == "call-1" && comp.Messages[3].ToolCallID == "call-1",
		"checkpoint messages = %+v, want two completed exchanges under the same opaque call ID", comp.Messages)

	// A duplicate still-outstanding ID stays invalid: retaining the first
	// exchange's call beside the second exchange keeps call-1 open twice, and
	// the pairing guard — over authentic retained tool content — rejects it.
	rejected, err := runCheckpointProposal(t, repeatedCallIDFixture(), func(in PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(2, 5, 6)}
	})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits its tool call's result"),
		"retained-occurrence rejection = %v, want the occurrence pairing rule over the frozen exchanges", err)
	assertNoCheckpointSettledFailureGraph(t, rejected, testOpID)

	// Identical result bytes cannot bridge two exchanges: retaining the first
	// exchange's assistant beside the second exchange's result pairs one
	// Operation's call with the other's result and is rejected.
	t.Run("retaining a call with another exchange's identical result is rejected", func(t *testing.T) {
		graph, err := runCheckpointProposal(t, repeatedCallIDFixture(), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: retainItems(2, 6)}
		})
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits"), "cross-exchange pairing rejection = %v, want the occurrence pairing rule", err)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
}

// multiCallPairFixture builds the fixture Session whose frozen projection
// carries one multi-call assistant (call-a then call-b) with its two results
// in source order, followed by a settled follow-up Operation's own completed
// exchange that reuses the opaque call-b ID with identical bytes.
func multiCallPairFixture() *testGraph {
	fixture := validTestGraph()
	callOne := validAssistantEntry(testOpID)
	callOne.EntryID = hexID(5)
	callOne.RequestID = hexID(5)
	callOne.Evidence = &RequestEvidence{BoundaryEntryID: hexID(2)}
	callA := validToolCallRecord()
	callA.ID = "call-a"
	callA.ResultEntryID = hexID(6)
	callB := validToolCallRecord()
	callB.ID = "call-b"
	callB.Ordinal = 1
	callB.ResultEntryID = hexID(7)
	callOne.ToolCalls = []toolCallRecord{callA, callB}
	resultA := validToolResultEntry(testOpID)
	resultA.EntryID = hexID(6)
	resultA.ToolCallID = "call-a"
	resultA.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}
	resultB := validToolResultEntry(testOpID)
	resultB.EntryID = hexID(7)
	resultB.ToolCallID = "call-b"
	resultB.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(5)}

	inputTwo := validInputEntry("op-2")
	inputTwo.EntryID = hexID(8)
	callTwo := validAssistantEntry("op-2")
	callTwo.EntryID = hexID(9)
	callTwo.RequestID = hexID(9)
	reused := validToolCallRecord() // the completed reuse of the opaque call-b ID
	reused.ID = "call-b"
	reused.ResultEntryID = hexID(10)
	callTwo.ToolCalls = []toolCallRecord{reused}
	callTwo.Evidence = &RequestEvidence{BoundaryEntryID: hexID(8)}
	resultTwo := validToolResultEntry("op-2")
	resultTwo.EntryID = hexID(10)
	resultTwo.ToolCallID = "call-b"
	resultTwo.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(9)}

	stamped := testTime
	settlement := validSettlementEntry()
	settlement.EntryID = hexID(11)
	settlement.OperationID = "op-2"
	settlement.Status = OperationSuccess
	settlement.RequestID = hexID(9)
	opTwo := validOperationRecord()
	opTwo.Admission.OperationID = "op-2"
	opTwo.Admission.AdmittedEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(8)}
	opTwo.State.Status = OperationSuccess
	opTwo.State.SettledAt = &stamped
	opTwo.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(11)}}

	fixture.entries = append(fixture.entries,
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryAssistant, Sequence: 3, CommittedAt: testTime}, assistant: &callOne},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(6), OperationID: testOpID, Kind: EntryToolResult, Sequence: 4, CommittedAt: testTime}, toolResult: &resultA},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(7), OperationID: testOpID, Kind: EntryToolResult, Sequence: 5, CommittedAt: testTime}, toolResult: &resultB},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(8), OperationID: "op-2", Kind: EntryInput, Sequence: 6, CommittedAt: testTime}, input: &inputTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(9), OperationID: "op-2", Kind: EntryAssistant, Sequence: 7, CommittedAt: testTime}, assistant: &callTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(10), OperationID: "op-2", Kind: EntryToolResult, Sequence: 8, CommittedAt: testTime}, toolResult: &resultTwo},
		testEntry{env: Entry{SessionID: testSessionID, ID: hexID(11), OperationID: "op-2", Kind: EntryOperationSettlement, Sequence: 9, CommittedAt: testTime}, settlement: &settlement},
	)
	fixture.ops = append(fixture.ops, opTwo)
	return fixture
}

// TestStrategyContractMultiCallOccurrencePairing proves the occurrence rule
// over a multi-call assistant through real automatic maintenance rounds: the
// retained assistant must carry every result of its own calls, a substituted
// identical result of another exchange cannot stand in for the missing one, and
// the complete pairs commit with their own pairing intact.
func TestStrategyContractMultiCallOccurrencePairing(t *testing.T) {
	commit := func(t *testing.T, items []CheckpointItem) (*sessionGraph, error) {
		t.Helper()
		return runCheckpointProposal(t, multiCallPairFixture(), func(in PlanInput) *CheckpointPlan {
			return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: items}
		})
	}
	t.Run("a retained multi-call assistant missing one of its results is rejected", func(t *testing.T) {
		graph, err := commit(t, retainItems(2, 3))
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits its tool call's result"), "missing-result rejection = %v", err)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
	t.Run("a substituted identical result of another exchange is rejected", func(t *testing.T) {
		graph, err := commit(t, retainItems(2, 3, 7))
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits"), "substituted-result rejection = %v", err)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})
	t.Run("the complete pairs commit with their own pairing", func(t *testing.T) {
		graph, err := commit(t, retainItems(2, 3, 4, 6, 7))
		testassert.NoError(t, err)
		testassert.Require(t, len(compactedEntryOf(t, graph).Messages) == 5, "checkpoint messages = %+v, want every retained complete pair", compactedEntryOf(t, graph).Messages)
	})
}

// TestAutomaticCheckpointRequestTransition proves the running-request state
// at a checkpoint's first transition through real automatic rounds: an
// automatic maintenance procedure publishes the conversation request it
// consumed — with its actual rebuilt checkpoint/tail references — in the
// checkpoint transaction when none is published yet; an already-published
// request keeps its identity and its committed hook results while only its
// references advance; a published request naming a different conversation
// request rejects the proposal instead of being replaced; and manual
// maintenance creates no conversation RunningRequest.
func TestAutomaticCheckpointRequestTransition(t *testing.T) {
	checkpointStrategy := proposalStrategy{checkpoint: func(in PlanInput) *CheckpointPlan {
		return &CheckpointPlan{BoundaryEntryID: in.BoundaryEntryID, Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
	}}
	t.Run("a pure hook-free automatic checkpoint publishes the conversation request", func(t *testing.T) {
		arrived, release, conversation := parkedConversation(t)
		exec := purposeExecution(conversation, forbiddenMaintenanceModel(t))
		exec.Strategy = fixtureStrategyBinding(checkpointStrategy)
		h, store, c, sessionID := strategyHarness(t, exec)
		done := make(chan error, 1)
		go func() { done <- runAutomaticMaintenance(t, h, c, exec) }()
		<-arrived
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		comp := compactedEntryOf(t, graph)
		rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, rerr)
		testassert.Require(t, rec.State.Request != nil, "the first checkpoint transition published no running request: %+v", rec.State)
		testassert.Require(t, rec.State.Request.RequestID != "", "running request = %+v, want the authoritative conversation request identity", rec.State.Request)
		testassert.Require(t, rec.State.Request.CheckpointEntryID == comp.EntryID && rec.State.Request.BoundaryEntryID == rebuiltBoundary(graph.Entries, comp.BoundaryEntryID, comp.EntryID),
			"running request refs = %+v, want the new checkpoint and the rebuilt tail boundary", rec.State.Request)
		testassert.Equal(t, len(rec.State.Request.HookResults), 0)
		close(release)
		testassert.NoError(t, <-done)
	})

	t.Run("committed hook results survive the advanced transition", func(t *testing.T) {
		hooks := []RequestHook{
			{ID: "req.effectful.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
				prompt := "first effect"
				return RequestConsequence{Prompt: &prompt}, nil
			}},
			{ID: "req.effectful.two", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
				prompt := "second effect"
				return RequestConsequence{Prompt: &prompt}, nil
			}},
		}
		arrived, release, conversation := parkedConversation(t)
		exec := requestHookExecution(purposeExecution(conversation, forbiddenMaintenanceModel(t)), hooks...)
		exec.Strategy = fixtureStrategyBinding(checkpointStrategy)
		h, store, c, sessionID := strategyHarness(t, exec)
		done := make(chan error, 1)
		go func() { done <- runAutomaticMaintenance(t, h, c, exec) }()
		<-arrived
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		comp := compactedEntryOf(t, graph)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Request != nil && rec.State.Request.RequestID != "" && len(rec.State.Request.HookResults) == 2,
			"running request state after the hook effects = %+v, want the published request with both committed results", rec.State.Request)
		testassert.Require(t, rec.State.Request.CheckpointEntryID == comp.EntryID && rec.State.Request.BoundaryEntryID == rebuiltBoundary(graph.Entries, comp.BoundaryEntryID, comp.EntryID),
			"advanced refs = %+v, want the new checkpoint and the rebuilt tail boundary", rec.State.Request)
		committed := hookGraphHookResults(t, store, sessionID)
		testassert.Require(t, len(committed) == 2 && committed[0].HookID == "req.effectful.one" && committed[1].HookID == "req.effectful.two",
			"hook results = %+v, want both effects in committed order", committed)
		for i, ref := range rec.State.Request.HookResults {
			testassert.Require(t, ref.SessionID == sessionID && ref.EntryID == committed[i].EntryID,
				"hook result reference %d = %+v, want the full SessionID and identity of committed effect %s", i, ref, committed[i].EntryID)
		}
		close(release)
		testassert.NoError(t, <-done)
	})

	t.Run("a published request of a foreign conversation request rejects the proposal", func(t *testing.T) {
		hooks := []RequestHook{
			{ID: "req.effectful.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
				prompt := "first effect"
				return RequestConsequence{Prompt: &prompt}, nil
			}},
		}
		first := requestHookExecution(purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t)), hooks...)
		first.Strategy = fixtureStrategyBinding(&noneStrategy{})
		h, store, c, sessionID := strategyHarness(t, first)
		injected := errors.New("injected model intent failure")
		failEffectRegister(store,
			func(ae *ActiveEffect) bool {
				return ae.Kind == EffectModel && ae.Purpose == InvocationPurposeConversation
			},
			false, injected, nil)
		err := runAutomaticMaintenance(t, h, c, first)
		testassert.Require(t, err != nil && errors.Is(err, injected), "first round = %v, want the injected publication failure", err)
		published, perr := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, perr)
		testassert.Require(t, published.State.Request != nil, "the failed first round published no running request: %+v", published.State)

		second := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
		second.Strategy = fixtureStrategyBinding(checkpointStrategy)
		// The second round runs on the same admitted Operation and coordinator.
		err = runAutomaticMaintenance(t, h, c, second)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "consumed request"), "foreign-request rejection = %v, want the association rejection of published request %q", err, published.State.Request.RequestID)
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		assertNoCheckpointSettledFailureGraph(t, graph, testOpID)
	})

	t.Run("manual maintenance creates no conversation request", func(t *testing.T) {
		store := idleFixture(t, validTestGraph(), hexID(20))
		exec := purposeExecution(forbiddenConversationModel(t), forbiddenMaintenanceModel(t))
		exec.Strategy = fixtureStrategyBinding(checkpointStrategy)
		prepared := compactPrepared(captureForHandoff(), exec)
		h := newCompactHarness(t, store, prepared)
		c := leaseCoordinator(t, h, testSessionID)
		testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
		_, gerr := validateFixture(t, store, testSessionID)
		testassert.NoError(t, gerr)
		rec, err := h.ReadOperation(context.Background(), testSessionID, "compact-1")
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Request == nil, "running request = %+v, want manual maintenance to create no conversation request", rec.State.Request)
	})
}

// requestHookExecution attaches the given request hooks to the conversation
// invocation of the real execution.
func requestHookExecution(exec Execution, hooks ...RequestHook) Execution {
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose == InvocationPurposeConversation {
			inv.RequestHooks = hooks
		}
		return inv, nil
	}
	return exec
}

// parkedConversation returns a conversation model that signals its arrival at
// the real model barrier and waits for release before completing the turn.
func parkedConversation(t *testing.T) (arrived chan struct{}, release chan struct{}, modelFn func(context.Context, model.Request) (model.Stream, error)) {
	t.Helper()
	arrived, release = make(chan struct{}), make(chan struct{})
	return arrived, release, func(context.Context, model.Request) (model.Stream, error) {
		close(arrived)
		<-release
		return streamOf(turnDelta("stop", "quiet")), nil
	}
}

// bindingMutatingStrategy rewrites the Strategy field of its own supplied
// binding during Plan — to a replacement implementation or nil — exercising
// that the maintenance procedure runs against the accepted binding value,
// never a live re-read of the caller's pointer between Plan and Consume.
type bindingMutatingStrategy struct {
	binding     *StrategyBinding
	replacement ContextStrategy
}

func (s *bindingMutatingStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.binding.Strategy = s.replacement
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("binding snapshot pass")}}},
	}}, nil
}

func (s *bindingMutatingStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("snapshot checkpoint")}),
	}}, nil
}

// TestStrategyContractAcceptedBindingValueSurvivesCallerMutation proves the
// accepted binding value through a real automatic round: a strategy that
// rewrites the caller's own supplied binding's implementation between Plan and
// Consume — with a replacement or nil — cannot re-dispatch Consume through the
// replacement or a nil implementation; Consume still runs the accepted
// implementation and its checkpoint commits under the original captured
// provenance.
func TestStrategyContractAcceptedBindingValueSurvivesCallerMutation(t *testing.T) {
	for _, row := range []struct {
		name        string
		replacement ContextStrategy
	}{
		{"replacement implementation", malformedStrategy{}},
		{"nil implementation", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			strategy := &bindingMutatingStrategy{replacement: row.replacement}
			binding := fixtureStrategyBinding(strategy)
			strategy.binding = binding
			exec := purposeExecution(quietConversation(), func(context.Context, model.Request) (model.Stream, error) {
				return streamOf(turnDelta("stop", "binding snapshot pass")), nil
			})
			exec.Strategy = binding
			h, store, c, sessionID := strategyHarness(t, exec)
			err := runAutomaticMaintenance(t, h, c, exec)
			testassert.Require(t, err == nil, "the procedure under the mid-procedure binding rewrite = %v, want the accepted implementation still owning Consume", err)
			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			comp := compactedEntryOf(t, graph)
			testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "snapshot checkpoint",
				"checkpoint messages = %+v, want the accepted implementation's checkpoint", comp.Messages)
			testassert.Require(t, comp.StrategyID == "test.strategy/v1", "checkpoint provenance = %q, want the accepted binding identity", comp.StrategyID)
		})
	}
}
