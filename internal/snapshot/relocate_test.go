package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreRelocateActiveSessionPaths proves the no-I/O path relocation: after
// a session dir is staged then atomically renamed into the sessions namespace,
// RelocateActiveSessionPaths repoints the active store's cached paths so it
// reads and writes at the final location, with the claim unaffected.
func TestStoreRelocateActiveSessionPaths(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-relocate"
	stagingRoot := filepath.Join(projectsRoot, projectID, ".staging", "sessions", "nonce1")
	finalRoot := filepath.Join(projectsRoot, projectID, "sessions")

	store, err := NewForSessionsRoot(stagingRoot, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	sid := store.SessionID()
	if store.Dir() != filepath.Join(stagingRoot, sid) {
		t.Fatalf("staged dir = %q", store.Dir())
	}

	// Atomic rename: move <staging>/<sid> into the final sessions namespace.
	if err := os.MkdirAll(finalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(stagingRoot, sid), filepath.Join(finalRoot, sid)); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if err := store.RelocateActiveSessionPaths(finalRoot); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if store.Root() != finalRoot || store.Dir() != filepath.Join(finalRoot, sid) {
		t.Fatalf("paths not relocated: root=%q dir=%q", store.Root(), store.Dir())
	}
	// The store reads/writes at the final location, and the claim still holds.
	if _, err := store.Meta(); err != nil {
		t.Fatalf("Meta after relocate: %v", err)
	}
	if err := store.SetActiveAgentType("primary"); err != nil {
		t.Fatalf("write after relocate: %v", err)
	}
	if _, ok, _ := AcquireSessionClaim(projectsRoot, projectID, sid); ok {
		t.Fatal("claim was released by relocation")
	}
	store.Detach()
}

// TestAtomicSessionDelete verifies the delete commit removes the session
// from the sessions namespace atomically (via rename), leaves it unlistable,
// and rejects a traversal id before any filesystem effect.
func TestAtomicSessionDelete(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-del"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
	dir := filepath.Join(sessionsRoot, "sess1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), SessionMeta{ID: "sess1", State: StateActive}); err != nil {
		t.Fatal(err)
	}

	if err := DeleteSession(sessionsRoot, "sess1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir still present after delete: %v", err)
	}
	entries, _ := os.ReadDir(sessionsRoot)
	if len(entries) != 0 {
		t.Fatalf("sessions root not empty after delete: %v", entries)
	}
	if err := DeleteSession(sessionsRoot, "../escape"); err == nil {
		t.Fatal("traversal id accepted by delete")
	}
}
