package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestSessionLifecycleTransactionContract asserts that a failed session
// creation leaves nothing behind: when newSession fails, the sessions root
// holds no session directory, no session is registered or selected, and no
// staging tree survives.
//
// It does not discriminate the ordering of the fallible steps relative to the
// durable commit, and would pass against an implementation that persists the
// model and reads the session meta after publishing. A failure cannot be
// injected between the prepare and publish calls, because the staged
// candidate is created inside newSession under a staging directory whose name
// is not known to the caller, and reaching it would require production
// surface that exists only for testing.
func TestSessionLifecycleTransactionContract(t *testing.T) {
	t.Run("shape=A/case=new_staging_partial_failure", func(t *testing.T) {
		// The failure is injected at the durable commit, not at the pre-commit
		// fallible steps (persisting the model, reading the session meta):
		// those steps run against a staging directory that
		// PrepareStagedNewSession creates fresh inside the call with random
		// nonce and session ids, so no filesystem state arranged beforehand
		// can fail exactly those steps through the exported surface — any
		// state that breaks them breaks preparation first, and the agent
		// exposes no injection seam for them. The deepest fallible step that
		// is externally determinable is the commit itself: make the sessions
		// root unwritable so the atomic rename cannot happen. The assertions
		// therefore cover only what that failure leaves behind — nothing in
		// the sessions root, nothing registered or selected, and no staging
		// tree.
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(sessionsRoot, 0o700) }()

		emitCalled := false
		if _, err := a.NewSessionWithBoundary(proj.ID, "primary", func(HydrationState) { emitCalled = true }); err == nil {
			t.Fatal("NewSessionWithBoundary should fail when the sessions root is unwritable")
		}

		// Nothing published: the sessions root holds no session directory, and
		// listing sees no partially created session.
		entries, err := os.ReadDir(sessionsRoot)
		if err != nil {
			t.Fatalf("read sessions root: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("session directory %q left in sessions root after failed creation", e.Name())
			}
		}
		list, err := snapshot.List(sessionsRoot, proj.Path, snapshot.StateActive)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("failed creation left listed sessions: %#v", list)
		}

		// The staged candidate is gone too, and the boundary capture — the
		// in-commit publish step — never ran.
		if staging, _ := os.ReadDir(filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")); len(staging) != 0 {
			t.Fatalf("staging left uncleaned: %v", staging)
		}
		if emitCalled {
			t.Fatal("boundary emit ran for a session that was never published")
		}

		// Nothing registered or selected in memory.
		a.ensureRuntime().mu.Lock()
		registered := len(a.sessions)
		current := a.currentSessionID
		a.ensureRuntime().mu.Unlock()
		if registered != 0 {
			t.Fatalf("failed creation registered %d live sessions", registered)
		}
		if current != "" {
			t.Fatalf("failed creation selected session %q", current)
		}
		if a.SessionCurrent().ID != "" {
			t.Fatalf("SessionCurrent = %q, want none", a.SessionCurrent().ID)
		}

		// The failed transaction left the namespace reusable: once the root is
		// writable again, a fresh creation succeeds and is selected, and the
		// published session is correctly claimed — a second claim on it is
		// refused.
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("creation after failed transaction: %v", err)
		}
		if a.SessionCurrent().ID != id {
			t.Fatalf("SessionCurrent = %q, want the new session %q", a.SessionCurrent().ID, id)
		}
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
		if err != nil {
			t.Fatalf("claim check: %v", err)
		}
		if ok {
			_ = claim.Release()
			t.Fatal("published session is not claimed by the live unit")
		}
	})
}
