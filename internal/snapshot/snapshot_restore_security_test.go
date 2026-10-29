package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestRevertSkipsCreatedDirectoryLeavesWithoutRecordedIdentity(t *testing.T) {
	t.Run("empty directory is preserved", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "created-dir")
		const turn = 1
		snapshotEntry(t, store, turn, createdPath, createdPath)
		testassert.NoError(t, os.Mkdir(createdPath, 0o755))

		affected, err := revertGroup(store, 0)
		testassert.NoError(t, err)
		testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == createdPath, "restored = %v, want none; skipped = %+v, want %s", affected.Restored, affected.Skipped, createdPath)
		if info, err := os.Lstat(createdPath); err != nil || !info.IsDir() {
			t.Fatalf("created directory should be preserved, stat = %v isDir=%v", err, err == nil && info.IsDir())
		}
	})

	t.Run("non-empty directory is preserved", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "created-dir")
		childPath := filepath.Join(createdPath, "child.txt")
		const turn = 1
		snapshotEntry(t, store, turn, createdPath, createdPath)
		testassert.NoError(t, os.Mkdir(createdPath, 0o755))
		testassert.NoError(t, os.WriteFile(childPath, []byte("child"), 0o644))

		affected, err := revertGroup(store, 0)
		testassert.NoError(t, err)
		testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == createdPath, "restored = %v, want none; skipped = %+v, want %s", affected.Restored, affected.Skipped, createdPath)
		{
			got, err := os.ReadFile(childPath)
			testassert.Require(t, err == nil && string(got) == "child", "child content = %q, %v; want preserved", got, err)
		}
	})
}

func TestLegacyCreatedDeleteRequiresCleanPathHash(t *testing.T) {
	// Hash match or mismatch, the legacy Existed:false branch must refuse —
	// legacy snapshots carry no capture-time canonical witness, so the
	// restore cannot prove the current path matches what was observed-as-missing.
	t.Run("hash match still refuses", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		testassert.NoError(t, os.Mkdir(filepath.Join(projectDir, "sub"), 0o755))
		cleanPath := filepath.Join(projectDir, "created.txt")
		dirtyPath := filepath.Join(projectDir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "created.txt"
		testassert.NoError(t, os.WriteFile(cleanPath, []byte("created"), 0o644))
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(dirtyPath)))
		testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
		testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: dirtyPath, Existed: false}))

		_, err := revertGroup(store, 0)
		testassert.Require(t, err != nil, "RevertCode succeeded for legacy Existed:false with hash match; want unconditional refusal")
		testassert.Require(t, strings.Contains(err.Error(), "legacy delete refused"), "error %v does not mention legacy delete refused", err)
		{
			got, readErr := os.ReadFile(cleanPath)
			testassert.Require(t, readErr == nil && string(got) == "created", "file content = %q, %v; want unchanged after refused legacy delete", got, readErr)
		}
	})

	t.Run("hash mismatch refuses", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		testassert.NoError(t, os.Mkdir(filepath.Join(projectDir, "sub"), 0o755))
		victimClean := filepath.Join(projectDir, "victim.txt")
		victimDirty := filepath.Join(projectDir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "victim.txt"
		otherClean := filepath.Join(projectDir, "other.txt")
		testassert.NoError(t, os.WriteFile(victimClean, []byte("victim"), 0o644))
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(otherClean)))
		testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
		testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: victimDirty, Existed: false}))

		affected, err := revertGroup(store, 0)
		testassert.Require(t, err != nil, "RevertCode succeeded for mismatched legacy delete hash, want refusal")
		testassert.Require(t, len(affected.Restored) == 0, "affected = %v, want none", affected)
		{
			got, err := os.ReadFile(victimClean)
			testassert.Require(t, err == nil && string(got) == "victim", "victim content = %q, %v; want unchanged", got, err)
		}
	})
}

func TestLegacyMissingLeafRestoreRequiresTargetProof(t *testing.T) {
	t.Run("symlink parent still points to original target", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		realDir := filepath.Join(projectDir, "real")
		testassert.NoError(t, os.Mkdir(realDir, 0o755))
		linkDir := filepath.Join(projectDir, "linked")
		testassert.NoError(t, os.Symlink(realDir, linkDir))
		target := filepath.Join(realDir, "file.txt")
		requested := filepath.Join(linkDir, "file.txt")
		testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))
		writeLegacySnapshotForMissingLeafProof(t, store, requested, target)
		testassert.NoError(t, os.Remove(target))

		affected, err := revertGroup(store, 0)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(affected.Restored, []string{requested}), "affected = %v, want [%s]", affected, requested)
		{
			got, err := os.ReadFile(target)
			testassert.Require(t, err == nil && string(got) == "before", "restored target = %q, %v; want before", got, err)
		}
	})

	t.Run("symlink parent repointed elsewhere", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		realDir := filepath.Join(projectDir, "real")
		secretDir := filepath.Join(projectDir, "secret")
		testassert.NoError(t, os.Mkdir(realDir, 0o755))
		testassert.NoError(t, os.Mkdir(secretDir, 0o755))
		linkDir := filepath.Join(projectDir, "linked")
		testassert.NoError(t, os.Symlink(realDir, linkDir))
		target := filepath.Join(realDir, "file.txt")
		requested := filepath.Join(linkDir, "file.txt")
		secretTarget := filepath.Join(secretDir, "file.txt")
		testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))
		testassert.NoError(t, os.WriteFile(secretTarget, []byte("secret"), 0o644))
		writeLegacySnapshotForMissingLeafProof(t, store, requested, target)
		testassert.NoError(t, os.Remove(target))
		testassert.NoError(t, os.Remove(linkDir))
		testassert.NoError(t, os.Symlink(secretDir, linkDir))

		affected, err := revertGroup(store, 0)
		testassert.Require(t, err != nil, "RevertCode succeeded for repointed symlink parent, want refusal")
		testassert.Require(t, len(affected.Restored) == 0, "affected = %v, want none", affected)
		{
			got, err := os.ReadFile(secretTarget)
			testassert.Require(t, err == nil && string(got) == "secret", "secret target = %q, %v; want unchanged", got, err)
		}
	})
}

func writeLegacySnapshotForMissingLeafProof(t *testing.T, store *CodeStore, requested, canonical string) {
	t.Helper()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(canonical))
	testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
	testassert.NoError(t, copyFile(canonical, filepath.Join(entryDir, "original")))
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{
		OriginalPath: requested,
		Existed:      true,
		LastWrite:    &SnapshotContentIdentity{Absent: true},
	}))
}

// Legacy Existed:false snapshots (no meta.CanonicalPath) must be refused
// at restore time — they carry no capture-time canonical witness, so the
// restore cannot prove the current path matches what was observed-as-missing.
func TestPR11Closure_LegacyExistedFalseDeletesAreRefused(t *testing.T) {
	t.Run("file present at OriginalPath", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "victim.txt")
		testassert.NoError(t, os.WriteFile(createdPath, []byte("victim-content"), 0o644))
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(createdPath)))
		testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
		testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"),
			SnapshotMeta{OriginalPath: createdPath, Existed: false}))

		_, err := revertGroup(store, 0)
		testassert.Require(t, err != nil, "RevertCode succeeded for legacy Existed:false, want refusal")
		testassert.Require(t, strings.Contains(err.Error(), "legacy delete refused"), "error %v does not mention legacy delete refused", err)
		{
			got, err := os.ReadFile(createdPath)
			testassert.Require(t, err == nil && string(got) == "victim-content", "file content = %q, %v; want unchanged", got, err)
		}
	})

	t.Run("target absent at OriginalPath", func(t *testing.T) {
		store := newTestCodeGroup(t)
		projectDir := t.TempDir()
		absentPath := filepath.Join(projectDir, "absent.txt")
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(absentPath)))
		testassert.NoError(t, os.MkdirAll(entryDir, 0o700))
		testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"),
			SnapshotMeta{OriginalPath: absentPath, Existed: false}))

		_, err := revertGroup(store, 0)
		testassert.Require(t, err != nil, "RevertCode succeeded for legacy Existed:false with absent target, want refusal")
		testassert.Require(t, strings.Contains(err.Error(), "legacy delete refused"), "error %v does not mention legacy delete refused", err)
	})
}

// A modern snapshot whose current on-disk leaf is a hardlink to an
// outside-boundary inode must be refused by restore so the FD-acquisition
// nlink check stops the mutation before it crosses the project boundary.
func TestPR11Closure_HardlinkRefusedOnSnapshotRestore(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "target.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))

	const turn = 1
	entryID := snapshotEntry(t, store, turn, target, target)

	// Replace target with a hardlink to an outside-boundary file.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	testassert.NoError(t, os.WriteFile(outside, []byte("outside-content"), 0o644))
	testassert.NoError(t, os.Remove(target))
	testassert.NoError(t, os.Link(outside, target))
	recordSnapshotContent(t, store, turn, entryID, "outside-content")

	_, err := revertGroup(store, 0)
	testassert.Require(t, err != nil, "RevertCode succeeded with hardlinked restore target; want refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	{
		got, err := os.ReadFile(outside)
		testassert.Require(t, err == nil && string(got) == "outside-content", "outside file = %q, %v; want unchanged", got, err)
	}
}

// A modern Existed:false snapshot must refuse to delete a current hardlinked
// regular-file leaf. This closes the created-file delete branch, which does
// not go through restoreFile's OpenForWrite nlink check.
func TestPR11Closure_HardlinkRefusedOnSnapshotCreatedDelete(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created.txt")

	const turn = 1
	entryID := snapshotEntry(t, store, turn, createdPath, createdPath)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	testassert.NoError(t, os.WriteFile(outside, []byte("outside-content"), 0o644))
	testassert.NoError(t, os.Link(outside, createdPath))
	recordSnapshotContent(t, store, turn, entryID, "outside-content")

	_, err := revertGroup(store, 0)
	testassert.Require(t, err != nil, "RevertCode succeeded deleting hardlinked created-file leaf; want refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	{
		_, err := os.Lstat(createdPath)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(outside)
		testassert.Require(t, err == nil && string(got) == "outside-content", "outside file = %q, %v; want unchanged", got, err)
	}
}

func TestPR11Closure_HardlinkedSymlinkRefusedOnSnapshotCreatedDelete(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created-link")

	const turn = 1
	entryID := snapshotEntry(t, store, turn, createdPath, createdPath)

	target := filepath.Join(t.TempDir(), "outside-target.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("outside-target"), 0o644))
	originalLink := filepath.Join(t.TempDir(), "original-link")
	testassert.NoError(t, os.Symlink(target, originalLink))
	testassert.NoError(t, os.Link(originalLink, createdPath))
	assertHardlinkedSymlink(t, originalLink)
	assertHardlinkedSymlink(t, createdPath)
	recordSnapshotContent(t, store, turn, entryID, "outside-target")

	_, err := revertGroup(store, 0)
	testassert.Require(t, err != nil, "RevertCode succeeded deleting hardlinked symlink created leaf; want refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	{
		_, err := os.Lstat(originalLink)
		testassert.NoError(t, err)
	}
	{
		_, err := os.Lstat(createdPath)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(target)
		testassert.Require(t, err == nil && string(got) == "outside-target", "target file = %q, %v; want unchanged", got, err)
	}
}

// A modern Existed:false snapshot must be bound to the entry directory ID
// (hashString(realPath)). A meta.json content rewrite alone that redirects
// CanonicalPath must be refused because hashString(new path) no longer
// matches the on-disk entry directory name.
func TestPR11Closure_ModernDeleteBoundToEntryID(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalPath := filepath.Join(projectDir, "created.txt")
	victimPath := filepath.Join(projectDir, "victim.txt")
	testassert.NoError(t, os.WriteFile(victimPath, []byte("victim-content"), 0o644))

	const turn = 1
	snapshotEntry(t, store, turn, originalPath, originalPath)
	// The capture writes meta with CanonicalPath=originalPath, entryDir=hashString(originalPath).
	// Tamper meta.json to redirect CanonicalPath (and OriginalPath) to victimPath.
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(originalPath)))
	{
		_, err := os.Stat(entryDir)
		testassert.Require(t, err == nil, "expected snapshot entry dir at %s: %v", entryDir, err)
	}
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"),
		SnapshotMeta{OriginalPath: victimPath, CanonicalPath: victimPath, Existed: false}))

	_, err := revertGroup(store, 0)
	testassert.Require(t, err != nil, "RevertCode succeeded with tampered meta.json; want refusal")
	{
		got, readErr := os.ReadFile(victimPath)
		testassert.Require(t, readErr == nil && string(got) == "victim-content", "victim file = %q, %v; want unchanged", got, readErr)
	}
	_ = turn
}

// A modern Existed:true snapshot must be bound to the entry directory ID
// (hashString(realPath)). A meta.json content rewrite alone that redirects
// CanonicalPath must be refused because hashString(new path) no longer
// matches the on-disk entry directory name. Symmetric with the modern delete
// binding in TestPR11Closure_ModernDeleteBoundToEntryID.
func TestPR11Closure_ModernRestoreBoundToEntryID(t *testing.T) {
	store := newTestCodeGroup(t)
	projectDir := t.TempDir()
	originalPath := filepath.Join(projectDir, "original.txt")
	victimPath := filepath.Join(projectDir, "victim.txt")
	testassert.NoError(t, os.WriteFile(originalPath, []byte("original-content"), 0o644))
	testassert.NoError(t, os.WriteFile(victimPath, []byte("victim-content"), 0o644))

	const turn = 1
	snapshotEntry(t, store, turn, originalPath, originalPath)
	// Snapshot names the entry dir after hashString(EvalSymlinks(canonicalPath)),
	// so resolve symlinks before hashing in case TMPDIR itself is symlinked.
	realOriginal, err := filepath.EvalSymlinks(originalPath)
	testassert.NoError(t, err)
	realVictim, err := filepath.EvalSymlinks(victimPath)
	testassert.NoError(t, err)
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(realOriginal))
	{
		_, err := os.Stat(entryDir)
		testassert.Require(t, err == nil, "expected snapshot entry dir at %s: %v", entryDir, err)
	}
	// Redirect meta to victimPath, but write its canonical-resolved form so
	// the tampered entry is self-consistent under ResolveFilePath. Without
	// the new entry-id binding, the redirect would slip through; with it,
	// hashString(realVictim) != entryID is the only line of defense.
	testassert.NoError(t, writeJSON(filepath.Join(entryDir, "meta.json"),
		SnapshotMeta{OriginalPath: victimPath, CanonicalPath: realVictim, Existed: true}))

	_, err = revertGroup(store, 0)
	testassert.Require(t, err != nil, "RevertCode succeeded with tampered modern restore meta.json; want refusal")
	testassert.Require(t, strings.Contains(err.Error(), "modern restore refused"), "error %v does not mention modern restore refusal", err)
	{
		got, readErr := os.ReadFile(victimPath)
		testassert.Require(t, readErr == nil && string(got) == "victim-content", "victim file = %q, %v; want unchanged", got, readErr)
	}
	_ = turn
}

func assertHardlinkedSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	testassert.Require(t, err == nil, "lstat %s: %v", path, err)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s mode = %s, want symlink", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s stat type = %T, want *syscall.Stat_t", path, info.Sys())
	}
	testassert.Require(t, st.Nlink > 1, "%s symlink nlink = %d, want > 1", path, st.Nlink)
}
