// Public per-Session sweep transition coverage: the exported one-Session
// lifecycle transition the owner's artifact-coordinated passes consume.
package harness_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestPublicSweepSessionSkipsBusyCandidate proves the one per-Session sweep
// transition: a running candidate takes the no-transition outcome without
// waiting on the Session's work; an unknown
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
		testassert.NoError(t, err)

		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // parked at the model boundary: the Session is running

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		deleted, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour))
		testassert.Require(t, err == nil && !deleted, "SweepSession of a running candidate = (%v, %v), want the no-transition outcome", deleted, err)
		still, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && still.Lifecycle == harness.LifecycleOpen && still.CurrentOperationID == "op-1", "SweepSession touched the running candidate: %+v err %v", still, err)

		{
			_, err := f.h.SweepSession(ctx, "0123456789abcdef0123456789abcdef", policy, first.LastActivity.Add(100*time.Hour))
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "SweepSession of an unknown identity = %v, want harness.ErrNotFound", err)
		}
		{
			_, err := f.h.SweepSession(ctx, session, policy, time.Time{})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "SweepSession with a zero time = %v, want harness.ErrInvalid", err)
		}

		script.releaseGate()
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
	})
}

// TestPublicSweepSessionDeletesEligibleCandidate proves the positive
// transition through the exported per-Session entry point: an archived
// candidate past its delete threshold commits its deletion inside the call
// and reports it through the exported per-Session entry point.
func TestPublicSweepSessionDeletesEligibleCandidate(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSessionHeader(ctx, session)
		testassert.NoError(t, err)
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		{
			_, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour))
			testassert.NoError(t, err)
		}

		deleted, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(200*time.Hour))
		testassert.Require(t, err == nil && deleted, "delete SweepSession = (%v, %v), want the committed deletion", deleted, err)
		{
			_, err := f.h.ReadSessionHeader(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read after SweepSession deletion = %v, want harness.ErrNotFound", err)
		}
	})
}
