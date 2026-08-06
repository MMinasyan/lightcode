package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
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

// TestNewSessionForProjectPathAfterShutdownLeavesInertProject pins the one
// durable mutation that sits above the admission gate: ensureProjectForPath
// runs before the checked path, so after shutdown a NewSessionForProjectPath
// on a new path refuses but may leave the project record it created — a
// meta.json and an empty sessions directory, no session and no claim, inert
// on the next start.
func TestNewSessionForProjectPathAfterShutdownLeavesInertProject(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if !a.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}

	newPath := filepath.Join(t.TempDir(), "unseen-project")
	if _, err := a.NewSessionForProjectPath(newPath, "primary"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("NewSessionForProjectPath after shutdown = %v, want errOwnerClosed", err)
	}

	// The project record the pre-gate mutation created is inert: it carries
	// no session.
	proj, err := project.FindByPath(a.projects.Root(), newPath)
	if err != nil {
		t.Fatalf("find created project: %v", err)
	}
	if proj == nil {
		t.Fatal("project record not created by the refused new-session call")
	}
	entries, err := os.ReadDir(filepath.Join(a.projects.Root(), proj.ID, "sessions"))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("sessions dir has %d entries, want none (the refused call published nothing)", len(entries))
	}
}
