package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestSnapshotDeduplicatesSymlinkAndRealPath(t *testing.T) {
	for _, symlinkFirst := range []bool{true, false} {
		name := "real-first"
		if symlinkFirst {
			name = "symlink-first"
		}
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			projectDir := t.TempDir()
			realPath := filepath.Join(projectDir, "file.txt")
			linkPath := filepath.Join(projectDir, "link.txt")
			if err := os.WriteFile(realPath, []byte("v1"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realPath, linkPath); err != nil {
				t.Fatal(err)
			}

			turn := store.BeginTurn()
			first, second := realPath, linkPath
			if symlinkFirst {
				first, second = linkPath, realPath
			}
			if err := store.Snapshot(turn, first); err != nil {
				t.Fatal(err)
			}
			if err := store.Snapshot(turn, second); err != nil {
				t.Fatal(err)
			}

			entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("snapshot entries = %d, want 1", len(entries))
			}

			if err := os.WriteFile(realPath, []byte("v2"), 0o644); err != nil {
				t.Fatal(err)
			}
			affected, err := store.RevertCode(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(affected) != 1 {
				t.Fatalf("affected = %v, want one path", affected)
			}
			got, err := os.ReadFile(realPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "v1" {
				t.Fatalf("content = %q, want v1", got)
			}
		})
	}
}

func TestSnapshotResolvedPreservesOriginalPathForListing(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	realPath := filepath.Join(projectDir, "file.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(realPath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.SnapshotResolved(turn, linkPath, realPath); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListTurns()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Files) != 1 {
		t.Fatalf("ListTurns = %+v, want one snapshotted file", entries)
	}
	if got := entries[0].Files[0].OriginalPath; got != linkPath {
		t.Fatalf("OriginalPath = %q, want requested path %q", got, linkPath)
	}
	if err := os.WriteFile(realPath, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{linkPath}) {
		t.Fatalf("affected = %v, want original requested path %q", affected, linkPath)
	}
	if got, err := os.ReadFile(realPath); err != nil || string(got) != "v1" {
		t.Fatalf("real file after revert = %q, %v; want v1", got, err)
	}
}

func TestRevertRefusesCanonicalPathReplacedBySymlink(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	repointedTarget := filepath.Join(projectDir, "repointed.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repointedTarget, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.SnapshotResolved(turn, linkPath, originalTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originalTarget, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repointedTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "canonical path changed") {
		t.Fatalf("RevertCode error = %v, want canonical path changed error", err)
	}
	if len(affected) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "after" {
		t.Fatalf("original target = %q, %v; want after", got, err)
	}
	if got, err := os.ReadFile(repointedTarget); err != nil || string(got) != "other" {
		t.Fatalf("repointed target = %q, %v; want other", got, err)
	}
}

func TestRevertOldSnapshotMetaFallsBackToOriginalPath(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(path, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: path, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRestoresMissingRegularPathWithOldHash(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(path, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: path, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRestoresCurrentSymlinkTargetWhenHashMatches(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(originalTarget, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: linkPath, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
	if err := os.WriteFile(originalTarget, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{linkPath}) {
		t.Fatalf("affected = %v, want [%s]", affected, linkPath)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRefusesGoneSymlinkTarget(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(originalTarget, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: linkPath, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "target missing and cannot be proven") {
		t.Fatalf("RevertCode error = %v, want missing legacy target refusal", err)
	}
	if len(affected) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
}

func TestRevertOldSnapshotMetaRefusesCurrentSymlinkPath(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	repointedTarget := filepath.Join(projectDir, "repointed.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repointedTarget, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(originalTarget, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: linkPath, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
	if err := os.WriteFile(originalTarget, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repointedTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "legacy snapshot target hash changed") {
		t.Fatalf("RevertCode error = %v, want legacy hash-change refusal", err)
	}
	if len(affected) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "after" {
		t.Fatalf("original target = %q, %v; want after", got, err)
	}
	if got, err := os.ReadFile(repointedTarget); err != nil || string(got) != "other" {
		t.Fatalf("repointed target = %q, %v; want other", got, err)
	}
}

func TestRevertModernSnapshotRefusesFinalTargetSwappedToSymlink(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "target.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.SnapshotResolved(turn, target, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "canonical path changed") {
		t.Fatalf("RevertCode error = %v, want canonical path changed error", err)
	}
	if len(affected) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Fatalf("secret target = %q, %v; want unchanged", got, err)
	}
}

func TestRevertExistedFalseRemovesSymlinkLeafOnly(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.Snapshot(turn, createdPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, createdPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{createdPath}) {
		t.Fatalf("affected = %v, want [%s]", affected, createdPath)
	}
	if _, err := os.Lstat(createdPath); !os.IsNotExist(err) {
		t.Fatalf("created symlink should be removed, lstat err = %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Fatalf("secret target = %q, %v; want unchanged", got, err)
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

func TestBeginTurnCreatesAndIncrementsTurnDirs(t *testing.T) {
	store := newTestStore(t)

	first := store.BeginTurn()
	second := store.BeginTurn()

	if first != 1 || second != 2 {
		t.Fatalf("turns = %d, %d; want 1, 2", first, second)
	}
	for _, dir := range []string{
		filepath.Join(store.snapshotsDir, "1"),
		filepath.Join(store.turnsDir, "1"),
		filepath.Join(store.snapshotsDir, "2"),
		filepath.Join(store.turnsDir, "2"),
	} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("turn dir %s stat = %v, isDir = %v; want existing dir", dir, err, err == nil && info.IsDir())
		}
	}
	if got := store.CurrentTurn(); got != 2 {
		t.Fatalf("CurrentTurn = %d, want 2", got)
	}
}

func TestBeginTurnReturnsZeroWithoutSession(t *testing.T) {
	store, err := NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.BeginTurn(); got != 0 {
		t.Fatalf("BeginTurn without session = %d, want 0", got)
	}
}

func TestMarkTurnCompleteCreatesMarkerAndRejectsInvalidState(t *testing.T) {
	store := newTestStore(t)
	turn := store.BeginTurn()

	if err := store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.turnsDir, "1", "complete")); err != nil {
		t.Fatalf("complete marker missing: %v", err)
	}
	if err := store.MarkTurnComplete(0); err == nil {
		t.Fatalf("MarkTurnComplete(0) error = nil, want error")
	}
	if _, err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnComplete(turn); !errors.Is(err, ErrNoSession) {
		t.Fatalf("MarkTurnComplete without session = %v, want ErrNoSession", err)
	}
}

func TestLoadCompleteTurnsReturnsMessagesInOrderAndDeletesIncomplete(t *testing.T) {
	store := newTestStore(t)
	turn1 := store.BeginTurn()
	mustAppendMessage(t, store, turn1, `{"role":"user","content":"one"}`)
	mustAppendMessage(t, store, turn1, `{"role":"assistant","content":"two"}`)
	if err := store.MarkTurnComplete(turn1); err != nil {
		t.Fatal(err)
	}
	turn2 := store.BeginTurn()
	mustAppendMessage(t, store, turn2, `{"role":"user","content":"incomplete"}`)

	turns, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Turn != 1 || len(turns[0].Messages) != 2 {
		t.Fatalf("turns = %+v, want one complete turn with two messages", turns)
	}
	if string(turns[0].Messages[0]) != `{"role":"user","content":"one"}` || string(turns[0].Messages[1]) != `{"role":"assistant","content":"two"}` {
		t.Fatalf("messages = %q, want append order", turns[0].Messages)
	}
	if _, err := os.Stat(filepath.Join(store.turnsDir, "2")); !os.IsNotExist(err) {
		t.Fatalf("incomplete turn should be deleted, stat err = %v", err)
	}
}

func TestLoadCompleteTurnsWithoutSessionReturnsErrNoSession(t *testing.T) {
	store, err := NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCompleteTurns(); !errors.Is(err, ErrNoSession) {
		t.Fatalf("LoadCompleteTurns without session = %v, want ErrNoSession", err)
	}
}

func TestForkIntoCopiesTurnsUpToTargetAndCanBeLoaded(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetModel("openrouter", "test/model"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		turn := store.BeginTurn()
		mustAppendMessage(t, store, turn, `{"role":"user","content":"turn"}`)
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}

	newID, newDir, err := store.ForkInto(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := readIntDirs(filepath.Join(newDir, "turns")); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("forked turns = %v, want [1 2]", got)
	}
	if got := readIntDirs(filepath.Join(newDir, "snapshots")); len(got) > 2 {
		t.Fatalf("forked snapshots = %v, want no turns after 2", got)
	}
	forked, err := NewForSessionsRoot(store.Root(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := forked.LoadSession(newID); err != nil {
		t.Fatal(err)
	}
	if got := forked.CurrentTurn(); got != 2 {
		t.Fatalf("forked CurrentTurn = %d, want 2", got)
	}
	meta, err := forked.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "openrouter" || meta.Model != "test/model" {
		t.Fatalf("forked model = %q/%q, want openrouter/test/model", meta.Provider, meta.Model)
	}
}

func TestSnapshotFirstWriteWinsAndNewFileDeletesOnRevert(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	existing := filepath.Join(projectDir, "existing.txt")
	created := filepath.Join(projectDir, "created.txt")
	if err := os.WriteFile(existing, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn := store.BeginTurn()
	if err := store.Snapshot(turn, existing); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(turn, existing); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(turn, created); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RevertCode(0); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "v1" {
		t.Fatalf("existing content = %q, err = %v; want v1", got, err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created file should be removed, stat err = %v", err)
	}
}

func TestSnapshotRejectsInvalidTurnAndNoSession(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := store.Snapshot(0, path); err == nil {
		t.Fatalf("Snapshot(0) error = nil, want error")
	}
	if _, err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(1, path); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Snapshot without session = %v, want ErrNoSession", err)
	}
}

func TestRevertHistoryNegativeTurnClearsHistory(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 2; i++ {
		turn := store.BeginTurn()
		mustAppendMessage(t, store, turn, `{"role":"user","content":"msg"}`)
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RevertHistory(-1); err != nil {
		t.Fatal(err)
	}
	if got := readIntDirs(store.turnsDir); len(got) != 0 {
		t.Fatalf("turn dirs after RevertHistory(-1) = %v, want none", got)
	}
	if got := store.CurrentTurn(); got != 0 {
		t.Fatalf("CurrentTurn = %d, want 0", got)
	}
}

func TestRevertCodeWithoutSessionReturnsErrNoSession(t *testing.T) {
	store, err := NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevertCode(0); !errors.Is(err, ErrNoSession) {
		t.Fatalf("RevertCode without session = %v, want ErrNoSession", err)
	}
}

func TestCloseDiscardsEmptySessionAndKeepsCompleteSession(t *testing.T) {
	empty := newTestStore(t)
	emptyDir := empty.Dir()
	discarded, err := empty.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !discarded {
		t.Fatalf("Close empty discarded = false, want true")
	}
	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Fatalf("empty session dir should be removed, stat err = %v", err)
	}

	kept := newTestStore(t)
	keptDir := kept.Dir()
	turn := kept.BeginTurn()
	mustAppendMessage(t, kept, turn, `{"role":"user","content":"msg"}`)
	if err := kept.MarkTurnComplete(turn); err != nil {
		t.Fatal(err)
	}
	discarded, err = kept.Close()
	if err != nil {
		t.Fatal(err)
	}
	if discarded {
		t.Fatalf("Close complete session discarded = true, want false")
	}
	if info, err := os.Stat(keptDir); err != nil || !info.IsDir() {
		t.Fatalf("complete session dir stat = %v, isDir = %v; want kept", err, err == nil && info.IsDir())
	}
	discarded, err = kept.Close()
	if err != nil || discarded {
		t.Fatalf("second Close = (%v, %v), want no-op false nil", discarded, err)
	}
}

func TestSaveAndLoadCompactionRoundTripAndAbsent(t *testing.T) {
	store := newTestStore(t)
	got, err := store.LoadCompaction()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("LoadCompaction absent = %+v, want nil", got)
	}
	want := CompactionRecord{Summary: "summary", BoundaryTurn: 1, CompactedAt: "2026-01-02T03:04:05Z", SummarizerModel: "model", SummarizerProvider: "provider"}
	if err := store.SaveCompaction(want); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadCompaction()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("LoadCompaction = %+v, want %+v", got, want)
	}
}

func mustAppendMessage(t *testing.T, store *Store, turn int, msg string) {
	t.Helper()
	if err := store.AppendMessage(turn, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}
