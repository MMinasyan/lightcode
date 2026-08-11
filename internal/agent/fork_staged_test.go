package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// forkContendedChild is the sole selector for this test's child branch: the
// parent runs the same binary with this flag set, so an inherited
// environment value alone can never put a parent test process into child
// mode.
var forkContendedChild = flag.Bool("lightcode.fork-contended-child", false, "run as the fork-contention test child")

// TestForkStagedPublication verifies fork publishes the new session
// through staged rename while the source stays live and claimed: the source is
// never closed or detached, the fork is registered and selected with the forked
// turns and preserved model, publication is atomic with no staging leftover, and
// a preparation rejection leaves the source and staging namespace untouched.
func TestForkStagedPublication(t *testing.T) {
	t.Run("success_source_live_fork_selected", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		appendUserTurn(t, a, "two")
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.session.store.Root()
		srcRef := a.session.currentRef
		// Seed the source queue to prove fork preserves the source's live state.
		seedQueue(t, a, 7, "kept on source")

		if err := a.ForkSessionForSession(sourceID, 2); err != nil {
			t.Fatalf("ForkSessionForSession: %v", err)
		}
		forkID := a.SessionCurrent().ID
		if forkID == "" || forkID == sourceID {
			t.Fatalf("fork current = %q, source %q", forkID, sourceID)
		}

		// The source stays a live, registered unit whose store is still active —
		// fork never closed or detached it — and it keeps its queued input.
		a.ensureRuntime().mu.Lock()
		src := a.sessions[sourceID]
		srcActive := src != nil && src.store != nil && src.store.Active()
		a.ensureRuntime().mu.Unlock()
		if !srcActive {
			t.Fatal("source unit missing or store closed after fork")
		}
		if q, err := a.QueueSnapshotForSession(sourceID); err != nil || len(q.Items) != 1 || q.Version != 7 {
			t.Fatalf("source queue after fork = %#v err=%v, want 1 item v7", q, err)
		}

		// Fork inherits the source model and carries the forked turns.
		if got := a.session.currentRef; got != srcRef || got.Model == "" {
			t.Fatalf("fork model = %#v, want preserved %#v", got, srcRef)
		}
		forkMsgs, err := a.SessionMessagesFor(forkID)
		if err != nil {
			t.Fatalf("fork messages: %v", err)
		}
		if got := userContents(forkMsgs); !reflect.DeepEqual(got, []string{"one", "two"}) {
			t.Fatalf("fork messages = %#v, want [one two]", got)
		}

		// Atomic publication: both sessions are published and listable; the
		// staging namespace holds no leftover candidate.
		assertActiveListed(t, a, sourceID)
		assertActiveListed(t, a, forkID)
		for _, id := range []string{sourceID, forkID} {
			if _, err := os.Stat(filepath.Join(sessionsRoot, id, "meta.json")); err != nil {
				t.Fatalf("session %s not published: %v", id, err)
			}
		}
		if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")); len(entries) != 0 {
			t.Fatalf("staging left uncleaned: %v", entries)
		}
	})

	t.Run("busy_source_rejected_unchanged", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.session.store.Root()

		a.ensureRuntime().mu.Lock()
		a.session.busy = true
		a.ensureRuntime().mu.Unlock()
		forkErr := a.ForkSessionForSession(sourceID, 1)
		a.ensureRuntime().mu.Lock()
		a.session.busy = false
		a.ensureRuntime().mu.Unlock()
		if forkErr == nil {
			t.Fatal("fork of a busy source should be rejected")
		}

		// Rejected before any staging: current unchanged, no candidate left.
		if a.SessionCurrent().ID != sourceID {
			t.Fatalf("current changed to %q after rejected fork", a.SessionCurrent().ID)
		}
		if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")); len(entries) != 0 {
			t.Fatalf("staging candidate left for a rejected fork: %v", entries)
		}
	})

	// A fork whose preparation fails must propagate the error and leave the
	// source untouched, and combined code-revert must not run — the working
	// tree stays as it was. Corrupting the source meta makes ForkInto fail after
	// the mutability guards pass, exercising both error propagation and the
	// revert-after-commit ordering.
	t.Run("prepare_failure_propagates_and_leaves_tree", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		clicked := appendUserTurn(t, a, "fork point")
		path := filepath.Join(a.ProjectRoot(), "created-after-fork.txt")
		appendUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
		if err := os.WriteFile(filepath.Join(a.session.store.Dir(), "meta.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID

		if _, err := a.ApplyTurnActionForSession(sourceID, clicked, TurnActionFork, true); err == nil {
			t.Fatal("fork should fail when the source meta is unreadable")
		}
		// Fork failed before publication: no code revert ran, so the file created
		// after the fork point survives, and the source is still current.
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("working tree changed by a failed fork+revert: %v", err)
		}
		if a.SessionCurrent().ID != sourceID {
			t.Fatalf("current changed to %q after a failed fork", a.SessionCurrent().ID)
		}
	})

	// Combined code revert is best-effort and runs after the fork commits: a
	// revert failure must not fail the already-published fork, so the adapter
	// and backend cannot diverge onto different sessions.
	t.Run("revert_failure_after_commit_keeps_fork", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		a := newCatalogBackedTestAgent(t)
		clicked := appendUserTurn(t, a, "fork point")
		sub := filepath.Join(a.ProjectRoot(), "sub")
		path := filepath.Join(sub, "created-after-fork.txt")
		appendUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
		sourceID := a.SessionCurrent().ID
		// Make the file's parent unwritable so the post-fork revert cannot remove
		// the file and fails after the fork has already committed.
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(sub, 0o700)

		res, err := a.ApplyTurnActionForSession(sourceID, clicked, TurnActionFork, true)
		if err != nil {
			t.Fatalf("best-effort code revert must not fail a committed fork: %v", err)
		}
		if res.Session.ID == "" || res.Session.ID == sourceID || a.SessionCurrent().ID != res.Session.ID {
			t.Fatalf("fork not current after best-effort revert: res=%q current=%q source=%q", res.Session.ID, a.SessionCurrent().ID, sourceID)
		}
		// The fork is published, so the result reports success; the failed
		// best-effort revert still has to reach the user, as a warning on the
		// result rather than as an error that would make the adapter treat the
		// published fork as a failure.
		if res.Warning == "" {
			t.Fatal("fork with a failed code revert must report the failure in Warning")
		}
	})

	// The desktop applies the fork's state from the ordered boundary the action
	// publishes, never from the returned value, so the failed code revert's
	// warning must ride that same frame: the in-commit emit callback carries it
	// to the adapter's frame. A warning that only comes back on the result is
	// clobbered when the boundary's snapshot replaces the transcript.
	t.Run("revert_failure_warning_rides_boundary", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		a := newCatalogBackedTestAgent(t)
		clicked := appendUserTurn(t, a, "fork point")
		sub := filepath.Join(a.ProjectRoot(), "sub")
		path := filepath.Join(sub, "created-after-fork.txt")
		appendUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
		sourceID := a.SessionCurrent().ID
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(sub, 0o700)

		var boundaryWarning string
		res, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, clicked, TurnActionFork, true, func(hs HydrationState, _ []snapshot.SkippedRevert, warning string) {
			boundaryWarning = warning
		})
		if err != nil {
			t.Fatalf("best-effort code revert must not fail a committed fork: %v", err)
		}
		if res.Warning == "" {
			t.Fatal("fork with a failed code revert must report the failure in Warning")
		}
		if boundaryWarning != res.Warning {
			t.Fatalf("boundary warning = %q, want result warning %q", boundaryWarning, res.Warning)
		}
	})

	// The fork inherits the source's live model, not its persisted metadata, so
	// an unpersisted model switch is not lost across a fork — in memory and
	// durably, so the fork reopens on the same model.
	t.Run("inherits_source_live_model", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		// Diverge the live selection from persisted metadata (no metadata write).
		alt := coremodel.ModelRef{Provider: "test", Model: "alt-model"}
		a.ensureRuntime().mu.Lock()
		a.session.currentRef = alt
		a.ensureRuntime().mu.Unlock()

		if err := a.ForkSessionForSession(a.SessionCurrent().ID, 1); err != nil {
			t.Fatalf("ForkSessionForSession: %v", err)
		}
		if got := a.session.currentRef; got != alt {
			t.Fatalf("fork model = %#v, want inherited live %#v", got, alt)
		}
		meta, err := a.session.store.Meta()
		if err != nil {
			t.Fatalf("fork meta: %v", err)
		}
		if meta.Provider != alt.Provider || meta.Model != alt.Model {
			t.Fatalf("fork meta model = %s/%s, want %s/%s", meta.Provider, meta.Model, alt.Provider, alt.Model)
		}
	})

	// If the source's live model cannot be reconstructed, the fork aborts before
	// publication rather than silently committing on the stale persisted model.
	t.Run("unresolvable_live_model_aborts", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		a.ensureRuntime().mu.Lock()
		a.session.currentRef = coremodel.ModelRef{Provider: "ghost", Model: "missing"}
		a.ensureRuntime().mu.Unlock()

		if err := a.ForkSessionForSession(sourceID, 1); err == nil {
			t.Fatal("fork should abort when the live model cannot be reconstructed")
		}
		if a.SessionCurrent().ID != sourceID {
			t.Fatalf("current changed to %q after an aborted fork", a.SessionCurrent().ID)
		}
	})
}

func assertActiveListed(t *testing.T, a *Agent, id string) {
	t.Helper()
	list, err := a.SessionList("active")
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	for _, s := range list {
		if s.ID == id {
			return
		}
	}
	t.Fatalf("session %q not in active list: %#v", id, list)
}

// TestForkCandidateContendedAborts proves a fork whose candidate claim is
// contended — another live process already drives the minted candidate id —
// aborts cleanly: the fork returns ErrSessionContended, the source stays
// live, current and claimed, the candidate's staging directory is removed,
// and the boundary callback never runs. A contended fork candidate is
// deliberately terminal: unlike the mint, it does not redraw. This test does
// not change mint behavior and asserts no mint redraw — the foreign claim is
// taken only after the mint has already drawn and published the id.
//
// The complete scenario runs in a self-exec child of this test, selected only
// by the dedicated child flag, so a mutation that hangs the fork is killed
// and reaped by the parent's five-second CommandContext deadline instead of
// hanging the test process. The parent asserts a successful child exit; a
// deadline or nonzero child output fails with diagnostics. The child's TMPDIR
// is a parent-created temporary directory, so the parent's t.TempDir cleanup
// removes the child's test filesystem state even when the child is killed at
// the deadline.
func TestForkCandidateContendedAborts(t *testing.T) {
	if *forkContendedChild {
		// Child mode: run the complete scenario synchronously and return; the
		// parent branch below is not re-entered.
		forkCandidateContendedScenario(t)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForkCandidateContendedAborts$", "-lightcode.fork-contended-child")
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fork contender child failed: %v\n%s", err, out)
	}
}

// forkCandidateContendedScenario is the whole fork-contention scenario, run
// synchronously in the child process. The fork mints its candidate id inside
// ForkInto with claim=false, so the id is unknown to the caller and
// unclaimed; the claim is taken later, by candidateStore.LoadSession inside
// forkCommitStagedLocked. durableReadHook fires one statement before that
// load, under rt.mu, with the staged tree already on disk. It fires more
// than once on this path, so the hook is one-shot and conditioned on the
// staged candidate being discoverable: nothing happens before the candidate
// exists, and after the load has taken its claim there is nothing left to
// act on. The foreign claim stays held until the fork call has returned,
// then is released; a release failure is an explicit test failure.
func forkCandidateContendedScenario(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	turn := appendUserTurn(t, a, "fork point")
	sourceID := a.SessionCurrent().ID
	if sourceID == "" {
		t.Fatal("no source session id")
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	projectID := a.session.projectID
	sessionsRoot := a.session.store.Root()
	rt.mu.Unlock()
	if projectID == "" {
		t.Fatal("source session has no project id")
	}
	projectsRoot := a.projects.Root()
	stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")

	var (
		once         sync.Once
		foreignClaim *atomicfs.Lock
		candidateID  string
	)
	a.durableReadHook = func() {
		// The hook fires before the candidate exists (the forkCopyTree fire,
		// before ForkInto has run): return immediately unless a staged
		// candidate is discoverable, and act only the first time one is.
		entries, err := os.ReadDir(stagingParent)
		if err != nil || len(entries) == 0 {
			return
		}
		once.Do(func() {
			for _, nonce := range entries {
				candEntries, err := os.ReadDir(filepath.Join(stagingParent, nonce.Name()))
				if err != nil || len(candEntries) == 0 {
					continue
				}
				candidateID = candEntries[0].Name()
				lock, ok, err := snapshot.AcquireSessionClaim(projectsRoot, projectID, candidateID)
				if err != nil || !ok {
					// The fork minted the candidate with claim=false, so the
					// claim is free here; a refusal is a test bug.
					return
				}
				foreignClaim = lock
				return
			}
		})
	}
	defer func() { a.durableReadHook = nil }()

	var emitted bool
	_, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, turn, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
		emitted = true
	})
	// The foreign claim stays held until the fork call has returned; only
	// then is it released. Releasing inside the hook would free the claim
	// before candidateStore.LoadSession runs, the load would succeed, and the
	// fork would complete against correct code.
	if foreignClaim != nil {
		if relErr := foreignClaim.Release(); relErr != nil {
			t.Fatalf("release foreign claim: %v", relErr)
		}
	} else {
		t.Fatalf("the candidate claim was never taken: the hook did not fire on the staged candidate %q", candidateID)
	}

	if !errors.Is(err, snapshot.ErrSessionContended) {
		t.Fatalf("fork = %v, want ErrSessionContended", err)
	}
	// The source stays live, current and claimed.
	rt.mu.Lock()
	src := a.sessions[sourceID]
	srcActive := src != nil && src.store != nil && src.store.Active()
	rt.mu.Unlock()
	if !srcActive {
		t.Fatal("source unit missing or store closed after the aborted fork")
	}
	if a.SessionCurrent().ID != sourceID {
		t.Fatalf("current = %q after the aborted fork, want source %q", a.SessionCurrent().ID, sourceID)
	}
	if lock, ok, aerr := snapshot.AcquireSessionClaim(projectsRoot, projectID, sourceID); aerr != nil || ok {
		if lock != nil {
			_ = lock.Release()
		}
		t.Fatalf("source claim released by the aborted fork: ok=%v err=%v", ok, aerr)
	}
	// The candidate's staging directory is gone: the namespace must be
	// readable before asserting it is empty.
	entries, err := os.ReadDir(stagingParent)
	if err != nil {
		t.Fatalf("read staging namespace: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging left uncleaned after the aborted fork: %v", entries)
	}
	if emitted {
		t.Fatal("boundary emitted for an aborted fork")
	}
}

// forkBarrierAdmission is the shared body of the copy-barrier and
// post-rename-barrier reservation tests: while the fork is parked, each actor
// is started with every other predicate satisfied and must be refused by the
// transitioning gate alone. A direct submit refuses; one queued item plus a
// real drainer attempt leaves the item retained with no turn; after the queue
// is cleared, one Wake pending signal plus a real scheduler attempt leaves the
// signal retained with no turn. The park callback blocks the fork and returns
// the release.
func forkBarrierAdmission(t *testing.T, a *Agent, reqs *atomic.Int32, cap *eventCapture, sourceID string, park func(t *testing.T) func()) {
	t.Helper()
	release := park(t)
	defer release()

	// Submit refusal: the source is idle and would otherwise accept the turn.
	if _, err := a.SubmitToSession(context.Background(), sourceID, "during fork"); err == nil {
		t.Fatal("submit admitted while the fork holds the reservation")
	}

	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[sourceID]
	rt.mu.Unlock()
	if unit == nil {
		t.Fatal("source unit not live")
	}

	// Drain refusal: one queued item plus a real drainer attempt; the pass
	// settles via the pass hook, the item is retained, and no turn starts.
	rt.mu.Lock()
	unit.queue = []QueuedItem{{ID: "q-1", Content: "queued during fork"}}
	unit.queueSeq = 1
	unit.queueVersion = 1
	rt.mu.Unlock()
	passDone := make(chan struct{})
	var passOnce sync.Once
	// The one-shot hook is left installed (a no-op after the first pass):
	// nilling it here would race the live drainer's later hook reads, and the
	// agent is shut down by startEventOrderAgent's cleanup.
	rt.queueDrainPassHook = func() { passOnce.Do(func() { close(passDone) }) }
	rt.nudgeQueueDrainer()
	select {
	case <-passDone:
	case <-time.After(10 * time.Second):
		t.Fatal("drainer pass never settled")
	}
	rt.mu.Lock()
	retained := len(unit.queue) == 1 && unit.queue[0].ID == "q-1"
	rt.mu.Unlock()
	if !retained {
		t.Fatalf("queued source item was not retained while the fork held the reservation")
	}
	if got := reqs.Load(); got != 0 {
		t.Fatalf("a model turn launched while the fork held the reservation (%d requests)", got)
	}

	// Signal refusal: clear the queue, one Wake pending signal plus a real
	// scheduler attempt; the attempt settles when the scheduler consumes the
	// wake token (a fresh send succeeding proves it is back waiting), the
	// signal stays pending, and no turn starts.
	rt.mu.Lock()
	unit.queue = nil
	unit.queueVersion++
	rt.mu.Unlock()
	a.lp.AddPendingSignal(loop.PendingSignal{Payload: "wake", Persist: true, Wake: true})
	// A real scheduler attempt driven directly: the scheduler's own entry
	// point runs synchronously and returns after the full attempt, so the
	// retained-signal/no-turn assertions observe its outcome deterministically
	// (the background scheduler's async attempt has no completion observable
	// without a runtime seam, which the contract forbids).
	rt.tryStartSignalTurn(context.Background())
	rt.mu.Lock()
	signalBusy := unit.busy
	rt.mu.Unlock()
	if signalBusy {
		t.Fatal("the signal scheduler claimed the source while the fork held the reservation")
	}
	if !a.lp.HasPendingWakeSignal() {
		t.Fatal("pending wake signal lost while the fork held the reservation")
	}
	if got := reqs.Load(); got != 0 {
		t.Fatalf("a model turn launched while the fork held the reservation (%d requests)", got)
	}
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventTurnStart {
			t.Fatalf("a turn_start was delivered while the fork held the reservation: %#v", ev)
		}
	}
}

// TestForkReservationBlocksAdmissionAtCopy proves the fork holds the source's
// transitioning reservation across its staged copy: with the fork parked at
// the copy barrier, a submit, the queue drainer, and the signal scheduler all
// refuse the source with every other predicate satisfied.
func TestForkReservationBlocksAdmissionAtCopy(t *testing.T) {
	var reqs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		writeTextResponse(w, "ok")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	appendUserTurn(t, a, "persisted")
	sourceID := a.SessionCurrent().ID

	// Park the fork at its staged copy: the first durable read of the fork is
	// the tree copy, so the hook fires while the reservation must already be
	// held. The hook is installed before the fork goroutine starts so the
	// fork's first fire is ordered after the assignment.
	copyStarted := make(chan struct{})
	releaseCopy := make(chan struct{})
	var once sync.Once
	a.durableReadHook = func() {
		once.Do(func() { close(copyStarted) })
		<-releaseCopy
	}
	// The hook is left installed (inert once releaseCopy is closed): nilling
	// it here would race the later post-fork drain turn's prompt fire, and the
	// agent is shut down by startEventOrderAgent's cleanup.
	defer func() {
		select {
		case <-releaseCopy:
		default:
			close(releaseCopy)
		}
	}()

	forkDone := make(chan error, 1)
	go func() { forkDone <- a.ForkSessionForSession(sourceID, 1) }()
	forkBarrierAdmission(t, a, &reqs, cap, sourceID, func(t *testing.T) func() {
		t.Helper()
		select {
		case <-copyStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("fork never began its staged copy")
		}
		return func() { close(releaseCopy) }
	})
	if err := <-forkDone; err != nil {
		t.Fatalf("fork: %v", err)
	}
	waitUntilFullyDrained(t, a)
}

// TestForkReservationBlocksAdmissionPostRename proves the reservation is still
// held after the candidate rename and path relocation: with the fork parked at
// the post-rename cleanup seam, a submit, the queue drainer, and the signal
// scheduler all refuse the source, exactly as at the copy barrier.
func TestForkReservationBlocksAdmissionPostRename(t *testing.T) {
	var reqs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		writeTextResponse(w, "ok")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	appendUserTurn(t, a, "persisted")
	sourceID := a.SessionCurrent().ID

	seamFired := make(chan struct{})
	releaseSeam := make(chan struct{})
	var seamOnce sync.Once
	origRemove := removeStagingTree
	removeStagingTree = func(string) error {
		seamOnce.Do(func() { close(seamFired) })
		<-releaseSeam
		return nil
	}
	defer func() { removeStagingTree = origRemove }()
	defer func() {
		select {
		case <-releaseSeam:
		default:
			close(releaseSeam)
		}
	}()

	forkDone := make(chan error, 1)
	go func() { forkDone <- a.ForkSessionForSession(sourceID, 1) }()
	select {
	case <-seamFired:
	case <-time.After(10 * time.Second):
		t.Fatal("post-rename cleanup seam never fired")
	}
	forkBarrierAdmission(t, a, &reqs, cap, sourceID, func(t *testing.T) func() {
		t.Helper()
		// The fork is already parked at the seam; the release unparks it.
		return func() { close(releaseSeam) }
	})
	if err := <-forkDone; err != nil {
		t.Fatalf("fork: %v", err)
	}
	waitUntilFullyDrained(t, a)
}

// TestForkPostRenameCleanupFailureReportsStderr proves the fork's post-rename
// empty-parent cleanup is exactly that: the cleanup seam runs only after the
// candidate rename and path relocation, its failure prints one stderr line and
// cannot fail the committed fork, and the reservation is still held at that
// point — submit, drain, and signal refuse, and the combined code revert has
// not yet restored the source tree. The same barrier also proves the boundary
// is prebuilt: with the published candidate's history unreadable at the
// barrier, the fork still publishes the prebuilt replacement.
func TestForkPostRenameCleanupFailureReportsStderr(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "ok")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	appendUserTurn(t, a, "first")
	clicked := appendUserTurn(t, a, "fork point")
	sub := filepath.Join(a.ProjectRoot(), "sub")
	path := filepath.Join(sub, "created-after-fork.txt")
	appendUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
	sourceID := a.SessionCurrent().ID
	sessionsRoot := a.session.store.Root()

	// Swap the seam: the post-rename call blocks, reports an injected failure,
	// and lets the fork continue. The seam fires exactly once on the success
	// path — the post-rename empty-parent removal.
	seamFired := make(chan struct{})
	releaseSeam := make(chan struct{})
	var seamOnce sync.Once
	origRemove := removeStagingTree
	removeStagingTree = func(string) error {
		seamOnce.Do(func() { close(seamFired) })
		<-releaseSeam
		return errors.New("injected staging removal failure")
	}
	defer func() { removeStagingTree = origRemove }()
	defer func() {
		select {
		case <-releaseSeam:
		default:
			close(releaseSeam)
		}
	}()

	stderr := captureStderr(t)
	var boundary HydrationState
	var boundaryWarning string
	forkDone := make(chan error, 1)
	go func() {
		_, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, clicked, TurnActionFork, true, func(hs HydrationState, _ []snapshot.SkippedRevert, warning string) {
			boundary = hs
			boundaryWarning = warning
		})
		forkDone <- err
	}()
	select {
	case <-seamFired:
	case <-time.After(10 * time.Second):
		t.Fatal("post-rename cleanup seam never fired")
	}

	// While the fork is parked after the rename, the source is still reserved:
	// submit refuses, no drain or signal launches, and the combined code
	// revert has not yet restored the tree.
	if _, err := a.SubmitToSession(ctx, sourceID, "during post-rename"); err == nil {
		t.Fatal("submit admitted while the fork was parked after the rename")
	}
	time.Sleep(50 * time.Millisecond)
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventTurnStart {
			t.Fatalf("a turn_start was delivered while the fork was parked after the rename: %#v", ev)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source tree restored before the post-rename barrier: %v", err)
	}
	// The published candidate's history is atomically relocated away at the
	// barrier: the cached candidate-store paths point at the missing
	// originals, so any post-publication durable read fails with ENOENT for
	// every UID (no permissions involved, root and non-root alike). The
	// prebuilt result must still publish.
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		t.Fatalf("read sessions root: %v", err)
	}
	var candidateID string
	for _, e := range entries {
		if e.IsDir() && e.Name() != sourceID {
			candidateID = e.Name()
			break
		}
	}
	if candidateID == "" {
		t.Fatal("published fork candidate not found")
	}
	candDir := filepath.Join(sessionsRoot, candidateID)
	restoreCandidateHistory := func() {
		for _, d := range []string{"turns", "snapshots"} {
			_ = os.Rename(filepath.Join(candDir, d+".orphan"), filepath.Join(candDir, d))
		}
	}
	for _, d := range []string{"turns", "snapshots"} {
		if err := os.Rename(filepath.Join(candDir, d), filepath.Join(candDir, d+".orphan")); err != nil {
			t.Fatal(err)
		}
	}
	defer restoreCandidateHistory()
	t.Cleanup(restoreCandidateHistory)

	close(releaseSeam)
	if err := <-forkDone; err != nil {
		t.Fatalf("fork: %v", err)
	}
	// The committed fork is a success despite the cleanup failure; the
	// failure reached stderr exactly once, naming the staging root.
	out := stderr()
	if !strings.Contains(out, "injected staging removal failure") {
		t.Fatalf("fork stderr = %q, want the injected cleanup failure", out)
	}
	// Restore the candidate history immediately after the fork returns, before
	// any later candidate use.
	restoreCandidateHistory()
	// The boundary carries the prebuilt replacement (rendered before the
	// rename), not a re-read of the missing candidate history.
	if c := userContents(boundary.Messages); !equalStrings(c, []string{"first", "fork point"}) {
		t.Fatalf("fork boundary messages = %q, want the prebuilt [first, fork point]", c)
	}
	if boundaryWarning != "" {
		t.Fatalf("boundary warning = %q, want empty (the code revert succeeded)", boundaryWarning)
	}
	// The code revert ran after the barrier and restored the tree.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file still exists after fork+code revert: %v", err)
	}
	// The source is live again and accepts work after the reservation release.
	if _, err := a.SubmitToSession(ctx, sourceID, "after fork"); err != nil {
		t.Fatalf("submit after fork release: %v", err)
	}
}

// TestForkCodeRevertKeepsForkPointFiles pins the fork+code revert target: the
// fork input N is inclusive in the candidate (ForkInto copies turns 1..N), so
// the source restore keeps clicked-turn changes and removes only later-turn
// changes. A file first changed in the clicked fork turn keeps its N state on
// disk, a file first changed in turn N+1 is removed, and the candidate still
// includes turn N. This is distinct from the direct/shared code-revert APIs,
// whose input is the first restored turn.
func TestForkCodeRevertKeepsForkPointFiles(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "first")
	// Turn N (clicked fork point): file A first changed here.
	forkA := filepath.Join(a.ProjectRoot(), "fork-a.txt")
	clicked := appendUserTurnWithSnapshot(t, a, "fork point", forkA, "a\n")
	// Turn N+1: file B first changed here.
	forkB := filepath.Join(a.ProjectRoot(), "fork-b.txt")
	appendUserTurnWithSnapshot(t, a, "after fork", forkB, "b\n")
	sourceID := a.SessionCurrent().ID

	res, err := a.ApplyTurnActionForSession(sourceID, clicked, TurnActionFork, true)
	if err != nil {
		t.Fatalf("fork+code: %v", err)
	}
	if res.Warning != "" {
		t.Fatalf("fork+code warning = %q, want empty (the revert succeeded)", res.Warning)
	}
	// File A keeps its turn-N state: only source changes after N are reverted.
	if got, rerr := os.ReadFile(forkA); rerr != nil || string(got) != "a\n" {
		t.Fatalf("file changed in the clicked fork turn = %q, %v; want its N state", got, rerr)
	}
	// File B (first changed after the fork point) is removed.
	if _, err := os.Stat(forkB); !os.IsNotExist(err) {
		t.Fatalf("file first changed after the fork point still exists: %v", err)
	}
	// The candidate still includes turn N.
	candMsgs, err := a.SessionMessagesFor(res.Session.ID)
	if err != nil {
		t.Fatalf("candidate messages: %v", err)
	}
	if c := userContents(candMsgs); !equalStrings(c, []string{"first", "fork point"}) {
		t.Fatalf("candidate messages = %q, want [first, fork point]", c)
	}
}

// TestForkUnresolvableAgentTypeFailsBeforeWork proves the fork resolves the
// candidate's agent type in its first rt.mu hold and fails immediately when
// the source's agent type can no longer be resolved — before any staging,
// copy, or publication — while the source stays current, live, and claimed
// with its readonly registry unchanged.
func TestForkUnresolvableAgentTypeFailsBeforeWork(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeAgentsTestConfig(t, a.configPath, `{"primary": {"model": "test/test-model"}, "custom": {"model": "test/test-model", "readonly": true, "prompt": "custom prompt"}}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload with custom type: %v", err)
	}
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(proj.ID, "custom"); err != nil {
		t.Fatalf("NewSession custom: %v", err)
	}
	appendUserTurn(t, a, "persisted")
	sourceID := a.SessionCurrent().ID
	sessionsRoot := a.session.store.Root()
	if _, ok := a.session.registry.Get("write_file"); ok {
		t.Fatal("setup: readonly source registry exposes write_file")
	}
	// A normal agent-config reload removes the custom type.
	writeAgentsTestConfig(t, a.configPath, `{"primary": {"model": "test/test-model"}}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload without custom type: %v", err)
	}

	var emitted bool
	_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
		emitted = true
	})
	if err == nil {
		t.Fatal("fork succeeded with an unresolvable agent type")
	}
	if !strings.Contains(err.Error(), "custom") {
		t.Fatalf("fork error = %q, want the agent-type resolution error naming the custom type", err.Error())
	}
	if emitted {
		t.Fatal("boundary emitted for a fork that failed before publication")
	}
	// No staging and no published candidate.
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")); len(entries) != 0 {
		t.Fatalf("staging left by the failed fork: %v", entries)
	}
	list, err := a.SessionList("active")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != sourceID {
		t.Fatalf("active sessions after failed fork = %#v, want only the source", list)
	}
	// The source stays current, live, claimed, and its readonly registry is
	// unchanged.
	if a.SessionCurrent().ID != sourceID {
		t.Fatalf("SessionCurrent = %q, want the source", a.SessionCurrent().ID)
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[sourceID]
	rt.mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		t.Fatal("failed fork detached the source")
	}
	claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, sourceID)
	if err != nil {
		t.Fatalf("claim check: %v", err)
	}
	if ok {
		_ = claim.Release()
		t.Fatal("source claim released by the failed fork")
	}
	if _, ok := unit.registry.Get("write_file"); ok {
		t.Fatal("failed fork changed the source's readonly registry")
	}
}

// TestForkResolvableReadonlySourceRetainsRestrictions is the positive sibling
// of TestForkUnresolvableAgentTypeFailsBeforeWork: a resolvable custom
// readonly source forks to a candidate that retains the readonly/write-tool
// restrictions.
func TestForkResolvableReadonlySourceRetainsRestrictions(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeAgentsTestConfig(t, a.configPath, `{"primary": {"model": "test/test-model"}, "custom": {"model": "test/test-model", "readonly": true, "prompt": "custom prompt"}}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(proj.ID, "custom"); err != nil {
		t.Fatalf("NewSession custom: %v", err)
	}
	appendUserTurn(t, a, "persisted")
	sourceID := a.SessionCurrent().ID

	if err := a.ForkSessionForSession(sourceID, 1); err != nil {
		t.Fatalf("fork: %v", err)
	}
	cand := a.session // the fork is selected when the source was current
	if cand.activeAgentType != "custom" {
		t.Fatalf("candidate agent type = %q, want custom", cand.activeAgentType)
	}
	if _, ok := cand.registry.Get("write_file"); ok {
		t.Fatal("candidate registry exposes write_file despite the readonly source type")
	}
	if _, ok := cand.registry.Get("read_file"); !ok {
		t.Fatal("candidate registry missing read_file")
	}
}

// TestForkCandidateAdaptationInstalledBeforePromptAssembly proves the fork's
// model adaptation is installed before the single prompt assembly: the
// candidate's activeAdapt, loop adaptation, installedPrompt, and loop system
// prompt agree and include the adaptation block, with no second prompt
// assembly and no rt.mu reacquisition.
func TestForkCandidateAdaptationInstalledBeforePromptAssembly(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.resolveAdapt = func(string) *adaptation.Adaptation {
		return &adaptation.Adaptation{Name: "test-adapt", Blocks: []string{"TEST-ADAPTATION-BLOCK"}}
	}
	appendUserTurn(t, a, "persisted")
	sourceID := a.SessionCurrent().ID

	if err := a.ForkSessionForSession(sourceID, 1); err != nil {
		t.Fatalf("fork: %v", err)
	}
	cand := a.session
	if cand.activeAdapt == nil || cand.activeAdapt.Name != "test-adapt" {
		t.Fatalf("candidate activeAdapt = %+v, want the fork adaptation", cand.activeAdapt)
	}
	if got := cand.lp.ActiveAdaptation(); got != cand.activeAdapt {
		t.Fatalf("loop adaptation = %+v, want the candidate activeAdapt", got)
	}
	loopPrompt := cand.lp.Messages()[0].TextContent()
	if !strings.Contains(cand.installedPrompt, "TEST-ADAPTATION-BLOCK") {
		t.Fatalf("installedPrompt lacks the adaptation block: %q", cand.installedPrompt)
	}
	if loopPrompt != cand.installedPrompt {
		t.Fatalf("loop system prompt disagrees with installedPrompt")
	}
	if !strings.Contains(loopPrompt, "TEST-ADAPTATION-BLOCK") {
		t.Fatalf("loop system prompt lacks the adaptation block: %q", loopPrompt)
	}
}

// TestForkPostRenameSourceTurnSurvives proves the source reservation covers
// the post-publication window: with fork+code parked after the candidate
// publication, a queued source turn is retained until the source restore and
// the boundary complete and endLiveTransition re-arms the drainer; the turn
// then runs for real, and its final file content, complete durable
// turn, and snapshot/undo entry all survive.
func TestForkPostRenameSourceTurnSurvives(t *testing.T) {
	a := newEventOrderAgent(t, "http://127.0.0.1:9/v1")
	path := filepath.Join(a.ProjectRoot(), "source-turn.txt")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeTaskToolCallResponse(w, "call-write", "write_file", fmt.Sprintf(`{"path":%q,"content":"written during post-fork turn"}`, path))
		default:
			writeTextResponse(w, "done")
		}
	}))
	defer server.Close()
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"

	cap := &eventCapture{}
	_ = startEventOrderAgent(t, a, cap)
	a.cfg.Permissions.Allow = []string{"read_file(/**)", "write_file(/**)", "edit_file(/**)"}
	appendUserTurn(t, a, "first")
	clicked := appendUserTurn(t, a, "fork point")
	sourceID := a.SessionCurrent().ID

	// Seed the source queue before the fork: the reservation retains the item
	// until the fork's release re-arms the drainer.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[sourceID]
	rt.mu.Unlock()
	rt.mu.Lock()
	unit.queue = []QueuedItem{{ID: "q-1", Content: "write the source file"}}
	unit.queueSeq = 1
	unit.queueVersion = 1
	rt.mu.Unlock()

	// Park fork+code at the post-rename seam.
	seamFired := make(chan struct{})
	releaseSeam := make(chan struct{})
	var seamOnce sync.Once
	origRemove := removeStagingTree
	removeStagingTree = func(string) error {
		seamOnce.Do(func() { close(seamFired) })
		<-releaseSeam
		return nil
	}
	defer func() { removeStagingTree = origRemove }()
	defer func() {
		select {
		case <-releaseSeam:
		default:
			close(releaseSeam)
		}
	}()

	forkDone := make(chan error, 1)
	go func() {
		_, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, clicked, TurnActionFork, true, func(HydrationState, []snapshot.SkippedRevert, string) {})
		forkDone <- err
	}()
	select {
	case <-seamFired:
	case <-time.After(10 * time.Second):
		t.Fatal("post-rename cleanup seam never fired")
	}

	// A real drainer attempt during the park: with the reservation held the
	// pass finds the source transitioning and retains the item; the turn runs
	// only after the release re-arms the drainer.
	passDone := make(chan struct{})
	var passOnce sync.Once
	// The one-shot hook is left installed (a no-op after the first pass):
	// nilling it here would race the live drainer's later hook reads, and the
	// agent is shut down by startEventOrderAgent's cleanup.
	rt.queueDrainPassHook = func() { passOnce.Do(func() { close(passDone) }) }
	rt.nudgeQueueDrainer()
	select {
	case <-passDone:
	case <-time.After(10 * time.Second):
		t.Fatal("drainer pass never settled")
	}
	// If the pass claimed (only possible under the source-loss mutation), wait
	// for that turn to finish so the release's revert acts on a completed
	// turn; the retained-item assertion below then fails deterministically.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		busy := unit.busy
		rt.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt.mu.Lock()
	retained := len(unit.queue) == 1 && unit.queue[0].ID == "q-1"
	rt.mu.Unlock()
	if !retained {
		t.Fatal("queued source turn ran while the fork was parked after publication")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("a model turn launched while the fork held the reservation (%d requests)", got)
	}

	close(releaseSeam)
	if err := <-forkDone; err != nil {
		t.Fatalf("fork+code: %v", err)
	}
	// The release re-arms the retained queue: the queued turn runs for real —
	// write_file then completion.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && calls.Load() != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("source turn never ran after the fork release (requests = %d, want 2)", got)
	}
	waitSessionDrained(t, a, sourceID)

	// The turn's work survives: the file content, the complete durable turn,
	// and the snapshot/undo entry.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "written during post-fork turn" {
		t.Fatalf("source-turn file = %q, %v; want the written content", data, err)
	}
	msgs, err := a.SessionMessagesFor(sourceID)
	if err != nil {
		t.Fatalf("source messages: %v", err)
	}
	if c := userContents(msgs); !contains(c, "write the source file") {
		t.Fatalf("source durable turns = %q, want the queued turn completed durably", c)
	}
	rt.mu.Lock()
	src := a.sessions[sourceID]
	rt.mu.Unlock()
	if src == nil || src.store == nil || !src.store.Active() {
		t.Fatal("source unit missing after the fork")
	}
	if c := loopUserContents(src.lp.Messages()); !contains(c, "write the source file") {
		t.Fatalf("source loop history = %q, want the queued turn completed in the loop", c)
	}
	snapTurns, err := os.ReadDir(filepath.Join(src.store.Dir(), "snapshots"))
	if err != nil {
		t.Fatalf("read source snapshots: %v", err)
	}
	newest := 0
	for _, e := range snapTurns {
		if n, perr := strconv.Atoi(e.Name()); perr == nil && n > newest {
			newest = n
		}
	}
	if newest <= clicked {
		t.Fatalf("no snapshot turn above the fork point %d: %v", clicked, snapTurns)
	}
	entries, err := os.ReadDir(filepath.Join(src.store.Dir(), "snapshots", strconv.Itoa(newest)))
	if err != nil || len(entries) == 0 {
		t.Fatalf("snapshot/undo entry missing for the post-fork turn: %v, %v", entries, err)
	}
}

// installForkCandidateHook installs a durableReadHook acting at the nth fire
// where a staged candidate is discoverable (fork candidate-aware ordinals:
// LoadSession=1, prompt=2, history=3, tokens=4, SetModel=5, Meta=6). act
// receives the candidate directory and id. The hook is left installed — inert
// once the ordinal passes and the operation returns — to avoid teardown races.
func installForkCandidateHook(a *Agent, sessionsRoot string, target int, act func(candDir, candidateID string)) {
	seen := 0
	a.durableReadHook = func() {
		parent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) == 0 {
			return
		}
		nonce := entries[0].Name()
		candEntries, err := os.ReadDir(filepath.Join(parent, nonce))
		if err != nil || len(candEntries) == 0 {
			return
		}
		seen++
		if seen == target {
			act(filepath.Join(parent, nonce, candEntries[0].Name()), candEntries[0].Name())
		}
	}
}

// assertForkStagedFailureInvariants asserts the precommit disposition shared
// by every staged fork failure: the source stays current, live, claimed, and
// released (transitioning cleared); no candidate is registered; no boundary
// was emitted; and the staging namespace is clean.
func assertForkStagedFailureInvariants(t *testing.T, a *Agent, sourceID, projectID string, emitted *bool, stagingParent string) {
	t.Helper()
	if a.SessionCurrent().ID != sourceID {
		t.Fatalf("SessionCurrent = %q, want the source %q", a.SessionCurrent().ID, sourceID)
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[sourceID]
	transitioning := false
	if unit != nil {
		transitioning = unit.transitioning
	}
	registered := len(a.sessions)
	rt.mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		t.Fatal("failed fork detached the source")
	}
	if transitioning {
		t.Fatal("transitioning reservation not released after the failed fork")
	}
	if registered != 1 {
		t.Fatalf("live sessions after failed fork = %d, want only the source", registered)
	}
	if emitted != nil && *emitted {
		t.Fatal("boundary emitted for a failed fork")
	}
	claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), projectID, sourceID)
	if err != nil {
		t.Fatalf("claim check: %v", err)
	}
	if ok {
		_ = claim.Release()
		t.Fatal("source claim released by the failed fork")
	}
	if entries, _ := os.ReadDir(stagingParent); len(entries) != 0 {
		t.Fatalf("staging left uncleaned after the failed fork: %v", entries)
	}
}

// TestForkStagedFailureCoverage proves each staged fork failure returns its
// exact error and the shared precommit disposition — candidate claim
// released, staging cleaned, source current/live/claimed with the reservation
// released, no candidate registration, no boundary — with the failure injected
// deterministically at the exact phase through the existing durableReadHook
// (candidate-aware ordinals: LoadSession=1, prompt=2, history=3, tokens=4,
// SetModel=5, Meta=6).
func TestForkStagedFailureCoverage(t *testing.T) {
	runFork := func(t *testing.T, a *Agent, sourceID string, emitted *bool) error {
		t.Helper()
		_, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			*emitted = true
		})
		return err
	}

	t.Run("post_claim_load_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var recordedID string
		var emitted bool
		installForkCandidateHook(a, sessionsRoot, 1, func(candDir, candidateID string) {
			recordedID = candidateID
			if err := os.WriteFile(filepath.Join(candDir, "meta.json"), []byte("{not json"), 0o600); err != nil {
				t.Error(err)
			}
		})

		err = runFork(t, a, sourceID, &emitted)
		if err == nil {
			t.Fatal("fork should fail when the staged meta is corrupt at the candidate load")
		}
		if errors.Is(err, snapshot.ErrSessionContended) {
			t.Fatalf("fork error = %v, want the load parse error, not contention", err)
		}
		if !strings.Contains(err.Error(), "snapshot: load") {
			t.Fatalf("fork error = %q, want the exact non-contention load error", err.Error())
		}
		// LoadSession's failure released the candidate claim it took.
		claim, ok, cerr := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, recordedID)
		if cerr != nil {
			t.Fatalf("claim check: %v", cerr)
		}
		if !ok {
			t.Fatal("candidate claim still held after the failed candidate load")
		}
		_ = claim.Release()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("history_load_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var emitted bool
		installForkCandidateHook(a, sessionsRoot, 3, func(candDir, candidateID string) {
			// Replace the first staged turn's messages file with a directory:
			// the history read fails with EISDIR for every UID.
			turnDir := filepath.Join(candDir, "turns")
			entries, err := os.ReadDir(turnDir)
			if err != nil || len(entries) == 0 {
				return
			}
			first := filepath.Join(turnDir, entries[0].Name())
			msgFile := filepath.Join(first, "messages.jsonl")
			if err := os.Rename(msgFile, msgFile+".orphan"); err != nil {
				return
			}
			if err := os.Mkdir(msgFile, 0o700); err != nil {
				return
			}
		})

		err = runFork(t, a, sourceID, &emitted)
		if err == nil {
			t.Fatal("fork should fail when the staged history is unreadable")
		}
		if !strings.Contains(err.Error(), "read turn") {
			t.Fatalf("fork error = %q, want the exact staged-history read error", err.Error())
		}
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("staged_setmodel_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var recordedID string
		var emitted bool
		installForkCandidateHook(a, sessionsRoot, 5, func(candDir, candidateID string) {
			recordedID = candidateID
			if err := os.WriteFile(filepath.Join(candDir, "meta.json"), []byte("{not json"), 0o600); err != nil {
				t.Error(err)
			}
		})

		err = runFork(t, a, sourceID, &emitted)
		if err == nil {
			t.Fatal("fork should fail when the staged meta is corrupt at the candidate model persist")
		}
		if !strings.Contains(err.Error(), "fork persist model") {
			t.Fatalf("fork error = %q, want the exact fork-persist-model error", err.Error())
		}
		claim, ok, cerr := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, recordedID)
		if cerr != nil {
			t.Fatalf("claim check: %v", cerr)
		}
		if !ok {
			t.Fatal("candidate claim still held after the failed staged model persist")
		}
		_ = claim.Release()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("staged_meta_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var emitted bool
		installForkCandidateHook(a, sessionsRoot, 6, func(candDir, candidateID string) {
			if err := os.WriteFile(filepath.Join(candDir, "meta.json"), []byte("{not json"), 0o600); err != nil {
				t.Error(err)
			}
		})

		err = runFork(t, a, sourceID, &emitted)
		if err == nil {
			t.Fatal("fork should fail when the staged meta is corrupt at the summary read")
		}
		if !strings.Contains(err.Error(), "meta.json") {
			t.Fatalf("fork error = %q, want the exact staged-meta read error", err.Error())
		}
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})
}

// TestForkStrictTokenPreparation proves the fork's strict token preparation:
// a missing tokens file is valid and yields empty prepared totals, and a valid
// compaction record plus tokens file are preserved with the prepared token
// totals matching the source entries.
func TestForkStrictTokenPreparation(t *testing.T) {
	t.Run("absent_tokens_file_yields_empty_totals", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		sourceID := a.SessionCurrent().ID
		if _, err := os.Stat(filepath.Join(a.session.store.Dir(), tokensFileName)); !os.IsNotExist(err) {
			t.Fatalf("setup: tokens file present: %v", err)
		}
		result, err := a.ApplyTurnActionForSession(sourceID, 1, TurnActionFork, false)
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		if result.Tokens.Total.Input != 0 || result.Tokens.Total.Output != 0 || len(result.Tokens.PerModel) != 0 {
			t.Fatalf("fork with an absent tokens file prepared totals %+v, want empty known totals", result.Tokens)
		}
	})

	t.Run("valid_compaction_and_tokens_preserved", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 1, Summary: "summary of turn 1"}); err != nil {
			t.Fatalf("SaveCompaction: %v", err)
		}
		writeTokenFile(t, a.session, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
		sourceID := a.SessionCurrent().ID

		result, err := a.ApplyTurnActionForSession(sourceID, 1, TurnActionFork, false)
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		// The candidate preserved the compaction record and the prepared token
		// totals match the source entries.
		rec, err := a.store.LoadCompaction()
		if err != nil {
			t.Fatalf("candidate LoadCompaction: %v", err)
		}
		if rec == nil || rec.BoundaryTurn != 1 || rec.Summary != "summary of turn 1" {
			t.Fatalf("candidate compaction record = %+v, want the preserved source record", rec)
		}
		if result.Tokens.Total.Input != 11 || result.Tokens.Total.Output != 7 {
			t.Fatalf("fork prepared token totals = %+v, want input 11 output 7", result.Tokens.Total)
		}
	})
}

// TestForkSourceCopyFailureMatrix proves the fork fails precommit with the
// exact source ForkInto copy error when a source file the copy depends on is
// unreadable as a file, and that the shared precommit disposition holds.
func TestForkSourceCopyFailureMatrix(t *testing.T) {
	t.Run("turns_copy_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		// Replace the source's first turn dir with a file: ForkInto's stat
		// check fails with its exact error before any copy.
		turnDir := filepath.Join(a.store.Dir(), "turns", "1")
		if err := os.Rename(turnDir, turnDir+".orphan"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(turnDir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		restoreTurn := func() {
			_ = os.Remove(turnDir)
			_ = os.Rename(turnDir+".orphan", turnDir)
		}
		defer restoreTurn()
		var emitted bool
		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			emitted = true
		})
		if err == nil {
			t.Fatal("fork should fail when a source turn dir is not a directory")
		}
		if !strings.Contains(err.Error(), "snapshot: fork: turns/1 is not a directory") {
			t.Fatalf("fork error = %q, want the exact source turns copy error", err.Error())
		}
		restoreTurn()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("tokens_copy_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		writeTokenFile(t, a.session, TokenEntry{Provider: "test", Model: "test-model", Input: 1, Output: 1, Known: true})
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		// Replace the source tokens file with a directory: ForkInto's file
		// copy fails with EISDIR for every UID.
		tokensPath := filepath.Join(a.store.Dir(), tokensFileName)
		if err := os.Rename(tokensPath, tokensPath+".orphan"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(tokensPath, 0o700); err != nil {
			t.Fatal(err)
		}
		restoreTokens := func() {
			_ = os.Remove(tokensPath)
			_ = os.Rename(tokensPath+".orphan", tokensPath)
		}
		defer restoreTokens()
		var emitted bool
		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			emitted = true
		})
		if err == nil {
			t.Fatal("fork should fail when the source tokens file is unreadable as a file")
		}
		if !strings.Contains(err.Error(), "snapshot: fork: copy tokens") {
			t.Fatalf("fork error = %q, want the exact source tokens copy error", err.Error())
		}
		restoreTokens()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("compaction_copy_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 1, Summary: "summary"}); err != nil {
			t.Fatal(err)
		}
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		// Replace the source compaction file with a directory: ForkInto's
		// record read fails with EISDIR for every UID.
		compPath := filepath.Join(a.store.Dir(), "compaction.json")
		if err := os.Rename(compPath, compPath+".orphan"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(compPath, 0o700); err != nil {
			t.Fatal(err)
		}
		restoreComp := func() {
			_ = os.Remove(compPath)
			_ = os.Rename(compPath+".orphan", compPath)
		}
		defer restoreComp()
		var emitted bool
		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			emitted = true
		})
		if err == nil {
			t.Fatal("fork should fail when the source compaction file is unreadable as a file")
		}
		if !strings.Contains(err.Error(), "snapshot: fork: read compaction") {
			t.Fatalf("fork error = %q, want the exact source compaction read error", err.Error())
		}
		restoreComp()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})
}

// TestForkStagedFileReadFailureMatrix proves the candidate-side staged file
// read failures: the compaction-record read before the bounded history load
// and the strict token read/JSON failure each fail the fork precommit with
// their exact errors and the shared disposition.
func TestForkStagedFileReadFailureMatrix(t *testing.T) {
	t.Run("candidate_compaction_read_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 1, Summary: "summary"}); err != nil {
			t.Fatal(err)
		}
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var emitted bool
		var recordedID string
		installForkCandidateHook(a, sessionsRoot, 3, func(candDir, candidateID string) {
			recordedID = candidateID
			if err := os.WriteFile(filepath.Join(candDir, "compaction.json"), []byte("{not json"), 0o600); err != nil {
				t.Error(err)
			}
		})

		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			emitted = true
		})
		if err == nil {
			t.Fatal("fork should fail when the candidate compaction record is unreadable")
		}
		if !strings.Contains(err.Error(), "compaction.json") {
			t.Fatalf("fork error = %q, want the exact candidate compaction read error", err.Error())
		}
		claim, ok, cerr := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, recordedID)
		if cerr != nil {
			t.Fatalf("claim check: %v", cerr)
		}
		if !ok {
			t.Fatal("candidate claim still held after the failed candidate compaction read")
		}
		_ = claim.Release()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})

	t.Run("candidate_strict_tokens_failure", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "persisted")
		writeTokenFile(t, a.session, TokenEntry{Provider: "test", Model: "test-model", Input: 1, Output: 1, Known: true})
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var emitted bool
		var recordedID string
		installForkCandidateHook(a, sessionsRoot, 4, func(candDir, candidateID string) {
			recordedID = candidateID
			if err := os.WriteFile(filepath.Join(candDir, tokensFileName), []byte("{not json"), 0o600); err != nil {
				t.Error(err)
			}
		})

		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string) {
			emitted = true
		})
		if err == nil {
			t.Fatal("fork should fail when the candidate tokens file is invalid JSON")
		}
		if !strings.Contains(err.Error(), "fork read tokens") {
			t.Fatalf("fork error = %q, want the exact strict token read error", err.Error())
		}
		claim, ok, cerr := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, recordedID)
		if cerr != nil {
			t.Fatalf("claim check: %v", cerr)
		}
		if !ok {
			t.Fatal("candidate claim still held after the failed strict token read")
		}
		_ = claim.Release()
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
	})
}
