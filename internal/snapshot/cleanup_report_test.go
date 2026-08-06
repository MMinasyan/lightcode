package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

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
// so the release-failure reporting paths are reachable.
func injectReleaseFailure(t *testing.T) {
	t.Helper()
	atomicfs.ReleaseFunc = func(*atomicfs.Lock) error { return errors.New("injected release failure") }
	t.Cleanup(func() { atomicfs.ReleaseFunc = nil })
}

// TestStoreDetachReportsFailedClaimRelease proves a release failure does not
// fail the detach: the store still detaches, and the failed release is
// reported to stderr naming the session.
func TestStoreDetachReportsFailedClaimRelease(t *testing.T) {
	injectReleaseFailure(t)
	stderr := captureStderr(t)

	projectsRoot := t.TempDir()
	projectID := "p-detach"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("BeginNewSession: %v", err)
	}
	sid := store.SessionID()
	if sid == "" {
		t.Fatal("session id is empty")
	}

	store.Detach()
	out := stderr()
	if !strings.Contains(out, sid) || !strings.Contains(out, "injected release failure") {
		t.Fatalf("detach stderr = %q, want the session id and the release failure", out)
	}
}

// TestLoadSessionJoinsFailedClaimRelease proves the deferred cleanup of a
// failed load joins the release error onto the load error, so the returned
// error carries both causes: the load failure and the retained lock.
func TestLoadSessionJoinsFailedClaimRelease(t *testing.T) {
	injectReleaseFailure(t)
	stderr := captureStderr(t)

	projectsRoot := t.TempDir()
	projectID := "p-load"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	err = store.LoadSession("missing-session")
	if err == nil {
		t.Fatal("LoadSession(missing) = nil error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadSession error = %v, want the missing-meta cause", err)
	}
	if !strings.Contains(err.Error(), "injected release failure") {
		t.Fatalf("LoadSession error = %q, want the joined release failure", err)
	}
	out := stderr()
	if !strings.Contains(out, "missing-session") || !strings.Contains(out, "injected release failure") {
		t.Fatalf("load stderr = %q, want the session id and the release failure", out)
	}
}

// TestMintCreateFailureJoinsFailedClaimRelease proves the mint's directory
// failure branch joins the release error onto the create error, so the
// retained lock is part of the returned failure.
func TestMintCreateFailureJoinsFailedClaimRelease(t *testing.T) {
	injectReleaseFailure(t)
	stderr := captureStderr(t)

	projectsRoot := t.TempDir()
	projectID := "p-mint-create"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	mintSessionIDFunc = func() (string, error) { return "mint0001", nil }

	// A regular file as the create root makes the reservation directory
	// creation fail after the claim was acquired.
	createRoot := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(createRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.mintReservedSessionID(createRoot, SessionMeta{}, true)
	if err == nil {
		t.Fatal("mint with failing create root = nil error")
	}
	if !strings.Contains(err.Error(), "injected release failure") {
		t.Fatalf("mint create error = %q, want the joined release failure", err)
	}
	out := stderr()
	if !strings.Contains(out, "mint0001") || !strings.Contains(out, "injected release failure") {
		t.Fatalf("mint create stderr = %q, want the session id and the release failure", out)
	}
}

// TestMintMetaWriteFailureJoinsFailedClaimRelease proves the mint's metadata
// write branch joins the release error onto the write error: the reservation
// failed, and the release failure is still part of the returned error.
func TestMintMetaWriteFailureJoinsFailedClaimRelease(t *testing.T) {
	injectReleaseFailure(t)
	// A failing temp sync makes the meta publication fail after the
	// reservation directories were created.
	atomicfs.SyncFileFunc = func(*os.File) error { return errors.New("injected sync failure") }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	stderr := captureStderr(t)

	projectsRoot := t.TempDir()
	projectID := "p-mint-write"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	mintSessionIDFunc = func() (string, error) { return "mint0002", nil }

	_, err = store.mintReservedSessionID(t.TempDir(), SessionMeta{}, true)
	if err == nil {
		t.Fatal("mint with failing meta write = nil error")
	}
	if !strings.Contains(err.Error(), "injected release failure") {
		t.Fatalf("mint meta error = %q, want the joined release failure", err)
	}
	out := stderr()
	if !strings.Contains(out, "mint0002") || !strings.Contains(out, "injected release failure") {
		t.Fatalf("mint meta stderr = %q, want the session id and the release failure", out)
	}
}

// TestMintMetaWriteFailureLeavesNothingBehind proves a mint whose metadata
// write fails removes the reservation directory it created under the create
// root and releases the claim: a failed mint leaves no orphan the id scan
// would avoid forever, and the create root itself survives.
func TestMintMetaWriteFailureLeavesNothingBehind(t *testing.T) {
	// A failing temp sync makes the meta publication fail after the
	// reservation directories were created.
	atomicfs.SyncFileFunc = func(*os.File) error { return errors.New("injected sync failure") }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })

	projectsRoot := t.TempDir()
	projectID := "p-mint-orphan"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}
	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	mintSessionIDFunc = func() (string, error) { return "orphan001", nil }

	createRoot := t.TempDir()
	_, err = store.mintReservedSessionID(createRoot, SessionMeta{}, true)
	if err == nil {
		t.Fatal("mint with failing meta write = nil error")
	}
	if _, serr := os.Stat(filepath.Join(createRoot, "orphan001")); !os.IsNotExist(serr) {
		t.Fatalf("reservation directory still present after failed mint (stat err = %v)", serr)
	}
	if _, serr := os.Stat(createRoot); serr != nil {
		t.Fatalf("create root removed by failed mint: %v", serr)
	}
	if store.claim != nil {
		t.Fatal("failed mint retained the session claim")
	}
}

// TestSweepReportsFailedClaimRelease proves a release failure does not fail
// the sweep: the candidate is still processed, and the failed release is
// reported to stderr naming the session.
func TestSweepReportsFailedClaimRelease(t *testing.T) {
	injectReleaseFailure(t)
	stderr := captureStderr(t)

	projectsRoot := t.TempDir()
	projectID := "p-sweep-rel"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
	writeSession := func(id string, meta SessionMeta) {
		dir := filepath.Join(sessionsRoot, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
			t.Fatal(err)
		}
	}
	writeSession("sweeprel1", SessionMeta{ID: "sweeprel1", State: StateActive, LastActivity: 1})

	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 3650}
	archived, deleted, err := SweepAllProjects(projectsRoot, cfg, nil, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 1 || deleted != 0 {
		t.Fatalf("sweep counts = archived:%d deleted:%d, want 1 archived 0 deleted", archived, deleted)
	}
	out := stderr()
	if !strings.Contains(out, "sweeprel1") || !strings.Contains(out, "injected release failure") {
		t.Fatalf("sweep stderr = %q, want the session id and the release failure", out)
	}
}
