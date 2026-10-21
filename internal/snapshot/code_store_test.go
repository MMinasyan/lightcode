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
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, os.WriteFile(target, original, 0o644))

	code, err := OpenCodeStore(groupDir)
	testassert.NoError(t, err)
	// Construction creates no directories.
	{
		_, err := os.Stat(filepath.Join(groupDir, "snapshots"))
		testassert.Require(t, os.IsNotExist(err), "construction created snapshots dir: stat err = %v, want not exist", err)
	}

	abs, err := filepath.Abs(target)
	testassert.NoError(t, err)
	entryID, created, err := code.SnapshotResolvedEntry(1, abs, abs)
	testassert.NoError(t, err)
	testassert.Require(t, created, "SnapshotResolvedEntry created = false, want true")
	code.RetainSnapshotEntry(1, entryID)

	mutated := []byte("mutated\n")
	testassert.NoError(t, os.WriteFile(target, mutated, 0o644))
	testassert.NoError(t, code.RecordSnapshotContent(1, entryID, mutated))

	// The group directory holds only the snapshot artifact tree: no
	// conversation, Project, claim, metadata or transcript files.
	entries, err := os.ReadDir(groupDir)
	testassert.NoError(t, err)
	testassert.Require(t, len(entries) == 1 && entries[0].Name() == "snapshots", "group dir entries = %v, want only snapshots", entries)

	turns, err := code.ListTurns()
	testassert.NoError(t, err)
	testassert.Require(t, len(turns) == 1 && turns[0].Turn == 1 && len(turns[0].Files) == 1, "ListTurns = %+v, want one turn with one file", turns)
	testassert.Require(t, turns[0].Files[0].LastWrite != nil && turns[0].Files[0].LastWrite.Hash == hashBytes(mutated), "last-write identity = %+v, want hash of mutated content", turns[0].Files[0].LastWrite)

	// Reopen the same group with a fresh handle: listing and restore must
	// work with no conversation/Project state at all.
	reopened, err := OpenCodeStore(groupDir)
	testassert.NoError(t, err)
	reopenedTurns, err := reopened.ListTurns()
	testassert.NoError(t, err)
	testassert.Require(t, len(reopenedTurns) == 1 && len(reopenedTurns[0].Files) == 1, "reopened ListTurns = %+v, want one turn with one file", reopenedTurns)

	result, err := reopened.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == abs && len(result.Skipped) == 0, "RevertCode restored = %v, want [%s]; RevertCode skipped = %+v, want none", result.Restored, abs, result.Skipped)
	got, err := os.ReadFile(target)
	testassert.NoError(t, err)
	testassert.Require(t, string(got) == string(original), "restored content = %q, want %q", got, original)
	after, err := os.ReadDir(filepath.Join(groupDir, "snapshots"))
	testassert.NoError(t, err)
	testassert.Require(t, len(after) == 0, "snapshots dir after restore = %v, want empty", after)
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
	testassert.NoError(t, os.WriteFile(fileA, originalA, 0o644))
	testassert.NoError(t, os.WriteFile(fileB, originalB, 0o644))

	// Each operation's group captures its own target's preimage; each
	// mutation records its post-write identity.
	capture := func(groupDir, target string, mutated []byte) {
		t.Helper()
		code, err := OpenCodeStore(groupDir)
		testassert.Require(t, err == nil, "OpenCodeStore(%s): %v", groupDir, err)
		entryID, created, err := code.SnapshotResolvedEntry(1, target, target)
		testassert.Require(t, err == nil && created, "SnapshotResolvedEntry(%s) = (%v, %v), want one created entry", groupDir, entryID, err)
		code.RetainSnapshotEntry(1, entryID)
		{
			err := os.WriteFile(target, mutated, 0o644)
			testassert.Require(t, err == nil, "mutate %s: %v", target, err)
		}
		{
			err := code.RecordSnapshotContent(1, entryID, mutated)
			testassert.Require(t, err == nil, "RecordSnapshotContent(%s): %v", groupDir, err)
		}
	}
	mutatedA, mutatedB := []byte("a mutated\n"), []byte("b mutated\n")
	capture(groupADir, fileA, mutatedA)
	capture(groupBDir, fileB, mutatedB)

	// Restoring group A through a fresh handle: only A's preimage returns,
	// B's file stays mutated and B's snapshot tree stays intact.
	restoreA, err := OpenCodeStore(groupADir)
	testassert.NoError(t, err)
	resultA, err := restoreA.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(resultA.Restored) == 1 && resultA.Restored[0] == fileA && len(resultA.Skipped) == 0, "RevertCode(A) = %+v, want exactly %s restored", resultA, fileA)
	gotA, err := os.ReadFile(fileA)
	testassert.Require(t, err == nil && string(gotA) == string(originalA), "a.txt after restore A = (%q, %v), want the original bytes", gotA, err)
	gotB, err := os.ReadFile(fileB)
	testassert.Require(t, err == nil && string(gotB) == string(mutatedB), "b.txt after restore A = (%q, %v), want it untouched at the mutated bytes", gotB, err)
	turnsB, err := mustOpenCodeStore(t, groupBDir).ListTurns()
	testassert.Require(t, err == nil && len(turnsB) == 1 && len(turnsB[0].Files) == 1, "group B turns after restore A = %+v err %v, want the snapshot tree intact", turnsB, err)
	turnsA, err := mustOpenCodeStore(t, groupADir).ListTurns()
	testassert.Require(t, err == nil && len(turnsA) == 0, "group A turns after restore A = %+v err %v, want the restored turn dirs removed", turnsA, err)

	// Restoring group B independently restores its own preimage.
	restoreB, err := OpenCodeStore(groupBDir)
	testassert.NoError(t, err)
	resultB, err := restoreB.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(resultB.Restored) == 1 && resultB.Restored[0] == fileB && len(resultB.Skipped) == 0, "RevertCode(B) = %+v, want exactly %s restored", resultB, fileB)
	gotB, err = os.ReadFile(fileB)
	testassert.Require(t, err == nil && string(gotB) == string(originalB), "b.txt after restore B = (%q, %v), want the original bytes", gotB, err)
}

// mustOpenCodeStore opens one code group, failing the test on error.
func mustOpenCodeStore(t *testing.T, groupDir string) *CodeStore {
	t.Helper()
	code, err := OpenCodeStore(groupDir)
	testassert.Require(t, err == nil, "OpenCodeStore(%s): %v", groupDir, err)
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
	testassert.NoError(t, err)
	return entryID
}

func recordSnapshotContent(t *testing.T, store *CodeStore, turn int, entryID string, content string) {
	t.Helper()
	testassert.NoError(t, store.RecordSnapshotContent(turn, entryID, []byte(content)))
}

func TestCodeStoreRejectsInvalidTurn(t *testing.T) {
	store := newTestCodeGroup(t)
	path := filepath.Join(t.TempDir(), "file.txt")
	{
		_, _, err := store.SnapshotResolvedEntry(0, path, path)
		testassert.Require(t, err != nil, "SnapshotResolvedEntry(0) error = nil, want error")
	}
}

func TestSnapshotMetaJSONRoundTrip(t *testing.T) {
	want := SnapshotMeta{OriginalPath: "/project/link.go", CanonicalPath: "/real/file.go", Existed: true}
	data, err := json.Marshal(want)
	testassert.NoError(t, err)
	var got SnapshotMeta
	testassert.NoError(t, json.Unmarshal(data, &got))
	testassert.Equal(t, got, want)
}

func TestSnapshotMetaBackwardsCompatibleWithoutCanonicalPath(t *testing.T) {
	var got SnapshotMeta
	testassert.NoError(t, json.Unmarshal([]byte(`{"original_path":"/project/file.go","existed":true}`), &got))
	testassert.Require(t, got.OriginalPath == "/project/file.go" && got.CanonicalPath == "" && got.Existed, "old snapshot meta decode = %+v, want original path with empty canonical path", got)
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
			testassert.NoError(t, os.WriteFile(realPath, []byte("v1"), 0o644))
			testassert.NoError(t, os.Symlink(realPath, linkPath))

			const turn = 1
			first, second := realPath, linkPath
			if symlinkFirst {
				first, second = linkPath, realPath
			}
			entryID := snapshotEntry(t, store, turn, first, realPath)
			// The second capture resolves its requested path to a canonical
			// target the way the tool layer binds one before capture.
			resolved, err := pathutil.ResolveFilePath(second)
			testassert.NoError(t, err)
			snapshotEntry(t, store, turn, second, resolved.CanonicalPath)

			entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1"))
			testassert.NoError(t, err)
			testassert.Equal(t, len(entries), 1)

			testassert.NoError(t, os.WriteFile(realPath, []byte("v2"), 0o644))
			recordSnapshotContent(t, store, turn, entryID, "v2")
			affected, err := store.RevertCode(0)
			testassert.NoError(t, err)
			testassert.Require(t, len(affected.Restored) == 1, "affected = %v, want one path", affected)
			got, err := os.ReadFile(realPath)
			testassert.NoError(t, err)
			testassert.Require(t, string(got) == "v1", "content = %q, want v1", got)
		})
	}
}

func TestSnapshotResolvedPreservesOriginalPathForListing(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	realPath := filepath.Join(projectDir, "file.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	testassert.NoError(t, os.WriteFile(realPath, []byte("v1"), 0o644))
	testassert.NoError(t, os.Symlink(realPath, linkPath))

	const turn = 1
	entryID := snapshotEntry(t, store, turn, linkPath, realPath)

	entries, err := store.ListTurns()
	testassert.NoError(t, err)
	testassert.Require(t, len(entries) == 1 && len(entries[0].Files) == 1, "ListTurns = %+v, want one snapshotted file", entries)
	testassert.Equal(t, entries[0].Files[0].OriginalPath, linkPath)
	testassert.NoError(t, os.WriteFile(realPath, []byte("v2"), 0o644))
	recordSnapshotContent(t, store, turn, entryID, "v2")
	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{linkPath}), "affected = %v, want original requested path %q", affected, linkPath)
	{
		got, err := os.ReadFile(realPath)
		testassert.Require(t, err == nil && string(got) == "v1", "real file after revert = %q, %v; want v1", got, err)
	}
}

func TestSnapshotResolvedEntryReportsCreatedOnlyForNewEntry(t *testing.T) {
	store := newTestCodeGroup(t)
	const turn = 1
	path := filepath.Join(t.TempDir(), "file.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	entryID, created, err := store.SnapshotResolvedEntry(turn, path, path)
	testassert.NoError(t, err)
	testassert.Require(t, entryID != "" && created, "first SnapshotResolvedEntry = (%q, %v), want non-empty created entry", entryID, created)
	entryID2, created, err := store.SnapshotResolvedEntry(turn, path, path)
	testassert.NoError(t, err)
	testassert.Require(t, entryID2 == entryID && !created, "second SnapshotResolvedEntry = (%q, %v), want same existing entry %q", entryID2, created, entryID)
	testassert.NoError(t, store.DiscardSnapshotEntry(turn, entryID))
	if entries, err := os.ReadDir(filepath.Join(store.snapshotsDir, "1")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("snapshot entries after one discard = %d, want one retained by second user", len(entries))
	}
	testassert.NoError(t, store.DiscardSnapshotEntry(turn, entryID2))
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
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	entryID, created, err := store.SnapshotResolvedEntry(turn, path, path)
	testassert.NoError(t, err)
	testassert.Require(t, created, "first SnapshotResolvedEntry was not created")
	entryID2, created, err := store.SnapshotResolvedEntry(turn, path, path)
	testassert.NoError(t, err)
	testassert.Require(t, entryID2 == entryID && !created, "second SnapshotResolvedEntry = (%q, %v), want same existing entry %q", entryID2, created, entryID)
	store.RetainSnapshotEntry(turn, entryID2)
	testassert.NoError(t, store.DiscardSnapshotEntry(turn, entryID))
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
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	entryID := snapshotEntry(t, store, turn, path, path)
	store.RetainSnapshotEntry(turn, entryID)
	testassert.NoError(t, store.DiscardSnapshotEntry(turn, hashString(path)))
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
	testassert.NoError(t, os.WriteFile(existing, []byte("v1"), 0o644))
	const turn = 1
	existingEntry := snapshotEntry(t, store, turn, existing, existing)
	testassert.NoError(t, os.WriteFile(existing, []byte("v2"), 0o644))
	recordSnapshotContent(t, store, turn, existingEntry, "v2")
	// The second capture of the same target keeps the first preimage.
	snapshotEntry(t, store, turn, existing, existing)
	createdEntry := snapshotEntry(t, store, turn, created, created)
	testassert.NoError(t, os.WriteFile(created, []byte("new"), 0o644))
	recordSnapshotContent(t, store, turn, createdEntry, "new")

	{
		_, err := store.RevertCode(0)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(existing)
		testassert.Require(t, err == nil && string(got) == "v1", "existing content = %q, err = %v; want v1", got, err)
	}
	{
		_, err := os.Stat(created)
		testassert.Require(t, os.IsNotExist(err), "created file should be removed, stat err = %v", err)
	}
}

func TestRevertSkipsCanonicalPathReplacedBySymlinkAndContinues(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	repointedTarget := filepath.Join(projectDir, "repointed.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	ownedPath := filepath.Join(projectDir, "owned.txt")
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("before"), 0o644))
	testassert.NoError(t, os.WriteFile(repointedTarget, []byte("other"), 0o644))
	testassert.NoError(t, os.WriteFile(ownedPath, []byte("owned-before"), 0o644))
	testassert.NoError(t, os.Symlink(originalTarget, linkPath))

	const turn = 1
	linkEntry := snapshotEntry(t, store, turn, linkPath, originalTarget)
	ownedEntry := snapshotEntry(t, store, turn, ownedPath, ownedPath)
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("after"), 0o644))
	recordSnapshotContent(t, store, turn, linkEntry, "after")
	testassert.NoError(t, os.WriteFile(ownedPath, []byte("owned-after"), 0o644))
	recordSnapshotContent(t, store, turn, ownedEntry, "owned-after")
	testassert.NoError(t, os.Remove(linkPath))
	testassert.NoError(t, os.Symlink(repointedTarget, linkPath))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{ownedPath}) && len(affected.Skipped) == 1 && affected.Skipped[0].Path == linkPath, "restored = %v, want [%s]; skipped = %+v, want %s", affected.Restored, ownedPath, affected.Skipped, linkPath)
	{
		got, err := os.ReadFile(originalTarget)
		testassert.Require(t, err == nil && string(got) == "after", "original target = %q, %v; want after", got, err)
	}
	{
		got, err := os.ReadFile(repointedTarget)
		testassert.Require(t, err == nil && string(got) == "other", "repointed target = %q, %v; want other", got, err)
	}
	{
		got, err := os.ReadFile(ownedPath)
		testassert.Require(t, err == nil && string(got) == "owned-before", "owned target = %q, %v; want owned-before", got, err)
	}
}

func TestRevertSkipsExistingFileReplacedByDirectoryAndContinues(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	dirPath := filepath.Join(projectDir, "target.txt")
	ownedPath := filepath.Join(projectDir, "owned.txt")
	testassert.NoError(t, os.WriteFile(dirPath, []byte("before"), 0o644))
	testassert.NoError(t, os.WriteFile(ownedPath, []byte("owned-before"), 0o644))

	const turn = 1
	dirEntry := snapshotEntry(t, store, turn, dirPath, dirPath)
	ownedEntry := snapshotEntry(t, store, turn, ownedPath, ownedPath)
	testassert.NoError(t, os.WriteFile(dirPath, []byte("after"), 0o644))
	recordSnapshotContent(t, store, turn, dirEntry, "after")
	testassert.NoError(t, os.WriteFile(ownedPath, []byte("owned-after"), 0o644))
	recordSnapshotContent(t, store, turn, ownedEntry, "owned-after")
	testassert.NoError(t, os.Remove(dirPath))
	testassert.NoError(t, os.Mkdir(dirPath, 0o755))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{ownedPath}) && len(affected.Skipped) == 1 && affected.Skipped[0].Path == dirPath, "restored = %v, want [%s]; skipped = %+v, want %s", affected.Restored, ownedPath, affected.Skipped, dirPath)
	{
		info, err := os.Lstat(dirPath)
		testassert.Require(t, err == nil && info.IsDir(), "directory replacement = %v, %v; want preserved directory", info, err)
	}
	{
		got, err := os.ReadFile(ownedPath)
		testassert.Require(t, err == nil && string(got) == "owned-before", "owned target = %q, %v; want owned-before", got, err)
	}
}

func TestRevertOldSnapshotMetaFallsBackToOriginalPath(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(path, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: path,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Hash: hashBytes([]byte("after"))},
	}))
	testassert.NoError(t, os.WriteFile(path, []byte("after"), 0o644))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{path}), "affected = %v, want [%s]", affected, path)
	{
		got, err := os.ReadFile(path)
		testassert.Require(t, err == nil && string(got) == "before", "restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRestoresMissingRegularPathWithOldHash(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(path))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(path, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: path,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Absent: true},
	}))
	testassert.NoError(t, os.Remove(path))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{path}), "affected = %v, want [%s]", affected, path)
	{
		got, err := os.ReadFile(path)
		testassert.Require(t, err == nil && string(got) == "before", "restored content = %q, %v; want before", got, err)
	}
}

func TestRevertSkipsExistingFileSnapshotWithoutRecordedLastWriteIdentity(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "target.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	const turn = 1
	snapshotEntry(t, store, turn, path, path)
	testassert.NoError(t, os.WriteFile(path, []byte("after"), 0o644))

	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == path, "restored = %v, want none; skipped = %+v, want %s", affected.Restored, affected, path)
	{
		got, err := os.ReadFile(path)
		testassert.Require(t, err == nil && string(got) == "after", "path content = %q, %v; want after", got, err)
	}
	{
		got := readIntDirs(store.snapshotsDir)
		testassert.Require(t, reflect.DeepEqual(got, []int{turn}), "snapshot dirs = %v, want [%d]", got, turn)
	}
}

func TestRevertOldSnapshotMetaRestoresCurrentSymlinkTargetWhenHashMatches(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("before"), 0o644))
	testassert.NoError(t, os.Symlink(originalTarget, linkPath))

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(originalTarget, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: linkPath,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Hash: hashBytes([]byte("after"))},
	}))
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("after"), 0o644))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{linkPath}), "affected = %v, want [%s]", affected, linkPath)
	{
		got, err := os.ReadFile(originalTarget)
		testassert.Require(t, err == nil && string(got) == "before", "restored content = %q, %v; want before", got, err)
	}
}

func TestRevertOldSnapshotMetaRefusesGoneSymlinkTarget(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("before"), 0o644))
	testassert.NoError(t, os.Symlink(originalTarget, linkPath))

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(originalTarget, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: linkPath, Existed: true}))
	testassert.NoError(t, os.Remove(linkPath))

	affected, err := store.RevertCode(0)

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "target missing and cannot be proven"), "RevertCode error = %v, want missing legacy target refusal", err)
	testassert.Require(t, len(affected.Restored) == 0, "affected = %v, want none on refused restore", affected)
}

func TestRevertOldSnapshotMetaRefusesCurrentSymlinkPath(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalTarget := filepath.Join(projectDir, "original.txt")
	repointedTarget := filepath.Join(projectDir, "repointed.txt")
	linkPath := filepath.Join(projectDir, "link.txt")
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("before"), 0o644))
	testassert.NoError(t, os.WriteFile(repointedTarget, []byte("other"), 0o644))
	testassert.NoError(t, os.Symlink(originalTarget, linkPath))

	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(originalTarget))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(originalTarget, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: linkPath, Existed: true}))
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("after"), 0o644))
	testassert.NoError(t, os.Remove(linkPath))
	testassert.NoError(t, os.Symlink(repointedTarget, linkPath))

	affected, err := store.RevertCode(0)

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "legacy snapshot target hash changed"), "RevertCode error = %v, want legacy hash-change refusal", err)
	testassert.Require(t, len(affected.Restored) == 0, "affected = %v, want none on refused restore", affected)
	{
		got, err := os.ReadFile(originalTarget)
		testassert.Require(t, err == nil && string(got) == "after", "original target = %q, %v; want after", got, err)
	}
	{
		got, err := os.ReadFile(repointedTarget)
		testassert.Require(t, err == nil && string(got) == "other", "repointed target = %q, %v; want other", got, err)
	}
}

func TestRevertModernSnapshotSkipsFinalTargetSwappedToSymlink(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "target.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))
	testassert.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))

	const turn = 1
	entryID := snapshotEntry(t, store, turn, target, target)
	testassert.NoError(t, os.WriteFile(target, []byte("after"), 0o644))
	recordSnapshotContent(t, store, turn, entryID, "after")
	testassert.NoError(t, os.Remove(target))
	testassert.NoError(t, os.Symlink(secret, target))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == target, "restored = %v, want none; skipped = %+v, want %s", affected.Restored, affected, target)
	{
		got, err := os.ReadFile(secret)
		testassert.Require(t, err == nil && string(got) == "secret", "secret target = %q, %v; want unchanged", got, err)
	}
}

func TestRevertExistedFalseSkipsSymlinkWithoutRecordedIdentity(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created.txt")
	secret := filepath.Join(projectDir, "secret.txt")
	testassert.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))

	const turn = 1
	snapshotEntry(t, store, turn, createdPath, createdPath)
	testassert.NoError(t, os.Symlink(secret, createdPath))

	affected, err := store.RevertCode(0)

	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == createdPath, "restored = %v, want none; skipped = %+v, want %s", affected.Restored, affected, createdPath)
	{
		_, err := os.Lstat(createdPath)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(secret)
		testassert.Require(t, err == nil && string(got) == "secret", "secret target = %q, %v; want unchanged", got, err)
	}
}

func TestRevertExistedFalseRefusesRepointedParent(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalRoot := filepath.Join(projectDir, "original")
	repointedRoot := filepath.Join(projectDir, "repointed")
	linkRoot := filepath.Join(projectDir, "link")
	for _, dir := range []string{originalRoot, repointedRoot} {
		testassert.NoError(t, os.Mkdir(dir, 0o755))
	}
	testassert.NoError(t, os.Symlink(originalRoot, linkRoot))
	requestedPath := filepath.Join(linkRoot, "created.txt")
	originalTarget := filepath.Join(originalRoot, "created.txt")
	repointedTarget := filepath.Join(repointedRoot, "created.txt")

	const turn = 1
	entryID := snapshotEntry(t, store, turn, requestedPath, originalTarget)
	testassert.NoError(t, os.WriteFile(originalTarget, []byte("original"), 0o644))
	recordSnapshotContent(t, store, turn, entryID, "original")
	testassert.NoError(t, os.WriteFile(repointedTarget, []byte("repointed"), 0o644))
	testassert.NoError(t, os.Remove(linkRoot))
	testassert.NoError(t, os.Symlink(repointedRoot, linkRoot))

	affected, err := store.RevertCode(0)

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "canonical path changed"), "RevertCode error = %v, want canonical path changed error", err)
	testassert.Require(t, len(affected.Restored) == 0, "affected = %v, want none on refused delete", affected)
	{
		got, err := os.ReadFile(originalTarget)
		testassert.Require(t, err == nil && string(got) == "original", "original target = %q, %v; want preserved", got, err)
	}
	{
		got, err := os.ReadFile(repointedTarget)
		testassert.Require(t, err == nil && string(got) == "repointed", "repointed target = %q, %v; want preserved", got, err)
	}
}

func TestRevertCodeRestoresLaterSnapshotsAndDeletesLaterSnapshotDirs(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "created.txt")

	// The empty turn-1 artifact directory is seeded explicitly: turn identity
	// is named by the test, not allocated.
	testassert.NoError(t, os.MkdirAll(filepath.Join(store.snapshotsDir, "1"), 0o700))
	entryID := snapshotEntry(t, store, 2, path, path)
	testassert.NoError(t, os.WriteFile(path, []byte("new"), 0o644))
	recordSnapshotContent(t, store, 2, entryID, "new")

	affected, err := store.RevertCode(1)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{path}), "affected = %v, want [%s]", affected, path)
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "created file should be removed after RevertCode, stat err = %v", err)
	}
	{
		got := readIntDirs(store.snapshotsDir)
		testassert.Require(t, reflect.DeepEqual(got, []int{1}), "snapshot dirs = %v, want [1]", got)
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
		testassert.NoError(t, os.WriteFile(p, []byte("before"), 0o644))
	}
	testassert.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))

	const turn = 1
	entryA := snapshotEntry(t, store, turn, pathA, pathA)
	entryB := snapshotEntry(t, store, turn, pathB, pathB)
	recordSnapshotContent(t, store, turn, entryA, "before")
	recordSnapshotContent(t, store, turn, entryB, "before")

	// revertOneTurn processes entries in os.ReadDir order (lexical by entry
	// dir name = hashString(EvalSymlinks(canonicalPath))). Resolve symlinks
	// before hashing so the prediction holds when TMPDIR itself is symlinked.
	realA, err := filepath.EvalSymlinks(pathA)
	testassert.NoError(t, err)
	realB, err := filepath.EvalSymlinks(pathB)
	testassert.NoError(t, err)
	firstPath, secondPath := pathA, pathB
	if hashString(realA) > hashString(realB) {
		firstPath, secondPath = pathB, pathA
	}
	testassert.NoError(t, os.Remove(secondPath))
	testassert.NoError(t, os.Symlink(secret, secondPath))

	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	containsFirst := false
	for _, p := range affected.Restored {
		if p == firstPath {
			containsFirst = true
			break
		}
	}
	testassert.Require(t, containsFirst, "affected = %v, want to include %q (already-restored entry before mid-turn failure)", affected, firstPath)
	testassert.Require(t, len(affected.Skipped) == 1 && affected.Skipped[0].Path == secondPath, "skipped = %+v, want %s", affected, secondPath)
}

// One skipped identity blocks every older snapshot entry of the same file
// across turns, and the skipped entries' turn dirs stay recorded.
func TestRevertSkipBlocksOlderSnapshotForSameFile(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "file.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	entry1 := snapshotEntry(t, store, 1, path, path)
	testassert.NoError(t, os.WriteFile(path, []byte("first"), 0o644))
	recordSnapshotContent(t, store, 1, entry1, "first")

	entry2 := snapshotEntry(t, store, 2, path, path)
	testassert.NoError(t, os.WriteFile(path, []byte("second"), 0o644))
	recordSnapshotContent(t, store, 2, entry2, "second")

	testassert.NoError(t, os.WriteFile(path, []byte("first"), 0o644))

	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == path, "restored = %v, want none; skipped = %+v, want one skipped %s", affected.Restored, affected, path)
	{
		got, err := os.ReadFile(path)
		testassert.Require(t, err == nil && string(got) == "first", "path content = %q, %v; want first", got, err)
	}
	{
		got := readIntDirs(store.snapshotsDir)
		testassert.Require(t, reflect.DeepEqual(got, []int{1, 2}), "snapshot dirs = %v, want [1 2]", got)
	}
}

// copyFile seeds a snapshot preimage by hand in this package's tests: it
// copies a source file into an entry's original/ path exactly as capture
// does. It lives here because no production caller remains.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyFromFile(in, dst)
}
