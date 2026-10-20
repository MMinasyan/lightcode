// Contract coverage for the conversation model effect's compaction trigger:
// the newest reported conversation usage after the current compaction boundary
// decides the checkpoint — an overflowing report compacts before the request
// is sent and the turn proceeds against the new projection, a report at or
// below the threshold leaves the request untouched, a missing report never
// compacts, a present zero obeys the same inequality, a failed checkpoint
// fails the Operation with the request unsent, a storage failure that leaves
// the Operation running propagates as the raw effect error to the
// storage-failure latch and Wait — over a live summary intent and over the
// quiet direct settlement — steering submitted during the orchestration stays
// buffered for the next model boundary, a later overflow in the same Operation
// compacts again, the committed boundary covers exactly the frozen snapshot so
// an entry appended between the freeze and the commit still projects, summary
// usage is billed without ever supplying the measurement, and compact summary
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

// triggerUsageCapture returns the fixture capture whose conversation
// threshold — ContextWindow minus OutputReserve, the value the trigger's
// strict inequality compares reported usage against — is exactly the given
// value. A negative threshold is a capture whose reserve exceeds its window.
func triggerUsageCapture(threshold int64) ExecutionCapture {
	capture := testCapture()
	capture.ContextWindow = int(threshold) + 1024
	capture.OutputReserve = 1024
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

// seedConversationUsage runs one ordinary conversation model effect whose
// completed output reports the given usage: the committed assistant entry is
// the provider measurement the trigger consumes. The seed effect runs over the
// given capture before any report the fixture tests exists.
func seedConversationUsage(t *testing.T, h *Harness, c *coordinator, capture ExecutionCapture, usage UsageCount) {
	t.Helper()
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return streamOf(turnDelta("stop", "seed turn"), usageDelta(model.Usage{
			InputTokens:       int(usage.InputTokens),
			CachedInputTokens: int(usage.CachedInputTokens),
			OutputTokens:      int(usage.OutputTokens),
		})), nil
	}
	exec := Execution{Model: modelFn, CompactModel: forbiddenConversationModel(t), NormalizeTool: objectNormalize}
	if set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), projectedTriggerRequest(t, h, c)); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("seed turn settlement = %+v, err %v, want ready", set, err)
	}
}

// TestModelEffectCompactTriggerReportedUsageCompacts is the provider-usage
// trigger regression: the request messages are too small for any text estimate
// to overflow the window, and a seeded reported conversation usage above the
// window minus the output reserve carries the trigger instead. Every summand
// is required for the verdict: the full total 2049 strictly overflows the
// fixture threshold 2048, while dropping any single summand lands at or below
// it — the cached input count participates exactly like the others.
func TestModelEffectCompactTriggerReportedUsageCompacts(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 49})
	compactCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{InputTokens: 10, OutputTokens: 5}), nil
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
		t.Fatalf("compact transport calls = %d, want the reported usage above the threshold to compact first", compactCalls)
	}
	if conversationCalls != 1 {
		t.Fatalf("conversation transport calls = %d, want exactly the post-compaction request", conversationCalls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.OperationID != testOpID || comp.Summary != "summary one" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned committed summary", comp)
	}
}

// TestModelEffectCompactTriggerNoReportNeverCompacts is the missing-report
// row: a window so small that every text estimate overflows it still never
// compacts while no reported usage exists.
func TestModelEffectCompactTriggerNoReportNeverCompacts(t *testing.T) {
	h, _, c, _ := newEffectHarness(t, nil)
	capture := testCapture()
	capture.ContextWindow = 1
	compactCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), projectedTriggerRequest(t, h, c))
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if compactCalls != 0 {
		t.Fatalf("compact transport calls = %d, want no compaction without a reported usage", compactCalls)
	}
	if conversationCalls != 1 {
		t.Fatalf("conversation transport calls = %d, want the fitting request sent unchanged", conversationCalls)
	}
}

// TestModelEffectCompactTriggerFittingRequestUntouched proves the fitting
// rows: a reported usage below the threshold and a reported usage exactly at
// the threshold are sent unchanged — the trigger is a strict inequality — the
// compact transport never runs, and no compaction work commits.
func TestModelEffectCompactTriggerFittingRequestUntouched(t *testing.T) {
	for _, row := range []struct {
		name  string
		usage UsageCount
	}{
		{"below threshold", UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 47}},
		{"equal threshold", UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 48}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			capture := testCapture() // threshold 4096 - 2048 = 2048
			seedConversationUsage(t, h, c, capture, row.usage)
			req := projectedTriggerRequest(t, h, c)
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
		})
	}
}

// TestModelEffectCompactTriggerPresentZeroWithReserveOverWindowCompacts
// proves the present-zero row: a reported all-zero usage obeys the same
// strict inequality, and when the output reserve exceeds the context window
// the threshold is negative, so the zero report overflows it.
func TestModelEffectCompactTriggerPresentZeroWithReserveOverWindowCompacts(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(-1) // window 1023, reserve 1024: threshold -1
	seedConversationUsage(t, h, c, capture, UsageCount{})
	compactCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), projectedTriggerRequest(t, h, c))
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if compactCalls != 1 || conversationCalls != 1 {
		t.Fatalf("compact/conversation transport calls = %d/%d, want 1/1 for the overflowing zero report", compactCalls, conversationCalls)
	}
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph: %v", err)
	}
}

// TestModelEffectCompactTriggerWideUsageSumCompacts proves big.Int exactness:
// counts whose total exceeds the widest int64 trigger through the exact
// comparison where int64 addition would wrap to a negative total.
func TestModelEffectCompactTriggerWideUsageSumCompacts(t *testing.T) {
	h, _, c, _ := newEffectHarness(t, nil)
	capture := testCapture()
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 6_000_000_000_000_000_000, CachedInputTokens: 6_000_000_000_000_000_000})
	compactCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, testOpID, exec, capture), projectedTriggerRequest(t, h, c))
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if compactCalls != 1 {
		t.Fatalf("compact transport calls = %d, want the exact wide sum above the threshold to compact", compactCalls)
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
// post-boundary entries, and the summary usage is attributed to the
// compaction entry and both register totals keyed by the compact model.
func TestModelEffectCompactTriggerOverflowCompactsFirst(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 3000, CachedInputTokens: 100, OutputTokens: 50})
	req := projectedTriggerRequest(t, h, c)
	compactRef := testCompactCapture().Model
	summaryUsage := UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}
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
	if parkedEntry.Summary != "summary one" || parkedEntry.Usage == nil || *parkedEntry.Usage != summaryUsage {
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
	if len(parkedSessionUsage.ByModel) != 1 || parkedSessionUsage.ByModel[0].Usage != summaryUsage {
		t.Fatalf("parked session usage = %+v, want the summary's counts", parked.session.State.Usage)
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
	if len(parkedOperationUsage.ByModel) != 1 || parkedOperationUsage.ByModel[0].Usage != summaryUsage {
		t.Fatalf("parked operation usage = %+v, want the summary's counts", parked.operation.State.Usage)
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
		t.Fatalf("compact transport calls = %d, want exactly one summary request", compactCalls)
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
	if comp.Usage == nil || *comp.Usage != summaryUsage {
		t.Fatalf("compaction entry usage = %+v, want the summary's reported counts", comp.Usage)
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
	if len(compactionUsage.ByModel) != 1 || compactionUsage.ByModel[0].Usage != summaryUsage {
		t.Fatalf("session usage = %+v, want the summary's counts keyed by the compact model", graph.Session.State.Usage)
	}
	assistants := 0
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistants++
		}
	}
	if assistants != 2 {
		t.Fatalf("assistant entries = %d, want the seed turn and the one turn committed after the compaction", assistants)
	}
}

// TestModelEffectCompactTriggerCheckpointFailureUnsent proves the failure
// row: a failed summary response that reports usage settles its own effect's
// terminal failure with that reported usage keyed by the compact model, the
// effect returns the failure settlement with the request never sent, and the
// previous projection stays current with its successful prior compaction
// unchanged. The prior checkpoint ran on a seeded overflowing report, and the
// failed checkpoint runs on the prior turn's present-zero report under a
// capture whose reserve exceeds its window.
func TestModelEffectCompactTriggerCheckpointFailureUnsent(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	priorCapture := testCapture()
	seedConversationUsage(t, h, c, priorCapture, UsageCount{InputTokens: 3000})
	priorRequest := projectedTriggerRequest(t, h, c)
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
	if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalSuccess}, nil, context.Background()); err != nil {
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
	const laterOperation = "op-2"
	const failingOperation = "op-3"
	// Two durable admitted inputs — the split-Operation shape of two
	// delivered messages.
	mustAdmitWithoutExecution(t, h, sessionID, laterOperation, admissionContent("later input"))
	if err := h.settleAgentTerminal(c, laterOperation, agent.TerminalResult{Status: agent.TerminalSuccess}, nil, context.Background()); err != nil {
		t.Fatal(err)
	}
	mustAdmitWithoutExecution(t, h, sessionID, failingOperation, admissionContent("failing input"))
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
	// Use the actual projection: the prior committed summary and turn plus
	// the two later inputs make the failed checkpoint's frozen snapshot.
	req, err := model.NewRequest(model.Request{
		Messages: before,
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	capture := triggerUsageCapture(-1) // the prior turn's zero report overflows a negative threshold
	compactRef := testCompactCapture().Model
	summaryCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return nil, errors.New("failed checkpoint sent its conversation request")
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			summaryCalls++
			if !strings.Contains(r.Messages[len(r.Messages)-1].TextContent(), "prior summary") {
				return nil, errors.New("later checkpoint omitted the committed prior summary")
			}
			// The one failed summary response: it reports usage before
			// the read failure.
			return failStream(errors.New("provider failure"),
				turnDelta("", "partial"), usageDelta(model.Usage{InputTokens: 4, OutputTokens: 2})), nil
		},
		Retry:         (&retrySpy{limit: 0}).retry,
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, failingOperation, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect = %v, want the settled failure with a nil error", err)
	}
	if set.Disposition != agent.DispoFailure || set.Output != nil || !strings.Contains(set.Detail, "provider failure") {
		t.Fatalf("settlement = %+v, want the outputless failure carrying the summary failure detail", set)
	}
	if summaryCalls != 1 || conversationCalls != 0 {
		t.Fatalf("later compact/conversation calls = %d/%d, want 1/0", summaryCalls, conversationCalls)
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
		t.Fatalf("settlement usage = %+v, want the failed summary's reported usage", settlement.Usage)
	}
	if !settlement.CompactionUsage {
		t.Fatalf("settlement compaction_usage = false, want the summary usage provenance marked")
	}
	// The failed checkpoint's reported usage reaches both totals keyed by
	// the compact model: the Operation register and the Session register.
	wantTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 4, OutputTokens: 2}}}}
	if state := operationStateOf(t, graph, failingOperation); state.Status != OperationFailure || !usageTotalsEqual(state.Usage, wantTotals) {
		t.Fatalf("operation usage = %+v, want the reported summary counts keyed by the compact model", state.Usage)
	}
	compactSessionTotals := UsageTotals{}
	for _, contribution := range graph.Session.State.Usage.ByModel {
		if contribution.Model == compactRef {
			compactSessionTotals.ByModel = append(compactSessionTotals.ByModel, contribution)
		}
	}
	wantSessionTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 11, OutputTokens: 5}}}}
	if !usageTotalsEqual(compactSessionTotals, wantSessionTotals) {
		t.Fatalf("session compact usage = %+v, want prior plus failed summary counts", compactSessionTotals)
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
// projection, no compact summary request exits at a boundary, and the
// completed turn settles ready so the steering is delivered through ordinary
// successor admission after the Operation's terminal.
func TestModelEffectCompactTriggerSteeringStaysBuffered(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil // the successor admission's own run completes
	})
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	req := projectedTriggerRequest(t, h, c)
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
	// Operation running inside the model effect, and no compact summary
	// request drains or exits at a boundary for the steering.
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
	// After the orchestration, the ordinary completion owner admits the head
	// under its own caller identity.
	if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalSuccess}, nil, context.Background()); err != nil {
		t.Fatalf("terminal settlement: %v", err)
	}
	release, err := h.reserve(h.ctx, c) // direct drain tests own the reservation around the body
	if err != nil {
		t.Fatalf("drain reservation: %v", err)
	}
	h.drainBuffers(c, &activeExecution{done: make(chan struct{}), execCtx: context.Background(), cancel: func() {}})
	release()
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess)
	awaitHarnessQuiet(t, h, sessionID)
	if got := strings.Join(entryTexts(t, store, sessionID), ","); got != "hello,s1" {
		t.Fatalf("committed inputs = %q, want the head admitted as its own input after orchestration", got)
	}
	if owner, _ := steeringInputOwner(t, store, sessionID, "s1"); owner != "op-2" {
		t.Fatalf("steering input owned by %q, want its own caller-ID Operation", owner)
	}
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph: %v", err)
	}
}

// TestModelEffectCompactTriggerSecondOverflowSameOperation proves the
// no-count rule: a second overflowing report later in the same Operation
// compacts again, committing a second compaction entry owned by the same
// Operation, and the Operation continues against the newest projection. The
// later turn's own modest report carries the second overflow; the seed's
// larger report stays behind the committed boundary.
func TestModelEffectCompactTriggerSecondOverflowSameOperation(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	req1 := projectedTriggerRequest(t, h, c)
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
			return completedTurnStream(), nil // reports {3,1,2}: above threshold 4
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
	if set, err := invokeTriggerEffect(t, me, req2); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("second request: settlement %+v err %v, want ready", set, err)
	}
	if compactCalls != 2 {
		t.Fatalf("compact transport calls = %d, want one summary request per compaction", compactCalls)
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

// TestModelEffectCompactTriggerSummaryUsageStaysTotalsOnly proves the
// measurement exclusion and the cutoff in one row: a committed summary's
// usage is billed on the entry and both totals but never supplies the
// trigger measurement, and the seed report that overflowed the first
// checkpoint stays behind the committed boundary. The turn's modest report
// fits the threshold, so the second checkpoint does not compact.
func TestModelEffectCompactTriggerSummaryUsageStaysTotalsOnly(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(12)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	summaryUsage := UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 1000}
	compactCalls := 0
	conversationCalls := 0
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil // reports {3,1,2}: below threshold 12
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 1000}), nil
		},
		NormalizeTool: objectNormalize,
	}
	me := h.modelEffect(c, testOpID, exec, capture)
	if set, err := invokeTriggerEffect(t, me, projectedTriggerRequest(t, h, c)); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("overflow checkpoint: settlement %+v err %v, want ready", set, err)
	}
	if set, err := invokeTriggerEffect(t, me, projectedTriggerRequest(t, h, c)); err != nil || set.Disposition != agent.DispoReady {
		t.Fatalf("fitting checkpoint: settlement %+v err %v, want ready", set, err)
	}
	if compactCalls != 1 {
		t.Fatalf("compact transport calls = %d, want the billed summary usage and the bounded seed report to leave the second checkpoint fitting", compactCalls)
	}
	if conversationCalls != 2 {
		t.Fatalf("conversation transport calls = %d, want one per checkpoint", conversationCalls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph) // exactly one compaction entry
	if comp.Usage == nil || *comp.Usage != summaryUsage {
		t.Fatalf("compaction entry usage = %+v, want the summary's reported counts", comp.Usage)
	}
	compactionUsage := UsageTotals{}
	for _, mu := range graph.Session.State.Usage.ByModel {
		if mu.Model == testCompactCapture().Model {
			compactionUsage.ByModel = append(compactionUsage.ByModel, mu)
		}
	}
	if len(compactionUsage.ByModel) != 1 || compactionUsage.ByModel[0].Usage != summaryUsage {
		t.Fatalf("session compact usage = %+v, want the summary's counts billed exactly once", graph.Session.State.Usage)
	}
}

// TestModelEffectCompactRequestNeverTriggers proves the structural recursion
// guard: with an overflowing report live, the orchestration's summary request
// runs one request and commits exactly one entry — a trigger inside the
// compact model effect would recurse into the orchestration without ever
// reaching a transport.
func TestModelEffectCompactRequestNeverTriggers(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
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
		t.Fatalf("compact transport calls = %d, want exactly one summary request with no recursion", compactCalls)
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
// unsettled row: a storage failure at the summary's ready result commit — the
// summary intent stays live, the failure left the Operation running for
// recovery — returns from the trigger as the raw storage error with no
// settlement shape, and the Operation keeps its committed running/intent
// state with no entry beyond the seed turn.
func TestModelEffectCompactTriggerStorageFailurePropagatesUnsettled(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	req := projectedTriggerRequest(t, h, c)
	injected := fmt.Errorf("summary result commit storage failure: %w", ErrStorage)
	replaces := 0
	store.txHook = func(step string) error {
		if step != "replace_register" {
			return nil
		}
		replaces++
		if replaces == 2 { // #1 the summary intent, #2/#3 the ready result commit
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
	if len(graph.Entries) != 2 { // the admitted input and the seed turn: the failed ready commit rolled back
		t.Fatalf("entries = %d, want the admitted input and the seed turn after the rollback", len(graph.Entries))
	}
}

// TestCompactTriggerUnsettledStorageFailureReachesWait proves the full latch
// row over a live summary intent: an ordinary turn first commits the reported
// usage the later checkpoint measures, a storage failure at that checkpoint's
// summary ready result commit — the intent stays live, the Operation running —
// propagates as the raw effect error through the run and the outer
// settlement, is latched as the storage failure that stopped admitted work,
// and Wait reports it while the checkpoint Operation keeps its committed
// running/intent state for recovery.
func TestCompactTriggerUnsettledStorageFailureReachesWait(t *testing.T) {
	store := emptyStore(t)
	capture := testCapture()
	injected := fmt.Errorf("summary result commit storage failure: %w", ErrStorage)
	fired := make(chan struct{}, 1)
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "seeded turn"), usageDelta(model.Usage{InputTokens: 3000})), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The intent's replace_register has already committed when the
			// transport runs; the next replace_register is the summary
			// result commit's operation-register write.
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
	awaitOperationTerminal(t, store, session, "op-1", OperationSuccess) // the ordinary turn commits the report
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "overflow"); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	<-fired // the checkpoint's summary result commit failed at its operation-register write
	cancel()
	if err := h.Wait(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Wait = %v, want the injected storage error reported over the running Operation", err)
	}
	rec := settledOperation(t, store, session, "op-2")
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect == nil {
		t.Fatalf("operation state = %+v, want the running state with its live intent for recovery", rec.State)
	}
}

// TestCompactTriggerQuietSettlementStorageFailureReachesWait proves the quiet
// row: the textless summary settles ready and clears the effect, and the
// direct empty-summary settlement's storage failure — the Operation quiet and
// running — propagates as the raw effect error, takes the outer settlement's
// no-compensating-write branch, is latched, and Wait reports it with the
// Operation left running and quiet for recovery and no settlement entry
// committed.
func TestCompactTriggerQuietSettlementStorageFailureReachesWait(t *testing.T) {
	store := emptyStore(t)
	capture := testCapture()
	injected := fmt.Errorf("direct settlement storage failure: %w", ErrStorage)
	fired := make(chan struct{}, 1)
	refusal := turnDelta("stop", "")
	refusal.RefusalFragment = "no"
	exec := Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "seeded turn"), usageDelta(model.Usage{InputTokens: 3000})), nil
		},
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The summary's ready settlement only replaces registers; the
			// first insert_entry of the checkpoint is the direct
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
	awaitOperationTerminal(t, store, session, "op-1", OperationSuccess) // the ordinary turn commits the report
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "overflow"); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	<-fired // the direct settlement failed at its settlement entry insert
	cancel()
	if err := h.Wait(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("Wait = %v, want the injected storage error reported over the running quiet Operation", err)
	}
	rec := settledOperation(t, store, session, "op-2")
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil {
		t.Fatalf("operation state = %+v, want the running quiet state for recovery", rec.State)
	}
	graph, err := validateFixture(t, store, session)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	settlements := 0
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			t.Fatalf("compaction entry committed on the failed direct settlement")
		}
		if graph.Entries[i].Settlement != nil {
			settlements++
			if graph.Entries[i].Envelope.OperationID != "op-1" {
				t.Fatalf("settlement entry for %q, want only op-1's ordinary terminal", graph.Entries[i].Envelope.OperationID)
			}
		}
	}
	if settlements != 1 { // only op-1's ordinary terminal: no checkpoint settlement committed
		t.Fatalf("settlement entries = %d, want only op-1's with none for the failed checkpoint", settlements)
	}
}

// TestModelEffectCompactBoundaryExcludesPostFreezeAppends proves the frozen
// boundary against the live interleave: a background completion delivered
// while the trigger's summary attempt is parked appends an entry after the
// frozen snapshot, and the committed boundary still names the freeze-time
// last projectable entry, so the appended signal projects after the summary
// instead of being named as covered and hidden.
func TestModelEffectCompactBoundaryExcludesPostFreezeAppends(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := triggerUsageCapture(4)
	seedConversationUsage(t, h, c, capture, UsageCount{InputTokens: 13})
	seeded, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded) != 2 || seeded[1].Kind != EntryAssistant {
		t.Fatalf("seed entries = %+v, want the admitted input and the seed assistant", seeded)
	}
	req := projectedTriggerRequest(t, h, c)
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
	if comp.BoundaryEntryID != seeded[1].ID {
		t.Fatalf("boundary %q, want the freeze-time last projectable entry %q", comp.BoundaryEntryID, seeded[1].ID)
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

// TestModelEffectCompactTriggerFullInputCommitAttributesUsage is the
// full-input commit observation the deleted rolling rows carried: the real
// orchestration succeeds on its single request over the complete rich input —
// the errored partial with its failure signal, the ordered rich tool turn,
// its complete tool result, the refusal turn, and the tail message — consumed
// exactly once in order, the run returns the one response's reported usage,
// the request carries the whole serialized snapshot under one framing, and the
// commit attributes that same total to the Operation-owned compaction entry
// and both registers in one transaction at the frozen boundary.
func TestModelEffectCompactTriggerFullInputCommitAttributesUsage(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	capture := testCapture()
	compactRef := testCompactCapture().Model
	var requests []model.Request
	exec := Execution{
		Model: forbiddenConversationModel(t),
		CompactModel: func(ctx context.Context, r model.Request) (model.Stream, error) {
			requests = append(requests, r)
			return summaryStream("full summary", model.Usage{InputTokens: 5, OutputTokens: 3}), nil
		},
		NormalizeTool: objectNormalize,
	}
	// The complete-input snapshot: the rich conversation prefix followed by
	// one further message.
	rich := completeCompactionInputMessages(t)
	tail := []model.Message{{Role: model.RoleUser, Content: admissionContent("the tail message")}}
	snapshot := append(append([]model.Message(nil), rich...), tail...)

	summary, usage, err := h.runCompaction(context.Background(), c, testOpID, exec, capture, snapshot)
	if err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if summary != "full summary" {
		t.Fatalf("summary = %q, want the request output", summary)
	}
	if usage == nil || *usage != (UsageCount{InputTokens: 5, OutputTokens: 3}) {
		t.Fatalf("run usage = %+v, want the one request's reported counts", usage)
	}
	if len(requests) != 1 {
		t.Fatalf("compact transport calls = %d, want one full-input request", len(requests))
	}
	// Consumed exactly once in order: the request's user text carries every
	// serialized snapshot line whole in snapshot order.
	wantLines := mustSerializeCompaction(t, snapshot)
	userText := requests[0].Messages[1].TextContent()
	if userText != strings.Join(wantLines, "\n") {
		t.Fatalf("request user text = %q, want every snapshot line whole in order", userText)
	}
	if strings.Contains(userText, "Previous summary:") || strings.Contains(userText, "Continuation:") {
		t.Fatalf("the request carries continuation framing: %q", userText)
	}
	// The rich lines on the wire: the errored partial, the wrapped failure
	// signal, the ordered content parts, the raw tool-call arguments, the
	// complete tool result and the refusal turn.
	for i, line := range strings.Split(userText, "\n")[:len(rich)] {
		var got compactPieceWireLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("request line %d unmarshal: %v", i, err)
		}
		if i == 3 { // the call's raw arguments ride the quoted representation
			if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call-rich" || got.ToolCalls[0].Name != "noop" {
				t.Fatalf("request line 3 tool calls = %+v, want exactly the one noop call", got.ToolCalls)
			}
			args, uerr := strconv.Unquote(got.ToolCalls[0].Arguments)
			if uerr != nil || args != `{"x":1}` {
				t.Fatalf("serialized tool-call arguments = %q (unquote error %v), want the submitted bytes", got.ToolCalls[0].Arguments, uerr)
			}
			got.ToolCalls[0].Arguments = ""
		}
		if !reflect.DeepEqual(got, wantRichPieceLines[i]) {
			t.Fatalf("request line %d = %+v, want %+v", i, got, wantRichPieceLines[i])
		}
	}
	// The commit attributes the same reported total to the Operation-owned
	// compaction entry and both registers in one transaction.
	if err := h.commitCompaction(c, testOpID, capture, summary, 1, usage, false); err != nil {
		t.Fatalf("commitCompaction: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	comp := compactedEntryOf(t, graph)
	if comp.OperationID != testOpID || comp.Summary != "full summary" {
		t.Fatalf("compaction entry = %+v, want the Operation-owned summary", comp)
	}
	if comp.BoundaryEntryID != graph.Entries[0].Envelope.ID {
		t.Fatalf("boundary %q, want the last projectable entry %q", comp.BoundaryEntryID, graph.Entries[0].Envelope.ID)
	}
	if comp.Usage == nil || *comp.Usage != (UsageCount{InputTokens: 5, OutputTokens: 3}) {
		t.Fatalf("compaction entry usage = %+v, want the reported counts {5,3}", comp.Usage)
	}
	if graph.Session.State.CompactionEntryID != comp.EntryID {
		t.Fatalf("compaction_entry_id %q, want the new entry %q", graph.Session.State.CompactionEntryID, comp.EntryID)
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 5, OutputTokens: 3}}}}
	if state := operationStateOf(t, graph, testOpID); !usageTotalsEqual(state.Usage, want) {
		t.Fatalf("operation usage = %+v, want the reported counts keyed by the compact model", state.Usage)
	}
	if !usageTotalsEqual(graph.Session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want the reported counts keyed by the compact model", graph.Session.State.Usage)
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

// completeCompactionInputMessages builds the complete-input row's rich
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
