package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// Delivery fixture identities: fixed durable session ids for the parent and
// child of the completion fixtures, one 8-hex job identity, and the child
// fixture's Operation identity.
const (
	completionParentID = "000000000000000000000000000000c2"
	completionChildID  = "000000000000000000000000000000c1"
	jobFixtureID       = "deadbeef"
	childFixtureOp     = "child-op"
)

// cachedCoordinator returns the registry-cached coordinator of one Session.
func cachedCoordinator(t *testing.T, h *Harness, sessionID string) *coordinator {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.sessions[sessionID]
	if c == nil {
		t.Fatalf("no cached coordinator for session %q", sessionID)
	}
	return c
}

// closeBackgroundMode forces the not-receivable closed mode on one
// coordinator, the shape a stopping or closed group presents.
func closeBackgroundMode(c *coordinator) {
	c.mu.Lock()
	c.bgState = bgClosed
	c.mu.Unlock()
}

// completionChildGraph builds one child Session fixture whose single
// Operation carries the given state: running, or terminal with the given
// status and detail. assistantText, when non-empty, commits one assistant
// entry with that text belonging to the Operation.
func completionChildGraph(parentID string, running bool, status OperationState, detail, assistantText string) *testGraph {
	session := validSessionRecord()
	session.Identity.SessionID = completionChildID
	session.Identity.ParentSessionID = parentID
	if running {
		session.State.CurrentOperationID = childFixtureOp
	}
	op := validOperationRecord()
	op.Admission.SessionID = completionChildID
	op.Admission.OperationID = childFixtureOp
	op.Admission.AdmittedEntry = EntryRef{SessionID: completionChildID, EntryID: hexID(1)}
	input := validInputEntry(childFixtureOp)
	input.SessionID = completionChildID
	input.EntryID = hexID(1)
	entries := []testEntry{
		{env: Entry{SessionID: completionChildID, ID: hexID(1), OperationID: childFixtureOp, Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &input},
	}
	if assistantText != "" {
		assistant := validAssistantEntry(childFixtureOp)
		assistant.SessionID = completionChildID
		assistant.EntryID = hexID(2)
		assistant.Content = []model.ContentPart{{Kind: model.PartText, Text: assistantText}}
		entries = append(entries, testEntry{env: Entry{SessionID: completionChildID, ID: hexID(2), OperationID: childFixtureOp, Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &assistant})
	}
	if !running {
		stamped := testTime
		op.State.Status = status
		op.State.SettledAt = &stamped
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: completionChildID, EntryID: hexID(3)}, Detail: detail}
		settlement := validSettlementEntry()
		settlement.SessionID = completionChildID
		settlement.EntryID = hexID(3)
		settlement.OperationID = childFixtureOp
		settlement.Status = status
		settlement.Detail = detail
		entries = append(entries, testEntry{env: Entry{SessionID: completionChildID, ID: hexID(3), OperationID: childFixtureOp, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime}, settlement: &settlement})
	}
	return &testGraph{session: session, ops: []OperationRecord{op}, entries: entries}
}

// addRootSession inserts one extra root Session register into the store, so
// fixed-identity child fixtures gain a delivery target parent.
func addRootSession(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	rec := validSessionRecord()
	rec.Identity.SessionID = sessionID
	raw, err := encodeSessionRegister(rec)
	mustEncode(t, err)
	if err := store.Transact(context.Background(), func(tx Transaction) error {
		_, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: sessionID, Kind: RegisterSession}, Payload: raw})
		return err
	}); err != nil {
		t.Fatalf("insert session %q: %v", sessionID, err)
	}
}

// storedSignals reads every background_completion signal entry of one Session
// straight from durable storage, in committed order.
func storedSignals(t *testing.T, store *graphStorage, sessionID string) []signalEntry {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []signalEntry
	for _, env := range store.entries[sessionID] {
		if env.Kind != EntrySignal {
			continue
		}
		v, err := decodeSignalEntry(env)
		if err != nil {
			t.Fatalf("decode signal entry %s: %v", env.ID, err)
		}
		if v.Signal == SignalBackgroundCompletion {
			out = append(out, v)
		}
	}
	return out
}

// storedEntryCount reads one Session's durable entry count.
func storedEntryCount(store *graphStorage, sessionID string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.entries[sessionID])
}

// storedOperationExists reports whether one Operation register is durable.
func storedOperationExists(store *graphStorage, sessionID, operationID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, reg := range store.registers[sessionID] {
		if reg.Key.Kind == RegisterOperation && reg.Key.OperationID == operationID {
			return true
		}
	}
	return false
}

// waitMemberDone joins one member's terminal transition.
func waitMemberDone(t *testing.T, m *backgroundMember) {
	t.Helper()
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("member done channel did not close")
	}
}

// runtimeInputText returns the text of one Session's runtime-origin input
// entry, read from the validated graph.
func runtimeInputText(t *testing.T, store *graphStorage, sessionID string) string {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("session graph: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Input != nil && entry.Input.Origin == InputOriginRuntime {
			return entry.Input.Content[0].Text
		}
	}
	t.Fatalf("session %q carries no runtime-origin input entry", sessionID)
	return ""
}

// TestDeliverBackgroundCompletionModes proves the delivery priority: a
// not-receivable parent takes the durable signal entry regardless of
// activity, a receivable active parent steers, and a receivable idle parent
// admits under the completion identity.
func TestDeliverBackgroundCompletionModes(t *testing.T) {
	t.Run("closed during harness cancellation with a settling run", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		session := createSession(t, h)
		c := cachedCoordinator(t, h, session)
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived // the run is parked at its model boundary
		cancel()         // harness loss while the run still settles

		if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "lost work"); err != nil {
			t.Fatalf("closed delivery: %v", err)
		}
		signals := storedSignals(t, store, session)
		if len(signals) != 1 {
			t.Fatalf("durable background_completion signals = %d, want 1", len(signals))
		}
		sig := signals[0]
		if sig.OperationID != "" || sig.RelatedMember == nil || sig.RelatedMember.Kind != "child" ||
			sig.RelatedMember.ID != completionChildID || sig.Content != "lost work" {
			t.Fatalf("committed signal = %+v, want the operationless child-member completion", sig)
		}
		waitMemberDone(t, member)

		script.releaseGate() // let the parked run converge
		c.mu.Lock()
		run := c.run
		c.mu.Unlock()
		if run != nil {
			select {
			case <-run.done:
			case <-time.After(2 * time.Second):
				t.Fatalf("the settling run did not converge")
			}
		}
	})

	t.Run("idle admits under the completion identity without a signal entry", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)
		c := cachedCoordinator(t, h, session)
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}

		if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "background answer"); err != nil {
			t.Fatalf("idle delivery: %v", err)
		}
		waitMemberDone(t, member)
		watch.next() // the admitted completion settles

		if rec := settledOperation(t, store, session, member.completionID); rec.State.Status != OperationSuccess {
			t.Fatalf("completion operation settled %q, want success", rec.State.Status)
		}
		if signals := storedSignals(t, store, session); len(signals) != 0 {
			t.Fatalf("a receivable parent recorded %d signal entries, want none", len(signals))
		}
		if got := runtimeInputText(t, store, session); got != "background answer" {
			t.Fatalf("admitted input = %q, want the completion content", got)
		}
	})

	t.Run("steering reaches the model boundary and continues the operation", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)
		c := cachedCoordinator(t, h, session)
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived // the operation is parked at its model boundary

		if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "steered completion"); err != nil {
			t.Fatalf("steering delivery: %v", err)
		}
		waitMemberDone(t, member)
		if n := storedEntryCount(store, session); n != 1 {
			t.Fatalf("steering committed %d durable entries, want only the admitted input", n)
		}

		script.releaseGate()
		<-script.arrived // the continuation drained the steering at the boundary
		if texts := strings.Join(script.lastTexts(), "|"); !strings.Contains(texts, "steered completion") {
			t.Fatalf("continuation projection = %q, want the steered completion", texts)
		}
		watch.next()
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("steered operation settled %q, want success", rec.State.Status)
		}
		if got := entryTexts(t, store, session); !reflect.DeepEqual(got, []string{"hello", "steered completion"}) {
			t.Fatalf("committed inputs = %q, want the steered completion after the first input", got)
		}
		if got := runtimeInputText(t, store, session); got != "steered completion" {
			t.Fatalf("steered input origin/content = %q, want the runtime completion", got)
		}
	})

	t.Run("steering admits under the completion identity when the operation settles first", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(payloadlessTurn())
		script.gate = make(chan struct{})
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)
		c := cachedCoordinator(t, h, session)
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived

		if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "queued completion"); err != nil {
			t.Fatalf("steering delivery: %v", err)
		}
		waitMemberDone(t, member)
		script.releaseGate() // op-1 settles failure; the preserved steering drains through admission
		watch.next()         // op-1's settlement
		watch.next()         // the drain-admitted completion's settlement

		if rec := settledOperation(t, store, session, member.completionID); rec.State.Status != OperationSuccess {
			t.Fatalf("completion operation settled %q, want success", rec.State.Status)
		}
		if got := runtimeInputText(t, store, session); got != "queued completion" {
			t.Fatalf("admitted input = %q, want the completion content under its identity", got)
		}
	})
}

// TestDeliverBackgroundCompletionEmptyContentRejected proves empty completion
// content is rejected before any obligation is claimed.
func TestDeliverBackgroundCompletionEmptyContentRejected(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty content = %v, want ErrInvalid", err)
	}
	c.mu.Lock()
	claimed := member.claimed
	c.mu.Unlock()
	if claimed {
		t.Fatalf("the rejected delivery claimed the member")
	}
}

// TestDeliverBackgroundCompletionSecondDeliveryIsNoOp proves the first-writer
// claim: a second delivery of the same completion changes nothing.
func TestDeliverBackgroundCompletionSecondDeliveryIsNoOp(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)
	c := cachedCoordinator(t, h, session)
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "first"); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	waitMemberDone(t, member)
	if err := h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "second"); err != nil {
		t.Fatalf("second delivery = %v, want a no-op success", err)
	}
	watch.next()

	if got := entryTexts(t, store, session); !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("committed inputs = %q, want exactly the first delivery", got)
	}
	if !storedOperationExists(store, session, member.completionID) {
		t.Fatalf("the completion identity was not admitted")
	}
}

// TestDeliverBackgroundCompletionCorruptParent proves an unavailable parent
// returns the ordinary corruption error with no durable write, closes the
// claimed member once for both producers, and a duplicate callback is a
// no-op.
func TestDeliverBackgroundCompletionCorruptParent(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinatorFor: %v", err)
	}
	childMember, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child admission: %v", err)
	}
	jobMember, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("job admission: %v", err)
	}
	h.mu.Lock()
	c.corru = corruptSession(testSessionID, "injected corruption")
	h.mu.Unlock()

	for _, member := range []*backgroundMember{childMember, jobMember} {
		err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "x")
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("%s delivery = %v, want a corruption error", member.kind, err)
		}
	}
	entries, regs := storedSessionState(store, testSessionID)
	if len(entries) != 0 || len(regs) != 1 {
		t.Fatalf("corrupt-parent delivery wrote entries %d registers %d, want none beyond the session register", len(entries), len(regs))
	}
	waitMemberDone(t, childMember)
	waitMemberDone(t, jobMember)

	if err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, childMember.completionID, "x"); err != nil {
		t.Fatalf("duplicate callback = %v, want a no-op success", err)
	}
}

// TestDeliverBackgroundCompletionIdleRaceReRoutesToSteering proves the idle
// race: a completion that resolved idle takes the steering path when an
// operation becomes active before its reservation is granted.
func TestDeliverBackgroundCompletionIdleRaceReRoutesToSteering(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	script.gate = make(chan struct{})
	stub := newPrepareStub(modelPrepared(script.model))
	stub.gate = make(chan struct{})
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)
	c := cachedCoordinator(t, h, session)
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	submitted := make(chan error, 1)
	go func() {
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID:   session,
			OperationID: "op-1",
			Origin:      InputOriginUser,
			Content:     admissionContent("hello"),
			Mode:        MessageModeRegular,
		})
		submitted <- err
	}()
	<-stub.arrived // the submission parks in preparation, holding the reservation

	delivered := make(chan error, 1)
	go func() {
		delivered <- h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "raced")
	}()
	close(stub.gate) // the submission publishes, installs its run, releases the reservation
	if err := <-submitted; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("raced delivery: %v", err)
	}

	// The rerouted completion is steering, never a separate admission: it is
	// either still buffered or already committed as the active Operation's
	// runtime input at its first model boundary.
	if storedOperationExists(store, session, member.completionID) {
		t.Fatalf("the raced completion admitted a new operation")
	}

	script.releaseGate()
	<-script.arrived // the boundary drained the steering into the running operation
	watch.next()
	if got := entryTexts(t, store, session); !reflect.DeepEqual(got, []string{"hello", "raced"}) {
		t.Fatalf("committed inputs = %q, want the rerouted completion as steering", got)
	}
	waitMemberDone(t, member)
}

// TestDeliverBackgroundCompletionReserveFailureTakesClosedPath proves the
// reservation-failure fallback: with the Session's admission reservation held
// and the harness context canceled, the idle-delivery path's reserve call
// fails and the completion takes the durable closed path. The public dispatch
// and the re-evaluation modes stay covered by the delivery-mode tests.
func TestDeliverBackgroundCompletionReserveFailureTakesClosedPath(t *testing.T) {
	store := emptyStore(t)
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, nil)
	defer cancel()
	session := createSession(t, h)
	c := cachedCoordinator(t, h, session)
	member, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	release, err := c.reserve(context.Background()) // the test holds the admission reservation
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	defer release()
	cancel() // the idle delivery's reserve call fails against the dead context

	if err := h.deliverIdleCompletion(c, member, admissionContent("closed on failure"), "closed on failure"); err != nil {
		t.Fatalf("reserve-failure delivery = %v, want the closed path success", err)
	}
	signals := storedSignals(t, store, session)
	if len(signals) != 1 || signals[0].Content != "closed on failure" {
		t.Fatalf("durable signals = %+v, want the closed-path record", signals)
	}
	h.finishBackgroundMember(c, member)
	waitMemberDone(t, member)
}

// TestDeliverBackgroundCompletionAdmissionFailuresFinal proves the idle
// admission attempt is final under the one-attempt rule: the original error
// returns, the member finishes once, no replacement Operation is admitted,
// and no retry or fallback signal write happens; a storage-class failure is
// latched and a non-storage one is not.
func TestDeliverBackgroundCompletionAdmissionFailuresFinal(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantLatched bool
		wantErr     func(t *testing.T, err error)
	}{
		{"storage failure", fmt.Errorf("%w: injected storage failure", ErrStorage), true, func(t *testing.T, err error) {
			if !errors.Is(err, ErrStorage) {
				t.Fatalf("delivery error = %v, want the storage class", err)
			}
		}},
		{"conflict failure", fmt.Errorf("%w: injected conflict", ErrConflict), false, func(t *testing.T, err error) {
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "already exists in another session") {
				t.Fatalf("delivery error = %v, want the cross-session reuse rejection", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := freshSessionStore(t)
			h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
				return validPrepared(), nil
			})
			c, err := h.coordinatorFor(context.Background(), testSessionID)
			if err != nil {
				t.Fatalf("coordinatorFor: %v", err)
			}
			member, err := admitLocked(h, c, memberChild, completionChildID, 0)
			if err != nil {
				t.Fatalf("admission: %v", err)
			}

			store.txHook = func(string) error { return tc.err } // the delivery's admission transaction fails once
			err = h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "content")
			store.txHook = nil
			tc.wantErr(t, err)

			waitMemberDone(t, member)
			if storedOperationExists(store, testSessionID, member.completionID) {
				t.Fatalf("the failed admission published a replacement Operation")
			}
			if n := storedEntryCount(store, testSessionID); n != 0 {
				t.Fatalf("the failed admission or a retry wrote %d entries, want none", n)
			}
			if signals := storedSignals(t, store, testSessionID); len(signals) != 0 {
				t.Fatalf("a fallback signal write committed %+v, want none", signals)
			}
			h.mu.Lock()
			latched := h.storageFailure != nil
			h.mu.Unlock()
			if latched != tc.wantLatched {
				t.Fatalf("storage failure latched = %v, want %v", latched, tc.wantLatched)
			}
			if err := h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "content"); err != nil {
				t.Fatalf("duplicate completion = %v, want a no-op success", err)
			}
		})
	}
}

// TestClosedDeliveryCommitsBeforeDoneCloses proves the ordering contract: the
// member's done channel closes only after the closed record committed.
func TestClosedDeliveryCommitsBeforeDoneCloses(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
	defer cancel()
	session := createSession(t, h)
	c := cachedCoordinator(t, h, session)
	member, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	closeBackgroundMode(c)

	arrived := make(chan struct{}, 1)
	gate := make(chan struct{})
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntrySignal {
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-gate
		}
		return nil
	}
	delivered := make(chan error, 1)
	go func() {
		delivered <- h.DeliverBackgroundCompletion(context.Background(), session, member.completionID, "durable first")
	}()
	<-arrived // the closed transaction is parked inside storage, the coordinator mutex held
	select {
	case <-member.done:
		t.Fatalf("the member finished before the closed record committed")
	default:
	}
	close(gate)
	if err := <-delivered; err != nil {
		t.Fatalf("closed delivery: %v", err)
	}
	waitMemberDone(t, member)
	signals := storedSignals(t, store, session)
	if len(signals) != 1 || signals[0].RelatedMember == nil || signals[0].RelatedMember.Kind != "job" ||
		signals[0].RelatedMember.ID != jobFixtureID || signals[0].Content != "durable first" {
		t.Fatalf("committed signal = %+v, want the job-member completion", signals)
	}
}

// TestConcurrentClosedDeliveriesSerialize proves two closed deliveries
// preserve cached and durable order: the second cannot enter storage while
// the first holds the coordinator, and both commit with agreeing order, each
// member finishing after its own commit.
func TestConcurrentClosedDeliveriesSerialize(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
	defer cancel()
	session := createSession(t, h)
	c := cachedCoordinator(t, h, session)
	memberA, err := admitLocked(h, c, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child admission: %v", err)
	}
	memberB, err := admitLocked(h, c, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("job admission: %v", err)
	}
	closeBackgroundMode(c)

	var (
		mu       sync.Mutex
		arrivals int
	)
	gate := make(chan struct{})
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntrySignal {
			mu.Lock()
			arrivals++
			first := arrivals == 1
			mu.Unlock()
			if first {
				<-gate
			}
		}
		return nil
	}
	resA := make(chan error, 1)
	resB := make(chan error, 1)
	go func() {
		resA <- h.DeliverBackgroundCompletion(context.Background(), session, memberA.completionID, "from child")
	}()
	go func() {
		resB <- h.DeliverBackgroundCompletion(context.Background(), session, memberB.completionID, "from job")
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := arrivals
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no closed transaction reached storage")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // an unserialized second delivery would have reached storage
	mu.Lock()
	n := arrivals
	mu.Unlock()
	if n != 1 {
		t.Fatalf("second closed delivery entered storage while the first held the coordinator: %d arrivals", n)
	}
	close(gate)

	if err := <-resA; err != nil {
		t.Fatalf("child delivery: %v", err)
	}
	if err := <-resB; err != nil {
		t.Fatalf("job delivery: %v", err)
	}
	waitMemberDone(t, memberA)
	waitMemberDone(t, memberB)

	signals := storedSignals(t, store, session)
	if len(signals) != 2 {
		t.Fatalf("durable signals = %d, want both completions", len(signals))
	}
	c.mu.Lock()
	var cached []string
	for _, entry := range c.graph.Entries {
		if entry.Signal != nil && entry.Signal.Signal == SignalBackgroundCompletion {
			cached = append(cached, entry.Signal.RelatedMember.Kind)
		}
	}
	c.mu.Unlock()
	durable := []string{signals[0].RelatedMember.Kind, signals[1].RelatedMember.Kind}
	if !reflect.DeepEqual(cached, durable) {
		t.Fatalf("cached signal order %v disagrees with durable order %v", cached, durable)
	}
}

// TestClosedDeliveryRevisionDrift proves the closed path never retries: a
// revision drift returns the conflict after one rematerialization with no
// committed entry, a failing rematerialization returns its own error, and a
// storage-class failure is latched.
func TestClosedDeliveryRevisionDrift(t *testing.T) {
	advanceRevision := func(t *testing.T, store *graphStorage) {
		t.Helper()
		err := store.Transact(context.Background(), func(tx Transaction) error {
			key := RegisterKey{SessionID: testSessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			_, err = tx.ReplaceRegister(key, reg.Revision, reg.Payload)
			return err
		})
		if err != nil {
			t.Fatalf("foreign revision advance: %v", err)
		}
	}

	t.Run("drift returns the conflict, rematerializes once, finishes the member", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		closeBackgroundMode(c)
		advanceRevision(t, store)

		err = h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "x")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("drifted delivery = %v, want the conflict class", err)
		}
		waitMemberDone(t, member)
		if n := storedEntryCount(store, testSessionID); n != 0 {
			t.Fatalf("the drifted delivery or a retry committed %d entries, want none", n)
		}
		c.mu.Lock()
		revision := c.graph.Session.Revision
		c.mu.Unlock()
		if revision != 2 {
			t.Fatalf("cached revision %d, want the rematerialized revision 2", revision)
		}
	})

	t.Run("failing rematerialization returns corruption", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		closeBackgroundMode(c)
		advanceRevision(t, store)
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			key := RegisterKey{SessionID: testSessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			_, err = tx.ReplaceRegister(key, reg.Revision, json.RawMessage(`{"bogus":true}`))
			return err
		}); err != nil {
			t.Fatalf("corrupt register: %v", err)
		}

		err = h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "x")
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("delivery = %v, want the rematerialization corruption", err)
		}
		waitMemberDone(t, member)
		if n := storedEntryCount(store, testSessionID); n != 0 {
			t.Fatalf("the failed delivery committed %d entries, want none", n)
		}
	})

	t.Run("storage-class rematerialization failure is latched", func(t *testing.T) {
		store := freshSessionStore(t)
		script := newModelScript()
		h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
		defer cancel()
		c, err := h.coordinatorFor(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("coordinatorFor: %v", err)
		}
		member, err := admitLocked(h, c, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
		closeBackgroundMode(c)
		advanceRevision(t, store)
		store.registersErr = fmt.Errorf("%w: storage down", ErrStorage)

		err = h.DeliverBackgroundCompletion(context.Background(), testSessionID, member.completionID, "x")
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("delivery = %v, want the rematerialization storage failure", err)
		}
		h.mu.Lock()
		latched := h.storageFailure != nil
		h.mu.Unlock()
		if !latched {
			t.Fatalf("the storage-class rematerialization failure was not latched")
		}
		waitMemberDone(t, member)
	})
}

// TestJobCompletionWaitsForChildSuccessorRetirement proves the recursive
// completion flow: a job completion arriving while the child Operation runs is
// queued, the member-finish tail defers, the post-terminal drain admits the
// completion as the successor Operation, the settlement check defers on the
// installed run until the successor retires, and the parent-facing result
// reaches the parent only afterwards — carrying the successor's own final
// answer.
func TestJobCompletionWaitsForChildSuccessorRetirement(t *testing.T) {
	store := completionChildGraph(completionParentID, false, OperationSuccess, "", "").storage(t)
	addRootSession(t, store, completionParentID)
	script := newModelScript(payloadlessTurn(), modelAttempt{stream: streamOf(turnDelta("stop", "final"))})
	script.gate = make(chan struct{})
	stub := newPrepareStub(modelPrepared(script.model))
	h, cancel := newCancelableHarness(t, store, PreparedExecution{}, stub.prepare)
	defer cancel()
	watch := watchSettlements(store)
	cP, err := h.coordinatorFor(context.Background(), completionParentID)
	if err != nil {
		t.Fatalf("parent coordinator: %v", err)
	}
	cC, err := h.coordinatorFor(context.Background(), completionChildID)
	if err != nil {
		t.Fatalf("child coordinator: %v", err)
	}
	member, err := admitLocked(h, cP, memberChild, completionChildID, 0)
	if err != nil {
		t.Fatalf("child member admission: %v", err)
	}
	cC.mu.Lock()
	cC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
	cC.mu.Unlock()
	jobMember, err := admitLocked(h, cC, memberJob, jobFixtureID, 0)
	if err != nil {
		t.Fatalf("job admission: %v", err)
	}

	if _, err := h.Submit(context.Background(), SubmitRequest{
		SessionID:   completionChildID,
		OperationID: "op-1",
		Origin:      InputOriginUser,
		Content:     admissionContent("hello"),
		Mode:        MessageModeRegular,
	}); err != nil {
		t.Fatalf("child submit: %v", err)
	}
	<-script.arrived // the child run is parked at its model boundary

	if err := h.DeliverBackgroundCompletion(context.Background(), completionChildID, jobMember.completionID, "job done"); err != nil {
		t.Fatalf("job completion delivery: %v", err)
	}
	waitMemberDone(t, jobMember)
	cC.mu.Lock()
	pending, steering := cC.pendingCompletion != nil, len(cC.steering)
	cC.mu.Unlock()
	if !pending || steering != 1 {
		t.Fatalf("after the job delivery: pending %v steering %d, want the obligation deferred and the completion queued", pending, steering)
	}
	if storedOperationExists(store, completionChildID, jobMember.completionID) {
		t.Fatalf("the job completion admitted an operation while the run was active")
	}

	stub.gate = make(chan struct{}) // the drain's admission of the completion will park here
	script.releaseGate()            // op-1 settles failure with the completion still queued
	<-stub.arrived                  // the drain is parked in preparation, the retired run still installed
	watch.next()                    // op-1's settlement committed

	// the retired-but-not-drained state: the settlement check defers on the run
	h.childCompletionSettled(cC, "")
	cC.mu.Lock()
	pendingDeferred, runInstalled := cC.pendingCompletion != nil, cC.run != nil
	cC.mu.Unlock()
	if !pendingDeferred || !runInstalled {
		t.Fatalf("deferred check: pending %v run %v, want the obligation held on the installed run", pendingDeferred, runInstalled)
	}
	if storedOperationExists(store, completionParentID, member.completionID) {
		t.Fatalf("the parent received the completion before the successor retired")
	}

	close(stub.gate) // the drain admits the completion as the successor operation
	watch.next()     // the successor's settlement committed
	watch.next()     // the parent completion's settlement committed

	if rec := settledOperation(t, store, completionParentID, member.completionID); rec.State.Status != OperationSuccess {
		t.Fatalf("parent completion settled %q, want success", rec.State.Status)
	}
	if got := runtimeInputText(t, store, completionParentID); got != "final" {
		t.Fatalf("delivered completion = %q, want the successor's own final answer", got)
	}
	cC.mu.Lock()
	pendingAfter := cC.pendingCompletion
	cC.mu.Unlock()
	if pendingAfter != nil {
		t.Fatalf("the pending obligation survived the completed delivery")
	}
	waitMemberDone(t, member)
}

// TestChildCompletionSettledAbandonment proves the settlement check abandons
// only on a non-empty settled operation identity still current — the failed
// terminal commit — and defers on an empty one while the child runs.
func TestChildCompletionSettledAbandonment(t *testing.T) {
	newRunningChild := func(t *testing.T) (*Harness, *graphStorage, *coordinator, *backgroundMember) {
		t.Helper()
		store := completionChildGraph(completionParentID, true, OperationRunning, "", "hi").storage(t)
		addRootSession(t, store, completionParentID)
		h := newTestHarness(t, store, nil)
		cC, err := h.coordinatorFor(context.Background(), completionChildID)
		if err != nil {
			t.Fatalf("child coordinator: %v", err)
		}
		cP, err := h.coordinatorFor(context.Background(), completionParentID)
		if err != nil {
			t.Fatalf("parent coordinator: %v", err)
		}
		member, err := admitLocked(h, cP, memberChild, completionChildID, 0)
		if err != nil {
			t.Fatalf("member admission: %v", err)
		}
		cC.mu.Lock()
		cC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
		cC.mu.Unlock()
		return h, store, cC, member
	}

	t.Run("abandons on the failed settlement identity", func(t *testing.T) {
		h, store, cC, member := newRunningChild(t)
		h.childCompletionSettled(cC, childFixtureOp)
		waitMemberDone(t, member)
		cC.mu.Lock()
		pending := cC.pendingCompletion
		cC.mu.Unlock()
		if pending != nil {
			t.Fatalf("the abandoned obligation survived")
		}
		if n := storedEntryCount(store, completionParentID); n != 0 {
			t.Fatalf("abandonment wrote %d durable entries, want none", n)
		}
	})

	t.Run("defers on an empty settled identity", func(t *testing.T) {
		h, _, cC, member := newRunningChild(t)
		h.childCompletionSettled(cC, "")
		select {
		case <-member.done:
			t.Fatalf("the deferred obligation finished the member")
		default:
		}
		cC.mu.Lock()
		pending := cC.pendingCompletion != nil
		cC.mu.Unlock()
		if !pending {
			t.Fatalf("the deferred obligation was taken")
		}
	})
}

// TestChildCompletionRendersParentFacingText proves the rendered completion
// per settlement status: the last assistant text of the settled Operation,
// the empty-success fallback, the interruption text, and the failure detail —
// each delivered to the idle parent under the completion identity.
func TestChildCompletionRendersParentFacingText(t *testing.T) {
	cases := []struct {
		name          string
		status        OperationState
		detail        string
		assistantText string
		want          string
	}{
		{"success assistant text", OperationSuccess, "", "done", "done"},
		{"empty success fallback", OperationSuccess, "", "", "Task completed."},
		{"interruption text", OperationInterruption, "stopped", "", "Task interrupted."},
		{"failure detail", OperationFailure, "boom", "", "Task failed: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := completionChildGraph(completionParentID, false, tc.status, tc.detail, tc.assistantText).storage(t)
			addRootSession(t, store, completionParentID)
			script := newModelScript()
			h, cancel := newCancelableHarness(t, store, modelPrepared(script.model), nil)
			defer cancel()
			watch := watchSettlements(store)
			cC, err := h.coordinatorFor(context.Background(), completionChildID)
			if err != nil {
				t.Fatalf("child coordinator: %v", err)
			}
			cP, err := h.coordinatorFor(context.Background(), completionParentID)
			if err != nil {
				t.Fatalf("parent coordinator: %v", err)
			}
			member, err := admitLocked(h, cP, memberChild, completionChildID, 0)
			if err != nil {
				t.Fatalf("member admission: %v", err)
			}
			cC.mu.Lock()
			cC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: 1024}
			cC.mu.Unlock()

			h.childCompletionSettled(cC, "")
			waitMemberDone(t, member)
			watch.next() // the parent completion's settlement committed
			if rec := settledOperation(t, store, completionParentID, member.completionID); rec.State.Status != OperationSuccess {
				t.Fatalf("parent completion settled %q, want success", rec.State.Status)
			}
			if got := runtimeInputText(t, store, completionParentID); got != tc.want {
				t.Fatalf("rendered completion = %q, want %q", got, tc.want)
			}
			cC.mu.Lock()
			pending := cC.pendingCompletion
			cC.mu.Unlock()
			if pending != nil {
				t.Fatalf("the delivered obligation survived")
			}
		})
	}
}

// TestChildCompletionTruncatesAtOutputLimit proves the rendered completion is
// capped at the output limit in UTF-8 bytes, retaining the longest
// complete-character prefix that fits the trailing marker.
func TestChildCompletionTruncatesAtOutputLimit(t *testing.T) {
	text := strings.Repeat("é", 700) // 1400 UTF-8 bytes
	cases := []struct {
		name  string
		limit int
		want  string
	}{
		{"even byte boundary", 1000, strings.Repeat("é", 494) + "\n[truncated]"},
		{"backed off to a complete character", 999, strings.Repeat("é", 493) + "\n[truncated]"},
		{"under the bound", 10000, text},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := completionChildGraph(completionParentID, false, OperationSuccess, "", text).storage(t)
			addRootSession(t, store, completionParentID)
			h := newTestHarness(t, store, nil)
			cC, err := h.coordinatorFor(context.Background(), completionChildID)
			if err != nil {
				t.Fatalf("child coordinator: %v", err)
			}
			cP, err := h.coordinatorFor(context.Background(), completionParentID)
			if err != nil {
				t.Fatalf("parent coordinator: %v", err)
			}
			member, err := admitLocked(h, cP, memberChild, completionChildID, 0)
			if err != nil {
				t.Fatalf("member admission: %v", err)
			}
			closeBackgroundMode(cP) // the closed path records the rendered content verbatim
			cC.mu.Lock()
			cC.pendingCompletion = &launchInfo{completionID: member.completionID, outputLimit: tc.limit}
			cC.mu.Unlock()

			h.childCompletionSettled(cC, "")
			waitMemberDone(t, member)
			signals := storedSignals(t, store, completionParentID)
			if len(signals) != 1 {
				t.Fatalf("durable signals = %d, want the closed-path record", len(signals))
			}
			if got := signals[0].Content; got != tc.want {
				t.Fatalf("truncated content = %q, want %q", got, tc.want)
			}
			if len(signals[0].Content) > tc.limit {
				t.Fatalf("truncated content exceeds the limit: %d > %d", len(signals[0].Content), tc.limit)
			}
		})
	}
}
