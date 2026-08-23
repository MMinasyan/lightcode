package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// TestIdentityOperationsRefuseAfterShutdown pins the admission gate on
// session-identity operations: after ShutdownOwner returns, an open, a new
// session and a fork each refuse with errOwnerClosed, before any of them
// builds a store, takes a claim, or registers a session — so nothing lands
// after the shutdown snapshot with a claim nothing will ever detach.
func TestIdentityOperationsRefuseAfterShutdown(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "persisted")
	unit := a.session
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("no session id after the first turn")
	}
	projectID := unit.projectID
	if !a.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}

	if _, err := a.OpenSession(id); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("OpenSession after shutdown = %v, want errOwnerClosed", err)
	}
	if _, err := a.NewSession("", "primary"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("NewSession after shutdown = %v, want errOwnerClosed", err)
	}
	// The exported fork entries resolve the live unit before the gate, and
	// after shutdown no unit is live, so the fork is driven through the
	// internal fork entry with the pre-shutdown unit. Exception recorded here:
	// the gate under test is the fork's own entry check, reachable only after
	// the unit resolution the exported entries perform.
	release := a.lockLifecycle()
	_, forkErr := a.applyForkTurnAction(unit, 1, false, nil)
	release()
	if !errors.Is(forkErr, errOwnerClosed) {
		t.Fatalf("fork after shutdown = %v, want errOwnerClosed", forkErr)
	}

	// The refusals left no live-session entry: nothing was registered by any
	// of the three refused operations, and the pre-shutdown unit's store was
	// detached by the shutdown itself.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	for sid, u := range a.sessions {
		if u != nil && u.store != nil && u.store.Active() {
			t.Fatalf("session %q still live after shutdown and refused identity operations", sid)
		}
	}
	rt.mu.Unlock()

	// And no claim is held: the session the refused open targeted is claimable
	// by a fresh owner.
	lock, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), projectID, id)
	if err != nil {
		t.Fatalf("acquire session claim: %v", err)
	}
	if !ok {
		t.Fatal("session claim still held after shutdown and refused identity operations")
	}
	_ = lock.Release()
}

// TestShutdownBarrierWaitsForInFlightResume asserts the shutdown barrier as
// ordering rather than as a race: a resume paused mid-span holds lifecycleMu,
// so ShutdownOwner cannot publish closed until the resume has finished and
// registered its unit; the shutdown snapshot then covers that registration and
// detaches its store. Without the barrier, shutdown would publish mid-span and
// the paused operation would go on to claim and register after the snapshot,
// stranding a claim nothing ever detaches.
func TestShutdownBarrierWaitsForInFlightResume(t *testing.T) {
	t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	a := newResumeRaceAgent(t, home, projectRoot)
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := a.store.AttachSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach sessions root: %v", err)
	}
	if err := a.store.BeginNewSession(projectRoot); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	raw, err := json.Marshal(message.NewText(message.RoleUser, "hello"))
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := a.store.AppendMessage(1, raw); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := a.store.MarkTurnComplete(1); err != nil {
		t.Fatalf("mark turn complete: %v", err)
	}
	if _, err := a.store.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// Pause resume at its first durable read — immediately before it lists
	// candidates, after the entry check. The hook blocks every later fire too,
	// but the release is closed before any of them can matter.
	firstRead := make(chan struct{})
	var firstOnce sync.Once
	release := make(chan struct{})
	a.durableReadHook = func() {
		firstOnce.Do(func() { close(firstRead) })
		<-release
	}
	defer func() { a.durableReadHook = nil }()

	resumeDone := make(chan struct{})
	var resumedID string
	go func() {
		resumedID, _ = a.resumeMostRecent()
		close(resumeDone)
	}()
	<-firstRead

	// The completion probe must happen when shutdown is one statement from the
	// barrier acquire, not an unbounded distance from it, so the seam fires
	// inside ShutdownOwner immediately before that acquire.
	shutdownStarted := make(chan struct{})
	a.shutdownBarrierHook = func() { close(shutdownStarted) }
	defer func() { a.shutdownBarrierHook = nil }()
	shutdownDone := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(shutdownDone)
	}()
	// Shutdown must still be waiting on the barrier: the paused resume holds
	// lifecycleMu, so ShutdownOwner cannot have returned while the resume is
	// mid-span, and a completed ShutdownOwner is a definite barrier violation.
	// The probe is not airtight: the seam fires one statement before the
	// acquire, and nothing observes that the goroutine is actually blocked on
	// the mutex, so one statement of window remains — the detached-store
	// assertion below is what catches a shutdown that slips past it.
	<-shutdownStarted
	select {
	case <-shutdownDone:
		t.Fatal("ShutdownOwner returned while an identity operation was mid-span")
	default:
	}

	close(release)
	<-resumeDone
	<-shutdownDone

	if resumedID == "" {
		t.Fatal("resume did not complete")
	}
	// The shutdown snapshot covered the late registration and detached its
	// store: the claim the resume took is released, not stranded.
	unit := a.sessions[resumedID]
	if unit == nil || unit.store == nil || unit.store.Active() {
		t.Fatal("resumed session store still active after shutdown: the registration landed after the snapshot")
	}
	// Admission is closed for later identity operations too.
	if _, err := a.OpenSession(resumedID); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("OpenSession after shutdown = %v, want errOwnerClosed", err)
	}
	if _, err := a.NewSession("", "primary"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("NewSession after shutdown = %v, want errOwnerClosed", err)
	}
}

// parkAtLifecycleAdmission arms the one-shot lifecycle-admission hook: the
// next lockLifecycle caller (the operation under test) signals parked and
// blocks immediately before acquiring the owner lifecycle lock, and every
// later caller — including ShutdownOwner's own acquisition — passes through
// immediately. The one-shot gate makes the ordering deterministic: the test
// waits for parked, runs ShutdownOwner, and only then releases the operation.
func parkAtLifecycleAdmission(t *testing.T, a *Agent) (<-chan struct{}, func()) {
	t.Helper()
	parked := make(chan struct{})
	release := make(chan struct{})
	gate := make(chan struct{})
	var releaseOnce sync.Once
	a.lifecycleAdmissionHook = func() {
		select {
		case <-gate:
			return
		default:
		}
		close(gate)
		close(parked)
		<-release
	}
	t.Cleanup(func() {
		a.lifecycleAdmissionHook = nil
		releaseOnce.Do(func() { close(release) })
	})
	return parked, func() { releaseOnce.Do(func() { close(release) }) }
}

// TestLifecycleOpsAfterClosedRefuseWithoutClaim pins the post-lifecycleMu
// admission recheck on the owner-mutating lifecycle operations: an operation
// admitted by the host before close but acquiring lifecycleMu after close
// refuses with errOwnerClosed before any claim, reservation, durable write, or
// rename. The barrier is the exact linearization point: the operation parks
// at lifecycle admission (immediately before its lock acquisition), the owner
// publishes close through ShutdownOwner and completes, and only then is the
// operation released — so it acquires lifecycleMu after close and must
// refuse. A closed check placed before lifecycle admission would run while
// close is still unpublished, let the operation park, and then proceed to
// mutate after close — which the durable assertions below catch.
func TestLifecycleOpsAfterClosedRefuseWithoutClaim(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, a *Agent) (string, *session)
		run   func(a *Agent, id string) error
	}{
		{
			name: "archive",
			setup: func(t *testing.T, a *Agent) (string, *session) {
				appendUserTurn(t, a, "persisted")
				return a.SessionCurrent().ID, a.session
			},
			run: func(a *Agent, id string) error { return a.SessionArchive(id) },
		},
		{
			name: "delete",
			setup: func(t *testing.T, a *Agent) (string, *session) {
				appendUserTurn(t, a, "persisted")
				return a.SessionCurrent().ID, a.session
			},
			run: func(a *Agent, id string) error { return a.SessionDelete(id) },
		},
		{
			name: "revert_history_turn_action",
			setup: func(t *testing.T, a *Agent) (string, *session) {
				appendUserTurn(t, a, "persisted")
				return a.SessionCurrent().ID, a.session
			},
			run: func(a *Agent, id string) error {
				_, err := a.ApplyTurnActionForSession(id, 1, TurnActionRevertHistory, false)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			id, unit := tc.setup(t, a)
			if id == "" {
				t.Fatal("no session id after the setup turn")
			}
			root := unit.store.Root()

			parked, release := parkAtLifecycleAdmission(t, a)
			errCh := make(chan error, 1)
			go func() { errCh <- tc.run(a, id) }()
			select {
			case <-parked:
			case <-time.After(5 * time.Second):
				t.Fatal("operation never parked at lifecycle admission")
			}
			// The owner publishes close and completes while the operation is
			// parked mid-admission.
			if !a.ShutdownOwner() {
				t.Fatal("clean shutdown reported abandoned in-flight work")
			}
			release()
			if err := <-errCh; !errors.Is(err, errOwnerClosed) {
				t.Fatalf("%s after close = %v, want errOwnerClosed", tc.name, err)
			}
			// Durable no-mutation: the session's metadata is still active and
			// its directory still exists on disk.
			meta, err := snapshot.LoadSessionMeta(root, id)
			if err != nil {
				t.Fatalf("load session meta: %v", err)
			}
			if meta.State != snapshot.StateActive {
				t.Fatalf("session state after refused %s = %q, want active", tc.name, meta.State)
			}
			if _, err := os.Stat(filepath.Join(root, id)); err != nil {
				t.Fatalf("session dir after refused %s: %v", tc.name, err)
			}
		})
	}
}

// TestLifecycleOpsAfterClosedRefuseWithoutClaimPersistedOnly proves the same
// refusal on the persisted-only removal path, where "without claim" is
// directly observable: the refused delete takes no temporary claim, so the
// session stays on disk and claimable by a fresh owner. The delete parks at
// lifecycle admission, the second owner publishes close and completes, then
// the delete is released.
func TestLifecycleOpsAfterClosedRefuseWithoutClaimPersistedOnly(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// Archive releases the first owner's claim, leaving the session on disk
	// and unclaimed — a persisted-only target for the second owner.
	if err := first.SessionArchive(id); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}
	proj, err := first.projects.Current()
	if err != nil || proj == nil {
		t.Fatalf("current project: %v", err)
	}
	sessionsRoot := first.projects.SessionsRoot(proj.ID)

	parked, release := parkAtLifecycleAdmission(t, second)
	errCh := make(chan error, 1)
	go func() { errCh <- second.SessionDelete(id) }()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("delete never parked at lifecycle admission")
	}
	if !second.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}
	release()
	if err := <-errCh; !errors.Is(err, errOwnerClosed) {
		t.Fatalf("persisted-only SessionDelete after close = %v, want errOwnerClosed", err)
	}

	// No claim was taken and no rename happened: the session is still on disk
	// and its claim is acquirable by a fresh owner.
	if _, err := os.Stat(filepath.Join(sessionsRoot, id)); err != nil {
		t.Fatalf("session dir after refused delete: %v", err)
	}
	lock, ok, err := snapshot.AcquireSessionClaim(first.projects.Root(), proj.ID, id)
	if err != nil || !ok {
		t.Fatalf("claim after refused delete: ok=%v err=%v, want acquirable", ok, err)
	}
	_ = lock.Release()
}

// TestCodeRevertAfterClosedRefusesWithoutMutation is the code-revert
// close-first sibling of TestLifecycleOpsAfterClosedRefuseWithoutClaim,
// covering both revert routes: the direct current-session RevertCode and the
// shared session-scoped RevertCodeForSession. With a real snapshot and a
// tracked read in place, a revert admitted by the host before close but
// entering the owner after close must refuse with errOwnerClosed before the
// revert body — the modified file is not restored and the file tracker is not
// reset. The revert parks at lifecycle admission, the owner publishes close
// through ShutdownOwner and completes, then the revert is released.
func TestCodeRevertAfterClosedRefusesWithoutMutation(t *testing.T) {
	cases := []struct {
		name string
		run  func(a *Agent, id string) error
	}{
		{
			name: "direct",
			run:  func(a *Agent, id string) error { _, err := a.RevertCode(1); return err },
		},
		{
			name: "shared",
			run:  func(a *Agent, id string) error { _, err := a.RevertCodeForSession(id, 1); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			path := filepath.Join(a.projectRoot, "revert-target.txt")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			appendUserTurnWithSnapshot(t, a, "persisted", path, "original")
			unit := a.session
			id := unit.store.SessionID()
			if id == "" {
				t.Fatal("no session id after the setup turn")
			}
			// The file is modified after the snapshotted turn; a revert to
			// turn 1 would restore it to "original".
			if err := os.WriteFile(path, []byte("mutated"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Track one read so the tracker has observable state a revert
			// would reset.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			unit.fileTracker.TrackIdentity(path, 0, 0, tool.FileIdentityFromFileInfo(info))
			trackerBefore := unit.fileTracker.Snapshot()
			if len(trackerBefore) == 0 {
				t.Fatal("tracked read was not recorded")
			}

			parked, release := parkAtLifecycleAdmission(t, a)
			errCh := make(chan error, 1)
			go func() { errCh <- tc.run(a, id) }()
			select {
			case <-parked:
			case <-time.After(5 * time.Second):
				t.Fatal("revert never parked at lifecycle admission")
			}
			if !a.ShutdownOwner() {
				t.Fatal("clean shutdown reported abandoned in-flight work")
			}
			release()
			if err := <-errCh; !errors.Is(err, errOwnerClosed) {
				t.Fatalf("%s code revert after close = %v, want errOwnerClosed", tc.name, err)
			}

			// The file was not restored and the tracker was not reset: the
			// revert body never ran. The durable metadata is untouched.
			if got, rerr := os.ReadFile(path); rerr != nil || string(got) != "mutated" {
				t.Fatalf("target file = %q, %v; want still mutated (revert must not run)", got, rerr)
			}
			if !reflect.DeepEqual(unit.fileTracker.Snapshot(), trackerBefore) {
				t.Fatal("file tracker was reset by the refused code revert")
			}
			meta, err := snapshot.LoadSessionMeta(unit.store.Root(), id)
			if err != nil {
				t.Fatalf("load session meta: %v", err)
			}
			if meta.State != snapshot.StateActive {
				t.Fatalf("session state after refused code revert = %q, want active", meta.State)
			}
		})
	}
}

// TestShutdownOwnerClosesProcessAdmissionBeforeJoins proves process admission
// closes at the start of shutdown, before any join: with an admitted process
// running and the background join held by a parked queue drainer, shutdown
// must kill and reap the admitted process before it begins waiting on the
// joins. The parked drainer makes the ordering deterministic on both
// branches: a shutdown that closed process admission only after the joins
// (the forbidden sibling) can never reach the kill while the drainer is
// parked, so the process stays alive and the test fails.
func TestShutdownOwnerClosesProcessAdmissionBeforeJoins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hello")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	startEventOrderAgent(t, a, &eventCapture{})

	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sessionID := a.SessionCurrent().ID
	if sessionID == "" {
		t.Fatal("no session id")
	}

	// Park the queue drainer: the background join is held until release.
	rt := a.ensureRuntime()
	parked := make(chan struct{})
	releaseDrainer := make(chan struct{})
	var parkOnce sync.Once
	var releaseOnce sync.Once
	rt.queueDrainPassHook = func() {
		parkOnce.Do(func() { close(parked) })
		<-releaseDrainer
	}
	defer func() {
		rt.queueDrainPassHook = nil
		releaseOnce.Do(func() { close(releaseDrainer) })
	}()
	rt.nudgeQueueDrainer()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("queue drainer never parked")
	}

	// An admitted background process, registered and running before shutdown.
	pidFile := filepath.Join(t.TempDir(), "proc.pid")
	procID, err := a.procMgr.Start("echo $$ > "+pidFile+"; exec sleep 100", 0)
	if err != nil {
		t.Fatalf("start background process: %v", err)
	}
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.procMgr.ActiveIDsForSession(sessionID)) > 0 {
			if data, rerr := os.ReadFile(pidFile); rerr == nil {
				if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
					pid = p
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("background process %s never started", procID)
	}

	shutdownDone := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(shutdownDone)
	}()

	// While shutdown is still inside its joins (the parked drainer holds the
	// background join), the admitted process must already be killed and
	// reaped: CloseAdmission snapshotted it and CloseWait ran before any
	// join. kill(pid, 0) returns ESRCH only once reaped.
	deadline = time.Now().Add(3 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			reaped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reaped {
		t.Fatal("admitted process still alive while shutdown waits on its joins: process admission closed after the joins")
	}
	select {
	case <-shutdownDone:
		t.Fatal("ShutdownOwner returned while the background join was still held")
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseDrainer) })
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ShutdownOwner did not complete after the drainer was released")
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
}

// TestNewSessionForProjectPathAfterShutdownPublishesNothing proves a refused
// new-session call does not create a project or session after owner shutdown.
func TestNewSessionForProjectPathAfterShutdownPublishesNothing(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if !a.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}

	newPath := filepath.Join(t.TempDir(), "unseen-project")
	if _, err := a.NewSessionForProjectPath(newPath, "primary"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("NewSessionForProjectPath after shutdown = %v, want errOwnerClosed", err)
	}

	proj, err := project.FindByPath(a.projects.Root(), newPath)
	if err != nil {
		t.Fatalf("find created project: %v", err)
	}
	if proj != nil {
		t.Fatal("project record created by the refused new-session call")
	}
}
