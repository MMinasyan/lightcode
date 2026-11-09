package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestClaimExternalDataRecordsActiveOwners proves the external-data claim is
// the ordinary open-Session mutation: a claim during an active Operation is
// accepted without waiting for quiescence, a repeat is a true no-op, and the
// ordered unique owners return only with the committed deletion.
func TestClaimExternalDataRecordsActiveOwners(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return preparedExecuting(validExecution()), nil
	})
	session := createSession(t, h)
	{
		_, err := submitText(t, h, session, "op-claim", MessageModeRegular, "running")
		testassert.NoError(t, err)
	}
	{
		rec, err := h.ReadOperation(context.Background(), session, "op-claim")
		testassert.Require(t, err == nil && rec.State.Status == OperationRunning, "operation = (%+v, %v), want the active Operation", rec, err)
	}
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest.a"))
	{
		reg := sessionRegister(t, store, session)
		rec, err := decodeSessionRegister(reg)
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"guest.a"}), "owners after the active claim = %v (%v), want [guest.a]", rec.State.ExternalDataOwners, err)
	}
	before := sessionRegister(t, store, session)
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest.a"))
	{
		after := sessionRegister(t, store, session)
		testassert.Require(t, after.Revision == before.Revision && string(after.Payload) == string(before.Payload), "repeated claim rewrote the register: revision %d -> %d", before.Revision, after.Revision)
	}
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest.b"))
	{
		rec, err := decodeSessionRegister(sessionRegister(t, store, session))
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"guest.a", "guest.b"}), "owners = %v (%v), want the ordered unique claim set", rec.State.ExternalDataOwners, err)
	}
}

// TestClaimExternalDataRefusesArchivedSession proves a claim requires an open
// Session, a missing identity is invalid, and a refused claim writes nothing.
func TestClaimExternalDataRefusesArchivedSession(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	session := createSession(t, h)
	{
		_, err := h.ArchiveSession(context.Background(), session)
		testassert.NoError(t, err)
	}
	before := sessionRegister(t, store, session)
	{
		err := h.ClaimExternalData(context.Background(), session, "guest")
		testassert.Require(t, errors.Is(err, ErrInvalid), "claim on an archived Session = %v, want ErrInvalid", err)
	}
	{
		err := h.ClaimExternalData(context.Background(), session, "")
		testassert.Require(t, errors.Is(err, ErrInvalid), "claim with an empty owner = %v, want ErrInvalid", err)
	}
	{
		err := h.ClaimExternalData(context.Background(), "0123456789abcdef0123456789abcdef", "guest")
		testassert.Require(t, errors.Is(err, ErrNotFound), "claim on an absent Session = %v, want ErrNotFound", err)
	}
	after := sessionRegister(t, store, session)
	testassert.Require(t, after.Revision == before.Revision && string(after.Payload) == string(before.Payload), "a refused claim rewrote the register")
}

// TestDeleteAndSweepReturnCommittedOwners proves the checked deletion paths
// return the register's owners only after the canonical commit, an absent
// repeat has no owners, and the sweep reports the same committed set.
func TestDeleteAndSweepReturnCommittedOwners(t *testing.T) {
	t.Run("direct delete", func(t *testing.T) {
		h := newTestHarness(t, emptyStore(t), nil)
		session := createSession(t, h)
		testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest.a"))
		testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest.b"))
		{
			_, err := h.ArchiveSession(context.Background(), session)
			testassert.NoError(t, err)
		}
		owners, err := h.DeleteSession(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, slices.Equal(owners, []string{"guest.a", "guest.b"}), "DeleteSession owners = %v, want the committed ordered set", owners)
		owners, err = h.DeleteSession(context.Background(), session)
		testassert.Require(t, errors.Is(err, ErrNotFound) && len(owners) == 0, "repeated delete = (%v, %v), want the absent class and no owners", owners, err)
	})

	t.Run("sweep", func(t *testing.T) {
		h := newTestHarness(t, emptyStore(t), nil)
		session := createSession(t, h)
		testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest"))
		{
			_, err := h.ArchiveSession(context.Background(), session)
			testassert.NoError(t, err)
		}
		now := time.Now().UTC()
		commit, owners, err := h.SweepSession(context.Background(), session, SweepPolicy{DeleteAfterArchive: time.Second}, now.Add(2*time.Second))
		testassert.NoError(t, err)
		testassert.Require(t, commit && slices.Equal(owners, []string{"guest"}), "SweepSession = (commit %v, owners %v, %v), want the committed deletion and its owners", commit, owners, err)
	})
}

// sessionRegister reads one Session register envelope from the fixture store.
func sessionRegister(t *testing.T, store Storage, sessionID string) Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterSession})
	testassert.Require(t, err == nil, "ReadRegister(%s): %v", sessionID, err)
	return reg
}

// TestSessionRegisterExternalOwnersIsStrictCanonical proves the new register
// member is always materialized on encode and required by the one strict
// decoder: there is no compatibility reader.
func TestSessionRegisterExternalOwnersIsStrictCanonical(t *testing.T) {
	record := validSessionRecord()
	payload, err := encodeSessionRegister(record)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(payload), `"external_data_owners":[]`), "encoded register = %s, want the materialized empty owner list", payload)

	record.State.ExternalDataOwners = []string{"guest.a", "guest.b"}
	payload, err = encodeSessionRegister(record)
	testassert.NoError(t, err)
	decoded, err := decodeSessionRegister(Register{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterSession}, Revision: 1, Payload: payload})
	testassert.Require(t, err == nil && slices.Equal(decoded.State.ExternalDataOwners, []string{"guest.a", "guest.b"}), "decoded owners = %v (%v)", decoded.State.ExternalDataOwners, err)

	record.State.ExternalDataOwners = []string{"guest", "guest"}
	if _, err := encodeSessionRegister(record); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate owner encode error = %v, want ErrInvalid", err)
	}
	key := RegisterKey{SessionID: testSessionID, Kind: RegisterSession}
	replaceOwners := func(value string) []byte {
		return []byte(strings.Replace(string(payload), `"external_data_owners":["guest.a","guest.b"]`, `"external_data_owners":`+value, 1))
	}
	for _, row := range []struct {
		name string
		body []byte
	}{
		{"missing", []byte(strings.Replace(string(payload), `,"external_data_owners":["guest.a","guest.b"]`, "", 1))},
		{"null", replaceOwners("null")},
		{"empty-id", replaceOwners(`["guest.a",""]`)},
		{"duplicate", replaceOwners(`["guest.a","guest.a"]`)},
		{"non-string", replaceOwners(`["guest.a",5]`)},
	} {
		if _, err := decodeSessionRegister(Register{Key: key, Revision: 1, Payload: row.body}); err == nil {
			t.Fatalf("decoder accepted %s external_data_owners", row.name)
		}
	}
	if _, err := decodeSessionRegister(Register{Key: key, Revision: 1, Payload: replaceOwners(`[]`)}); err != nil {
		t.Fatalf("decoder rejected the empty owner array: %v", err)
	}
}

// TestClaimExternalDataRollbackLeavesNoPartialOwner proves a failed claim
// transaction writes no owner and a later retry commits normally.
func TestClaimExternalDataRollbackLeavesNoPartialOwner(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	session := createSession(t, h)
	injected := errors.New("injected claim failure")
	store.txHook = func(step string) error {
		if step == "replace_register" {
			return injected
		}
		return nil
	}
	before := sessionRegister(t, store, session)
	{
		err := h.ClaimExternalData(context.Background(), session, "guest")
		testassert.Require(t, errors.Is(err, injected), "claim transaction failure = %v, want the injected error", err)
	}
	after := sessionRegister(t, store, session)
	{
		rec, err := decodeSessionRegister(after)
		testassert.Require(t, err == nil && len(rec.State.ExternalDataOwners) == 0 && after.Revision == before.Revision && string(after.Payload) == string(before.Payload), "failed claim left partial state: revision %d -> %d, owners %v", before.Revision, after.Revision, rec.State.ExternalDataOwners)
	}
	store.txHook = nil
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "guest"))
	{
		rec, err := decodeSessionRegister(sessionRegister(t, store, session))
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"guest"}), "retried claim owners = %v (%v)", rec.State.ExternalDataOwners, err)
	}
}

// TestClaimExternalDataStaleResidentRevision proves a real second owner
// mutation against a pinned resident graph reports the conflict, retains the
// second writer's claim, rematerializes, and allows an ordinary retry.
func TestClaimExternalDataStaleResidentRevision(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	session := createSession(t, h)
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "owner.a"))

	_, release := leaseCoordinatorNow(t, h, session)
	defer release()
	testassert.NoError(t, store.Transact(context.Background(), func(tx Transaction) error {
		key := RegisterKey{SessionID: session, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return err
		}
		state := current.State
		state.ExternalDataOwners = append(append([]string(nil), state.ExternalDataOwners...), "owner.b")
		payload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: state})
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, payload)
		return err
	}))
	{
		err := h.ClaimExternalData(context.Background(), session, "owner.c")
		testassert.Require(t, errors.Is(err, ErrConflict), "claim against the stale resident revision = %v, want ErrConflict", err)
	}
	{
		rec, err := decodeSessionRegister(sessionRegister(t, store, session))
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"owner.a", "owner.b"}), "owners after the stale conflict = %v (%v), want the second writer's claim retained", rec.State.ExternalDataOwners, err)
	}
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "owner.c"))
	{
		rec, err := decodeSessionRegister(sessionRegister(t, store, session))
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"owner.a", "owner.b", "owner.c"}), "owners after the retry = %v (%v), want no lost claim", rec.State.ExternalDataOwners, err)
	}
}

// TestClaimExternalDataReservationWaitAndCancel proves the claim observes the
// existing reservation mechanism: it waits for a held reservation and returns
// the caller's cancellation, then commits normally after the reservation
// releases.
func TestClaimExternalDataReservationWaitAndCancel(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	session := createSession(t, h)
	c, release := leaseCoordinatorNow(t, h, session)
	defer release()
	releaseReservation, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ClaimExternalData(ctx, session, "owner.a") }()
	select {
	case err := <-done:
		t.Fatalf("claim returned (%v) while the admission reservation was held", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		testassert.Require(t, errors.Is(err, context.Canceled), "canceled claim = %v, want context.Canceled", err)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled claim never returned")
	}
	releaseReservation()
	testassert.NoError(t, h.ClaimExternalData(context.Background(), session, "owner.a"))
	{
		rec, err := decodeSessionRegister(sessionRegister(t, store, session))
		testassert.Require(t, err == nil && slices.Equal(rec.State.ExternalDataOwners, []string{"owner.a"}), "owners after the reservation retry = %v (%v)", rec.State.ExternalDataOwners, err)
	}
}
