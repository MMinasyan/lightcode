// Public per-Session sweep transition coverage: the exported one-Session
// transition the broad sweep and the owner's artifact-coordinated passes
// delegate to.
package harness_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// TestPublicSweepSessionSkipsBusyCandidate proves the one per-Session sweep
// transition: a running candidate returns the same no-transition outcome the
// broad sweep applies, without waiting on the Session's work; an unknown
// identity reports the not-found class; a zero time is invalid input; and an
// eligible archived candidate transitions inside the call.
func TestPublicSweepSessionSkipsBusyCandidate(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSessionHeader(ctx, session)
		if err != nil {
			t.Fatalf("read created session: %v", err)
		}

		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		<-script.arrived // parked at the model boundary: the Session is running

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		deleted, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour))
		if err != nil || deleted {
			t.Fatalf("SweepSession of a running candidate = (%v, %v), want the no-transition outcome", deleted, err)
		}
		still, err := f.h.ReadSessionHeader(ctx, session)
		if err != nil || still.Lifecycle != harness.LifecycleOpen || still.CurrentOperationID != "op-1" {
			t.Fatalf("SweepSession touched the running candidate: %+v err %v", still, err)
		}

		if _, err := f.h.SweepSession(ctx, "0123456789abcdef0123456789abcdef", policy, first.LastActivity.Add(100*time.Hour)); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("SweepSession of an unknown identity = %v, want harness.ErrNotFound", err)
		}
		if _, err := f.h.SweepSession(ctx, session, policy, time.Time{}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("SweepSession with a zero time = %v, want harness.ErrInvalid", err)
		}

		script.releaseGate()
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPublicSweepSessionDeletesEligibleCandidate proves the positive
// transition through the exported per-Session entry point: an archived
// candidate past its delete threshold commits its deletion inside the call
// and reports it, like the broad sweep's row.
func TestPublicSweepSessionDeletesEligibleCandidate(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSessionHeader(ctx, session)
		if err != nil {
			t.Fatalf("read created session: %v", err)
		}
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		if _, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour)); err != nil {
			t.Fatalf("archive SweepSession: %v", err)
		}

		deleted, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(200*time.Hour))
		if err != nil || !deleted {
			t.Fatalf("delete SweepSession = (%v, %v), want the committed deletion", deleted, err)
		}
		if _, err := f.h.ReadSessionHeader(ctx, session); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("read after SweepSession deletion = %v, want harness.ErrNotFound", err)
		}
	})
}
