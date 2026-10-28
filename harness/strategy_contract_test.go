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
}

func (s *fixtureStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.seenInputs = append(s.seenInputs, in)
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("strategy pass " + strconv.Itoa(len(s.seenInputs)))}}},
		State:   json.RawMessage(`{"step":` + strconv.Itoa(len(s.seenInputs)) + `}`),
	}}, nil
}

func (s *fixtureStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	s.seenStates = append(s.seenStates, in.State)
	s.seenOutput++
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

// twoPassExecution returns the fixture execution: the maintenance transport
// answers the first request with the given usage and the second with no
// reported usage at all, and the conversation transport is the forbidden
// tripwire.
func twoPassExecution(t *testing.T, first model.Usage, strategy *StrategyBinding) (Execution, *int) {
	calls := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		calls++
		if calls == 1 {
			return streamOf(turnDelta("stop", "first output"), usageDelta(first)), nil
		}
		return streamOf(turnDelta("stop", "second output")), nil
	}
	exec := compactionExecution(t, compact, nil)
	exec.Strategy = strategy
	return exec, &calls
}

// TestStrategyContractTwoFactCheckpoint proves the replacement policy's
// complete checkpoint: two model plans run through their own maintenance
// invocations and producer identities, the ordered facts carry both requests
// with distinct identities and inputs, only the reporting producer carries
// usage, the checkpoint messages are the policy's non-summary messages, and
// the reported usage is charged exactly once on the checkpoint and both
// registers.
func TestStrategyContractTwoFactCheckpoint(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	strategy := &fixtureStrategy{}
	binding := fixtureStrategyBinding(strategy)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	exec, calls := twoPassExecution(t, model.Usage{InputTokens: 10, OutputTokens: 5}, binding)

	inputID := fixtureInputEntryID(c)
	contributions := RequestEvidence{BoundaryEntryID: inputID}
	planInput := PlanInput{
		Trigger:         triggerAutomatic,
		Messages:        projectedSnapshot(t, h, c),
		BoundaryEntryID: inputID,
		Instructions:    "summarize",
		Contributions:   &contributions,
		ContextWindow:   4096,
		OutputReserve:   1,
	}
	suspended := RunningRequest{RequestID: "request-conversation", BoundaryEntryID: inputID}
	checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput, &suspended)
	defer closeMaintenance()
	testassert.NoError(t, err)
	testassert.Equal(t, *calls, 2)
	testassert.Require(t, checkpoint != nil && len(checkpoint.Items) == 2, "checkpoint = %+v, want the two non-summary items", checkpoint)

	err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
	testassert.NoError(t, err)

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
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
	testassert.Require(t, len(after) == 3 && after[1].Role == model.RoleUser && after[1].TextContent() == "kept turn one" && after[2].Role == model.RoleAssistant && after[2].TextContent() == "kept turn two", "projection = %+v, want the system message and the two ordinary checkpoint messages", after)

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

// TestStrategyContractCoverageRejections proves the pre-commit gates: a
// boundary outside the frozen snapshot's covered range, fabricated tool-call
// checkpoint messages, and a cut between a tool call and its result are all
// rejected before the checkpoint commit, the operation settles as the
// strategy failure, and no checkpoint entry appears.
func TestStrategyContractCoverageRejections(t *testing.T) {
	t.Run("boundary outside the covered range", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		planInput := PlanInput{Trigger: triggerManual, Messages: projectedSnapshot(t, h, c), BoundaryEntryID: inputID}
		checkpoint := &CheckpointPlan{BoundaryEntryID: hexID(9), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
		testassert.Require(t, err != nil, "an out-of-range boundary was accepted")
		assertNoCheckpointSettledFailure(t, h, store, c, sessionID)
	})

	t.Run("fabricated tool-call checkpoint messages", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		inputID := fixtureInputEntryID(c)
		planInput := PlanInput{Trigger: triggerManual, Messages: projectedSnapshot(t, h, c), BoundaryEntryID: inputID}
		checkpoint := &CheckpointPlan{BoundaryEntryID: inputID, Items: authoredItems(model.Message{
			Role:      model.RoleAssistant,
			Source:    testCompactCapture().Model,
			ToolCalls: []model.ToolCall{{ID: "call-fabricated", Name: "echo", Arguments: json.RawMessage(`{}`)}},
		})}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", inputID), checkpoint, false)
		testassert.Require(t, err != nil, "a fabricated tool-call checkpoint message was accepted")
		assertNoCheckpointSettledFailure(t, h, store, c, sessionID)
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
		h := newTestHarness(t, fixture.storage(t), nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		planInput := PlanInput{Trigger: triggerManual, Messages: projectedSnapshot(t, h, c), BoundaryEntryID: hexID(6)}
		// The proposed boundary at the assistant cuts between call-1 and
		// its result even though the checkpoint messages are self-contained.
		checkpoint := &CheckpointPlan{BoundaryEntryID: hexID(5), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", hexID(6)), checkpoint, false)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "cuts between tool call"), "straddling cut error = %v, want the call/result rejection", err)
	})
}

// assertNoCheckpointSettledFailure asserts the failed strategy attempt left
// no checkpoint entry and settled the running Operation as the failure
// terminal.
func assertNoCheckpointSettledFailure(t *testing.T, h *Harness, store *graphStorage, c *coordinator, sessionID string) {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "rejected checkpoint committed an entry")
	}
	state := operationStateOf(t, graph, testOpID)
	testassert.Require(t, state.Status == OperationFailure, "operation state = %+v, want the strategy failure terminal", state)
}

// TestStrategyContractPublicationFailure proves the publication failure
// split: the injected checkpoint transaction failure leaves the running
// state for recovery with no follow-up terminal write, the previous
// checkpoint stays current, and the waiting conversation request is never
// sent uncompacted.
func TestStrategyContractPublicationFailure(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	inputID := fixtureInputEntryID(c)
	// The previous checkpoint first.
	priorFacts := newMaintenanceFacts("request-conversation", inputID)
	err := h.commitMaintenance(context.Background(), c, testOpID, capture, PlanInput{Trigger: triggerManual, Messages: projectedSnapshot(t, h, c), BoundaryEntryID: inputID}, priorFacts, summaryCheckpoint(t, "prior summary", inputID), false)
	testassert.NoError(t, err)
	graph0, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	priorID := compactedEntryOf(t, graph0).EntryID
	before, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	beforeEntries, _ := storedSessionState(store, sessionID)

	strategy := &fixtureStrategy{}
	exec, _ := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(strategy))
	injected := errors.New("injected checkpoint publication failure")
	store.txHook = func(step string) error {
		if step == "insert_entry" {
			return injected
		}
		return nil
	}
	priorView := RequestEvidence{CheckpointEntryID: priorID, BoundaryEntryID: priorID}
	planInput := PlanInput{Trigger: triggerAutomatic, Messages: projectedSnapshot(t, h, c), CheckpointEntryID: priorID, BoundaryEntryID: priorID, Contributions: &priorView}
	checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput, &RunningRequest{RequestID: "request-conversation", CheckpointEntryID: priorID, BoundaryEntryID: priorID})
	defer closeMaintenance()
	testassert.NoError(t, err)
	testassert.Require(t, checkpoint != nil, "the strategy planned no checkpoint")
	err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "injected checkpoint publication failure"), "commit error = %v, want the injected publication failure", err)

	store.txHook = nil
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	afterEntries, _ := storedSessionState(store, sessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries))
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil, "publication failure fabricated a follow-up terminal write")
	}
	// The previous checkpoint stays current and the waiting request is
	// never sent uncompacted.
	testassert.Require(t, summaryText(t, compactedEntryOf(t, graph)) == "prior summary", "previous checkpoint no longer current: %+v", compactedEntryOf(t, graph))
	state := operationStateOf(t, graph, testOpID)
	testassert.Require(t, state.Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", state)
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == len(before), "projection changed through the publication failure")
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
// exchange and an authored — fabricated — tool exchange fail before commit.
func TestStrategyContractToolExchangeSources(t *testing.T) {
	t.Run("retained valid tool pairs commit with byte-preserving arguments", func(t *testing.T) {
		arguments := []byte{0x7b, 0xff, 0x00, 0x7d} // not valid JSON
		fixture := toolPairFixture(arguments)
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		testassert.Require(t, len(frozen) == 4, "frozen view = %d messages, want the input, assistant, call and result", len(frozen))
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		// Retain the call's assistant message and its result message.
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: retainItems(2, 3)}, false)
		testassert.NoError(t, err)
		graph, gerr := validateFixture(t, store, testSessionID)
		testassert.Require(t, gerr == nil, "retained tool pair corrupts: %v", gerr)
		comp := compactedEntryOf(t, graph)
		testassert.Require(t, len(comp.Messages) == 2 && len(comp.Messages[0].ToolCalls) == 1 && comp.Messages[1].ToolCallID == comp.Messages[0].ToolCalls[0].ID,
			"checkpoint messages = %+v, want the retained call/result pair", comp.Messages)
		testassert.Require(t, bytes.Equal(comp.Messages[0].ToolCalls[0].Arguments, arguments),
			"retained arguments = %v, want the exact raw bytes %v", comp.Messages[0].ToolCalls[0].Arguments, arguments)
	})

	t.Run("an unpaired retained exchange fails before commit", func(t *testing.T) {
		fixture := toolPairFixture([]byte(`{}`))
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: retainItems(2)}, false)
		testassert.Require(t, err != nil, "an unpaired retained call was accepted")
		assertNoCheckpointSettledFailure(t, h, store, c, testSessionID)
	})

	t.Run("an authored tool exchange is fabricated and fails before commit", func(t *testing.T) {
		fixture := toolPairFixture([]byte(`{}`))
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := testCapture()
		capture.Strategy = fixtureStrategyCapture()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		fabricated := model.Message{
			Role:      model.RoleAssistant,
			Source:    testCompactCapture().Model,
			ToolCalls: []model.ToolCall{{ID: "call-fabricated", Name: "echo", Arguments: json.RawMessage(`{}`)}},
		}
		err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: authoredItems(fabricated, model.Message{Role: model.RoleTool, ToolCallID: "call-fabricated", Content: admissionContent("result")})}, false)
		testassert.Require(t, err != nil, "an authored fabricated tool exchange was accepted")
		assertNoCheckpointSettledFailure(t, h, store, c, testSessionID)
	})
}

// TestStrategyContractPublicationFailureNoFollowUp proves the publication
// failure split by the boundary where it occurred: a quiet failed second-model
// intent and a failed second-model result publication propagate the original
// cause with the durable running state retained and no guessed follow-up
// terminal write.
func TestStrategyContractPublicationFailureNoFollowUp(t *testing.T) {
	for _, row := range []struct {
		name    string
		failOn  int
		message string
	}{
		{"second-model intent publication failure", 4, "injected intent publication failure"},
		{"second-model result publication failure", 5, "injected result publication failure"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := testCapture()
			capture.Strategy = fixtureStrategyCapture()
			exec, _ := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
			injected := errors.New(row.message)
			replaced := 0
			store.txHook = func(step string) error {
				if step == "replace_register" {
					replaced++
					if replaced == row.failOn {
						return injected
					}
				}
				return nil
			}
			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
			_, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
				&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
			defer closeMaintenance()
			testassert.Require(t, err != nil && errors.Is(err, injected), "error = %v, want the original publication cause preserved", err)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

			store.txHook = nil
			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Settlement == nil, "publication failure fabricated a follow-up terminal write")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
			testassert.Require(t, facts.hasInput(), "the incurred facts must stay with the caller")
		})
	}
}

// TestStrategyContractLaterMaintenanceIntentUnderCanceledExecution proves the
// attempted publication's owner boundary on a later maintenance model: the
// second model plan's intent transaction cancels the maintenance execution
// and returns a cancellation-shaped cause, the marked failure propagates with
// that original cause, the running state and the incurred facts are retained,
// and the later model is never dispatched.
func TestStrategyContractLaterMaintenanceIntentUnderCanceledExecution(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := captureForHandoff()
			exec, calls := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
			execCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := fmt.Errorf("intent publication aborted: %w", cause)
			replaced := 0
			store.txHook = func(step string) error {
				if step == "replace_register" {
					replaced++
					if replaced == 4 { // the second maintenance model plan's intent transaction cancels the execution as it is invoked
						cancel()
						return injected
					}
				}
				return nil
			}
			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
			_, facts, closeMaintenance, err := h.runContextMaintenance(execCtx, c, testOpID, exec, capture, planInput,
				&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
			defer closeMaintenance()
			testassert.Require(t, errors.Is(err, cause), "error = %v, want the original %v publication cause preserved", err, cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)
			testassert.Require(t, facts.hasInput(), "the incurred facts must stay with the caller")

			store.txHook = nil
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

// TestStrategyContractInvocationLifecycle proves the invocation ownership
// across multiple model plans: an intermediate invocation disposes once its
// validated successor model plan is about to open, before that successor
// opens, and the final one disposes only after the checkpoint consumption.
func TestStrategyContractInvocationLifecycle(t *testing.T) {
	events := []string{}
	record := func(event string) { events = append(events, event) }
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "pass"), usageDelta(model.Usage{InputTokens: 1})), nil
	}
	exec := compactionExecution(t, compact, nil)
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
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
	exec.Strategy = fixtureStrategyBinding(&fixtureStrategy{})

	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
		&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
	testassert.NoError(t, err)
	testassert.Require(t, checkpoint != nil, "the strategy planned no checkpoint")
	// The intermediate invocation disposed before its successor opened; the
	// final one is still open until the caller consumes the checkpoint.
	testassert.Require(t, reflect.DeepEqual(events, []string{"open", "close", "open"}), "events = %v, want open-close-open with the final invocation held", events)

	testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false))
	testassert.NoError(t, closeMaintenance())
	testassert.Require(t, reflect.DeepEqual(events, []string{"open", "close", "open", "close"}), "events = %v, want the final disposal after the checkpoint consumption", events)
	_, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
}

// malformedConsumeStrategy runs one valid model pass and then folds its
// completed output into a successor plan outside the closed union.
type malformedConsumeStrategy struct{}

func (malformedConsumeStrategy) Plan(PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("one pass")}}},
	}}, nil
}

func (malformedConsumeStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel}, nil // no variants at all
}

// TestStrategyContractMalformedConsumeKeepsFinalInvocationThroughSettlement
// proves the disposal order after a model pass: only a validated successor
// model plan makes the completed invocation intermediate, so a malformed
// Consume plan settles the terminal failure — with its reported facts and
// usage durable — while the invocation is still owned, and the retained
// closer disposes it only after that settlement.
func TestStrategyContractMalformedConsumeKeepsFinalInvocationThroughSettlement(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	}, nil)
	exec.Strategy = fixtureStrategyBinding(malformedConsumeStrategy{})
	closes := 0
	var closedStatus OperationState
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
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

	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	_, _, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
		&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "model plan requires exactly its model variant"), "error = %v, want the closed-union rejection of the successor plan", err)
	testassert.Require(t, closes == 0, "the completed invocation disposed before its successor was validated and the failure settled")

	testassert.NoError(t, closeMaintenance())
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
// accounting combination: the facts an earlier model->none left uncommitted
// settle exactly once beside the ordinary terminal usage on one settlement.
func TestStrategyContractNoneThenOrdinaryTerminal(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 7, OutputTokens: 3})), nil
	}, nil)
	exec.Strategy = fixtureStrategyBinding(noneAfterModelStrategy{})

	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	_, _, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
		&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
	defer closeMaintenance()
	testassert.NoError(t, err)

	// The ordinary terminal: the conversation's own no-output usage beside
	// the accumulated maintenance facts, both accounted once.
	ordinary := UsageCount{InputTokens: 2, OutputTokens: 1}
	_, err = h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{
		terminal:   OperationSuccess,
		usage:      &ordinary,
		usageModel: testModelRef(),
	})
	testassert.NoError(t, err)

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
	sharedState   json.RawMessage
	sharedMessage model.Message
	seenStates    []string
	seenMessages  []string
	seenSettings  []string
}

func (s *ownedValuesStrategy) Plan(in PlanInput) (PlanOutput, error) {
	s.seenSettings = append(s.seenSettings, string(in.Settings))
	s.seenMessages = append(s.seenMessages, in.Messages[0].TextContent())
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
	s.sharedMessage = model.Message{Role: model.RoleUser, Content: admissionContent("authored context")}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           []CheckpointItem{{Context: &s.sharedMessage}},
	}}, nil
}

// TestStrategyContractOwnedCallbackValues proves the composition isolation:
// the captured settings are delivered as owned strategy input, every frozen
// input reaches each callback as an owned deep copy, and returned plan values
// are owned before the Harness retains them — so a mutating strategy can
// never rewrite the Harness's frozen source copy used to resolve Retain,
// persisted facts, the admission capture or a later callback.
func TestStrategyContractOwnedCallbackValues(t *testing.T) {
	strategy := &ownedValuesStrategy{}
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()
	capture.Strategy.Settings = json.RawMessage(`{"setting":1}`)
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "owned pass"), usageDelta(model.Usage{InputTokens: 1})), nil
	}, nil)
	exec.Strategy = fixtureStrategyBinding(strategy)

	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	pristine := frozen[0].TextContent()
	measurement := &UsageCount{InputTokens: 11}
	contributions := RequestEvidence{CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	planInput := PlanInput{
		Trigger:           triggerAutomatic,
		Messages:          frozen,
		CheckpointEntryID: frozenCheckpoint,
		BoundaryEntryID:   frozenBoundary,
		Tools:             cloneToolDefinitions(capture.Tools),
		Contributions:     &contributions,
		Measurement:       measurement,
		ContextWindow:     4096,
		OutputReserve:     1,
		Settings:          model.CloneRaw(capture.Strategy.Settings),
	}
	checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
		&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
	defer closeMaintenance()
	testassert.NoError(t, err)
	testassert.Require(t, checkpoint != nil, "the strategy planned no checkpoint")

	// The returned checkpoint items are owned before the Harness retains
	// them: rewriting the strategy's shared authored message after the
	// procedure returned must not reach the commit.
	strategy.sharedMessage.Content[0].Text = "mutated after return"

	err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false)
	testassert.NoError(t, err)

	// Each callback observed the pristine frozen values and the delivered
	// captured settings.
	testassert.Require(t, reflect.DeepEqual(strategy.seenMessages, []string{pristine, pristine}), "callback messages = %v, want pristine frozen input at every callback", strategy.seenMessages)
	testassert.Require(t, reflect.DeepEqual(strategy.seenSettings, []string{`{"setting":1}`, `{"setting":1}`}), "callback settings = %v, want the captured settings delivered owned", strategy.seenSettings)
	testassert.Require(t, reflect.DeepEqual(strategy.seenStates, []string{`{"step":1}`}), "consumed states = %v, want the plan's raw State owned before retention", strategy.seenStates)
	testassert.Require(t, measurement.InputTokens == 11, "the frozen measurement was rewritten to %+v", measurement)
	testassert.Require(t, string(capture.Strategy.Settings) == `{"setting":1}`, "the capture settings were rewritten to %s", capture.Strategy.Settings)
	testassert.Require(t, planInput.Messages[0].TextContent() == pristine, "the frozen input copy was rewritten to %q", planInput.Messages[0].TextContent())

	graph, gerr := validateFixture(t, store, sessionID)
	testassert.Require(t, gerr == nil, "the owned commit corrupts: %v", gerr)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, len(comp.Messages) == 1 && comp.Messages[0].TextContent() == "authored context",
		"checkpoint messages = %+v, want the authored context at its returned value", comp.Messages)
	// The persisted fact keeps the actual tool-free input it consumed.
	testassert.Require(t, len(comp.Facts) == 1 && comp.Facts[0].Input[0].TextContent() == "owned pass", "persisted fact input = %+v, want the actual model request", comp.Facts[0].Input)
}

// TestStrategyContractRepeatedPartialCheckpoints proves repeated partial
// checkpoints rebuild coherent references: after each commit the projection
// names the new checkpoint and the actual kept tail boundary, and the
// committed graph validates with the complete checkpoint-and-tail pair.
func TestStrategyContractRepeatedPartialCheckpoints(t *testing.T) {
	fixture := partialCutFixture()
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	capture := testCapture()
	capture.Strategy = fixtureStrategyCapture()

	// First partial checkpoint: the cut at hexID(5) keeps the hexID(6) tail.
	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	firstFacts := newMaintenanceFacts("request-conversation", frozenBoundary)
	firstFacts.maintenanceID = "maintenance-two"
	firstFacts.facts[0].RequestID = "maintenance-two"
	err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, firstFacts,
		&CheckpointPlan{BoundaryEntryID: hexID(5), Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("first cut")})}, false)
	testassert.NoError(t, err)

	after1, checkpoint1, boundary1, err := h.projectCanonical(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, boundary1 == hexID(6), "rebuilt tail boundary = %q, want the kept tail's %q", boundary1, hexID(6))
	testassert.Require(t, len(after1) == 2 && after1[0].TextContent() == "first cut", "rebuilt view = %+v, want the checkpoint message and the kept tail", after1)

	// Second partial checkpoint over the rebuilt view: the cut at the frozen
	// boundary keeps no entry tail.
	planInput2 := PlanInput{Trigger: triggerManual, Messages: after1, CheckpointEntryID: checkpoint1, BoundaryEntryID: boundary1}
	secondFacts := newMaintenanceFacts("request-conversation", boundary1)
	secondFacts.maintenanceID = "maintenance-three"
	secondFacts.facts[0].RequestID = "maintenance-three"
	err = h.commitMaintenance(context.Background(), c, testOpID, capture, planInput2, secondFacts,
		&CheckpointPlan{BoundaryEntryID: boundary1, Items: retainItems(0)}, false)
	testassert.NoError(t, err)

	after2, checkpoint2, boundary2, err := h.projectCanonical(c, testOpID)
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
// settlement or waiting dispatch follows.
func TestPublicationHandoffAcrossEngineContract(t *testing.T) {
	rows := []struct {
		name    string
		vehicle string
		failOn  int
		cause   error
	}{
		{"conversation intent under cancellation", "conversation", 1, context.Canceled},
		{"conversation result under cancellation", "conversation", 2, context.Canceled},
		{"conversation intent under deadline", "conversation", 1, context.DeadlineExceeded},
		{"conversation result under deadline", "conversation", 2, context.DeadlineExceeded},
		{"later maintenance intent under cancellation", "maintenance", 4, context.Canceled},
		{"later maintenance result under cancellation", "maintenance", 5, context.Canceled},
		{"later maintenance intent under deadline", "maintenance", 4, context.DeadlineExceeded},
		{"later maintenance result under deadline", "maintenance", 5, context.DeadlineExceeded},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			injected := fmt.Errorf("publication aborted: %w", row.cause)
			var err error
			var store *graphStorage
			var c *coordinator
			var sessionID string
			var modelCalls *int
			if row.vehicle == "conversation" {
				calls := 0
				modelCalls = &calls
				modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
					calls++
					return streamOf(turnDelta("stop", "answer")), nil
				}
				prepared := modelPrepared(modelFn)
				var h *Harness
				h, store, c, sessionID = newAdmittedHarness(t, context.Background(), prepared)
				replaced := 0
				store.txHook = func(step string) error {
					if step == "replace_register" {
						replaced++
						if replaced == row.failOn {
							return injected
						}
					}
					return nil
				}
				err = executeDirect(h, c, context.Background(), testOpID, prepared)
			} else {
				h, s, coord, sid := newEffectHarness(t, nil)
				store, c, sessionID = s, coord, sid
				exec, calls := twoPassExecution(t, model.Usage{InputTokens: 3}, fixtureStrategyBinding(&fixtureStrategy{}))
				modelCalls = calls
				replaced := 0
				store.txHook = func(step string) error {
					if step == "replace_register" {
						replaced++
						if replaced == row.failOn {
							return injected
						}
					}
					return nil
				}
				frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
				testassert.NoError(t, ferr)
				planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
				_, facts, closeMaintenance, merr := h.runContextMaintenance(context.Background(), c, testOpID, exec, captureForHandoff(), planInput,
					&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
				defer closeMaintenance()
				testassert.Require(t, facts.hasInput(), "the incurred facts must stay with the caller")
				err = merr
			}
			testassert.Require(t, err != nil && errors.Is(err, row.cause), "error = %v, want the original %v cause preserved", err, row.cause)
			testassert.Require(t, isPublicationFailure(err), "error = %v, want the publication-origin classification", err)

			store.txHook = nil
			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Settlement == nil, "publication failure fabricated a follow-up terminal write")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", operationStateOf(t, graph, testOpID))
			if row.vehicle == "maintenance" {
				// The later intent fails before its model runs; the later
				// result fails after it. The conversation transport never
				// runs on the maintenance vehicle.
				if row.failOn == 4 {
					testassert.Equal(t, *modelCalls, 1)
				} else {
					testassert.Equal(t, *modelCalls, 2)
				}
			} else if row.failOn == 1 {
				testassert.Equal(t, *modelCalls, 0) // the intent publication failed before any dispatch
			}
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
	store.txHook = func(step string) error {
		if step == "insert_entry" && !failed {
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

	store.txHook = nil
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

func (s *immediateCutStrategy) Consume(ConsumeInput) (PlanOutput, error) {
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

func (s requestShapeStrategy) Plan(PlanInput) (PlanOutput, error) {
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
// request rule: zero advertised tools, never a restriction on its historical
// messages. An empty accepted request and one carrying an authentic paired
// historical tool exchange run through a real maintenance model and publish
// their facts — with the actual inputs, empty included — on the checkpoint,
// the failure terminal and the manual none-success terminal.
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
	}
	outcomes := []string{"checkpoint", "failure", "none-success"}
	for _, shape := range shapes {
		for _, outcome := range outcomes {
			t.Run(shape.name+" on "+outcome, func(t *testing.T) {
				h, store, c, sessionID := newEffectHarness(t, nil)
				capture := captureForHandoff()
				exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
					return streamOf(turnDelta("stop", "one pass"), usageDelta(model.Usage{InputTokens: 4, OutputTokens: 2})), nil
				}, nil)
				exec.Strategy = fixtureStrategyBinding(requestShapeStrategy{request: shape.request, outcome: outcome})

				frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
				testassert.NoError(t, ferr)
				trigger, manual := triggerAutomatic, false
				if outcome == "none-success" {
					trigger, manual = triggerManual, true
				}
				planInput := PlanInput{Trigger: trigger, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
				checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
					&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
				defer closeMaintenance()
				if outcome == "failure" {
					testassert.Require(t, err != nil, "the strategy failure returned success")
				} else {
					testassert.NoError(t, err)
					testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, manual))
				}

				graph, gerr := validateFixture(t, store, sessionID)
				testassert.NoError(t, gerr)
				var published []maintenanceFact
				switch outcome {
				case "checkpoint":
					published = compactedEntryOf(t, graph).Facts
				default:
					published = settlementEntryOf(t, graph).Facts
				}
				testassert.Require(t, len(published) == 1, "published facts = %+v, want the one effect fact, empty input included", published)
				testassert.Require(t, len(published[0].Input) == len(shape.request), "fact input = %+v, want the actual request messages", published[0].Input)
				for i := range shape.request {
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
// call IDs: two authentic completed exchanges from different Operations
// sharing one opaque call ID retain, commit, round-trip the codec and
// project as ordinary messages, while a duplicate still-outstanding ID stays
// invalid.
func TestRepeatedOpaqueCallIDsAcrossOperations(t *testing.T) {
	fixture := repeatedCallIDFixture()
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	capture := captureForHandoff()
	frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
	testassert.NoError(t, ferr)
	testassert.Require(t, len(frozen) == 7, "frozen view = %d messages, want the two exchanges across the two Operations", len(frozen))
	planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
	// Retain both authentic completed exchanges sharing the opaque call ID.
	err := h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
		&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: retainItems(2, 3, 5, 6)}, false)
	testassert.NoError(t, err)

	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.Require(t, gerr == nil, "the repeated call ID corrupts: %v", gerr)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, len(comp.Messages) == 4, "checkpoint messages = %+v, want both exchanges", comp.Messages)
	testassert.Require(t, len(comp.Messages[0].ToolCalls) == 1 && comp.Messages[0].ToolCalls[0].ID == "call-1" && comp.Messages[1].ToolCallID == "call-1" &&
		len(comp.Messages[2].ToolCalls) == 1 && comp.Messages[2].ToolCalls[0].ID == "call-1" && comp.Messages[3].ToolCallID == "call-1",
		"checkpoint messages = %+v, want two completed exchanges under the same opaque call ID", comp.Messages)
	// The projection carries both exchanges as ordinary messages.
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	calls, results := 0, 0
	for _, m := range after {
		for _, call := range m.ToolCalls {
			if call.ID == "call-1" {
				calls++
			}
		}
		if m.ToolCallID == "call-1" {
			results++
		}
	}
	testassert.Require(t, calls == 2 && results == 2, "projection = %+v, want both same-ID exchanges retained", after)

	// A duplicate still-outstanding ID stays invalid: retaining the first
	// exchange's call beside the second exchange keeps call-1 open twice, and
	// the pairing guard — over authentic retained tool content — rejects it.
	second := repeatedCallIDFixture()
	store2 := second.storage(t)
	h2 := newTestHarness(t, store2, nil)
	c2 := leaseCoordinator(t, h2, testSessionID)
	capture2 := captureForHandoff()
	frozen2, frozenCheckpoint2, frozenBoundary2, ferr2 := h2.projectCanonical(c2, testOpID)
	testassert.NoError(t, ferr2)
	planInput2 := PlanInput{Trigger: triggerManual, Messages: frozen2, CheckpointEntryID: frozenCheckpoint2, BoundaryEntryID: frozenBoundary2}
	// A retained occurrence keeps its own pairing: retaining the first
	// exchange's call beside the second exchange keeps call-1 open twice AND
	// omits the first exchange's own result — the occurrence rule over the
	// frozen source rejects it before the materialized pairing guard.
	err = h2.commitMaintenance(context.Background(), c2, testOpID, capture2, planInput2, newMaintenanceFacts("request-conversation", frozenBoundary2),
		&CheckpointPlan{BoundaryEntryID: frozenBoundary2, Items: retainItems(2, 5, 6)}, false)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits its tool call's result"),
		"retained-occurrence rejection = %v, want the occurrence pairing rule over the frozen exchanges", err)
	assertNoCheckpointSettledFailure(t, h2, store2, c2, testSessionID)

	// Identical result bytes cannot bridge two exchanges: retaining the first
	// exchange's assistant beside the second exchange's result pairs one
	// Operation's call with the other's result and is rejected.
	t.Run("retaining a call with another exchange's identical result is rejected", func(t *testing.T) {
		fixture := repeatedCallIDFixture()
		store3 := fixture.storage(t)
		h3 := newTestHarness(t, store3, nil)
		c3 := leaseCoordinator(t, h3, testSessionID)
		capture3 := captureForHandoff()
		frozen3, frozenCheckpoint3, frozenBoundary3, ferr3 := h3.projectCanonical(c3, testOpID)
		testassert.NoError(t, ferr3)
		planInput3 := PlanInput{Trigger: triggerManual, Messages: frozen3, CheckpointEntryID: frozenCheckpoint3, BoundaryEntryID: frozenBoundary3}
		err := h3.commitMaintenance(context.Background(), c3, testOpID, capture3, planInput3, newMaintenanceFacts("request-conversation", frozenBoundary3),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary3, Items: retainItems(2, 6)}, false)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits"), "cross-exchange pairing rejection = %v, want the occurrence pairing rule", err)
		assertNoCheckpointSettledFailure(t, h3, store3, c3, testSessionID)
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
// over a multi-call assistant: the retained assistant must carry every result
// of its own calls, a substituted identical result of another exchange cannot
// stand in for the missing one, and the complete pairs commit with their own
// pairing intact.
func TestStrategyContractMultiCallOccurrencePairing(t *testing.T) {
	commit := func(t *testing.T, items []CheckpointItem) error {
		t.Helper()
		fixture := multiCallPairFixture()
		store := fixture.storage(t)
		h := newTestHarness(t, store, nil)
		c := leaseCoordinator(t, h, testSessionID)
		capture := captureForHandoff()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		testassert.Require(t, len(frozen) == 8, "frozen view = %d messages, want the multi-call exchange and the reused-ID exchange", len(frozen))
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		return h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: items}, false)
	}
	t.Run("a retained multi-call assistant missing one of its results is rejected", func(t *testing.T) {
		err := commit(t, retainItems(2, 3))
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits its tool call's result"), "missing-result rejection = %v", err)
	})
	t.Run("a substituted identical result of another exchange is rejected", func(t *testing.T) {
		err := commit(t, retainItems(2, 3, 7))
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "omits"), "substituted-result rejection = %v", err)
	})
	t.Run("the complete pairs commit with their own pairing", func(t *testing.T) {
		testassert.NoError(t, commit(t, retainItems(2, 3, 4, 6, 7)))
	})
}

// TestAutomaticCheckpointRequestTransition proves the running-request state
// at a checkpoint's first transition: an automatic maintenance procedure
// publishes the conversation request it consumed — with its actual rebuilt
// checkpoint/tail references — in the checkpoint transaction when none is
// published yet; an already-published request keeps its identity and its
// committed hook results while only its references advance; a published
// request naming a different conversation request rejects the proposal
// instead of being replaced; and manual maintenance creates no conversation
// RunningRequest.
func TestAutomaticCheckpointRequestTransition(t *testing.T) {
	automaticCheckpoint := func(t *testing.T, h *Harness, c *coordinator, facts *maintenanceFacts) error {
		t.Helper()
		capture := captureForHandoff()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		return h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts,
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}, false)
	}

	t.Run("a pure hook-free automatic checkpoint publishes the conversation request", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		inputID := fixtureInputEntryID(c)
		testassert.NoError(t, automaticCheckpoint(t, h, c, newMaintenanceFacts("request-conversation", inputID)))
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		comp := compactedEntryOf(t, graph)
		rec, rerr := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, rerr)
		testassert.Require(t, rec.State.Request != nil, "the first checkpoint transition published no running request: %+v", rec.State)
		testassert.Require(t, rec.State.Request.RequestID == "request-conversation", "running request = %+v, want the authoritative conversation request identity", rec.State.Request)
		testassert.Require(t, rec.State.Request.CheckpointEntryID == comp.EntryID && rec.State.Request.BoundaryEntryID == rebuiltBoundary(graph.Entries, comp.BoundaryEntryID, comp.EntryID),
			"running request refs = %+v, want the new checkpoint and the rebuilt tail boundary", rec.State.Request)
		testassert.Equal(t, len(rec.State.Request.HookResults), 0)
	})

	// hookRequestRound runs the real request source through two effectful
	// request hooks and stops before the conversation model intent: the hook
	// results publish the running request recording both committed results.
	hookRequestRound := func(t *testing.T) (*Harness, *graphStorage, *coordinator, string, string) {
		t.Helper()
		e1 := RequestHook{ID: "req.effectful.one", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			prompt := "first effect"
			return RequestConsequence{Prompt: &prompt}, nil
		}}
		e2 := RequestHook{ID: "req.effectful.two", ExtensionID: "req-ext", Mode: HookModeEffectful, Run: func(context.Context, RequestHookInput) (RequestConsequence, error) {
			prompt := "second effect"
			return RequestConsequence{Prompt: &prompt}, nil
		}}
		exec := requestHookFixtureExecution(forbiddenConversationModel(t), nil, e1, e2)
		h, store, c, sessionID := newHookHarness(t, exec)
		inv := openedConversation(t, exec)
		round := &conversationRound{}
		_, err := h.requestSource(c, testOpID, testCapture(), exec, inv, round)(context.Background())
		testassert.NoError(t, err)
		return h, store, c, sessionID, round.requestID
	}
	consumedHookIDs := func(facts *maintenanceFacts) *maintenanceFacts {
		facts.consumed.HookIDs = []HookIdentity{{ExtensionID: "req-ext", HookID: "req.effectful.one"}, {ExtensionID: "req-ext", HookID: "req.effectful.two"}}
		return facts
	}

	t.Run("committed hook results survive the advanced transition", func(t *testing.T) {
		h, store, c, sessionID, requestID := hookRequestRound(t)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Request != nil && rec.State.Request.RequestID == requestID && len(rec.State.Request.HookResults) == 2,
			"running request state after the hook effects = %+v, want the published request with both committed results", rec.State.Request)
		before := *rec.State.Request
		testassert.NoError(t, automaticCheckpoint(t, h, c, consumedHookIDs(newMaintenanceFacts(requestID, fixtureInputEntryID(c)))))
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		comp := compactedEntryOf(t, graph)
		rec2, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec2.State.Request != nil && rec2.State.Request.RequestID == requestID,
			"advanced running request = %+v, want the published identity preserved", rec2.State.Request)
		testassert.Require(t, reflect.DeepEqual(rec2.State.Request.HookResults, before.HookResults),
			"hook results = %+v, want the committed hook results kept intact", rec2.State.Request.HookResults)
		testassert.Require(t, rec2.State.Request.CheckpointEntryID == comp.EntryID && rec2.State.Request.BoundaryEntryID == rebuiltBoundary(graph.Entries, comp.BoundaryEntryID, comp.EntryID),
			"advanced refs = %+v, want the new checkpoint and the rebuilt tail boundary", rec2.State.Request)
	})

	t.Run("a published request of a foreign conversation request rejects the proposal", func(t *testing.T) {
		h, store, c, sessionID, requestID := hookRequestRound(t)
		err := automaticCheckpoint(t, h, c, newMaintenanceFacts("other-request", fixtureInputEntryID(c)))
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "consumed request"), "foreign-request rejection = %v, want the association rejection of published request %q", err, requestID)
		assertNoCheckpointSettledFailure(t, h, store, c, sessionID)
	})

	t.Run("manual maintenance creates no conversation request", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		capture := captureForHandoff()
		frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
		testassert.NoError(t, ferr)
		planInput := PlanInput{Trigger: triggerManual, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
		testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, newMaintenanceFacts("request-conversation", frozenBoundary),
			&CheckpointPlan{BoundaryEntryID: frozenBoundary, Items: authoredItems(model.Message{Role: model.RoleUser, Content: admissionContent("kept")})}, true))
		_, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Request == nil, "running request = %+v, want manual maintenance to create no conversation request", rec.State.Request)
	})
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
// accepted binding value: a strategy that rewrites the caller's own supplied
// binding's implementation between Plan and Consume — with a replacement or
// nil — cannot re-dispatch Consume through the replacement or a nil
// implementation; Consume still runs the accepted implementation and its
// checkpoint commits under the original captured provenance.
func TestStrategyContractAcceptedBindingValueSurvivesCallerMutation(t *testing.T) {
	for _, row := range []struct {
		name        string
		replacement ContextStrategy
	}{
		{"replacement implementation", malformedStrategy{}},
		{"nil implementation", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := testCapture()
			capture.Strategy = fixtureStrategyCapture()
			strategy := &bindingMutatingStrategy{replacement: row.replacement}
			binding := fixtureStrategyBinding(strategy)
			strategy.binding = binding
			exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
				return streamOf(turnDelta("stop", "binding snapshot pass")), nil
			}, nil)
			exec.Strategy = binding
			frozen, frozenCheckpoint, frozenBoundary, ferr := h.projectCanonical(c, testOpID)
			testassert.NoError(t, ferr)
			planInput := PlanInput{Trigger: triggerAutomatic, Messages: frozen, CheckpointEntryID: frozenCheckpoint, BoundaryEntryID: frozenBoundary}
			checkpoint, facts, closeMaintenance, err := h.runContextMaintenance(context.Background(), c, testOpID, exec, capture, planInput,
				&RunningRequest{RequestID: "request-conversation", BoundaryEntryID: fixtureInputEntryID(c)})
			defer closeMaintenance()
			testassert.Require(t, err == nil, "the procedure under the mid-procedure binding rewrite = %v, want the accepted implementation still owning Consume", err)
			testassert.Require(t, checkpoint != nil && len(checkpoint.Items) == 1 && checkpoint.Items[0].Context.TextContent() == "snapshot checkpoint",
				"checkpoint = %+v, want the accepted implementation's checkpoint", checkpoint)
			testassert.NoError(t, h.commitMaintenance(context.Background(), c, testOpID, capture, planInput, facts, checkpoint, false))
			graph, gerr := validateFixture(t, store, sessionID)
			testassert.NoError(t, gerr)
			comp := compactedEntryOf(t, graph)
			testassert.Require(t, comp.StrategyID == "test.strategy/v1", "checkpoint provenance = %q, want the accepted binding identity", comp.StrategyID)
		})
	}
}
