package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
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
	if len(turns) != 2 || turns[0].Turn != complete || turns[1].Turn != incomplete {
		t.Fatalf("turns = %+v, want completed turn plus recovered text turn", turns)
	}
	if string(turns[1].Messages[0]) != `{"role":"assistant","content":"not done"}` {
		t.Fatalf("recovered messages = %q, want the valid text response", turns[1].Messages)
	}
}

// TestRevertNeverReissuesTurnNumber proves a live session never reuses a turn
// number after a code rewind removes the snapshot dirs and the message tree is
// lost on top of it: with both trees below previously used numbers, allocation
// from disk alone would reissue them. The recorded high-water mark must also
// survive later rewinds whose pre-rewind union maximum is below it — a bare
// assignment would drop the mark and reissue a used number — and must die with
// the session, so a Store that moves to another session starts allocating from
// disk again.
func TestRevertNeverReissuesTurnNumber(t *testing.T) {
	store := newTestStore(t)
	maxIssued := 0
	for i := 0; i < 10; i++ {
		turn := store.BeginTurn()
		if turn > maxIssued {
			maxIssued = turn
		}
	}
	// Code rewind to turn 5 removes the snapshot dirs above it, and a lost
	// message tree on top drops both trees below every issued number.
	if _, err := store.RevertCode(5); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(store.turnsDir); err != nil {
		t.Fatal(err)
	}
	// A later rewind scans a union whose maximum (3) is below the recorded mark
	// (10); the mark must hold at 10, not drop to 3.
	if _, err := store.RevertCode(3); err != nil {
		t.Fatal(err)
	}

	next := store.BeginTurn()
	if next <= maxIssued {
		t.Fatalf("BeginTurn after rewind and message-tree loss = %d, want a number above every issued turn %d", next, maxIssued)
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

// TestListSkipsSessionWhoseMetaDeclaresAnotherID proves List takes a
// session's identity from its directory: a record that declares a different
// id is not listed under that id, and a correctly-declared session in the
// same project is still listed under its own.
func TestListSkipsSessionWhoseMetaDeclaresAnotherID(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()

	real, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := real.BeginNewSession(project); err != nil {
		t.Fatal(err)
	}
	realID := real.SessionID()

	// Plant a session directory whose meta.json declares another id.
	const dirID = "dirA"
	planted := filepath.Join(root, dirID)
	if err := os.MkdirAll(filepath.Join(planted, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := SessionMeta{ID: "dirB", ProjectPath: project, State: StateActive, LastActivity: 5}
	if err := writeJSON(filepath.Join(planted, "meta.json"), meta); err != nil {
		t.Fatal(err)
	}

	infos, err := List(root, project, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == "dirB" {
			t.Fatalf("List reported the planted session under its declared id %q", info.ID)
		}
	}
	for _, info := range infos {
		if info.ID == realID {
			return
		}
	}
	t.Fatalf("List = %+v, missing the correctly-declared session %q", infos, realID)
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

// TestSweepCloseFirstRefusesWithoutClaim proves a close-first sweep candidate
// is refused by the lifecycle serializer before any claim: a candidate whose
// serializer reports not-admitted performs no claim, metadata re-read, archive
// write, or delete, so it stays active on disk and its claim stays
// acquirable. The serializer's admitted result is the owner-close admission
// carried into the sweep.
func TestSweepCloseFirstRefusesWithoutClaim(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep-close"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "close1"
	if err := os.MkdirAll(filepath.Join(sessionsRoot, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(sessionsRoot, id, "meta.json"), SessionMeta{ID: id, State: StateActive, LastActivity: 1}); err != nil {
		t.Fatal(err)
	}

	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 3650}
	archived, deleted, err := SweepAllProjects(projectsRoot, cfg, nil, func() (func(), bool) {
		// Owner close won before this candidate was admitted.
		return nil, false
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if archived != 0 || deleted != 0 {
		t.Fatalf("sweep counts = archived:%d deleted:%d, want 0/0 for a refused candidate", archived, deleted)
	}

	meta, err := LoadSessionMeta(sessionsRoot, id)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if effectiveState(meta.State) != StateActive {
		t.Fatalf("refused candidate state = %q, want active (no archive write)", meta.State)
	}
	lock, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
	if err != nil || !ok {
		t.Fatalf("refused candidate claim: ok=%v err=%v, want acquirable (no claim taken)", ok, err)
	}
	_ = lock.Release()
}

// TestSweepCloseFirstRefusedCandidatePerformsNoMetaRead proves the refusal
// happens before the candidate's metadata is read at all: the candidate's
// meta.json is a FIFO, so any read would block forever. A close-first sweep
// must return promptly without touching it. If a pre-fix ordering read the
// meta before the serializer, the sweep blocks on the FIFO; the test then
// opens the FIFO read/write to unblock it before failing, so the pre-fix
// ordering is detected without any production test seam.
func TestSweepCloseFirstRefusedCandidatePerformsNoMetaRead(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep-fifo"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "fifo1"
	if err := os.MkdirAll(filepath.Join(sessionsRoot, id), 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(sessionsRoot, id, "meta.json")
	if err := syscall.Mkfifo(metaPath, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", metaPath, err)
	}

	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 3650}
	done := make(chan struct{})
	go func() {
		_, _, _ = SweepAllProjects(projectsRoot, cfg, nil, func() (func(), bool) {
			// Owner close won before this candidate was admitted.
			return nil, false
		})
		close(done)
	}()

	select {
	case <-done:
		// The refused candidate never opened the FIFO: no metadata read.
	case <-time.After(2 * time.Second):
		// Pre-fix ordering: the sweep blocked reading the FIFO. Open it
		// read/write so the blocked read unblocks and the sweep can finish,
		// then fail — the block itself is the detected defect.
		if w, err := os.OpenFile(metaPath, os.O_RDWR, 0); err == nil {
			w.Close()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("sweep did not finish even after the FIFO was unblocked")
		}
		t.Fatal("close-first sweep blocked reading the candidate meta: a refused candidate performed a metadata read")
	}

	// The refused candidate still took no claim: the session stays claimable.
	lock, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
	if err != nil || !ok {
		t.Fatalf("refused candidate claim: ok=%v err=%v, want acquirable (no claim taken)", ok, err)
	}
	_ = lock.Release()
}
