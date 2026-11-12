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

	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
)

// projectedTriggerRequest builds the validated model request the Agent's
// context boundary supplies: the fresh projection of the fixture conversation.
func projectedTriggerRequest(t *testing.T, h *Harness, c *coordinator) model.Request {
	t.Helper()
	msgs, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	req, err := model.NewRequest(model.Request{
		Messages: msgs,
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	testassert.NoError(t, err)
	return req
}

// seededTriggerHarness admits one running Operation over the fixture with the
// given trigger capture; a non-nil assistant usage is the reported provider
// measurement committed before the run, with matching register totals.
func seededTriggerHarness(t *testing.T, capture ExecutionCapture, seed *UsageCount) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	fixture := validTestGraph()
	fixture.ops[0].Admission.Execution = capture
	if seed != nil {
		fixture.entries[1].assistant.Usage = seed
		totals := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: *seed}}}
		fixture.ops[0].State.Usage, fixture.session.State.Usage = totals, totals
	}
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)
	return h, store, leaseCoordinator(t, h, testSessionID), testSessionID
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

// TestModelEffectCompactTriggerReportedUsageCompacts is the provider-usage
// trigger regression: the request messages are too small for any text estimate
// to overflow the window, and a seeded reported conversation usage above the
// window minus the output reserve carries the trigger instead. Every summand
// is required for the verdict: the full total 2049 strictly overflows the
// fixture threshold 2048, while dropping any single summand lands at or below
// it — the cached input count participates exactly like the others.
func TestModelEffectCompactTriggerReportedUsageCompacts(t *testing.T) {
	capture := testCapture()
	seed := UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 49}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	compactCalls := 0
	conversationCalls := 0
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{InputTokens: 10, OutputTokens: 5}), nil
		},
	)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 1)
	testassert.Equal(t, conversationCalls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.OperationID == testOpID && summaryText(t, comp) == "summary one", "compaction entry = %+v, want the Operation-owned committed summary", comp)
}

// TestModelEffectCompactTriggerNoReportNeverCompacts is the missing-report
// row: a window so small that every text estimate overflows it still never
// compacts while no reported usage exists.
func TestModelEffectCompactTriggerNoReportNeverCompacts(t *testing.T) {
	capture := testCapture()
	capture.ContextWindow = 1
	h, store, c, sessionID := seededTriggerHarness(t, capture, nil)
	compactCalls := 0
	conversationCalls := 0
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 0)
	testassert.Equal(t, conversationCalls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed with no reported measurement")
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
			capture := testCapture() // threshold 4096 - 2048 = 2048
			seed := row.usage
			h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
			var sent []model.Request
			exec := purposeExecution(
				func(ctx context.Context, r model.Request) (model.Stream, error) {
					sent = append(sent, r)
					return completedTurnStream(), nil
				},
				forbiddenConversationModel(t),
			)
			expected, err := h.projectContext(c, testOpID)
			testassert.NoError(t, err)
			testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
			testassert.Equal(t, len(sent), 1)
			testassert.Require(t, reflect.DeepEqual(expected, sent[0].Messages), "sent messages changed through a fitting request:\nwant %+v\ngot  %+v", expected, sent[0].Messages)
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			testassert.Equal(t, graph.Session.State.CheckpointEntryID, "")
			for i := range graph.Entries {
				testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed for a fitting request")
			}
			testassert.Require(t, operationStateOf(t, graph, testOpID).Status == OperationSuccess, "operation = %+v, want the ordinary completed turn", operationStateOf(t, graph, testOpID))
		})
	}
}

// TestModelEffectCompactTriggerPresentZeroWithReserveOverWindowCompacts
// proves the present-zero row: a reported all-zero usage obeys the same
// strict inequality, and when the output reserve exceeds the context window
// the threshold is negative, so the zero report overflows it.
func TestModelEffectCompactTriggerPresentZeroWithReserveOverWindowCompacts(t *testing.T) {
	capture := triggerUsageCapture(-1) // window 1023, reserve 1024: threshold -1
	seed := UsageCount{}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	compactCalls := 0
	conversationCalls := 0
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return completedTurnStream(), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Require(t, compactCalls == 1 && conversationCalls == 1, "compact/conversation transport calls = %d/%d, want 1/1 for the overflowing zero report", compactCalls, conversationCalls)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, compactedEntryOf(t, graph) != nil, "the present-zero report committed no checkpoint")
}

// TestModelEffectCompactTriggerWideUsageSumCompacts proves big.Int exactness:
// counts whose total exceeds the widest int64 trigger through the exact
// comparison where int64 addition would wrap to a negative total.
func TestModelEffectCompactTriggerWideUsageSumCompacts(t *testing.T) {
	capture := testCapture()
	seed := UsageCount{InputTokens: 6_000_000_000_000_000_000, CachedInputTokens: 6_000_000_000_000_000_000}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	compactCalls := 0
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, compactedEntryOf(t, graph) != nil, "the wide usage sum committed no checkpoint")
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
// the automatic maintenance commit lands before the request is sent, the sent
// request carries the projection's summary message with no post-boundary
// entries, and the summary usage is attributed to the compaction entry and both
// register totals keyed by the compact model. The store gate parks the run
// after the successful compaction transaction returned and before coordinator
// adoption, so the durable split is observed at the real transaction boundary.
func TestModelEffectCompactTriggerOverflowCompactsFirst(t *testing.T) {
	capture := testCapture()
	seed := UsageCount{InputTokens: 3000, CachedInputTokens: 100, OutputTokens: 50}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	compactRef := testCompactCapture().Model
	summaryUsage := UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}
	var sent []model.Request
	var conversationCalls atomic.Int64
	compactCalls := 0
	boundary := ""
	gated := &triggerCompactCommitStore{Storage: store, started: make(chan struct{}, 1), release: make(chan struct{})}
	h.deps.Storage = gated
	exec := purposeExecution(
		func(ctx context.Context, r model.Request) (model.Stream, error) {
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
			if sess.State.CheckpointEntryID == "" {
				return nil, errors.New("the compaction had not committed when the request was sent")
			}
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
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
	)
	type runResult struct{ err error }
	done := make(chan runResult, 1)
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
		done <- runResult{err: runAutomaticMaintenance(t, h, c, exec)}
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
	testassert.NoError(t, err)
	sreg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterSession})
	testassert.NoError(t, err)
	sess, err := decodeSessionRegister(sreg)
	testassert.NoError(t, err)
	oreg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
	testassert.NoError(t, err)
	op, err := decodeOperationRegister(oreg)
	testassert.NoError(t, err)
	parked := struct {
		entries   []Entry
		session   SessionRecord
		operation OperationRecord
	}{entries, sess, op}
	testassert.Require(t, len(parked.entries) != 0 && parked.entries[len(parked.entries)-1].Kind == EntryCompaction, "parked entry tail = %+v, want the compaction entry committed last", parked.entries)
	var parkedEntry struct {
		EntryID         string `json:"entry_id"`
		BoundaryEntryID string `json:"boundary_entry_id"`
		Messages        []struct {
			Role    string `json:"role"`
			Content []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Facts []struct {
			Usage *UsageCount `json:"usage"`
		} `json:"facts"`
	}
	{
		err := json.Unmarshal(parked.entries[len(parked.entries)-1].Payload, &parkedEntry)
		testassert.NoError(t, err)
	}
	parkedSummary := ""
	if len(parkedEntry.Messages) > 0 && len(parkedEntry.Messages[0].Content) > 0 {
		parkedSummary = parkedEntry.Messages[0].Content[0].Text
	}
	var parkedUsage *UsageCount
	if len(parkedEntry.Facts) > 0 {
		parkedUsage = parkedEntry.Facts[0].Usage
	}
	testassert.Require(t, parkedEntry.BoundaryEntryID == boundary && (parkedSummary == "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" && parkedUsage != nil && *parkedUsage == summaryUsage), "got %v, want %v; parked compaction summary/usage = %+v", parkedEntry.BoundaryEntryID, boundary, parkedEntry)
	testassert.Equal(t, parked.session.State.CheckpointEntryID, parkedEntry.EntryID)
	parkedSessionUsage := UsageByModelForTest(parked.session.State.Usage, compactRef)
	testassert.Require(t, len(parkedSessionUsage.ByModel) == 1 && parkedSessionUsage.ByModel[0].Usage == summaryUsage, "parked session usage = %+v, want the summary's counts", parked.session.State.Usage)
	testassert.Require(t, parked.operation.State.Status == OperationRunning && parked.operation.State.Terminal == nil, "parked operation status = %q, want the enclosing Operation still running", parked.operation.State.Status)
	parkedOperationUsage := UsageByModelForTest(parked.operation.State.Usage, compactRef)
	testassert.Require(t, len(parkedOperationUsage.ByModel) == 1 && parkedOperationUsage.ByModel[0].Usage == summaryUsage, "parked operation usage = %+v, want the summary's counts", parked.operation.State.Usage)
	select {
	case result := <-done:
		t.Fatalf("automatic effect returned while compaction transaction return held: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	testassert.Require(t, conversationCalls.Load() == 0, "%v", "conversation callback ran before compaction transaction return")
	close(gated.release)
	select {
	case result := <-done:
		testassert.Require(t, result.err == nil, "automatic effect after release = %v", result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("automatic effect did not finish after compaction transaction release")
	}
	testassert.Equal(t, compactCalls, 1)
	testassert.Equal(t, len(sent), 1)
	sentMsgs := sent[0].Messages
	testassert.Require(t, len(sentMsgs) == 2, "sent messages = %d, want the system message and the summary with no post-boundary entries:\n%+v", len(sentMsgs), sentMsgs)
	testassert.Require(t, sentMsgs[0].Role == model.RoleSystem && sentMsgs[0].TextContent() == capture.SystemPrompt, "sent system message = %+v, want the captured prompt", sentMsgs[0])
	testassert.Require(t, sentMsgs[len(sentMsgs)-1].Role == model.RoleAssistant && sentMsgs[len(sentMsgs)-1].Source == compactRef, "sent summary message = %+v, want an assistant message sourced by the compact model", sentMsgs[len(sentMsgs)-1])
	{
		got := sentMsgs[len(sentMsgs)-1].TextContent()
		testassert.Equal(t, got, "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]")
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.OperationID == testOpID && summaryText(t, comp) == "summary one", "compaction entry = %+v, want the Operation-owned committed summary", comp)
	testassert.Require(t, comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == summaryUsage, "compaction entry fact usage = %+v, want the summary's reported counts", comp.Facts[0].Usage)
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, comp.EntryID)
	compactionUsage := UsageByModelForTest(graph.Session.State.Usage, compactRef)
	testassert.Require(t, len(compactionUsage.ByModel) == 1 && compactionUsage.ByModel[0].Usage == summaryUsage, "session usage = %+v, want the summary's counts keyed by the compact model", graph.Session.State.Usage)
	assistants := 0
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistants++
		}
	}
	testassert.Equal(t, assistants, 2) // the seed report and the completed turn
}

// TestModelEffectCompactTriggerCheckpointFailureUnsent proves the failure
// row through real runs: a prior manual checkpoint commits over the seed
// report, its zero-reporting conversation turn stays after the boundary, and a
// later admitted Operation whose zero report overflows a negative threshold
// fails its summary response — the failure terminal carries the failed
// summary's reported usage keyed by the compact model, the request is never
// sent, and the previous projection and checkpoint stay current with its
// successful prior compaction unchanged.
func TestModelEffectCompactTriggerCheckpointFailureUnsent(t *testing.T) {
	// The prior turn reports zero usage and is committed after the prior
	// checkpoint, so it is the current measurement while the earlier seed's
	// large report stays behind the committed boundary.
	fixture := validTestGraph()
	seed := UsageCount{InputTokens: 3000}
	fixture.entries[1].assistant.Usage = &seed
	seedTotals := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: seed}}}
	fixture.ops[0].State.Usage, fixture.session.State.Usage = seedTotals, seedTotals
	store := fixture.storage(t)
	priorUsage := UsageCount{InputTokens: 7, OutputTokens: 3}
	priorCapture := testCapture()
	priorExec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "prior answer"), usageDelta(model.Usage{})), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			return summaryStream("prior summary", model.Usage{InputTokens: 7, OutputTokens: 3}), nil
		},
	)
	priorPrepared := PreparedExecution{Capture: priorCapture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return priorExec, nil
	}}
	priorHarness := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return priorPrepared, nil
	})
	priorC := leaseCoordinator(t, priorHarness, testSessionID)
	testassert.NoError(t, executeDirect(priorHarness, priorC, context.Background(), testOpID, priorPrepared))

	priorGraph, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	prior := *compactedEntryOf(t, priorGraph)
	testassert.Require(t, prior.EntryID != "" && summaryText(t, &prior) == "prior summary" && prior.Facts[0].Usage != nil && *prior.Facts[0].Usage == priorUsage, "successful nonempty prior checkpoint missing: %+v", prior)

	// The later Operation is admitted under the negative-threshold capture: the
	// prior turn's present-zero report overflows it.
	capture := triggerUsageCapture(-1)
	compactRef := testCompactCapture().Model
	summaryCalls := 0
	conversationCalls := 0
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			return nil, errors.New("failed checkpoint sent its conversation request")
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			summaryCalls++
			if !strings.Contains(r.Messages[len(r.Messages)-1].TextContent(), "prior summary") {
				return nil, errors.New("later checkpoint omitted the committed prior summary")
			}
			// The one failed summary response: it reports usage before
			// the read failure.
			return failStream(errors.New("provider failure"),
				turnDelta("", "partial"), usageDelta(model.Usage{InputTokens: 4, OutputTokens: 2})), nil
		},
	)
	exec.Retry = (&retrySpy{limit: 0}).retry
	latePrepared := PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return latePrepared, nil
	})
	const failingOperation = "op-3"
	mustAdmitWithoutExecution(t, h, testSessionID, failingOperation, admissionContent("failing input"))
	c := leaseCoordinator(t, h, testSessionID)
	before, err := h.projectContext(c, failingOperation)
	testassert.NoError(t, err)
	preexisting, err := store.ReadEntries(context.Background(), testSessionID, 0)
	testassert.NoError(t, err)
	for i := range preexisting {
		preexisting[i].Payload = append(json.RawMessage(nil), preexisting[i].Payload...)
	}
	err = executeDirect(h, c, context.Background(), failingOperation, latePrepared)
	testassert.Require(t, err == nil || strings.Contains(err.Error(), "provider failure"), "round error = %v, want the settled summary failure's cause or its durable terminal", err)
	testassert.Require(t, summaryCalls == 1 && conversationCalls == 0, "later compact/conversation calls = %d/%d, want 1/0", summaryCalls, conversationCalls)
	graph, err := validateFixture(t, store, testSessionID)
	testassert.NoError(t, err)
	var settlement *operationSettlementEntry
	for _, entry := range graph.Entries {
		if entry.Settlement != nil && entry.Envelope.OperationID == failingOperation {
			settlement = entry.Settlement
		}
	}
	testassert.Require(t, settlement != nil && (settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "provider failure")), "%v; settlement entry = %+v, want the failure terminal", "later failed Operation settlement missing", settlement)
	testassert.Require(t, settlement.Facts[0].Model == compactRef, "settlement fact model = %+v, want the compact model identity", settlement.Facts[0].Model)
	testassert.Require(t, settlement.Facts[0].Usage != nil && *settlement.Facts[0].Usage == (UsageCount{InputTokens: 4, OutputTokens: 2}) && settlement.CompactionUsage, "settlement fact usage = %+v, want the failed summary's reported usage; settlement compaction_usage = false, want the summary usage provenance marked", settlement.Facts[0].Usage)
	wantTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 4, OutputTokens: 2}}}}
	{
		state := operationStateOf(t, graph, failingOperation)
		testassert.Require(t, state.Status == OperationFailure && usageTotalsEqual(state.Usage, wantTotals), "operation usage = %+v, want the reported summary counts keyed by the compact model", state.Usage)
	}
	compactSessionTotals := UsageByModelForTest(graph.Session.State.Usage, compactRef)
	wantSessionTotals := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 11, OutputTokens: 5}}}}
	testassert.Require(t, usageTotalsEqual(compactSessionTotals, wantSessionTotals), "session compact usage = %+v, want prior plus failed summary counts", compactSessionTotals)
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, prior.EntryID)
	{
		current := compactedEntryOf(t, graph)
		testassert.Require(t, current.EntryID == prior.EntryID && summaryText(t, current) == summaryText(t, &prior), "prior compaction replaced: before=%+v after=%+v", prior, current)
	}
	afterEntries, err := store.ReadEntries(context.Background(), testSessionID, 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(afterEntries) >= len(preexisting) && reflect.DeepEqual(preexisting, afterEntries[:len(preexisting)]), "%v", "pre-existing entry envelope/payload bytes changed through later failure")
	tail, err := store.ReadEntries(context.Background(), testSessionID, preexisting[len(preexisting)-1].Sequence)
	testassert.NoError(t, err)
	testassert.Require(t, len(tail) == 1 && tail[0].Kind == EntryOperationSettlement && tail[0].OperationID == failingOperation, "exact later-checkpoint tail = %+v, want only the failed Operation settlement and no replacement compaction", tail)
	after, err := h.projectContext(c, failingOperation)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(before, after), "projection changed through the failed checkpoint:\nwant %+v\ngot  %+v", before, after)
}

// TestModelEffectCompactTriggerSteeringStaysBuffered proves the steering row
// end to end: steering submitted during the orchestration never enters the
// frozen snapshot and stays buffered — the rebuilt request carries only the
// compacted projection, no compact summary request exits at a boundary, and
// the completed turn settles so the steering is delivered through ordinary
// successor admission after the Operation's terminal.
func TestModelEffectCompactTriggerSteeringStaysBuffered(t *testing.T) {
	capture := triggerUsageCapture(4)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	successor := preparedExecuting(effectExecution(func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}, nil))
	h.deps.Prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return successor, nil
	}
	var sent []model.Request
	exec := purposeExecution(
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			if _, err := h.Submit(context.Background(), SubmitRequest{
				SessionID:   sessionID,
				OperationID: "op-2",
				Origin:      InputOriginUser,
				Content:     admissionContent("s1"),
				Mode:        MessageModeRegular,
			}); err != nil {
				return nil, err
			}
			c.mu.Lock()
			buffered := len(c.steering)
			c.mu.Unlock()
			testassert.Equal(t, buffered, 1)
			testassert.Require(t, !storedOperationExists(store, sessionID, "op-2"), "the steering submitted during compaction admitted its own operation")
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	prepared := PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}}
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, prepared))
	// The completed turn settles: waiting steering never keeps the Operation
	// running inside the model effect, and no compact summary request drains
	// or exits at a boundary for the steering.
	testassert.Equal(t, len(sent), 1)
	for _, msg := range sent[0].Messages {
		testassert.Require(t, !strings.Contains(msg.TextContent(), "s1"), "the rebuilt request carried the buffered steering: %+v", sent[0].Messages)
	}
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess)
	awaitHarnessQuiet(t, h, sessionID)
	{
		got := strings.Join(entryTexts(t, store, sessionID), ",")
		testassert.Equal(t, got, "hello,s1")
	}
	{
		owner, _ := steeringInputOwner(t, store, sessionID, "s1")
		testassert.Equal(t, owner, "op-2")
	}
	{
		_, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
	}
}

// TestModelEffectCompactTriggerSecondOverflowSameOperation proves the
// no-count rule through one real conversation run with a tool round: a second
// overflowing report later in the same Operation compacts again, committing a
// second compaction entry owned by the same Operation, and the Operation
// continues against the newest projection. The later turn's own modest report
// carries the second overflow; the seed's larger report stays behind the
// committed boundary.
func TestModelEffectCompactTriggerSecondOverflowSameOperation(t *testing.T) {
	capture := triggerUsageCapture(4)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	summaries := []string{"S1", "S2"}
	compactCalls := 0
	var sentTexts [][]string
	rounds := 0
	tool := func(ctx context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	exec := purposeExecution(
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			texts := make([]string, 0, len(r.Messages))
			for _, msg := range r.Messages {
				texts = append(texts, msg.TextContent())
			}
			sentTexts = append(sentTexts, texts)
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil // reports {3,1,2}: above threshold 4
			}
			return completedTurnStream(), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			compactCalls++
			if compactCalls <= len(summaries) {
				return summaryStream(summaries[compactCalls-1], model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
			return summaryStream("S9", model.Usage{}), nil
		},
	)
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
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 2)
	testassert.Equal(t, len(sentTexts), 2)
	// The first request was built against the first compaction's projection
	// and the second against the newest one.
	testassert.Require(t, len(sentTexts[0]) == 2 && strings.Contains(sentTexts[0][1], "S1"), "first sent request = %+v, want the system message and the first summary", sentTexts[0])
	testassert.Require(t, len(sentTexts[1]) == 2 && strings.Contains(sentTexts[1][1], "S2"), "second sent request = %+v, want the system message and the second summary", sentTexts[1])
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var comps []*contextCheckpointEntry
	for i := range graph.Entries {
		if graph.Entries[i].Compaction != nil {
			comps = append(comps, graph.Entries[i].Compaction)
		}
	}
	testassert.Equal(t, len(comps), 2)
	for i, comp := range comps {
		testassert.Require(t, comp.OperationID == testOpID && summaryText(t, comp) == summaries[i], "compaction entry %d = %+v, want the Operation-owned %q summary", i, comp, summaries[i])
	}
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, comps[1].EntryID)
}

// TestModelEffectCompactTriggerSummaryUsageStaysTotalsOnly proves the
// measurement exclusion and the cutoff in one real tool-round run: a committed
// summary's usage is billed on the entry and both totals but never supplies the
// trigger measurement, and the seed report that overflowed the first checkpoint
// stays behind the committed boundary. The turn's modest report fits the
// threshold, so the second checkpoint does not compact.
func TestModelEffectCompactTriggerSummaryUsageStaysTotalsOnly(t *testing.T) {
	capture := triggerUsageCapture(12)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	summaryUsage := UsageCount{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 1000}
	compactCalls := 0
	conversationCalls := 0
	rounds := 0
	tool := func(ctx context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			conversationCalls++
			rounds++
			if rounds == 1 {
				return completedTurnStream(testToolCall("call-1")), nil // reports {3,1,2}: below threshold 12
			}
			return completedTurnStream(), nil
		},
		func(context.Context, model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{InputTokens: 1000, CachedInputTokens: 1000, OutputTokens: 1000}), nil
		},
	)
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
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 1)
	testassert.Equal(t, conversationCalls, 2)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph) // exactly one compaction entry
	testassert.Require(t, comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == summaryUsage, "compaction entry fact usage = %+v, want the summary's reported counts", comp.Facts[0].Usage)
	compactionUsage := UsageByModelForTest(graph.Session.State.Usage, testCompactCapture().Model)
	testassert.Require(t, len(compactionUsage.ByModel) == 1 && compactionUsage.ByModel[0].Usage == summaryUsage, "session compact usage = %+v, want the summary's counts billed exactly once", graph.Session.State.Usage)
}

// TestModelEffectCompactRequestNeverTriggers proves the structural recursion
// guard: with an overflowing report live, the orchestration's summary request
// runs one request and commits exactly one entry — a trigger inside the
// compact model effect would recurse into the orchestration without ever
// reaching a transport.
func TestModelEffectCompactRequestNeverTriggers(t *testing.T) {
	capture := triggerUsageCapture(4)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	compactCalls := 0
	conversationCalls := 0
	exec := purposeExecution(
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			conversationCalls++ // the post-compaction request alone reaches the conversation transport
			return completedTurnStream(), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			compactCalls++
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	testassert.NoError(t, runAutomaticMaintenance(t, h, c, exec))
	testassert.Equal(t, compactCalls, 1)
	testassert.Equal(t, conversationCalls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph) // exactly one compaction entry
	testassert.Require(t, comp.OperationID == testOpID && summaryText(t, comp) == "summary one", "compaction entry = %+v, want the Operation-owned committed summary", comp)
}

// TestModelEffectCompactTriggerStorageFailurePropagatesUnsettled proves the
// unsettled row: a storage failure at the summary's ready result commit — named
// by the live maintenance intent's own request identity — leaves the intent
// live and the Operation running for recovery, returns the raw storage error
// with no settlement entry, and keeps the committed running/intent state with
// no entry beyond the seed report.
func TestModelEffectCompactTriggerStorageFailurePropagatesUnsettled(t *testing.T) {
	capture := triggerUsageCapture(4)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	injected := fmt.Errorf("summary result commit storage failure: %w", ErrStorage)
	exec := purposeExecution(
		forbiddenConversationModel(t),
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
			if err != nil {
				return nil, err
			}
			op, err := decodeOperationRegister(reg)
			if err != nil {
				return nil, err
			}
			if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectModel || op.State.ActiveEffect.Purpose != InvocationPurposeMaintenance {
				return nil, errors.New("no maintenance model intent is durable while the summary request runs")
			}
			// The next Operation-register write after this named callback is
			// the ready result commit that clears the maintenance intent.
			store.replaceHook = func(key RegisterKey, payload json.RawMessage) error {
				if key.Kind != RegisterOperation {
					return nil
				}
				next, derr := decodeOperationRegister(Register{Key: key, Payload: payload})
				if derr == nil && next.State.ActiveEffect == nil {
					store.replaceHook = nil // one-shot
					return injected
				}
				return nil
			}
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	err := runAutomaticMaintenance(t, h, c, exec)
	testassert.Require(t, errors.Is(err, injected), "model effect = %v, want the raw storage error returned unsettled", err)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil, "operation state = %+v, want the committed running/intent state for recovery", rec.State)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	// the seed input and report: the failed ready commit rolled back
	testassert.Equal(t, len(graph.Entries), 2)
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Settlement == nil, "the failed ready commit fabricated a settlement")
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
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "seeded turn"), usageDelta(model.Usage{InputTokens: 3000})), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The intent's operation-register write has already committed when
			// the transport runs; the next Operation-register write is the
			// summary result commit that clears the maintenance intent.
			store.replaceHook = func(key RegisterKey, payload json.RawMessage) error {
				if key.Kind != RegisterOperation {
					return nil
				}
				next, derr := decodeOperationRegister(Register{Key: key, Payload: payload})
				if derr == nil && next.State.ActiveEffect == nil {
					store.replaceHook = nil // one-shot
					fired <- struct{}{}
					return injected
				}
				return nil
			}
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	prepared := PreparedExecution{
		Capture: capture,
		Open:    func(context.Context, OperationAdmission) (Execution, error) { return exec, nil },
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	awaitOperationTerminal(t, store, session, "op-1", OperationSuccess) // the ordinary turn commits the report
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "overflow")
		testassert.NoError(t, err)
	}
	<-fired // the checkpoint's summary result commit failed at its operation-register write
	cancel()
	{
		err := h.Wait(context.Background())
		testassert.Require(t, errors.Is(err, injected), "Wait = %v, want the injected storage error reported over the running Operation", err)
	}
	rec := settledOperation(t, store, session, "op-2")
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil, "operation state = %+v, want the running state with its live intent for recovery", rec.State)
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
	exec := purposeExecution(
		func(context.Context, model.Request) (model.Stream, error) {
			return streamOf(turnDelta("stop", "seeded turn"), usageDelta(model.Usage{InputTokens: 3000})), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			// The summary's ready settlement only replaces registers; the
			// direct empty-summary settlement's own entry insert is the named
			// failing write.
			store.entryHook = func(draft EntryDraft) error {
				if draft.Kind == EntryOperationSettlement && draft.OperationID == "op-2" {
					store.entryHook = nil // one-shot
					fired <- struct{}{}
					return injected
				}
				return nil
			}
			return streamOf(refusal, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1})), nil
		},
	)
	prepared := PreparedExecution{
		Capture: capture,
		Open:    func(context.Context, OperationAdmission) (Execution, error) { return exec, nil },
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	awaitOperationTerminal(t, store, session, "op-1", OperationSuccess) // the ordinary turn commits the report
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "overflow")
		testassert.NoError(t, err)
	}
	<-fired // the direct settlement failed at its settlement entry insert
	cancel()
	{
		err := h.Wait(context.Background())
		testassert.Require(t, errors.Is(err, injected), "Wait = %v, want the injected storage error reported over the running quiet Operation", err)
	}
	rec := settledOperation(t, store, session, "op-2")
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil, "operation state = %+v, want the running quiet state for recovery", rec.State)
	graph, err := validateFixture(t, store, session)
	testassert.NoError(t, err)
	settlements := 0
	for i := range graph.Entries {
		testassert.Require(t, graph.Entries[i].Compaction == nil, "compaction entry committed on the failed direct settlement")
		if graph.Entries[i].Settlement != nil {
			settlements++
			testassert.Equal(t, graph.Entries[i].Envelope.OperationID, "op-1")
		}
	}
	// only op-1's ordinary terminal: no checkpoint settlement committed
	testassert.Equal(t, settlements, 1)
}

// TestModelEffectCompactBoundaryExcludesPostFreezeAppends proves the frozen
// boundary against the live interleave: a valid entry appended while the
// trigger's summary attempt is parked lands after the frozen snapshot, and the
// committed boundary still names the freeze-time last projectable entry, so the
// appended signal projects after the summary instead of being named as covered
// and hidden. The post-freeze append is an explicit controlled canonical-signal
// injection through the closed-delivery producer with the admitted fixture
// member — the fixture tests that valid entry, not public background delivery
// continuing a permanently closed execution.
func TestModelEffectCompactBoundaryExcludesPostFreezeAppends(t *testing.T) {
	capture := triggerUsageCapture(4)
	seed := UsageCount{InputTokens: 13}
	h, store, c, sessionID := seededTriggerHarness(t, capture, &seed)
	seeded, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(seeded) == 2 && seeded[1].Kind == EntryAssistant, "seed entries = %+v, want the admitted input and the seed assistant", seeded)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var sent []model.Request
	exec := purposeExecution(
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			sent = append(sent, r)
			return completedTurnStream(), nil
		},
		func(ctx context.Context, r model.Request) (model.Stream, error) {
			arrived <- struct{}{}
			<-release // the park: the freeze is past, the commit has not run
			return summaryStream("summary one", model.Usage{}), nil
		},
	)
	done := make(chan error, 1)
	go func() { // a synchronous call would deadlock on the park
		done <- runAutomaticMaintenance(t, h, c, exec)
	}()
	<-arrived
	// The controlled stimulus: the admitted fixture member's canonical
	// background_completion signal is injected through the closed-delivery
	// producer while the summary is parked, then the member finishes.
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	testassert.NoError(t, err)
	{
		err := h.deliverClosedCompletion(c, member, "Task completed.")
		testassert.NoError(t, err)
		h.finishBackgroundMember(c, member)
	}
	signals := storedSignals(t, store, sessionID)
	testassert.Equal(t, len(signals), 1)
	testassert.Require(t, signals[0].OperationID == "" && signals[0].RelatedMember != nil && signals[0].RelatedMember.Kind == "child" && signals[0].RelatedMember.ID == completionChildID && signals[0].Content == "Task completed.", "committed signal = %+v, want the operationless child-member completion", signals[0])
	close(release)
	testassert.NoError(t, <-done)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Equal(t, comp.BoundaryEntryID, seeded[1].ID)
	// The rebuilt request carried the appended signal after the summary.
	testassert.Equal(t, len(sent), 1)
	sentMsgs := sent[0].Messages
	testassert.Require(t, len(sentMsgs) == 3 && sentMsgs[0].Role == model.RoleSystem && sentMsgs[1].Role == model.RoleAssistant && sentMsgs[1].Source == testCompactCapture().Model && sentMsgs[2].Role == model.RoleUser && sentMsgs[2].TextContent() == "<system-signal>Task completed.</system-signal>", "rebuilt request = %+v, want the system message, the summary, and the wrapped post-freeze signal", sentMsgs)
	// The post-commit projection keeps the signal visible after the
	// summary, ahead of the turn the settled request committed.
	after, err := h.projectContext(c, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 4 && after[0].Role == model.RoleSystem && after[1].Role == model.RoleAssistant && after[1].Source == testCompactCapture().Model && after[1].TextContent() == "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]" && after[2].Role == model.RoleUser && after[2].TextContent() == "<system-signal>Task completed.</system-signal>" && after[3].TextContent() == "done", "post-commit projection = %+v, want the system message, the summary, the wrapped post-freeze signal and the settled turn", after)
}

// TestModelEffectCompactTriggerFullInputCommitAttributesUsage is the
// full-input commit observation the deleted rolling rows carried. A real
// conversation produces the failed partial with its continuation signal, the
// rich tool call and complete result, and the refusal turn. The ordinary manual
// compact then receives the complete ordered frozen input exactly once; the
// expected serialized lines are independently authored (not recomputed by the
// serializer the producer uses), including the raw tool-call argument bytes,
// and the one response's reported usage lands on the Operation-owned
// compaction entry and both registers.
func TestModelEffectCompactTriggerFullInputCommitAttributesUsage(t *testing.T) {
	zero := 0
	rich := streamOf(model.StreamDelta{
		HasChoice: true,
		Role:      "assistant",
		ContentFragments: []model.ContentFragment{
			{Position: 0, Kind: model.PartText, Text: "working"},
			{Position: 1, Kind: model.PartOpaque, OpaqueWireType: "thinking"},
			{Position: 2, Kind: model.PartImageURL, URL: "https://img.test/p.png"},
		},
		ToolFragments: []model.ToolCallFragment{{Position: &zero, ID: "call-rich", Name: "noop", ArgumentFragment: `{"x":1}`}},
		FinishReason:  "tool_calls",
	}, usageDelta(model.Usage{InputTokens: 2, OutputTokens: 1}))
	rounds := 0
	conversation := Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return InvocationExecution{
			Model: func(context.Context, model.Request) (model.Stream, error) {
				rounds++
				switch rounds {
				case 1:
					return failStream(errors.New("provider failure"), turnDelta("", "partial answer"), usageDelta(model.Usage{InputTokens: 1, OutputTokens: 1})), nil
				case 2:
					return rich, nil
				}
				refusal := turnDelta("stop", "")
				refusal.RefusalFragment = "cannot do that"
				return streamOf(refusal), nil
			},
			Tool: func(ctx context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
			},
			NormalizeTool: objectNormalize,
		}, nil
	}}
	compactRef := testCompactCapture().Model
	var requests []model.Request
	compact := compactionExecution(t, func(ctx context.Context, r model.Request) (model.Stream, error) {
		requests = append(requests, r)
		return summaryStream("full summary", model.Usage{InputTokens: 5, OutputTokens: 3}), nil
	}, nil)

	store := emptyStore(t)
	turnCapture := testCapture()
	turnCapture.Tools = append(turnCapture.Tools, model.ToolDefinition{Name: "noop", Description: "noop", Parameters: json.RawMessage(`{"type":"object"}`)})
	preparedTurn := PreparedExecution{Capture: turnCapture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return conversation, nil
	}}
	preparedCompact := compactPrepared(testCapture(), compact)
	queue := []PreparedExecution{preparedTurn, preparedCompact}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		next := queue[0]
		queue = queue[1:]
		return next, nil
	})
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: testSelection("coder")})
	testassert.NoError(t, err)
	sessionID := session.Identity.SessionID
	c := leaseCoordinator(t, h, sessionID)
	mustAdmitWithoutExecution(t, h, sessionID, testOpID, admissionContent("question one"))
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedTurn))

	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistants int
	var signals int
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistants++
		}
		if graph.Entries[i].Signal != nil {
			signals++
		}
	}
	testassert.Require(t, assistants == 3 && signals == 1, "produced rich history = %d assistants, %d signals, want partial, rich and refusal beside one continuation signal", assistants, signals)

	testassert.NoError(t, runManualCompact(t, h, c, sessionID, "compact-1"))
	testassert.Equal(t, len(requests), 1)
	userText := requests[0].Messages[1].TextContent()
	testassert.Require(t, !strings.Contains(userText, "Previous summary:") && !strings.Contains(userText, "Continuation:"), "the request carries continuation framing: %q", userText)
	lines := strings.Split(userText, "\n")
	testassert.Require(t, len(lines) == len(wantRichPieceLines), "serialized lines = %d, want the independently authored %d rich lines:\n%s", len(lines), len(wantRichPieceLines), userText)
	for i, line := range lines {
		var got compactPieceWireLine
		{
			err := json.Unmarshal([]byte(line), &got)
			testassert.Require(t, err == nil, "request line %d unmarshal: %v", i, err)
		}
		want := wantRichPieceLines[i]
		if len(got.ToolCalls) > 0 {
			testassert.Require(t, len(got.ToolCalls) == 1 && got.ToolCalls[0].ID == "call-rich" && got.ToolCalls[0].Name == "noop", "request line %d tool calls = %+v, want the rich noop call", i, got.ToolCalls)
			arguments, uerr := strconv.Unquote(got.ToolCalls[0].Arguments)
			testassert.Require(t, uerr == nil && arguments == `{"x":1}`, "request line %d raw arguments = %q (unquote %v), want the submitted bytes", i, got.ToolCalls[0].Arguments, uerr)
			want.ToolCalls = append([]struct {
				ID        string
				Name      string
				Arguments string
			}(nil), want.ToolCalls...)
			want.ToolCalls[0].Arguments = got.ToolCalls[0].Arguments
		}
		testassert.Require(t, reflect.DeepEqual(got, want), "request line %d = %+v, want independently authored %+v", i, got, want)
	}

	graph, err = validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	comp := compactedEntryOf(t, graph)
	testassert.Require(t, comp.OperationID == "compact-1" && summaryText(t, comp) == "full summary", "compaction entry = %+v, want the Operation-owned summary", comp)
	refusalBoundary := ""
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil && graph.Entries[i].Assistant.Refusal != "" {
			refusalBoundary = graph.Entries[i].Envelope.ID
		}
	}
	testassert.Require(t, refusalBoundary != "" && comp.BoundaryEntryID == refusalBoundary, "compaction boundary = %q, want the refusal turn's entry %q", comp.BoundaryEntryID, refusalBoundary)
	testassert.Require(t, comp.Facts[0].Usage != nil && *comp.Facts[0].Usage == (UsageCount{InputTokens: 5, OutputTokens: 3}), "compaction entry fact usage = %+v, want the reported counts {5,3}", comp.Facts[0].Usage)
	testassert.Equal(t, graph.Session.State.CheckpointEntryID, comp.EntryID)
	want := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 5, OutputTokens: 3}}}}
	{
		state := operationStateOf(t, graph, "compact-1")
		testassert.Require(t, usageTotalsEqual(UsageByModelForTest(state.Usage, compactRef), want), "operation usage = %+v, want the reported counts keyed by the compact model", state.Usage)
	}
	testassert.Require(t, usageTotalsEqual(UsageByModelForTest(graph.Session.State.Usage, compactRef), want), "session compact usage = %+v, want the reported counts keyed by the compact model", graph.Session.State.Usage)
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
