package runtime

// This file owns the retained automatic lifecycle-sweep scheduling: the
// Runtime never reimplements Session transitions. Every pass enumerates the
// Harness's owned Session headers and consumes the per-Session sweep
// transition through the admitted-call gate, coordinating each candidate's
// artifact interval without waiting on busy siblings.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// sweepInterval is the retained hourly automatic cadence: one pass runs
// during construction and one per ticker fire afterward.
const sweepInterval = time.Hour

// startMaintenance brings the retained automatic sweep up: the loop is
// registered as Runtime-owned work, one initial pass runs synchronously, and
// each later pass runs on one tick, so shutdown cancels and joins any
// admitted pass before storage teardown and stops the ticker. The loop runs
// the passes serially, so no two passes overlap. ticks carries each pass's
// explicit sweep time: production supplies a time.Ticker channel whose sends
// are the clock readings at each fire, while a caller may supply a controlled
// stream. stopTicker disposes the ticker alongside the loop's exit.
func (r *Runtime) startMaintenance(ticks <-chan time.Time, stopTicker func()) {
	work := r.work
	r.calls.Add(1)
	r.runSweepPass(work, time.Now())
	go func() {
		defer r.calls.Done()
		defer stopTicker()
		for {
			select {
			case <-work.Done():
				return
			case now := <-ticks:
				r.runSweepPass(work, now)
			}
		}
	}()
}

// runSweepPass performs one automatic lifecycle sweep: it samples the
// current session policy, retains auto_archive=false as disabling the whole
// sweep, converts each positive day count into its checked 24-hour
// threshold and leaves a nonpositive count at zero so the Harness disables
// only that transition, and enumerates the Harness's owned Session headers
// inside one admitted call. Each candidate's Session artifact interval is
// taken nonblockingly: a held interval skips that candidate for the pass, so
// one busy Session never makes the pass wait on unrelated execution. Under
// the token the pass calls the per-Session Sweep transition; a gone or
// corrupt candidate is omitted, and any other error stops the pass. Every
// committed deletion's artifact tree is then removed inside the same
// admission under its own lease — including successes collected before a
// stopping error — and all of their Session warning groups are removed
// together in one observation section with one runtime-scoped hint; only
// then does every lease release, on every exit. Cleanup failures join the
// pass result, so one diagnostic reports everything. The diagnostic decision
// is sampled once after the pass returns: a failure while the owned context
// is live is reported through the retained stderr diagnostic unless the
// admission gate returned ErrClosed, and once shutdown is observed the pass
// stays quiet, accepting that an unrelated failure concurrent with shutdown
// may go unlogged. A context-valued source error against a live owner is an
// ordinary failure too. Later cancellation never retracts a report the check
// already admitted. No special event, startup failure, or immediate retry
// follows a failed pass, and a failed cleanup never re-runs the committed
// deletion.
func (r *Runtime) runSweepPass(ctx context.Context, now time.Time) {
	sessions := r.config.current().sessions
	if !sessions.AutoArchive {
		return
	}
	policy := harness.SweepPolicy{
		ArchiveAfter:       sweepThreshold(sessions.ArchiveAfterDays),
		DeleteAfterArchive: sweepThreshold(sessions.DeleteAfterArchiveDays),
	}
	err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		headers, err := h.ListSessions(ctx)
		if err != nil {
			return err
		}
		var (
			deleted   []string
			leases    []func() // committed-deletion leases, retained through their cleanups and the batch's warning cleanup
			cleanErrs []error
			passErr   error
		)
		for _, header := range headers {
			sessionID := header.Identity.SessionID
			releaseArtifacts, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, sessionID))
			if err != nil {
				continue // a held interval skips this candidate for the pass
			}
			commit, err := h.SweepSession(ctx, sessionID, policy, now)
			if err != nil {
				releaseArtifacts()
				var corrupt *harness.CorruptionError
				if errors.As(err, &corrupt) || errors.Is(err, harness.ErrNotFound) {
					continue // a gone or corrupt candidate is omitted
				}
				passErr = err // stop the pass; the accumulated committed deletions are cleaned below
				break
			}
			if commit {
				deleted = append(deleted, sessionID)
				leases = append(leases, releaseArtifacts) // retained through the cleanups and the batch's warning cleanup
				continue
			}
			releaseArtifacts()
		}
		// Every committed deleted identity's artifact tree is removed inside
		// the same admission under its own lease — including successes
		// collected before a pass failure.
		for _, sessionID := range deleted {
			if err := r.removeSessionCode(sessionID); err != nil {
				cleanErrs = append(cleanErrs, err)
			}
		}
		// The committed batch's warning cleanup is one section and one hint,
		// and it runs for every committed identity before the pass error is
		// returned; only then do the retained leases release.
		r.passive.removeSessionWarnings(deleted)
		for _, releaseArtifacts := range leases {
			releaseArtifacts()
		}
		return errors.Join(append([]error{passErr}, cleanErrs...)...)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
		fmt.Fprintf(os.Stderr, "lightcode: sweep: %v\n", err)
	}
}

// sweepThreshold converts one configured day count into its 24-hour
// threshold. Positive counts arrive overflow-checked from the configuration
// parser, so the multiplication cannot overflow; a nonpositive count becomes
// zero, the Harness rule for a disabled transition.
func sweepThreshold(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}
