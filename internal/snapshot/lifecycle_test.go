package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEndOfTurnOrderingPersistsMessagesBeforeCompleteMarker(t *testing.T) {
	store := newTestStore(t)
	turn := store.BeginTurn()
	messages := []string{
		`{"role":"user","content":"request"}`,
		`{"role":"assistant","tool_calls":[{"id":"call-1"}]}`,
		`{"role":"tool","tool_call_id":"call-1","content":"tool result"}`,
		`{"role":"assistant","content":"done"}`,
	}
	for _, msg := range messages {
		mustAppendMessage(t, store, turn, msg)
	}
	if err := store.MarkTurnComplete(turn); err != nil {
		t.Fatal(err)
	}

	turns, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Turn != turn {
		t.Fatalf("turns = %+v, want one completed turn %d", turns, turn)
	}
	var got []string
	for _, msg := range turns[0].Messages {
		got = append(got, string(msg))
	}
	if !reflect.DeepEqual(got, messages) {
		t.Fatalf("message order = %v, want %v", got, messages)
	}
}

func TestEndOfTurnOrderingIncompleteTurnIsHiddenUntilComplete(t *testing.T) {
	store := newTestStore(t)
	complete := store.BeginTurn()
	mustAppendMessage(t, store, complete, `{"role":"user","content":"complete"}`)
	if err := store.MarkTurnComplete(complete); err != nil {
		t.Fatal(err)
	}
	incomplete := store.BeginTurn()
	mustAppendMessage(t, store, incomplete, `{"role":"assistant","content":"not done"}`)

	turns, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Turn != complete {
		t.Fatalf("turns = %+v, want only completed turn", turns)
	}
	if _, err := os.Stat(filepath.Join(store.turnsDir, "2")); !os.IsNotExist(err) {
		t.Fatalf("incomplete turn should be deleted, stat err = %v", err)
	}
}

func TestRevertHistoryAtomicAgainstBeginTurn(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 5; i++ {
		turn := store.BeginTurn()
		mustAppendMessage(t, store, turn, `{"role":"user","content":"msg"}`)
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 2)
	go func() { done <- store.RevertHistory(2) }()
	go func() {
		if turn := store.BeginTurn(); turn == 0 {
			done <- ErrNoSession
			return
		}
		done <- nil
	}()

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	turns := readIntDirs(store.turnsDir)
	validTurns := reflect.DeepEqual(turns, []int{1, 2}) || reflect.DeepEqual(turns, []int{1, 2, 3}) || reflect.DeepEqual(turns, []int{1, 2, 6})
	if !validTurns {
		t.Fatalf("turn dirs after concurrent revert/begin = %v, want a serialized state", turns)
	}
	if got := store.CurrentTurn(); got != turns[len(turns)-1] {
		t.Fatalf("CurrentTurn after concurrent revert/begin = %d, want last turn dir %d", got, turns[len(turns)-1])
	}
}

// TestRevertNeverReissuesTurnNumber proves a live session never reuses a turn
// number after a combined revert: RevertHistory removes the turns tree and
// RevertCode removes the snapshots tree, so the disk maximum drops and
// allocation from disk alone would reissue a number this session already used.
// The recorded high-water mark must also survive a later revert whose
// pre-revert union maximum is below it — a bare assignment would drop the mark
// and reissue a used number — and must die with the session, so a Store that
// moves to another session starts allocating from disk again.
func TestRevertNeverReissuesTurnNumber(t *testing.T) {
	store := newTestStore(t)
	maxIssued := 0
	for i := 0; i < 10; i++ {
		turn := store.BeginTurn()
		if turn > maxIssued {
			maxIssued = turn
		}
	}
	// Combined revert to turn 5: both trees drop to 5, the disk maximum falls
	// from 10 to 5.
	if err := store.RevertHistory(5); err != nil {
		t.Fatal(err)
	}
	// A deeper code revert scans a union whose maximum (8) is below the
	// recorded mark (10); the mark must hold at 10, not drop to 8.
	if _, err := store.RevertCode(8); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevertCode(5); err != nil {
		t.Fatal(err)
	}

	next := store.BeginTurn()
	if next <= maxIssued {
		t.Fatalf("BeginTurn after both reverts = %d, want a number above every issued turn %d", next, maxIssued)
	}

	// The mark is per-session: a Store that moves to another session restarts
	// allocation from disk rather than from the reverted session's high-water.
	store.Detach()
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := store.BeginTurn(); got != 1 {
		t.Fatalf("BeginTurn in a new session after a reverted one = %d, want 1", got)
	}
}

func TestListAndLoadMostRecentUseCompletedSessionMetadata(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()
	older, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := older.BeginNewSession(project); err != nil {
		t.Fatal(err)
	}
	olderID := older.SessionID()
	olderMeta, err := older.Meta()
	if err != nil {
		t.Fatal(err)
	}
	olderMeta.LastActivity = 10
	if err := writeJSON(filepath.Join(older.Dir(), "meta.json"), olderMeta); err != nil {
		t.Fatal(err)
	}

	newer, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.BeginNewSession(project); err != nil {
		t.Fatal(err)
	}
	newerID := newer.SessionID()
	newerMeta, err := newer.Meta()
	if err != nil {
		t.Fatal(err)
	}
	newerMeta.LastActivity = 20
	if err := writeJSON(filepath.Join(newer.Dir(), "meta.json"), newerMeta); err != nil {
		t.Fatal(err)
	}

	infos, err := List(root, project, StateActive)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{infos[0].ID, infos[1].ID}; !reflect.DeepEqual(got, []string{newerID, olderID}) {
		t.Fatalf("List order = %v, want newest first", got)
	}
	mostRecent, err := LoadMostRecent(root, project)
	if err != nil {
		t.Fatal(err)
	}
	if mostRecent != newerID {
		t.Fatalf("LoadMostRecent = %q, want %q", mostRecent, newerID)
	}
}

func TestLoadMostRecentSkipsChildSessions(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()
	parent, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.BeginNewSession(project); err != nil {
		t.Fatal(err)
	}
	parentID := parent.SessionID()
	parentMeta, err := parent.Meta()
	if err != nil {
		t.Fatal(err)
	}
	parentMeta.LastActivity = 10
	if err := writeJSON(filepath.Join(parent.Dir(), "meta.json"), parentMeta); err != nil {
		t.Fatal(err)
	}

	child, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.BeginChildSession(project, parentID); err != nil {
		t.Fatal(err)
	}
	childMeta, err := child.Meta()
	if err != nil {
		t.Fatal(err)
	}
	childMeta.LastActivity = 20
	if err := writeJSON(filepath.Join(child.Dir(), "meta.json"), childMeta); err != nil {
		t.Fatal(err)
	}

	mostRecent, err := LoadMostRecent(root, project)
	if err != nil {
		t.Fatal(err)
	}
	if mostRecent != parentID {
		t.Fatalf("LoadMostRecent = %q, want parent %q", mostRecent, parentID)
	}
}
