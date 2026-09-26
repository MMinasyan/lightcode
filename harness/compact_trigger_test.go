// Contract coverage for the conversation model effect's compaction trigger:
// an overflowing request compacts before it is sent and proceeds against the
// new projection, a fitting request is untouched, a failed checkpoint fails
// the Operation with the request unsent, a storage failure that leaves the
// Operation running propagates as the raw effect error to the storage-failure
// latch and Wait — over a live piece intent and over the quiet direct
// settlement — steering submitted during the orchestration stays buffered for
// the next model boundary, a later overflow in the same Operation compacts
// again, and compact piece requests never trigger compaction.
package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// projectedTriggerRequest builds the validated model request the Agent's
// context boundary supplies: the fresh projection of the fixture conversation.
func projectedTriggerRequest(t *testing.T, h *Harness, c *coordinator) model.Request {
	t.Helper()
	msgs, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	req, err := model.NewRequest(model.Request{
		Messages: msgs,
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return req
}

// triggerCapture returns the fixture capture with the conversation window set
// against the request's estimate-plus-reserve threshold: one below it makes
// the request overflow the trigger's strict inequality, exactly at it leaves
// the request fitting.
func triggerCapture(t *testing.T, req model.Request, overflow bool) ExecutionCapture {
	t.Helper()
	capture := testCapture()
	threshold := estimateTokens("", req.Messages) + capture.OutputReserve
	if overflow {
		capture.ContextWindow = threshold - 1
	} else {
		capture.ContextWindow = threshold
	}
	return capture
}

// overflowingTriggerCapture sizes the conversation window through the
// triggerCapture arithmetic over the deterministic projection of one admitted
// text input — the captured system message plus the one input message — so
// the projected request overflows the trigger's estimate-plus-reserve
// threshold by one.
func overflowingTriggerCapture(input string) ExecutionCapture {
	capture := testCapture()
	projected := []model.Message{
		{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: capture.SystemPrompt}}},
		{Role: model.RoleUser, Content: admissionContent(input)},
	}
	capture.ContextWindow = estimateTokens("", projected) + capture.OutputReserve - 1
	return capture
}

// invokeTriggerEffect drives one model effect with the given request through
// the real Agent assembly.
func invokeTriggerEffect(t *testing.T, me agent.ModelEffect, req model.Request) (agent.ModelSettlement, error) {
	t.Helper()
	return me(context.Background(), req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
		return agent.Assemble(context.Background(), source, stream)
	})
}

// TestModelEffectCompactTriggerFittingRequestUntouched proves the fitting row:
// a request whose estimate plus the output reserve does not exceed the
// conversation window is sent unchanged, the compact transport never runs, and
// no compaction work commits.
func TestModelEffectCompactTriggerFittingRequestUntouched(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, false)
	var sent []model.Request
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		CompactModel:  forbiddenConversationModel(t),
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if len(sent) != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly one", len(sent))
	}
	if !reflect.DeepEqual(req.Messages, sent[0].Messages) {
		t.Fatalf("sent messages changed through a fitting request:\nwant %+v\ngot  %+v", req.Messages, sent[0].Messages)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.Session.State.CompactionEntryID != "" {
		t.Fatalf("compaction_entry_id %q, want it empty on a fitting request", graph.Session.State.CompactionEntryID)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed for a fitting request")
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning {
		t.Fatalf("operation status %q, want running after the fitting request", rec.State.Status)
	}
}

// TestModelEffectCompactTriggerOverflowCompactsFirst proves the overflow row:
// the orchestration runs and the automatic commit lands before the request is
// sent, the sent request carries the projection's summary message with no
// post-boundary entries, and the piece usage is attributed to the compaction
// entry and both register totals keyed by the compact model.
func TestModelEffectCompactTriggerOverflowCompactsFirst(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, true)
	compactRef := testCompactCapture().Model
	pieceUsage := UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}
	var sent []model.Request
	compactCalls := 0
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The compaction committed before the request was sent.
			reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterSession})
			if err != nil {
				return nil, err
			}
			sess, err := decodeSessionRegister(reg)
			if err != nil {
				return nil, err
			}
			if sess.State.CompactionEntryID == "" {
				return nil, errors.New("the compaction had not committed when the request was sent")
			}
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready after the compaction", set.Disposition)
	}
	if compactCalls != 1 {
		t.Fatalf("compact transport calls = %d, want exactly one piece", compactCalls)
	}
	if len(sent) != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly one after the compaction", len(sent))
	}
	sentMsgs := sent[0].Messages
	if len(sentMsgs) != 2 {
		t.Fatalf("sent messages = %d, want the system message and the summary with no post-boundary entries:\n%+v", len(sentMsgs), sentMsgs)
	}
	if sentMsgs[0].Role != model.RoleSystem || sentMsgs[0].TextContent() != capture.SystemPrompt {
		t.Fatalf("sent system message = %+v, want the captured prompt", sentMsgs[0])
	}
	if sentMsgs[1].Role != model.RoleAssistant || sentMsgs[1].Source != compactRef {
		t.Fatalf("sent summary message = %+v, want an assistant message sourced by the compact model", sentMsgs[1])
	}
	if got := sentMsgs[1].TextContent(); got != "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" {
		t.Fatalf("sent summary text %q, want the verbatim framing around the committed summary", got)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.OperationID != testOpID || comp.Summary != "summary one" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned committed summary", comp)
	}
	if comp.Usage == nil || *comp.Usage != pieceUsage {
		t.Fatalf("compaction entry usage = %+v, want the piece's reported counts", comp.Usage)
	}
	if graph.Session.State.CompactionEntryID != comp.EntryID {
		t.Fatalf("compaction_entry_id %q, want the new entry %q", graph.Session.State.CompactionEntryID, comp.EntryID)
	}
	compactionUsage := UsageTotals{}
	for _, mu := range graph.Session.State.Usage.ByModel {
		if mu.Model == compactRef {
			compactionUsage.ByModel = append(compactionUsage.ByModel, mu)
		}
	}
	if len(compactionUsage.ByModel) != 1 || compactionUsage.ByModel[0].Usage != pieceUsage {
		t.Fatalf("session usage = %+v, want the piece's counts keyed by the compact model", graph.Session.State.Usage)
	}
	assistants := 0
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant entries = %d, want the one turn committed after the compaction", assistants)
	}
}

// TestModelEffectCompactTriggerCheckpointFailureUnsent proves the failure row:
// a piece failure inside its own effect settles the terminal failure with the
// accumulated usage keyed by the compact model, the effect returns the failure
// settlement with the request never sent, and the previous projection stays
// current with no compaction entry. The two-piece snapshot makes the
// accumulated total real: the first piece settles ready reporting usage, the
// second fails before any attempt.
func TestModelEffectCompactTriggerCheckpointFailureUnsent(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	before, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	// The request arrives fully built: the projected system message plus a
	// two-piece conversation snapshot, so the orchestration runs two pieces.
	messages := append([]model.Message{before[0]}, compactTwoPieceSnapshot(t)...)
	req, err := model.NewRequest(model.Request{
		Messages: messages,
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	capture := triggerCapture(t, req, true)
	compactRef := testCompactCapture().Model
	pieceCalls := 0
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			pieceCalls++
			if pieceCalls == 1 {
				// the first piece: reports usage and settles ready
				return summaryStream("S1", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
			}
			// the continuation piece: fails before any attempt
			return nil, errors.New("provider failure")
		},
		Retry:         (&retrySpy{limit: 0}).retry,
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect = %v, want the settled failure with a nil error", err)
	}
	if set.Disposition != agent.DispoFailure || set.Output != nil || !strings.Contains(set.Detail, "provider failure") {
		t.Fatalf("settlement = %+v, want the outputless failure carrying the piece detail", set)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	settlement := settlementEntryOf(t, graph)
	if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "provider failure") {
		t.Fatalf("settlement entry = %+v, want the failure terminal", settlement)
	}
	if settlement.Model == nil || *settlement.Model != compactRef {
		t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
	}
	if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 4, OutputTokens: 2}) {
		t.Fatalf("settlement usage = %+v, want the accumulated piece usage", settlement.Usage)
	}
	if graph.Session.State.CompactionEntryID != "" {
		t.Fatalf("compaction_entry_id %q, want it empty on the failed checkpoint", graph.Session.State.CompactionEntryID)
	}
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on a failed checkpoint")
		}
	}
	after, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("projection changed through the failed checkpoint:\nwant %+v\ngot  %+v", before, after)
	}
}

// TestModelEffectCompactTriggerSteeringStaysBuffered proves the steering row:
// steering submitted during the orchestration is not drained again and stays
// buffered — the rebuilt request carries only the compacted projection — and
// it drains at the next model boundary through the ordinary context source.
func TestModelEffectCompactTriggerSteeringStaysBuffered(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, true)
	var sent []model.Request
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			if _, err := h.Submit(context.Background(), SubmitRequest{
				SessionID:   sessionID,
				OperationID: "op-2",
				Origin:      InputOriginUser,
				Content:     admissionContent("s1"),
				Mode:        MessageModeRegular,
			}); err != nil {
				return nil, err
			}
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	// The waiting steering kept the Operation running across the ready
	// boundary; the rebuilt request itself carried none of it.
	if set.Disposition != agent.DispoContinue {
		t.Fatalf("settlement disposition %q, want the waiting steering to continue the Operation", set.Disposition)
	}
	if len(sent) != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly one", len(sent))
	}
	for _, msg := range sent[0].Messages {
		if strings.Contains(msg.TextContent(), "s1") {
			t.Fatalf("the rebuilt request carried the buffered steering: %+v", sent[0].Messages)
		}
	}
	c.mu.Lock()
	buffered := len(c.steering)
	c.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("steering buffered = %d, want the steering submitted during compaction to stay buffered", buffered)
	}
	// The next model boundary drains it through the ordinary context source.
	msgs, err := h.contextSource(c, testOpID)(context.Background())
	if err != nil {
		t.Fatalf("context source: %v", err)
	}
	drained := false
	for _, msg := range msgs {
		if msg.Role == model.RoleUser && msg.TextContent() == "s1" {
			drained = true
		}
	}
	if !drained {
		t.Fatalf("next-boundary projection = %+v, want the drained steering message", msgs)
	}
	if got := strings.Join(entryTexts(t, store, sessionID), ","); got != "hello,s1" {
		t.Fatalf("committed inputs = %q, want the steering committed as ordinary input at the boundary", got)
	}
}

// TestModelEffectCompactTriggerSecondOverflowSameOperation proves the
// no-count rule: a second overflowing request later in the same Operation
// compacts again, committing a second compaction entry owned by the same
// Operation, and the Operation continues against the newest projection.
func TestModelEffectCompactTriggerSecondOverflowSameOperation(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req1 := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req1, true)
	summaries := []string{"S1", "S2"}
	compactCalls := 0
	var sentTexts [][]string
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			texts := make([]string, 0, len(r.Messages))
			for _, msg := range r.Messages {
				texts = append(texts, msg.TextContent())
			}
			sentTexts = append(sentTexts, texts)
			return completedTurnStream(), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			compactCalls++
			if compactCalls <= len(summaries) {
				return summaryStream(summaries[compactCalls-1], model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
			return summaryStream("S9", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	me := h.modelEffect(c, testOpID, exec, capture)
	if set, err := invokeTriggerEffect(t, me, req1); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("first request: settlement %+v err %v, want ready", set, err)
	}
	req2 := projectedTriggerRequest(t, h, c)
	if estimateTokens("", req2.Messages)+capture.OutputReserve <= capture.ContextWindow {
		t.Fatalf("fixture precondition: the second request %d tokens fits the window", estimateTokens("", req2.Messages))
	}
	if set, err := invokeTriggerEffect(t, me, req2); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("second request: settlement %+v err %v, want ready", set, err)
	}
	if compactCalls != 2 {
		t.Fatalf("compact transport calls = %d, want one piece per compaction", compactCalls)
	}
	if len(sentTexts) != 2 {
		t.Fatalf("conversation transport calls = %d, want one per boundary", len(sentTexts))
	}
	// The second request was built against the first compaction's projection
	// and the second against the newest one.
	if len(sentTexts[0]) != 2 || !strings.Contains(sentTexts[0][1], "S1") {
		t.Fatalf("first sent request = %+v, want the system message and the first summary", sentTexts[0])
	}
	if len(sentTexts[1]) != 2 || !strings.Contains(sentTexts[1][1], "S2") {
		t.Fatalf("second sent request = %+v, want the system message and the second summary", sentTexts[1])
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	var comps []*compactionEntry
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			comps = append(comps, graph.Entries[i].Compaction)
		}
	}
	if len(comps) != 2 {
		t.Fatalf("compaction entries = %d, want one per overflow in the same Operation", len(comps))
	}
	for i, comp := range comps {
		if comp.OperationID != testOpID || comp.Summary != summaries[i] {
			t.Fatalf("compaction entry %d = %+v, want the Operation-owned %q summary", i, comp, summaries[i])
		}
	}
	if graph.Session.State.CompactionEntryID != comps[1].EntryID {
		t.Fatalf("compaction_entry_id %q, want the newest entry %q", graph.Session.State.CompactionEntryID, comps[1].EntryID)
	}
}

// TestModelEffectCompactRequestNeverTriggers proves the structural recursion
// guard: with the conversation window small enough to fire the trigger on any
// request and the compact window large, the compact piece requests run the
// orchestration's single piece and commit exactly one entry — a trigger inside
// the compact model effect would recurse into the orchestration without ever
// reaching a transport.
func TestModelEffectCompactRequestNeverTriggers(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	capture.ContextWindow = 1 // every conversation request overflows; the compact capture stays large
	compactCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			conversationCalls++ // the post-compaction request alone reaches the conversation transport
			return completedTurnStream(), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	req := projectedTriggerRequest(t, h, c)
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if compactCalls != 1 {
		t.Fatalf("compact transport calls = %d, want exactly one piece with no recursion", compactCalls)
	}
	if conversationCalls != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly the post-compaction request", conversationCalls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph) // exactly one compaction entry
	if comp.OperationID != testOpID || comp.Summary != "summary one" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned committed summary", comp)
	}
}

// TestModelEffectCompactTriggerStorageFailurePropagatesUnsettled proves the
// unsettled row: a storage failure at a piece's ready result commit — the
// piece intent stays live, the failure left the Operation running for
// recovery — returns from the trigger as the raw storage error with no
// settlement shape, and the Operation keeps its committed running/intent
// state with no entry beyond the admitted input.
func TestModelEffectCompactTriggerStorageFailurePropagatesUnsettled(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, true)
	injected := fmt.Errorf("piece result commit storage failure: %w", ErrStorage)
	replaces := 0
	store.txHook = func(step string) error {
		if step != "replace_register" {
			return nil
		}
		replaces++
		if replaces == 2 { // #1 the piece intent, #2/#3 the ready result commit
			return injected
		}
		return nil
	}
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
	if !errors.Is(err, injected) {
		t.Fatalf("model effect = %v, want the raw storage error returned unsettled", err)
	}
	if !reflect.DeepEqual(set, agent.ModelSettlement{}) {
		t.Fatalf("settlement = %+v, want no settlement shape behind the raw error", set)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect == nil {
		t.Fatalf("operation state = %+v, want the committed running/intent state for recovery", rec.State)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(graph.Entries) != 1 { // only the admitted input: the failed ready commit rolled back
		t.Fatalf("entries = %d, want only the admitted input after the rollback", len(graph.Entries))
	}
}

// TestCompactTriggerUnsettledStorageFailureReachesWait proves the full latch
// row over a live piece intent: a storage failure at the piece's ready result
// commit — the intent stays live, the Operation running — propagates as the
// raw effect error through the run and the outer settlement, is latched as
// the storage failure that stopped admitted work, and Wait reports it while
// the Operation keeps its committed running/intent state for recovery.
func TestCompactTriggerUnsettledStorageFailureReachesWait(t *testing.T) {
	store := emptyStore(t)
	capture := overflowingTriggerCapture("hello")
	injected := fmt.Errorf("piece result commit storage failure: %w", ErrStorage)
	fired := make(chan struct{}, 1)
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The intent's replace_register has already committed when the
			// transport runs; the next replace_register is the piece result
			// commit's operation-register write.
			store.txHook = func(step string) error {
				if step != "replace_register" {
					return nil
				}
				store.txHook = nil // one-shot
				fired <- struct{}{}
				return injected
			}
			return summaryStream("summary one", model.Usage{}), nil
		},
		Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
		NormalizeTool: objectNormalize,
	}
	prepared := PreparedExecution{
		Capture: capture,
		Open:    func(context.Context, OperationAdmission) (Execution, error) { return exec, nil },
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-fired // the piece's ready result commit failed at its operation-register write
	cancel()
	if err := h.Wait(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Wait = %v, want the injected storage error reported over the running Operation", err)
	}
	rec := settledOperation(t, store, session, "op-1")
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect == nil {
		t.Fatalf("operation state = %+v, want the running state with its live intent for recovery", rec.State)
	}
}

// TestCompactTriggerQuietSettlementStorageFailureReachesWait proves the quiet
// row: the textless piece settles ready and clears the effect, and the
// direct empty-summary settlement's storage failure — the Operation quiet and
// running — propagates as the raw effect error, takes the outer settlement's
// no-compensating-write branch, is latched, and Wait reports it with the
// Operation left running and quiet for recovery and no settlement entry
// committed.
func TestCompactTriggerQuietSettlementStorageFailureReachesWait(t *testing.T) {
	store := emptyStore(t)
	capture := overflowingTriggerCapture("hello")
	injected := fmt.Errorf("direct settlement storage failure: %w", ErrStorage)
	fired := make(chan struct{}, 1)
	refusal := turnDelta("stop", "")
	refusal.RefusalFragment = "no"
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The piece's ready settlement only replaces registers; the first
			// insert_entry of the trigger invocation is the direct
			// settlement's settlement entry.
			store.txHook = func(step string) error {
				if step != "insert_entry" {
					return nil
				}
				store.txHook = nil // one-shot
				fired <- struct{}{}
				return injected
			}
			return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
		},
		Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
		NormalizeTool: objectNormalize,
	}
	prepared := PreparedExecution{
		Capture: capture,
		Open:    func(context.Context, OperationAdmission) (Execution, error) { return exec, nil },
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-fired // the direct settlement failed at its settlement entry insert
	cancel()
	if err := h.Wait(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Wait = %v, want the injected storage error reported over the running quiet Operation", err)
	}
	rec := settledOperation(t, store, session, "op-1")
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil {
		t.Fatalf("operation state = %+v, want the running quiet state for recovery", rec.State)
	}
	graph, err := validateFixture(t, store, session)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(graph.Entries) != 1 { // only the admitted input: no settlement and no compaction entry committed
		t.Fatalf("entries = %d, want only the admitted input with no settlement entry", len(graph.Entries))
	}
}
