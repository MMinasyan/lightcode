// Contract coverage for the conversation model effect's compaction trigger:
// an overflowing request compacts before it is sent and proceeds against the
// new projection, a fitting request is untouched, a failed checkpoint fails
// the Operation with the request unsent, a storage failure that leaves the
// Operation running propagates as the raw effect error to the storage-failure
// latch and Wait — over a live piece intent and over the quiet direct
// settlement — steering submitted during the orchestration stays buffered for
// the next model boundary, a later overflow in the same Operation compacts
// again, the committed boundary covers exactly the frozen snapshot so an entry
// appended between the freeze and the commit still projects, and compact piece
// requests never trigger compaction.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// triggerCompactCommitStore holds the successful compaction transaction
// return, after the test store committed and before coordinator adoption.
type triggerCompactCommitStore struct {
	Storage
	started chan struct{}
	release chan struct{}
}

type triggerCompactCommitTx struct {
	Transaction
	inserted bool
}

func (tx *triggerCompactCommitTx) InsertEntry(draft EntryDraft) (Entry, error) {
	if draft.Kind == EntryCompaction {
		tx.inserted = true
	}
	return tx.Transaction.InsertEntry(draft)
}

func (s *triggerCompactCommitStore) Transact(ctx context.Context, fn func(Transaction) error) error {
	var gate triggerCompactCommitTx
	err := s.Storage.Transact(ctx, func(tx Transaction) error {
		gate = triggerCompactCommitTx{Transaction: tx}
		return fn(&gate)
	})
	if err == nil && gate.inserted {
		s.started <- struct{}{}
		<-s.release
	}
	return err
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
	var conversationCalls atomic.Int64
	compactCalls := 0
	boundary := ""
	gated := &triggerCompactCommitStore{Storage: store, started: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Storage = gated
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			conversationCalls.Add(1)
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
			// Derived before the commit: the last durable entry at this
			// instant is the boundary the commit must name.
			committed, err := store.ReadEntries(ctx, sessionID, 0)
			if err != nil {
				return nil, err
			}
			if len(committed) == 0 {
				return nil, errors.New("no committed entries before the compaction commit")
			}
			boundary = committed[len(committed)-1].ID
			return summaryStream("summary one", model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), nil
		},
		NormalizeTool: objectNormalize,
	}
	type effectResult struct {
		set agent.ModelSettlement
		err error
	}
	done := make(chan effectResult, 1)
	defer func() {
		select {
		case <-gated.release:
		default:
			close(gated.release)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("automatic effect cleanup did not join after gate release")
		}
	}()
	go func() {
		set, err := h.modelEffect(c, testOpID, exec, capture)(context.Background(), req, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		})
		done <- effectResult{set, err}
		close(done)
	}()
	select {
	case <-gated.started:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic compaction never reached its transaction-return gate")
	}
	// The frozen instant: the compaction entry, the flipped projection, and
	// both usage totals were durable together while the enclosing Operation
	// was still running — a split implementation would show the entry without
	// the register changes.
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	sreg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterSession})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := decodeSessionRegister(sreg)
	if err != nil {
		t.Fatal(err)
	}
	oreg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
	if err != nil {
		t.Fatal(err)
	}
	op, err := decodeOperationRegister(oreg)
	if err != nil {
		t.Fatal(err)
	}
	parked := struct {
		entries   []Entry
		session   SessionRecord
		operation OperationRecord
	}{entries, sess, op}
	if len(parked.entries) == 0 || parked.entries[len(parked.entries)-1].Kind != EntryCompaction {
		t.Fatalf("parked entry tail = %+v, want the compaction entry committed last", parked.entries)
	}
	var parkedEntry struct {
		EntryID         string      `json:"entry_id"`
		BoundaryEntryID string      `json:"boundary_entry_id"`
		Summary         string      `json:"summary"`
		Usage           *UsageCount `json:"usage"`
	}
	if err := json.Unmarshal(parked.entries[len(parked.entries)-1].Payload, &parkedEntry); err != nil {
		t.Fatalf("decode parked compaction entry: %v", err)
	}
	if parkedEntry.BoundaryEntryID != boundary {
		t.Fatalf("parked boundary %q != the last pre-commit entry %q", parkedEntry.BoundaryEntryID, boundary)
	}
	if parkedEntry.Summary != "summary one" || parkedEntry.Usage == nil || *parkedEntry.Usage != pieceUsage {
		t.Fatalf("parked compaction summary/usage = %+v", parkedEntry)
	}
	if parked.session.State.CompactionEntryID != parkedEntry.EntryID {
		t.Fatalf("parked compaction_entry_id = %q, want the committed entry %q", parked.session.State.CompactionEntryID, parkedEntry.EntryID)
	}
	parkedSessionUsage := UsageTotals{}
	for _, mu := range parked.session.State.Usage.ByModel {
		if mu.Model == compactRef {
			parkedSessionUsage.ByModel = append(parkedSessionUsage.ByModel, mu)
		}
	}
	if len(parkedSessionUsage.ByModel) != 1 || parkedSessionUsage.ByModel[0].Usage != pieceUsage {
		t.Fatalf("parked session usage = %+v, want the piece's counts", parked.session.State.Usage)
	}
	if parked.operation.State.Status != OperationRunning || parked.operation.State.Terminal != nil {
		t.Fatalf("parked operation status = %q, want the enclosing Operation still running", parked.operation.State.Status)
	}
	parkedOperationUsage := UsageTotals{}
	for _, mu := range parked.operation.State.Usage.ByModel {
		if mu.Model == compactRef {
			parkedOperationUsage.ByModel = append(parkedOperationUsage.ByModel, mu)
		}
	}
	if len(parkedOperationUsage.ByModel) != 1 || parkedOperationUsage.ByModel[0].Usage != pieceUsage {
		t.Fatalf("parked operation usage = %+v, want the piece's counts", parked.operation.State.Usage)
	}
	select {
	case result := <-done:
		t.Fatalf("automatic effect returned while compaction transaction return held: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if conversationCalls.Load() != 0 {
		t.Fatal("conversation callback ran before compaction transaction return")
	}
	close(gated.release)
	select {
	case result := <-done:
		if result.err != nil || result.set.Disposition != agent.DispoReady {
			t.Fatalf("automatic effect after release = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("automatic effect did not finish after compaction transaction release")
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
// current with its successful prior compaction unchanged. The two-piece snapshot makes the
// accumulated total real: the first piece settles ready reporting usage, the
// second fails before any attempt.
func TestModelEffectCompactTriggerCheckpointFailureUnsent(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	priorRequest := projectedTriggerRequest(t, h, c)
	priorCapture := triggerCapture(t, priorRequest, true)
	priorUsage := UsageCount{InputTokens: 7, OutputTokens: 3}
	priorExec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			return summaryStream("prior answer", model.Usage{}), nil
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			return summaryStream("prior summary", model.Usage{InputTokens: 7, OutputTokens: 3}), nil
		},
		NormalizeTool: objectNormalize,
	}
	if result, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, priorExec, priorCapture), priorRequest); err != nil || result.Disposition != agent.DispoReady {
		t.Fatalf("prior automatic checkpoint = %+v, %v", result, err)
	}
	if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalSuccess}, nil); err != nil {
		t.Fatal(err)
	}
	priorGraph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	prior := *compactedEntryOf(t, priorGraph)
	if prior.EntryID == "" || prior.Summary != "prior summary" || prior.Usage == nil || *prior.Usage != priorUsage || operationStateOf(t, priorGraph, testOpID).Status != OperationSuccess {
		t.Fatalf("successful nonempty prior checkpoint missing: %+v", prior)
	}
	const firstPieceOp = "op-2"
	const failingOperation = "op-3"
	pieces := compactTwoPieceSnapshot(t)
	// Two durable admitted inputs — the split-Operation shape of two delivered
	// messages — make the later checkpoint need a continuation piece.
	mustAdmitWithoutExecution(t, h, sessionID, firstPieceOp, pieces[0].Content)
	if err := h.settleAgentTerminal(c, firstPieceOp, agent.TerminalResult{Status: agent.TerminalSuccess}, nil); err != nil {
		t.Fatal(err)
	}
	mustAdmitWithoutExecution(t, h, sessionID, failingOperation, pieces[1].Content)
	before, err := h.projectContext(c, failingOperation)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	preexisting, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range preexisting {
		preexisting[i].Payload = append(json.RawMessage(nil), preexisting[i].Payload...)
	}
	// Use the actual projection: the prior committed summary/answer plus two
	// durable inputs make the later checkpoint need a continuation piece.
	req, err := model.NewRequest(model.Request{
		Messages: before,
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	capture := triggerCapture(t, req, true)
	compactRef := testCompactCapture().Model
	pieceCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return nil, errors.New("failed checkpoint sent its conversation request")
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			pieceCalls++
			if pieceCalls == 1 {
				if !strings.Contains(r.Messages[len(r.Messages)-1].TextContent(), "prior summary") {
					return nil, errors.New("later checkpoint omitted the committed prior summary")
				}
				// the first piece: reports usage and settles ready
				return summaryStream("S1", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
			}
			// the continuation piece: fails before any attempt
			return nil, errors.New("provider failure")
		},
		Retry:         (&retrySpy{limit: 0}).retry,
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, failingOperation, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect = %v, want the settled failure with a nil error", err)
	}
	if set.Disposition != agent.DispoFailure || set.Output != nil || !strings.Contains(set.Detail, "provider failure") {
		t.Fatalf("settlement = %+v, want the outputless failure carrying the piece detail", set)
	}
	if pieceCalls != 2 || conversationCalls != 0 {
		t.Fatalf("later compact/conversation calls = %d/%d, want 2/0", pieceCalls, conversationCalls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	var settlement *operationSettlementEntry
	for _, entry := range graph.Entries {
		if entry.Settlement != nil && entry.Envelope.OperationID == failingOperation {
			settlement = entry.Settlement
		}
	}
	if settlement == nil {
		t.Fatal("later failed Operation settlement missing")
	}
	if settlement.Status != OperationFailure || !strings.Contains(settlement.Detail, "provider failure") {
		t.Fatalf("settlement entry = %+v, want the failure terminal", settlement)
	}
	if settlement.Model == nil || *settlement.Model != compactRef {
		t.Fatalf("settlement model = %+v, want the compact model identity", settlement.Model)
	}
	if settlement.Usage == nil || *settlement.Usage != (UsageCount{InputTokens: 4, OutputTokens: 2}) {
		t.Fatalf("settlement usage = %+v, want the accumulated piece usage", settlement.Usage)
	}
	// The failed checkpoint's accumulated usage reaches both totals keyed by
	// the compact model: the Operation register and the Session register.
	wantTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 4, OutputTokens: 2}}}}
	if state := operationStateOf(t, graph, failingOperation); state.Status != OperationFailure || !usageTotalsEqual(state.Usage, wantTotals) {
		t.Fatalf("operation usage = %+v, want the accumulated piece counts keyed by the compact model", state.Usage)
	}
	compactSessionTotals := UsageTotals{}
	for _, contribution := range graph.Session.State.Usage.ByModel {
		if contribution.Model == compactRef {
			compactSessionTotals.ByModel = append(compactSessionTotals.ByModel, contribution)
		}
	}
	wantSessionTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 11, OutputTokens: 5}}}}
	if !usageTotalsEqual(compactSessionTotals, wantSessionTotals) {
		t.Fatalf("session compact usage = %+v, want prior plus failed piece counts", compactSessionTotals)
	}
	if graph.Session.State.CompactionEntryID != prior.EntryID {
		t.Fatalf("compaction_entry_id %q, want prior %q on failed checkpoint", graph.Session.State.CompactionEntryID, prior.EntryID)
	}
	if current := compactedEntryOf(t, graph); current.EntryID != prior.EntryID || current.Summary != prior.Summary {
		t.Fatalf("prior compaction replaced: before=%+v after=%+v", prior, current)
	}
	afterEntries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEntries) < len(preexisting) || !reflect.DeepEqual(preexisting, afterEntries[:len(preexisting)]) {
		t.Fatal("pre-existing entry envelope/payload bytes changed through later failure")
	}
	tail, err := store.ReadEntries(context.Background(), sessionID, preexisting[len(preexisting)-1].Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Kind != EntryOperationSettlement || tail[0].OperationID != failingOperation {
		t.Fatalf("exact later-checkpoint tail = %+v, want only failed Operation settlement and no replacement compaction", tail)
	}
	after, err := h.projectContext(c, failingOperation)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("projection changed through the failed checkpoint:\nwant %+v\ngot  %+v", before, after)
	}
}

// TestModelEffectCompactTriggerSteeringStaysBuffered proves the steering row:
// steering submitted during the orchestration never enters the frozen snapshot
// and stays buffered — the rebuilt request carries only the compacted
// projection, no compact piece performs a steering handoff, and the completed
// turn settles ready so the steering is delivered through ordinary successor
// admission after the Operation's terminal.
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
	// The completed turn settles ready: waiting steering never keeps the
	// Operation running inside the model effect, and no compact piece drains
	// or hands off the steering.
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want the ordinary ready settlement", set.Disposition)
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
	if got := strings.Join(entryTexts(t, store, sessionID), ","); got != "hello" {
		t.Fatalf("committed inputs = %q, want the steering never committed in place", got)
	}
	// The steering stays pending under its own identity for the successor
	// admission after the compaction-owning Operation's terminal: no separate
	// Operation was ever admitted while the orchestration ran.
	if storedOperationExists(store, sessionID, "op-2") {
		t.Fatalf("the steering submitted during compaction admitted its own operation")
	}
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph: %v", err)
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

// TestModelEffectCompactBoundaryExcludesPostFreezeAppends proves the frozen
// boundary against the live interleave: a background completion delivered
// while the trigger's piece attempt is parked appends an entry after the
// frozen snapshot, and the committed boundary still names the freeze-time
// last projectable entry, so the appended signal projects after the summary
// instead of being named as covered and hidden.
func TestModelEffectCompactBoundaryExcludesPostFreezeAppends(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, true)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var sent []model.Request
	exec := Execution{
		Model: func(ctx context.Context, r model.Request) (model.Stream, error) {
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			arrived <- struct{}{}
			<-release // the park: the freeze is past, the commit has not run
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	type invocation struct {
		set agent.ModelSettlement
		err error
	}
	done := make(chan invocation, 1)
	go func() { // a synchronous call would deadlock on the park
		set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), req)
		done <- invocation{set: set, err: err}
	}()
	<-arrived
	// Admission precedes the close: a closed group rejects admission, and
	// the closed group makes the delivery commit the durable signal
	// regardless of the running Operation.
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	closeBackgroundMode(c)
	if err := h.DeliverBackgroundCompletion(context.Background(), sessionID, member.completionID, "Task completed."); err != nil {
		t.Fatalf("closed delivery: %v", err)
	}
	signals := storedSignals(t, store, sessionID)
	if len(signals) != 1 {
		t.Fatalf("durable background_completion signals = %d, want the one closed-path completion", len(signals))
	}
	if signals[0].OperationID != "" || signals[0].RelatedMember == nil || signals[0].RelatedMember.Kind != "child" ||
		signals[0].RelatedMember.ID != completionChildID || signals[0].Content != "Task completed." {
		t.Fatalf("committed signal = %+v, want the operationless child-member completion", signals[0])
	}
	close(release)
	out := <-done
	if out.err != nil {
		t.Fatalf("model effect: %v", out.err)
	}
	if out.set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready after the rebuilt request", out.set.Disposition)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.BoundaryEntryID != graph.Entries[0].Envelope.ID {
		t.Fatalf("boundary %q, want the freeze-time last projectable entry %q", comp.BoundaryEntryID, graph.Entries[0].Envelope.ID)
	}
	// The rebuilt request carried the appended signal after the summary.
	if len(sent) != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly the rebuilt request", len(sent))
	}
	sentMsgs := sent[0].Messages
	if len(sentMsgs) != 3 || sentMsgs[0].Role != model.RoleSystem ||
		sentMsgs[1].Role != model.RoleAssistant || sentMsgs[1].Source != testCompactCapture().Model ||
		sentMsgs[2].Role != model.RoleUser || sentMsgs[2].TextContent() != "<system-signal>Task completed.</system-signal>" {
		t.Fatalf("rebuilt request = %+v, want the system message, the summary, and the wrapped post-freeze signal", sentMsgs)
	}
	// The post-commit projection keeps the signal visible after the
	// summary, ahead of the turn the settled request committed.
	after, err := h.projectContext(c, testOpID)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	if len(after) != 4 || after[0].Role != model.RoleSystem ||
		after[1].Role != model.RoleAssistant || after[1].Source != testCompactCapture().Model ||
		after[1].TextContent() != "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" ||
		after[2].Role != model.RoleUser || after[2].TextContent() != "<system-signal>Task completed.</system-signal>" ||
		after[3].Role != model.RoleAssistant || after[3].TextContent() != "done" {
		t.Fatalf("post-commit projection = %+v, want the system message, the summary, the wrapped post-freeze signal, and the settled turn", after)
	}
}

// TestModelEffectCompactTriggerTwoPieceCommitAccumulatesUsage proves the
// rolling-commit chain the deleted Runtime row carried: the real two-piece
// orchestration succeeds on both pieces, the complete rich input — the errored
// partial with its failure signal, the ordered rich tool turn, its complete
// tool result and the refusal turn — is consumed exactly once in order over
// the estimator-sized complete pieces, the run returns the field-wise
// accumulated total, the second piece carries exactly the remaining message
// whole under the previous-summary framing, and the commit attributes that
// same accumulated total to the Operation-owned compaction entry and both
// registers in one transaction.
func TestModelEffectCompactTriggerTwoPieceCommitAccumulatesUsage(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	compactRef := testCompactCapture().Model
	var pieces []model.Request
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			pieces = append(pieces, r)
			if len(pieces) == 1 {
				return summaryStream("piece one", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
			}
			return summaryStream("piece two", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
		},
		NormalizeTool: objectNormalize,
	}
	// The complete-input snapshot: the rich conversation prefix followed by
	// the estimator-sized two-message tail, so the first piece carries the
	// whole rich history plus the first complete message and the second
	// piece carries only the remaining message.
	rich := completeCompactionInputMessages(t)
	tail := compactTwoPieceSnapshot(t)
	snapshot := append(append([]model.Message(nil), rich...), tail...)
	// Fixture preconditions against the real estimator: the prefix plus the
	// first tail message fit one fresh piece, and the second tail message
	// exceeds that piece's remaining budget while fitting a whole piece.
	budget := compactionPieceBudget(capture.Compact, "")
	prefixCost := 0
	for _, line := range mustSerializeCompaction(t, rich) {
		prefixCost += estimateTokens(line, nil)
	}
	costFirst := estimateTokens(mustSerializeCompaction(t, tail[:1])[0], nil)
	costSecond := estimateTokens(mustSerializeCompaction(t, tail[1:])[0], nil)
	if prefixCost+costFirst > budget {
		t.Fatalf("fixture precondition: the rich prefix plus the first message costs %d over the %d-token piece budget", prefixCost+costFirst, budget)
	}
	if costSecond <= budget-prefixCost-costFirst {
		t.Fatalf("fixture precondition: the second message would fit the first piece's remaining budget")
	}

	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, snapshot)
	if err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if summary != "piece two" {
		t.Fatalf("summary = %q, want the final piece output", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 5, OutputTokens: 3}) {
		t.Fatalf("run usage = %+v, want the field-wise piece sum {5,3}", usage)
	}
	if len(pieces) != 2 {
		t.Fatalf("compact transport calls = %d, want exactly two pieces", len(pieces))
	}
	// Consumed exactly once in order: the two pieces' user texts carry the
	// snapshot's serialized lines whole and in snapshot order.
	wantLines := mustSerializeCompaction(t, snapshot)
	firstText := pieces[0].Messages[1].TextContent()
	if firstText != strings.Join(wantLines[:len(wantLines)-1], "\n") {
		t.Fatalf("first piece user text = %q, want the whole rich history plus the first message in order", firstText)
	}
	// The second piece carries exactly the remaining message whole under the
	// continuation framing — the one-message-per-piece isolation.
	framing := "Previous summary:\npiece one\n\nContinuation:\n"
	if got := pieces[1].Messages[1].TextContent(); got != framing+wantLines[len(wantLines)-1] {
		t.Fatalf("second piece user text %q, want the whole remaining message under the continuation framing", got)
	}
	// The first piece's rich lines on the wire: the errored partial, the
	// wrapped failure signal, the ordered content parts, the raw tool-call
	// arguments, the complete tool result and the refusal turn.
	for i, line := range strings.Split(firstText, "\n")[:len(rich)] {
		var got compactPieceWireLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("first piece line %d unmarshal: %v", i, err)
		}
		if i == 3 { // the call's raw arguments ride the quoted representation
			if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call-rich" || got.ToolCalls[0].Name != "noop" {
				t.Fatalf("first piece line 3 tool calls = %+v, want exactly the one noop call", got.ToolCalls)
			}
			args, uerr := strconv.Unquote(got.ToolCalls[0].Arguments)
			if uerr != nil || args != `{"x":1}` {
				t.Fatalf("serialized tool-call arguments = %q (unquote error %v), want the submitted bytes", got.ToolCalls[0].Arguments, uerr)
			}
			got.ToolCalls[0].Arguments = ""
		}
		if !reflect.DeepEqual(got, wantRichPieceLines[i]) {
			t.Fatalf("first piece line %d = %+v, want %+v", i, got, wantRichPieceLines[i])
		}
	}
	// The rolling commit attributes the same accumulated total to the
	// Operation-owned compaction entry and both registers in one transaction.
	if err := h.commitCompaction(c, testOpID, capture, summary, 1, usage, false); err != nil {
		t.Fatalf("commitCompaction: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.OperationID != testOpID || comp.Summary != "piece two" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned final piece summary", comp)
	}
	if comp.Usage == nil || *comp.Usage != (UsageCount{InputTokens: 5, OutputTokens: 3}) {
		t.Fatalf("compaction entry usage = %+v, want the accumulated piece sum {5,3}", comp.Usage)
	}
	if graph.Session.State.CompactionEntryID != comp.EntryID {
		t.Fatalf("compaction_entry_id %q, want the new entry %q", graph.Session.State.CompactionEntryID, comp.EntryID)
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 5, OutputTokens: 3}}}}
	if state := operationStateOf(t, graph, testOpID); !usageTotalsEqual(state.Usage, want) {
		t.Fatalf("operation usage = %+v, want the accumulated piece counts keyed by the compact model", state.Usage)
	}
	if !usageTotalsEqual(graph.Session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want the accumulated piece counts keyed by the compact model", graph.Session.State.Usage)
	}
}

// compactPieceWireLine is one serialized compaction line's observed shape: the
// fields the complete-input assertion reads on the wire.
type compactPieceWireLine struct {
	Role       string
	Content    []compactPieceWirePart
	Refusal    string
	ToolCallID string
	ToolCalls  []struct {
		ID        string
		Name      string
		Arguments string
	}
}

type compactPieceWirePart struct {
	Kind           string
	Text           string
	URL            string
	OpaqueWireType string
}

// wantRichPieceLines is the expected wire shape of the rich prefix's six
// serialized lines, in conversation order.
var wantRichPieceLines = []compactPieceWireLine{
	{Role: "user", Content: []compactPieceWirePart{{Kind: "text", Text: "question one"}}},
	{Role: "assistant", Content: []compactPieceWirePart{{Kind: "text", Text: "partial answer"}}},
	{Role: "user", Content: []compactPieceWirePart{{Kind: "text", Text: "<system-signal>The previous model response failed after partial output. Continue from the retained response.</system-signal>"}}},
	{Role: "assistant", Content: []compactPieceWirePart{
		{Kind: "text", Text: "working"},
		{Kind: "opaque", OpaqueWireType: "thinking"},
		{Kind: "image_url", URL: "https://img.test/p.png"},
	}, ToolCalls: []struct {
		ID        string
		Name      string
		Arguments string
	}{{ID: "call-rich", Name: "noop"}}},
	{Role: "tool", ToolCallID: "call-rich", Content: []compactPieceWirePart{{Kind: "text", Text: "ok"}}},
	{Role: "assistant", Refusal: "cannot do that"},
}

// completeCompactionInputMessages builds the deleted complete-input row's rich
// conversation prefix through the accepting message constructor: the errored
// partial with its production-wrapped failure signal, the ordered rich tool
// turn, its complete tool result and the refusal turn.
func completeCompactionInputMessages(t *testing.T) []model.Message {
	t.Helper()
	build := func(m model.Message) model.Message {
		msg, err := model.NewMessage(m)
		if err != nil {
			t.Fatalf("fixture message: %v", err)
		}
		return msg
	}
	return []model.Message{
		build(model.Message{Role: model.RoleUser, Content: admissionContent("question one")}),
		build(model.Message{Role: model.RoleAssistant, Source: testModelRef(), Content: []model.ContentPart{{Kind: model.PartText, Text: "partial answer"}}}),
		build(model.Message{Role: model.RoleUser, Content: admissionContent(signalProjectedText(signalModelFailureContinuationContent))}),
		build(model.Message{
			Role:    model.RoleAssistant,
			Source:  testModelRef(),
			Content: []model.ContentPart{{Kind: model.PartText, Text: "working"}, {Kind: model.PartOpaque, OpaqueWireType: "thinking"}, {Kind: model.PartImageURL, URL: "https://img.test/p.png"}},
			ToolCalls: []model.ToolCall{{
				ID:        "call-rich",
				Name:      "noop",
				Arguments: json.RawMessage(`{"x":1}`),
			}},
		}),
		build(model.Message{Role: model.RoleTool, ToolCallID: "call-rich", Content: []model.ContentPart{{Kind: model.PartText, Text: "ok"}}}),
		build(model.Message{Role: model.RoleAssistant, Source: testModelRef(), Refusal: "cannot do that"}),
	}
}

// mustSerializeCompaction serializes compaction input lines, failing the test
// on an encoding error.
func mustSerializeCompaction(t *testing.T, messages []model.Message) []string {
	t.Helper()
	lines, err := serializeCompactionMessages(messages)
	if err != nil {
		t.Fatalf("serializeCompactionMessages: %v", err)
	}
	return lines
}
