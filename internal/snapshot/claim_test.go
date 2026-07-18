package snapshot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionIDPathBoundary(t *testing.T) {
	for _, id := range []string{"a1b2c3d4", "session-1", "abc123"} {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "/abs", "../escape", "sub/dir", string(filepath.Separator)} {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("ValidateSessionID(%q) = nil, want error", id)
		}
	}
	// A traversal id is rejected before any filesystem effect.
	if _, _, err := AcquireSessionClaim(t.TempDir(), "p-x", ".."); err == nil {
		t.Error("AcquireSessionClaim(..) = nil error, want rejection")
	}
}

func TestSessionClaimLifecycle(t *testing.T) {
	root := t.TempDir()
	projectID := "p-claim"
	sid := "sess1"

	claim, ok, err := AcquireSessionClaim(root, projectID, sid)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// Contention: a second claim on the same session fails without an error.
	if c2, ok2, err2 := AcquireSessionClaim(root, projectID, sid); err2 != nil || ok2 {
		if c2 != nil {
			c2.Release()
		}
		t.Fatalf("contended claim: ok=%v err=%v, want false/nil", ok2, err2)
	}
	// A different session is independently claimable while the first is held.
	if c3, ok3, err3 := AcquireSessionClaim(root, projectID, "other"); err3 != nil || !ok3 {
		t.Fatalf("other-session claim: ok=%v err=%v", ok3, err3)
	} else {
		c3.Release()
	}
	// Release lets the same session be reacquired.
	if err := claim.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	c4, ok4, err4 := AcquireSessionClaim(root, projectID, sid)
	if err4 != nil || !ok4 {
		t.Fatalf("reacquire after release: ok=%v err=%v", ok4, err4)
	}
	c4.Release()
}

// TestStoreLoadSessionClaimsExclusively proves that a store claims its
// session on load/begin and releases it on Close: a second owner cannot load
// the same session while the first drives it, but can once the first closes.
func TestStoreLoadSessionClaimsExclusively(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-two-owner"
	root := filepath.Join(projectsRoot, projectID, "sessions")

	first, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	if err := first.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	sid := first.SessionID()

	second, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	if err := second.LoadSession(sid); !errors.Is(err, ErrSessionContended) {
		t.Fatalf("second owner LoadSession = %v, want ErrSessionContended", err)
	}

	if _, err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Close released the claim, so it is reacquirable.
	claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, sid)
	if err != nil || !ok {
		t.Fatalf("claim not released after Close: ok=%v err=%v", ok, err)
	}
	claim.Release()
}

// TestSweepClaimsCandidatesAndSkipsContended proves the sweep claims each
// candidate before mutating and skips one that is contended (driven by a live
// holder), while archiving a free, eligible session.
func TestSweepClaimsCandidatesAndSkipsContended(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep"
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
	writeSession("free1", SessionMeta{ID: "free1", State: StateActive, LastActivity: 1})
	writeSession("held1", SessionMeta{ID: "held1", State: StateActive, LastActivity: 1})

	// Simulate a live holder driving held1.
	held, ok, err := AcquireSessionClaim(projectsRoot, projectID, "held1")
	if err != nil || !ok {
		t.Fatalf("hold claim: ok=%v err=%v", ok, err)
	}
	defer held.Release()

	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 3650}
	if _, _, err := SweepAllProjects(projectsRoot, cfg, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if fm, _ := LoadSessionMeta(sessionsRoot, "free1"); effectiveState(fm.State) != StateArchived {
		t.Fatalf("free session not archived: %q", fm.State)
	}
	if hm, _ := LoadSessionMeta(sessionsRoot, "held1"); effectiveState(hm.State) != StateActive {
		t.Fatalf("contended session was swept: %q", hm.State)
	}
}

// TestSessionClaimReleasedOnHolderExit proves a claim held by a process that
// exits without unlocking is reacquirable: the OS drops the flock on process
// exit, so a killed holder never strands a session.
func TestSessionClaimReleasedOnHolderExit(t *testing.T) {
	root := os.Getenv("LIGHTCODE_CLAIM_HOLDER_ROOT")
	if root != "" {
		// Child process: acquire the claim and exit while still holding it.
		_, ok, err := AcquireSessionClaim(root, "p-killed", "sess-killed")
		if err != nil || !ok {
			os.Exit(2)
		}
		os.Exit(0)
	}

	root = t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionClaimReleasedOnHolderExit$")
	cmd.Env = append(os.Environ(), "LIGHTCODE_CLAIM_HOLDER_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("holder subprocess failed: %v\n%s", err, out)
	}
	claim, ok, err := AcquireSessionClaim(root, "p-killed", "sess-killed")
	if err != nil || !ok {
		t.Fatalf("reacquire after holder exit: ok=%v err=%v", ok, err)
	}
	claim.Release()
}
