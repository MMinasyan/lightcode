// Contract coverage for manual compaction admission: the compact request
// kind's existing-resolution and idle-guard rules across the shared admission
// paths, the compact admission's entry-less wire and graph shapes, and the
// recovery path over the kind-specific invariant.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// insertForeignCompactAdmission publishes one complete compact admission of
// the given identity directly into storage, as a concurrent winner would.
func insertForeignCompactAdmission(t *testing.T, tx Transaction, sessionID, operationID string) error {
	t.Helper()
	sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(sessionKey)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return err
	}
	_, _, _, err = produceAdmission(tx, current, testCapture(), admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Kind:        RequestKindCompact,
	})
	return err
}

// compactOperationRecord returns the valid running compact Operation record:
// the shared valid shape with the compact kind and no admitted input entry.
func compactOperationRecord(operationID string) OperationRecord {
	op := validOperationRecord()
	op.Admission.OperationID = operationID
	op.Admission.RequestKind = RequestKindCompact
	op.Admission.AdmittedEntry = EntryRef{}
	return op
}

// TestCompactAdmissionKindResolution proves the same-ID resolution rules: a
// compact retry resolves the first compact Operation without new work, every
// message-kind producer rejects a compact identity at its own existing
// path, and Compact rejects an existing message identity at its own
// resolution path — which precedes the idle guard.
func TestCompactAdmissionKindResolution(t *testing.T) {
	t.Run("compact resolves its own existing identity without new work", func(t *testing.T) {
		store := freshSessionStore(t)
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return insertForeignCompactAdmission(t, tx, testSessionID, "cx-1")
			})
			testassert.NoError(t, err)
		}
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		beforeEntries, beforeRegs := storedSessionState(store, testSessionID)
		rec, err := h.Compact(context.Background(), CompactRequest{SessionID: testSessionID, OperationID: "cx-1"})
		testassert.NoError(t, err)
		testassert.Require(t, rec.Admission.RequestKind == RequestKindCompact && rec.Admission.AdmittedEntry == (EntryRef{}) && rec.State.Status == OperationRunning, "resolved record = %+v, want the existing compact admission; resolved state = %+v, want the running first Operation", rec.Admission, rec.State)
		{
			got := stub.callCount()
			testassert.Equal(t, got, 0)
		}
		afterEntries, afterRegs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(afterEntries) == len(beforeEntries) && len(afterRegs) == len(beforeRegs), "retry published entries %v registers %v, want the unchanged %v %v", afterEntries, afterRegs, beforeEntries, beforeRegs)
	})

	t.Run("compact rejects an existing message identity before the idle check", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("x"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		_, err := h.Compact(context.Background(), CompactRequest{SessionID: testSessionID, OperationID: testOpID})
		testassert.Require(t, errors.Is(err, ErrInvalid) && !strings.Contains(err.Error(), "is not idle") && strings.Contains(err.Error(), "different request kind"), "compact reuse of a message identity = %v, want ErrInvalid; the resolution kind check must precede the idle guard: %v; error %v, want the request-kind rejection text", err, err, err)
	})

	t.Run("message admission rejects a compact identity at the reserved path", func(t *testing.T) {
		store := freshSessionStore(t)
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return insertForeignCompactAdmission(t, tx, testSessionID, "cx-1")
			})
			testassert.NoError(t, err)
		}
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		c := leaseCoordinator(t, h, testSessionID)
		release, err := h.reserve(context.Background(), c)
		testassert.NoError(t, err)
		defer release()
		_, _, _, err = h.admitReserved(context.Background(), c, admissionRequest{Kind: RequestKindMessage, SessionID: testSessionID, OperationID: "cx-1", Origin: InputOriginUser, Content: admissionContent("x")})
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "different request kind"), "reserved reuse = %v, want the request-kind rejection", err)
	})

	t.Run("submit rejects a compact identity at its pre-routing check", func(t *testing.T) {
		store := freshSessionStore(t)
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return insertForeignCompactAdmission(t, tx, testSessionID, "cx-1")
			})
			testassert.NoError(t, err)
		}
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: "cx-1", Origin: InputOriginUser, Content: admissionContent("x"), Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "different request kind"), "submit reuse = %v, want the request-kind rejection", err)
	})
}

// TestCompactAdmissionConflictKindCheck proves the transactional conflict
// paths carry the request-kind check: a raced loser whose identity a foreign
// winner of the other kind claimed is rejected, and a same-kind foreign
// winner resolves as the first Operation.
func TestCompactAdmissionConflictKindCheck(t *testing.T) {
	race := func(t *testing.T, kind RequestKind, foreign func(tx Transaction) error, wantExisting bool) *OperationRecord {
		t.Helper()
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)
		c := leaseCoordinator(t, h, testSessionID)
		release, err := h.reserve(context.Background(), c)
		testassert.NoError(t, err)
		defer release()
		type outcome struct {
			rec *OperationRecord
			err error
		}
		out := make(chan outcome, 1)
		go func() {
			rec, _, _, err := h.admitReserved(context.Background(), c, admissionRequest{
				SessionID:   testSessionID,
				OperationID: testOpID,
				Kind:        kind,
				Origin:      InputOriginUser,
				Content:     admissionContent("raced"),
			})
			if err == nil {
				out <- outcome{rec: &rec}
				return
			}
			out <- outcome{err: err}
		}()
		<-stub.arrived
		{
			err := store.Transact(context.Background(), foreign)
			testassert.NoError(t, err)
		}
		close(gate)
		res := <-out
		if wantExisting {
			testassert.Require(t, res.err == nil && res.rec != nil, "raced admission = (%+v, %v), want the foreign winner's record", res.rec, res.err)
		} else if !errors.Is(res.err, ErrInvalid) || !strings.Contains(res.err.Error(), "different request kind") {
			t.Fatalf("raced admission error = %v, want the request-kind rejection", res.err)
		}
		return res.rec
	}

	t.Run("raced message admission against a foreign compact winner is invalid", func(t *testing.T) {
		race(t, RequestKindMessage, func(tx Transaction) error {
			return insertForeignCompactAdmission(t, tx, testSessionID, testOpID)
		}, false)
	})

	t.Run("raced compact admission against a foreign message winner is invalid", func(t *testing.T) {
		race(t, RequestKindCompact, func(tx Transaction) error {
			return insertForeignAdmission(t, tx, testSessionID, testOpID)
		}, false)
	})

	t.Run("raced compact admission resolves the same-kind foreign winner", func(t *testing.T) {
		rec := race(t, RequestKindCompact, func(tx Transaction) error {
			return insertForeignCompactAdmission(t, tx, testSessionID, testOpID)
		}, true)
		testassert.Require(t, rec.Admission.RequestKind == RequestKindCompact, "resolved record = %+v, want the compact winner", rec.Admission)
	})
}

// tamperOperationAdmission rewrites one Operation register's admission
// member directly in storage, as a corrupting writer would.
func tamperOperationAdmission(t *testing.T, store *graphStorage, sessionID, operationID string, mutate func(admission map[string]json.RawMessage)) {
	t.Helper()
	key := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
	reg, err := store.ReadRegister(context.Background(), key)
	testassert.NoError(t, err)
	payload := map[string]json.RawMessage{}
	{
		err := json.Unmarshal(reg.Payload, &payload)
		testassert.NoError(t, err)
	}
	admission := map[string]json.RawMessage{}
	{
		err := json.Unmarshal(payload["admission"], &admission)
		testassert.NoError(t, err)
	}
	mutate(admission)
	mutatedAdmission, err := json.Marshal(admission)
	testassert.NoError(t, err)
	mutated, err := json.Marshal(map[string]json.RawMessage{"admission": mutatedAdmission, "state": payload["state"]})
	testassert.NoError(t, err)
	{
		err := store.Transact(context.Background(), func(tx Transaction) error {
			_, err := tx.ReplaceRegister(key, reg.Revision, mutated)
			return err
		})
		testassert.NoError(t, err)
	}
}

// forkLineageCompactGraph returns a valid Session whose register lineage
// matches a Fork request's source and boundary identities and whose compact
// Operation holds the identity that fork reuses: only the request-kind arm
// of Fork's existing-resolution check can reject it.
func forkLineageCompactGraph() *testGraph {
	session := validSessionRecord()
	session.Identity.SourceSessionID = testSessionID
	session.Identity.SourceBoundaryEntryID = hexID(1)
	session.State.CurrentOperationID = "cx-1"
	return &testGraph{session: session, ops: []OperationRecord{compactOperationRecord("cx-1")}}
}

// TestCompactAdmissionForkKindArm proves the Fork half of the
// request-kind-specific invariant: a compact Operation's identity reaching
// Fork's existing resolution — through a same-lineage register, so only the
// request-kind arm can fire — is rejected before any preparation, and no
// destination is prepared or published.
func TestCompactAdmissionForkKindArm(t *testing.T) {
	store := forkLineageCompactGraph().storage(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)
	_, err := h.Fork(context.Background(), ForkRequest{
		SourceSessionID: testSessionID,
		BoundaryEntryID: hexID(1),
		OperationID:     "cx-1",
		Content:         admissionContent("fork input"),
	})
	testassert.Require(t, errors.Is(err, ErrInvalid) && strings.Contains(err.Error(), "different fork lineage or request kind"), "fork over a compact identity = %v, want the lineage-or-kind rejection", err)
	{
		got := stub.callCount()
		testassert.Equal(t, got, 0)
	}
	ids, err := store.ListSessionIDs(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, len(ids) == 1, "sessions after the rejected fork = %v, want only the seeded one", ids)
	entries, regs := storedSessionState(store, testSessionID)
	testassert.Require(t, len(entries) == 0 && len(regs) == 2, "rejected fork published entries %v registers %v, want the unchanged fixture", entries, regs)
}

// TestCompactOperationGraphInvariant proves the request-kind-specific
// admitted-input rule and its preservation through recovery: a compact
// Operation validates with no admitted input, a compact Operation carrying
// one corrupts, a message Operation without its admitted input corrupts, and
// recovery settles a running compact Operation without breaking the shape.
func TestCompactOperationGraphInvariant(t *testing.T) {
	validCompactGraph := func() *testGraph {
		session := validSessionRecord()
		session.State.CurrentOperationID = "cx-1"
		op := compactOperationRecord("cx-1")
		return &testGraph{session: session, ops: []OperationRecord{op}}
	}

	t.Run("compact operation without an admitted input validates", func(t *testing.T) {
		store := validCompactGraph().storage(t)
		{
			_, err := validateFixture(t, store, testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("compact operation with an admitted input corrupts", func(t *testing.T) {
		session := validSessionRecord()
		session.State.CurrentOperationID = "cx-1"
		input := validInputEntry("cx-1")
		input.EntryID = hexID(1)
		fixture := &testGraph{
			session: session,
			ops:     []OperationRecord{compactOperationRecord("cx-1")},
			entries: []testEntry{
				{env: Entry{SessionID: testSessionID, ID: hexID(1), OperationID: "cx-1", Kind: EntryInput, Sequence: 1, CommittedAt: testTime}, input: &input},
			},
		}
		store := fixture.storage(t)
		tamperOperationAdmission(t, store, testSessionID, "cx-1", func(admission map[string]json.RawMessage) {
			admission["admitted_entry"] = json.RawMessage(`{"session_id":"` + testSessionID + `","entry_id":"` + hexID(1) + `"}`)
		})
		{
			_, err := validateFixture(t, store, testSessionID)
			testassert.Require(t, isCorruption(err), "compact graph with an admitted entry = %v, want corruption", err)
		}
	})

	t.Run("message operation without its admitted input corrupts", func(t *testing.T) {
		store := validTestGraph().storage(t)
		tamperOperationAdmission(t, store, testSessionID, testOpID, func(admission map[string]json.RawMessage) {
			delete(admission, "admitted_entry")
		})
		{
			_, err := validateFixture(t, store, testSessionID)
			testassert.Require(t, isCorruption(err), "message graph without its admitted input = %v, want corruption", err)
		}
	})

	t.Run("recovery settles a running compact Operation and preserves the invariant", func(t *testing.T) {
		store := validCompactGraph().storage(t)
		{
			err := Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: "cx-1"})
		testassert.NoError(t, err)
		op, err := decodeOperationRegister(reg)
		testassert.NoError(t, err)
		testassert.Require(t, op.State.Status == OperationInterruption && op.State.Terminal != nil, "recovered state = %+v, want the interruption terminal", op.State)
		{
			_, err := validateFixture(t, store, testSessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("recovery leaves a corrupt compact Operation unchanged", func(t *testing.T) {
		store := validCompactGraph().storage(t)
		tamperOperationAdmission(t, store, testSessionID, "cx-1", func(admission map[string]json.RawMessage) {
			admission["admitted_entry"] = json.RawMessage(`{"session_id":"` + testSessionID + `","entry_id":"` + hexID(1) + `"}`)
		})
		{
			err := Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		{
			_, err := validateFixture(t, store, testSessionID)
			testassert.Require(t, isCorruption(err), "post-recovery graph = %v, want the preserved corruption", err)
		}
	})
}

// TestCompactAdmissionCodecRules proves the compact admission's wire shape:
// the admitted-entry member is omitted and must stay absent, the message
// kind keeps its required reference, and unknown kinds reject.
func TestCompactAdmissionCodecRules(t *testing.T) {
	compact := compactOperationRecord(testOpID)
	raw, err := encodeOperationRegister(compact)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(raw), `"admitted_entry"`), "compact admission carries the admitted_entry member: %s", raw)
	decoded, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: raw})
	testassert.NoError(t, err)
	testassert.Require(t, decoded.Admission.RequestKind == RequestKindCompact && decoded.Admission.AdmittedEntry == (EntryRef{}), "decoded admission = %+v, want the compact kind with no admitted entry", decoded.Admission)

	// A compact payload carrying the member rejects.
	admission := wireObject(t, raw)["admission"]
	mutatedAdmission := setKey(admission, "admitted_entry", json.RawMessage(`{"session_id":"`+testSessionID+`","entry_id":"`+hexID(1)+`"}`))
	mutated := setKey(raw, "admission", mutatedAdmission)

	// A message payload without the member rejects.
	messageRaw, err := encodeOperationRegister(validOperationRecord())
	testassert.NoError(t, err)
	messageless := setKey(messageRaw, "admission", deleteKey(t, wireObject(t, messageRaw)["admission"], "admitted_entry"))

	// An unknown kind rejects on both directions.
	unknown := compactOperationRecord(testOpID)
	unknown.Admission.RequestKind = RequestKind("bogus")
	{
		_, err := encodeOperationRegister(unknown)
		testassert.Require(t, err != nil, "unknown request kind encoded")
	}
	unknownWire := setKey(raw, "admission", setKey(admission, "request_kind", json.RawMessage(`"bogus"`)))

	// The raw payloads above bypass the production encoder: each names one
	// invalid persisted state the decoder must refuse.
	rejections := []struct {
		name     string
		payload  json.RawMessage
		wantText string
	}{
		{"compact payload carrying the admitted entry", mutated, "admitted_entry"},
		{"message payload without the admitted entry", messageless, ""},
		{"unknown request kind", unknownWire, ""},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOperationRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: tc.payload})
			testassert.Require(t, err != nil, "%s decoded", tc.name)
			testassert.Require(t, tc.wantText == "" || strings.Contains(err.Error(), tc.wantText), "error %v, want the %s member named", err, tc.wantText)
		})
	}
}

// deleteKey returns one payload object without the named member.
func deleteKey(t *testing.T, raw json.RawMessage, key string) json.RawMessage {
	t.Helper()
	obj := map[string]json.RawMessage{}
	{
		err := json.Unmarshal(raw, &obj)
		testassert.NoError(t, err)
	}
	delete(obj, key)
	out, err := json.Marshal(obj)
	testassert.NoError(t, err)
	return out
}
