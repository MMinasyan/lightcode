package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestForkIntoCopiesCompaction(t *testing.T) {
	store := newTestStore(t)
	turn := store.BeginTurn()
	if err := store.MarkTurnComplete(turn); err != nil {
		t.Fatal(err)
	}

	want := CompactionRecord{
		Summary:            "test summary",
		BoundaryTurn:       turn,
		CompactedAt:        "2026-01-02T03:04:05Z",
		SummarizerModel:    "summarizer-model",
		SummarizerProvider: "test-provider",
	}
	if err := store.SaveCompaction(want); err != nil {
		t.Fatal(err)
	}

	_, forkDir, err := store.ForkInto(turn)
	if err != nil {
		t.Fatal(err)
	}

	var got CompactionRecord
	if err := readJSON(filepath.Join(forkDir, "compaction.json"), &got); err != nil {
		t.Fatalf("forked session missing compaction.json: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compaction = %+v, want %+v", got, want)
	}
}

func TestForkIntoOmitsCompactionPastForkTurn(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 3; i++ {
		turn := store.BeginTurn()
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.SaveCompaction(CompactionRecord{
		Summary:            "test summary",
		BoundaryTurn:       3,
		CompactedAt:        "2026-01-02T03:04:05Z",
		SummarizerModel:    "summarizer-model",
		SummarizerProvider: "test-provider",
	}); err != nil {
		t.Fatal(err)
	}

	_, forkDir, err := store.ForkInto(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(forkDir, "compaction.json")); !os.IsNotExist(err) {
		t.Fatalf("forked session should not copy compaction.json past fork turn, stat err = %v", err)
	}
}

func TestRevertHistoryDeletesLaterTurnsAndUpdatesCurrentTurn(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 3; i++ {
		turn := store.BeginTurn()
		if err := store.AppendMessage(turn, []byte(`{"role":"user","content":"msg"}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.RevertHistory(1); err != nil {
		t.Fatal(err)
	}

	if got := readIntDirs(store.turnsDir); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("turn dirs = %v, want [1]", got)
	}
	if got := store.CurrentTurn(); got != 1 {
		t.Fatalf("current turn = %d, want 1", got)
	}
}

func TestRevertCodeRestoresLaterSnapshotsAndDeletesLaterSnapshotDirs(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "created.txt")

	turn1 := store.BeginTurn()
	if err := os.WriteFile(filepath.Join(projectDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnComplete(turn1); err != nil {
		t.Fatal(err)
	}

	turn2 := store.BeginTurn()
	if err := store.Snapshot(turn2, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnComplete(turn2); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file should be removed after RevertCode, stat err = %v", err)
	}
	if got := readIntDirs(store.snapshotsDir); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("snapshot dirs = %v, want [1]", got)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return store
}
