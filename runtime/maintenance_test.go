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
// error, and parks the next transaction on a test release.
type sweepStore struct {
	harness.Storage
	mu       sync.Mutex
	lists    int
	failErr  error
	failLeft int
	blocked  bool
	release  chan struct{}
	arrived  chan struct{}
	written  chan struct{}
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

// armBlock parks the next transaction until releaseBlock, signalling arrival
// from inside the park.
func (s *sweepStore) armBlock() {
	s.mu.Lock()
	s.blocked = true
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
	park, release := s.blocked, s.release
	s.mu.Unlock()
	if park {
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		<-release
	}
	wrote := false
	err := s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(&writeSpy{Transaction: tx, wrote: &wrote})
	})
	if err == nil && wrote {
		select {
		case s.written <- struct{}{}:
		default:
		}
	}
	return err
}

// writeSpy reports whether one transaction performed a register mutation.
type writeSpy struct {
	harness.Transaction
	wrote *bool
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
// pass row: one non-cancellation storage error reaches the retained stderr
// diagnostic exactly once, with no retry, and the next tick runs again.
func TestMaintenanceFailedPassReportsAndWaitsForTheNextTick(t *testing.T) {
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
		wrapped.armFailOnce(errors.New("test sweep store listing failure"))
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
		if n := strings.Count(out, "lightcode: sweep: test sweep store listing failure"); n != 1 {
			t.Fatalf("stderr sweep diagnostics = %d in %q, want the failed pass reported exactly once", n, out)
		}
		if got := wrapped.listCount(); got != 4 {
			t.Fatalf("storage lists = %d, want recovery, the initial pass, one failing pass, and one later pass: no immediate retry", got)
		}
	})
}

// TestMaintenanceDeadlineValuedFailureReportsWhileTheOwnerIsLive is the
// nearest sibling of the silent deadline cancellation: a storage failure
// carrying the deadline-expiry identity while the Runtime-owned context is
// still alive is an ordinary pass failure, reaches the retained stderr
// diagnostic exactly once, and leaves the later-tick cadence intact.
func TestMaintenanceDeadlineValuedFailureReportsWhileTheOwnerIsLive(t *testing.T) {
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
		wrapped.armFailOnce(context.DeadlineExceeded)
		tick := created.State.LastActivity.Add(100 * time.Hour)
		sendTick(t, ticks, tick) // this pass lists, fails with the deadline identity, and reports
		sendTick(t, ticks, tick) // the rendezvous proves the deadline-failed pass converged; this pass sweeps again
		waitWritten(t, wrapped)  // the later tick committed the archive
		if rec, err := readSweptSession(t, r, created.Identity.SessionID); err != nil || rec.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("Session after the later tick = %+v err %v, want the deadline-failed pass to leave no lasting damage", rec, err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if out := stderr(); strings.Count(out, "lightcode: sweep: context deadline exceeded") != 1 {
			t.Fatalf("stderr sweep diagnostics = %q, want the live-owner deadline-valued failure reported exactly once", out)
		}
		if got := wrapped.listCount(); got != 4 {
			t.Fatalf("storage lists = %d, want recovery, the initial pass, one deadline-failed pass, and one later pass: no immediate retry", got)
		}
	})
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
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
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
