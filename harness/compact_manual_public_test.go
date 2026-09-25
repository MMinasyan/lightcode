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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

var compactModelRef = model.ModelRef{Provider: "cprov", Model: "compact-x"}

// compactManualFixture wires one Harness whose conversation and compact
// transports are separate scripted models over the same prepared capture,
// with the fixture's preparation hook available for gating.
type compactManualFixture struct {
	h           *harness.Harness
	store       harness.Storage
	cancel      context.CancelFunc
	prepMu      sync.Mutex
	preps       []harness.PreparationRequest
	prepareHook func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error)
	defaultPrep harness.PreparedExecution
}

func compactManualCapture() harness.ExecutionCapture {
	capture := publicCapture()
	capture.Compact.Model = compactModelRef
	return capture
}

func newCompactManualFixture(t *testing.T, store harness.Storage, conversation, compact *scriptModel) *compactManualFixture {
	t.Helper()
	f := &compactManualFixture{store: store, cancel: func() {}}
	f.defaultPrep = harness.PreparedExecution{
		Capture: compactManualCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return harness.Execution{
				Model:         conversation.effect,
				CompactModel:  compact.effect,
				NormalizeTool: publicNormalize,
				Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
					return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
				},
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	h, err := harness.New(ctx, harness.Dependencies{Storage: store, Prepare: func(_ context.Context, req harness.PreparationRequest) (harness.PreparedExecution, error) {
		f.prepMu.Lock()
		call := len(f.preps)
		f.preps = append(f.preps, req)
		hook := f.prepareHook
		f.prepMu.Unlock()
		if hook != nil {
			return hook(call, req)
		}
		return f.defaultPrep, nil
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.h = h
	return f
}

func (f *compactManualFixture) preparationCalls() []harness.PreparationRequest {
	f.prepMu.Lock()
	defer f.prepMu.Unlock()
	return append([]harness.PreparationRequest(nil), f.preps...)
}

func (f *compactManualFixture) close() { f.cancel() }

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
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.Kind == harness.EntryCompaction {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

// entryKindsByOperation returns the entry kinds owned by one Operation.
func entryKindsByOperation(t *testing.T, store harness.Storage, sessionID, operationID string) []harness.EntryKind {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var kinds []harness.EntryKind
	for _, entry := range entries {
		if entry.OperationID == operationID {
			kinds = append(kinds, entry.Kind)
		}
	}
	return kinds
}

// idleHistory runs one completed conversation turn so the Session carries
// compactable history and is idle again.
func idleHistory(t *testing.T, f *compactManualFixture, session string) {
	t.Helper()
	res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
	if err != nil || res.Disposition != harness.DispositionAdmitted {
		t.Fatalf("submit = %+v err %v, want admitted", res, err)
	}
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
		if err != nil && !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("ReadOperation: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never settled within the wait bound", operation)
		}
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
		if !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") { // only the transient idle-guard rejection retries
			t.Fatalf("Compact %s: %v", operation, err)
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("Compact %s never admitted within the wait bound: %v", operation, last)
		}
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
func TestPublicManualCompactIdleCommitsAndSettles(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		conversation := newScriptModel(publicTurn())
		compact := newScriptModel(summaryAttempt("S1"))
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)
		conversationCalls := len(conversation.seen())

		res, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if res.Admission.RequestKind != harness.RequestKindCompact || res.Admission.AdmittedEntry != (harness.EntryRef{}) {
			t.Fatalf("admission = %+v, want the compact kind with no admitted entry", res.Admission)
		}
		rec := awaitTerminal(t, f.h, session, "c-1")
		if rec.State.Status != harness.OperationSuccess || rec.State.Terminal == nil {
			t.Fatalf("compact operation state = %+v, want the success terminal", rec.State)
		}
		if got := len(compact.seen()); got != 1 {
			t.Fatalf("compact transport calls = %d, want exactly one piece", got)
		}
		if got := len(conversation.seen()); got != conversationCalls {
			t.Fatalf("conversation transport calls = %d, want unchanged during compaction", got)
		}
		// No input entry and no model-visible user message: the compact
		// Operation owns only its compaction entry and its settlement.
		kinds := entryKindsByOperation(t, store, session, "c-1")
		if len(kinds) != 2 || kinds[0] != harness.EntryCompaction || kinds[1] != harness.EntryOperationSettlement {
			t.Fatalf("compact Operation entries = %v, want exactly one compaction entry and its settlement", kinds)
		}
		if ids := compactionEntryIDs(t, store, session); len(ids) != 1 {
			t.Fatalf("compaction entries = %v, want exactly one", ids)
		}
		// The piece usage landed keyed by the compact model.
		sess, err := f.h.ReadSession(context.Background(), session)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		found := false
		for _, mu := range sess.State.Usage.ByModel {
			if mu.Model == compactModelRef {
				found = mu.Usage == (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5})
			}
		}
		if !found {
			t.Fatalf("session usage = %+v, want the piece counts keyed by the compact model", sess.State.Usage)
		}
		// The compact preparation carried the compact request kind while the
		// conversation turn's preparation carried the message kind.
		preps := f.preparationCalls()
		if len(preps) != 2 || preps[0].RequestKind != harness.RequestKindMessage || preps[1].RequestKind != harness.RequestKindCompact {
			t.Fatalf("preparations = %+v, want the message and the compact kinds", preps)
		}

		// A retry with the same identity resolves the first compact Operation
		// without new work — the resolution precedes the idle check, so it
		// answers even inside the terminal-to-retirement window.
		before := len(compact.seen())
		beforePreps := len(f.preparationCalls())
		retry, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
		if err != nil {
			t.Fatalf("retry Compact: %v", err)
		}
		if retry.Admission.AdmittedAt != res.Admission.AdmittedAt || retry.State.Status != harness.OperationSuccess {
			t.Fatalf("retry record = %+v, want the first settled compact Operation", retry)
		}
		if len(compact.seen()) != before || len(f.preparationCalls()) != beforePreps {
			t.Fatalf("retry ran new work: compact calls %d preps %d", len(compact.seen()), len(f.preparationCalls()))
		}
		if ids := compactionEntryIDs(t, store, session); len(ids) != 1 {
			t.Fatalf("compaction entries after retry = %v, want exactly one", ids)
		}

		// The next conversation request proceeds from the summary projection.
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second"); err != nil {
			t.Fatalf("submit op-2: %v", err)
		}
		awaitSettled(t, f.h, session, "op-2")
		reqs := conversation.seen()
		if len(reqs) != conversationCalls+1 {
			t.Fatalf("conversation transport calls = %d, want one more after the compaction", len(reqs))
		}
		texts := texts(reqs[len(reqs)-1])
		if len(texts) != 3 || !strings.Contains(texts[1], "[Previous conversation summary]") || !strings.Contains(texts[1], "S1") || texts[2] != "second" {
			t.Fatalf("post-compaction request texts = %q, want the system, summary, and new message", texts)
		}

		// Cross-kind identity reuse is rejected on both sides.
		if _, err := submit(t, f.h, session, "c-1", harness.MessageModeRegular, "reuse"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("submit reuse of the compact identity = %v, want ErrInvalid", err)
		}
		if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "op-1"}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("Compact reuse of a message identity = %v, want ErrInvalid", err)
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
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)

		gate := make(chan struct{})
		conversation.gate = gate
		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		if err != nil || res.Disposition != harness.DispositionAdmitted {
			t.Fatalf("submit = %+v err %v, want admitted", res, err)
		}
		<-conversation.arrived

		rejectIdle := func(stage string) {
			t.Helper()
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
			if err == nil || !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
				t.Fatalf("%s: Compact error = %v, want the idle-guard rejection", stage, err)
			}
			if _, err := f.h.ReadOperation(context.Background(), session, "c-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("%s: compact Operation = %v, want not found", stage, err)
			}
		}

		// An active Session.
		rejectIdle("active")

		// A Session with buffered steering: the running Operation consumes it
		// at its next boundary; Compact must not admit beside it.
		if res, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer"); err != nil || res.Disposition != harness.DispositionSteering {
			t.Fatalf("steering submit = %+v err %v, want steering", res, err)
		}
		rejectIdle("steering buffered")

		// A Session with a queued message.
		if res, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queue"); err != nil || res.Disposition != harness.DispositionQueued {
			t.Fatalf("queued submit = %+v err %v, want queued", res, err)
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
		if rec := awaitSettled(t, f.h, session, "op-3"); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("queued operation state = %+v, want success", rec.State.Status)
		}
		if _, err := f.h.ReadOperation(context.Background(), session, "c-1"); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("compact Operation after the drain = %v, want not found", err)
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
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"}); err != nil {
			t.Fatalf("first Compact: %v", err)
		}
		awaitTerminal(t, f.h, session, "c-1")

		// A summary-only Session: the second compaction's input is the
		// existing summary alone. The retry bound covers only the retiring
		// run's release after the first compaction settled.
		compactWhenIdle(t, f.h, session, "c-2")
		awaitSettled(t, f.h, session, "c-2")
		reqs := compact.seen()
		if len(reqs) != 2 {
			t.Fatalf("compact transport calls = %d, want two", len(reqs))
		}
		summaryOnly := texts(reqs[1])[1]
		if !strings.Contains(summaryOnly, "[Previous conversation summary]") || !strings.Contains(summaryOnly, "S1") {
			t.Fatalf("summary-only request = %q, want the serialized summary message", summaryOnly)
		}
		if strings.Contains(summaryOnly, "hello") {
			t.Fatalf("summary-only request carried pre-boundary entries: %q", summaryOnly)
		}

		// With later entries, the snapshot carries the summary and the
		// post-boundary entries.
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second turn"); err != nil {
			t.Fatalf("submit op-2: %v", err)
		}
		awaitSettled(t, f.h, session, "op-2")
		compactWhenIdle(t, f.h, session, "c-3")
		awaitSettled(t, f.h, session, "c-3")
		reqs = compact.seen()
		if len(reqs) != 3 {
			t.Fatalf("compact transport calls = %d, want three", len(reqs))
		}
		both := texts(reqs[2])[1]
		if !strings.Contains(both, "S2") || !strings.Contains(both, "second turn") {
			t.Fatalf("snapshot request = %q, want the summary and the later entries", both)
		}
		if ids := compactionEntryIDs(t, store, session); len(ids) != 3 {
			t.Fatalf("compaction entries = %v, want three", ids)
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
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)

		if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"}); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		rec := awaitTerminal(t, f.h, session, "c-1")
		if rec.State.Status != harness.OperationFailure || rec.State.Terminal.Detail != "nothing to compact" {
			t.Fatalf("compact operation = %+v, want the failure with the retained detail", rec.State)
		}
		if got := len(compact.seen()); got != 0 {
			t.Fatalf("compact transport calls = %d, want none", got)
		}
		kinds := entryKindsByOperation(t, store, session, "c-1")
		if len(kinds) != 1 || kinds[0] != harness.EntryOperationSettlement {
			t.Fatalf("compact Operation entries = %v, want only the settlement", kinds)
		}
		if ids := compactionEntryIDs(t, store, session); len(ids) != 0 {
			t.Fatalf("compaction entries = %v, want none", ids)
		}
		sess, err := f.h.ReadSession(context.Background(), session)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if sess.State.CompactionEntryID != "" {
			t.Fatalf("compaction_entry_id = %q, want it unchanged", sess.State.CompactionEntryID)
		}
		// Subsequent messages never see a synthetic compact request.
		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		awaitSettled(t, f.h, session, "op-1")
		reqs := conversation.seen()
		if len(reqs) != 1 {
			t.Fatalf("conversation calls = %d, want one", len(reqs))
		}
		if texts := texts(reqs[0]); len(texts) != 2 || texts[1] != "hello" {
			t.Fatalf("first conversation request = %q, want the system and the user message alone", texts)
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
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		session := createSession(t, f.h)
		idleHistory(t, f, session)

		gate := make(chan struct{})
		compact.gate = gate
		compactErr := make(chan error, 1)
		go func() {
			_, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-1"})
			compactErr <- err
		}()
		<-compact.arrived // the request arrived: the snapshot is already frozen

		// A Submit arriving after Compact starts stays buffered.
		res, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "second turn")
		if err != nil || res.Disposition != harness.DispositionSteering {
			t.Fatalf("submit during compaction = %+v err %v, want steering", res, err)
		}
		// The pure manual snapshot never carried the buffered message.
		snapshot := texts(compact.seen()[0])[1]
		if strings.Contains(snapshot, "second turn") {
			t.Fatalf("compact snapshot carried the buffered message: %q", snapshot)
		}
		// A second Compact under the running compaction is rejected.
		if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "c-2"}); err == nil || !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
			t.Fatalf("second Compact = %v, want the idle-guard rejection", err)
		}

		close(gate)
		if err := <-compactErr; err != nil {
			t.Fatalf("Compact: %v", err)
		}
		awaitSettled(t, f.h, session, "c-1")
		// The buffered message is delivered only after the compaction settles.
		awaitSettled(t, f.h, session, "op-2")
		reqs := conversation.seen()
		if len(reqs) != 2 {
			t.Fatalf("conversation calls = %d, want the pre-compaction turn and the drained one", len(reqs))
		}
		texts := texts(reqs[1])
		if len(texts) != 3 || !strings.Contains(texts[1], "S1") || texts[2] != "second turn" {
			t.Fatalf("drained request = %q, want the summary projection with the buffered message", texts)
		}
	})
}
