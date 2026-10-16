package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/pathutil"
)

// TestCodeStoreArtifactOnlyGroupReopenAndRestore proves that an isolated
// target code group can be captured, reopened, listed and restored through
// CodeStore alone, with no conversation state, Project record, claim,
// session metadata, transcript, tokens or compaction file involved.
func TestCodeStoreArtifactOnlyGroupReopenAndRestore(t *testing.T) {
	groupDir := t.TempDir()
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "notes.txt")
	original := []byte("original\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	code, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("OpenCodeStore: %v", err)
	}
	// Construction creates no directories.
	if _, err := os.Stat(filepath.Join(groupDir, "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("construction created snapshots dir: stat err = %v, want not exist", err)
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	entryID, created, err := code.SnapshotResolvedEntry(1, abs, abs)
	if err != nil {
		t.Fatalf("SnapshotResolvedEntry: %v", err)
	}
	if !created {
		t.Fatalf("SnapshotResolvedEntry created = false, want true")
	}
	code.RetainSnapshotEntry(1, entryID)

	mutated := []byte("mutated\n")
	if err := os.WriteFile(target, mutated, 0o644); err != nil {
		t.Fatalf("mutate target: %v", err)
	}
	if err := code.RecordSnapshotContent(1, entryID, mutated); err != nil {
		t.Fatalf("RecordSnapshotContent: %v", err)
	}

	// The group directory holds only the snapshot artifact tree: no
	// conversation, Project, claim, metadata or transcript files.
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		t.Fatalf("read group dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshots" {
		t.Fatalf("group dir entries = %v, want only snapshots", entries)
	}

	turns, err := code.ListTurns()
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 1 || turns[0].Turn != 1 || len(turns[0].Files) != 1 {
		t.Fatalf("ListTurns = %+v, want one turn with one file", turns)
	}
	if turns[0].Files[0].LastWrite == nil || turns[0].Files[0].LastWrite.Hash != hashBytes(mutated) {
		t.Fatalf("last-write identity = %+v, want hash of mutated content", turns[0].Files[0].LastWrite)
	}

	// Reopen the same group with a fresh handle: listing and restore must
	// work with no conversation/Project state at all.
	reopened, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("reopen OpenCodeStore: %v", err)
	}
	reopenedTurns, err := reopened.ListTurns()
	if err != nil {
		t.Fatalf("reopened ListTurns: %v", err)
	}
	if len(reopenedTurns) != 1 || len(reopenedTurns[0].Files) != 1 {
		t.Fatalf("reopened ListTurns = %+v, want one turn with one file", reopenedTurns)
	}

	result, err := reopened.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode: %v", err)
	}
	if len(result.Restored) != 1 || result.Restored[0] != abs {
		t.Fatalf("RevertCode restored = %v, want [%s]", result.Restored, abs)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("RevertCode skipped = %+v, want none", result.Skipped)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read restored target: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("restored content = %q, want %q", got, original)
	}
	after, err := os.ReadDir(filepath.Join(groupDir, "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir after restore: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("snapshots dir after restore = %v, want empty", after)
	}
}

// TestCodeStoreTwoGroupsRestoreIndependently proves the one-disk-group-per-
// Operation ownership boundary: two groups under one session's code dir each
// hold their own captured preimage, restoring one group through a fresh
// OpenCodeStore handle restores only its own preimage and leaves the other
// group's snapshot tree untouched, and the second group restores
// independently afterward.
func TestCodeStoreTwoGroupsRestoreIndependently(t *testing.T) {
	sessionCodeDir := t.TempDir()
	groupADir := filepath.Join(sessionCodeDir, "entry-a")
	groupBDir := filepath.Join(sessionCodeDir, "entry-b")
	projectDir := t.TempDir()
	fileA := filepath.Join(projectDir, "a.txt")
	fileB := filepath.Join(projectDir, "b.txt")
	originalA, originalB := []byte("a original\n"), []byte("b original\n")
	if err := os.WriteFile(fileA, originalA, 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(fileB, originalB, 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	// Each operation's group captures its own target's preimage; each
	// mutation records its post-write identity.
	capture := func(groupDir, target string, mutated []byte) {
		t.Helper()
		code, err := OpenCodeStore(groupDir)
		if err != nil {
			t.Fatalf("OpenCodeStore(%s): %v", groupDir, err)
		}
		entryID, created, err := code.SnapshotResolvedEntry(1, target, target)
		if err != nil || !created {
			t.Fatalf("SnapshotResolvedEntry(%s) = (%v, %v), want one created entry", groupDir, entryID, err)
		}
		code.RetainSnapshotEntry(1, entryID)
		if err := os.WriteFile(target, mutated, 0o644); err != nil {
			t.Fatalf("mutate %s: %v", target, err)
		}
		if err := code.RecordSnapshotContent(1, entryID, mutated); err != nil {
			t.Fatalf("RecordSnapshotContent(%s): %v", groupDir, err)
		}
	}
	mutatedA, mutatedB := []byte("a mutated\n"), []byte("b mutated\n")
	capture(groupADir, fileA, mutatedA)
	capture(groupBDir, fileB, mutatedB)

	// Restoring group A through a fresh handle: only A's preimage returns,
	// B's file stays mutated and B's snapshot tree stays intact.
	restoreA, err := OpenCodeStore(groupADir)
	if err != nil {
		t.Fatalf("reopen group A: %v", err)
	}
	resultA, err := restoreA.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode(A): %v", err)
	}
	if len(resultA.Restored) != 1 || resultA.Restored[0] != fileA || len(resultA.Skipped) != 0 {
		t.Fatalf("RevertCode(A) = %+v, want exactly %s restored", resultA, fileA)
	}
	gotA, err := os.ReadFile(fileA)
	if err != nil || string(gotA) != string(originalA) {
		t.Fatalf("a.txt after restore A = (%q, %v), want the original bytes", gotA, err)
	}
	gotB, err := os.ReadFile(fileB)
	if err != nil || string(gotB) != string(mutatedB) {
		t.Fatalf("b.txt after restore A = (%q, %v), want it untouched at the mutated bytes", gotB, err)
	}
	turnsB, err := mustOpenCodeStore(t, groupBDir).ListTurns()
	if err != nil || len(turnsB) != 1 || len(turnsB[0].Files) != 1 {
		t.Fatalf("group B turns after restore A = %+v err %v, want the snapshot tree intact", turnsB, err)
	}
	turnsA, err := mustOpenCodeStore(t, groupADir).ListTurns()
	if err != nil || len(turnsA) != 0 {
		t.Fatalf("group A turns after restore A = %+v err %v, want the restored turn dirs removed", turnsA, err)
	}

	// Restoring group B independently restores its own preimage.
	restoreB, err := OpenCodeStore(groupBDir)
	if err != nil {
		t.Fatalf("reopen group B: %v", err)
	}
	resultB, err := restoreB.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode(B): %v", err)
	}
	if len(resultB.Restored) != 1 || resultB.Restored[0] != fileB || len(resultB.Skipped) != 0 {
		t.Fatalf("RevertCode(B) = %+v, want exactly %s restored", resultB, fileB)
	}
	gotB, err = os.ReadFile(fileB)
	if err != nil || string(gotB) != string(originalB) {
		t.Fatalf("b.txt after restore B = (%q, %v), want the original bytes", gotB, err)
	}
}

// mustOpenCodeStore opens one code group, failing the test on error.
func mustOpenCodeStore(t *testing.T, groupDir string) *CodeStore {
	t.Helper()
	code, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("OpenCodeStore(%s): %v", groupDir, err)
	}
	return code
}

// --- rows migrated to the artifact owner from the removed conversation
// store's mixed tests: capture, legacy restore proof, hardlink, skip/identity
// and partial-effect guarantees keep their real preimage, identity and
// metadata fixtures with explicit group/turn identity. ---

// newTestCodeGroup opens one artifact-only code group at an explicit test
// directory. Turn identity is named by each test directly; the removed
// store's turn allocation and session bookkeeping play no part.
func newTestCodeGroup(t *testing.T) *CodeStore {
	t.Helper()
	return mustOpenCodeStore(t, filepath.Join(t.TempDir(), "group"))
}

func snapshotEntry(t *testing.T, store *CodeStore, turn int, originalPath, canonicalPath string) string {
	t.Helper()
	entryID, _, err := store.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	return entryID
}

func recordSnapshotContent(t *testing.T, store *CodeStore, turn int, entryID string, content string) {
	t.Helper()
	if err := store.RecordSnapshotContent(turn, entryID, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func TestCodeStoreRejectsInvalidTurn(t *testing.T) {
	store := newTestCodeGroup(t)
	path := filepath.Join(t.TempDir(), "file.txt")
	if _, _, err := store.SnapshotResolvedEntry(0, path, path); err == nil {
		t.Fatal("SnapshotResolvedEntry(0) error = nil, want error")
	}
}

func TestSnapshotMetaJSONRoundTrip(t *testing.T) {
	want := SnapshotMeta{OriginalPath: "/project/link.go", CanonicalPath: "/real/file.go", Existed: true}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SnapshotMeta
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip snapshot meta = %+v, want %+v", got, want)
	}
}

func TestSnapshotMetaBackwardsCompatibleWithoutCanonicalPath(t *testing.T) {
	var got SnapshotMeta
	if err := json.Unmarshal([]byte(`{"original_path":"/project/file.go","existed":true}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.OriginalPath != "/project/file.go" || got.CanonicalPath != "" || !got.Existed {
		t.Fatalf("old snapshot meta decode = %+v, want original path with empty canonical path", got)
	}
}

func TestSnapshotDeduplicatesSymlinkAndRealPath(t *testing.T) {
	for _, symlinkFirst := range []bool{true, false} {
		name := "real-first"
		if symlinkFirst {
			name = "symlink-first"
		}
		t.Run(name, func(t *testing.T) {
			store := newTestCodeGroup(t)
			projectDir := t.TempDir()
			realPath := filepath.Join(projectDir, "file.txt")
			linkPath := filepath.Join(projectDir, "link.txt")
			if err := os.WriteFile(realPath, []byte("v1"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realPath, linkPath); err != nil {
				t.Fatal(err)
			}

			const turn = 1
			first, second := realPath, linkPath
			if symlinkFirst {
				first, second = linkPath, realPath
			}
			entryID := snapshotEntry(t, store, turn, first, realPath)
			// The second capture resolves its requested path to a canonical
			// target the way the tool layer binds one before capture.
			resolved, err := pathutil.ResolveFilePath(second)
			if err != nil {
				t.Fatal(err)
			}
			snapshotEntry(t, store, turn, second, resolved.CanonicalPath)

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
			recordSnapshotContent(t, store, turn, entryID, "v2")
			affected, err := store.RevertCode(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(affected.Restored) != 1 {
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
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	realPath := filepath.Join(projectDir, "file.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(realPath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	entryID := snapshotEntry(t, store, turn, linkPath, realPath)

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
	recordSnapshotContent(t, store, turn, entryID, "v2")
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{linkPath}) {
		t.Fatalf("affected = %v, want original requested path %q", affected, linkPath)
	}
	if got, err := os.ReadFile(realPath); err != nil || string(got) != "v1" {
		t.Fatalf("real file after revert = %q, %v; want v1", got, err)
	}
}

func TestSnapshotResolvedEntryReportsCreatedOnlyForNewEntry(t *testing.T) {
	store := newTestCodeGroup(t)
	const turn = 1
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	entryID, created, err := store.SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatal(err)
	}
	if entryID == "" || !created {
		t.Fatalf("first SnapshotResolvedEntry = (%q, %v), want non-empty created entry", entryID, created)
	}
	entryID2, created, err := store.SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatal(err)
	}
	if entryID2 != entryID || created {
		t.Fatalf("second SnapshotResolvedEntry = (%q, %v), want same existing entry %q", entryID2, created, entryID)
	}
	if err := store.DiscardSnapshotEntry(turn, entryID); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("snapshot entries after one discard = %d, want one retained by second user", len(entries))
	}
	if err := store.DiscardSnapshotEntry(turn, entryID2); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("snapshot entries after discard = %d, want none", len(entries))
	}
}

func TestSnapshotResolvedEntryRetainPreventsConcurrentDiscard(t *testing.T) {
	store := newTestCodeGroup(t)
	const turn = 1
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	entryID, created, err := store.SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first SnapshotResolvedEntry was not created")
	}
	entryID2, created, err := store.SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatal(err)
	}
	if entryID2 != entryID || created {
		t.Fatalf("second SnapshotResolvedEntry = (%q, %v), want same existing entry %q", entryID2, created, entryID)
	}
	store.RetainSnapshotEntry(turn, entryID2)
	if err := store.DiscardSnapshotEntry(turn, entryID); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("snapshot entries after retained discard = %d, want one", len(entries))
	}
}

func TestSnapshotResolvedRetainsEntryAgainstLaterDiscard(t *testing.T) {
	store := newTestCodeGroup(t)
	const turn = 1
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	entryID := snapshotEntry(t, store, turn, path, path)
	store.RetainSnapshotEntry(turn, entryID)
	if err := store.DiscardSnapshotEntry(turn, hashString(path)); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("snapshot entries after discard of retained snapshot = %d, want one", len(entries))
	}
}

func TestSnapshotFirstWriteWinsAndNewFileDeletesOnRevert(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	existing := filepath.Join(projectDir, "existing.txt")
	created := filepath.Join(projectDir, "created.txt")
	if err := os.WriteFile(existing, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	const turn = 1
	existingEntry := snapshotEntry(t, store, turn, existing, existing)
	if err := os.WriteFile(existing, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, existingEntry, "v2")
	// The second capture of the same target keeps the first preimage.
	snapshotEntry(t, store, turn, existing, existing)
	createdEntry := snapshotEntry(t, store, turn, created, created)
	if err := os.WriteFile(created, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, createdEntry, "new")

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

func TestRevertSkipsCanonicalPathReplacedBySymlinkAndContinues(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	repointedTarget := filepath.Join(projectDir, "repointed.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	ownedPath := filepath.Join(projectDir, "owned.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repointedTarget, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownedPath, []byte("owned-before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	linkEntry := snapshotEntry(t, store, turn, linkPath, originalTarget)
	ownedEntry := snapshotEntry(t, store, turn, ownedPath, ownedPath)
	if err := os.WriteFile(originalTarget, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, linkEntry, "after")
	if err := os.WriteFile(ownedPath, []byte("owned-after"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, ownedEntry, "owned-after")
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repointedTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatalf("RevertCode error = %v, want skipped symlink target", err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{ownedPath}) {
		t.Fatalf("restored = %v, want [%s]", affected.Restored, ownedPath)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != linkPath {
		t.Fatalf("skipped = %+v, want %s", affected.Skipped, linkPath)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "after" {
		t.Fatalf("original target = %q, %v; want after", got, err)
	}
	if got, err := os.ReadFile(repointedTarget); err != nil || string(got) != "other" {
		t.Fatalf("repointed target = %q, %v; want other", got, err)
	}
	if got, err := os.ReadFile(ownedPath); err != nil || string(got) != "owned-before" {
		t.Fatalf("owned target = %q, %v; want owned-before", got, err)
	}
}

func TestRevertSkipsExistingFileReplacedByDirectoryAndContinues(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	dirPath := filepath.Join(projectDir, "target.txt")
	ownedPath := filepath.Join(projectDir, "owned.txt")
	if err := os.WriteFile(dirPath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownedPath, []byte("owned-before"), 0o644); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	dirEntry := snapshotEntry(t, store, turn, dirPath, dirPath)
	ownedEntry := snapshotEntry(t, store, turn, ownedPath, ownedPath)
	if err := os.WriteFile(dirPath, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, dirEntry, "after")
	if err := os.WriteFile(ownedPath, []byte("owned-after"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, ownedEntry, "owned-after")
	if err := os.Remove(dirPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatalf("RevertCode error = %v, want skipped directory replacement", err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{ownedPath}) {
		t.Fatalf("restored = %v, want [%s]", affected.Restored, ownedPath)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != dirPath {
		t.Fatalf("skipped = %+v, want %s", affected.Skipped, dirPath)
	}
	if info, err := os.Lstat(dirPath); err != nil || !info.IsDir() {
		t.Fatalf("directory replacement = %v, %v; want preserved directory", info, err)
	}
	if got, err := os.ReadFile(ownedPath); err != nil || string(got) != "owned-before" {
		t.Fatalf("owned target = %q, %v; want owned-before", got, err)
	}
}

func TestRevertOldSnapshotMetaFallsBackToOriginalPath(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(path, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: path,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Hash: hashBytes([]byte("after"))},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRestoresMissingRegularPathWithOldHash(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(path, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: path,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Absent: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertSkipsExistingFileSnapshotWithoutRecordedLastWriteIdentity(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "target.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	snapshotEntry(t, store, turn, path, path)
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("restored = %v, want none", affected.Restored)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != path {
		t.Fatalf("skipped = %+v, want %s", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "after" {
		t.Fatalf("path content = %q, %v; want after", got, err)
	}
	if got := readIntDirs(store.snapshotsDir); !reflect.DeepEqual(got, []int{turn}) {
		t.Fatalf("snapshot dirs = %v, want [%d]", got, turn)
	}
}

func TestRevertOldSnapshotMetaRestoresCurrentSymlinkTargetWhenHashMatches(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(originalTarget, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: linkPath,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Hash: hashBytes([]byte("after"))},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originalTarget, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{linkPath}) {
		t.Fatalf("affected = %v, want [%s]", affected, linkPath)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "before" {
		t.Fatalf("restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRefusesGoneSymlinkTarget(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	if err := os.WriteFile(originalTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(originalTarget, linkPath); err != nil {
		t.Fatal(err)
	}

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
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "target missing and cannot be proven") {
		t.Fatalf("RevertCode error = %v, want missing legacy target refusal", err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
}

func TestRevertOldSnapshotMetaRefusesCurrentSymlinkPath(t *testing.T) {
	store := newTestCodeGroup(t)
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
	if len(affected.Restored) != 0 {
		t.Fatalf("affected = %v, want none on refused restore", affected)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "after" {
		t.Fatalf("original target = %q, %v; want after", got, err)
	}
	if got, err := os.ReadFile(repointedTarget); err != nil || string(got) != "other" {
		t.Fatalf("repointed target = %q, %v; want other", got, err)
	}
}

func TestRevertModernSnapshotSkipsFinalTargetSwappedToSymlink(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "target.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	entryID := snapshotEntry(t, store, turn, target, target)
	if err := os.WriteFile(target, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, entryID, "after")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatalf("RevertCode error = %v, want skipped symlink replacement", err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("restored = %v, want none", affected.Restored)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != target {
		t.Fatalf("skipped = %+v, want %s", affected, target)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Fatalf("secret target = %q, %v; want unchanged", got, err)
	}
}

func TestRevertExistedFalseSkipsSymlinkWithoutRecordedIdentity(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	snapshotEntry(t, store, turn, createdPath, createdPath)
	if err := os.Symlink(secret, createdPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("restored = %v, want none", affected.Restored)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != createdPath {
		t.Fatalf("skipped = %+v, want %s", affected, createdPath)
	}
	if _, err := os.Lstat(createdPath); err != nil {
		t.Fatalf("created symlink should be preserved, lstat err = %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Fatalf("secret target = %q, %v; want unchanged", got, err)
	}
}

func TestRevertExistedFalseRefusesRepointedParent(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalRoot := filepath.Join(projectDir, "original")
	repointedRoot := filepath.Join(projectDir, "repointed")
	linkRoot := filepath.Join(projectDir, "link")
	for _, dir := range []string{originalRoot, repointedRoot} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(originalRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	requestedPath := filepath.Join(linkRoot, "created.txt")
	originalTarget := filepath.Join(originalRoot, "created.txt")
	repointedTarget := filepath.Join(repointedRoot, "created.txt")

	const turn = 1
	entryID := snapshotEntry(t, store, turn, requestedPath, originalTarget)
	if err := os.WriteFile(originalTarget, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, turn, entryID, "original")
	if err := os.WriteFile(repointedTarget, []byte("repointed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(linkRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repointedRoot, linkRoot); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)

	if err == nil || !strings.Contains(err.Error(), "canonical path changed") {
		t.Fatalf("RevertCode error = %v, want canonical path changed error", err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("affected = %v, want none on refused delete", affected)
	}
	if got, err := os.ReadFile(originalTarget); err != nil || string(got) != "original" {
		t.Fatalf("original target = %q, %v; want preserved", got, err)
	}
	if got, err := os.ReadFile(repointedTarget); err != nil || string(got) != "repointed" {
		t.Fatalf("repointed target = %q, %v; want preserved", got, err)
	}
}

func TestRevertCodeRestoresLaterSnapshotsAndDeletesLaterSnapshotDirs(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "created.txt")

	// The empty turn-1 artifact directory is seeded explicitly: turn identity
	// is named by the test, not allocated.
	if err := os.MkdirAll(filepath.Join(store.snapshotsDir, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	entryID := snapshotEntry(t, store, 2, path, path)
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, 2, entryID, "new")

	affected, err := store.RevertCode(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected.Restored, []string{path}) {
		t.Fatalf("affected = %v, want [%s]", affected, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file should be removed after RevertCode, stat err = %v", err)
	}
	if got := readIntDirs(store.snapshotsDir); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("snapshot dirs = %v, want [1]", got)
	}
}

// RevertCode must include both restored and skipped paths from the same turn so
// callers can surface accurate partial-state reporting while revert continues.
func TestRevertCodeReportsRestoredAndSkippedStateInSameTurn(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	pathA := filepath.Join(projectDir, "fileA.txt")
	pathB := filepath.Join(projectDir, "fileB.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	const turn = 1
	entryA := snapshotEntry(t, store, turn, pathA, pathA)
	entryB := snapshotEntry(t, store, turn, pathB, pathB)
	recordSnapshotContent(t, store, turn, entryA, "before")
	recordSnapshotContent(t, store, turn, entryB, "before")

	// revertOneTurn processes entries in os.ReadDir order (lexical by entry
	// dir name = hashString(EvalSymlinks(canonicalPath))). Resolve symlinks
	// before hashing so the prediction holds when TMPDIR itself is symlinked.
	realA, err := filepath.EvalSymlinks(pathA)
	if err != nil {
		t.Fatal(err)
	}
	realB, err := filepath.EvalSymlinks(pathB)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, secondPath := pathA, pathB
	if hashString(realA) > hashString(realB) {
		firstPath, secondPath = pathB, pathA
	}
	if err := os.Remove(secondPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, secondPath); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode error = %v, want skipped symlink target", err)
	}
	containsFirst := false
	for _, p := range affected.Restored {
		if p == firstPath {
			containsFirst = true
			break
		}
	}
	if !containsFirst {
		t.Fatalf("affected = %v, want to include %q (already-restored entry before mid-turn failure)", affected, firstPath)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != secondPath {
		t.Fatalf("skipped = %+v, want %s", affected, secondPath)
	}
}

// One skipped identity blocks every older snapshot entry of the same file
// across turns, and the skipped entries' turn dirs stay recorded.
func TestRevertSkipBlocksOlderSnapshotForSameFile(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry1 := snapshotEntry(t, store, 1, path, path)
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, 1, entry1, "first")

	entry2 := snapshotEntry(t, store, 2, path, path)
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSnapshotContent(t, store, 2, entry2, "second")

	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("restored = %v, want none", affected.Restored)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != path {
		t.Fatalf("skipped = %+v, want one skipped %s", affected, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first" {
		t.Fatalf("path content = %q, %v; want first", got, err)
	}
	if got := readIntDirs(store.snapshotsDir); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("snapshot dirs = %v, want [1 2]", got)
	}
}
