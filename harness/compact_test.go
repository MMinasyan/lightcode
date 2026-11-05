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
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

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

// compactionExecution returns the compaction fixtures' execution: its opener
// answers maintenance Engine requests with the given summarizer callback under
// the Operation's retry classifier, and answers a conversation request with
// the forbidden-model tripwire — compaction never wires a conversation
// invocation.
func compactionExecution(t *testing.T, compact func(context.Context, model.Request) (model.Stream, error), retry RetryPolicy) Execution {
	noop := func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	return Execution{
		Retry: retry,
		OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
			if request.Purpose == InvocationPurposeConversation {
				return invocationExecution(forbiddenConversationModel(t), noop), nil
			}
			return invocationExecution(compact, noop), nil
		},
	}
}

// maintenanceInvocationOf opens the given execution's maintenance invocation
// for the captured compact model — the opened-invocation shape the direct
// compact effect fixtures drive.
// summaryText returns the summary inside the included policy's checkpoint
// message wrapper, failing when the wrapper text is not the exact current one.
func summaryText(t *testing.T, entry *contextCheckpointEntry) string {
	t.Helper()
	testassert.Require(t, len(entry.Messages) == 1, "checkpoint carries %d messages, want the one summary message", len(entry.Messages))
	const prefix = "[Previous conversation summary]\n\n"
	const suffix = "\n\n[End of summary. Continue from here.]"
	text := entry.Messages[0].TextContent()
	testassert.Require(t, strings.HasPrefix(text, prefix) && strings.HasSuffix(text, suffix), "checkpoint message %q is not the summary wrapper", text)
	return strings.TrimSuffix(strings.TrimPrefix(text, prefix), suffix)
}

// summaryCheckpoint returns the included policy's checkpoint plan over the
// given summary text at the given covered boundary.
func summaryCheckpoint(t *testing.T, summary, boundary string) *CheckpointPlan {
	t.Helper()
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  testCompactCapture().Model,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + summary + "\n\n[End of summary. Continue from here.]"}},
	})
	testassert.NoError(t, err)
	return &CheckpointPlan{BoundaryEntryID: boundary, Items: authoredItems(msg)}
}

// authoredItems wraps explicitly strategy-authored context messages into
// checkpoint proposal items.
func authoredItems(messages ...model.Message) []CheckpointItem {
	items := make([]CheckpointItem, 0, len(messages))
	for i := range messages {
		owned := messages[i]
		items = append(items, CheckpointItem{Context: &owned})
	}
	return items
}

// retainItems wraps frozen-view source positions into checkpoint proposal
// items.
func retainItems(positions ...int) []CheckpointItem {
	items := make([]CheckpointItem, 0, len(positions))
	for _, position := range positions {
		pos := position
		items = append(items, CheckpointItem{Retain: &pos})
	}
	return items
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

// settlementEntryOfOperation returns the one settlement entry of the named
// Operation in the validated graph.
func settlementEntryOfOperation(t *testing.T, graph *sessionGraph, operationID string) *operationSettlementEntry {
	t.Helper()
	var found *operationSettlementEntry
	for i := range graph.Entries {
		if graph.Entries[i].Settlement != nil && graph.Entries[i].Envelope.OperationID == operationID {
			testassert.Require(t, found == nil, "operation %q carries more than one settlement entry", operationID)
			found = graph.Entries[i].Settlement
		}
	}
	testassert.Require(t, found != nil, "operation %q carries no settlement entry", operationID)
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

// TestRunCompactionReadyPathRunsOneRequest proves the complete manual
// procedure's ready shape through the real compact owner: the run executes
// exactly one request whose user text is the serialized snapshot alone, one
// intent transaction commits before it, the ready model settlement itself
// inserts no entry, and the checkpoint plus its success settlement are the only
// durable writes, charging the reported summary usage once; the ready
// settlement's reserved identity stays unused.
func TestRunCompactionReadyPathRunsOneRequest(t *testing.T) {
	store := idleFixture(t, validTestGraph(), hexID(20))
	opKey := RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: "compact-1"}
	beforeEntries, _ := storedSessionState(store, testSessionID)

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
	exec := compactionExecution(t, compact, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)

	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	testassert.Equal(t, len(requests), 1)
	req := requests[0]
	testassert.Require(t, len(req.Messages) == 2 && len(req.Tools) == 0, "got %v, want %v; got %v, want %v", len(req.Messages), 2, len(req.Tools), 0)
	testassert.Require(t, req.Messages[0].Role == model.RoleSystem && req.Messages[0].Source == (model.ModelRef{}), "system message = %+v, want role system with zero source", req.Messages[0])
	testassert.Equal(t, req.Messages[0].TextContent(), testCapture().Compact.SystemPrompt)
	testassert.Require(t, req.Messages[1].Role == model.RoleUser && req.Messages[1].Source == (model.ModelRef{}), "user message = %+v, want role user with zero source", req.Messages[1])
	expectedLines, err := serializeCompactionMessages(snapshot)
	testassert.NoError(t, err)
	testassert.Equal(t, req.Messages[1].TextContent(), strings.Join(expectedLines, "\n"))
	testassert.Require(t, !strings.Contains(req.Messages[1].TextContent(), "Previous summary:"), "the request carries a Previous summary block: %q", req.Messages[1].TextContent())

	// The compact checkpoint and its success settlement are the only new
	// entries: the ready model settlement itself inserted none.
	afterEntries, _ := storedSessionState(store, testSessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries)+2)
	graph, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, summaryText(t, comp) == "summary one" && comp.OperationID == "compact-1", "compaction entry = %+v, want the manual summary checkpoint", comp)
	ids := entryIDs(t, graph)
	testassert.Require(t, reservedID != "" && !ids[reservedID], "reserved identity %q must stay unused by any entry", reservedID)
	rec, err := h.ReadOperation(context.Background(), testSessionID, "compact-1")
	testassert.NoError(t, err)
	wantUsage := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}}}}
	testassert.Require(t, rec.State.Status == OperationSuccess && rec.State.ActiveEffect == nil, "operation state = %+v, want the settled manual terminal with the effect cleared", rec.State)
	testassert.Require(t, usageTotalsEqual(rec.State.Usage, wantUsage), "operation usage = %+v, want the published summary usage charged once", rec.State.Usage)
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, wantUsage), "session usage = %+v, want the published summary usage charged once", graph.Session.State.Usage)
}

// TestRunCompactionFullInputRunsOneRequest is the single-request regression:
// a serialized input larger than any per-piece budget the removed packing
// loop imposed runs as one full-input request whose user text is the
// serialized snapshot alone, with the one response's reported usage.
func TestRunCompactionFullInputRunsOneRequest(t *testing.T) {
	fixture := validTestGraph()
	unit := "summarize the whole conversation turn carefully "
	large := strings.Repeat(unit, 210)
	fixture.entries[0].input.Content = admissionContent(large)
	store := idleFixture(t, fixture, hexID(20))
	var requests []model.Request
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{InputTokens: 5, OutputTokens: 3}), nil
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)
	lines, err := serializeCompactionMessages(snapshot)
	testassert.NoError(t, err)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	testassert.Equal(t, len(requests), 1)
	testassert.Equal(t, requests[0].Messages[1].TextContent(), strings.Join(lines, "\n"))
}

// TestCompactModelEffectSettlements proves the effect contract through real
// manual runs: attempts run through the compact transport under the retry
// policy, a failed summary that reports usage settles the terminal with that
// reported usage keyed by the compact model, an interrupted effect settles
// through the existing terminal path with no attempt, and a textless completed
// output is carried to the folding stage, which decides acceptance after the
// settlement.
func TestCompactModelEffectSettlements(t *testing.T) {
	compactRef := testCompactCapture().Model
	run := func(t *testing.T, compact func(context.Context, model.Request) (model.Stream, error), retry RetryPolicy, execCtx context.Context) (*Harness, *graphStorage, *coordinator, string, error) {
		t.Helper()
		store := idleFixture(t, validTestGraph(), hexID(20))
		exec := compactionExecution(t, compact, retry)
		prepared := compactPrepared(testCapture(), exec)
		h := newCompactHarness(t, store, prepared)
		c := leaseCoordinator(t, h, testSessionID)
		if execCtx == nil {
			execCtx = context.Background()
		}
		return h, store, c, testSessionID, runManualCompactCtx(t, h, c, execCtx, testSessionID, "compact-1")
	}

	t.Run("attempts run through the compact transport", func(t *testing.T) {
		spy := &retrySpy{limit: 1}
		calls := 0
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			calls++
			if calls == 1 {
				return nil, &net.OpError{Op: "dial", Err: errors.New("boom")}
			}
			return summaryStream("summary one", model.Usage{}), nil
		}
		_, store, _, sessionID, err := run(t, compact, spy.retry, nil)
		testassert.NoError(t, err)
		testassert.Equal(t, calls, 2)
		testassert.Require(t, len(spy.attempts) == 1 && spy.attempts[0] == 1 && len(spy.seen) == 1, "retry consultations = %v seen %v, want the one failed attempt classified by the policy", spy.attempts, spy.seen)
		{
			_, ok := spy.seen[0].(*net.OpError)
			testassert.Require(t, ok, "classified failure %v, want the first attempt's transport error", spy.seen[0])
		}
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, summaryText(t, compactedEntryOf(t, graph)) == "summary one", "checkpoint = %+v, want the retried attempt's summary", compactedEntryOf(t, graph))
	})

	t.Run("failed summary settles the reported usage", func(t *testing.T) {
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			// The failing response reports usage before the read failure:
			// the errored assembly retains it.
			return failStream(errors.New("provider failure"),
				turnDelta("", "partial"), usageDelta(model.Usage{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5})), nil
		}
		h, store, _, sessionID, err := run(t, compact, (&retrySpy{limit: 0}).retry, nil)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "provider failure"), "run = %v, want the summary failure", err)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOfOperation(t, graph, "compact-1")
		testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "provider failure"), "settlement entry = %+v, want the failure terminal", settlement)
		testassert.Require(t, settlement.Facts[0].Model == compactRef, "settlement fact model = %+v, want the compact model identity", settlement.Facts[0].Model)
		testassert.Require(t, settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 5, CachedInputTokens: 5, OutputTokens: 5}) && settlement.CompactionUsage, "settlement fact usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Facts[0].Usage)
		testassert.Require(t, len(graph.Session.State.Usage.ByModel) == 1 && graph.Session.State.Usage.ByModel[0].Model == compactRef, "session usage = %+v, want the reported total keyed by the compact model", graph.Session.State.Usage)
		view, err := h.ReadSessionView(context.Background(), sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, view.History.LatestContextUsage() == nil, "latest context measurement = %+v, want the marked summary usage billed but ineligible", view.History.LatestContextUsage())
	})

	t.Run("interruption settles through the terminal path", func(t *testing.T) {
		_, store, _, sessionID, err := run(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
			t.Errorf("no attempt may start under a canceled execution context")
			return nil, errors.New("unreachable")
		}, nil, canceledContext())
		testassert.Require(t, err == errExecutionInterrupted, "run = %v, want the interruption token through the real owner", err)
		graph, gerr := validateFixture(t, store, sessionID)
		testassert.NoError(t, gerr)
		settlement := settlementEntryOfOperation(t, graph, "compact-1")
		testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail && settlement.CompactionUsage, "settlement entry = %+v, want the interruption terminal; settlement compaction_usage = false, want the compact effect's interruption provenance marked", settlement)
		var signals int
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on an interrupted path")
			if graph.Entries[i].Signal != nil {
				signals++
			}
		}
		testassert.Equal(t, signals, 1)
	})

	t.Run("refusal-only completed output is carried to the folding stage", func(t *testing.T) {
		refusal := turnDelta("stop", "")
		refusal.RefusalFragment = "no"
		compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
			return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
		}
		store := idleFixture(t, validTestGraph(), hexID(20))
		exec := compactionExecution(t, compact, nil)
		exec.Strategy = &StrategyBinding{ID: "test.accept/v1", Strategy: acceptAnyStrategy{}}
		capture := testCapture()
		capture.Strategy = StrategyCapture{ID: "test.accept/v1"}
		prepared := compactPrepared(capture, exec)
		h := newCompactHarness(t, store, prepared)
		c := leaseCoordinator(t, h, testSessionID)
		testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
		graph, err := validateFixture(t, store, testSessionID)
		testassert.NoError(t, err)
		settlement := settlementEntryOfOperation(t, graph, "compact-1")
		testassert.Require(t, settlement.Status == OperationSuccess && len(settlement.Facts) == 1 && settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 2, OutputTokens: 1}),
			"settlement = %+v, want the completed textless output accepted by the folding stage with its reported usage", settlement)
	})
}

// acceptAnyStrategy runs one maintenance model pass and folds every completed
// output — including a textless one — into none.
type acceptAnyStrategy struct{}

func (acceptAnyStrategy) Plan(PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}},
	}}, nil
}

func (acceptAnyStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanNone}, nil
}

// canceledContext returns a context that is already canceled.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestRunCompactionCanceledBeforeRunSettlesInterruption proves the pre-run
// window: an execution context already canceled before the compact request
// makes the run return interruption with no effect ever begun, and the
// orchestration settles the quiet running Operation as interruption with no
// usage and no compaction entry.
func TestRunCompactionCanceledBeforeRunSettlesInterruption(t *testing.T) {
	store := idleFixture(t, validTestGraph(), hexID(20))
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no request may run under a canceled execution context")
		return nil, errors.New("unreachable")
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	err := runManualCompactCtx(t, h, c, canceledContext(), testSessionID, "compact-1")
	testassert.Require(t, err == errExecutionInterrupted, "run = %v, want the interruption token through the real owner", err)
	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.NoError(t, gerr)
	settlement := settlementEntryOfOperation(t, graph, "compact-1")
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
	store := idleFixture(t, validTestGraph(), hexID(20))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	compact := func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests++
		return &cancelOnCloseStream{scriptStream: summaryStream("S1", model.Usage{InputTokens: 4, OutputTokens: 2}), cancel: cancel}, nil
	}
	exec := compactionExecution(t, compact, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	err := runManualCompactCtx(t, h, c, ctx, testSessionID, "compact-1")
	testassert.Require(t, err == errExecutionInterrupted, "run = %v, want the interruption token through the real owner", err)
	testassert.Equal(t, requests, 1)
	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.NoError(t, gerr)
	settlement := settlementEntryOfOperation(t, graph, "compact-1")
	testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail, "settlement entry = %+v, want the run's interruption terminal", settlement)
	testassert.Require(t, settlement.Facts[0].Model == testCompactCapture().Model, "settlement fact model = %+v, want the compact model identity", settlement.Facts[0].Model)
	testassert.Require(t, settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 4, OutputTokens: 2}) && settlement.CompactionUsage, "settlement fact usage = %+v, want the summary's reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Facts[0].Usage)
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
	fixture.session.State.CheckpointEntryID = hexID(3)
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
	store := idleFixture(t, fixture, hexID(20))
	var requests []model.Request
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)
	testassert.Equal(t, len(snapshot), 2)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	testassert.Equal(t, len(requests), 1)
	userText := requests[0].Messages[1].TextContent()
	testassert.Require(t, !strings.Contains(userText, "Previous summary:") && strings.Contains(userText, "Summary of the earlier conversation.") && strings.Contains(userText, "after the boundary") && !strings.Contains(userText, `"system"`), "the request carries a Previous summary block: %q; the serialized input lost the existing summary: %q; the serialized input lost the post-boundary message: %q; the system prompt entered the serialized conversation: %q", userText, userText, userText, userText)
}

// TestRunCompactionSummaryOnlySessionSuppliesSummary proves a summary-only
// Session still supplies that summary as the serialized input.
func TestRunCompactionSummaryOnlySessionSuppliesSummary(t *testing.T) {
	store := idleFixture(t, compactedFixtureGraph(), hexID(20))
	var requests []model.Request
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		requests = append(requests, req)
		return summaryStream("S1", model.Usage{}), nil
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	snapshot := projectedSnapshot(t, h, c)
	testassert.Equal(t, len(snapshot), 1)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	testassert.Equal(t, len(requests), 1)
	userText := requests[0].Messages[1].TextContent()
	testassert.Require(t, strings.Contains(userText, "Summary of the earlier conversation.") && !strings.Contains(userText, "Previous summary:"), "the summary-only input lost the summary: %q; the request carries a Previous summary block: %q", userText, userText)
}

// TestRunCompactionEmptySnapshotFailsNothingToCompact proves an empty
// snapshot settles the quiet running Operation with the retained detail and
// writes no compaction work.
func TestRunCompactionEmptySnapshotFailsNothingToCompact(t *testing.T) {
	store := emptyStore(t)
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		t.Errorf("no request may run for an empty snapshot")
		return nil, errors.New("unreachable")
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	c := leaseCoordinator(t, h, session.Identity.SessionID)
	err = runManualCompact(t, h, c, session.Identity.SessionID, "compact-1")
	testassert.Require(t, err != nil && err.Error() == "nothing to compact", "run = %v, want the retained nothing-to-compact failure", err)
	graph, gerr := validateFixture(t, store, session.Identity.SessionID)
	testassert.NoError(t, gerr)
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
	store := idleFixture(t, validTestGraph(), hexID(20))
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil // text "partial" with usage, then the read failure
	}, (&retrySpy{limit: 0}).retry)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	err := runManualCompact(t, h, c, testSessionID, "compact-1")
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "provider failure"), "error %v, want the summary's own failure detail, not a settlement-shape boundary violation", err)
	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.NoError(t, gerr)
	settlement := settlementEntryOfOperation(t, graph, "compact-1")
	testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "provider failure"), "settlement entry = %+v, want the failure terminal", settlement)
	testassert.Require(t, settlement.Facts[0].Model == testCompactCapture().Model, "settlement fact model = %+v, want the compact model identity", settlement.Facts[0].Model)
	testassert.Require(t, settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 5, OutputTokens: 7}) && settlement.CompactionUsage, "settlement fact usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Facts[0].Usage)
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
	store := idleFixture(t, validTestGraph(), hexID(20))
	calls := 0
	refusal := turnDelta("stop", "")
	refusal.RefusalFragment = "no"
	exec := compactionExecution(t, func(ctx context.Context, req model.Request) (model.Stream, error) {
		calls++
		return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	err := runManualCompact(t, h, c, testSessionID, "compact-1")
	testassert.Require(t, err != nil && err.Error() == "compaction summary is empty", "error %v, want the retained empty-summary detail", err)
	testassert.Equal(t, calls, 1)
	graph, gerr := validateFixture(t, store, testSessionID)
	testassert.NoError(t, gerr)
	settlement := settlementEntryOfOperation(t, graph, "compact-1")
	testassert.Require(t, settlement.Status == OperationFailure && settlement.Detail == "compaction summary is empty", "settlement entry = %+v, want the empty-summary failure terminal", settlement)
	testassert.Require(t, settlement.Facts[0].Model == testCompactCapture().Model, "settlement fact model = %+v, want the compact model identity", settlement.Facts[0].Model)
	testassert.Require(t, settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 2, OutputTokens: 1}) && settlement.CompactionUsage, "settlement fact usage = %+v, want the reported counts; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Facts[0].Usage)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failure path")
	}
}

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
func compactedEntryOf(t *testing.T, graph *sessionGraph) *contextCheckpointEntry {
	t.Helper()
	var found *contextCheckpointEntry
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

// summaryAfterModelStrategy runs one maintenance model pass and folds its
// completed output into the summary checkpoint at the frozen boundary.
type summaryAfterModelStrategy struct {
	summary  string
	boundary string
}

func (s summaryAfterModelStrategy) Plan(PlanInput) (PlanOutput, error) {
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("summary")}}},
	}}, nil
}

func (s summaryAfterModelStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  in.Output.Source,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + s.summary + "\n\n[End of summary. Continue from here.]"}},
	})
	if err != nil {
		return PlanOutput{}, err
	}
	boundary := in.Input.BoundaryEntryID
	if s.boundary != "" {
		boundary = s.boundary
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: boundary,
		Items:           authoredItems(msg),
	}}, nil
}

// nonComparableCause is a valid error value whose dynamic type is not
// comparable: the maintenance mappers must never compare it.
type nonComparableCause struct{ parts []string }

func (c nonComparableCause) Error() string { return strings.Join(c.parts, ": ") }

// nonComparableStrategy returns the non-comparable cause from Plan or Consume
// on demand, otherwise planning one model pass and ending at none.
type nonComparableStrategy struct {
	planCause    *nonComparableCause
	consumeCause *nonComparableCause
}

func (s nonComparableStrategy) Plan(PlanInput) (PlanOutput, error) {
	if s.planCause != nil {
		return PlanOutput{}, *s.planCause
	}
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{
		Request: model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("pass")}}},
	}}, nil
}

func (s nonComparableStrategy) Consume(ConsumeInput) (PlanOutput, error) {
	if s.consumeCause != nil {
		return PlanOutput{}, *s.consumeCause
	}
	return PlanOutput{Kind: PlanNone}, nil
}

// causeCapture is the maintenance capture selecting the cause strategy.
func causeCapture() ExecutionCapture {
	capture := triggerUsageCapture(4)
	capture.Strategy = StrategyCapture{ID: "test.cause/v1"}
	return capture
}

// TestMaintenanceNonComparableCauseSettlesWithoutPanic proves a valid
// non-comparable strategy Plan, Consume or maintenance-opener error settles
// the ordinary failure with its own detail through real automatic and manual
// owners, with the established cleanup/retirement and no arbitrary comparison.
func TestMaintenanceNonComparableCauseSettlesWithoutPanic(t *testing.T) {
	planCause := nonComparableCause{parts: []string{"plan stage", "broke"}}
	consumeCause := nonComparableCause{parts: []string{"consume stage", "broke"}}
	openCause := nonComparableCause{parts: []string{"maintenance opener", "broke"}}
	rows := []struct {
		name   string
		cause  nonComparableCause
		source string
	}{
		{"plan", planCause, "plan"},
		{"consume", consumeCause, "consume"},
		{"opener", openCause, "opener"},
	}
	for _, row := range rows {
		for _, manual := range []bool{false, true} {
			name := row.name + "/automatic"
			if manual {
				name = row.name + "/manual"
			}
			t.Run(name, func(t *testing.T) {
				capture := causeCapture()
				closes, executionCloses := 0, 0
				strategy := nonComparableStrategy{}
				switch row.source {
				case "plan":
					strategy.planCause = &row.cause
				case "consume":
					strategy.consumeCause = &row.cause
				}
				binding := &StrategyBinding{ID: "test.cause/v1", Strategy: strategy}
				compactFn := func(context.Context, model.Request) (model.Stream, error) {
					if row.source == "opener" {
						t.Fatal("the maintenance opener failure reached the compact model")
					}
					return summaryStream("summary", model.Usage{}), nil
				}
				exec := purposeExecution(quietConversation(), compactFn)
				exec.Strategy = binding
				exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
					if row.source == "opener" && request.Purpose == InvocationPurposeMaintenance {
						return InvocationExecution{}, row.cause
					}
					inv, err := purposeExecution(quietConversation(), compactFn).OpenInvocation(ctx, request)
					if err != nil {
						return inv, err
					}
					if request.Purpose == InvocationPurposeMaintenance {
						inner := inv.Close
						inv.Close = func() error {
							closes++
							if inner != nil {
								return inner()
							}
							return nil
						}
					}
					return inv, nil
				}
				exec.Close = func() error { executionCloses++; return nil }

				var err error
				var h *Harness
				var c *coordinator
				var sessionID string
				if manual {
					store := emptyStore(t)
					prepared := compactPrepared(capture, exec)
					h = newCompactHarness(t, store, prepared)
					session, serr := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
					testassert.NoError(t, serr)
					sessionID = session.Identity.SessionID
					c = leaseCoordinator(t, h, sessionID)
					err = runManualCompact(t, h, c, sessionID, "compact-1")
				} else {
					var store *graphStorage
					h, store, c, sessionID = seededTriggerHarness(t, capture, &UsageCount{InputTokens: 13})
					_ = store
					err = runAutomaticMaintenance(t, h, c, exec)
				}
				if err != nil {
					testassert.Require(t, strings.Contains(err.Error(), row.cause.Error()), "run = %v, want the original cause text %q", err, row.cause.Error())
				}
				rec, rerr := h.ReadOperation(context.Background(), sessionID, "compact-1")
				if !manual {
					rec, rerr = h.ReadOperation(context.Background(), sessionID, testOpID)
				}
				testassert.NoError(t, rerr)
				testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == row.cause.Error(),
					"operation = %+v, want the failure terminal carrying the original cause value", rec.State)
				testassert.Equal(t, executionCloses, 1)
				if row.source == "consume" {
					testassert.Equal(t, closes, 1)
				}
				c.mu.Lock()
				running, reserved := c.run != nil, c.reserved != nil
				c.mu.Unlock()
				testassert.Require(t, !running && !reserved, "coordinator still holds run=%v reserved=%v after retirement", running, reserved)
			})
		}
	}
}

// TestMaintenanceInterruptionPublicationFailureWins proves the nearest
// interruption sibling: an interruption proposal whose terminal publication
// fails returns the actual marked publication cause, never the interruption
// token or a retry, and the rollback retains the running state for recovery.
func TestMaintenanceInterruptionPublicationFailureWins(t *testing.T) {
	store := emptyStore(t)
	exec := compactionExecution(t, func(context.Context, model.Request) (model.Stream, error) {
		t.Fatal("a pre-plan interruption reached the compact model")
		return nil, nil
	}, nil)
	prepared := compactPrepared(causeCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	sessionID := session.Identity.SessionID
	c := leaseCoordinator(t, h, sessionID)
	injected := fmt.Errorf("injected interruption settlement failure: %w", ErrStorage)
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryOperationSettlement && draft.OperationID == "compact-1" {
			return injected
		}
		return nil
	}
	runErr := runManualCompactCtx(t, h, c, canceledContext(), sessionID, "compact-1")
	store.entryHook = nil
	testassert.Require(t, runErr != nil && runErr != errExecutionInterrupted, "run = %v, want the marked publication cause instead of the interruption token", runErr)
	testassert.Require(t, errors.Is(runErr, injected), "run = %v, want the original publication cause preserved", runErr)
	testassert.Require(t, isPublicationFailure(runErr), "run = %v, want the publication-origin classification", runErr)
	graph, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	state := operationStateOf(t, graph, "compact-1")
	testassert.Require(t, state.Status == OperationRunning && state.Terminal == nil, "operation state = %+v, want the running state retained for recovery", state)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil || graph.Entries[i].Envelope.OperationID != "compact-1", "the failed interruption publication fabricated a settlement")
		testassert.Require(t, graph.Entries[i].Compaction == nil, "the failed interruption publication committed a checkpoint")
	}
}

// TestCommitCompactionAutomaticCommitsAtomically proves the automatic shape at
// the real model barrier: the compaction entry, the Session register's
// projection field, and both register usage totals land together while the
// enclosing Operation still runs; validation agrees and the projection flips
// to the summary before the conversation request is sent.
func TestCommitCompactionAutomaticCommitsAtomically(t *testing.T) {
	strategy := summaryAfterModelStrategy{summary: "summary one"}
	capture := testCapture()
	capture.Strategy = StrategyCapture{ID: "test.auto/v1"}
	usage := model.Usage{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 4}
	arrived, release, conversation := parkedConversation(t)
	exec := purposeExecution(conversation, func(context.Context, model.Request) (model.Stream, error) {
		return summaryStream("summary one", usage), nil
	})
	exec.Strategy = &StrategyBinding{ID: "test.auto/v1", Strategy: strategy}
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), PreparedExecution{
		Capture: capture,
		Open: func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		},
	})
	beforeEntries, _ := storedSessionState(store, sessionID)
	done := make(chan error, 1)
	go func() { done <- runAutomaticMaintenance(t, h, c, exec) }()
	<-arrived

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.OperationID == testOpID && summaryText(t, comp) == "summary one", "compaction entry = %+v, want the Operation-owned committed summary", comp)
	testassert.Equal(t, comp.BoundaryEntryID, graph.Entries[0].Envelope.ID)
	testassert.Require(t, comp.Facts[0].Model == testCompactCapture().Model && comp.ConfigurationRevision == "rev-1", "compaction entry identity = %+v rev %q, want the compact model and the capture's revision", comp.Facts[0].Model, comp.ConfigurationRevision)
	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 4}}}}
	testassert.Require(t, comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == want.ByModel[0].Usage, "compaction entry fact usage = %+v, want the orchestration's reported total", comp.Facts[0].Usage)
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, comp.EntryID)
	testassert.Equal(t, graph.Session.State.CurrentOperationID, testOpID)
	state := operationStateOf(t, graph, testOpID)
	testassert.Require(t, state.Status == OperationRunning && !state.UnreportedMaintenance && state.ActiveEffect != nil && state.ActiveEffect.Kind == EffectModel,
		"operation state = %+v, want the running conversation model intent already dispatched against the consumed summary with the maintenance marker cleared", state)
	testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want %+v", state.Usage, want)
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want %+v", graph.Session.State.Usage, want)
	afterEntries, _ := storedSessionState(store, sessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries)+1) // the checkpoint alone
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil, "settlement entry committed inside the automatic checkpoint transaction")
	}

	close(release)
	testassert.NoError(t, <-done)
	graph, err = validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, summaryText(t, compactedEntryOf(t, graph)) == "summary one" && graph.Session.State.CheckpointEntryID == compactedEntryOf(t, graph).EntryID,
		"committed checkpoint = %+v, want the pre-dispatch summary still current", compactedEntryOf(t, graph))
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 3 && after[1].Role == model.RoleAssistant && after[1].Source == testCompactCapture().Model && after[1].TextContent() == "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" && after[2].TextContent() == "quiet",
		"projection = %+v, want the system message, the committed summary and the quiet conversation turn", after)
}

// TestCommitCompactionManualSettlesInTheSameTransaction proves the manual
// shape: the automatic members land, then the dedicated Operation settles as
// success in the same transaction — settlement entry, terminal Operation
// register, cleared current Operation — with the settlement itself carrying
// no usage and the unmarked source.
func TestCommitCompactionManualSettlesInTheSameTransaction(t *testing.T) {
	store := idleFixture(t, validTestGraph(), hexID(20))
	beforeEntries, _ := storedSessionState(store, testSessionID)
	usage := model.Usage{InputTokens: 6, CachedInputTokens: 1, OutputTokens: 2}
	exec := compactionExecution(t, func(context.Context, model.Request) (model.Stream, error) {
		return summaryStream("manual summary", usage), nil
	}, nil)
	prepared := compactPrepared(testCapture(), exec)
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))

	graph, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, summaryText(t, comp) == "manual summary" && comp.BoundaryEntryID == hexID(2), "compaction entry = %+v, want the committed summary at the projectable boundary", comp)
	settlement := settlementEntryOfOperation(t, graph, "compact-1")
	testassert.Require(t, settlement.Status == OperationSuccess && settlement.Detail == "" && (settlement.Model == nil && settlement.Usage == nil) && !settlement.CompactionUsage, "settlement entry = %+v, want the success terminal with no detail; settlement entry = %+v, want no usage on the settlement itself; settlement compaction_usage = true, want the manual settlement unmarked", settlement, settlement)
	state := operationStateOf(t, graph, "compact-1")
	testassert.Require(t, state.Status == OperationSuccess && state.Terminal != nil && state.Terminal.SettlementEntry.EntryID == settlement.EntryID, "operation state = %+v, want the success terminal naming the settlement entry", state)
	testassert.Equal(t, graph.Session.State.CurrentOperationID, "")
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, comp.EntryID)

	want := UsageTotals{ByModel: []ModelUsage{{Model: testCompactCapture().Model, Usage: UsageCount{InputTokens: 6, CachedInputTokens: 1, OutputTokens: 2}}}}
	testassert.Require(t, usageTotalsEqual(state.Usage, want), "operation usage = %+v, want %+v", state.Usage, want)
	testassert.Require(t, usageTotalsEqual(graph.Session.State.Usage, want), "session usage = %+v, want %+v", graph.Session.State.Usage, want)

	afterEntries, _ := storedSessionState(store, testSessionID)
	testassert.Equal(t, len(afterEntries), len(beforeEntries)+2) // the checkpoint and its settlement
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 2 && after[1].Role == model.RoleAssistant && after[1].TextContent() == "[Previous conversation summary]\n\nmanual summary\n\n[End of summary. Continue from here.]", "projection = %+v, want the system message and the committed summary", after)
}

// TestManualCompactAdoptsAdmissionAndReleasesAfterClose proves the manual
// owner executes the preparation the reserved admission actually returned: the
// maintenance invocation closes, then the opened Execution closes, then the
// admitted preparation's release runs exactly once — never an early caller
// release of a substituted copy.
func TestManualCompactAdoptsAdmissionAndReleasesAfterClose(t *testing.T) {
	store := idleFixture(t, validTestGraph(), hexID(20))
	var events []string
	exec := compactionExecution(t, func(context.Context, model.Request) (model.Stream, error) {
		return summaryStream("summary", model.Usage{}), nil
	}, nil)
	exec.Close = func() error {
		events = append(events, "execution-close")
		return nil
	}
	opened := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := opened(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose == InvocationPurposeMaintenance {
			inner := inv.Close
			inv.Close = func() error {
				events = append(events, "maintenance-close")
				if inner != nil {
					return inner()
				}
				return nil
			}
		}
		return inv, nil
	}
	prepared := compactPrepared(testCapture(), exec)
	releases := 0
	prepared.Release = func() error {
		releases++
		events = append(events, "prepare-release")
		return nil
	}
	h := newCompactHarness(t, store, prepared)
	c := leaseCoordinator(t, h, testSessionID)
	testassert.NoError(t, runManualCompact(t, h, c, testSessionID, "compact-1"))
	testassert.Require(t, reflect.DeepEqual(events, []string{"maintenance-close", "execution-close", "prepare-release"}),
		"disposal order = %v, want the admitted preparation's release after the execution close", events)
	testassert.Equal(t, releases, 1)
	rec, err := h.ReadOperation(context.Background(), testSessionID, "compact-1")
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationSuccess, "operation = %+v, want the adopted preparation's settled success", rec.State)
}

// TestCommitCompactionInjectedFailureLeavesPreviousStateCurrent proves the
// failure contract: an injected transaction failure commits nothing of the
// checkpoint — the previous projection and checkpoint stay current, the
// waiting conversation request is never sent uncompacted — and the
// publication failure fabricates no follow-up terminal write: the running
// state stays for recovery and no settlement entry appears.
func TestCommitCompactionInjectedFailureLeavesPreviousStateCurrent(t *testing.T) {
	setup := func(t *testing.T) (*Harness, *graphStorage, *coordinator) {
		t.Helper()
		store := idleFixture(t, validTestGraph(), hexID(20))
		exec := compactionExecution(t, func(context.Context, model.Request) (model.Stream, error) {
			return summaryStream("summary", model.Usage{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 4}), nil
		}, nil)
		prepared := compactPrepared(testCapture(), exec)
		h := newCompactHarness(t, store, prepared)
		return h, store, leaseCoordinator(t, h, testSessionID)
	}
	assertRetained := func(t *testing.T, h *Harness, store *graphStorage, c *coordinator, before []model.Message) {
		t.Helper()
		graph, err := validateFixture(t, store, testSessionID)
		testassert.NoError(t, err)
		for i := range graph.Entries {
			testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on a failed transaction")
			testassert.Require(t, graph.Entries[i].Settlement == nil || graph.Entries[i].Envelope.OperationID != "compact-1", "publication failure fabricated a follow-up terminal write")
		}
		testassert.Equal(t, graph.Session.State.CheckpointEntryID, "")
		state := operationStateOf(t, graph, "compact-1")
		testassert.Require(t, state.Status == OperationRunning, "operation state = %+v, want the running state retained for recovery", state)
		after, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(before, after), "projection changed through a failed commit:\nbefore %+v\nafter  %+v", before, after)
	}

	t.Run("a failing checkpoint transaction leaves every durable value unchanged", func(t *testing.T) {
		h, store, c := setup(t)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		beforeEntries, _ := storedSessionState(store, testSessionID)
		injected := fmt.Errorf("injected storage failure: %w", ErrStorage)
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryCompaction {
				return injected
			}
			return nil
		}
		err = runManualCompact(t, h, c, testSessionID, "compact-1")
		store.entryHook = nil
		testassert.Require(t, err != nil && errors.Is(err, injected), "run = %v, want the injected failure", err)
		testassert.Require(t, isPublicationFailure(err), "run = %v, want the publication-origin classification", err)
		assertRetained(t, h, store, c, before)
		afterEntries, _ := storedSessionState(store, testSessionID)
		testassert.Equal(t, len(afterEntries), len(beforeEntries))
	})

	t.Run("a failed checkpoint publication fabricates no follow-up terminal write", func(t *testing.T) {
		h, store, c := setup(t)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		injected := errors.New("injected checkpoint failure")
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryCompaction {
				store.entryHook = nil // one-shot
				return injected
			}
			return nil
		}
		err = runManualCompact(t, h, c, testSessionID, "compact-1")
		testassert.Require(t, err != nil && errors.Is(err, injected), "run = %v, want the injected commit failure", err)
		assertRetained(t, h, store, c, before)
	})

	t.Run("the manual settlement shares the checkpoint transaction", func(t *testing.T) {
		h, store, c := setup(t)
		before, err := h.projectContext(c, testOpID)
		testassert.NoError(t, err)
		injected := errors.New("injected settlement failure")
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryOperationSettlement && draft.OperationID == "compact-1" {
				return injected
			}
			return nil
		}
		err = runManualCompact(t, h, c, testSessionID, "compact-1")
		store.entryHook = nil
		testassert.Require(t, err != nil && errors.Is(err, injected), "run = %v, want the injected settlement failure", err)
		assertRetained(t, h, store, c, before)
	})
}

// fixtureInputEntryID returns the admitted input entry identity of the
// fixture Operation's cached graph — the canonical boundary an automatic
// maintenance procedure freezes its consumed view at.
func fixtureInputEntryID(c *coordinator) string {
	for _, entry := range c.graph.Entries {
		if entry.Input != nil {
			return entry.Envelope.ID
		}
	}
	return ""
}
