package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
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
	t.Run("hash match deletes clean original path", func(t *testing.T) {
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

		affected, err := store.RevertCode(0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(affected, []string{dirtyPath}) {
			t.Fatalf("affected = %v, want [%s]", affected, dirtyPath)
		}
		if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
			t.Fatalf("created file stat err = %v, want not exist", err)
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
