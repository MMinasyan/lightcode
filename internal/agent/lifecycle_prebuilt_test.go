package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestLifecycleReturnsPrebuiltReplacement proves each session-changing operation
// publishes its complete-state replacement through the in-commit boundary callback
// exactly once, carrying the operation's own prebuilt committed history rather than a
// separate postcommit capture, and that a failed preparation publishes no boundary at
// all (never a detach after a commit).
func TestLifecycleReturnsPrebuiltReplacement(t *testing.T) {
	t.Run("case=new_session", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		var got []HydrationState
		id, err := a.NewSessionWithBoundary("", "primary", func(hs HydrationState) {
			got = append(got, hs)
		})
		if err != nil {
			t.Fatalf("NewSessionWithBoundary: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		if got[0].Session.ID != id {
			t.Fatalf("boundary session = %q, want %q", got[0].Session.ID, id)
		}
		if got[0].Session.State != snapshot.StateActive {
			t.Fatalf("boundary state = %q, want active", got[0].Session.State)
		}
		if len(got[0].Messages) != 0 {
			t.Fatalf("new-session boundary has %d messages, want empty", len(got[0].Messages))
		}
	})

	t.Run("case=live_selection", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		turn := appendUserTurn(t, a, "hello")
		// Commit the turn in the coordinator so the bounded live-selection
		// boundary covers it: a marked-but-uncommitted turn stays below the
		// committed bound.
		feedTranscript(a.transcriptForSessionID(a.SessionCurrent().ID), Event{Kind: EventTurnEnd, SessionID: a.SessionCurrent().ID, Turn: turn})
		id := a.SessionCurrent().ID
		var got []HydrationState
		if _, err := a.OpenSessionWithBoundary(id, func(hs HydrationState) {
			got = append(got, hs)
		}); err != nil {
			t.Fatalf("OpenSessionWithBoundary: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		if c := userContents(got[0].Messages); !equalStrings(c, []string{"hello"}) {
			t.Fatalf("live-selection boundary messages = %q, want [hello]", c)
		}
	})

	t.Run("case=reactivation", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "kept")
		archivedID := a.SessionCurrent().ID
		// Switch away so the session archives non-current, then archive it; reopening
		// it takes the reactivation path.
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if err := a.SessionArchive(archivedID); err != nil {
			t.Fatalf("SessionArchive: %v", err)
		}
		var got []HydrationState
		if _, err := a.OpenSessionWithBoundary(archivedID, func(hs HydrationState) {
			got = append(got, hs)
		}); err != nil {
			t.Fatalf("OpenSessionWithBoundary reactivate: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		if got[0].Session.State != snapshot.StateActive {
			t.Fatalf("reactivated boundary state = %q, want active", got[0].Session.State)
		}
		if c := userContents(got[0].Messages); !equalStrings(c, []string{"kept"}) {
			t.Fatalf("reactivation boundary messages = %q, want [kept]", c)
		}
	})

	t.Run("case=revert_typed_outcome", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "first")
		clicked := appendUserTurn(t, a, "second")
		appendUserTurn(t, a, "third")
		id := a.SessionCurrent().ID
		var got []HydrationState
		if _, err := a.ApplyTurnActionForSessionWithBoundary(id, clicked, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string) {
			got = append(got, hs)
		}); err != nil {
			t.Fatalf("ApplyTurnActionForSessionWithBoundary revert: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		// RevertHistory targets the clicked turn minus one, so only "first" survives.
		if c := userContents(got[0].Messages); !equalStrings(c, []string{"first"}) {
			t.Fatalf("revert boundary messages = %q, want [first]", c)
		}
	})

	t.Run("case=fork", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "first")
		clicked := appendUserTurn(t, a, "fork point")
		appendUserTurn(t, a, "after")
		id := a.SessionCurrent().ID
		var got []HydrationState
		result, err := a.ApplyTurnActionForSessionWithBoundary(id, clicked, TurnActionFork, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string) {
			got = append(got, hs)
		})
		if err != nil {
			t.Fatalf("ApplyTurnActionForSessionWithBoundary fork: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		if got[0].Session.ID != result.Session.ID || got[0].Session.ID == id {
			t.Fatalf("fork boundary session = %q, result = %q, source = %q", got[0].Session.ID, result.Session.ID, id)
		}
		if c := userContents(got[0].Messages); !equalStrings(c, []string{"first", "fork point"}) {
			t.Fatalf("fork boundary messages = %q, want [first, fork point]", c)
		}
	})

	// Current-session removal (archive/delete) commits durable removal first and then performs
	// a deterministic detach with no complete-state capture: there is no boundary-
	// capturing variant of SessionArchive/SessionDelete, and removing the current
	// session clears selection with no automatic fallback replacement.
	t.Run("case=removal_no_capture", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "kept")
		id := a.SessionCurrent().ID
		if err := a.SessionArchive(id); err != nil {
			t.Fatalf("SessionArchive: %v", err)
		}
		if cur := a.SessionCurrent().ID; cur != "" {
			t.Fatalf("current = %q after archiving the only session; current-session removal selects no session and never falls back", cur)
		}
	})

	// A failed activation must not publish a boundary. The old
	// postcommit capture emitted a zero-state detach when its fallible read failed;
	// the prebuilt path fails preparation before any commit, so nothing is published.
	t.Run("preparation/read_failure_publishes_no_boundary", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		var got []HydrationState
		_, err := a.OpenSessionWithBoundary("does-not-exist", func(hs HydrationState) {
			got = append(got, hs)
		})
		if err == nil {
			t.Fatal("OpenSessionWithBoundary on an unknown session must return an error")
		}
		if len(got) != 0 {
			t.Fatalf("failed activation published %d boundaries, want 0 (no detach after a failed prep)", len(got))
		}
	})

	// The fork's boundary is prebuilt before the durable rename: with the
	// published candidate's history unreadable at the post-rename cleanup
	// seam, the fork still publishes the replacement rendered from the staged
	// copy — no fallible read runs after the rename, so the boundary cannot be
	// a postcommit capture.
	t.Run("case=fork_prebuilt_survives_candidate_history_loss", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "first")
		clicked := appendUserTurn(t, a, "fork point")
		appendUserTurn(t, a, "after")
		id := a.SessionCurrent().ID
		sessionsRoot := a.session.store.Root()

		// Hold the fork at the post-rename cleanup seam, then make the
		// published candidate's history unreadable.
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

		var got []HydrationState
		forkDone := make(chan error, 1)
		go func() {
			_, err := a.ApplyTurnActionForSessionWithBoundary(id, clicked, TurnActionFork, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string) {
				got = append(got, hs)
			})
			forkDone <- err
		}()
		select {
		case <-seamFired:
		case <-time.After(10 * time.Second):
			t.Fatal("post-rename cleanup seam never fired")
		}
		// Atomically relocate the published candidate's history away at the
		// barrier: the cached candidate-store paths point at the missing
		// originals, so any post-publication durable read fails with ENOENT
		// for every UID (no permissions involved, root and non-root alike).
		entries, err := os.ReadDir(sessionsRoot)
		if err != nil {
			t.Fatal(err)
		}
		var candidateID string
		for _, e := range entries {
			if e.IsDir() && e.Name() != id {
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
		// Restore the candidate history immediately after the fork returns,
		// before any later candidate use.
		restoreCandidateHistory()
		if len(got) != 1 {
			t.Fatalf("emit called %d times, want exactly 1", len(got))
		}
		if got[0].Session.ID != candidateID {
			t.Fatalf("boundary session = %q, want the candidate %q", got[0].Session.ID, candidateID)
		}
		if c := userContents(got[0].Messages); !equalStrings(c, []string{"first", "fork point"}) {
			t.Fatalf("fork boundary messages = %q, want the prebuilt [first, fork point]", c)
		}
	})
}
