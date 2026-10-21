// Public-surface contract coverage for manual compaction: an idle Session
// admits a dedicated compact Operation that runs the orchestration over the
// compact transport and commits the one manual transaction, non-idle
// Sessions are rejected by the one idle-guard text without overtaking
// buffered work, the frozen snapshot carries the existing summary and later
// entries, an empty conversation fails with the retained detail, and a
// Submit arriving after Compact starts stays buffered until the manual
// compaction settles.
package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

var compactModelRef = model.ModelRef{Provider: "cprov", Model: "compact-x"}

// compactManualCapture is the manual compaction fixture's capture: the
// conversation capture with the compact model identity.
func compactManualCapture() harness.ExecutionCapture {
	capture := publicCapture()
	capture.Compact.Model = compactModelRef
	return capture
}

// summaryTurnStream assembles to one completed output carrying the given
// summary text and reported usage.
func summaryTurnStream(text string, usage model.Usage) model.Stream {
	return &publicScriptStream{deltas: []model.StreamDelta{
		{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: text}}, FinishReason: "stop"},
		{Usage: &usage},
	}}
}

// summaryAttempt scripts one accepted compact attempt carrying a summary.
func summaryAttempt(text string) publicAttempt {
	return publicAttempt{stream: summaryTurnStream(text, model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5})}
}

// compactionEntryIDs returns the entry identities of one compaction kind.
func compactionEntryIDs(t *testing.T, store harness.Storage, sessionID string) []string {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	var ids []string
	for _, entry := range entries {
		if entry.Kind == harness.EntryCompaction {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

// entryKindsByOperation returns the entry kinds owned by one Operation
// through the shared entry-kind selection.
func entryKindsByOperation(t *testing.T, store harness.Storage, sessionID, operationID string) []harness.EntryKind {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	owned := make([]harness.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.OperationID == operationID {
			owned = append(owned, entry)
		}
	}
	return kindsOf(owned)
}

// settlementPayloadOf returns the one settlement entry payload of the given
// Operation.
func settlementPayloadOf(t *testing.T, store harness.Storage, sessionID, operationID string) string {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	var found string
	for _, entry := range entries {
		if entry.OperationID == operationID && entry.Kind == harness.EntryOperationSettlement {
			testassert.Require(t, found == "", "operation %q carries more than one settlement entry", operationID)
			found = string(entry.Payload)
		}
	}
	testassert.Require(t, found != "", "operation %q carries no settlement entry", operationID)
	return found
}

// idleHistory runs one completed conversation turn so the Session carries
// compactable history and is idle again.
func idleHistory(t *testing.T, f *publicFixture, session string) {
	t.Helper()
	res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
	testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted, "submit = %+v err %v, want admitted", res, err)
	awaitTerminal(t, f.h, session, "op-1")
}

// awaitSettled polls one Operation until it exists and reaches a terminal
// settlement, tolerating the admission delay of a pending drain delivery.
func awaitSettled(t *testing.T, h *harness.Harness, session, operation string) harness.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := h.ReadOperation(context.Background(), session, operation)
		if err == nil && rec.State.Terminal != nil {
			return rec
		}
		testassert.Require(t, err == nil || errors.Is(err, harness.ErrNotFound), "ReadOperation: %v", err)
		testassert.Require(t, !time.Now().After(deadline), "operation %s never settled within the wait bound", operation)
		time.Sleep(time.Millisecond)
	}
}

// compactWhenIdle retries one Compact call until the Session's retiring run
// releases and the admission succeeds, bounding the wait.
func compactWhenIdle(t *testing.T, h *harness.Harness, session, operation string) harness.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for {
		rec, err := h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: operation})
		if err == nil {
			return rec
		}
		// only the transient idle-guard rejection retries
		testassert.Require(t, strings.Contains(err.Error(), "is not idle; admission requires an idle Session"), "Compact %s: %v", operation, err)
		last = err
		testassert.Require(t, !time.Now().After(deadline), "Compact %s never admitted within the wait bound: %v", operation, last)
		time.Sleep(time.Millisecond)
	}
}

// TestPublicManualCompactIdleCommitsAndSettles proves the manual row: an
// idle Session with history admits the caller's compact identity with no
// input entry or model-visible user message, runs the orchestration through
// the compact transport only, commits the one manual transaction, and
// settles success; the projection then carries the summary, a retry resolves
// the first compact Operation without new work, and cross-kind identity
// reuse is rejected on both sides.
// compactCommitGateStore parks the return of every transaction that inserted
// a compaction entry: the frozen post-commit instant where the manual settle
// transaction has committed and its caller has not yet returned.
type compactCommitGateStore struct {
	harness.Storage
	started chan struct{}
	release chan struct{}
}

// compactCommitGateTx watches one transaction for a compaction insertion.
type compactCommitGateTx struct {
	harness.Transaction
	inserted bool
}

func (tx *compactCommitGateTx) InsertEntry(draft harness.EntryDraft) (harness.Entry, error) {
	if draft.Kind == harness.EntryCompaction {
		tx.inserted = true
	}
	return tx.Transaction.InsertEntry(draft)
}

func (s *compactCommitGateStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	var gate compactCommitGateTx
	err := s.Storage.Transact(ctx, func(inner harness.Transaction) error {
		gate = compactCommitGateTx{Transaction: inner}
		return fn(&gate)
	})
	if err == nil && gate.inserted {
		select {
		case s.started <- struct{}{}:
		default:
		}
		<-s.release
	}
	return err
}

// parkedUsageCount reports whether one raw register usage section carries
// exactly the summary counts keyed by the compact model.
func parkedUsageCount(t *testing.T, raw json.RawMessage, wantRef model.ModelRef) bool {
	t.Helper()
	// The register's usage section encodes its model identity in the wire
	// object form, not the ModelRef string form.
	var totals struct {
		ByModel []struct {
			Model struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"model"`
			Usage harness.UsageCount `json:"usage"`
		} `json:"by_model"`
	}
	{
		err := json.Unmarshal(raw, &totals)
		testassert.NoError(t, err)
	}
	for _, mu := range totals.ByModel {
		if mu.Model.Provider == wantRef.Provider && mu.Model.Model == wantRef.Model {
			return mu.Usage == (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5})
		}
	}
	return false
}

func TestPublicManualCompactIdleCommitsAndSettles(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		gated := &compactCommitGateStore{Storage: store, started: make(chan struct{}, 1), release: make(chan struct{})}
		f := newPublicFixtureWithJobs(t, gated, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)
		conversationCalls := len(conversation.seen())

		res := compactWhenIdle(t, f.h, session, "c-1")
		testassert.Require(t, res.Admission.RequestKind == harness.RequestKindCompact && res.Admission.AdmittedEntry == (harness.EntryRef{}), "admission = %+v, want the compact kind with no admitted entry", res.Admission)
		// The frozen instant: the manual settle transaction committed the
		// compaction entry and the already-terminal register state together,
		// and its caller has not returned — a split implementation would show
		// the entry without the terminal registers.
		select {
		case <-gated.started:
		case <-time.After(10 * time.Second):
			t.Fatalf("the manual compact never committed its settle transaction")
		}
		parkedEntries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		var parkedCompaction *harness.Entry
		for i := range parkedEntries {
			if parkedEntries[i].Kind == harness.EntryCompaction {
				parkedCompaction = &parkedEntries[i]
			}
		}
		testassert.Require(t, parkedCompaction != nil && parkedCompaction.OperationID == "c-1", "parked compaction entry = %+v, want the c-1-owned commit", parkedCompaction)
		var parkedWire struct {
			Summary string              `json:"summary"`
			Usage   *harness.UsageCount `json:"usage"`
		}
		{
			err := json.Unmarshal(parkedCompaction.Payload, &parkedWire)
			testassert.NoError(t, err)
		}
		testassert.Require(t, parkedWire.Summary == "S1" && parkedWire.Usage != nil && *parkedWire.Usage == (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), "parked compaction entry = %+v, want the committed summary and summary usage", parkedWire)
		parkedSessionReg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var parkedSession struct {
			State struct {
				CompactionEntryID string          `json:"compaction_entry_id"`
				Usage             json.RawMessage `json:"usage"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(parkedSessionReg.Payload, &parkedSession)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, parkedSession.State.CompactionEntryID, parkedCompaction.ID)
		testassert.Require(t, parkedUsageCount(t, parkedSession.State.Usage, compactModelRef), "parked session usage = %s, want the summary counts keyed by the compact model", parkedSessionReg.Payload)
		parkedOpReg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "c-1"})
		testassert.NoError(t, err)
		var parkedOperation struct {
			State struct {
				Status   harness.OperationState `json:"status"`
				Terminal *struct {
					SettlementEntry struct {
						EntryID string `json:"entry_id"`
					} `json:"settlement_entry"`
				} `json:"terminal"`
				Usage json.RawMessage `json:"usage"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(parkedOpReg.Payload, &parkedOperation)
			testassert.NoError(t, err)
		}
		testassert.Require(t, parkedOperation.State.Status == harness.OperationSuccess && parkedOperation.State.Terminal != nil, "parked operation state = %s, want the register already terminal at the frozen instant", parkedOpReg.Payload)
		testassert.Require(t, parkedUsageCount(t, parkedOperation.State.Usage, compactModelRef), "parked operation usage = %s, want the summary counts keyed by the compact model", parkedOpReg.Payload)
		close(gated.release)
		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationSuccess && rec.State.Terminal != nil, "compact operation state = %+v, want the success terminal", rec.State)
		{
			got := len(compact.seen())
			testassert.Equal(t, got, 1)
		}
		{
			got := len(conversation.seen())
			testassert.Equal(t, got, conversationCalls)
		}
		// No input entry and no model-visible user message: the compact
		// Operation owns only its compaction entry and its settlement.
		kinds := entryKindsByOperation(t, store, session, "c-1")
		testassert.Require(t, len(kinds) == 2 && kinds[0] == harness.EntryCompaction && kinds[1] == harness.EntryOperationSettlement, "compact Operation entries = %v, want exactly one compaction entry and its settlement", kinds)
		{
			payload := settlementPayloadOf(t, store, session, "c-1")
			testassert.Require(t, !strings.Contains(payload, "compaction_usage"), "manual success settlement payload %s, want the marker omitted", payload)
		}
		{
			ids := compactionEntryIDs(t, store, session)
			testassert.Require(t, len(ids) == 1, "compaction entries = %v, want exactly one", ids)
		}
		// The summary usage landed keyed by the compact model.
		view, err := f.h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		pieces := usageByModel(view.TotalUsage, compactModelRef)
		testassert.Require(t, len(pieces.ByModel) == 1 && pieces.ByModel[0].Usage == (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), "session usage = %+v, want the summary counts keyed by the compact model", view.TotalUsage)
		// The compact preparation carried the compact request kind while the
		// conversation turn's preparation carried the message kind.
		preps := f.preparationCalls()
		testassert.Require(t, len(preps) == 2 && preps[0].RequestKind == harness.RequestKindMessage && preps[1].RequestKind == harness.RequestKindCompact, "preparations = %+v, want the message and the compact kinds", preps)

		// A retry with the same identity resolves the first compact Operation
		// without new work — the resolution precedes the idle check, so it
		// answers even inside the terminal-to-retirement window.
		before := len(compact.seen())
		beforePreps := len(f.preparationCalls())
		retry, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
		testassert.NoError(t, err)
		testassert.Require(t, retry.Admission.AdmittedAt == res.Admission.AdmittedAt && retry.State.Status == harness.OperationSuccess, "retry record = %+v, want the first settled compact Operation", retry)
		if len(compact.seen()) != before || len(f.preparationCalls()) != beforePreps {
			t.Fatalf("retry ran new work: compact calls %d preps %d", len(compact.seen()), len(f.preparationCalls()))
		}
		{
			ids := compactionEntryIDs(t, store, session)
			testassert.Require(t, len(ids) == 1, "compaction entries after retry = %v, want exactly one", ids)
		}

		// The next conversation request proceeds from the summary projection.
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second")
			testassert.NoError(t, err)
		}
		awaitSettled(t, f.h, session, "op-2")
		reqs := conversation.seen()
		testassert.Equal(t, len(reqs), conversationCalls+1)
		texts := texts(reqs[len(reqs)-1])
		testassert.Require(t, len(texts) == 3 && strings.Contains(texts[1], "[Previous conversation summary]") && strings.Contains(texts[1], "S1") && texts[2] == "second", "post-compaction request texts = %q, want the system, summary, and new message", texts)

		// Cross-kind identity reuse is rejected on both sides.
		{
			_, err := submit(t, f.h, session, "c-1", harness.MessageModeRegular, "reuse")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "submit reuse of the compact identity = %v, want ErrInvalid", err)
		}
		{
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "op-1"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "Compact reuse of a message identity = %v, want ErrInvalid", err)
		}
	})
}

// TestPublicManualCompactRejectsNonIdle proves the idle-guard rows: an active
// Session, a Session with buffered steering or queued messages, and a Session
// inside the terminal-to-drain window with a buffered message all reject with
// the one idle-guard text, create no Operation, never overtake the pending
// message, and never enter either buffer — the normal drain delivers the
// buffered message first.
func TestPublicManualCompactRejectsNonIdle(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)

		gate := make(chan struct{})
		conversation.gate = gate
		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted, "submit = %+v err %v, want admitted", res, err)
		<-conversation.arrived

		rejectIdle := func(stage string) {
			t.Helper()
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "is not idle; admission requires an idle Session"), "%s: Compact error = %v, want the idle-guard rejection", stage, err)
			{
				_, err := f.h.ReadOperation(context.Background(), session, "c-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "%s: compact Operation = %v, want not found", stage, err)
			}
		}

		// An active Session.
		rejectIdle("active")

		// A Session with buffered steering: the boundary exit delivers it
		// through its own successor admission; Compact must not admit beside it.
		{
			res, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer")
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionSteering, "steering submit = %+v err %v, want steering", res, err)
		}
		rejectIdle("steering buffered")

		// A Session with a queued message.
		{
			res, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queue")
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionQueued, "queued submit = %+v err %v, want queued", res, err)
		}
		rejectIdle("queued buffered")

		// The terminal-to-drain window: op-1 settles, the drain prepares the
		// queued message while Compact must still see the retiring run.
		preparedDrain := make(chan struct{})
		releaseDrain := make(chan struct{})
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			if call == 1 { // op-3's drain-delivered preparation
				close(preparedDrain)
				<-releaseDrain
			}
			return f.defaultPrep, nil
		}
		close(gate)
		awaitTerminal(t, f.h, session, "op-1")
		<-preparedDrain
		rejectIdle("retiring window")
		close(releaseDrain)
		// The normal drain delivered the queued message; the compaction never
		// overtook it and never entered either buffer.
		{
			rec := awaitSettled(t, f.h, session, "op-3")
			testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
		}
		{
			_, err := f.h.ReadOperation(context.Background(), session, "c-1")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "compact Operation after the drain = %v, want not found", err)
		}
	})
}

// TestPublicManualCompactSnapshotShape proves the frozen snapshot rows: a
// fully summarized idle Session supplies its existing summary even with no
// later entries, and with later entries the snapshot carries both the summary
// and those entries.
func TestPublicManualCompactSnapshotShape(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"), summaryAttempt("S2"), summaryAttempt("S3"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		compactWhenIdle(t, f.h, session, "c-1")
		awaitTerminal(t, f.h, session, "c-1")

		// A summary-only Session: the second compaction's input is the
		// existing summary alone. The retry bound covers only the retiring
		// run's release after the first compaction settled.
		compactWhenIdle(t, f.h, session, "c-2")
		awaitSettled(t, f.h, session, "c-2")
		reqs := compact.seen()
		testassert.Equal(t, len(reqs), 2)
		summaryOnly := texts(reqs[1])[1]
		testassert.Require(t, strings.Contains(summaryOnly, "[Previous conversation summary]") && strings.Contains(summaryOnly, "S1") && !strings.Contains(summaryOnly, "hello"), "summary-only request = %q, want the serialized summary message; summary-only request carried pre-boundary entries: %q", summaryOnly, summaryOnly)

		// With later entries, the snapshot carries the summary and the
		// post-boundary entries.
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second turn")
			testassert.NoError(t, err)
		}
		awaitSettled(t, f.h, session, "op-2")
		compactWhenIdle(t, f.h, session, "c-3")
		awaitSettled(t, f.h, session, "c-3")
		reqs = compact.seen()
		testassert.Equal(t, len(reqs), 3)
		both := texts(reqs[2])[1]
		testassert.Require(t, strings.Contains(both, "S2") && strings.Contains(both, "second turn"), "snapshot request = %q, want the summary and the later entries", both)
		{
			ids := compactionEntryIDs(t, store, session)
			testassert.Require(t, len(ids) == 3, "compaction entries = %v, want three", ids)
		}
	})
}

// TestPublicManualCompactEmptyConversationFails proves the empty row: a fresh
// Session's manual compaction fails with the retained detail, commits no
// input or compaction entry, and leaves no synthetic compact request in the
// next conversation's input.
func TestPublicManualCompactEmptyConversationFails(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)

		{
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
			testassert.NoError(t, err)
		}
		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationFailure && rec.State.Terminal.Detail == "nothing to compact", "compact operation = %+v, want the failure with the retained detail", rec.State)
		{
			got := len(compact.seen())
			testassert.Equal(t, got, 0)
		}
		kinds := entryKindsByOperation(t, store, session, "c-1")
		testassert.Require(t, len(kinds) == 1 && kinds[0] == harness.EntryOperationSettlement, "compact Operation entries = %v, want only the settlement", kinds)
		{
			payload := settlementPayloadOf(t, store, session, "c-1")
			testassert.Require(t, strings.Contains(payload, `"compaction_usage":true`), "failed compact settlement payload %s, want the inert summary marker durable", payload)
		}
		{
			ids := compactionEntryIDs(t, store, session)
			testassert.Require(t, len(ids) == 0, "compaction entries = %v, want none", ids)
		}
		sessionReg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var sessionState struct {
			State struct {
				CompactionEntryID string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(sessionReg.Payload, &sessionState)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, sessionState.State.CompactionEntryID, "")
		// Subsequent messages never see a synthetic compact request.
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		awaitSettled(t, f.h, session, "op-1")
		reqs := conversation.seen()
		testassert.Equal(t, len(reqs), 1)
		{
			texts := texts(reqs[0])
			testassert.Require(t, len(texts) == 2 && texts[1] == "hello", "first conversation request = %q, want the system and the user message alone", texts)
		}
	})
}

// TestPublicManualCompactFailedSummaryMarkerSurvivesReload proves the usage
// provenance across a cold reload on both stores: a manual compaction whose
// summary fails after reporting usage leaves that usage durably on the marked
// settlement entry, and the conversation measurement reads the assistant
// turn's usage unchanged through the warm view and through a freshly built
// Harness over the same store — whose Session graph cold-loads from storage
// via validateSessionGraph — because the marked settlement never supplies the
// measurement. A reload that lost the marker would surface the settlement's
// {5,7} in place of the assistant's {3,1,2} and flip this assertion.
func TestPublicManualCompactFailedSummaryMarkerSurvivesReload(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(publicAttempt{stream: publicPartialTurnStream()})
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		compactWhenIdle(t, f.h, session, "c-1")
		rec := awaitTerminal(t, f.h, session, "c-1")
		testassert.Require(t, rec.State.Status == harness.OperationFailure && strings.Contains(rec.State.Terminal.Detail, "provider failure"), "compact operation = %+v, want the failed summary terminal", rec.State)
		payload := settlementPayloadOf(t, store, session, "c-1")
		testassert.Require(t, strings.Contains(payload, `"compaction_usage":true`) && strings.Contains(payload, `"input_tokens":5`), "failed compact settlement payload %s, want the marked {5,7} summary usage durable", payload)

		want := harness.UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}
		// The cold reload first: a fresh Harness over the same store
		// cold-loads the Session graph from storage; the same value the warm
		// live graph returns must survive the durable round-trip.
		cold := newPublicFixtureWithJobs(t, store, nil, newScriptModel(publicTurn()), nil, newScriptModel(publicTurn()).effect)
		cold.defaultPrep.Capture = compactManualCapture()
		defer cold.close()
		view, err := cold.h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		got := view.History.LatestContextUsage()
		testassert.Require(t, got != nil && *got == want, "reloaded measurement = %+v, want the assistant turn's %+v: the cold load lost the settlement marker", got, want)
		warm, err := f.h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		{
			measured := warm.History.LatestContextUsage()
			testassert.Require(t, measured != nil && *measured == *got, "warm measurement = %+v, want the reloaded view's %+v", measured, got)
		}
	})
}

// TestPublicManualCompactSubmitAfterAdmissionStaysBuffered proves the
// concurrency row: Compact and Submit share the admission reservation, a
// Submit arriving after Compact starts remains buffered throughout the pure
// manual snapshot and is delivered only after Compact settles, and a second
// Compact under the running compaction is rejected.
func TestPublicManualCompactSubmitAfterAdmissionStaysBuffered(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		gate := make(chan struct{})
		compact.gate = gate
		compactErr := make(chan error, 1)
		go func() {
			// The guarded retry: a pre-reservation idle-guard rejection
			// creates nothing, so retrying it is clean, and the result sent
			// on the channel is always the admission's real outcome — a
			// transient rejection never goes unread.
			deadline := time.Now().Add(10 * time.Second)
			var err error
			for {
				_, err = f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
				if err == nil || !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
					break
				}
				if time.Now().After(deadline) {
					break // the last idle-guard rejection is the reported outcome
				}
				time.Sleep(time.Millisecond)
			}
			compactErr <- err
		}()
		<-compact.arrived // the request arrived: the snapshot is already frozen

		// A Submit arriving after Compact starts stays buffered.
		res, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second turn")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionSteering, "submit during compaction = %+v err %v, want steering", res, err)
		// The pure manual snapshot never carried the buffered message.
		snapshot := texts(compact.seen()[0])[1]
		testassert.Require(t, !strings.Contains(snapshot, "second turn"), "compact snapshot carried the buffered message: %q", snapshot)
		// A second Compact under the running compaction is rejected.
		{
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-2"})
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "is not idle; admission requires an idle Session"), "second Compact = %v, want the idle-guard rejection", err)
		}

		close(gate)
		{
			err := <-compactErr
			testassert.NoError(t, err)
		}
		awaitSettled(t, f.h, session, "c-1")
		// The buffered message is delivered only after the compaction settles.
		awaitSettled(t, f.h, session, "op-2")
		reqs := conversation.seen()
		testassert.Equal(t, len(reqs), 2)
		texts := texts(reqs[1])
		testassert.Require(t, len(texts) == 3 && strings.Contains(texts[1], "S1") && texts[2] == "second turn", "drained request = %q, want the summary projection with the buffered message", texts)
	})
}

// TestPublicManualCompactInterruptKeepsPriorProjection proves the interrupt
// row: a live interrupt of a manual compact Operation parked at its summary request
// settles the interruption with no new compaction entry, the previous
// projection stays current, and the Session's current Operation clears.
func TestPublicManualCompactInterruptKeepsPriorProjection(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		f := newPublicFixtureWithJobs(t, store, nil, conversation, nil, compact.effect)
		f.defaultPrep.Capture = compactManualCapture()
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		compactWhenIdle(t, f.h, session, "c-1")
		{
			rec := awaitTerminal(t, f.h, session, "c-1")
			testassert.Require(t, rec.State.Status == harness.OperationSuccess, "first compact = %+v, want success", rec.State)
		}
		prior := compactionEntryIDs(t, store, session)
		testassert.Require(t, len(prior) == 1, "prior compaction entries = %v, want exactly one", prior)
		priorID := prior[0]

		// The second compact's summary request parks until its own effect context
		// ends: the interrupt releases it and settles the interruption.
		summaryStarted := make(chan struct{}, 1)
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			if req.RequestKind == harness.RequestKindCompact && call >= 2 {
				return harness.PreparedExecution{
					Capture: compactManualCapture(),
					Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
						return harness.Execution{
							Model: conversation.effect,
							CompactModel: func(ctx context.Context, _ model.Request) (model.Stream, error) {
								select {
								case summaryStarted <- struct{}{}:
								default:
								}
								<-ctx.Done() // the interrupt ends the parked summary request
								return nil, ctx.Err()
							},
							Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
								return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
							},
							NormalizeTool: publicNormalize,
						}, nil
					},
				}, nil
			}
			return f.defaultPrep, nil
		}
		compactWhenIdle(t, f.h, session, "c-2")
		select {
		case <-summaryStarted:
		case <-time.After(10 * time.Second):
			t.Fatalf("the second compact summary request never started")
		}
		{
			err := f.h.Interrupt(ctx, session)
			testassert.NoError(t, err)
		}
		rec := awaitTerminal(t, f.h, session, "c-2")
		testassert.Require(t, rec.State.Status == harness.OperationInterruption, "interrupted compact = %+v, want the interruption terminal", rec.State)
		// No new compaction entry: the prior projection stays current.
		{
			ids := compactionEntryIDs(t, store, session)
			testassert.Require(t, len(ids) == 1 && ids[0] == priorID, "compaction entries after the interrupt = %v, want only the prior %q", ids, priorID)
		}
		reg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var wire struct {
			State struct {
				CurrentOperationID string `json:"current_operation_id"`
				CompactionEntryID  string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(reg.Payload, &wire)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, wire.State.CompactionEntryID, priorID)
		deadline := time.Now().Add(10 * time.Second)
		for wire.State.CurrentOperationID != "" {
			testassert.Require(t, !time.Now().After(deadline), "current Operation %q never cleared after the interruption", wire.State.CurrentOperationID)
			time.Sleep(time.Millisecond)
			reg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession})
			testassert.NoError(t, err)
			{
				err := json.Unmarshal(reg.Payload, &wire)
				testassert.NoError(t, err)
			}
		}
	})
}
