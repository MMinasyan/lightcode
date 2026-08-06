package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// seedCompleteTurns persists n complete turns, one user message each, through
// the store and the loop.
func seedCompleteTurns(t *testing.T, a *Agent, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		appendUserTurn(t, a, fmt.Sprintf("turn %d", i))
	}
}

// blockTurnDir makes one turn directory's removal fail: an unwritable
// directory blocks os.RemoveAll exactly there, so the descending history walk
// stops at it.
func blockTurnDir(t *testing.T, a *Agent, turn int) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	blocked := filepath.Join(a.store.Dir(), "turns", strconv.Itoa(turn))
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
}

// corruptCompactionRecord makes the post-walk reload fail: the loop derives
// from disk, and the first disk read is the compaction record.
func corruptCompactionRecord(t *testing.T, a *Agent) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.store.Dir(), "compaction.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loopUserContents(msgs []message.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != message.RoleUser {
			continue
		}
		var text string
		for _, p := range m.Content {
			text += p.Text
		}
		out = append(out, text)
	}
	return out
}

// TestRevertPartialWalkReloadsLoopAndPublishesBoundary covers the partial-walk
// outcome with a reload that succeeds: the walk removed turns 8-10 and stopped
// at turn 7, so the loop must be re-derived from disk (turns 1-7), the error
// must name turn 7, and the boundary must be published over the reconciled
// state — today the loop keeps all ten turns and nothing is published.
func TestRevertPartialWalkReloadsLoopAndPublishesBoundary(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	blockTurnDir(t, a, 7)

	var got []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert) {
		got = append(got, hs)
	})
	if err == nil {
		t.Fatal("partial walk reported success")
	}
	if !strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want it to name turn 7 where removal stopped", err.Error())
	}
	// The loop matches disk: turns 8-10 are gone from both.
	if c := loopUserContents(a.lp.Messages()); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7"}) {
		t.Fatalf("loop history after partial revert = %q, want turns 1-7 (the loop must equal disk)", c)
	}
	// The unit stays live: only a failed reload evicts.
	if _, err := a.SessionSummaryForSession(id); err != nil {
		t.Fatalf("unit was evicted after a reconciled partial walk: %v", err)
	}
	// The boundary is published exactly once, over the reconciled history.
	if len(got) != 1 {
		t.Fatalf("boundary emitted %d times, want exactly 1", len(got))
	}
	if c := userContents(got[0].Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7"}) {
		t.Fatalf("boundary messages = %q, want turns 1-7", c)
	}
}

// assertEvicted verifies the failed-reload outcome: the unit is out of the
// live map, its transcript is unregistered, its claim is released, the empty
// HydrationState was delivered, and no turn is admitted afterwards.
func assertEvicted(t *testing.T, a *Agent, id string, got []HydrationState) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("empty-state boundary emitted %d times, want exactly 1", len(got))
	}
	if got[0].Session.ID != "" || len(got[0].Messages) != 0 || len(got[0].Tail) != 0 || len(got[0].Errors) != 0 {
		t.Fatalf("eviction boundary = %#v, want the empty HydrationState", got[0])
	}
	if a.store.Active() {
		t.Fatal("store still active after eviction")
	}
	if a.currentSessionID != "" {
		t.Fatalf("currentSessionID = %q after eviction, want empty", a.currentSessionID)
	}
	if a.sessions[id] != nil {
		t.Fatal("evicted unit is still in the live map")
	}
	if a.transcriptForSessionID(id) != nil {
		t.Fatal("evicted unit's transcript is still registered")
	}
	if cur := a.SessionCurrent().ID; cur != "" {
		t.Fatalf("SessionCurrent = %q after eviction, want no current session", cur)
	}
	if _, err := a.resolveLiveSession(id); err == nil {
		t.Fatal("evicted id still resolves as a live session")
	}
	// No turn is admitted afterwards: the unit is gone from the live map.
	if _, err := a.ApplyTurnActionForSession(id, 2, TurnActionRevertHistory, false); err == nil {
		t.Fatal("a turn action was admitted on an evicted session")
	}
	// The claim was released: a fresh store with the same project context can
	// load the session.
	proj, err := a.projects.Current()
	if err != nil {
		t.Fatalf("current project: %v", err)
	}
	fresh, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := fresh.LoadSession(id); err != nil {
		t.Fatalf("session claim still held after eviction: %v", err)
	}
	fresh.Detach()
}

// TestRevertPartialWalkReloadFailureEvictsUnit covers the partial-walk
// outcome with a reload that fails: the unit must be evicted rather than
// released back over a loop that no longer matches disk, and the returned
// error stays the walk's.
func TestRevertPartialWalkReloadFailureEvictsUnit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	corruptCompactionRecord(t, a)
	blockTurnDir(t, a, 7)

	var got []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert) {
		got = append(got, hs)
	})
	if err == nil {
		t.Fatal("partial walk with failed reload reported success")
	}
	if !strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want the walk's error naming turn 7", err.Error())
	}
	assertEvicted(t, a, id, got)
}

// TestRevertCompleteWalkReloadFailureEvictsUnit covers the complete-walk
// outcome with a reload that fails: the same eviction, with the reload's error
// returned. It fails against any fix applied only to the walk's error branch.
func TestRevertCompleteWalkReloadFailureEvictsUnit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	corruptCompactionRecord(t, a)

	var got []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert) {
		got = append(got, hs)
	})
	if err == nil {
		t.Fatal("complete walk with failed reload reported success")
	}
	if strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want the reload error, not the walk's", err.Error())
	}
	assertEvicted(t, a, id, got)
	// The complete walk removed turns 6-10; only 1-5 survive on disk.
	if got := loadedTurnNumbers(t, a, id); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("turns on disk after eviction = %v, want [1 2 3 4 5]", got)
	}
}

func loadedTurnNumbers(t *testing.T, a *Agent, id string) []int {
	t.Helper()
	proj, err := a.projects.Current()
	if err != nil {
		t.Fatalf("current project: %v", err)
	}
	store, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := store.LoadSession(id); err != nil {
		t.Fatalf("load evicted session: %v", err)
	}
	defer store.Detach()
	turns, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatalf("load complete turns: %v", err)
	}
	var out []int
	for _, t := range turns {
		out = append(out, t.Turn)
	}
	return out
}

// TestRevertPartialWalkClearsQueue covers the queue rule on a partial walk:
// at least one turn was removed, so queued input written against the removed
// turns is cleared.
func TestRevertPartialWalkClearsQueue(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	seedQueue(t, a, 7, "stale after partial revert")
	blockTurnDir(t, a, 7)

	if _, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, 6, TurnActionRevertHistory, false); err == nil {
		t.Fatal("partial walk reported success")
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("queue after partial revert = %#v, want empty: turns were removed, so queued input no longer applies", got.Items)
	}
	if got.Version <= 7 {
		t.Fatalf("queue version = %d, want > 7", got.Version)
	}
}

// TestRevertFirstRemovalFailureKeepsQueue covers the queue rule when the walk
// removed nothing: the first removal failed, so the queued input is kept.
func TestRevertFirstRemovalFailureKeepsQueue(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	seedQueue(t, a, 7, "stale after failed first removal")
	blockTurnDir(t, a, 10)

	if _, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, 6, TurnActionRevertHistory, false); err == nil {
		t.Fatal("first-removal failure reported success")
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 1 || got.Items[0].Content != "stale after failed first removal" {
		t.Fatalf("queue after first-removal failure = %#v, want the queued item kept", got.Items)
	}
	if got.Version != 7 {
		t.Fatalf("queue version = %d, want unchanged 7", got.Version)
	}
}
