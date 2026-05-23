package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestRevertHandlesCreatedDirectoryLeaves(t *testing.T) {
	t.Run("empty directory is removed", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "created-dir")
		turn := store.BeginTurn()
		if err := store.Snapshot(turn, createdPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(createdPath, 0o755); err != nil {
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
			t.Fatalf("created directory should be removed, lstat err = %v", err)
		}
	})

	t.Run("non-empty directory is refused and preserved", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "created-dir")
		childPath := filepath.Join(createdPath, "child.txt")
		turn := store.BeginTurn()
		if err := store.Snapshot(turn, createdPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(createdPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(childPath, []byte("child"), 0o644); err != nil {
			t.Fatal(err)
		}

		affected, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode removed non-empty directory, want refusal")
		}
		if len(affected) != 0 {
			t.Fatalf("affected = %v, want none on refused non-empty directory delete", affected)
		}
		if got, err := os.ReadFile(childPath); err != nil || string(got) != "child" {
			t.Fatalf("child content = %q, %v; want preserved", got, err)
		}
	})
}

func TestLegacyCreatedDeleteRequiresCleanPathHash(t *testing.T) {
	// Hash match or mismatch, the legacy Existed:false branch must refuse —
	// legacy snapshots carry no capture-time canonical witness, so the
	// restore cannot prove the current path matches what was observed-as-missing.
	t.Run("hash match still refuses", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(projectDir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		cleanPath := filepath.Join(projectDir, "created.txt")
		dirtyPath := filepath.Join(projectDir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "created.txt"
		if err := os.WriteFile(cleanPath, []byte("created"), 0o644); err != nil {
			t.Fatal(err)
		}
		turn := store.BeginTurn()
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(dirtyPath)))
		if err := os.MkdirAll(entryDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: dirtyPath, Existed: false}); err != nil {
			t.Fatal(err)
		}
		if turn != 1 {
			t.Fatalf("turn = %d, want 1", turn)
		}

		_, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode succeeded for legacy Existed:false with hash match; want unconditional refusal")
		}
		if !strings.Contains(err.Error(), "legacy delete refused") {
			t.Fatalf("error %v does not mention legacy delete refused", err)
		}
		if got, readErr := os.ReadFile(cleanPath); readErr != nil || string(got) != "created" {
			t.Fatalf("file content = %q, %v; want unchanged after refused legacy delete", got, readErr)
		}
	})

	t.Run("hash mismatch refuses", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(projectDir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		victimClean := filepath.Join(projectDir, "victim.txt")
		victimDirty := filepath.Join(projectDir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "victim.txt"
		otherClean := filepath.Join(projectDir, "other.txt")
		if err := os.WriteFile(victimClean, []byte("victim"), 0o644); err != nil {
			t.Fatal(err)
		}
		turn := store.BeginTurn()
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(otherClean)))
		if err := os.MkdirAll(entryDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: victimDirty, Existed: false}); err != nil {
			t.Fatal(err)
		}
		if turn != 1 {
			t.Fatalf("turn = %d, want 1", turn)
		}

		affected, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode succeeded for mismatched legacy delete hash, want refusal")
		}
		if len(affected) != 0 {
			t.Fatalf("affected = %v, want none", affected)
		}
		if got, err := os.ReadFile(victimClean); err != nil || string(got) != "victim" {
			t.Fatalf("victim content = %q, %v; want unchanged", got, err)
		}
	})
}

func TestLegacyMissingLeafRestoreRequiresTargetProof(t *testing.T) {
	t.Run("symlink parent still points to original target", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		realDir := filepath.Join(projectDir, "real")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(projectDir, "linked")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(realDir, "file.txt")
		requested := filepath.Join(linkDir, "file.txt")
		if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeLegacySnapshotForMissingLeafProof(t, store, requested, target)
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}

		affected, err := store.RevertCode(0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(affected, []string{requested}) {
			t.Fatalf("affected = %v, want [%s]", affected, requested)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "before" {
			t.Fatalf("restored target = %q, %v; want before", got, err)
		}
	})

	t.Run("symlink parent repointed elsewhere", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		realDir := filepath.Join(projectDir, "real")
		secretDir := filepath.Join(projectDir, "secret")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(secretDir, 0o755); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(projectDir, "linked")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(realDir, "file.txt")
		requested := filepath.Join(linkDir, "file.txt")
		secretTarget := filepath.Join(secretDir, "file.txt")
		if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(secretTarget, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeLegacySnapshotForMissingLeafProof(t, store, requested, target)
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(linkDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secretDir, linkDir); err != nil {
			t.Fatal(err)
		}

		affected, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode succeeded for repointed symlink parent, want refusal")
		}
		if len(affected) != 0 {
			t.Fatalf("affected = %v, want none", affected)
		}
		if got, err := os.ReadFile(secretTarget); err != nil || string(got) != "secret" {
			t.Fatalf("secret target = %q, %v; want unchanged", got, err)
		}
	})
}

func writeLegacySnapshotForMissingLeafProof(t *testing.T, store *Store, requested, canonical string) {
	t.Helper()
	turn := store.BeginTurn()
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(canonical))
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(canonical, filepath.Join(entryDir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"), SnapshotMeta{OriginalPath: requested, Existed: true}); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("turn = %d, want 1", turn)
	}
}

// Legacy Existed:false snapshots (no meta.CanonicalPath) must be refused
// at restore time — they carry no capture-time canonical witness, so the
// restore cannot prove the current path matches what was observed-as-missing.
func TestPR11Closure_LegacyExistedFalseDeletesAreRefused(t *testing.T) {
	t.Run("file present at OriginalPath", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		createdPath := filepath.Join(projectDir, "victim.txt")
		if err := os.WriteFile(createdPath, []byte("victim-content"), 0o644); err != nil {
			t.Fatal(err)
		}
		turn := store.BeginTurn()
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(createdPath)))
		if err := os.MkdirAll(entryDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(entryDir, "meta.json"),
			SnapshotMeta{OriginalPath: createdPath, Existed: false}); err != nil {
			t.Fatal(err)
		}
		if turn != 1 {
			t.Fatalf("turn = %d, want 1", turn)
		}

		_, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode succeeded for legacy Existed:false, want refusal")
		}
		if !strings.Contains(err.Error(), "legacy delete refused") {
			t.Fatalf("error %v does not mention legacy delete refused", err)
		}
		if got, err := os.ReadFile(createdPath); err != nil || string(got) != "victim-content" {
			t.Fatalf("file content = %q, %v; want unchanged", got, err)
		}
	})

	t.Run("target absent at OriginalPath", func(t *testing.T) {
		store := newTestStore(t)
		projectDir := t.TempDir()
		absentPath := filepath.Join(projectDir, "absent.txt")
		turn := store.BeginTurn()
		entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(absentPath)))
		if err := os.MkdirAll(entryDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(entryDir, "meta.json"),
			SnapshotMeta{OriginalPath: absentPath, Existed: false}); err != nil {
			t.Fatal(err)
		}
		if turn != 1 {
			t.Fatalf("turn = %d, want 1", turn)
		}

		_, err := store.RevertCode(0)
		if err == nil {
			t.Fatal("RevertCode succeeded for legacy Existed:false with absent target, want refusal")
		}
		if !strings.Contains(err.Error(), "legacy delete refused") {
			t.Fatalf("error %v does not mention legacy delete refused", err)
		}
	})
}

// A modern snapshot whose current on-disk leaf is a hardlink to an
// outside-boundary inode must be refused by restore so the FD-acquisition
// nlink check stops the mutation before it crosses the project boundary.
func TestPR11Closure_HardlinkRefusedOnSnapshotRestore(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "target.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.SnapshotResolved(turn, target, target); err != nil {
		t.Fatal(err)
	}

	// Replace target with a hardlink to an outside-boundary file.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, target); err != nil {
		t.Fatal(err)
	}

	_, err := store.RevertCode(0)
	if err == nil {
		t.Fatal("RevertCode succeeded with hardlinked restore target; want refusal")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hardlink") && !strings.Contains(msg, "link count") && !strings.Contains(msg, "nlink") {
		t.Fatalf("error %v does not mention hardlink/link count/nlink", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside-content" {
		t.Fatalf("outside file = %q, %v; want unchanged", got, err)
	}
}

// A modern Existed:false snapshot must refuse to delete a current hardlinked
// regular-file leaf. This closes the created-file delete branch, which does
// not go through restoreFile's OpenForWrite nlink check.
func TestPR11Closure_HardlinkRefusedOnSnapshotCreatedDelete(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created.txt")

	turn := store.BeginTurn()
	if err := store.Snapshot(turn, createdPath); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, createdPath); err != nil {
		t.Fatal(err)
	}

	_, err := store.RevertCode(0)
	if err == nil {
		t.Fatal("RevertCode succeeded deleting hardlinked created-file leaf; want refusal")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hardlink") && !strings.Contains(msg, "link count") && !strings.Contains(msg, "nlink") {
		t.Fatalf("error %v does not mention hardlink/link count/nlink", err)
	}
	if _, err := os.Lstat(createdPath); err != nil {
		t.Fatalf("created hardlink lstat = %v; want preserved", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside-content" {
		t.Fatalf("outside file = %q, %v; want unchanged", got, err)
	}
}

func TestPR11Closure_HardlinkedSymlinkRefusedOnSnapshotCreatedDelete(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	createdPath := filepath.Join(projectDir, "created-link")

	turn := store.BeginTurn()
	if err := store.Snapshot(turn, createdPath); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "outside-target.txt")
	if err := os.WriteFile(target, []byte("outside-target"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalLink := filepath.Join(t.TempDir(), "original-link")
	if err := os.Symlink(target, originalLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(originalLink, createdPath); err != nil {
		t.Fatal(err)
	}
	assertHardlinkedSymlink(t, originalLink)
	assertHardlinkedSymlink(t, createdPath)

	_, err := store.RevertCode(0)
	if err == nil {
		t.Fatal("RevertCode succeeded deleting hardlinked symlink created leaf; want refusal")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hardlink") && !strings.Contains(msg, "link count") && !strings.Contains(msg, "nlink") {
		t.Fatalf("error %v does not mention hardlink/link count/nlink", err)
	}
	if _, err := os.Lstat(originalLink); err != nil {
		t.Fatalf("original symlink lstat = %v; want preserved", err)
	}
	if _, err := os.Lstat(createdPath); err != nil {
		t.Fatalf("created hardlinked symlink lstat = %v; want preserved", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "outside-target" {
		t.Fatalf("target file = %q, %v; want unchanged", got, err)
	}
}

// A modern Existed:false snapshot must be bound to the entry directory ID
// (hashString(realPath)). A meta.json content rewrite alone that redirects
// CanonicalPath must be refused because hashString(new path) no longer
// matches the on-disk entry directory name.
func TestPR11Closure_ModernDeleteBoundToEntryID(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalPath := filepath.Join(projectDir, "created.txt")
	victimPath := filepath.Join(projectDir, "victim.txt")
	if err := os.WriteFile(victimPath, []byte("victim-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.Snapshot(turn, originalPath); err != nil {
		t.Fatal(err)
	}
	// store.Snapshot writes meta with CanonicalPath=originalPath, entryDir=hashString(originalPath).
	// Tamper meta.json to redirect CanonicalPath (and OriginalPath) to victimPath.
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(filepath.Clean(originalPath)))
	if _, err := os.Stat(entryDir); err != nil {
		t.Fatalf("expected snapshot entry dir at %s: %v", entryDir, err)
	}
	if err := writeJSON(filepath.Join(entryDir, "meta.json"),
		SnapshotMeta{OriginalPath: victimPath, CanonicalPath: victimPath, Existed: false}); err != nil {
		t.Fatal(err)
	}

	_, err := store.RevertCode(0)
	if err == nil {
		t.Fatal("RevertCode succeeded with tampered meta.json; want refusal")
	}
	if got, readErr := os.ReadFile(victimPath); readErr != nil || string(got) != "victim-content" {
		t.Fatalf("victim file = %q, %v; want unchanged", got, readErr)
	}
	_ = turn
}

// A modern Existed:true snapshot must be bound to the entry directory ID
// (hashString(realPath)). A meta.json content rewrite alone that redirects
// CanonicalPath must be refused because hashString(new path) no longer
// matches the on-disk entry directory name. Symmetric with the modern delete
// binding in TestPR11Closure_ModernDeleteBoundToEntryID.
func TestPR11Closure_ModernRestoreBoundToEntryID(t *testing.T) {
	store := newTestStore(t)
	projectDir := t.TempDir()
	originalPath := filepath.Join(projectDir, "original.txt")
	victimPath := filepath.Join(projectDir, "victim.txt")
	if err := os.WriteFile(originalPath, []byte("original-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victimPath, []byte("victim-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := store.BeginTurn()
	if err := store.Snapshot(turn, originalPath); err != nil {
		t.Fatal(err)
	}
	// Snapshot names the entry dir after hashString(EvalSymlinks(canonicalPath)),
	// so resolve symlinks before hashing in case TMPDIR itself is symlinked.
	realOriginal, err := filepath.EvalSymlinks(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	realVictim, err := filepath.EvalSymlinks(victimPath)
	if err != nil {
		t.Fatal(err)
	}
	entryDir := filepath.Join(store.snapshotsDir, "1", hashString(realOriginal))
	if _, err := os.Stat(entryDir); err != nil {
		t.Fatalf("expected snapshot entry dir at %s: %v", entryDir, err)
	}
	// Redirect meta to victimPath, but write its canonical-resolved form so
	// the tampered entry is self-consistent under ResolveFilePath. Without
	// the new entry-id binding, the redirect would slip through; with it,
	// hashString(realVictim) != entryID is the only line of defense.
	if err := writeJSON(filepath.Join(entryDir, "meta.json"),
		SnapshotMeta{OriginalPath: victimPath, CanonicalPath: realVictim, Existed: true}); err != nil {
		t.Fatal(err)
	}

	_, err = store.RevertCode(0)
	if err == nil {
		t.Fatal("RevertCode succeeded with tampered modern restore meta.json; want refusal")
	}
	if !strings.Contains(err.Error(), "modern restore refused") {
		t.Fatalf("error %v does not mention modern restore refusal", err)
	}
	if got, readErr := os.ReadFile(victimPath); readErr != nil || string(got) != "victim-content" {
		t.Fatalf("victim file = %q, %v; want unchanged", got, readErr)
	}
	_ = turn
}

func assertHardlinkedSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s mode = %s, want symlink", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s stat type = %T, want *syscall.Stat_t", path, info.Sys())
	}
	if st.Nlink <= 1 {
		t.Fatalf("%s symlink nlink = %d, want > 1", path, st.Nlink)
	}
}
