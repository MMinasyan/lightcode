// Contract coverage for the compaction orchestration: the frozen projected
// conversation is serialized into inert transcript lines, packed whole-message
// into budget-fitting pieces, and each piece runs one agent.Run invocation
// whose model effect commits the ordinary intent over the compact transport
// and settles without writing entries.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// forbiddenConversationModel fails the test if the conversation model
// transport ever runs during a compaction.
func forbiddenConversationModel(t *testing.T) func(context.Context, model.Request) (model.Stream, error) {
	return func(context.Context, model.Request) (model.Stream, error) {
		t.Errorf("the conversation model transport must never run during compaction")
		return nil, errors.New("conversation model must not run")
	}
}

// summaryStream assembles to one completed output carrying the given summary
// text and reported usage.
func summaryStream(text string, usage model.Usage) *scriptStream {
	return streamOf(turnDelta("stop", text), usageDelta(usage))
}

// compactSnapshotMessages returns the fixture two-message conversation
// snapshot.
func compactSnapshotMessages() []model.Message {
	return []model.Message{
		{Role: model.RoleUser, Content: admissionContent("hello there")},
		{Role: model.RoleAssistant, Source: testModelRef(), Content: admissionContent("hi again")},
	}
}

// registerRevision reads one register's current revision from the store.
func registerRevision(t *testing.T, store *graphStorage, sessionID string, key RegisterKey) int64 {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), key)
	if err != nil {
		t.Fatalf("ReadRegister: %v", err)
	}
	return reg.Revision
}

// settlementEntryOf returns the one operation settlement entry of the
// validated fixture graph.
func settlementEntryOf(t *testing.T, graph *sessionGraph) *operationSettlementEntry {
	t.Helper()
	var found *operationSettlementEntry
	for i := range graph.Entries {
		if graph.Entries[i].Settlement != nil {
			if found != nil {
				t.Fatalf("graph carries more than one settlement entry")
			}
			found = graph.Entries[i].Settlement
		}
	}
	if found == nil {
		t.Fatalf("graph carries no settlement entry")
	}
	return found
}

// entryIDs returns every entry identity of the validated fixture graph.
func entryIDs(t *testing.T, graph *sessionGraph) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for i := range graph.Entries {
		ids[graph.Entries[i].Envelope.ID] = true
	}
	return ids
}

// TestRunCompactionOneBudgetSnapshotRunsOnePiece proves the one-piece shape:
// a snapshot that fits one budget runs exactly one request whose user text is
// the serialized snapshot alone, one intent transaction commits before it,
// the ready settlement inserts no entry and leaves both registers with no
// usage, and the reserved identity stays unused.
func TestRunCompactionOneBudgetSnapshotRunsOnePiece(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
	sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	opRevision := registerRevision(t, store, sessionID, opKey)
	sessionRevision := registerRevision(t, store, sessionID, sessionKey)
	beforeEntries, _ := storedSessionState(store, sessionID)

	snapshot := compactSnapshotMessages()
	var requests []model.Request
	var reservedID string
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		reg, err := store.ReadRegister(context.Background(), opKey)
		if err != nil {
			return nil, err
		}
		op, err := decodeOperationRegister(reg)
		if err != nil {
			return nil, err
		}
		if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectModel {
			return nil, errors.New("no model effect intent is durable while the compact request runs")
		}
		reservedID = op.State.ActiveEffect.ResultEntryID
		return summaryStream("summary one", model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}

	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), snapshot)
	if err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if summary != "summary one" {
		t.Fatalf("summary %q, want the piece output text", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
		t.Fatalf("usage = %+v, want the one piece's reported counts", usage)
	}
	if len(requests) != 1 {
		t.Fatalf("compact transport calls = %d, want exactly one piece request", len(requests))
	}
	req := requests[0]
	if len(req.Messages) != 2 {
		t.Fatalf("request messages = %d, want exactly a system and a user message", len(req.Messages))
	}
	if len(req.Tools) != 0 {
		t.Fatalf("request tools = %d, want none advertised", len(req.Tools))
	}
	if req.Messages[0].Role != model.RoleSystem || req.Messages[0].Source != (model.ModelRef{}) {
		t.Fatalf("system message = %+v, want role system with zero source", req.Messages[0])
	}
	if got := req.Messages[0].TextContent(); got != testCapture().Compact.SystemPrompt {
		t.Fatalf("system message text %q, want the compact prompt", got)
	}
	if req.Messages[1].Role != model.RoleUser || req.Messages[1].Source != (model.ModelRef{}) {
		t.Fatalf("user message = %+v, want role user with zero source", req.Messages[1])
	}
	expectedLines, err := serializeCompactionMessages(snapshot)
	if err != nil {
		t.Fatalf("serializeCompactionMessages: %v", err)
	}
	if got := req.Messages[1].TextContent(); got != strings.Join(expectedLines, "\n") {
		t.Fatalf("user request text %q, want the serialized snapshot alone", got)
	}
	if strings.Contains(req.Messages[1].TextContent(), "Previous summary:") {
		t.Fatalf("the one-piece request carries a Previous summary block: %q", req.Messages[1].TextContent())
	}

	// One intent transaction and one result transaction: the operation
	// register advanced exactly twice, the session register once.
	if got := registerRevision(t, store, sessionID, opKey); got != opRevision+2 {
		t.Fatalf("operation register revision %d, want %d (one intent and one result transaction)", got, opRevision+2)
	}
	if got := registerRevision(t, store, sessionID, sessionKey); got != sessionRevision+1 {
		t.Fatalf("session register revision %d, want %d", got, sessionRevision+1)
	}
	afterEntries, _ := storedSessionState(store, sessionID)
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("entry count %d, want the unchanged %d", len(afterEntries), len(beforeEntries))
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-run graph: %v", err)
	}
	ids := entryIDs(t, graph)
	if reservedID == "" || ids[reservedID] {
		t.Fatalf("reserved identity %q must stay unused by any entry", reservedID)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil {
		t.Fatalf("operation state = %+v, want running with the effect cleared", rec.State)
	}
	if len(rec.State.Usage.ByModel) != 0 {
		t.Fatalf("operation usage = %+v, want none on the ready path", rec.State.Usage)
	}
	if len(graph.Session.State.Usage.ByModel) != 0 {
		t.Fatalf("session usage = %+v, want none on the ready path", graph.Session.State.Usage)
	}
}

// TestRunCompactionMultiPieceConsumesEveryMessageOnce proves the rolling
// shape: a message that exceeds a nonempty piece's remaining budget starts
// the next piece, every message is consumed exactly once in order, no message
// is split, and every continuation request carries the previous summary.
func TestRunCompactionMultiPieceConsumesEveryMessageOnce(t *testing.T) {
	h, _, c, _ := newEffectHarness(t, nil)
	capture := testCapture()

	// Fixture sized to the contract's exact case: the middle message fits a
	// fresh empty piece but exceeds the space left in the piece carrying the
	// first message, so it starts the next piece, and the third message
	// exceeds the middle piece's remaining budget and starts the third piece.
	// No message exceeds a whole empty piece's budget.
	budget := compactionPieceBudget(capture.Compact, "")
	unit := "summarize the whole conversation turn carefully "
	alpha := strings.Repeat(unit, 12)
	alphaLines, err := serializeCompactionMessages([]model.Message{{Role: model.RoleUser, Content: admissionContent(alpha)}})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	costAlpha := estimateTokens(alphaLines[0], nil)
	big := ""
	costBig := 0
	for n := 1; ; n++ {
		candidate := strings.Repeat(unit, n)
		sized, err := serializeCompactionMessages([]model.Message{{Role: model.RoleUser, Content: admissionContent(candidate)}})
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		cost := estimateTokens(sized[0], nil)
		if cost > budget {
			t.Fatalf("no message size lands in the %d-token window between the first piece's remaining budget and the whole piece budget", costAlpha)
		}
		if cost > budget-costAlpha {
			big, costBig = candidate, cost
			break
		}
	}
	gamma := strings.Repeat(unit, 14)
	gammaLines, err := serializeCompactionMessages([]model.Message{{Role: model.RoleUser, Content: admissionContent(gamma)}})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	costGamma := estimateTokens(gammaLines[0], nil)
	if costAlpha+costBig <= budget {
		t.Fatalf("fixture precondition: the middle message would fit the first piece's remaining budget")
	}
	if costBig > budget {
		t.Fatalf("fixture precondition: the middle message exceeds a whole empty piece's budget")
	}
	if budget2 := compactionPieceBudget(capture.Compact, "S1"); costBig+costGamma <= budget2 {
		t.Fatalf("fixture precondition: the third message would fit the middle piece's remaining budget")
	}
	snapshot := []model.Message{
		{Role: model.RoleUser, Content: admissionContent(alpha)},
		{Role: model.RoleUser, Content: admissionContent(big)},
		{Role: model.RoleUser, Content: admissionContent(gamma)},
	}
	lines, err := serializeCompactionMessages(snapshot)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	var requests []model.Request
	usages := []model.Usage{{InputTokens: 1, OutputTokens: 2}, {InputTokens: 3, CachedInputTokens: 1, OutputTokens: 4}, {InputTokens: 5, CachedInputTokens: 2, OutputTokens: 6}}
	call := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		call++
		if call <= len(usages) {
			return summaryStream(fmt.Sprintf("S%d", call), usages[call-1]), nil
		}
		return summaryStream("S9", model.Usage{}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}

	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, snapshot)
	if err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if summary != "S3" {
		t.Fatalf("summary %q, want the final piece output", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 9, CachedInputTokens: 3, OutputTokens: 12}) {
		t.Fatalf("usage = %+v, want the field-wise sum across the three pieces", usage)
	}
	if len(requests) != 3 {
		t.Fatalf("compact transport calls = %d, want three pieces", len(requests))
	}
	wantUserText := []string{
		lines[0],
		"Previous summary:\nS1\n\nContinuation:\n" + lines[1],
		"Previous summary:\nS2\n\nContinuation:\n" + lines[2],
	}
	for i, req := range requests {
		if len(req.Messages) != 2 {
			t.Fatalf("request %d messages = %d, want exactly two", i, len(req.Messages))
		}
		if got := req.Messages[1].TextContent(); got != wantUserText[i] {
			t.Fatalf("request %d user text %q, want %q", i, got, wantUserText[i])
		}
	}
}

// TestCompactModelEffectSettlements proves the effect contract: attempts run
// through the compact transport under the retry policy, a piece failure
// settles the terminal with the accumulated usage keyed by the compact model
// and carries the errored assembly on the failure row, an interrupted effect
// settles through the existing terminal path with no output before any
// assembly, and a textless completed output settles ready with its output —
// the orchestration validates the summary text after the settlement. The
// ready settlement's durable shape is pinned by the one-budget orchestration
// test.
func TestCompactModelEffectSettlements(t *testing.T) {
	compactRef := testCompactCapture().Model

	t.Run("attempts run through the compact transport", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		spy := &retrySpy{limit: 1}
		calls := 0
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			calls++
			if calls == 1 {
				return nil, &net.OpError{Op: "dial", Err: errors.New("boom")}
			}
			return summaryStream("piece summary", model.Usage{}), nil
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: spy.retry, NormalizeTool: objectNormalize}
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &usageAccumulator{})
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("piece")}}})
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		set, err := me(context.Background(), req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		})
		if err != nil {
			t.Fatalf("compact model effect: %v", err)
		}
		if set.Disposition != agent.DispoReady {
			t.Fatalf("settlement disposition %q, want ready", set.Disposition)
		}
		if calls != 2 {
			t.Fatalf("compact transport calls = %d, want the retried attempt pair", calls)
		}
		if len(spy.attempts) != 1 || spy.attempts[0] != 1 || len(spy.seen) != 1 {
			t.Fatalf("retry consultations = %v seen %v, want the one failed attempt classified by the policy", spy.attempts, spy.seen)
		}
		if _, ok := spy.seen[0].(*net.OpError); !ok {
			t.Fatalf("classified failure %v, want the first attempt's transport error", spy.seen[0])
		}
	})

	t.Run("piece failure settles the accumulated usage", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			return failStream(errors.New("provider failure")), nil
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: (&retrySpy{limit: 0}).retry, NormalizeTool: objectNormalize}
		accum := &usageAccumulator{}
		if err := accum.add(UsageCount{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5}); err != nil {
			t.Fatalf("seed accumulator: %v", err)
		}
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), accum)
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("piece")}}})
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		set, err := me(context.Background(), req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		})
		if err != nil {
			t.Fatalf("compact model effect: %v", err)
		}
		if set.Disposition != agent.DispoFailure || set.Detail == "" || !strings.Contains(set.Detail, "provider failure") {
			t.Fatalf("settlement = %+v, want failure carrying the piece failure detail", set)
		}
		if set.Output == nil || set.Output.Status != model.OutputErrored {
			t.Fatalf("settlement output = %+v, want the errored assembly carried on the failure row", set.Output)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-settlement graph: %v", err)
		}
		settlement := settlementEntryOf(t, graph)
		if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "provider failure") {
			t.Fatalf("settlement entry = %+v, want the failure terminal", settlement)
		}
		if settlement.Model == nil || *settlement.Model != compactRef {
			t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
		}
		if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5}) {
			t.Fatalf("settlement usage = %+v, want the accumulated total", settlement.Usage)
		}
		if len(graph.Session.State.Usage.ByModel) != 1 || graph.Session.State.Usage.ByModel[0].Model != compactRef {
			t.Fatalf("session usage = %+v, want the accumulated total keyed by the compact model", graph.Session.State.Usage)
		}
	})

	t.Run("interruption settles through the terminal path", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			t.Errorf("no attempt may start under a canceled execution context")
			return nil, errors.New("unreachable")
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &usageAccumulator{})
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("piece")}}})
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		set, err := me(ctx, req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		})
		if err != nil {
			t.Fatalf("compact model effect: %v", err)
		}
		if set.Disposition != agent.DispoInterruption || set.Detail != executionInterruptedDetail {
			t.Fatalf("settlement = %+v, want the fixed interruption settlement", set)
		}
		if set.Output != nil {
			t.Fatalf("settlement output = %+v, want none on the pre-assembly cancellation", set.Output)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-settlement graph: %v", err)
		}
		settlement := settlementEntryOf(t, graph)
		if settlement.Status != OperationInterruption || settlement.Detail != executionInterruptedDetail {
			t.Fatalf("settlement entry = %+v, want the interruption terminal", settlement)
		}
		var signals int
		for i := range graph.Entries {
			if graph.Entries[i].Signal != nil {
				signals++
			}
		}
		if signals != 1 {
			t.Fatalf("interruption signal entries = %d, want exactly one", signals)
		}
	})

	t.Run("refusal-only completed output settles ready with the carried output", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		spy := &retrySpy{limit: 3}
		refusal := turnDelta("stop", "")
		refusal.RefusalFragment = "no"
		calls := 0
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			calls++
			return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: spy.retry, NormalizeTool: objectNormalize}
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &usageAccumulator{})
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("piece")}}})
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		set, err := me(context.Background(), req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		})
		if err != nil {
			t.Fatalf("compact model effect: %v", err)
		}
		if set.Disposition != agent.DispoReady || set.Output == nil || set.Output.Status != model.OutputCompleted {
			t.Fatalf("settlement = %+v, want the ready settlement carrying the completed output", set)
		}
		if calls != 1 {
			t.Fatalf("compact transport calls = %d, want exactly one attempt and no retry", calls)
		}
	})
}

// compactTwoPieceSnapshot returns a two-message snapshot whose second message
// fits a fresh empty piece but exceeds the first piece's remaining budget,
// so the orchestration runs two pieces.
func compactTwoPieceSnapshot(t *testing.T) []model.Message {
	t.Helper()
	capture := testCapture()
	budget := compactionPieceBudget(capture.Compact, "")
	unit := "summarize the whole conversation turn carefully "
	first := strings.Repeat(unit, 12)
	firstLines, err := serializeCompactionMessages([]model.Message{{Role: model.RoleUser, Content: admissionContent(first)}})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	costFirst := estimateTokens(firstLines[0], nil)
	second := ""
	for n := 1; ; n++ {
		candidate := strings.Repeat(unit, n)
		sized, err := serializeCompactionMessages([]model.Message{{Role: model.RoleUser, Content: admissionContent(candidate)}})
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		cost := estimateTokens(sized[0], nil)
		if cost > budget {
			t.Fatalf("no message size lands in the %d-token window between the first piece's remaining budget and the whole piece budget", costFirst)
		}
		if cost > budget-costFirst {
			second = candidate
			break
		}
	}
	return []model.Message{
		{Role: model.RoleUser, Content: admissionContent(first)},
		{Role: model.RoleUser, Content: admissionContent(second)},
	}
}

// TestRunCompactionCanceledBeforeRunSettlesInterruption proves the pre-run
// window: an execution context already canceled before the first piece makes
// the run return interruption with no effect ever begun, and the
// orchestration settles the quiet running Operation as interruption with no
// usage and no compaction entry.
func TestRunCompactionCanceledBeforeRunSettlesInterruption(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no piece may run under a canceled execution context")
		return nil, errors.New("unreachable")
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, usage, err := h.runCompaction(ctx, c, testOpID, exec, testCapture(), compactSnapshotMessages())
	if err == nil || err.Error() != executionInterruptedDetail {
		t.Fatalf("error %v, want the run's interruption detail", err)
	}
	if summary != "" || usage != nil {
		t.Fatalf("summary %q usage %+v, want no summary and no usage", summary, usage)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-settlement graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationInterruption || settlement.Detail != executionInterruptedDetail {
		t.Fatalf("settlement entry = %+v, want the run's interruption terminal", settlement)
	}
	if settlement.Model != nil || settlement.Usage != nil {
		t.Fatalf("settlement entry = %+v, want no usage on the never-run compaction", settlement)
	}
	var signals int
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failure path")
		}
		if graph.Entries[i].Signal != nil {
			signals++
		}
	}
	if signals != 1 {
		t.Fatalf("interruption signal entries = %d, want exactly one", signals)
	}
}

// TestRunCompactionCanceledBetweenPiecesSettlesInterruption proves the
// between-pieces window: a two-piece snapshot whose first piece completes and
// settles ready durably, with cancellation observed before the run's success
// return — the run returns interruption, and the orchestration settles the
// quiet running Operation as interruption carrying the first piece's
// accumulated usage keyed by the compact model. A cancellation landing
// between the runs instead returns the same terminal at the second run's loop
// top and takes the same direct settlement.
func TestRunCompactionCanceledBetweenPiecesSettlesInterruption(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	snapshot := compactTwoPieceSnapshot(t)
	ctx, cancel := context.WithCancel(context.Background())
	requests := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests++
		return &cancelOnCloseStream{scriptStream: summaryStream("S1", model.Usage{InputTokens: 4, OutputTokens: 2}), cancel: cancel}, nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(ctx, c, testOpID, exec, testCapture(), snapshot)
	if err == nil || err.Error() != executionInterruptedDetail {
		t.Fatalf("error %v, want the run's interruption detail", err)
	}
	if summary != "" {
		t.Fatalf("summary %q, want none on the interrupted orchestration", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 4, OutputTokens: 2}) {
		t.Fatalf("usage = %+v, want the first piece's reported counts", usage)
	}
	if requests != 1 {
		t.Fatalf("compact transport calls = %d, want only the first piece", requests)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-settlement graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationInterruption || settlement.Detail != executionInterruptedDetail {
		t.Fatalf("settlement entry = %+v, want the run's interruption terminal", settlement)
	}
	if settlement.Model == nil || *settlement.Model != testCompactCapture().Model {
		t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
	}
	if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 4, OutputTokens: 2}) {
		t.Fatalf("settlement usage = %+v, want the first piece's accumulated counts", settlement.Usage)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failure path")
		}
	}
}

// TestSerializeCompactionMessagesCompleteInput proves the inert serialization:
// every model-visible field survives, malformed tool-call argument bytes are
// retained through their quoted representation, the source identity does not
// enter the lines, and the source messages are never mutated.
func TestSerializeCompactionMessagesCompleteInput(t *testing.T) {
	malformed := json.RawMessage(`{"x":1`)
	input := []model.Message{
		{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "<system-signal>hello &amp; <b>quoted</b></system-signal>"}}},
		{Role: model.RoleAssistant, Source: testModelRef(),
			Content: []model.ContentPart{
				{Kind: model.PartText, Text: "thinking out loud"},
				{Kind: model.PartOpaque, OpaqueWireType: "reasoning", Extra: model.Extra{"summary": json.RawMessage(`"hidden"`)}},
				{Kind: model.PartImageURL, URL: "https://example.com/img.png"},
			},
			Refusal: "partly refused",
			ToolCalls: []model.ToolCall{
				{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`), Extra: model.Extra{"kind": json.RawMessage(`"custom"`)}},
				{ID: "call-2", Name: "broken", Arguments: malformed},
			},
			Extra: model.Extra{"finish": json.RawMessage(`"stop"`)},
		},
		{Role: model.RoleTool, ToolCallID: "call-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "result body"}}},
	}
	owned := make([]model.Message, 0, len(input))
	for _, m := range input {
		msg, err := model.NewMessage(m)
		if err != nil {
			t.Fatalf("fixture message: %v", err)
		}
		owned = append(owned, msg)
	}
	snapshot := append([]model.Message(nil), owned...)

	lines, err := serializeCompactionMessages(snapshot)
	if err != nil {
		t.Fatalf("serializeCompactionMessages: %v", err)
	}
	if len(lines) != len(snapshot) {
		t.Fatalf("lines = %d, want one per message", len(lines))
	}
	decoded := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON: %q", i, line)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d unmarshal: %v", i, err)
		}
		decoded = append(decoded, obj)
		if strings.Contains(line, testModelRef().Provider) {
			t.Fatalf("line %d carries model-source replay metadata: %q", i, line)
		}
	}

	// The signal's wrapped text, the parts, refusal, extras and identities
	// survive verbatim.
	first := decoded[0]
	if first["Role"] != "user" {
		t.Fatalf("first line role = %v, want user", first["Role"])
	}
	signalParts, ok := first["Content"].([]any)
	if !ok || len(signalParts) != 1 {
		t.Fatalf("first line content = %v, want one part", first["Content"])
	}
	signalPart, _ := signalParts[0].(map[string]any)
	if signalPart["Text"] != "<system-signal>hello &amp; <b>quoted</b></system-signal>" {
		t.Fatalf("signal wrapper text lost: %v", signalPart["Text"])
	}
	second := decoded[1]
	if second["Role"] != "assistant" || second["Refusal"] != "partly refused" {
		t.Fatalf("assistant line = %v, want role and refusal preserved", second)
	}
	if second["Source"] != "" {
		t.Fatalf("assistant line source = %v, want the cleared source identity", second["Source"])
	}
	if !strings.Contains(lines[1], `"OpaqueWireType":"reasoning"`) || !strings.Contains(lines[1], `"summary":"hidden"`) {
		t.Fatalf("opaque part lost: %q", lines[1])
	}
	if !strings.Contains(lines[1], `"URL":"https://example.com/img.png"`) {
		t.Fatalf("image part lost: %q", lines[1])
	}
	if !strings.Contains(lines[1], `"finish":"stop"`) {
		t.Fatalf("message extra lost: %q", lines[1])
	}
	calls, ok := second["ToolCalls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("tool calls = %v, want both preserved", second["ToolCalls"])
	}
	firstCall, _ := calls[0].(map[string]any)
	if firstCall["ID"] != "call-1" || firstCall["Name"] != "echo" {
		t.Fatalf("first call = %v, want identity preserved", firstCall)
	}
	if !strings.Contains(lines[1], `"kind":"custom"`) {
		t.Fatalf("tool-call extra lost: %q", lines[1])
	}
	// Tool-call arguments ride the JSON encoding of their quoted byte
	// representation, keeping even malformed bytes marshalable and recoverable.
	for _, idx := range []int{0, 1} {
		call, _ := calls[idx].(map[string]any)
		raw, err := json.Marshal(call["Arguments"])
		if err != nil {
			t.Fatalf("call %d arguments marshal: %v", idx, err)
		}
		var quoted string
		if err := json.Unmarshal(raw, &quoted); err != nil {
			t.Fatalf("call %d arguments is not a JSON string: %v", idx, err)
		}
		recovered, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("call %d quoted arguments do not unquote: %v", idx, err)
		}
		want := `{"x":1}`
		if idx == 1 {
			want = `{"x":1`
		}
		if recovered != want {
			t.Fatalf("call %d recovered arguments %q, want the original raw bytes %q", idx, recovered, want)
		}
	}
	third := decoded[2]
	if third["Role"] != "tool" || third["ToolCallID"] != "call-1" {
		t.Fatalf("tool line = %v, want the tool-result identity preserved", third)
	}
	if !strings.Contains(lines[2], "result body") {
		t.Fatalf("tool result content lost: %q", lines[2])
	}

	// The source messages are unchanged, including Source, the ToolCalls
	// slice and the raw argument bytes.
	for i := range owned {
		if !reflect.DeepEqual(owned[i], snapshot[i]) {
			t.Fatalf("message %d changed through serialization:\nbefore %+v\nafter  %+v", i, owned[i], snapshot[i])
		}
		for j, call := range owned[i].ToolCalls {
			if string(call.Arguments) != string(snapshot[i].ToolCalls[j].Arguments) {
				t.Fatalf("message %d call %d argument bytes changed", i, j)
			}
		}
	}
}

// compactedFixtureGraph returns the fixture graph of a Session whose
// conversation was compacted: the register names the compaction entry and the
// given post-boundary entries follow it.
func compactedFixtureGraph(postBoundary ...testEntry) *testGraph {
	fixture := validTestGraph()
	comp := validCompactionEntry(testOpID)
	comp.EntryID = hexID(3)
	comp.BoundaryEntryID = hexID(2)
	fixture.entries = append(fixture.entries, testEntry{
		env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryCompaction, Sequence: 3, CommittedAt: testTime},
		compaction: &comp,
	})
	fixture.entries = append(fixture.entries, postBoundary...)
	fixture.session.State.CompactionEntryID = hexID(3)
	return fixture
}

// projectedSnapshot projects the fixture conversation and drops the leading
// system message, the frozen snapshot shape both compaction paths use.
func projectedSnapshot(t *testing.T, h *Harness, c *coordinator) []model.Message {
	t.Helper()
	msgs, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Role != model.RoleSystem {
		t.Fatalf("projection = %+v, want a leading system message", msgs)
	}
	return msgs[1:]
}

// TestRunCompactionExistingSummaryAndNewMessages proves the register-named
// summary message is part of the frozen snapshot together with the entries
// after the boundary, and the system prompt stays outside the input.
func TestRunCompactionExistingSummaryAndNewMessages(t *testing.T) {
	input4 := validInputEntry(testOpID)
	input4.EntryID = hexID(4)
	input4.Content = admissionContent("after the boundary")
	fixture := compactedFixtureGraph(testEntry{
		env:   Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryInput, Sequence: 4, CommittedAt: testTime},
		input: &input4,
	})
	h := newTestHarness(t, fixture.storage(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	snapshot := projectedSnapshot(t, h, c)
	if len(snapshot) != 2 {
		t.Fatalf("snapshot messages = %d, want the summary message and the post-boundary entry", len(snapshot))
	}

	var requests []model.Request
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, _, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), snapshot)
	if err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if summary != "S1" {
		t.Fatalf("summary %q, want the piece output", summary)
	}
	if len(requests) != 1 {
		t.Fatalf("compact transport calls = %d, want one piece", len(requests))
	}
	userText := requests[0].Messages[1].TextContent()
	if strings.Contains(userText, "Previous summary:") {
		t.Fatalf("the first piece carries a Previous summary block: %q", userText)
	}
	if !strings.Contains(userText, "Summary of the earlier conversation.") {
		t.Fatalf("the serialized input lost the existing summary: %q", userText)
	}
	if !strings.Contains(userText, "after the boundary") {
		t.Fatalf("the serialized input lost the post-boundary message: %q", userText)
	}
	if strings.Contains(userText, `"system"`) {
		t.Fatalf("the system prompt entered the serialized conversation: %q", userText)
	}
}

// TestRunCompactionSummaryOnlySessionSuppliesSummary proves a summary-only
// Session still supplies that summary as the serialized input.
func TestRunCompactionSummaryOnlySessionSuppliesSummary(t *testing.T) {
	fixture := compactedFixtureGraph()
	h := newTestHarness(t, fixture.storage(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	snapshot := projectedSnapshot(t, h, c)
	if len(snapshot) != 1 {
		t.Fatalf("snapshot messages = %d, want only the summary message", len(snapshot))
	}

	var requests []model.Request
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	if _, _, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), snapshot); err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("compact transport calls = %d, want one piece", len(requests))
	}
	userText := requests[0].Messages[1].TextContent()
	if !strings.Contains(userText, "Summary of the earlier conversation.") {
		t.Fatalf("the summary-only input lost the summary: %q", userText)
	}
	if strings.Contains(userText, "Previous summary:") {
		t.Fatalf("the first piece carries a Previous summary block: %q", userText)
	}
}

// TestRunCompactionEmptySnapshotFailsNothingToCompact proves an empty
// snapshot settles the quiet running Operation with the retained detail and
// writes no compaction work.
func TestRunCompactionEmptySnapshotFailsNothingToCompact(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no piece may run for an empty snapshot")
		return nil, errors.New("unreachable")
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), nil)
	if err == nil || err.Error() != "nothing to compact" {
		t.Fatalf("error %v, want the retained nothing-to-compact failure", err)
	}
	if summary != "" || usage != nil {
		t.Fatalf("summary %q usage %+v, want no summary and no usage", summary, usage)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-failure graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationFailure || settlement.Detail != "nothing to compact" {
		t.Fatalf("settlement entry = %+v, want the nothing-to-compact failure terminal", settlement)
	}
	if settlement.Model != nil || settlement.Usage != nil {
		t.Fatalf("settlement entry = %+v, want no usage on the never-run compaction", settlement)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failure path")
		}
	}
}

// TestRunCompactionErroredPieceCarriesOutputThroughTheRun proves the carried
// output through a real agent.Run: an errored piece with a retained payload
// settles its terminal inside its own effect and the returned failure
// settlement carries the errored output, satisfying the run's callback gate —
// the run returns the piece's own failure detail, never a settlement-shape
// boundary violation about an outputless settlement.
func TestRunCompactionErroredPieceCarriesOutputThroughTheRun(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil // text "partial" with usage, then the read failure
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: (&retrySpy{limit: 0}).retry, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), compactSnapshotMessages())
	if err == nil || !strings.Contains(err.Error(), "provider failure") {
		t.Fatalf("error %v, want the piece's own failure detail, not a settlement-shape boundary violation", err)
	}
	if summary != "" {
		t.Fatalf("summary %q, want none on the failed piece", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 5, OutputTokens: 7}) {
		t.Fatalf("usage = %+v, want the errored piece's reported counts", usage)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-settlement graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "provider failure") {
		t.Fatalf("settlement entry = %+v, want the failure terminal", settlement)
	}
	if settlement.Model == nil || *settlement.Model != testCompactCapture().Model {
		t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
	}
	if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 5, OutputTokens: 7}) {
		t.Fatalf("settlement usage = %+v, want the accumulated counts", settlement.Usage)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failure path")
		}
	}
}

// TestRunCompactionEmptyPieceSummaryFailsWithTheRetainedDetail proves the
// relocated textless-completed rule: the effect settles a textless completed
// piece ready, and the orchestration fails the empty summary after the run —
// the direct settlement carries the retained detail and the accumulated
// usage, exactly one model call runs with no retry and no next piece, and no
// compaction entry commits.
func TestRunCompactionEmptyPieceSummaryFailsWithTheRetainedDetail(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	refusal := turnDelta("stop", "")
	refusal.RefusalFragment = "no"
	calls := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		calls++
		return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), compactSnapshotMessages())
	if err == nil || err.Error() != "compaction summary is empty" {
		t.Fatalf("error %v, want the retained empty-summary detail", err)
	}
	if summary != "" {
		t.Fatalf("summary %q, want none on the failed piece", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 2, OutputTokens: 1}) {
		t.Fatalf("usage = %+v, want the piece's reported counts", usage)
	}
	if calls != 1 {
		t.Fatalf("compact transport calls = %d, want exactly one with no retry and no next piece", calls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-failure graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationFailure || settlement.Detail != "compaction summary is empty" {
		t.Fatalf("settlement entry = %+v, want the empty-summary failure terminal", settlement)
	}
	if settlement.Model == nil || *settlement.Model != testCompactCapture().Model {
		t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
	}
	if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 2, OutputTokens: 1}) {
		t.Fatalf("settlement usage = %+v, want the accumulated counts", settlement.Usage)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failure path")
		}
	}
}

// TestCompactionPieceBudgetArithmetic pins the budget arithmetic: the fixed
// 64-token wrapper overhead and the previous-summary subtraction in its
// message form.
func TestCompactionPieceBudgetArithmetic(t *testing.T) {
	capture := testCompactCapture()
	base := capture.ContextWindow - capture.OutputReserve - estimateTokens(capture.SystemPrompt, nil) - 64
	if got := compactionPieceBudget(capture, ""); got != base {
		t.Fatalf("empty-previous budget = %d, want %d", got, base)
	}
	previous := "a previous summary of some length"
	messageForm := estimateTokens("", []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: previous}}}})
	if got := compactionPieceBudget(capture, previous); got != base-messageForm {
		t.Fatalf("previous-summary budget = %d, want %d", got, base-messageForm)
	}
	// The subtraction uses the message form, not the plain-text form: the
	// estimator's per-message overhead is part of it.
	if messageForm != estimateTokens(previous, nil)+4 {
		t.Fatalf("message-form estimate %d, want the plain-text estimate plus the per-message overhead", messageForm)
	}
}

// TestCompactionBoundaryRows pins the pure boundary helper: the covered
// source is the frozen snapshot minus the prior summary message, counted
// from the prior compaction's own boundary target — every projectable kind
// derives the covered last message, an entry raced between the boundary
// target and the named compaction counts against the target's sequence, and
// the summary-only re-compaction names the prior compaction entry itself.
// The rows build minimal typed graphEntry values: an envelope with the
// sequence and the one non-nil payload pointer the kind needs.
func TestCompactionBoundaryRows(t *testing.T) {
	input := graphEntry{Envelope: Entry{ID: "in-1", Sequence: 1}, Input: &inputEntry{}}
	assistant := graphEntry{Envelope: Entry{ID: "as-1", Sequence: 2}, Assistant: &assistantEntry{}}
	toolResult := graphEntry{Envelope: Entry{ID: "tr-1", Sequence: 3}, ToolResult: &toolResultEntry{}}
	signal := graphEntry{Envelope: Entry{ID: "sg-1", Sequence: 4}, Signal: &signalEntry{}}

	t.Run("every projectable kind derives the covered last message", func(t *testing.T) {
		for _, covered := range []graphEntry{input, assistant, toolResult, signal} {
			if got := compactionBoundary([]graphEntry{covered}, "", 1); got != covered.Envelope.ID {
				t.Fatalf("boundary over the %s row = %q, want the covered entry %q", covered.Envelope.Kind, got, covered.Envelope.ID)
			}
		}
	})

	t.Run("an entry between the boundary target and the named compaction counts", func(t *testing.T) {
		compaction := graphEntry{Envelope: Entry{ID: "co-1", Sequence: 5}, Compaction: &compactionEntry{BoundaryEntryID: assistant.Envelope.ID}}
		// The frozen snapshot carried the prior summary and the raced
		// signal: the boundary is the first entry after the target's
		// sequence, not the first after the named entry's own.
		if got := compactionBoundary([]graphEntry{input, assistant, signal, compaction}, compaction.Envelope.ID, 2); got != signal.Envelope.ID {
			t.Fatalf("boundary = %q, want the raced signal %q counted after the boundary target", got, signal.Envelope.ID)
		}
	})

	t.Run("the summary-only re-compaction names the prior compaction entry", func(t *testing.T) {
		compaction := graphEntry{Envelope: Entry{ID: "co-1", Sequence: 3}, Compaction: &compactionEntry{BoundaryEntryID: assistant.Envelope.ID}}
		if got := compactionBoundary([]graphEntry{input, assistant, compaction}, compaction.Envelope.ID, 1); got != compaction.Envelope.ID {
			t.Fatalf("boundary = %q, want the named compaction entry itself", got)
		}
	})
}

// compactedEntryOf returns the one compaction entry of the validated graph.
func compactedEntryOf(t *testing.T, graph *sessionGraph) *compactionEntry {
	t.Helper()
	var found *compactionEntry
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			if found != nil {
				t.Fatalf("graph carries more than one compaction entry")
			}
			found = graph.Entries[i].Compaction
		}
	}
	if found == nil {
		t.Fatalf("graph carries no compaction entry")
	}
	return found
}

// operationStateOf returns the state of the named Operation in the validated
// graph.
func operationStateOf(t *testing.T, graph *sessionGraph, operationID string) OperationCurrentState {
	t.Helper()
	for i := range graph.Operations {
		if graph.Operations[i].Admission.OperationID == operationID {
			return graph.Operations[i].State
		}
	}
	t.Fatalf("graph carries no Operation %q", operationID)
	return OperationCurrentState{}
}

// TestCommitCompactionAutomaticCommitsAtomically proves the automatic shape:
// the compaction entry, the Session register's projection field, and both
// register usage totals land in one transaction while the Operation keeps
// running; validateUsage agrees and the projection flips to the summary.
func TestCommitCompactionAutomaticCommitsAtomically(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
	sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	opRevision := registerRevision(t, store, sessionID, opKey)
	sessionRevision := registerRevision(t, store, sessionID, sessionKey)
	beforeEntries, _ := storedSessionState(store, sessionID)

	usage := &UsageCount{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 4}
	// snapshotLen 1: the fresh fixture's frozen snapshot carries the one
	// admitted input message.
	if err := h.commitCompaction(c, testOpID, testCapture(), "summary one", 1, usage, false); err != nil {
		t.Fatalf("commitCompaction: %v", err)
	}

	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-commit graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.OperationID != testOpID || comp.Summary != "summary one" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned committed summary", comp)
	}
	if comp.BoundaryEntryID != graph.Entries[0].Envelope.ID {
		t.Fatalf("boundary %q, want the last projectable entry %q", comp.BoundaryEntryID, graph.Entries[0].Envelope.ID)
	}
	if comp.Model != testCompactCapture().Model || comp.ConfigurationRevision != "rev-1" {
		t.Fatalf("compaction entry identity = %+v rev %q, want the compact model and the capture's revision", comp.Model, comp.ConfigurationRevision)
	}
	if comp.Usage == nil || *comp.Usage != *usage {
		t.Fatalf("compaction entry usage = %+v, want the orchestration's accumulated total", comp.Usage)
	}
	if graph.Session.State.CompactionEntryID != comp.EntryID {
		t.Fatalf("compaction_entry_id %q, want the new entry %q", graph.Session.State.CompactionEntryID, comp.EntryID)
	}

	// The Operation continues: running, current, quiet, with no settlement.
	if graph.Session.State.CurrentOperationID != testOpID {
		t.Fatalf("current Operation %q, want the continuing %q", graph.Session.State.CurrentOperationID, testOpID)
	}
	state := operationStateOf(t, graph, testOpID)
	if state.Status != OperationRunning || state.ActiveEffect != nil {
		t.Fatalf("operation state = %+v, want running and quiet", state)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Settlement != nil {
			t.Fatalf("settlement entry committed on the automatic path")
		}
	}

	// Both register totals carry the accumulated usage keyed by the compact
	// model.
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: *usage}}}
	if !usageTotalsEqual(state.Usage, want) {
		t.Fatalf("operation usage = %+v, want %+v", state.Usage, want)
	}
	if !usageTotalsEqual(graph.Session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want %+v", graph.Session.State.Usage, want)
	}

	// One transaction: one new entry, each register advanced once.
	afterEntries, _ := storedSessionState(store, sessionID)
	if len(afterEntries) != len(beforeEntries)+1 {
		t.Fatalf("entry count %d, want %d (exactly the compaction entry)", len(afterEntries), len(beforeEntries)+1)
	}
	if got := registerRevision(t, store, sessionID, opKey); got != opRevision+1 {
		t.Fatalf("operation register revision %d, want %d", got, opRevision+1)
	}
	if got := registerRevision(t, store, sessionID, sessionKey); got != sessionRevision+1 {
		t.Fatalf("session register revision %d, want %d", got, sessionRevision+1)
	}

	// The projection flips to the summary message.
	after, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("projection messages = %d, want the system message and the summary", len(after))
	}
	if after[1].Role != model.RoleAssistant || after[1].Source != testCompactCapture().Model {
		t.Fatalf("summary message = %+v, want an assistant message sourced by the compact model", after[1])
	}
	if got := after[1].TextContent(); got != "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" {
		t.Fatalf("summary message text %q, want the verbatim framing around the committed summary", got)
	}
}

// TestCommitCompactionManualSettlesInTheSameTransaction proves the manual
// shape: the automatic members land, then the dedicated Operation settles as
// success in the same transaction — settlement entry, terminal Operation
// register, cleared current Operation — with the settlement itself carrying
// no usage.
func TestCommitCompactionManualSettlesInTheSameTransaction(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
	sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	opRevision := registerRevision(t, store, sessionID, opKey)
	sessionRevision := registerRevision(t, store, sessionID, sessionKey)
	beforeEntries, _ := storedSessionState(store, sessionID)

	usage := &UsageCount{InputTokens: 6, CachedInputTokens: 1, OutputTokens: 2}
	// snapshotLen 1: the fresh fixture's frozen snapshot carries the one
	// admitted input message.
	if err := h.commitCompaction(c, testOpID, testCapture(), "manual summary", 1, usage, true); err != nil {
		t.Fatalf("commitCompaction: %v", err)
	}

	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-commit graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.Summary != "manual summary" || comp.BoundaryEntryID != graph.Entries[0].Envelope.ID {
		t.Fatalf("compaction entry = %+v, want the committed summary at the projectable boundary", comp)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationSuccess || settlement.Detail != "" {
		t.Fatalf("settlement entry = %+v, want the success terminal with no detail", settlement)
	}
	if settlement.Model != nil || settlement.Usage != nil {
		t.Fatalf("settlement entry = %+v, want no usage on the settlement itself", settlement)
	}
	state := operationStateOf(t, graph, testOpID)
	if state.Status != OperationSuccess || state.Terminal == nil || state.Terminal.SettlementEntry.EntryID != settlement.EntryID {
		t.Fatalf("operation state = %+v, want the success terminal naming the settlement entry", state)
	}
	if graph.Session.State.CurrentOperationID != "" {
		t.Fatalf("current Operation %q, want it cleared", graph.Session.State.CurrentOperationID)
	}
	if graph.Session.State.CompactionEntryID != comp.EntryID {
		t.Fatalf("compaction_entry_id %q, want the new entry %q", graph.Session.State.CompactionEntryID, comp.EntryID)
	}

	// The usage landed through the compaction part: both totals carry it
	// keyed by the compact model.
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: *usage}}}
	if !usageTotalsEqual(state.Usage, want) {
		t.Fatalf("operation usage = %+v, want %+v", state.Usage, want)
	}
	if !usageTotalsEqual(graph.Session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want %+v", graph.Session.State.Usage, want)
	}

	// One transaction: the entry and the settlement are the only new
	// entries, and each register advanced once per write inside it.
	afterEntries, _ := storedSessionState(store, sessionID)
	if len(afterEntries) != len(beforeEntries)+2 {
		t.Fatalf("entry count %d, want %d (the compaction entry and the settlement)", len(afterEntries), len(beforeEntries)+2)
	}
	if got := registerRevision(t, store, sessionID, opKey); got != opRevision+2 {
		t.Fatalf("operation register revision %d, want %d", got, opRevision+2)
	}
	if got := registerRevision(t, store, sessionID, sessionKey); got != sessionRevision+2 {
		t.Fatalf("session register revision %d, want %d", got, sessionRevision+2)
	}

	// The projection flips to the summary message.
	after, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if len(after) != 2 || after[1].Role != model.RoleAssistant || after[1].TextContent() != "[Previous conversation summary]\n\nmanual summary\n\n[End of summary. Continue from here.]" {
		t.Fatalf("projection = %+v, want the system message and the committed summary", after)
	}
}

// TestCommitCompactionInjectedFailureLeavesPreviousStateCurrent proves the
// failure contract: an injected transaction failure commits nothing of the
// compaction — the previous projection stays current — and the commit's own
// failure settles the quiet running Operation through the direct terminal
// settlement carrying the commit error and the accumulated usage keyed by the
// compact model.
func TestCommitCompactionInjectedFailureLeavesPreviousStateCurrent(t *testing.T) {
	t.Run("a persistently failing store leaves every durable value unchanged", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
		sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		opRevision := registerRevision(t, store, sessionID, opKey)
		sessionRevision := registerRevision(t, store, sessionID, sessionKey)
		beforeEntries, _ := storedSessionState(store, sessionID)
		before, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		store.txHook = func(string) error { return errors.New("injected storage failure") }

		if err := h.commitCompaction(c, testOpID, testCapture(), "summary", 1, nil, false); err == nil {
			t.Fatalf("commitCompaction succeeded past an injected failure")
		}

		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-failure graph: %v", err)
		}
		afterEntries, _ := storedSessionState(store, sessionID)
		if len(afterEntries) != len(beforeEntries) {
			t.Fatalf("entry count %d, want the unchanged %d", len(afterEntries), len(beforeEntries))
		}
		if got := registerRevision(t, store, sessionID, opKey); got != opRevision {
			t.Fatalf("operation register revision %d, want the unchanged %d", got, opRevision)
		}
		if got := registerRevision(t, store, sessionID, sessionKey); got != sessionRevision {
			t.Fatalf("session register revision %d, want the unchanged %d", got, sessionRevision)
		}
		if graph.Session.State.CompactionEntryID != "" {
			t.Fatalf("compaction_entry_id %q, want it empty", graph.Session.State.CompactionEntryID)
		}
		state := operationStateOf(t, graph, testOpID)
		if state.Status != OperationRunning {
			t.Fatalf("operation state = %+v, want still running", state)
		}
		after, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("a one-shot commit failure settles the accumulated usage", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		before, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		injected := errors.New("injected commit failure")
		seen := false
		store.txHook = func(step string) error {
			if step == "insert_entry" && !seen {
				seen = true
				return injected
			}
			return nil
		}
		usage := &UsageCount{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 4}

		err = h.commitCompaction(c, testOpID, testCapture(), "summary", 1, usage, false)
		if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
			t.Fatalf("error %v, want the injected commit failure", err)
		}

		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-failure graph: %v", err)
		}
		for i := range graph.Entries {
			if graph.Entries[i].Compaction != nil {
				t.Fatalf("compaction entry committed on a failed transaction")
			}
		}
		if graph.Session.State.CompactionEntryID != "" {
			t.Fatalf("compaction_entry_id %q, want it empty", graph.Session.State.CompactionEntryID)
		}
		settlement := settlementEntryOf(t, graph)
		if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "injected commit failure") {
			t.Fatalf("settlement entry = %+v, want the commit failure terminal", settlement)
		}
		if settlement.Model == nil || *settlement.Model != testCompactCapture().Model {
			t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
		}
		if settlement.Usage == nil || *settlement.Usage != *usage {
			t.Fatalf("settlement usage = %+v, want the accumulated total", settlement.Usage)
		}
		after, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("the manual settlement shares the commit transaction", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		before, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		injected := errors.New("injected settlement failure")
		inserts := 0
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				inserts++
				if inserts == 2 { // the settlement entry's insertion inside the commit transaction
					return injected
				}
			}
			return nil
		}

		err = h.commitCompaction(c, testOpID, testCapture(), "summary", 1, nil, true)
		if err == nil || !strings.Contains(err.Error(), "injected settlement failure") {
			t.Fatalf("error %v, want the injected settlement failure", err)
		}

		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-failure graph: %v", err)
		}
		for i := range graph.Entries {
			if graph.Entries[i].Compaction != nil {
				t.Fatalf("compaction entry survived the failed shared transaction")
			}
		}
		if graph.Session.State.CompactionEntryID != "" {
			t.Fatalf("compaction_entry_id %q, want it empty", graph.Session.State.CompactionEntryID)
		}
		// The commit's own failure settlement stands alone.
		settlement := settlementEntryOf(t, graph)
		if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "injected settlement failure") {
			t.Fatalf("settlement entry = %+v, want the commit failure terminal", settlement)
		}
		after, err := h.projectContext(c, testOpID)
		if err != nil {
			t.Fatalf("projectContext: %v", err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}
