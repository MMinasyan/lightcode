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

// TestPublicSweepSessionRejectsUnknownOrZeroTime proves the per-Session sweep
// transition's remaining rejection rows: an unknown identity reports the
// not-found class and a zero time is invalid input. The busy candidate's
// no-transition outcome is observed with its stronger register and buffer
// evidence by TestPublicSweepRunningLeftUnchanged.
func TestPublicSweepSessionRejectsUnknownOrZeroTime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSessionHeader(ctx, session)
		testassert.NoError(t, err)

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		{
			_, err := f.h.SweepSession(ctx, "0123456789abcdef0123456789abcdef", policy, first.LastActivity.Add(100*time.Hour))
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "SweepSession of an unknown identity = %v, want harness.ErrNotFound", err)
		}
		{
			_, err := f.h.SweepSession(ctx, session, policy, time.Time{})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "SweepSession with a zero time = %v, want harness.ErrInvalid", err)
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
