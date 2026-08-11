package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// failingDeleteMemoryHooks is a memory-hooks fake whose summary deletion
// always fails, so the delete-summaries reporting paths are reachable.
type failingDeleteMemoryHooks struct {
	fakeMemoryHooks
}

func (h *failingDeleteMemoryHooks) DeleteSessionSummaries(sessionID string) error {
	h.deleteCalls++
	return errors.New("injected summaries delete failure")
}

// captureStderr redirects os.Stderr into a temp file for the test's duration
// and returns a func that reads what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
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

// injectReleaseFailure forces every atomicfs Lock.Release in the test to fail,
// so the claim-release reporting paths are reachable.
func injectReleaseFailure(t *testing.T) {
	t.Helper()
	atomicfs.ReleaseFunc = func(*atomicfs.Lock) error { return errors.New("injected release failure") }
	t.Cleanup(func() { atomicfs.ReleaseFunc = nil })
}

// TestSessionDeleteReportsFailedSummariesDeletion proves a failed summaries
// deletion does not fail the delete: SessionDelete still succeeds, and the
// residue is reported to stderr naming the session.
func TestSessionDeleteReportsFailedSummariesDeletion(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hooks := &failingDeleteMemoryHooks{}
	a.memoryHooks = hooks
	stderr := captureStderr(t)

	if err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}
	if hooks.deleteCalls != 1 {
		t.Fatalf("DeleteSessionSummaries calls = %d, want 1", hooks.deleteCalls)
	}
	out := stderr()
	if !strings.Contains(out, id) || !strings.Contains(out, "injected summaries delete failure") {
		t.Fatalf("SessionDelete stderr = %q, want the session id and the deletion failure", out)
	}
}

// TestSweepReportsFailedSummariesDeletion proves the sweep's post-delete
// summaries cleanup reports a failure to stderr while the sweep still
// completes: the deleted session stays deleted.
func TestSweepReportsFailedSummariesDeletion(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	sessionsRoot := a.projects.SessionsRoot(proj.ID)
	const id = "sweepdel1"
	dir := filepath.Join(sessionsRoot, id)
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"id":%q,"state":"archived","archived_at":1}`, id)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Sessions.AutoArchive = true
	a.cfg.Sessions.ArchiveAfterDays = 1
	a.cfg.Sessions.DeleteAfterArchiveDays = 1
	hooks := &failingDeleteMemoryHooks{}
	a.memoryHooks = hooks
	stderr := captureStderr(t)

	a.runSweep()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("swept session still on disk after sweep: %v", err)
	}
	if hooks.deleteCalls != 1 {
		t.Fatalf("DeleteSessionSummaries calls = %d, want 1", hooks.deleteCalls)
	}
	out := stderr()
	if !strings.Contains(out, id) || !strings.Contains(out, "injected summaries delete failure") {
		t.Fatalf("sweep stderr = %q, want the session id and the deletion failure", out)
	}
}

// TestRevertBelowBoundaryReportsFailedSummariesDeletion proves the history
// revert's summaries deletion reports a failure to stderr without failing the
// revert: the turn truncation commits regardless.
func TestRevertBelowBoundaryReportsFailedSummariesDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hook summary")
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	sessionID := a.store.SessionID()
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	hooks := &failingDeleteMemoryHooks{}
	a.memoryHooks = hooks
	stderr := captureStderr(t)

	if err := a.runCompaction(context.Background(), false); err != nil {
		t.Fatalf("runCompaction: %v", err)
	}
	if _, err := a.ApplyTurnActionForSession(sessionID, 6, TurnActionRevertHistory, false); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if hooks.deleteCalls != 1 {
		t.Fatalf("DeleteSessionSummaries calls = %d, want 1", hooks.deleteCalls)
	}
	out := stderr()
	if !strings.Contains(out, sessionID) || !strings.Contains(out, "injected summaries delete failure") {
		t.Fatalf("revert stderr = %q, want the session id and the deletion failure", out)
	}
}

// TestPersistedOnlyDeleteSurvivesFailedClaimRelease proves the temporary
// claim taken for a persisted-only delete is released through the same
// reporting helper as the store: a release failure is reported to stderr
// naming the session and does not fail the delete.
func TestPersistedOnlyDeleteSurvivesFailedClaimRelease(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// The first owner must stop driving the session before the second can
	// delete it: archive releases its claim, leaving the session on disk and
	// unclaimed, so the second owner's delete takes a fresh temporary claim.
	if err := first.SessionArchive(id); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}
	injectReleaseFailure(t)
	stderr := captureStderr(t)

	if err := second.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete on a persisted-only session: %v", err)
	}
	out := stderr()
	if !strings.Contains(out, id) || !strings.Contains(out, "injected release failure") {
		t.Fatalf("persisted-only delete stderr = %q, want the session id and the release failure", out)
	}
}

// TestDeferredStoreCloseFailureReportsStderr proves a deferred child/compact
// store close keeps the caller's primary result and reports the cleanup
// failure to stderr: the store's empty-session discard fails, yet the store is
// left inactive and its claim released, so the cleanup failure cannot retain
// drive authority. The real deferred sites (runSubagent and the compaction
// caller) always complete the child turn before closing, so the discard path
// is unreachable through them; it is exercised through the same production
// helper the deferred sites call, with a real child store.
func TestDeferredStoreCloseFailureReportsStderr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	proj, err := a.projects.Current()
	if err != nil || proj == nil {
		t.Fatalf("current project: %v", err)
	}
	parentID := a.SessionCurrent().ID
	childStore, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("child store: %v", err)
	}
	if err := childStore.BeginChildSession(a.ProjectRoot(), parentID); err != nil {
		t.Fatalf("BeginChildSession: %v", err)
	}
	childID := childStore.SessionID()
	// Make the empty child dir's discard fail; the claim lives outside the dir.
	childDir := childStore.Dir()
	if err := os.Chmod(childDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(childDir, 0o700) }()
	stderr := captureStderr(t)

	closeStoreDeferred(childStore, "subagent")

	out := stderr()
	if !strings.Contains(out, childID) || !strings.Contains(out, "subagent") {
		t.Fatalf("deferred close stderr = %q, want the child session and the caller named", out)
	}
	if childStore.Active() {
		t.Fatal("deferred close left the child store active")
	}
	// The claim was released despite the failed removal: a fresh store can
	// load the child.
	fresh, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := fresh.LoadSession(childID); err != nil {
		t.Fatalf("child claim still held after the failed deferred close: %v", err)
	}
	fresh.Detach()
}
