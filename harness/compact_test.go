// Contract coverage for the compaction orchestration: the frozen projected
// conversation is serialized into inert transcript lines, joined whole into
// one full-input compact request, and that request runs one agent.Run
// invocation whose model effect commits the ordinary intent over the compact
// transport and settles without writing entries.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	return reg.Revision
}

// settlementEntryOf returns the one operation settlement entry of the
// validated fixture graph.
func settlementEntryOf(t *testing.T, graph *sessionGraph) *operationSettlementEntry {
	t.Helper()
	var found *operationSettlementEntry
	for i := range graph.Entries {
		if graph.Entries[i].Settlement != nil {
			testassert.Require(t, found == nil, "graph carries more than one settlement entry")
			found = graph.Entries[i].Settlement
		}
	}
	testassert.Require(t, found != nil, "graph carries no settlement entry")
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

// TestRunCompactionReadyPathRunsOneRequest proves the ready shape: the run
// executes exactly one request whose user text is the serialized snapshot
// alone, one intent transaction commits before it, the ready settlement
// inserts no entry and leaves both registers with no usage, and the reserved
// identity stays unused.
func TestRunCompactionReadyPathRunsOneRequest(t *testing.T) {
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
	testassert.NoError(t, err)
	testassert.Equal(t, summary, "summary one")
	testassert.Require(t, usage != nil && *usage == (UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), "usage = %+v, want the one request's reported counts", usage)
	testassert.Equal(t, len(requests), 1)
	req := requests[0]
	testassert.Require(t, len(req.Messages) == 2 && len(req.Tools) == 0, "got %v, want %v; got %v, want %v", len(req.Messages), 2, len(req.Tools), 0)
	testassert.Require(t, req.Messages[0].Role == model.RoleSystem && req.Messages[0].Source == (model.ModelRef{}), "system message = %+v, want role system with zero source", req.Messages[0])
	{
		got := req.Messages[0].TextContent()
		testassert.Equal(t, got, testCapture().Compact.SystemPrompt)
	}
	testassert.Require(t, req.Messages[1].Role == model.RoleUser && req.Messages[1].Source == (model.ModelRef{}), "user message = %+v, want role user with zero source", req.Messages[1])
	expectedLines, err := serializeCompactionMessages(snapshot)
	testassert.NoError(t, err)
	{
		got := req.Messages[1].TextContent()
		testassert.Equal(t, got, strings.Join(expectedLines, "\n"))
	}
	if strings.Contains(req.Messages[1].TextContent(), "Previous summary:") {
		t.Fatalf("the request carries a Previous summary block: %q", req.Messages[1].TextContent())
	}

	// One intent transaction and one result transaction: the operation
	// register advanced exactly twice, the session register once.
	{
		got := registerRevision(t, store, sessionID, opKey)
		testassert.Equal(t, got, opRevision+2)
	}
	{
		got := registerRevision(t, store, sessionID, sessionKey)
		testassert.Equal(t, got, sessionRevision+1)
	}
	afterEntries, _ := storedSessionState(store, sessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries))
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	ids := entryIDs(t, graph)
	testassert.Require(t, reservedID != "" && !ids[reservedID], "reserved identity %q must stay unused by any entry", reservedID)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.Usage.ByModel) == 0, "operation state = %+v, want running with the effect cleared; operation usage = %+v, want none on the ready path", rec.State, rec.State.Usage)
	testassert.Require(t, len(graph.Session.State.Usage.ByModel) == 0, "session usage = %+v, want none on the ready path", graph.Session.State.Usage)
}

// TestRunCompactionFullInputRunsOneRequest is the single-request regression:
// a serialized input larger than any per-piece budget the removed packing
// loop imposed runs as one full-input request whose user text is the
// serialized snapshot alone, with the one response's reported usage.
func TestRunCompactionFullInputRunsOneRequest(t *testing.T) {
	h, _, c, _ := newEffectHarness(t, nil)
	capture := testCapture()
	unit := "summarize the whole conversation turn carefully "
	snapshot := []model.Message{
		{Role: model.RoleUser, Content: admissionContent(strings.Repeat(unit, 70))},
		{Role: model.RoleUser, Content: admissionContent(strings.Repeat(unit, 70))},
		{Role: model.RoleUser, Content: admissionContent(strings.Repeat(unit, 70))},
	}
	lines, err := serializeCompactionMessages(snapshot)
	testassert.NoError(t, err)
	var requests []model.Request
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{InputTokens: 5, OutputTokens: 3}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, snapshot)
	testassert.NoError(t, err)
	testassert.Equal(t, summary, "S1")
	testassert.Require(t, usage != nil && *usage == (UsageCount{InputTokens: 5, OutputTokens: 3}), "usage = %+v, want the one request's reported counts", usage)
	testassert.Equal(t, len(requests), 1)
	{
		got := requests[0].Messages[1].TextContent()
		testassert.Equal(t, got, strings.Join(lines, "\n"))
	}
}

// TestCompactModelEffectSettlements proves the effect contract: attempts run
// through the compact transport under the retry policy, a failed summary that
// reports usage settles the terminal with that reported usage keyed by the
// compact model and carries the errored assembly on the failure row, an
// interrupted effect settles through the existing terminal path with no output
// before any assembly, and a textless completed output settles ready with its
// output — the orchestration validates the summary text after the settlement.
// The ready settlement's durable shape is pinned by the ready-path
// orchestration test.
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
			return summaryStream("summary one", model.Usage{}), nil
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: spy.retry, NormalizeTool: objectNormalize}
		var reported *UsageCount
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &reported)
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}})
		testassert.NoError(t, err)
		set, err := invokeModelEffect(t, me, req, nil)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoReady)
		testassert.Equal(t, calls, 2)
		testassert.Require(t, len(spy.attempts) == 1 && spy.attempts[0] == 1 && len(spy.seen) == 1, "retry consultations = %v seen %v, want the one failed attempt classified by the policy", spy.attempts, spy.seen)
		{
			_, ok := spy.seen[0].(*net.OpError)
			testassert.Require(t, ok, "classified failure %v, want the first attempt's transport error", spy.seen[0])
		}
	})

	t.Run("failed summary settles the reported usage", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			// The failing response reports usage before the read failure:
			// the errored assembly retains it.
			return failStream(errors.New("provider failure"),
				turnDelta("", "partial"), usageDelta(model.Usage{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5})), nil
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: (&retrySpy{limit: 0}).retry, NormalizeTool: objectNormalize}
		var reported *UsageCount
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &reported)
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}})
		testassert.NoError(t, err)
		set, err := invokeModelEffect(t, me, req, nil)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoFailure && set.Detail != "" && strings.Contains(set.Detail, "provider failure") && (set.Output != nil && set.Output.Status == model.OutputErrored), "settlement = %+v, want the failure carrying the summary failure detail; settlement output = %+v, want the errored assembly carried on the failure row", set, set.Output)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "provider failure"), "settlement entry = %+v, want the failure terminal", settlement)
		testassert.Require(t, settlement.Model != nil && *settlement.Model == compactRef, "settlement model = %+v, want the compact model identity", settlement.Model)
		testassert.Require(t, settlement.Usage != nil && *settlement.Usage == (UsageCount{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5}) && settlement.CompactionUsage, "settlement usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Usage)
		testassert.Require(t, len(graph.Session.State.Usage.ByModel) == 1 && graph.Session.State.Usage.ByModel[0].Model == compactRef, "session usage = %+v, want the reported total keyed by the compact model", graph.Session.State.Usage)
		view, err := h.ReadSessionView(context.Background(), sessionID)
		testassert.NoError(t, err)
		{
			measured := view.History.LatestContextUsage()
			testassert.Require(t, measured == nil, "latest context measurement = %+v, want the marked summary usage billed but ineligible", measured)
		}
	})

	t.Run("interruption settles through the terminal path", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			t.Errorf("no attempt may start under a canceled execution context")
			return nil, errors.New("unreachable")
		}
		exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
		var reported *UsageCount
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &reported)
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}})
		testassert.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		set, err := invokeModelEffectCtx(t, me, ctx, req, nil)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoInterruption && set.Detail == executionInterruptedDetail && set.Output == nil, "settlement = %+v, want the fixed interruption settlement; settlement output = %+v, want none on the pre-assembly cancellation", set, set.Output)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail && settlement.CompactionUsage, "settlement entry = %+v, want the interruption terminal; settlement compaction_usage = false, want the compact effect's interruption provenance marked", settlement)
		var signals int
		for i := range graph.Entries {
			if graph.Entries[i].Signal != nil {
				signals++
			}
		}
		testassert.Equal(t, signals, 1)
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
		var reported *UsageCount
		me := h.compactModelEffect(c, testOpID, exec, testCapture(), &reported)
		req, err := model.NewRequest(model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}})
		testassert.NoError(t, err)
		set, err := invokeModelEffect(t, me, req, nil)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoReady && set.Output != nil && set.Output.Status == model.OutputCompleted, "settlement = %+v, want the ready settlement carrying the completed output", set)
		testassert.Equal(t, calls, 1)
	})
}

// TestRunCompactionCanceledBeforeRunSettlesInterruption proves the pre-run
// window: an execution context already canceled before the compact request
// makes the run return interruption with no effect ever begun, and the
// orchestration settles the quiet running Operation as interruption with no
// usage and no compaction entry.
func TestRunCompactionCanceledBeforeRunSettlesInterruption(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no request may run under a canceled execution context")
		return nil, errors.New("unreachable")
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, usage, err := h.runCompaction(ctx, c, testOpID, exec, testCapture(), compactSnapshotMessages())
	testassert.Require(t, err != nil && err.Error() == executionInterruptedDetail, "error %v, want the run's interruption detail", err)
	testassert.Require(t, summary == "" && usage == nil, "summary %q usage %+v, want no summary and no usage", summary, usage)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail && (settlement.Model == nil && settlement.Usage == nil) && settlement.CompactionUsage, "settlement entry = %+v, want the run's interruption terminal; settlement entry = %+v, want no usage on the never-run compaction; settlement compaction_usage = false, want the direct interruption settlement marked", settlement, settlement)
	var signals int
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
		if graph.Entries[i].Signal != nil {
			signals++
		}
	}
	testassert.Equal(t, signals, 1)
}

// TestRunCompactionCanceledAfterSummarySettlesInterruption proves the
// post-result window: the single summary request completes and its ready
// settlement lands durably, with cancellation observed before the run's
// success return — the run returns interruption, and the orchestration settles
// the quiet running Operation as interruption carrying the summary's reported
// usage keyed by the compact model, with the marker provenance. A
// cancellation landing inside the run's loop instead returns the same terminal
// and takes the same direct settlement.
func TestRunCompactionCanceledAfterSummarySettlesInterruption(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	snapshot := compactSnapshotMessages()
	ctx, cancel := context.WithCancel(context.Background())
	requests := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests++
		return &cancelOnCloseStream{scriptStream: summaryStream("S1", model.Usage{InputTokens: 4, OutputTokens: 2}), cancel: cancel}, nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(ctx, c, testOpID, exec, testCapture(), snapshot)
	testassert.Require(t, err != nil && err.Error() == executionInterruptedDetail, "error %v, want the run's interruption detail", err)
	testassert.Equal(t, summary, "")
	testassert.Require(t, usage != nil && *usage == (UsageCount{InputTokens: 4, OutputTokens: 2}), "usage = %+v, want the summary's reported counts", usage)
	testassert.Equal(t, requests, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail, "settlement entry = %+v, want the run's interruption terminal", settlement)
	testassert.Require(t, settlement.Model != nil && *settlement.Model == testCompactCapture().Model, "settlement model = %+v, want the compact model identity", settlement.Model)
	testassert.Require(t, settlement.Usage != nil && *settlement.Usage == (UsageCount{InputTokens: 4, OutputTokens: 2}) && settlement.CompactionUsage, "settlement usage = %+v, want the summary's reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Usage)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
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
		testassert.NoError(t, err)
		owned = append(owned, msg)
	}
	snapshot := append([]model.Message(nil), owned...)

	lines, err := serializeCompactionMessages(snapshot)
	testassert.NoError(t, err)
	testassert.Equal(t, len(lines), len(snapshot))
	decoded := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		testassert.Require(t, json.Valid([]byte(line)), "line %d is not valid JSON: %q", i, line)
		var obj map[string]any
		{
			err := json.Unmarshal([]byte(line), &obj)
			testassert.Require(t, err == nil, "line %d unmarshal: %v", i, err)
		}
		decoded = append(decoded, obj)
		testassert.Require(t, !strings.Contains(line, testModelRef().Provider), "line %d carries model-source replay metadata: %q", i, line)
	}

	// The signal's wrapped text, the parts, refusal, extras and identities
	// survive verbatim.
	first := decoded[0]
	testassert.Equal(t, first["Role"], "user")
	signalParts, ok := first["Content"].([]any)
	testassert.Require(t, ok && len(signalParts) == 1, "first line content = %v, want one part", first["Content"])
	signalPart, _ := signalParts[0].(map[string]any)
	testassert.Equal(t, signalPart["Text"], "<system-signal>hello &amp; <b>quoted</b></system-signal>")
	second := decoded[1]
	testassert.Require(t, second["Role"] == "assistant" && second["Refusal"] == "partly refused" && second["Source"] == "", "assistant line = %v, want role and refusal preserved; got %v, want %v", second, second["Source"], "")
	testassert.Require(t, strings.Contains(lines[1], `"OpaqueWireType":"reasoning"`) && strings.Contains(lines[1], `"summary":"hidden"`), "opaque part lost: %q", lines[1])
	testassert.Require(t, strings.Contains(lines[1], `"URL":"https://example.com/img.png"`), "image part lost: %q", lines[1])
	testassert.Require(t, strings.Contains(lines[1], `"finish":"stop"`), "message extra lost: %q", lines[1])
	calls, ok := second["ToolCalls"].([]any)
	testassert.Require(t, ok && len(calls) == 2, "tool calls = %v, want both preserved", second["ToolCalls"])
	firstCall, _ := calls[0].(map[string]any)
	testassert.Require(t, firstCall["ID"] == "call-1" && firstCall["Name"] == "echo", "first call = %v, want identity preserved", firstCall)
	testassert.Require(t, strings.Contains(lines[1], `"kind":"custom"`), "tool-call extra lost: %q", lines[1])
	// Tool-call arguments ride the JSON encoding of their quoted byte
	// representation, keeping even malformed bytes marshalable and recoverable.
	for _, idx := range []int{0, 1} {
		call, _ := calls[idx].(map[string]any)
		raw, err := json.Marshal(call["Arguments"])
		testassert.Require(t, err == nil, "call %d arguments marshal: %v", idx, err)
		var quoted string
		{
			err := json.Unmarshal(raw, &quoted)
			testassert.Require(t, err == nil, "call %d arguments is not a JSON string: %v", idx, err)
		}
		recovered, err := strconv.Unquote(quoted)
		testassert.Require(t, err == nil, "call %d quoted arguments do not unquote: %v", idx, err)
		want := `{"x":1}`
		if idx == 1 {
			want = `{"x":1`
		}
		testassert.Require(t, recovered == want, "call %d recovered arguments %q, want the original raw bytes %q", idx, recovered, want)
	}
	third := decoded[2]
	testassert.Require(t, third["Role"] == "tool" && third["ToolCallID"] == "call-1", "tool line = %v, want the tool-result identity preserved", third)
	testassert.Require(t, strings.Contains(lines[2], "result body"), "tool result content lost: %q", lines[2])

	// The source messages are unchanged, including Source, the ToolCalls
	// slice and the raw argument bytes.
	for i := range owned {
		testassert.Require(t, reflect.DeepEqual(owned[i], snapshot[i]), "message %d changed through serialization:\nbefore %+v\nafter  %+v", i, owned[i], snapshot[i])
		for j, call := range owned[i].ToolCalls {
			testassert.Require(t, string(call.Arguments) == string(snapshot[i].ToolCalls[j].Arguments), "message %d call %d argument bytes changed", i, j)
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
	testassert.NoError(t, err)
	testassert.Require(t, len(msgs) != 0 && msgs[0].Role == model.RoleSystem, "projection = %+v, want a leading system message", msgs)
	return msgs[1:]
}

// TestRunCompactionExistingSummaryAndNewMessages proves the register-named
// summary message is part of the frozen snapshot together with the entries
// after the boundary as ordinary projected input, and the system prompt stays
// outside the input.
func TestRunCompactionExistingSummaryAndNewMessages(t *testing.T) {
	input4, settlement4, op4 := projectionFollowUp(hexID(4), "op-2", "after the boundary", hexID(5), 4, 5)
	fixture := compactedFixtureGraph(input4, settlement4)
	fixture.ops = append(fixture.ops, op4)
	h := newTestHarness(t, fixture.storage(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)
	testassert.Equal(t, len(snapshot), 2)

	var requests []model.Request
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, _, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), snapshot)
	testassert.NoError(t, err)
	testassert.Equal(t, summary, "S1")
	testassert.Equal(t, len(requests), 1)
	userText := requests[0].Messages[1].TextContent()
	testassert.Require(t, !strings.Contains(userText, "Previous summary:") && strings.Contains(userText, "Summary of the earlier conversation.") && strings.Contains(userText, "after the boundary") && !strings.Contains(userText, `"system"`), "the request carries a Previous summary block: %q; the serialized input lost the existing summary: %q; the serialized input lost the post-boundary message: %q; the system prompt entered the serialized conversation: %q", userText, userText, userText, userText)
}

// TestRunCompactionSummaryOnlySessionSuppliesSummary proves a summary-only
// Session still supplies that summary as the serialized input.
func TestRunCompactionSummaryOnlySessionSuppliesSummary(t *testing.T) {
	fixture := compactedFixtureGraph()
	h := newTestHarness(t, fixture.storage(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)
	testassert.Equal(t, len(snapshot), 1)

	var requests []model.Request
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	{
		_, _, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), snapshot)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, len(requests), 1)
	userText := requests[0].Messages[1].TextContent()
	testassert.Require(t, strings.Contains(userText, "Summary of the earlier conversation.") && !strings.Contains(userText, "Previous summary:"), "the summary-only input lost the summary: %q; the request carries a Previous summary block: %q", userText, userText)
}

// TestRunCompactionEmptySnapshotFailsNothingToCompact proves an empty
// snapshot settles the quiet running Operation with the retained detail and
// writes no compaction work.
func TestRunCompactionEmptySnapshotFailsNothingToCompact(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no request may run for an empty snapshot")
		return nil, errors.New("unreachable")
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), nil)
	testassert.Require(t, err != nil && err.Error() == "nothing to compact", "error %v, want the retained nothing-to-compact failure", err)
	testassert.Require(t, summary == "" && usage == nil, "summary %q usage %+v, want no summary and no usage", summary, usage)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationFailure && settlement.Detail == "nothing to compact" && (settlement.Model == nil && settlement.Usage == nil), "settlement entry = %+v, want the nothing-to-compact failure terminal; settlement entry = %+v, want no usage on the never-run compaction", settlement, settlement)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
	}
}

// TestRunCompactionErroredSummaryCarriesOutputThroughTheRun proves the carried
// output through a real agent.Run: an errored summary with a retained payload
// settles its terminal inside its own effect and the returned failure
// settlement carries the errored output, satisfying the run's callback gate —
// the run returns the summary's own failure detail, never a settlement-shape
// boundary violation about an outputless settlement.
func TestRunCompactionErroredSummaryCarriesOutputThroughTheRun(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil // text "partial" with usage, then the read failure
	}
	exec := Execution{Model: forbiddenConversationModel(t), CompactModel: compact, Retry: (&retrySpy{limit: 0}).retry, NormalizeTool: objectNormalize}
	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, testCapture(), compactSnapshotMessages())
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "provider failure"), "error %v, want the summary's own failure detail, not a settlement-shape boundary violation", err)
	testassert.Equal(t, summary, "")
	testassert.Require(t, usage != nil && *usage == (UsageCount{InputTokens: 5, OutputTokens: 7}), "usage = %+v, want the errored summary's reported counts", usage)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "provider failure"), "settlement entry = %+v, want the failure terminal", settlement)
	testassert.Require(t, settlement.Model != nil && *settlement.Model == testCompactCapture().Model, "settlement model = %+v, want the compact model identity", settlement.Model)
	testassert.Require(t, settlement.Usage != nil && *settlement.Usage == (UsageCount{InputTokens: 5, OutputTokens: 7}) && settlement.CompactionUsage, "settlement usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Usage)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
	}
}

// TestRunCompactionEmptySummaryFailsWithTheRetainedDetail proves the
// textless-completed rule: the effect settles a textless completed summary
// ready, and the orchestration fails the empty summary after the run — the
// direct settlement carries the retained detail and the reported usage,
// exactly one model call runs with no retry, and no compaction entry commits.
func TestRunCompactionEmptySummaryFailsWithTheRetainedDetail(t *testing.T) {
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
	testassert.Require(t, err != nil && err.Error() == "compaction summary is empty", "error %v, want the retained empty-summary detail", err)
	testassert.Equal(t, summary, "")
	testassert.Require(t, usage != nil && *usage == (UsageCount{InputTokens: 2, OutputTokens: 1}), "usage = %+v, want the summary's reported counts", usage)
	testassert.Equal(t, calls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationFailure && settlement.Detail == "compaction summary is empty", "settlement entry = %+v, want the empty-summary failure terminal", settlement)
	testassert.Require(t, settlement.Model != nil && *settlement.Model == testCompactCapture().Model, "settlement model = %+v, want the compact model identity", settlement.Model)
	testassert.Require(t, settlement.Usage != nil && *settlement.Usage == (UsageCount{InputTokens: 2, OutputTokens: 1}) && settlement.CompactionUsage, "settlement usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Usage)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
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
			{
				got := compactionBoundary([]graphEntry{covered}, "", 1)
				testassert.Require(t, got == covered.Envelope.ID, "boundary over the %s row = %q, want the covered entry %q", covered.Envelope.Kind, got, covered.Envelope.ID)
			}
		}
	})

	t.Run("an entry between the boundary target and the named compaction counts", func(t *testing.T) {
		compaction := graphEntry{Envelope: Entry{ID: "co-1", Sequence: 5}, Compaction: &compactionEntry{BoundaryEntryID: assistant.Envelope.ID}}
		// The frozen snapshot carried the prior summary and the raced
		// signal: the boundary is the first entry after the target's
		// sequence, not the first after the named entry's own.
		{
			got := compactionBoundary([]graphEntry{input, assistant, signal, compaction}, compaction.Envelope.ID, 2)
			testassert.Equal(t, got, signal.Envelope.ID)
		}
	})

	t.Run("the summary-only re-compaction names the prior compaction entry", func(t *testing.T) {
		compaction := graphEntry{Envelope: Entry{ID: "co-1", Sequence: 3}, Compaction: &compactionEntry{BoundaryEntryID: assistant.Envelope.ID}}
		{
			got := compactionBoundary([]graphEntry{input, assistant, compaction}, compaction.Envelope.ID, 1)
			testassert.Equal(t, got, compaction.Envelope.ID)
		}
	})
}

// UsageByModelForTest returns the totals' pieces keyed by ref: the shared
// usage-by-model selection; each caller keeps its own expected counts.
func UsageByModelForTest(totals UsageTotals, ref model.ModelRef) UsageTotals {
	var out UsageTotals
	for _, mu := range totals.ByModel {
		if mu.Model == ref {
			out.ByModel = append(out.ByModel, mu)
		}
	}
	return out
}

// compactedEntryOf returns the one compaction entry of the validated graph.
func compactedEntryOf(t *testing.T, graph *sessionGraph) *compactionEntry {
	t.Helper()
	var found *compactionEntry
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			testassert.Require(t, found == nil, "graph carries more than one compaction entry")
			found = graph.Entries[i].Compaction
		}
	}
	testassert.Require(t, found != nil, "graph carries no compaction entry")
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
	{
		err := h.commitCompaction(c, testOpID, testCapture(), "summary one", 1, usage, false)
		testassert.NoError(t, err)
	}

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.OperationID == testOpID && comp.Summary == "summary one", "compaction entry = %+v, want the Operation-owned committed summary", comp)
	testassert.Equal(t, comp.BoundaryEntryID, graph.Entries[0].Envelope.ID)
	testassert.Require(t, comp.Model == testCompactCapture().Model && comp.ConfigurationRevision == "rev-1", "compaction entry identity = %+v rev %q, want the compact model and the capture's revision", comp.Model, comp.ConfigurationRevision)
	testassert.Require(t, comp.Usage != nil && *comp.Usage == *usage, "compaction entry usage = %+v, want the orchestration's reported total", comp.Usage)
	testassert.Equal(t, graph.Session.State.CompactionEntryID, comp.EntryID)

	// The Operation continues: running, current, quiet, with no settlement.
	testassert.Equal(t, graph.Session.State.CurrentOperationID, testOpID)
	state := operationStateOf(t, graph, testOpID)
	testassert.Require(t, state.Status == OperationRunning && state.ActiveEffect == nil, "operation state = %+v, want running and quiet", state)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil, "settlement entry committed on the automatic path")
	}

	// Both register totals carry the reported usage keyed by the compact
	// model.
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: *usage}}}
	testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want %+v", state.Usage, want)
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want %+v", graph.Session.State.Usage, want)

	// One transaction: one new entry, each register advanced once.
	afterEntries, _ := storedSessionState(store, sessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries)+1)
	{
		got := registerRevision(t, store, sessionID, opKey)
		testassert.Equal(t, got, opRevision+1)
	}
	{
		got := registerRevision(t, store, sessionID, sessionKey)
		testassert.Equal(t, got, sessionRevision+1)
	}

	// The projection flips to the summary message.
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Equal(t, len(after), 2)
	testassert.Require(t, after[1].Role == model.RoleAssistant && after[1].Source == testCompactCapture().Model, "summary message = %+v, want an assistant message sourced by the compact model", after[1])
	{
		got := after[1].TextContent()
		testassert.Equal(t, got, "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]")
	}
}

// TestCommitCompactionManualSettlesInTheSameTransaction proves the manual
// shape: the automatic members land, then the dedicated Operation settles as
// success in the same transaction — settlement entry, terminal Operation
// register, cleared current Operation — with the settlement itself carrying
// no usage and the unmarked source.
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
	{
		err := h.commitCompaction(c, testOpID, testCapture(), "manual summary", 1, usage, true)
		testassert.NoError(t, err)
	}

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.Summary == "manual summary" && comp.BoundaryEntryID == graph.Entries[0].Envelope.ID, "compaction entry = %+v, want the committed summary at the projectable boundary", comp)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationSuccess && settlement.Detail == "" && (settlement.Model == nil && settlement.Usage == nil) && !settlement.CompactionUsage, "settlement entry = %+v, want the success terminal with no detail; settlement entry = %+v, want no usage on the settlement itself; settlement compaction_usage = true, want the manual settlement unmarked", settlement, settlement)
	state := operationStateOf(t, graph, testOpID)
	testassert.Require(t, state.Status == OperationSuccess && state.Terminal != nil && state.Terminal.SettlementEntry.EntryID == settlement.EntryID, "operation state = %+v, want the success terminal naming the settlement entry", state)
	testassert.Equal(t, graph.Session.State.CurrentOperationID, "")
	testassert.Equal(t, graph.Session.State.CompactionEntryID, comp.EntryID)

	// The usage landed through the compaction part: both totals carry it
	// keyed by the compact model.
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: *usage}}}
	testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want %+v", state.Usage, want)
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want %+v", graph.Session.State.Usage, want)

	// One transaction: the entry and the settlement are the only new
	// entries, and each register advanced once per write inside it.
	afterEntries, _ := storedSessionState(store, sessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries)+2)
	{
		got := registerRevision(t, store, sessionID, opKey)
		testassert.Equal(t, got, opRevision+2)
	}
	{
		got := registerRevision(t, store, sessionID, sessionKey)
		testassert.Equal(t, got, sessionRevision+2)
	}

	// The projection flips to the summary message.
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 2 && after[1].Role == model.RoleAssistant && after[1].TextContent() == "[Previous conversation summary]\n\nmanual summary\n\n[End of summary. Continue from here.]", "projection = %+v, want the system message and the committed summary", after)
}

// TestCommitCompactionInjectedFailureLeavesPreviousStateCurrent proves the
// failure contract: an injected transaction failure commits nothing of the
// compaction — the previous projection stays current — and the commit's own
// failure settles the quiet running Operation through the direct terminal
// settlement carrying the commit error and the reported usage keyed by the
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
		testassert.NoError(t, err)
		store.txHook = func(string) error { return errors.New("injected storage failure") }

		{
			err := h.commitCompaction(c, testOpID, testCapture(), "summary", 1, nil, false)
			testassert.Require(t, err != nil, "commitCompaction succeeded past an injected failure")
		}

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		afterEntries, _ := storedSessionState(store, sessionID)
		testassert.Equal(t, len(afterEntries), len(beforeEntries))
		{
			got := registerRevision(t, store, sessionID, opKey)
			testassert.Equal(t, got, opRevision)
		}
		{
			got := registerRevision(t, store, sessionID, sessionKey)
			testassert.Equal(t, got, sessionRevision)
		}
		testassert.Equal(t, graph.Session.State.CompactionEntryID, "")
		state := operationStateOf(t, graph, testOpID)
		testassert.Require(t, state.Status == OperationRunning, "operation state = %+v, want still running", state)
		after, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(before, after), "projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
	})

	t.Run("a one-shot commit failure settles the reported usage", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
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
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "injected commit failure"), "error %v, want the injected commit failure", err)

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failed transaction")
		}
		testassert.Equal(t, graph.Session.State.CompactionEntryID, "")
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "injected commit failure"), "settlement entry = %+v, want the commit failure terminal", settlement)
		testassert.Require(t, settlement.Model != nil && *settlement.Model == testCompactCapture().Model, "settlement model = %+v, want the compact model identity", settlement.Model)
		testassert.Require(t, settlement.Usage != nil && *settlement.Usage == *usage && settlement.CompactionUsage, "settlement usage = %+v, want the reported total; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Usage)
		after, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(before, after), "projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
	})

	t.Run("the manual settlement shares the commit transaction", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
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
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "injected settlement failure"), "error %v, want the injected settlement failure", err)

		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry survived the failed shared transaction")
		}
		testassert.Equal(t, graph.Session.State.CompactionEntryID, "")
		// The commit's own failure settlement stands alone.
		settlement := settlementEntryOf(t, graph)
		testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "injected settlement failure"), "settlement entry = %+v, want the commit failure terminal", settlement)
		after, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(before, after), "projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
	})
}
