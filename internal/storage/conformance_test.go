package storage

// The shared storage conformance suite runs unchanged against every
// implementation of the public harness storage contract. Each case tests
// public behavior through the harness.Storage and harness.Transaction
// interfaces only; backend-private fixtures are supplied by the wiring test
// file of each backend to inject durable state that no caller could create
// through the public API.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// conformanceBackend supplies one storage implementation to the shared suite.
// reopen, when non-nil, closes and durably reopens the store; backends without
// a reopenable state leave it nil. The corruption and exhaustion fixtures
// mutate the backend's persisted state directly so the shared cases can assert
// the same public outcomes for state no caller could stage through the API.
type conformanceBackend struct {
	name            string
	newStore        func(t *testing.T) harness.Storage
	reopen          func(t *testing.T, store harness.Storage) harness.Storage
	corruptEntry    func(t *testing.T, store harness.Storage, sessionID string)
	corruptRegister func(t *testing.T, store harness.Storage, sessionID string)
	orphanEntry     func(t *testing.T, store harness.Storage, sessionID string)
	// orphanOperationRegister persists one structurally valid operation
	// register for sessionID without that session's register.
	orphanOperationRegister func(t *testing.T, store harness.Storage, sessionID string)
	// malformedSessionRegister persists one session-register envelope carrying
	// an operation identity for sessionID, which is not a valid parent.
	malformedSessionRegister func(t *testing.T, store harness.Storage, sessionID string)
	exhaustSequence          func(t *testing.T, store harness.Storage, sessionID string)
	exhaustRevision          func(t *testing.T, store harness.Storage, sessionID string)
}

func runConformance(t *testing.T, b conformanceBackend) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, conformanceBackend)
	}{
		{"envelope validity", confEnvelopeValidity},
		{"payload ownership", confPayloadOwnership},
		{"entry identity and order", confEntryIdentityOrder},
		{"register mechanics", confRegisterMechanics},
		{"structural ownership", confStructuralOwnership},
		{"structural orphan corruption", confStructuralOrphan},
		{"malformed session register", confMalformedSessionRegister},
		{"transaction read-your-writes", confTransactionReadYourWrites},
		{"transaction rollback", confTransactionRollback},
		{"transaction ignored conflict", confTransactionIgnoredConflict},
		{"transaction panic", confTransactionPanic},
		{"transaction context", confTransactionContext},
		{"transaction queued cancellation", confTransactionQueuedCancellation},
		{"transaction outside-reader barrier", confTransactionBarrier},
		{"retained transaction", confRetainedTransaction},
		{"multi-session transaction", confMultiSessionTransaction},
		{"revision conflict aborts transaction", confRevisionConflict},
		{"recovery reads", confRecoveryReads},
		{"sequence exhaustion", confSequenceExhaustion},
		{"revision exhaustion", confRevisionExhaustion},
		{"corruption result", confCorruption},
		{"compaction entry payload round-trip", confCompactionPayloadRoundTrip},
		{"session deletion", confSessionDeletion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, b)
		})
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

func confSessionKey(sessionID string) harness.RegisterKey {
	return harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
}

func confOpKey(sessionID, opID string) harness.RegisterKey {
	return harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: opID}
}

func confSessionPayload(sessionID string) json.RawMessage {
	return rawJSON(fmt.Sprintf(`{"session":%q}`, sessionID))
}

func confEntry(sessionID, id string) harness.EntryDraft {
	return harness.EntryDraft{
		SessionID: sessionID,
		ID:        id,
		Kind:      harness.EntryInput,
		Payload:   rawJSON(fmt.Sprintf(`{"entry":%q}`, id)),
	}
}

func confOpRegister(sessionID, opID string) harness.RegisterDraft {
	return harness.RegisterDraft{
		Key:     confOpKey(sessionID, opID),
		Payload: rawJSON(fmt.Sprintf(`{"operation":%q}`, opID)),
	}
}

func confTxn(t *testing.T, ctx context.Context, store harness.Storage, fn func(txn harness.Transaction) error) {
	t.Helper()
	testassert.NoError(t, store.Transact(ctx, fn))
}

func confCreateSession(t *testing.T, ctx context.Context, store harness.Storage, sessionID string) {
	t.Helper()
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertRegister(harness.RegisterDraft{Key: confSessionKey(sessionID), Payload: confSessionPayload(sessionID)})
		return err
	})
}

func confPayloadEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	testassert.Require(t, string(got) == want, "payload = %s, want %s", got, want)
}

func confWantInvalid(t *testing.T, name string, err error) {
	t.Helper()
	testassert.Check(t, errors.Is(err, harness.ErrInvalid), "%s: error = %v, want ErrInvalid", name, err)
}

// confEnvelopeValidity covers the caller-side envelope rules: closed kinds,
// required conditional identities, syntactically valid JSON payloads, negative
// read boundaries, and not-found for valid keys addressing nothing.
func confEnvelopeValidity(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	invalidEntries := map[string]harness.EntryDraft{
		"empty session": {SessionID: "", ID: "e1", Kind: harness.EntryInput, Payload: rawJSON(`{}`)},
		"empty id":      {SessionID: "s1", ID: "", Kind: harness.EntryInput, Payload: rawJSON(`{}`)},
		"empty kind":    {SessionID: "s1", ID: "e1", Kind: "", Payload: rawJSON(`{}`)},
		"unknown kind":  {SessionID: "s1", ID: "e1", Kind: "bogus", Payload: rawJSON(`{}`)},
		"nil payload":   {SessionID: "s1", ID: "e1", Kind: harness.EntryInput},
		"bad payload":   {SessionID: "s1", ID: "e1", Kind: harness.EntryInput, Payload: rawJSON(`{"n":`)},
	}
	for name, draft := range invalidEntries {
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(draft)
			return err
		})
		confWantInvalid(t, "InsertEntry "+name, err)
	}
	{
		entries, err := store.ReadEntries(ctx, "s1", 0)
		testassert.Require(t, err == nil && len(entries) == 0, "rejected drafts persisted: %d entries, error %v", len(entries), err)
	}

	validPayloads := []string{`{}`, `null`, `42`, `"text"`, `[1,2,{"k":"v"}]`}
	for i, payload := range validPayloads {
		draft := harness.EntryDraft{SessionID: "s1", ID: fmt.Sprintf("v%d", i), Kind: harness.EntryInput, Payload: rawJSON(payload)}
		confTxn(t, ctx, store, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(draft)
			return err
		})
	}
	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	testassert.Equal(t, len(entries), len(validPayloads))
	for i, entry := range entries {
		testassert.Check(t, entry.Sequence == int64(i+1), "entry %s sequence = %d, want %d", entry.ID, entry.Sequence, i+1)
		confPayloadEqual(t, entry.Payload, validPayloads[i])
	}

	opEntry := confEntry("s1", "op-entry")
	opEntry.OperationID = "op-1"
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(opEntry)
		return err
	})
	entries, err = store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	{
		got := entries[len(entries)-1]
		testassert.Check(t, got.ID == "op-entry" && got.OperationID == "op-1", "optional operation identity not round-tripped: %+v", got)
	}

	invalidRegisters := map[string]harness.RegisterDraft{
		"empty session":                       {Key: harness.RegisterKey{SessionID: "", Kind: harness.RegisterSession}, Payload: rawJSON(`{}`)},
		"session kind with operation id":      {Key: harness.RegisterKey{SessionID: "s2", Kind: harness.RegisterSession, OperationID: "op1"}, Payload: rawJSON(`{}`)},
		"operation kind without operation id": {Key: harness.RegisterKey{SessionID: "s2", Kind: harness.RegisterOperation}, Payload: rawJSON(`{}`)},
		"unknown kind":                        {Key: harness.RegisterKey{SessionID: "s2", Kind: "bogus", OperationID: "op1"}, Payload: rawJSON(`{}`)},
		"bad payload":                         {Key: harness.RegisterKey{SessionID: "s2", Kind: harness.RegisterSession}, Payload: rawJSON(`{`)},
	}
	for name, draft := range invalidRegisters {
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(draft)
			return err
		})
		confWantInvalid(t, "InsertRegister "+name, err)
	}
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertRegister(harness.RegisterDraft{Key: confSessionKey("s2"), Payload: confSessionPayload("s2")})
		return err
	})
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertRegister(confOpRegister("s2", "op1"))
		return err
	})

	invalidKeys := map[string]harness.RegisterKey{
		"empty session":                       {Kind: harness.RegisterSession},
		"operation kind without operation id": {SessionID: "s1", Kind: harness.RegisterOperation},
		"unknown kind":                        {SessionID: "s1", Kind: "bogus", OperationID: "op1"},
		"session kind with operation id":      {SessionID: "s1", Kind: harness.RegisterSession, OperationID: "x"},
	}
	for name, key := range invalidKeys {
		_, err := store.ReadRegister(ctx, key)
		confWantInvalid(t, "ReadRegister "+name, err)
	}
	{
		_, err := store.ReadRegister(ctx, confSessionKey("missing"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "ReadRegister missing session: error = %v, want ErrNotFound", err)
	}

	for _, revision := range []int64{0, -1} {
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.ReplaceRegister(confSessionKey("s1"), revision, rawJSON(`{}`))
			return err
		})
		confWantInvalid(t, "ReplaceRegister non-positive revision", err)
	}
	err = store.Transact(ctx, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(confSessionKey("s1"), 1, rawJSON(`{`))
		return err
	})
	confWantInvalid(t, "ReplaceRegister bad payload", err)

	{
		_, err := store.ReadEntries(ctx, "s1", -1)
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "ReadEntries negative boundary: error = %v, want ErrInvalid", err)
	}
	{
		_, err := store.ReadEntries(ctx, "", 0)
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "ReadEntries empty session: error = %v, want ErrInvalid", err)
	}
	{
		_, err := store.ReadRegisters(ctx, "")
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "ReadRegisters empty session: error = %v, want ErrInvalid", err)
	}
}

// confPayloadOwnership proves every returned value owns its payload bytes:
// caller mutation of drafts, returned values, and bytes passed to replacement
// can never change stored state or another returned value.
func confPayloadOwnership(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	draftPayload := rawJSON(`{"n":1}`)
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(harness.EntryDraft{SessionID: "s1", ID: "e1", Kind: harness.EntryInput, Payload: draftPayload})
		return err
	})
	draftPayload[5] = '2' // mutating the caller's bytes must not reach the store
	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	confPayloadEqual(t, entries[0].Payload, `{"n":1}`)

	registerPayload := rawJSON(`{"v":1}`)
	opDraft := confOpRegister("s1", "op1")
	opDraft.Payload = registerPayload
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertRegister(opDraft)
		return err
	})
	registerPayload[5] = '9'
	register, err := store.ReadRegister(ctx, confOpKey("s1", "op1"))
	testassert.NoError(t, err)
	confPayloadEqual(t, register.Payload, `{"v":1}`)

	replacePayload := rawJSON(`{"rev":2}`)
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(confSessionKey("s1"), 1, replacePayload)
		return err
	})
	replacePayload[7] = '9'
	register, err = store.ReadRegister(ctx, confSessionKey("s1"))
	testassert.NoError(t, err)
	confPayloadEqual(t, register.Payload, `{"rev":2}`)

	first, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	first[0].Payload[5] = '7'
	second, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	confPayloadEqual(t, second[0].Payload, `{"n":1}`)

	g1, err := store.ReadRegister(ctx, confSessionKey("s1"))
	testassert.NoError(t, err)
	g1.Payload[7] = '8'
	g2, err := store.ReadRegister(ctx, confSessionKey("s1"))
	testassert.NoError(t, err)
	confPayloadEqual(t, g2.Payload, `{"rev":2}`)
}

// confEntryIdentityOrder covers immutable unique entry identity, per-Session
// sequence assignment across transactions, and ordered reads with the strict
// after boundary.
func confEntryIdentityOrder(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confCreateSession(t, ctx, store, "s2")

	var firstSequence, secondSequence int64
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		e1, err := txn.InsertEntry(confEntry("s1", "e1"))
		if err != nil {
			return err
		}
		e2, err := txn.InsertEntry(confEntry("s1", "e2"))
		if err != nil {
			return err
		}
		firstSequence, secondSequence = e1.Sequence, e2.Sequence
		return nil
	})
	testassert.Require(t, firstSequence == 1 && secondSequence == 2, "first transaction sequences = %d, %d; want 1, 2", firstSequence, secondSequence)

	var s2Sequence, s1Next int64
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		e3, err := txn.InsertEntry(confEntry("s2", "e3"))
		if err != nil {
			return err
		}
		e4, err := txn.InsertEntry(confEntry("s1", "e4"))
		if err != nil {
			return err
		}
		s2Sequence, s1Next = e3.Sequence, e4.Sequence
		return nil
	})
	testassert.Check(t, s2Sequence == 1, "second session first sequence = %d, want 1 (per-session counters are independent)", s2Sequence)
	testassert.Check(t, s1Next == 3, "first session next sequence = %d, want 3", s1Next)

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	wantIDs := []string{"e1", "e2", "e4"}
	for i, entry := range entries {
		testassert.Check(t, entry.ID == wantIDs[i] && entry.Sequence == int64(i+1), "entry %d = {%s, seq %d}, want {%s, seq %d}", i, entry.ID, entry.Sequence, wantIDs[i], i+1)
		testassert.Check(t, entry.CommittedAt.Location() == time.UTC, "entry %s commit time %v is not UTC", entry.ID, entry.CommittedAt)
	}
	{
		tail, err := store.ReadEntries(ctx, "s1", 2)
		testassert.Check(t, err == nil && len(tail) == 1 && tail[0].ID == "e4", "ReadEntries after 2 = %v (error %v), want [e4]", tail, err)
	}
	{
		rest, err := store.ReadEntries(ctx, "s1", 3)
		testassert.Check(t, err == nil && len(rest) == 0, "ReadEntries after last sequence = %v (error %v), want empty", rest, err)
	}
	{
		s2Entries, err := store.ReadEntries(ctx, "s2", 0)
		testassert.Check(t, err == nil && len(s2Entries) == 1 && s2Entries[0].ID == "e3", "second session entries = %v (error %v), want [e3]", s2Entries, err)
	}

	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(confEntry("s1", "e1"))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrConflict), "duplicate entry id in one session: error = %v, want ErrConflict", err)
	}
	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(confEntry("s2", "e1"))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrConflict), "duplicate entry id across sessions: error = %v, want ErrConflict", err)
	}
	{
		entries, err := store.ReadEntries(ctx, "s1", 0)
		testassert.Check(t, err == nil && len(entries) == 3, "conflicted transactions changed entries: %d entries (error %v)", len(entries), err)
	}
}

// confRegisterMechanics covers absent insertion at revision 1, whole-payload
// replacement at the exact revision, stale and duplicate conflicts, the
// unchanged-state oracle, and cross-Session operation identity uniqueness.
func confRegisterMechanics(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	key := confSessionKey("s1")

	register, err := store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	testassert.Require(t, register.Revision == 1 && register.Key == key, "fresh session register = revision %d key %+v, want revision 1 key %+v", register.Revision, register.Key, key)
	confPayloadEqual(t, register.Payload, `{"session":"s1"}`)

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(key, 1, rawJSON(`{"state":"replaced","extra":[1,2]}`))
		return err
	})
	register, err = store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	testassert.Check(t, register.Revision == 2, "replacement revision = %d, want 2", register.Revision)
	confPayloadEqual(t, register.Payload, `{"state":"replaced","extra":[1,2]}`)

	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.ReplaceRegister(key, 1, rawJSON(`{"stale":true}`))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrConflict), "stale replacement: error = %v, want ErrConflict", err)
	}
	register, err = store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	testassert.Check(t, register.Revision == 2 && string(register.Payload) == `{"state":"replaced","extra":[1,2]}`, "conflicted replacement changed state: revision %d payload %s", register.Revision, register.Payload)

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(key, 2, rawJSON(`{"state":"replaced","extra":[1,2]}`))
		return err
	})
	register, err = store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	testassert.Check(t, register.Revision == 3, "unchanged-payload replacement revision = %d, want 3", register.Revision)

	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(harness.RegisterDraft{Key: key, Payload: confSessionPayload("s1")})
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrConflict), "duplicate register insertion: error = %v, want ErrConflict", err)
	}

	confCreateSession(t, ctx, store, "s2")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertRegister(confOpRegister("s1", "op1"))
		return err
	})
	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(confOpRegister("s2", "op1"))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrConflict), "duplicate operation identity across sessions: error = %v, want ErrConflict", err)
	}
	{
		_, err := store.ReadRegister(ctx, confOpKey("s2", "op1"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "conflicted operation register exists: error = %v, want ErrNotFound", err)
	}
}

// confStructuralOwnership covers same-transaction parent creation and the
// not-found outcome for dependents whose parent register was never created.
func confStructuralOwnership(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertRegister(harness.RegisterDraft{Key: confSessionKey("fork-origin"), Payload: confSessionPayload("fork-origin")}); err != nil {
			return err
		}
		if _, err := txn.InsertEntry(confEntry("fork-origin", "e1")); err != nil {
			return err
		}
		_, err := txn.InsertRegister(confOpRegister("fork-origin", "op1"))
		return err
	})
	{
		entries, err := store.ReadEntries(ctx, "fork-origin", 0)
		testassert.Require(t, err == nil && len(entries) == 1, "same-transaction dependent insertion lost: %d entries (error %v)", len(entries), err)
	}
	{
		_, err := store.ReadRegister(ctx, confOpKey("fork-origin", "op1"))
		testassert.NoError(t, err)
	}

	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(confEntry("ghost", "e1"))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "entry without session register: error = %v, want ErrNotFound", err)
	}
	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(confOpRegister("ghost", "op9"))
			return err
		})
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "operation register without session register: error = %v, want ErrNotFound", err)
	}
	{
		ids, err := store.ListSessionIDs(ctx)
		testassert.Check(t, err == nil && len(ids) == 1 && ids[0] == "fork-origin", "ListSessionIDs = %v (error %v), want [fork-origin]", ids, err)
	}
}

// confStructuralOrphan proves persisted dependents without a session register
// are typed corruption owned by that session, that discovery still lists the
// orphan, and that deletion reports the same corruption.
func confStructuralOrphan(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "keeper")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("keeper", "k1"))
		return err
	})

	b.orphanEntry(t, store, "ghost")

	for name, read := range map[string]func() error{
		"ReadEntries":    func() error { _, err := store.ReadEntries(ctx, "ghost", 0); return err },
		"ReadRegisters":  func() error { _, err := store.ReadRegisters(ctx, "ghost"); return err },
		"ReadRegister":   func() error { _, err := store.ReadRegister(ctx, confSessionKey("ghost")); return err },
		"ReadRegisterOp": func() error { _, err := store.ReadRegister(ctx, confOpKey("ghost", "op1")); return err },
	} {
		err := read()
		testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "%s on orphan session: error = %v, want ErrCorrupt", name, err)
		var corrupt *harness.CorruptionError
		testassert.Check(t, errors.As(err, &corrupt) && corrupt.SessionID == "ghost", "%s on orphan session: recovered %+v, want owning session ghost", name, corrupt)
	}

	ids, err := store.ListSessionIDs(ctx)
	testassert.Require(t, err == nil && len(ids) == 2 && ids[0] == "ghost" && ids[1] == "keeper", "ListSessionIDs = %v (error %v), want [ghost keeper]", ids, err)

	err = store.Transact(ctx, func(txn harness.Transaction) error {
		return txn.DeleteSession("ghost")
	})
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "DeleteSession on orphan session: error = %v, want ErrCorrupt", err)
	var corrupt *harness.CorruptionError
	testassert.Check(t, errors.As(err, &corrupt) && corrupt.SessionID == "ghost", "DeleteSession orphan corruption recovered %+v, want owning session ghost", corrupt)

	// Every mutation that meets the orphan state reports the same typed
	// corruption instead of treating the session as absent, and inserting the
	// missing session register cannot repair or hide the orphan.
	mutations := map[string]func(harness.Transaction) error{
		"InsertEntry": func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(confEntry("ghost", "e-orphan-insert"))
			return err
		},
		"InsertRegister session": func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(harness.RegisterDraft{Key: confSessionKey("ghost"), Payload: confSessionPayload("ghost")})
			return err
		},
		"InsertRegister operation": func(txn harness.Transaction) error {
			_, err := txn.InsertRegister(confOpRegister("ghost", "op-orphan"))
			return err
		},
		"ReplaceRegister": func(txn harness.Transaction) error {
			_, err := txn.ReplaceRegister(confSessionKey("ghost"), 1, confSessionPayload("ghost"))
			return err
		},
	}
	for name, mutate := range mutations {
		err := store.Transact(ctx, func(txn harness.Transaction) error { return mutate(txn) })
		testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "%s on orphan session: error = %v, want ErrCorrupt", name, err)
		var orphanErr *harness.CorruptionError
		testassert.Check(t, errors.As(err, &orphanErr) && orphanErr.SessionID == "ghost", "%s on orphan session recovered %+v, want owning session ghost", name, orphanErr)
	}
	{
		_, err := store.ReadRegister(ctx, confSessionKey("ghost"))
		testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "orphan session after rejected mutations: error = %v, want ErrCorrupt (the orphan must stay unrepaired)", err)
	}
	{
		ids, err := store.ListSessionIDs(ctx)
		testassert.Check(t, err == nil && len(ids) == 2 && ids[0] == "ghost" && ids[1] == "keeper", "rejected mutations changed discovery: %v (error %v)", ids, err)
	}

	{
		entries, err := store.ReadEntries(ctx, "keeper", 0)
		testassert.Check(t, err == nil && len(entries) == 1, "keeper entries = %d (error %v), want 1", len(entries), err)
	}

	if b.reopen != nil {
		store = b.reopen(t, store)
		{
			_, err := store.ReadEntries(ctx, "ghost", 0)
			testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "orphan corruption after reopen: error = %v, want ErrCorrupt", err)
		}
	}

	// An orphan operation register alone must read as the same typed
	// owning-session corruption on the entries read, not as an absent session.
	b.orphanOperationRegister(t, store, "ghost-op")
	_, err = store.ReadEntries(ctx, "ghost-op", 0)
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "orphan operation register: error = %v, want ErrCorrupt", err)
	var opOrphan *harness.CorruptionError
	testassert.Check(t, errors.As(err, &opOrphan) && opOrphan.SessionID == "ghost-op", "orphan operation register recovered %+v, want owning session ghost-op", opOrphan)

	// Replacing that exact extant orphan Operation register is the same typed
	// owning-session corruption, and the orphan stays unchanged: a second
	// replacement at the original revision is still corruption, not a
	// stale-revision conflict.
	err = store.Transact(ctx, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(confOpKey("ghost-op", "orphan-op"), 1, rawJSON(`{"operation":"replaced"}`))
		return err
	})
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "ReplaceRegister on an extant orphan Operation register: error = %v, want ErrCorrupt", err)
	testassert.Check(t, errors.As(err, &opOrphan) && opOrphan.SessionID == "ghost-op", "ReplaceRegister on an extant orphan Operation register recovered %+v, want owning session ghost-op", opOrphan)
	{
		err = store.Transact(ctx, func(txn harness.Transaction) error {
			_, err := txn.ReplaceRegister(confOpKey("ghost-op", "orphan-op"), 1, rawJSON(`{"operation":"replaced"}`))
			return err
		})
		testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "second ReplaceRegister on the unchanged orphan: error = %v, want ErrCorrupt (a consumed revision would be ErrConflict)", err)
	}
}

// confMalformedSessionRegister proves a persisted session-register envelope
// carrying an operation identity is not a valid parent: with no canonical
// session register but a register envelope present for the session, every
// session-scoped read and mutation is the typed owning-Session corruption and
// the session stays discoverable.
func confMalformedSessionRegister(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)

	b.malformedSessionRegister(t, store, "forged")

	reads := map[string]func() error{
		"ReadEntries":   func() error { _, err := store.ReadEntries(ctx, "forged", 0); return err },
		"ReadRegister":  func() error { _, err := store.ReadRegister(ctx, confSessionKey("forged")); return err },
		"ReadRegisters": func() error { _, err := store.ReadRegisters(ctx, "forged"); return err },
	}
	for name, read := range reads {
		err := read()
		testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "%s on a malformed session register: error = %v, want ErrCorrupt", name, err)
		var corrupt *harness.CorruptionError
		testassert.Check(t, errors.As(err, &corrupt) && corrupt.SessionID == "forged", "%s on a malformed session register recovered %+v, want owning session forged", name, corrupt)
	}

	err := store.Transact(ctx, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("forged", "e1"))
		return err
	})
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "InsertEntry on a malformed session register: error = %v, want ErrCorrupt", err)

	ids, err := store.ListSessionIDs(ctx)
	testassert.Check(t, err == nil && len(ids) == 1 && ids[0] == "forged", "ListSessionIDs = %v (error %v), want [forged]", ids, err)

	if b.reopen != nil {
		store = b.reopen(t, store)
		{
			_, err := store.ReadEntries(ctx, "forged", 0)
			testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "malformed session register after reopen: error = %v, want ErrCorrupt", err)
		}
	}

	// The nearest valid-parent sibling: the same malformed session-register
	// envelope beside a valid canonical Session register and a valid
	// Operation register. ReadRegisters must surface the malformed envelope
	// as owning-Session corruption instead of silently skipping it.
	confCreateSession(t, ctx, store, "valid-parent")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertRegister(confOpRegister("valid-parent", "op1")); err != nil {
			return err
		}
		return nil
	})
	validRegisters, err := store.ReadRegisters(ctx, "valid-parent")
	testassert.Require(t, err == nil && len(validRegisters) == 2 && validRegisters[0].Key.Kind == harness.RegisterSession && validRegisters[1].Key.OperationID == "op1", "valid parent ReadRegisters = %v (error %v), want the canonical Session register followed by the Operation register", validRegisters, err)
	b.malformedSessionRegister(t, store, "valid-parent")
	_, err = store.ReadRegisters(ctx, "valid-parent")
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "ReadRegisters beside a valid parent: error = %v, want ErrCorrupt", err)
	var siblingCorrupt *harness.CorruptionError
	testassert.Check(t, errors.As(err, &siblingCorrupt) && siblingCorrupt.SessionID == "valid-parent", "ReadRegisters beside a valid parent recovered %+v, want owning session valid-parent", siblingCorrupt)
}

// confTransactionReadYourWrites proves transaction reads observe the committed
// snapshot plus the callback's staged writes.
func confTransactionReadYourWrites(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
			return err
		}
		entries, err := txn.ReadEntries("s1", 0)
		if err != nil {
			return err
		}
		testassert.Check(t, len(entries) == 2 && entries[0].ID == "e1" && entries[1].ID == "e2", "transaction read saw %v, want [e1 e2]", entries)
		if _, err := txn.InsertRegister(confOpRegister("s1", "op1")); err != nil {
			return err
		}
		staged, err := txn.ReadRegister(confOpKey("s1", "op1"))
		if err != nil {
			return err
		}
		testassert.Check(t, staged.Revision == 1, "staged register revision = %d, want 1", staged.Revision)
		return nil
	})

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.Require(t, err == nil && len(entries) == 2, "committed entries = %d (error %v), want 2", len(entries), err)
	{
		_, err := store.ReadRegister(ctx, confOpKey("s1", "op1"))
		testassert.NoError(t, err)
	}
}

// confTransactionRollback proves a callback error rolls back every staged
// change without consuming a sequence, and the store stays usable afterwards.
func confTransactionRollback(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	boom := errors.New("callback boom")
	err := store.Transact(ctx, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
			return err
		}
		if _, err := txn.ReplaceRegister(confSessionKey("s1"), 1, rawJSON(`{"x":1}`)); err != nil {
			return err
		}
		if _, err := txn.InsertRegister(confOpRegister("s1", "op1")); err != nil {
			return err
		}
		return boom
	})
	testassert.Require(t, errors.Is(err, boom), "Transact error = %v, want the callback error", err)

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.Require(t, err == nil && len(entries) == 1 && entries[0].ID == "e1", "rollback left %d entries (error %v), want [e1]", len(entries), err)
	register, err := store.ReadRegister(ctx, confSessionKey("s1"))
	testassert.Require(t, err == nil && register.Revision == 1, "rollback left register revision %d (error %v), want 1", register.Revision, err)
	confPayloadEqual(t, register.Payload, `{"session":"s1"}`)
	{
		_, err := store.ReadRegister(ctx, confOpKey("s1", "op1"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "rollback left staged register: error = %v, want ErrNotFound", err)
	}

	var retrySequence int64
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		entry, err := txn.InsertEntry(confEntry("s1", "e2-retry"))
		retrySequence = entry.Sequence
		return err
	})
	testassert.Check(t, retrySequence == 2, "retry sequence = %d, want 2 (rollback must not consume a sequence)", retrySequence)
}

// confTransactionIgnoredConflict proves a mutation conflict latches failure
// for the transaction even when the callback ignores the returned error, and
// the latched conflict discards every staged change.
func confTransactionIgnoredConflict(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	err := store.Transact(ctx, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("s1", "e-staged")); err != nil {
			return err
		}
		if _, err := txn.InsertRegister(confOpRegister("s1", "dup")); err != nil {
			return err
		}
		_, err := txn.InsertRegister(confOpRegister("s1", "dup")) // conflict, ignored
		_ = err
		return nil
	})
	testassert.Require(t, errors.Is(err, harness.ErrConflict), "ignored mutation conflict: Transact error = %v, want ErrConflict", err)

	{
		entries, err := store.ReadEntries(ctx, "s1", 0)
		testassert.Check(t, err == nil && len(entries) == 0, "latched conflict committed staged entries: %d (error %v)", len(entries), err)
	}
	{
		_, err := store.ReadRegister(ctx, confOpKey("s1", "dup"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "latched conflict committed staged register: error = %v, want ErrNotFound", err)
	}
}

// confTransactionPanic proves a panicking callback rolls back every staged
// change and then propagates the panic.
func confTransactionPanic(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = store.Transact(ctx, func(txn harness.Transaction) error {
			if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	testassert.Equal(t, panicked, "boom")

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.Require(t, err == nil && len(entries) == 1 && entries[0].ID == "e1", "panic rollback left %d entries (error %v), want [e1]", len(entries), err)
	var retrySequence int64
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		entry, err := txn.InsertEntry(confEntry("s1", "e3"))
		retrySequence = entry.Sequence
		return err
	})
	testassert.Check(t, retrySequence == 2, "post-panic insert sequence = %d, want 2 (rollback must not consume a sequence)", retrySequence)
}

// confTransactionContext proves context cancellation rolls back staged writes
// and that every read and mutation path preserves the context error classes.
func confTransactionContext(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := store.Transact(canceled, func(txn harness.Transaction) error {
		called = true
		return nil
	})
	testassert.Check(t, errors.Is(err, context.Canceled), "Transact on canceled context: error = %v, want context.Canceled", err)
	testassert.Check(t, !called, "callback invoked on canceled context")

	transactionCtx, cancelTransaction := context.WithCancel(context.Background())
	err = store.Transact(transactionCtx, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
			return err
		}
		cancelTransaction()
		return nil
	})
	testassert.Check(t, errors.Is(err, context.Canceled), "pre-commit cancellation: error = %v, want context.Canceled", err)
	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.Check(t, err == nil && len(entries) == 1 && entries[0].ID == "e1", "cancellation committed staged writes: %d entries (error %v)", len(entries), err)

	reads := map[string]func() error{
		"ReadEntries":    func() error { _, err := store.ReadEntries(canceled, "s1", 0); return err },
		"ReadRegister":   func() error { _, err := store.ReadRegister(canceled, confSessionKey("s1")); return err },
		"ReadRegisters":  func() error { _, err := store.ReadRegisters(canceled, "s1"); return err },
		"ListSessionIDs": func() error { _, err := store.ListSessionIDs(canceled); return err },
		"Transact":       func() error { return store.Transact(canceled, func(harness.Transaction) error { return nil }) },
	}
	for name, read := range reads {
		{
			err := read()
			testassert.Check(t, errors.Is(err, context.Canceled), "%s on canceled context: error = %v, want context.Canceled", name, err)
		}
	}

	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	{
		_, err := store.ReadEntries(deadlineCtx, "s1", 0)
		testassert.Check(t, errors.Is(err, context.DeadlineExceeded), "ReadEntries on expired deadline: error = %v, want context.DeadlineExceeded", err)
	}
}

// confTransactionQueuedCancellation proves a transaction queued behind
// another writer observes cancellation while it waits: the context error is
// preserved and the callback never runs. The queued transaction's deadline is
// arranged to expire strictly while the writer is still held, so the wait
// itself must recheck the context.
func confTransactionQueuedCancellation(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	inside := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.Transact(ctx, func(txn harness.Transaction) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside // the first transaction holds the writer

	queuedCtx, cancelQueued := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelQueued()
	queuedCalled := false
	queued := make(chan error, 1)
	go func() {
		queued <- store.Transact(queuedCtx, func(txn harness.Transaction) error {
			queuedCalled = true
			return nil
		})
	}()

	<-queuedCtx.Done() // the deadline expires while the queued transaction still waits
	close(release)

	testassert.NoError(t, <-first)
	{
		err := <-queued
		testassert.Check(t, errors.Is(err, context.DeadlineExceeded), "queued transaction error = %v, want context.DeadlineExceeded", err)
	}
	testassert.Check(t, !queuedCalled, "queued transaction callback ran after cancellation")
}

// confTransactionBarrier proves readers outside a transaction observe either
// the complete pre-commit or the complete post-commit state, never a partial
// transition. Memory readers wait for the writer and see the post-commit
// state; backends whose reads run beside the writer, such as SQLite on
// separate pooled connections, may return the pre-commit state until the
// commit lands. The transaction stages two records so a partial observation
// would be distinguishable from either complete state.
func confTransactionBarrier(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	staged := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- store.Transact(ctx, func(txn harness.Transaction) error {
			if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
				return err
			}
			if _, err := txn.InsertEntry(confEntry("s1", "e3")); err != nil {
				return err
			}
			close(staged)
			return nil
		})
	}()
	<-staged

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	preCommit := len(entries) == 1 && entries[0].ID == "e1"
	postCommit := len(entries) == 3 && entries[0].ID == "e1" && entries[1].ID == "e2" && entries[2].ID == "e3"
	testassert.Check(t, preCommit || postCommit, "outside reader observed a partial transition %v, want exactly [e1] or [e1 e2 e3]", entries)
	testassert.NoError(t, <-committed)
	entries, err = store.ReadEntries(ctx, "s1", 0)
	testassert.Check(t, err == nil && len(entries) == 3 && entries[2].ID == "e3", "post-commit read = %v (error %v), want the complete committed transition [e1 e2 e3]", entries, err)
}

// confRetainedTransaction proves using a transaction after its callback
// returns is invalid.
func confRetainedTransaction(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	var retained harness.Transaction
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		retained = txn
		return nil
	})
	{
		_, err := retained.ReadEntries("s1", 0)
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "retained ReadEntries: error = %v, want ErrInvalid", err)
	}
	{
		_, err := retained.ReadRegister(confSessionKey("s1"))
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "retained ReadRegister: error = %v, want ErrInvalid", err)
	}
	{
		_, err := retained.InsertEntry(confEntry("s1", "e1"))
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "retained InsertEntry: error = %v, want ErrInvalid", err)
	}
}

// confMultiSessionTransaction proves one callback may read one Session and
// create another, and that a rollback discards both sides atomically.
func confMultiSessionTransaction(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "src")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		for _, id := range []string{"e1", "e2"} {
			if _, err := txn.InsertEntry(confEntry("src", id)); err != nil {
				return err
			}
		}
		return nil
	})

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		source, err := txn.ReadEntries("src", 0)
		if err != nil {
			return err
		}
		testassert.Check(t, len(source) == 2, "transaction read of source session saw %d entries, want 2", len(source))
		if _, err := txn.InsertRegister(harness.RegisterDraft{Key: confSessionKey("dst"), Payload: confSessionPayload("dst")}); err != nil {
			return err
		}
		_, err = txn.InsertEntry(confEntry("dst", "copy"))
		return err
	})

	dstEntries, err := store.ReadEntries(ctx, "dst", 0)
	testassert.Require(t, err == nil && len(dstEntries) == 1 && dstEntries[0].ID == "copy", "destination entries = %v (error %v), want [copy]", dstEntries, err)
	srcEntries, err := store.ReadEntries(ctx, "src", 0)
	testassert.Require(t, err == nil && len(srcEntries) == 2, "source entries = %d (error %v), want 2", len(srcEntries), err)

	boom := errors.New("multi-session rollback")
	err = store.Transact(ctx, func(txn harness.Transaction) error {
		if _, err := txn.ReadEntries("src", 0); err != nil {
			return err
		}
		if _, err := txn.InsertEntry(confEntry("dst", "rollback")); err != nil {
			return err
		}
		return boom
	})
	testassert.Require(t, errors.Is(err, boom), "Transact error = %v, want the callback error", err)
	{
		ids, err := store.ListSessionIDs(ctx)
		testassert.Check(t, err == nil && len(ids) == 2, "rollback changed the session set: %v (error %v)", ids, err)
	}
	{
		entries, err := store.ReadEntries(ctx, "dst", 0)
		testassert.Check(t, err == nil && len(entries) == 1, "rollback changed destination entries: %d (error %v)", len(entries), err)
	}
}

// confRevisionConflict proves a stale replacement aborts the complete
// transaction, leaving no sequence or revision change behind.
func confRevisionConflict(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})
	key := confSessionKey("s1")

	err := store.Transact(ctx, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("s1", "e2")); err != nil {
			return err
		}
		_, err := txn.ReplaceRegister(key, 99, rawJSON(`{"stale":true}`))
		return err
	})
	testassert.Require(t, errors.Is(err, harness.ErrConflict), "stale preparation: error = %v, want ErrConflict", err)

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.Check(t, err == nil && len(entries) == 1 && entries[0].ID == "e1", "stale replacement consumed a staged sequence: %v (error %v)", entries, err)
	register, err := store.ReadRegister(ctx, key)
	testassert.Check(t, err == nil && register.Revision == 1, "stale replacement changed the register: revision %d (error %v)", register.Revision, err)
	confPayloadEqual(t, register.Payload, `{"session":"s1"}`)

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(key, 1, rawJSON(`{"current":true}`))
		return err
	})
	register, err = store.ReadRegister(ctx, key)
	testassert.Check(t, err == nil && register.Revision == 2, "retry at exact revision = revision %d (error %v), want 2", register.Revision, err)
}

// confRecoveryReads covers sorted session discovery, deterministic register
// ordering, presence of an empty session, and read stability.
func confRecoveryReads(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)

	ids, err := store.ListSessionIDs(ctx)
	testassert.Require(t, err == nil && len(ids) == 0, "empty store ListSessionIDs = %v (error %v), want empty", ids, err)

	confCreateSession(t, ctx, store, "b")
	confCreateSession(t, ctx, store, "a")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(confEntry("a", "e1")); err != nil {
			return err
		}
		if _, err := txn.InsertRegister(confOpRegister("a", "op-b")); err != nil {
			return err
		}
		_, err := txn.InsertRegister(confOpRegister("a", "op-a"))
		return err
	})

	ids, err = store.ListSessionIDs(ctx)
	testassert.Require(t, err == nil && len(ids) == 2 && ids[0] == "a" && ids[1] == "b", "ListSessionIDs = %v (error %v), want [a b]", ids, err)

	registers, err := store.ReadRegisters(ctx, "a")
	testassert.NoError(t, err)
	testassert.Equal(t, len(registers), 3)
	testassert.Check(t, registers[0].Key.Kind == harness.RegisterSession, "first register = %+v, want the session register", registers[0].Key)
	testassert.Check(t, registers[1].Key.OperationID == "op-a" && registers[2].Key.OperationID == "op-b", "operation register order = %s, %s; want op-a, op-b", registers[1].Key.OperationID, registers[2].Key.OperationID)
	for _, register := range registers {
		testassert.Check(t, register.Revision == 1, "register %+v revision = %d, want 1", register.Key, register.Revision)
	}

	onlySession, err := store.ReadRegisters(ctx, "b")
	testassert.Require(t, err == nil && len(onlySession) == 1 && onlySession[0].Key.Kind == harness.RegisterSession, "register-only session ReadRegisters = %v (error %v), want the session register", onlySession, err)
	{
		entries, err := store.ReadEntries(ctx, "b", 0)
		testassert.Check(t, err == nil && len(entries) == 0, "register-only session ReadEntries = %v (error %v), want empty", entries, err)
	}

	idsAgain, err := store.ListSessionIDs(ctx)
	testassert.Check(t, err == nil && len(idsAgain) == 2 && idsAgain[0] == "a" && idsAgain[1] == "b", "repeated ListSessionIDs = %v (error %v), want [a b]", idsAgain, err)
	reread, err := store.ReadRegisters(ctx, "a")
	testassert.Check(t, err == nil && len(reread) == 3 && reread[2].Key.OperationID == "op-b" && reread[2].Revision == 1, "recovery reads mutated state: %v (error %v)", reread, err)
}

// confSequenceExhaustion proves sequence advancement beyond math.MaxInt64
// returns ErrStorage and aborts the complete transaction.
func confSequenceExhaustion(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})

	b.exhaustSequence(t, store, "s1")

	err := store.Transact(ctx, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "overflow"))
		return err
	})
	testassert.Require(t, errors.Is(err, harness.ErrStorage), "sequence exhaustion: error = %v, want ErrStorage", err)

	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	testassert.Check(t, len(entries) == 2 && entries[1].Sequence == math.MaxInt64, "aborted exhaustion transaction changed entries: %v", entries)
}

// confRevisionExhaustion proves revision advancement beyond math.MaxInt64
// returns ErrStorage and aborts the complete transaction.
func confRevisionExhaustion(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")
	key := confSessionKey("s1")

	b.exhaustRevision(t, store, "s1")

	register, err := store.ReadRegister(ctx, key)
	testassert.Require(t, err == nil && register.Revision == math.MaxInt64, "fixture revision = %d (error %v), want %d", register.Revision, err, int64(math.MaxInt64))
	err = store.Transact(ctx, func(txn harness.Transaction) error {
		_, err := txn.ReplaceRegister(key, math.MaxInt64, rawJSON(`{"next":true}`))
		return err
	})
	testassert.Require(t, errors.Is(err, harness.ErrStorage), "revision exhaustion: error = %v, want ErrStorage", err)
	register, err = store.ReadRegister(ctx, key)
	testassert.Require(t, err == nil && register.Revision == math.MaxInt64, "aborted exhaustion transaction changed revision: %d (error %v)", register.Revision, err)
	confPayloadEqual(t, register.Payload, `{"session":"s1"}`)
}

// confCorruption proves persisted malformed envelopes are typed corruption
// owned by one session, that the owning session stays discoverable, and that
// another session remains readable.
func confCorruption(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "owner")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("owner", "e1"))
		return err
	})
	confCreateSession(t, ctx, store, "keeper")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("keeper", "k1"))
		return err
	})

	b.corruptEntry(t, store, "owner")

	ids, err := store.ListSessionIDs(ctx)
	testassert.Require(t, err == nil && len(ids) == 2 && ids[0] == "keeper" && ids[1] == "owner", "corrupt state disappeared from discovery: %v (error %v)", ids, err)
	_, err = store.ReadEntries(ctx, "owner", 0)
	testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "corrupt entry: error = %v, want ErrCorrupt", err)
	var corrupt *harness.CorruptionError
	testassert.Check(t, errors.As(err, &corrupt) && corrupt.SessionID == "owner", "corrupt entry recovered %+v, want owning session owner", corrupt)
	keeperEntries, err := store.ReadEntries(ctx, "keeper", 0)
	testassert.Check(t, err == nil && len(keeperEntries) == 1 && keeperEntries[0].ID == "k1", "keeper readable entries = %v (error %v), want [k1]", keeperEntries, err)

	b.corruptRegister(t, store, "owner")

	for name, read := range map[string]func() error{
		"ReadRegister":  func() error { _, err := store.ReadRegister(ctx, confSessionKey("owner")); return err },
		"ReadRegisters": func() error { _, err := store.ReadRegisters(ctx, "owner"); return err },
	} {
		err := read()
		testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "%s on corrupt register: error = %v, want ErrCorrupt", name, err)
		var corrupt *harness.CorruptionError
		testassert.Check(t, errors.As(err, &corrupt) && corrupt.SessionID == "owner", "%s on corrupt register recovered %+v, want owning session owner", name, corrupt)
	}
	{
		_, err := store.ReadRegister(ctx, confSessionKey("keeper"))
		testassert.Check(t, err == nil, "keeper register unreadable beside corruption: %v", err)
	}
	ids, err = store.ListSessionIDs(ctx)
	testassert.Check(t, err == nil && len(ids) == 2, "register corruption removed discovery: %v (error %v)", ids, err)

	if b.reopen != nil {
		store = b.reopen(t, store)
		{
			_, err := store.ReadEntries(ctx, "owner", 0)
			testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "entry corruption after reopen: error = %v, want ErrCorrupt", err)
		}
		{
			_, err := store.ReadRegister(ctx, confSessionKey("owner"))
			testassert.Check(t, errors.Is(err, harness.ErrCorrupt), "register corruption after reopen: error = %v, want ErrCorrupt", err)
		}
		{
			entries, err := store.ReadEntries(ctx, "keeper", 0)
			testassert.Check(t, err == nil && len(entries) == 1, "keeper after reopen: %d entries (error %v)", len(entries), err)
		}
	}
}

// confCompactionPayloadRoundTrip proves the compaction entry kind round-trips
// its opaque payload byte-exactly on every backend, in both the operation-owned
// and the independently copied operationless shape, across a durable reopen
// where the backend supports one.
func confCompactionPayloadRoundTrip(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "s1")

	owned := `{"session_id":"s1","entry_id":"c1","operation_id":"op-1","summary":"Summary of the earlier conversation.",` +
		`"boundary_entry_id":"e1","model":{"provider":"prov","model":"gpt-x"},"configuration_revision":"rev-1",` +
		`"usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":6}}`
	copied := `{"session_id":"s1","entry_id":"c2","summary":"Summary of the earlier conversation.",` +
		`"boundary_entry_id":"e1","model":{"provider":"prov","model":"gpt-x"},"configuration_revision":"rev-1"}`

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertEntry(harness.EntryDraft{SessionID: "s1", ID: "c1", OperationID: "op-1", Kind: harness.EntryCompaction, Payload: rawJSON(owned)}); err != nil {
			return err
		}
		_, err := txn.InsertEntry(harness.EntryDraft{SessionID: "s1", ID: "c2", Kind: harness.EntryCompaction, Payload: rawJSON(copied)})
		return err
	})

	if b.reopen != nil {
		store = b.reopen(t, store)
	}
	entries, err := store.ReadEntries(ctx, "s1", 0)
	testassert.NoError(t, err)
	testassert.Equal(t, len(entries), 2)
	testassert.Check(t, entries[0].ID == "c1" && entries[0].Kind == harness.EntryCompaction && entries[0].OperationID == "op-1", "owned compaction envelope = %+v", entries[0])
	confPayloadEqual(t, entries[0].Payload, owned)
	testassert.Check(t, entries[1].ID == "c2" && entries[1].Kind == harness.EntryCompaction && entries[1].OperationID == "", "copied compaction envelope = %+v", entries[1])
	confPayloadEqual(t, entries[1].Payload, copied)
}

// confSessionDeletion covers the removal primitive: one transaction removes
// the session register, every operation register, and every entry; absent
// sessions are not-found; empty identity is invalid; staged deletions roll
// back; and outside readers never observe a half-deleted session.
func confSessionDeletion(t *testing.T, b conformanceBackend) {
	ctx := context.Background()
	store := b.newStore(t)
	confCreateSession(t, ctx, store, "gone")
	confCreateSession(t, ctx, store, "kept")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertRegister(confOpRegister("gone", "op1")); err != nil {
			return err
		}
		if _, err := txn.InsertRegister(confOpRegister("gone", "op2")); err != nil {
			return err
		}
		if _, err := txn.InsertEntry(confEntry("gone", "e1")); err != nil {
			return err
		}
		_, err := txn.InsertEntry(confEntry("gone", "e2"))
		return err
	})
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("kept", "k1"))
		return err
	})

	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		return txn.DeleteSession("gone")
	})

	ids, err := store.ListSessionIDs(ctx)
	testassert.Require(t, err == nil && len(ids) == 1 && ids[0] == "kept", "ListSessionIDs after deletion = %v (error %v), want [kept]", ids, err)
	{
		_, err := store.ReadRegister(ctx, confSessionKey("gone"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "deleted session register: error = %v, want ErrNotFound", err)
	}
	{
		_, err := store.ReadEntries(ctx, "gone", 0)
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "deleted session entries: error = %v, want ErrNotFound", err)
	}
	{
		_, err := store.ReadRegisters(ctx, "gone")
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "deleted session registers: error = %v, want ErrNotFound", err)
	}
	{
		_, err := store.ReadRegister(ctx, confOpKey("gone", "op1"))
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "deleted session operation register: error = %v, want ErrNotFound", err)
	}
	{
		entries, err := store.ReadEntries(ctx, "kept", 0)
		testassert.Check(t, err == nil && len(entries) == 1, "other session changed by deletion: %d entries (error %v)", len(entries), err)
	}

	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			return txn.DeleteSession("absent")
		})
		testassert.Check(t, errors.Is(err, harness.ErrNotFound), "DeleteSession absent: error = %v, want ErrNotFound", err)
	}
	{
		err := store.Transact(ctx, func(txn harness.Transaction) error {
			return txn.DeleteSession("")
		})
		testassert.Check(t, errors.Is(err, harness.ErrInvalid), "DeleteSession empty identity: error = %v, want ErrInvalid", err)
	}

	boom := errors.New("deletion rollback")
	err = store.Transact(ctx, func(txn harness.Transaction) error {
		if err := txn.DeleteSession("kept"); err != nil {
			return err
		}
		return boom
	})
	testassert.Require(t, errors.Is(err, boom), "staged deletion Transact error = %v, want the callback error", err)
	{
		ids, err := store.ListSessionIDs(ctx)
		testassert.Check(t, err == nil && len(ids) == 1 && ids[0] == "kept", "rolled-back deletion removed the session: %v (error %v)", ids, err)
	}

	staged := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- store.Transact(ctx, func(txn harness.Transaction) error {
			if err := txn.DeleteSession("kept"); err != nil {
				return err
			}
			close(staged)
			return nil
		})
	}()
	<-staged
	ids, err = store.ListSessionIDs(ctx) // the complete pre-commit or post-commit set, never partial
	testassert.NoError(t, err)
	testassert.Check(t, len(ids) <= 1 && (len(ids) != 1 || ids[0] == "kept"), "outside reader observed a partially deleted session set: %v, want exactly [kept] or empty", ids)
	testassert.NoError(t, <-committed)
	{
		ids, err = store.ListSessionIDs(ctx)
		testassert.Check(t, err == nil && len(ids) == 0, "session set after committed deletion = %v (error %v), want empty", ids, err)
	}
}
