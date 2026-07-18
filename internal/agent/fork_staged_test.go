package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
)

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

		if err := a.ForkSession(2); err != nil {
			t.Fatalf("ForkSession: %v", err)
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
		forkErr := a.ForkSession(1)
		a.ensureRuntime().mu.Lock()
		a.session.busy = false
		a.ensureRuntime().mu.Unlock()
		if forkErr == nil {
			t.Fatal("fork of a busy source should be rejected")
		}

		// Rejected before any staging: current unchanged, no staging created.
		if a.SessionCurrent().ID != sourceID {
			t.Fatalf("current changed to %q after rejected fork", a.SessionCurrent().ID)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(sessionsRoot), ".staging")); !os.IsNotExist(err) {
			t.Fatalf("staging created for a rejected fork: %v", err)
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

		if _, err := a.ApplyTurnAction(clicked, TurnActionFork, true); err == nil {
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

		res, err := a.ApplyTurnAction(clicked, TurnActionFork, true)
		if err != nil {
			t.Fatalf("best-effort code revert must not fail a committed fork: %v", err)
		}
		if res.Session.ID == "" || res.Session.ID == sourceID || a.SessionCurrent().ID != res.Session.ID {
			t.Fatalf("fork not current after best-effort revert: res=%q current=%q source=%q", res.Session.ID, a.SessionCurrent().ID, sourceID)
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

		if err := a.ForkSession(1); err != nil {
			t.Fatalf("ForkSession: %v", err)
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

		if err := a.ForkSession(1); err == nil {
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
