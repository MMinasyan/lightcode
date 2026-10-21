package runtime

// Automatic lifecycle-sweep scheduling tests: the retained startup/hourly
// cadence drives the existing Harness.SweepSession transition under the
// sampled session policy, with controlled tick streams, one owned loop,
// ordinary admitted-call join, and the retained stderr diagnostic. Each case
// runs on the real owner over memory and temporary SQLite through the
// existing fixtures; HOME and bundled credentials are isolated.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// ownerSweepDocument builds one owner configuration document with the fixed
// prov/m catalog and an explicit sessions section. The unroutable endpoint
// stays: the sweep rows that admit no model work never reach it, and the two
// startup-probe rows below build their own local-endpoint document.
func ownerSweepDocument(sessions string) string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "", false,
		`{"m":{"name":"M","context_window":4096}}`) + `},"sessions":` + sessions + `}`
}

// ownerSweepProbedDocument builds the sweep fixture's local-endpoint document
// with a generous window and the hooks plugin section, so an accidental sweep
// admission reaches the selected preparation hook and the local model server
// instead of failing for absent metadata.
func ownerSweepProbedDocument(endpoint, sessions string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "", false,
		`{"m":{"name":"M","context_window":262144,"max_output_tokens":4096}}`) + `},"plugins":{"hooks":{"tag":"T1"}},"sessions":` + sessions + `}`
}

// ownerSweepAgentsDocument selects the local model and the preparation hook
// for both agent types the sweep fixtures create.
const ownerSweepAgentsDocument = `{"solo":{"model":"prov/m","system_prompt":"simple","capabilities":["hook.first"]},"seeded":{"model":"prov/m","system_prompt":"simple","capabilities":["hook.first"]}}`

// assertNoOperationRegisters proves one Session carries no Operation
// register: no admission created durable Operation state.
func assertNoOperationRegisters(t *testing.T, store harness.Storage, sessionID string) {
	t.Helper()
	regs, err := store.ReadRegisters(context.Background(), sessionID)
	if err != nil {
		if errors.Is(err, harness.ErrNotFound) {
			return // a deleted Session carries no registers: no admission exists
		}
		t.Fatalf("ReadRegisters(%s): %v", sessionID, err)
	}
	for _, reg := range regs {
		testassert.Require(t, reg.Key.Kind != harness.RegisterOperation, "session %s carries the Operation register %q, want no admission", sessionID, reg.Key.OperationID)
	}
}

// captureSweepStderr redirects os.Stderr into a temp file for the test's
// duration and returns a func that reads what was written.
func captureSweepStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "sweep-stderr-*")
	testassert.NoError(t, err)
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
// error, fails one named Session's register read (the candidate's
// validation-class fault), parks the next transaction on a test release, and
// can fail one named Session's deletion after its real effect.
type sweepStore struct {
	harness.Storage
	mu           sync.Mutex
	lists        int
	failErr      error
	failLeft     int
	readErrFor   map[string]error
	blocked      bool
	releaseErr   error
	release      chan struct{}
	arrived      chan struct{}
	written      chan struct{}
	deleteTarget string
}

func newSweepStore(base harness.Storage) *sweepStore {
	return &sweepStore{Storage: base, readErrFor: map[string]error{}, arrived: make(chan struct{}, 1), written: make(chan struct{}, 8)}
}

// armReadFailure makes one Session's register read fail until it is disarmed,
// the validation-class fault that stops a pass at its candidate load.
func (s *sweepStore) armReadFailure(sessionID string, err error) {
	s.mu.Lock()
	s.readErrFor[sessionID] = err
	s.mu.Unlock()
}

// disarmReadFailure clears one Session's armed read failure.
func (s *sweepStore) disarmReadFailure(sessionID string) {
	s.mu.Lock()
	delete(s.readErrFor, sessionID)
	s.mu.Unlock()
}

func (s *sweepStore) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	s.mu.Lock()
	failure := s.readErrFor[sessionID]
	s.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	return s.Storage.ReadRegisters(ctx, sessionID)
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

func readSweepRegister(t *testing.T, store harness.Storage, sessionID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	testassert.Require(t, err == nil, "read session register %q: %v", sessionID, err)
	return reg
}

// rewriteSessionRegister replaces one Session register's durable state
// section under its current revision and returns the rewritten register.
func rewriteSessionRegister(t *testing.T, store harness.Storage, sessionID string, mutate func(state map[string]json.RawMessage)) harness.Register {
	t.Helper()
	ctx := context.Background()
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
	reg, err := store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	var obj map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(reg.Payload, &obj))
	var state map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(obj["state"], &state))
	mutate(state)
	obj["state"] = mustSweepJSON(t, state)
	testassert.NoError(t, store.Transact(ctx, func(tx harness.Transaction) error {
		_, err := tx.ReplaceRegister(key, reg.Revision, mustSweepJSON(t, obj))
		return err
	}))
	out, err := store.ReadRegister(ctx, key)
	testassert.NoError(t, err)
	return out
}

func mustSweepJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	rec, err := h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: workspace, AgentType: "seeded"})
	testassert.NoError(t, err)
	rewriteSessionRegister(t, store, rec.Identity.SessionID, func(state map[string]json.RawMessage) {
		var last time.Time
		testassert.NoError(t, json.Unmarshal(state["last_activity"], &last))
		state["last_activity"] = mustSweepJSON(t, last.Add(-stale))
	})
	return rec.Identity.SessionID
}

// TestMaintenanceInitialPassRunsAtStartup proves the startup pass: one
// automatic sweep completes before the owner publication returns, under the
// initial policy, through the real Harness transition on both stores, with
// no model admission. The fixture's agent types select the local model and a
// real preparation hook, so accidental work would reach the probe rather
// than fail for absent metadata.
func TestMaintenanceInitialPassRunsAtStartup(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepProbedDocument(e.server.URL, `{"archive_after_days":1,"delete_after_archive_days":365}`))
		writeServiceFile(t, agents.PathForConfig(e.configPath), ownerSweepAgentsDocument)
		seeded := seedStaleSession(t, store, filepath.Join(e.dataDir, "seeded"), 100*time.Hour)
		hook := &recordHook{name: "hook.first", events: &traceLog{}}
		var opOpens, agOpens atomic.Int64
		r, err := e.open(context.Background(),
			e.storagePlugin(store),
			hookPlugin(hook),
			shortScopeProbe("probe", ScopeOperation, &opOpens),
			shortScopeProbe("probe-agent", ScopeAgent, &agOpens))
		testassert.NoError(t, err)
		rec := headerThroughRuntime(t, r, seeded)
		testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived && rec.ArchivedAt != nil, "seeded Session after startup = %+v, want the initial pass to have archived it", rec)
		testassert.Equal(t, e.server.requests(), 0)
		testassert.Equal(t, len(hook.calls()), 0)
		{
			ops, ags := opOpens.Load(), agOpens.Load()
			testassert.Require(t, ops == 0 && ags == 0, "short-scope factory opens during startup = %d/%d, want the sweep to admit no model work", ops, ags)
		}
		assertNoOperationRegisters(t, store, seeded)
		testassert.NoError(t, r.Close(context.Background()))
	})
}

// TestMaintenanceControlledTicksSweepUnderTheCurrentPolicy proves the
// retained policy translation and the tick-driven cadence over one owned
// loop: auto_archive=false disables the whole sweep even at construction,
// Reloaded policy is sampled by the next pass, each tick timestamp is that
// pass's explicit time at the exact 24-hour archive/delete boundaries, and
// the pass admits no model work. The created Session's agent type selects
// the local model and a real preparation hook, so accidental work would
// reach the probe rather than fail for absent metadata.
func TestMaintenanceControlledTicksSweepUnderTheCurrentPolicy(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepProbedDocument(e.server.URL, `{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":1}`))
		writeServiceFile(t, agents.PathForConfig(e.configPath), ownerSweepAgentsDocument)
		wrapped := newSweepStore(store)
		hook := &recordHook{name: "hook.first", events: &traceLog{}}
		var opOpens, agOpens atomic.Int64
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped), hookPlugin(hook),
			shortScopeProbe("probe", ScopeOperation, &opOpens),
			shortScopeProbe("probe-agent", ScopeAgent, &agOpens))
		testassert.NoError(t, err)
		testassert.Equal(t, wrapped.listCount(), 1)
		created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
		testassert.NoError(t, err)
		id := created.Identity.SessionID
		stderr := captureSweepStderr(t)

		archiveBoundary := created.State.LastActivity.Add(24 * time.Hour)
		archivedAt := archiveBoundary.Add(time.Nanosecond)
		writeServiceFile(t, e.configPath, ownerSweepProbedDocument(e.server.URL, `{"archive_after_days":1,"delete_after_archive_days":1}`))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}

		// Each send's rendezvous proves the previous pass fully converged, so
		// the boundary/no-op and transition passes cannot interleave.
		sendTick(t, ticks, archiveBoundary) // exact archive boundary: no transition
		sendTick(t, ticks, archivedAt)      // one nanosecond past: archive, stamped with the tick time
		waitWritten(t, wrapped)             // the archive committed; the boundary pass converged at the previous rendezvous
		archived := headerThroughRuntime(t, r, id)
		testassert.Require(t, archived.Lifecycle == harness.LifecycleArchived && archived.Revision.DurableRevision == created.Revision+1 && archived.ArchivedAt != nil && archived.ArchivedAt.Equal(archivedAt), "Session after the two archive-boundary ticks = %+v rev %d, want exactly one archive at revision %d; sweep stamped %v, want the archive tick's explicit time %v", archived, archived.Revision.DurableRevision, created.Revision+1, archived.ArchivedAt, archivedAt)
		sendTick(t, ticks, archivedAt.Add(24*time.Hour))                 // exact delete boundary
		sendTick(t, ticks, archivedAt.Add(24*time.Hour+time.Nanosecond)) // one nanosecond past: delete
		waitWritten(t, wrapped)
		testassert.NoError(t, r.Close(context.Background()))
		{
			_, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "Session register after the delete-boundary pair = err %v, want it past the boundary deleted", err)
		}
		testassert.Equal(t, wrapped.listCount(), 5)
		testassert.Equal(t, e.server.requests(), 0)
		testassert.Equal(t, len(hook.calls()), 0)
		{
			ops, ags := opOpens.Load(), agOpens.Load()
			testassert.Require(t, ops == 0 && ags == 0, "short-scope factory opens = %d/%d, want none: the sweep admits no model work", ops, ags)
		}
		assertNoOperationRegisters(t, wrapped, id)
		{
			out := stderr()
			testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "boundary passes reported diagnostics: %q", out)
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
				r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
				testassert.NoError(t, err)
				created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
				testassert.NoError(t, err)
				stderr := captureSweepStderr(t)
				wrapped.armFailOnce(tc.failure)
				tick := created.State.LastActivity.Add(100 * time.Hour)
				sendTick(t, ticks, tick) // this pass lists, fails, and reports
				sendTick(t, ticks, tick) // the rendezvous proves the failed pass converged; this pass sweeps again
				waitWritten(t, wrapped)  // the later tick committed the archive
				rec := headerThroughRuntime(t, r, created.Identity.SessionID)
				testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived, "Session after the later tick = %+v, want the failed pass to leave no lasting damage", rec)
				testassert.NoError(t, r.Close(context.Background()))
				out := stderr()
				{
					n := strings.Count(out, "lightcode: sweep: "+tc.failure.Error())
					testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want the failed pass reported exactly once", n, out)
				}
				testassert.Equal(t, wrapped.listCount(), 4)
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
			r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
			testassert.NoError(t, err)
			created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
			testassert.NoError(t, err)
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
			{
				events := eventNames(e.events.all())
				testassert.Require(t, !slices.Contains(events, "close:core"), "the Core storage closed while a sweep pass was still admitted")
			}
			wrapped.releaseBlock()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the shutdown never joined the released sweep pass")
			}
			testassert.NoError(t, r.Close(context.Background()))
			{
				out := stderr()
				testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "the canceled pass reported to stderr: %q, want cancellation joined silently", out)
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
			ctx := newDeadlineContext()
			r, ticks, err := e.openSwept(ctx, e.storagePlugin(wrapped))
			testassert.NoError(t, err)
			created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
			testassert.NoError(t, err)
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
			testassert.NoError(t, r.Close(context.Background()))
			{
				out := stderr()
				testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "the deadline-canceled pass reported to stderr: %q, want deadline expiry silent like cancellation", out)
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
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
		testassert.NoError(t, err)
		created, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "sweep"), "solo")
		testassert.NoError(t, err)
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
		testassert.NoError(t, r.Close(context.Background()))
		{
			out := stderr()
			testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "the shutdown-observed unrelated failure reported to stderr: %q, want silence once shutdown is observed", out)
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
		testassert.NoError(t, err)
		testassert.NoError(t, r.Close(context.Background()))
		stderr := captureSweepStderr(t)
		before := wrapped.listCount()
		r.runSweepPass(context.Background(), time.Now())
		testassert.Equal(t, wrapped.listCount(), before)
		{
			out := stderr()
			testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "the gate-rejected pass reported to stderr: %q, want ErrClosed quiet", out)
		}
	})
}

// TestMaintenanceLeavesRunningSessionsAndAdmitsNoModel proves a pass racing
// live execution: the running Session's durable register is left unchanged,
// the idle sibling is archived, the execution settles untouched, and the
// only model request belongs to the explicit submit.
func TestMaintenanceLeavesRunningSessionsAndAdmitsNoModel(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepProbedDocument(e.server.URL, `{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped), hookPlugin(&recordHook{name: "hook.first", events: &traceLog{}}))
		testassert.NoError(t, err)
		busy, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "busy"), "solo")
		testassert.NoError(t, err)
		idle, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "idle"), "solo")
		testassert.NoError(t, err)
		gate := make(chan struct{})
		e.server.setHold(gate)
		submitThroughRuntime(t, r, busy.Identity.SessionID, "op-busy", "work")
		select {
		case <-e.server.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the submitted execution never reached its model request")
		}
		before := readSweepRegister(t, wrapped, busy.Identity.SessionID)
		tick := idle.State.LastActivity.Add(100 * time.Hour)
		sendTick(t, ticks, tick)
		sendTick(t, ticks, tick) // returns once the first pass fully converged
		after := readSweepRegister(t, wrapped, busy.Identity.SessionID)
		testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "the sweep changed the running Session's register (%d -> %d), want it left unchanged", before.Revision, after.Revision)
		rec := headerThroughRuntime(t, r, idle.Identity.SessionID)
		testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived, "idle sibling after the sweep = %+v, want it archived", rec)
		testassert.Equal(t, e.server.requests(), 1)
		close(gate)
		{
			op := awaitOperation(t, r, busy.Identity.SessionID, "op-busy", harness.OperationSuccess)
			testassert.Require(t, op.State.Status == harness.OperationSuccess, "Operation during the sweep = %+v, want the untouched success terminal", op.State)
		}
		testassert.NoError(t, r.Close(context.Background()))
	})
}

// TestMaintenancePassSweepsValidSiblingsAroundCorruption proves the
// corruption and omitted-row axis through the retained scheduler using the
// Harness semantics: a corrupt Session's durable state is left exactly in
// place, a malformed listed identity and a register corrupt before its first
// materialization are both omitted, the eligible sibling is archived, and the
// pass reports no failure.
func TestMaintenancePassSweepsValidSiblingsAroundCorruption(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		wrapped := newSweepStore(store)
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
		testassert.NoError(t, err)
		corrupted, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "bad"), "solo")
		testassert.NoError(t, err)
		valid, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "good"), "solo")
		testassert.NoError(t, err)
		bad := rewriteSessionRegister(t, wrapped, corrupted.Identity.SessionID, func(state map[string]json.RawMessage) {
			state["lifecycle"] = json.RawMessage(`"bogus"`)
		})

		// Two more rows the same pass must omit: a listed identity that
		// violates the durable shape is skipped before any materialization,
		// and a valid identity whose register is corrupt before its first
		// materialization is omitted when the enumeration discovers the
		// corruption. Neither is acted on and neither is reported.
		const malformed = "corrupt-id"                        // non-empty, not the durable 32-hex shape
		const preCorrupt = "fedcba9876543210fedcba9876543210" // valid hex, corrupt register
		for _, row := range []string{malformed, preCorrupt} {
			lifecycleInsertRegister(t, wrapped, harness.RegisterKey{SessionID: row, Kind: harness.RegisterSession}, `{}`)
		}
		stderr := captureSweepStderr(t)
		tick := valid.State.LastActivity.Add(100 * time.Hour)
		sendTick(t, ticks, tick)
		sendTick(t, ticks, tick)
		rec := headerThroughRuntime(t, r, valid.Identity.SessionID)
		testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived, "valid sibling after the sweep = %+v, want it archived", rec)
		// Shutdown still converges — Close returns, joining the latched
		// corruption of the planted corrupt Session as reporting, not as a
		// failure to converge.
		err = r.Close(context.Background())
		testassert.Require(t, err != nil, "Close = nil, want the joined corruption error for the planted corrupt Session")
		testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "Close error = %v, want the corruption-class error", err)
		var corrupt *harness.CorruptionError
		testassert.Require(t, errors.As(err, &corrupt) && corrupt.SessionID == corrupted.Identity.SessionID, "Close error = %v, want CorruptionError for session %s", err, corrupted.Identity.SessionID)
		{
			after := readSweepRegister(t, wrapped, corrupted.Identity.SessionID)
			testassert.Require(t, after.Revision == bad.Revision && bytes.Equal(after.Payload, bad.Payload), "the sweep changed the corrupt register (%d -> %d), want it left in place", bad.Revision, after.Revision)
		}
		for _, id := range []string{malformed, preCorrupt} {
			{
				_, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
				testassert.Require(t, err == nil, "omitted row %s after the pass = err %v, want it left in place", id, err)
			}
		}
		{
			out := stderr()
			testassert.Require(t, !strings.Contains(out, "lightcode: sweep:"), "the corrupt sibling was reported as a pass failure: %q", out)
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
		testassert.NoError(t, err)
		testassert.Equal(t, wrapped.listCount(), 2)
		var payload struct {
			State struct {
				Lifecycle string `json:"lifecycle"`
			} `json:"state"`
		}
		reg := readSweepRegister(t, wrapped, seeded)
		testassert.NoError(t, json.Unmarshal(reg.Payload, &payload))
		testassert.Require(t, payload.State.Lifecycle == string(harness.LifecycleOpen), "stale Session lifecycle after a zero-threshold pass = %q, want it unchanged open", payload.State.Lifecycle)
		testassert.NoError(t, r.Close(context.Background()))
		testassert.Equal(t, wrapped.listCount(), 2)
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
	testassert.NoError(t, os.MkdirAll(filepath.Join(root, "snapshots", "op-1"), 0o755))
	testassert.NoError(t, os.WriteFile(filepath.Join(root, "snapshots", "op-1", "snapshot.txt"), []byte("snapshot"), 0o644))
	testassert.NoError(t, os.MkdirAll(filepath.Join(root, "output"), 0o755))
	testassert.NoError(t, os.WriteFile(filepath.Join(root, "output", "spill.txt"), []byte("spill"), 0o644))
	return root
}

// mustNotExist asserts one filesystem path is absent.
func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "%s = err %v, want it removed", what, err)
	}
}

// mustExist asserts one filesystem path is present.
func mustExist(t *testing.T, path, what string) {
	t.Helper()
	{
		_, err := os.Stat(path)
		testassert.Require(t, err == nil, "%s = err %v, want it present", what, err)
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
		testassert.NoError(t, err)
		archive := func(sessionID string) {
			t.Helper()
			{
				err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
					_, err := h.ArchiveSession(ctx, sessionID)
					return err
				})
				testassert.Require(t, err == nil, "archive %s: %v", sessionID, err)
			}
		}

		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		testassert.NoError(t, err)
		archivedID := archived.Identity.SessionID
		archive(archivedID)
		archivedCode := plantSessionCode(t, e.dataDir, archivedID)

		sibling, err := r.createSession(ctx, filepath.Join(e.dataDir, "sibling"), "solo")
		testassert.NoError(t, err)
		siblingCode := plantSessionCode(t, e.dataDir, sibling.Identity.SessionID)
		unrelated := filepath.Join(e.dataDir, "unrelated.txt")
		testassert.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o644))

		testassert.NoError(t, r.deleteSession(ctx, archivedID))
		mustNotExist(t, archivedCode, "the deleted session's artifact tree")
		mustExist(t, siblingCode, "the sibling session's artifact tree")
		mustExist(t, unrelated, "unrelated data-directory content")
		{
			err := headerErrorThroughRuntime(ctx, r, archivedID)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read after delete = err %v, want ErrNotFound", err)
		}

		// the repeated delete is the same idempotent result with no artifacts left to clean
		testassert.NoError(t, r.deleteSession(ctx, archivedID))

		// canonical deletion with no artifact directory at all is already clean
		bare, err := r.createSession(ctx, filepath.Join(e.dataDir, "bare"), "solo")
		testassert.NoError(t, err)
		archive(bare.Identity.SessionID)
		testassert.NoError(t, r.deleteSession(ctx, bare.Identity.SessionID))

		// an unknown valid identity with planted artifacts still cleans them
		const ghost = "0123456789abcdef0123456789abcdef" // valid hex identity, never registered
		ghostCode := plantSessionCode(t, e.dataDir, ghost)
		testassert.NoError(t, r.deleteSession(ctx, ghost))
		mustNotExist(t, ghostCode, "the unknown identity's artifacts")

		// invalid and uncommitted deletions remove nothing
		open, err := r.createSession(ctx, filepath.Join(e.dataDir, "open"), "solo")
		testassert.NoError(t, err)
		openCode := plantSessionCode(t, e.dataDir, open.Identity.SessionID)
		{
			err := r.deleteSession(ctx, open.Identity.SessionID)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "deleteSession of an open session = err %v, want ErrInvalid", err)
		}
		mustExist(t, openCode, "the open session's artifacts after the rejected delete")
		{
			err := r.deleteSession(ctx, "not-a-session-id")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "deleteSession of a malformed identity = err %v, want ErrInvalid", err)
		}
		mustExist(t, siblingCode, "the sibling session's artifacts after the malformed delete")

		testassert.NoError(t, r.Close(ctx))
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
		testassert.NoError(t, err)
		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		testassert.NoError(t, err)
		id := archived.Identity.SessionID
		testassert.NoError(t, r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, id)
			return err
		}))
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
		{
			events := eventNames(e.events.all())
			testassert.Require(t, !slices.Contains(events, "close:core"), "the Core storage closed while the direct deletion was still admitted")
		}

		blocked.releaseBlock()
		testassert.NoError(t, <-done)
		<-closeDone
		mustNotExist(t, code, "the artifact tree after the joined deletion")
		{
			_, err := blocked.ReadRegister(ctx, harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted session register = err %v, want ErrNotFound", err)
		}
		testassert.NoError(t, r.Close(ctx))
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
		testassert.NoError(t, err)
		archived, err := r.createSession(ctx, filepath.Join(e.dataDir, "ws"), "solo")
		testassert.NoError(t, err)
		id := archived.Identity.SessionID
		testassert.NoError(t, r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, id)
			return err
		}))
		code := plantSessionCode(t, e.dataDir, id)
		codeRoot := filepath.Join(e.dataDir, "code")
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		// denies the removal of the Session's tree: the cleanup fails
		testassert.NoError(t, os.Chmod(codeRoot, 0o555))
		t.Cleanup(func() { os.Chmod(codeRoot, 0o755) })

		delErr := r.deleteSession(ctx, id)
		testassert.Require(t, delErr != nil && strings.Contains(delErr.Error(), code), "deleteSession with a failing cleanup = err %v, want the removal error naming the blocked tree", delErr)
		{
			_, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "register after the failed cleanup = err %v, want it gone: the deletion stays committed", err)
		}

		testassert.NoError(t, r.Close(ctx))
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
		testassert.NoError(t, os.WriteFile(codeKeep, []byte("keep"), 0o644))

		wrapped := newSweepStore(store)
		wrapped.armDeleteTarget(second)
		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(wrapped)))
		testassert.NoError(t, err)

		mustNotExist(t, firstCode, "the committed deletion's artifacts despite the stopping pass error")
		mustExist(t, secondCode, "the rolled-back deletion's artifacts")
		mustExist(t, codeKeep, "unrelated code-root content")
		{
			_, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: first, Kind: harness.RegisterSession})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "committed deletion's register = err %v, want ErrNotFound", err)
		}
		readSweepRegister(t, wrapped, second) // the rolled-back deletion left the Session present

		out := stderr()
		{
			n := strings.Count(out, "lightcode: sweep: "+errSweepRollback.Error())
			testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want one line reporting the stopping pass error", n, out)
		}
		testassert.NoError(t, r.Close(context.Background()))
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
		// denies the committed deletion's cleanup
		testassert.NoError(t, os.Chmod(codeRoot, 0o555))
		t.Cleanup(func() { os.Chmod(codeRoot, 0o755) })

		wrapped := newSweepStore(store)
		wrapped.armDeleteTarget(second)
		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(wrapped)))
		testassert.NoError(t, err)

		out := stderr()
		{
			n := strings.Count(out, "lightcode: sweep:")
			testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want exactly one line for the whole pass", n, out)
		}
		testassert.Require(t, strings.Contains(out, errSweepRollback.Error()) && strings.Contains(out, firstCode), "stderr sweep diagnostic %q, want it to carry both the stopping pass error and the failing cleanup", out)
		testassert.NoError(t, r.Close(context.Background()))
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
		testassert.NoError(t, os.Chmod(blocked, 0o555))
		t.Cleanup(func() { os.Chmod(blocked, 0o755) })

		stderr := captureSweepStderr(t)
		r, err := open(context.Background(), e.options(e.storagePlugin(store)))
		testassert.NoError(t, err)

		for _, id := range []string{a, b} { // committed deletions are never rolled back
			{
				_, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "committed deletion of %s = err %v, want the register gone despite the cleanup failure", id, err)
			}
		}
		mustExist(t, filepath.Join(aCode, "snapshots", "op-1"), "the failing cleanup's blocked directory")
		mustNotExist(t, bCode, "the succeeding cleanup's artifact tree")

		out := stderr()
		{
			n := strings.Count(out, "lightcode: sweep:")
			testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want one line for the whole pass", n, out)
		}
		testassert.Require(t, strings.Contains(out, filepath.Join(aCode, "snapshots")), "stderr sweep diagnostic %q, want it to report the failing cleanup's path", out)
		testassert.NoError(t, r.Close(context.Background()))
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
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(blocked))
		testassert.NoError(t, err)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
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
		{
			events := eventNames(e.events.all())
			testassert.Require(t, !slices.Contains(events, "close:core"), "the Core storage closed while the sweep cleanup was still admitted")
		}

		blocked.releaseBlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the shutdown never joined the released pass")
		}
		mustNotExist(t, firstCode, "the committed deletion's artifacts after the joined pass")
		testassert.NoError(t, r.Close(context.Background()))
	})
}

// TestMaintenanceSweepRemovesWarningsForEveryCommittedDeletion proves the
// batch warning cleanup over the sweep pass: with the third stale
// archived Session's deletion rolled back, both committed deletions — and only
// those — have their Session warning groups removed before the pass error
// returns, in one observation section with exactly one runtime-scoped
// warning_changed hint, while an unrelated Session's group survives.
func TestMaintenanceSweepRemovesWarningsForEveryCommittedDeletion(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":1}`))
		var ids []string
		for _, name := range []string{"a", "b", "c"} {
			ids = append(ids, seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, name), 100*time.Hour))
		}
		slices.Sort(ids)
		first, second, third := ids[0], ids[1], ids[2]

		wrapped := newSweepStore(store)
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
		testassert.NoError(t, err)
		defer func() {
			{
				err := r.Close(context.Background())
				testassert.Check(t, err == nil, "Close: %v", err)
			}
		}()

		keeper, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "keeper"), "solo")
		testassert.NoError(t, err)
		keeperID := keeper.Identity.SessionID
		seed := func(sessionID, kind string) {
			id := sessionID
			r.warnings.setSessionPrompt(sessionID, []protocol.Warning{{Source: "runtime:prompt", Kind: kind, Message: kind, SessionId: &id}})
		}
		seed(first, "first")
		seed(second, "second")
		seed(third, "third")
		seed(keeperID, "keeper")

		sub, err := r.Subscribe(64)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		baseRevision, _ := r.warnings.snapshot()

		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		wrapped.armDeleteTarget(third)
		stderr := captureSweepStderr(t)
		now := time.Now().Add(200 * time.Hour)
		sendTick(t, ticks, now) // the pass commits first and second, then rolls third back

		// The batch hint is the failed pass's warning-cleanup completion
		// barrier: it publishes after every returned committed identity was
		// cleaned.
		var hint *protocol.WarningChangedEvent
		for hint == nil {
			event, ok := nextEvent(t, sub)
			testassert.Require(t, ok, "the subscription closed before the sweep's warning hint")
			if eventKind(t, event) != "warning_changed" {
				continue
			}
			body, err := event.AsWarningChangedEvent()
			testassert.NoError(t, err)
			testassert.Require(t, scopeKind(t, body.Scope) == "runtime", "sweep warning hint scope = %+v, want one runtime-scoped hint for the batch", body.Scope)
			hint = &body
		}
		testassert.Require(t, hint.WarningsRevision.Revision == strconv.FormatUint(baseRevision+1, 10), "sweep warning hint revision = %q, want one advance from %d", hint.WarningsRevision.Revision, baseRevision)
		assertNoEvent(t, sub)

		for _, id := range []string{first, second} {
			{
				_, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "committed deletion of %s = err %v, want the register gone", id, err)
			}
			{
				_, warnings := r.warnings.snapshot()
				testassert.Require(t, !sessionWarningPresent(warnings, id), "committed deletion of %s kept its warning group: %+v", id, warnings)
			}
		}
		readSweepRegister(t, wrapped, third)
		{
			_, warnings := r.warnings.snapshot()
			testassert.Require(t, sessionWarningPresent(warnings, third), "the rolled-back deletion dropped its warning group")
		}
		{
			_, warnings := r.warnings.snapshot()
			testassert.Require(t, sessionWarningPresent(warnings, keeperID), "the unrelated Session's warning group was removed")
		}

		// The pass error's one retained diagnostic may land just after the
		// hint; poll it under a bounded budget.
		deadline := time.Now().Add(5 * time.Second)
		for strings.Count(stderr(), "lightcode: sweep:") == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		out := stderr()
		{
			n := strings.Count(out, "lightcode: sweep:")
			testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want one line for the whole pass", n, out)
		}
	})
}

// TestMaintenanceSweepStopsOnCandidateReadFailure proves the validation-class
// partial batch through the pass: two candidates' deletions commit, a later
// candidate's graph load fails at its storage read — outside every transaction
// — the pass stops with that error, and both committed deletions still receive
// their artifact and warning cleanup in the one batch section with one retained
// diagnostic. The unreadable candidate is left retryable, not latched.
func TestMaintenanceSweepStopsOnCandidateReadFailure(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":1}`))
		var ids []string
		for _, name := range []string{"a", "b", "c"} {
			ids = append(ids, seedStaleArchivedSession(t, store, filepath.Join(e.dataDir, name), 100*time.Hour))
		}
		slices.Sort(ids)
		first, second, third := ids[0], ids[1], ids[2]
		firstCode := plantSessionCode(t, e.dataDir, first)
		secondCode := plantSessionCode(t, e.dataDir, second)
		thirdCode := plantSessionCode(t, e.dataDir, third)

		wrapped := newSweepStore(store)
		r, ticks, err := e.openSwept(context.Background(), e.storagePlugin(wrapped))
		testassert.NoError(t, err)
		defer func() {
			{
				err := r.Close(context.Background())
				testassert.Check(t, err == nil, "Close: %v", err)
			}
		}()

		keeper, err := r.createSession(context.Background(), filepath.Join(e.dataDir, "keeper"), "solo")
		testassert.NoError(t, err)
		keeperID := keeper.Identity.SessionID
		seed := func(sessionID, kind string) {
			id := sessionID
			r.warnings.setSessionPrompt(sessionID, []protocol.Warning{{Source: "runtime:prompt", Kind: kind, Message: kind, SessionId: &id}})
		}
		seed(first, "first")
		seed(second, "second")
		seed(third, "third")
		seed(keeperID, "keeper")

		sub, err := r.Subscribe(64)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		baseRevision, _ := r.warnings.snapshot()

		writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		readFailure := errors.New("test sweep register read failure")
		wrapped.armReadFailure(third, readFailure)
		stderr := captureSweepStderr(t)
		sendTick(t, ticks, time.Now().Add(200*time.Hour)) // the pass commits first and second, then stops on third's read

		// The batch hint is the failed pass's warning-cleanup completion
		// barrier: it publishes after every returned committed identity was
		// cleaned.
		var hint *protocol.WarningChangedEvent
		for hint == nil {
			event, ok := nextEvent(t, sub)
			testassert.Require(t, ok, "the subscription closed before the sweep's warning hint")
			if eventKind(t, event) != "warning_changed" {
				continue
			}
			body, err := event.AsWarningChangedEvent()
			testassert.NoError(t, err)
			testassert.Require(t, scopeKind(t, body.Scope) == "runtime", "sweep warning hint scope = %+v, want one runtime-scoped hint for the batch", body.Scope)
			hint = &body
		}
		testassert.Require(t, hint.WarningsRevision.Revision == strconv.FormatUint(baseRevision+1, 10), "sweep warning hint revision = %q, want one advance from %d", hint.WarningsRevision.Revision, baseRevision)
		assertNoEvent(t, sub)

		mustNotExist(t, firstCode, "the first committed deletion's artifacts despite the stopping read failure")
		mustNotExist(t, secondCode, "the second committed deletion's artifacts despite the stopping read failure")
		mustExist(t, thirdCode, "the unreadable candidate's artifacts")
		for _, id := range []string{first, second} {
			{
				_, err := wrapped.ReadRegister(context.Background(), harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "committed deletion of %s = err %v, want the register gone", id, err)
			}
			{
				_, warnings := r.warnings.snapshot()
				testassert.Require(t, !sessionWarningPresent(warnings, id), "committed deletion of %s kept its warning group: %+v", id, warnings)
			}
		}
		readSweepRegister(t, wrapped, third) // the unreadable candidate stays present for the next pass
		{
			_, warnings := r.warnings.snapshot()
			testassert.Require(t, sessionWarningPresent(warnings, third), "the unreadable candidate dropped its warning group")
		}
		{
			_, warnings := r.warnings.snapshot()
			testassert.Require(t, sessionWarningPresent(warnings, keeperID), "the unrelated Session's warning group was removed")
		}
		wrapped.disarmReadFailure(third) // the transient read failure is retryable, never latched
		rec := headerThroughRuntime(t, r, third)
		testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived, "the unreadable candidate after the pass = %+v, want it left readable and archived", rec)

		// The pass error's one retained diagnostic may land just after the
		// hint; poll it under a bounded budget.
		deadline := time.Now().Add(5 * time.Second)
		for strings.Count(stderr(), "lightcode: sweep: "+readFailure.Error()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		out := stderr()
		{
			n := strings.Count(out, "lightcode: sweep: "+readFailure.Error())
			testassert.Require(t, n == 1, "stderr sweep diagnostics = %d in %q, want one line reporting the stopping read failure", n, out)
		}
	})
}

// TestSweepSkipsHeldArtifactInterval proves the pass's ownership row: a
// header-enumerated candidate whose artifact interval is held by a parked
// restore is skipped for that pass without blocking the sibling's transition
// and cleanup; the held Session keeps its artifacts and register until a later
// pass owns the interval and completes the deletion.
func TestSweepSkipsHeldArtifactInterval(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerSweepProbedDocument(e.server.URL, `{"archive_after_days":1,"delete_after_archive_days":1}`))
		ctx := context.Background()
		r, err := e.open(ctx, e.storagePlugin(store), hookPlugin(&recordHook{name: "hook.first", events: &traceLog{}}))
		testassert.NoError(t, err)
		heldWorkspace := filepath.Join(e.dataDir, "held-ws")
		testassert.NoError(t, os.MkdirAll(heldWorkspace, 0o700))
		held := projectionSession(t, r, heldWorkspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, held, "op-1", "held")
		file := filepath.Join(heldWorkspace, "held.txt")
		testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, held, op.Admission.AdmittedEntry.EntryID), file, "v1")
		{
			_, err := r.archiveSession(ctx, held)
			testassert.NoError(t, err)
		}
		heldCode := filepath.Join(e.dataDir, "code", held)

		sibling := projectionSession(t, r, filepath.Join(e.dataDir, "swept-ws"), "solo").Identity.SessionID
		{
			_, err := r.archiveSession(ctx, sibling)
			testassert.NoError(t, err)
		}
		siblingCode := plantSessionCode(t, e.dataDir, sibling)

		park, outcomes := parkRestore(t, r, held, "op-1", fifo)

		defer park.flush()

		r.runSweepPass(ctx, time.Now().Add(200*time.Hour))

		{
			_, err := os.Stat(siblingCode)
			testassert.Require(t, os.IsNotExist(err), "sibling artifacts = %v, want them swept by the pass", err)
		}
		{
			err := headerErrorThroughRuntime(ctx, r, sibling)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "sibling after the pass = %v, want the committed deletion", err)
		}
		{
			_, err := os.Stat(heldCode)
			testassert.NoError(t, err)
		}
		{
			header := headerThroughRuntime(t, r, held)
			testassert.Equal(t, header.Lifecycle, harness.LifecycleArchived)
		}

		park.release("restored")
		var parked restoreOutcome
		select {
		case parked = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parked.err == nil && len(parked.result.Restored) == 1 && parked.result.Restored[0] == file, "parked restore = %v; restored = %v, want the parked group's restore", parked.err, parked.result.Restored)

		// A later pass owns the interval and completes the deletion.
		r.runSweepPass(ctx, time.Now().Add(200*time.Hour))
		{
			_, err := os.Stat(heldCode)
			testassert.Require(t, os.IsNotExist(err), "held artifacts after the later pass = %v, want them removed", err)
		}
		{
			err := headerErrorThroughRuntime(ctx, r, held)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "held candidate after the later pass = %v, want the committed deletion", err)
		}
		testassert.NoError(t, r.Close(ctx))
	})
}
