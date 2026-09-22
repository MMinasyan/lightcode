package runtime

// Automatic lifecycle-sweep scheduling tests: the retained startup/hourly
// cadence drives the existing Harness.Sweep transition under the sampled
// session policy, with controlled tick streams, one owned loop, ordinary
// admitted-call join, and the retained stderr diagnostic. Each case runs on
// the real owner over memory and temporary SQLite through the existing
// fixtures; HOME and bundled credentials are isolated.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// ownerSweepDocument builds one owner configuration document with the fixed
// prov/m catalog and an explicit sessions section.
func ownerSweepDocument(sessions string) string {
	return `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"sessions":` + sessions + `}`
}

// captureSweepStderr redirects os.Stderr into a temp file for the test's
// duration and returns a func that reads what was written.
func captureSweepStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "sweep-stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})
	return func() string {
		data, _ := os.ReadFile(f.Name())
		return string(data)
	}
}

// sweepStore wraps the Core storage for the scheduler cases: it counts
// ListSessionIDs calls, fails the next armed number of them with a chosen
// error, parks the next transaction on a test release, and can fail one named
// Session's deletion after its real effect.
type sweepStore struct {
	harness.Storage
	mu           sync.Mutex
	lists        int
	failErr      error
	failLeft     int
	blocked      bool
	releaseErr   error
	release      chan struct{}
	arrived      chan struct{}
	written      chan struct{}
	deleteTarget string
}

func newSweepStore(base harness.Storage) *sweepStore {
	return &sweepStore{Storage: base, arrived: make(chan struct{}, 1), written: make(chan struct{}, 8)}
}

func (s *sweepStore) ListSessionIDs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	s.lists++
	var failErr error
	if s.failLeft > 0 {
		s.failLeft--
		failErr = s.failErr
		if s.failLeft == 0 {
			s.failErr = nil
		}
	}
	s.mu.Unlock()
	if failErr != nil {
		return nil, failErr
	}
	return s.Storage.ListSessionIDs(ctx)
}

func (s *sweepStore) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// armFailOnce makes exactly the next ListSessionIDs fail with err and then
// disarms.
func (s *sweepStore) armFailOnce(err error) {
	s.mu.Lock()
	s.failErr = err
	s.failLeft = 1
	s.mu.Unlock()
}

// armDeleteTarget arms exactly the next transaction deleting the target
// Session to perform its real mutation and then fail, so the surrounding
// transaction rolls back a completed mutation.
func (s *sweepStore) armDeleteTarget(target string) {
	s.mu.Lock()
	s.deleteTarget = target
	s.mu.Unlock()
}

// takeDeleteTarget reports whether the armed deletion target matches this
// identity and disarms it when it does.
func (s *sweepStore) takeDeleteTarget(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteTarget != "" && sessionID == s.deleteTarget {
		s.deleteTarget = ""
		return true
	}
	return false
}

// armBlock parks the next transaction until releaseBlock, signalling arrival
// from inside the park.
func (s *sweepStore) armBlock() {
	s.mu.Lock()
	s.blocked = true
	s.release = make(chan struct{})
	s.mu.Unlock()
}

// armFailAfterRelease parks the next transaction until releaseBlock and then
// fails it with err without reaching the base store, modelling a genuine
// storage failure that converges whenever the test releases it.
func (s *sweepStore) armFailAfterRelease(err error) {
	s.mu.Lock()
	s.blocked = true
	s.releaseErr = err
	s.release = make(chan struct{})
	s.mu.Unlock()
}

func (s *sweepStore) releaseBlock() {
	s.mu.Lock()
	if s.blocked {
		s.blocked = false
		close(s.release)
	}
	s.mu.Unlock()
}

func (s *sweepStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	s.mu.Lock()
	park, release, releaseErr := s.blocked, s.release, s.releaseErr
	s.releaseErr = nil
	s.mu.Unlock()
	if park {
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		<-release
		if releaseErr != nil {
			return releaseErr
		}
	}
	wrote := false
	err := s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(&writeSpy{Transaction: tx, wrote: &wrote, store: s})
	})
	if err == nil && wrote {
		select {
		case s.written <- struct{}{}:
		default:
		}
	}
	return err
}

// writeSpy reports whether one transaction performed a register mutation and
// fires the store's armed deletion target after its real effect.
type writeSpy struct {
	harness.Transaction
	wrote *bool
	store *sweepStore
}

func (w *writeSpy) ReplaceRegister(key harness.RegisterKey, expected int64, payload json.RawMessage) (harness.Register, error) {
	reg, err := w.Transaction.ReplaceRegister(key, expected, payload)
	if err == nil {
		*w.wrote = true
	}
	return reg, err
}

func (w *writeSpy) DeleteSession(sessionID string) error {
	err := w.Transaction.DeleteSession(sessionID)
	if err == nil {
		*w.wrote = true
		if w.store.takeDeleteTarget(sessionID) {
			return errSweepRollback
		}
	}
	return err
}

// waitWritten blocks until one sweep transition commits.
func waitWritten(t *testing.T, store *sweepStore) {
	t.Helper()
	select {
	case <-store.written:
	case <-time.After(10 * time.Second):
		t.Fatal("no sweep transaction committed a register mutation")
	}
}

// sendTick hands one explicit timestamp to the scheduler. Because the loop
// only returns to the select after a pass converges, the next send's
// rendezvous is the previous pass's completion barrier.
func sendTick(t *testing.T, ticks chan<- time.Time, now time.Time) {
	t.Helper()
	select {
	case ticks <- now:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep scheduler never took a tick")
	}
}

// deadlineContext is the minimal controllable test context: its Done closes
// with the deadline-expiry error exactly when the test expires it, replacing
// a wall-clock deadline. It adds no synchronization beyond Done itself.
type deadlineContext struct {
	context.Context
	done chan struct{}
}

func newDeadlineContext() *deadlineContext {
	return &deadlineContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *deadlineContext) Done() <-chan struct{} { return c.done }

func (c *deadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *deadlineContext) expire() { close(c.done) }

func readSweptSession(t *testing.T, r *Runtime, sessionID string) (harness.SessionRecord, error) {
	t.Helper()
	var rec harness.SessionRecord
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.ReadSession(ctx, sessionID)
		return err
	})
	return rec, err
}

func readSweepRegister(t *testing.T, store harness.Storage, sessionID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("read session register %q: %v", sessionID, err)
	}
	return reg
}

// rewriteSessionRegister replaces one Session register's durable state
// section under its current revision and returns the rewritten register.
func rewriteSessionRegister(t *testing.T, store harness.Storage, sessionID string, mutate func(state map[string]json.RawMessage)) harness.Register {
	t.Helper()
	ctx := context.Background()
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
	reg, err := store.ReadRegister(ctx, key)
	if err != nil {
		t.Fatalf("read register to rewrite: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(reg.Payload, &obj); err != nil {
		t.Fatalf("unmarshal session payload: %v", err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(obj["state"], &state); err != nil {
		t.Fatalf("unmarshal state section: %v", err)
	}
	mutate(state)
	obj["state"] = mustSweepJSON(t, state)
	if err := store.Transact(ctx, func(tx harness.Transaction) error {
		_, err := tx.ReplaceRegister(key, reg.Revision, mustSweepJSON(t, obj))
		return err
	}); err != nil {
		t.Fatalf("replace register: %v", err)
	}
	out, err := store.ReadRegister(ctx, key)
	if err != nil {
		t.Fatalf("reread rewritten register: %v", err)
	}
	return out
}

func mustSweepJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// seedStaleSession creates one idle Session through a standalone Harness on
// the shared store and shifts its durable last_activity into the past, so
// the automatic sweep is eligible against a positive-day threshold before
// the owner exists.
func seedStaleSession(t *testing.T, store harness.Storage, workspace string, stale time.Duration) string {
	t.Helper()
	ctx := context.Background()
	h, err := harness.New(ctx, harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{}, errors.New("seed harness prepares nothing")
		},
	})
	if err != nil {
		t.Fatalf("seed harness.New: %v", err)
	}
	rec, err := h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: workspace, AgentType: "seeded"})
	if err != nil {
		t.Fatalf("seed CreateSession: %v", err)
	}
	rewriteSessionRegister(t, store, rec.Identity.SessionID, func(state map[string]json.RawMessage) {
		var last time.Time
		if err := json.Unmarshal(state["last_activity"], &last); err != nil {
			t.Fatalf("decode last_activity: %v", err)
		}
		state["last_activity"] = mustSweepJSON(t, last.Add(-stale))
	})
	return rec.Identity.SessionID
}

// TestMaintenanceInitialPassRunsAtStartup proves the startup pass: one
// automatic sweep completes before the owner publication returns, under the
// initial policy, through the real Harness transition on both stores, with
// no model admission.
func TestMaintenanceInitialPassRunsAtStartup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":365}`))
		seeded := seedStaleSession(t, store, filepath.Join(e.dataDir, "seeded"), 100*time.Hour)
		r, err := e.open(context.Background(), e.storagePlugin(store))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		rec, err := readSweptSession(t, r, seeded)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if rec.State.Lifecycle != harness.LifecycleArchived || rec.State.ArchivedAt == nil {
			t.Fatalf("seeded Session after startup = %+v, want the initial pass to have archived it", rec.State)
		}
		if calls, opens := e.prep.counts(); calls != 0 || opens != 0 {
			t.Fatalf("preparation/opener calls during startup = %d/%d, want the sweep to admit no model work", calls, opens)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenanceControlledTicksSweepUnderTheCurrentPolicy proves the
// retained policy translation and the tick-driven cadence over one owned
// loop: auto_archive=false disables the whole sweep even at construction,
// Reloaded policy is sampled by the next pass, each tick timestamp is that
// pass's explicit time at the exact 24-hour archive/delete boundaries, and
// the pass admits no model work.
func TestMaintenanceControlledTicksSweepUnderTheCurrentPolicy(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		ticks := make(chan time.Time)
		opts := e.options(e.storagePlugin(wrapped))
		opts.sweepTicks = ticks
		r, err := open(context.Background(), opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got := wrapped.listCount(); got != 1 {
			t.Fatalf("storage lists after construction with auto_archive=false = %d, want recovery's single list only: false must disable the whole sweep", got)
		}
		created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		id := created.Identity.SessionID
		stderr := captureSweepStderr(t)

		archiveBoundary := created.State.LastActivity.Add(24 * time.Hour)
		archivedAt := archiveBoundary.Add(time.Nanosecond)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		if _, err := r.Reload(context.Background()); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		// Each send's rendezvous proves the previous pass fully converged, so
		// the boundary/no-op and transition passes cannot interleave.
		sendTick(t, ticks, archiveBoundary) // exact archive boundary: no transition
		sendTick(t, ticks, archivedAt)      // one nanosecond past: archive, stamped with the tick time
		waitWritten(t, wrapped)             // the archive committed; the boundary pass converged at the previous rendezvous
		archived, err := readSweptSession(t, r, id)
		if err != nil {
			t.Fatalf("ReadSession after the archive ticks: %v", err)
		}
		if archived.State.Lifecycle != harness.LifecycleArchived || archived.Revision != created.Revision+1 {
			t.Fatalf("Session after the two archive-boundary ticks = %+v rev %d, want exactly one archive at revision %d", archived.State, archived.Revision, created.Revision+1)
		}
		if archived.State.ArchivedAt == nil || !archived.State.ArchivedAt.Equal(archivedAt) {
			t.Fatalf("sweep stamped %v, want the archive tick's explicit time %v", archived.State.ArchivedAt, archivedAt)
		}
		sendTick(t, ticks, archivedAt.Add(24*time.Hour))                 // exact delete boundary
		sendTick(t, ticks, archivedAt.Add(24*time.Hour+time.Nanosecond)) // one nanosecond past: delete
		waitWritten(t, wrapped)
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("Session register after the delete-boundary pair = err %v, want it past the boundary deleted", err)
		}
		if got := wrapped.listCount(); got != 5 {
			t.Fatalf("storage lists = %d, want recovery plus exactly the four controlled passes", got)
		}
		if calls, opens := e.prep.counts(); calls != 0 || opens != 0 {
			t.Fatalf("preparation/opener calls = %d/%d, want none: the sweep admits no model work", calls, opens)
		}
		if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
			t.Fatalf("boundary passes reported diagnostics: %q", out)
		}
	})
}

// TestMaintenanceFailedPassReportsAndWaitsForTheNextTick proves the failed
// pass row: a storage error with a live owner reaches the retained stderr
// diagnostic exactly once, with no retry, and the next tick runs again.
func TestMaintenanceFailedPassReportsAndWaitsForTheNextTick(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"ordinary", errors.New("test sweep store listing failure")},
		{"deadline", context.DeadlineExceeded},
		{"canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newOwnerEnv(t)
				writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
				wrapped := newSweepStore(store)
				ticks := make(chan time.Time)
				opts := e.options(e.storagePlugin(wrapped))
				opts.sweepTicks = ticks
				r, err := open(context.Background(), opts)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
				if err != nil {
					t.Fatalf("createSession: %v", err)
				}
				stderr := captureSweepStderr(t)
				wrapped.armFailOnce(tc.failure)
				tick := created.State.LastActivity.Add(100 * time.Hour)
				sendTick(t, ticks, tick) // this pass lists, fails, and reports
				sendTick(t, ticks, tick) // the rendezvous proves the failed pass converged; this pass sweeps again
				waitWritten(t, wrapped)  // the later tick committed the archive
				if rec, err := readSweptSession(t, r, created.Identity.SessionID); err != nil || rec.State.Lifecycle != harness.LifecycleArchived {
					t.Fatalf("Session after the later tick = %+v err %v, want the failed pass to leave no lasting damage", rec, err)
				}
				if err := r.Close(context.Background()); err != nil {
					t.Fatalf("Close: %v", err)
				}
				out := stderr()
				if n := strings.Count(out, "lightcode: sweep: "+tc.failure.Error()); n != 1 {
					t.Fatalf("stderr sweep diagnostics = %d in %q, want the failed pass reported exactly once", n, out)
				}
				if got := wrapped.listCount(); got != 4 {
					t.Fatalf("storage lists = %d, want recovery, the initial pass, one failing pass, and one later pass: no immediate retry", got)
				}
			})
		})
	}
}

// TestMaintenanceShutdownJoinsTheBlockedPassBeforeStorageTeardown proves the
// shutdown join for maintenance: neither Close nor pure Runtime-context
// cancellation converges while a sweep pass sits inside a storage
// transaction; after release the pass is joined silently, the Core store
// closes only later, and the lock is released.
func TestMaintenanceShutdownJoinsTheBlockedPassBeforeStorageTeardown(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		run := func(t *testing.T, startShutdown func(r *Runtime) <-chan struct{}) {
			t.Helper()
			e := newOwnerEnv(t)
			writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
			wrapped := newSweepStore(store)
			ticks := make(chan time.Time)
			opts := e.options(e.storagePlugin(wrapped))
			opts.sweepTicks = ticks
			r, err := open(context.Background(), opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
			if err != nil {
				t.Fatalf("createSession: %v", err)
			}
			stderr := captureSweepStderr(t)
			wrapped.armBlock()
			sendTick(t, ticks, created.State.LastActivity.Add(100*time.Hour))
			select {
			case <-wrapped.arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the sweep pass never entered the blocked transaction")
			}
			done := startShutdown(r)
			select {
			case <-done:
				t.Fatal("shutdown converged while a sweep pass was blocked inside a storage transaction")
			case <-time.After(200 * time.Millisecond):
			}
			if events := eventNames(e.events.all()); slices.Contains(events, "close:core") {
				t.Fatal("the Core storage closed while a sweep pass was still admitted")
			}
			wrapped.releaseBlock()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the shutdown never joined the released sweep pass")
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("Close = %v, want the joined shutdown to succeed with the pass error kept out of the owner result", err)
			}
			if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
				t.Fatalf("the canceled pass reported to stderr: %q, want cancellation joined silently", out)
			}
			e.assertLockReleased(t)
		}
		t.Run("Close joins the admitted pass", func(t *testing.T) {
			run(t, func(r *Runtime) <-chan struct{} {
				done := make(chan struct{})
				go func() { defer close(done); _ = r.Close(context.Background()) }()
				return done
			})
		})
		t.Run("Runtime-context cancellation joins the admitted pass", func(t *testing.T) {
			run(t, func(r *Runtime) <-chan struct{} {
				r.cancelWork()
				return r.shutdownDone
			})
		})
		t.Run("Runtime-context deadline expiry is a silent cancellation", func(t *testing.T) {
			e := newOwnerEnv(t)
			writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
			wrapped := newSweepStore(store)
			ticks := make(chan time.Time)
			opts := e.options(e.storagePlugin(wrapped))
			opts.sweepTicks = ticks
			ctx := newDeadlineContext()
			r, err := open(ctx, opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
			if err != nil {
				t.Fatalf("createSession: %v", err)
			}
			stderr := captureSweepStderr(t)
			wrapped.armBlock()
			sendTick(t, ticks, created.State.LastActivity.Add(100*time.Hour))
			select {
			case <-wrapped.arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the sweep pass never entered the blocked transaction")
			}
			// The owned work context is a WithCancel child of the expired
			// parent, so its Done is the barrier that the deadline identity
			// has arrived before the release hands it to the real store.
			ctx.expire()
			<-r.work.Done()
			if !errors.Is(r.work.Err(), context.DeadlineExceeded) {
				t.Fatalf("owned context error = %v, want the deadline-expiry identity", r.work.Err())
			}
			wrapped.releaseBlock()
			select {
			case <-r.shutdownDone:
			case <-time.After(10 * time.Second):
				t.Fatal("the deadline-started shutdown never joined the released sweep pass")
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("Close = %v, want the joined deadline cancellation to succeed with the pass error kept out of the owner result", err)
			}
			if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
				t.Fatalf("the deadline-canceled pass reported to stderr: %q, want deadline expiry silent like cancellation", out)
			}
			e.assertLockReleased(t)
		})
	})
}

// TestMaintenanceUnrelatedFailureIsQuietOnceShutdownIsObserved proves the
// accepted race of the selected policy: an ordinary, non-cancellation pass
// failure that converges while the owned context is already done stays quiet
// in exchange for reporting every failure while running. The pass is joined
// by the shutdown like the cancellation rows, and the failing transaction
// never reaches the base store.
func TestMaintenanceUnrelatedFailureIsQuietOnceShutdownIsObserved(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		ticks := make(chan time.Time)
		opts := e.options(e.storagePlugin(wrapped))
		opts.sweepTicks = ticks
		r, err := open(context.Background(), opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		stderr := captureSweepStderr(t)
		unrelated := errors.New("test unrelated store failure")
		wrapped.armFailAfterRelease(unrelated)
		sendTick(t, ticks, created.State.LastActivity.Add(100*time.Hour))
		select {
		case <-wrapped.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the sweep pass never entered the parked transaction")
		}
		r.cancelWork()
		select {
		case <-r.shutdownDone:
			t.Fatal("shutdown converged while a sweep pass was blocked inside a storage transaction")
		case <-time.After(200 * time.Millisecond):
		}
		wrapped.releaseBlock()
		select {
		case <-r.shutdownDone:
		case <-time.After(10 * time.Second):
			t.Fatal("the shutdown never joined the released sweep pass")
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close = %v, want the joined shutdown to succeed with the pass error kept out of the owner result", err)
		}
		if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
			t.Fatalf("the shutdown-observed unrelated failure reported to stderr: %q, want silence once shutdown is observed", out)
		}
		e.assertLockReleased(t)
	})
}

// TestMaintenanceGateRejectionIsQuiet proves the ErrClosed arm of the
// decision on its own: an admitted pass call made against the closed Runtime
// with a live caller context is rejected by the admission gate before the
// store is touched and emits no diagnostic.
func TestMaintenanceGateRejectionIsQuiet(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		r, err := e.open(context.Background(), e.storagePlugin(wrapped))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		stderr := captureSweepStderr(t)
		before := wrapped.listCount()
		r.runSweepPass(context.Background(), time.Now())
		if got := wrapped.listCount(); got != before {
			t.Fatalf("storage lists = %d, want the gate rejection to leave the store untouched (was %d)", got, before)
		}
		if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
			t.Fatalf("the gate-rejected pass reported to stderr: %q, want ErrClosed quiet", out)
		}
	})
}

// TestMaintenanceLeavesRunningSessionsAndAdmitsNoModel proves a pass racing
// live execution: the running Session's durable register is left unchanged,
// the idle sibling is archived, the execution settles untouched, and the
// only preparation/opener calls belong to the explicit submit.
func TestMaintenanceLeavesRunningSessionsAndAdmitsNoModel(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		ticks := make(chan time.Time)
		opts := e.options(e.storagePlugin(wrapped))
		opts.sweepTicks = ticks
		r, err := open(context.Background(), opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		busy, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "busy"), "solo")
		if err != nil {
			t.Fatalf("createSession busy: %v", err)
		}
		idle, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "idle"), "solo")
		if err != nil {
			t.Fatalf("createSession idle: %v", err)
		}
		gate := make(chan struct{})
		e.prep.mu.Lock()
		e.prep.modelGate = gate
		e.prep.mu.Unlock()
		submitThroughRuntime(t, r, busy.Identity.SessionID, "op-busy", "work")
		select {
		case <-e.prep.modelArrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the submitted execution never reached its model effect")
		}
		before := readSweepRegister(t, wrapped, busy.Identity.SessionID)
		tick := idle.State.LastActivity.Add(100 * time.Hour)
		sendTick(t, ticks, tick)
		sendTick(t, ticks, tick) // returns once the first pass fully converged
		after := readSweepRegister(t, wrapped, busy.Identity.SessionID)
		if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatalf("the sweep changed the running Session's register (%d -> %d), want it left unchanged", before.Revision, after.Revision)
		}
		if rec, err := readSweptSession(t, r, idle.Identity.SessionID); err != nil || rec.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("idle sibling after the sweep = %+v err %v, want it archived", rec, err)
		}
		if calls, opens := e.prep.counts(); calls != 1 || opens != 1 {
			t.Fatalf("preparation/opener calls = %d/%d, want only the explicit submit: the sweep admitted no model work", calls, opens)
		}
		close(gate)
		e.prep.awaitCleanups(1)
		if op := readOperation(t, r, busy.Identity.SessionID, "op-busy"); op.State.Status != harness.OperationSuccess {
			t.Fatalf("Operation during the sweep = %+v, want the untouched success terminal", op.State)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenancePassSweepsValidSiblingsAroundCorruption proves the
// corruption axis through the retained scheduler using the Harness
// semantics: a corrupt Session's durable state is left exactly in place, the
// eligible sibling is archived, and the pass reports no failure.
func TestMaintenancePassSweepsValidSiblingsAroundCorruption(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		ticks := make(chan time.Time)
		opts := e.options(e.storagePlugin(wrapped))
		opts.sweepTicks = ticks
		r, err := open(context.Background(), opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		corrupted, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "bad"), "solo")
		if err != nil {
			t.Fatalf("createSession bad: %v", err)
		}
		valid, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "good"), "solo")
		if err != nil {
			t.Fatalf("createSession good: %v", err)
		}
		bad := rewriteSessionRegister(t, wrapped, corrupted.Identity.SessionID, func(state map[string]json.RawMessage) {
			state["lifecycle"] = json.RawMessage(`"bogus"`)
		})
		stderr := captureSweepStderr(t)
		tick := valid.State.LastActivity.Add(100 * time.Hour)
		sendTick(t, ticks, tick)
		sendTick(t, ticks, tick)
		if rec, err := readSweptSession(t, r, valid.Identity.SessionID); err != nil || rec.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("valid sibling after the sweep = %+v err %v, want it archived", rec, err)
		}
		// Shutdown still converges — Close returns, joining the latched
		// corruption of the planted corrupt Session as reporting, not as a
		// failure to converge.
		err = r.Close(context.Background())
		if err == nil {
			t.Fatal("Close = nil, want the joined corruption error for the planted corrupt Session")
		}
		if !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("Close error = %v, want the corruption-class error", err)
		}
		var corrupt *harness.CorruptionError
		if !errors.As(err, &corrupt) || corrupt.SessionID != corrupted.Identity.SessionID {
			t.Fatalf("Close error = %v, want CorruptionError for session %s", err, corrupted.Identity.SessionID)
		}
		if after := readSweepRegister(t, wrapped, corrupted.Identity.SessionID); after.Revision != bad.Revision || !bytes.Equal(after.Payload, bad.Payload) {
			t.Fatalf("the sweep changed the corrupt register (%d -> %d), want it left in place", bad.Revision, after.Revision)
		}
		if out := stderr(); strings.Contains(out, "lightcode: sweep:") {
			t.Fatalf("the corrupt sibling was reported as a pass failure: %q", out)
		}
	})
}

// TestMaintenanceNonpositiveThresholdsDisableTheirTransitions proves the
// day-count translation: with the sweep enabled and both day counts at zero,
// the pass still runs and touches a 100-hour-stale Session at all.
func TestMaintenanceNonpositiveThresholdsDisableTheirTransitions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":0,"delete_after_archive_days":0}`))
		seeded := seedStaleSession(t, store, filepath.Join(e.dataDir, "seeded"), 100*time.Hour)
		wrapped := newSweepStore(store)
		r, err := e.open(context.Background(), e.storagePlugin(wrapped))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got := wrapped.listCount(); got != 2 {
			t.Fatalf("storage lists after startup = %d, want recovery plus exactly one initial pass: zero thresholds still schedule the sweep", got)
		}
		var payload struct {
			State struct {
				Lifecycle string `json:"lifecycle"`
			} `json:"state"`
		}
		reg := readSweepRegister(t, wrapped, seeded)
		if err := json.Unmarshal(reg.Payload, &payload); err != nil {
			t.Fatalf("unmarshal seeded payload: %v", err)
		}
		if payload.State.Lifecycle != string(harness.LifecycleOpen) {
			t.Fatalf("stale Session lifecycle after a zero-threshold pass = %q, want it unchanged open", payload.State.Lifecycle)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := wrapped.listCount(); got != 2 {
			t.Fatalf("storage lists after Close = %d, want no pass beyond the initial one", got)
		}
	})
}

// errSweepRollback is the sentinel the sweep store returns from the armed
// Session's deletion after performing its real effect, so the surrounding
// transaction rolls back a completed mutation.
var errSweepRollback = errors.New("runtime_test: injected post-mutation deletion failure")

// targetedBlockStore wraps one store: once armed with a target, the target
// Session's deletion parks inside its transaction until releaseBlock,
// signalling arrival from inside the park before the real deletion runs.
type targetedBlockStore struct {
	harness.Storage
	mu      sync.Mutex
	target  string
	release chan struct{}
	arrived chan struct{}
}

func newTargetedBlockStore(base harness.Storage) *targetedBlockStore {
	return &targetedBlockStore{Storage: base, arrived: make(chan struct{}, 1)}
}

// block arms the park for one Session identity.
func (s *targetedBlockStore) block(target string) {
	s.mu.Lock()
	s.target = target
	s.release = make(chan struct{})
	s.mu.Unlock()
}

func (s *targetedBlockStore) releaseBlock() {
	s.mu.Lock()
	release := s.release
	s.target = ""
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func (s *targetedBlockStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	return s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(&targetedBlockTransaction{Transaction: tx, store: s})
	})
}

// targetedBlockTransaction forwards every transaction call and parks the
// target Session's deletion before its real effect.
type targetedBlockTransaction struct {
	harness.Transaction
	store *targetedBlockStore
}

func (t *targetedBlockTransaction) DeleteSession(sessionID string) error {
	t.store.mu.Lock()
	target, release := t.store.target, t.store.release
	t.store.mu.Unlock()
	if sessionID == target {
		select {
		case t.store.arrived <- struct{}{}:
		default:
		}
		<-release
	}
	return t.Transaction.DeleteSession(sessionID)
}

// seedStaleArchivedSession creates one idle Session through a standalone
// harness on the shared store and rewrites its durable state to archived with
// an old archived_at, so the automatic sweep is eligible to delete it before
// the owner exists.
func seedStaleArchivedSession(t *testing.T, store harness.Storage, workspace string, stale time.Duration) string {
	t.Helper()
	id := seedStaleSession(t, store, workspace, stale)
	rewriteSessionRegister(t, store, id, func(state map[string]json.RawMessage) {
		state["lifecycle"] = json.RawMessage(`"archived"`)
		state["archived_at"] = mustSweepJSON(t, time.Now().UTC().Add(-stale))
	})
	return id
}

// plantSessionCode creates one Session's artifact tree under DataDir/code —
// a snapshot group directory and an output spill file — and returns its root.
func plantSessionCode(t *testing.T, dataDir, sessionID string) string {
	t.Helper()
	root := filepath.Join(dataDir, "code", sessionID)
	if err := os.MkdirAll(filepath.Join(root, "snapshots", "op-1"), 0o755); err != nil {
		t.Fatalf("plant snapshot group: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "snapshots", "op-1", "snapshot.txt"), []byte("snapshot"), 0o644); err != nil {
		t.Fatalf("plant snapshot: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "output"), 0o755); err != nil {
		t.Fatalf("plant output directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "output", "spill.txt"), []byte("spill"), 0o644); err != nil {
		t.Fatalf("plant spill: %v", err)
	}
	return root
}

// mustNotExist asserts one filesystem path is absent.
func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s = err %v, want it removed", what, err)
	}
}

// mustExist asserts one filesystem path is present.
func mustExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s = err %v, want it present", what, err)
	}
}

// TestPrivateDeleteSessionCleansArtifacts proves the direct deletion row: an
// archived Session's deletion removes its whole artifact tree and nothing
// else; the repeated delete and an unknown valid identity return the same
// idempotent cleanup success; a deletion with no artifact directory at all is
// already clean; and invalid or uncommitted deletions authorize no cleanup.
func TestPrivateDeleteSessionCleansArtifacts(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		ctx := context.Background()
		r, err := e.open(ctx, e.storagePlugin(store))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		archive := func(sessionID string) {
			t.Helper()
			if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, sessionID)
				return err
			}); err != nil {
				t.Fatalf("archive %s: %v", sessionID, err)
			}
		}

		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		archivedID := archived.Identity.SessionID
		archive(archivedID)
		archivedCode := plantSessionCode(t, e.dataDir, archivedID)

		sibling, err := r.createSession(ctx, filepath.Join(e.dataDir, "sibling"), "solo")
		if err != nil {
			t.Fatalf("create sibling: %v", err)
		}
		siblingCode := plantSessionCode(t, e.dataDir, sibling.Identity.SessionID)
		unrelated := filepath.Join(e.dataDir, "unrelated.txt")
		if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
			t.Fatalf("plant unrelated content: %v", err)
		}

		if err := r.deleteSession(ctx, archivedID); err != nil {
			t.Fatalf("deleteSession of an archived session: %v", err)
		}
		mustNotExist(t, archivedCode, "the deleted session's artifact tree")
		mustExist(t, siblingCode, "the sibling session's artifact tree")
		mustExist(t, unrelated, "unrelated data-directory content")
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSession(ctx, archivedID)
			return err
		}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("read after delete = err %v, want ErrNotFound", err)
		}

		// the repeated delete is the same idempotent result with no artifacts left to clean
		if err := r.deleteSession(ctx, archivedID); err != nil {
			t.Fatalf("repeated deleteSession = err %v, want the idempotent cleanup success", err)
		}

		// canonical deletion with no artifact directory at all is already clean
		bare, err := r.createSession(ctx, filepath.Join(e.dataDir, "bare"), "solo")
		if err != nil {
			t.Fatalf("create for the missing-artifacts row: %v", err)
		}
		archive(bare.Identity.SessionID)
		if err := r.deleteSession(ctx, bare.Identity.SessionID); err != nil {
			t.Fatalf("deleteSession with no artifact directory = err %v, want nil", err)
		}

		// an unknown valid identity with planted artifacts still cleans them
		const ghost = "0123456789abcdef0123456789abcdef" // valid hex identity, never registered
		ghostCode := plantSessionCode(t, e.dataDir, ghost)
		if err := r.deleteSession(ctx, ghost); err != nil {
			t.Fatalf("deleteSession of an unknown valid identity = err %v, want the cleanup success", err)
		}
		mustNotExist(t, ghostCode, "the unknown identity's artifacts")

		// invalid and uncommitted deletions remove nothing
		open, err := r.createSession(ctx, filepath.Join(e.dataDir, "open"), "solo")
		if err != nil {
			t.Fatalf("create open: %v", err)
		}
		openCode := plantSessionCode(t, e.dataDir, open.Identity.SessionID)
		if err := r.deleteSession(ctx, open.Identity.SessionID); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("deleteSession of an open session = err %v, want ErrInvalid", err)
		}
		mustExist(t, openCode, "the open session's artifacts after the rejected delete")
		if err := r.deleteSession(ctx, "not-a-session-id"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("deleteSession of a malformed identity = err %v, want ErrInvalid", err)
		}
		mustExist(t, siblingCode, "the sibling session's artifacts after the malformed delete")

		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestPrivateDeleteSessionJoinsShutdown proves the direct path's admission
// join: the deletion parks inside its transaction with the admission held,
// shutdown neither converges nor closes the Core store while parked, and
// after the release the committed deletion's cleanup completes inside the
// same admission before Close returns.
func TestPrivateDeleteSessionJoinsShutdown(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		ctx := context.Background()
		blocked := newTargetedBlockStore(store)
		r, err := open(ctx, e.options(e.storagePlugin(blocked)))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		id := archived.Identity.SessionID
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, id)
			return err
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		code := plantSessionCode(t, e.dataDir, id)

		blocked.block(id)
		done := make(chan error, 1)
		go func() { done <- r.deleteSession(ctx, id) }()
		select {
		case <-blocked.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("deleteSession never parked in the target deletion")
		}
		mustExist(t, code, "the artifact tree before the deletion commits") // the cleanup follows the commit, never precedes it

		closeDone := make(chan struct{})
		go func() { defer close(closeDone); _ = r.Close(ctx) }()
		select {
		case <-closeDone:
			t.Fatal("shutdown converged while the direct deletion's admission was still held")
		case <-time.After(200 * time.Millisecond):
		}
		if events := eventNames(e.events.all()); slices.Contains(events, "close:core") {
			t.Fatal("the Core storage closed while the direct deletion was still admitted")
		}

		blocked.releaseBlock()
		if err := <-done; err != nil {
			t.Fatalf("deleteSession after the release: %v", err)
		}
		<-closeDone
		mustNotExist(t, code, "the artifact tree after the joined deletion")
		if _, err := blocked.ReadRegister(ctx, harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("deleted session register = err %v, want ErrNotFound", err)
		}
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestPrivateDeleteSessionReturnsCleanupErrors proves the direct path's
// cleanup-error half: when the artifact removal fails, deleteSession returns
// the removal error naming the blocked path while the committed deletion
// stays committed — never rolled back or recreated.
func TestPrivateDeleteSessionReturnsCleanupErrors(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		ctx := context.Background()
		r, err := e.open(ctx, e.storagePlugin(store))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		id := archived.Identity.SessionID
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, id)
			return err
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		code := plantSessionCode(t, e.dataDir, id)
		codeRoot := filepath.Join(e.dataDir, "code")
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		if err := os.Chmod(codeRoot, 0o555); err != nil { // denies the removal of the Session's tree: the cleanup fails
			t.Fatalf("chmod the code root: %v", err)
		}
		t.Cleanup(func() { os.Chmod(codeRoot, 0o755) })

		delErr := r.deleteSession(ctx, id)
		if delErr == nil || !strings.Contains(delErr.Error(), code) {
			t.Fatalf("deleteSession with a failing cleanup = err %v, want the removal error naming the blocked tree", delErr)
		}
		if _, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("register after the failed cleanup = err %v, want it gone: the deletion stays committed", err)
		}

		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenanceSweepCleansCommittedSessions proves the partial-sweep row
// through the startup pass: the first Session's committed deletion cleans its
// artifacts even though the second Session's rolled-back delete stops the
// pass with an error, the second Session's artifacts remain, and the one
// retained diagnostic line reports the pass failure.
func TestMaintenanceSweepCleansCommittedSessions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		a := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "a"), 100*time.Hour)
		b := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "b"), 100*time.Hour)
		first, second := a, b // the pass enumerates sorted identities: the rollback targets the second
		if first > second {
			first, second = second, first
		}
		firstCode := plantSessionCode(t, e.dataDir, first)
		secondCode := plantSessionCode(t, e.dataDir, second)
		codeKeep := filepath.Join(e.dataDir, "code", "keep.txt")
		if err := os.WriteFile(codeKeep, []byte("keep"), 0o644); err != nil {
			t.Fatalf("plant code-root content: %v", err)
		}

		wrapped := newSweepStore(store)
		wrapped.armDeleteTarget(second)
		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(wrapped)))
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		mustNotExist(t, firstCode, "the committed deletion's artifacts despite the stopping pass error")
		mustExist(t, secondCode, "the rolled-back deletion's artifacts")
		mustExist(t, codeKeep, "unrelated code-root content")
		if _, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: first, Kind: harness.RegisterSession}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("committed deletion's register = err %v, want ErrNotFound", err)
		}
		readSweepRegister(t, wrapped, second) // the rolled-back deletion left the Session present

		out := stderr()
		if n := strings.Count(out, "lightcode: sweep: "+errSweepRollback.Error()); n != 1 {
			t.Fatalf("stderr sweep diagnostics = %d in %q, want one line reporting the stopping pass error", n, out)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenanceSweepJoinsPassAndCleanupErrorsInOneDiagnostic proves the
// single-diagnostic row: a stopping pass error and a failing cleanup are
// joined into ONE retained stderr line — the prefix appears exactly once and
// the line carries both errors.
func TestMaintenanceSweepJoinsPassAndCleanupErrorsInOneDiagnostic(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		a := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "a"), 100*time.Hour)
		b := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "b"), 100*time.Hour)
		first, second := a, b // the pass enumerates sorted identities: the rollback targets the second
		if first > second {
			first, second = second, first
		}
		firstCode := plantSessionCode(t, e.dataDir, first)
		plantSessionCode(t, e.dataDir, second)
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		codeRoot := filepath.Join(e.dataDir, "code")
		if err := os.Chmod(codeRoot, 0o555); err != nil { // denies the committed deletion's cleanup
			t.Fatalf("chmod the code root: %v", err)
		}
		t.Cleanup(func() { os.Chmod(codeRoot, 0o755) })

		wrapped := newSweepStore(store)
		wrapped.armDeleteTarget(second)
		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(wrapped)))
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		out := stderr()
		if n := strings.Count(out, "lightcode: sweep:"); n != 1 {
			t.Fatalf("stderr sweep diagnostics = %d in %q, want exactly one line for the whole pass", n, out)
		}
		if !strings.Contains(out, errSweepRollback.Error()) || !strings.Contains(out, firstCode) {
			t.Fatalf("stderr sweep diagnostic %q, want it to carry both the stopping pass error and the failing cleanup", out)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenanceSweepReportsCleanupErrorsAndAttemptsEveryID proves the
// cleanup-error row: both committed deletions are cleaned up — one failing,
// one succeeding — the failure is reported through the one retained
// diagnostic line, and neither committed deletion is rolled back or rerun.
func TestMaintenanceSweepReportsCleanupErrorsAndAttemptsEveryID(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		a := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "a"), 100*time.Hour)
		b := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "b"), 100*time.Hour)
		aCode := plantSessionCode(t, e.dataDir, a)
		bCode := plantSessionCode(t, e.dataDir, b)
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		blocked := filepath.Join(aCode, "snapshots") // denies the removal of its contents: the cleanup of a fails
		if err := os.Chmod(blocked, 0o555); err != nil {
			t.Fatalf("chmod the blocked directory: %v", err)
		}
		t.Cleanup(func() { os.Chmod(blocked, 0o755) })

		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(store)))
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		for _, id := range []string{a, b} { // committed deletions are never rolled back
			if _, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("committed deletion of %s = err %v, want the register gone despite the cleanup failure", id, err)
			}
		}
		mustExist(t, filepath.Join(aCode, "snapshots", "op-1"), "the failing cleanup's blocked directory")
		mustNotExist(t, bCode, "the succeeding cleanup's artifact tree")

		out := stderr()
		if n := strings.Count(out, "lightcode: sweep:"); n != 1 {
			t.Fatalf("stderr sweep diagnostics = %d in %q, want one line for the whole pass", n, out)
		}
		if !strings.Contains(out, filepath.Join(aCode, "snapshots")) {
			t.Fatalf("stderr sweep diagnostic %q, want it to report the failing cleanup's path", out)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestMaintenanceShutdownJoinsSweepCleanup proves the shutdown row for
// cleanup: with the first Session's deletion committed, the pass parks inside
// the second Session's deletion and shutdown waits without converging and
// without closing the Core store; after the release, the committed first
// deletion's cleanup completes inside the joined admission before Close
// returns.
func TestMaintenanceShutdownJoinsSweepCleanup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":1}`))
		a := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "a"), 100*time.Hour)
		b := seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, "b"), 100*time.Hour)
		first, second := a, b // the pass enumerates sorted identities: the park targets the second
		if first > second {
			first, second = second, first
		}
		firstCode := plantSessionCode(t, e.dataDir, first)
		plantSessionCode(t, e.dataDir, second)

		blocked := newTargetedBlockStore(store)
		ticks := make(chan time.Time)
		opts := e.options(e.storagePlugin(blocked))
		opts.sweepTicks = ticks
		r, err := open(context.Background(), opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		if _, err := r.Reload(context.Background()); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		blocked.block(second)
		sendTick(t, ticks, time.Now().Add(2*time.Hour)) // the pass deletes first, then parks in second's deletion
		select {
		case <-blocked.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the sweep pass never parked in the target deletion")
		}
		mustExist(t, firstCode, "the committed deletion's artifacts before the pass's cleanup")

		done := make(chan struct{})
		go func() { defer close(done); _ = r.Close(context.Background()) }()
		select {
		case <-done:
			t.Fatal("shutdown converged while the pass's cleanup admission was still held")
		case <-time.After(200 * time.Millisecond):
		}
		if events := eventNames(e.events.all()); slices.Contains(events, "close:core") {
			t.Fatal("the Core storage closed while the sweep cleanup was still admitted")
		}

		blocked.releaseBlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the shutdown never joined the released pass")
		}
		mustNotExist(t, firstCode, "the committed deletion's artifacts after the joined pass")
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
