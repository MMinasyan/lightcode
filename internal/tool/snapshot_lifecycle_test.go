package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

func TestWriteFileDiscardsSnapshotWhenValidationFailsBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(t *testing.T, path string)
		assertPath func(t *testing.T, path string)
	}{
		{
			name: "deleted",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			assertPath: func(t *testing.T, path string) {
				t.Helper()
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("post-revert stat = %v, want file to remain absent", err)
				}
			},
		},
		{
			name: "recreated",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("external"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			assertPath: func(t *testing.T, path string) {
				t.Helper()
				assertFileContent(t, path, "external")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := snapshotLifecycleFile(t, root, "file.txt", "before")
			store := newSnapshotLifecycleStore(t, root)
			tracker := snapshotLifecycleTracker(t, path)
			wrapped := &mutatingTransactionalSnapshotStore{
				Store: store,
				mutate: func() {
					tc.mutate(t, path)
				},
			}

			_, err := NewWriteFileWithSnapshot(wrapped, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
				"path":    path,
				"content": "after",
			})

			var changedErr *FileChangedError
			if !errors.As(err, &changedErr) {
				t.Fatalf("write_file error = %T %v, want *FileChangedError", err, err)
			}
			affected, err := store.RevertCode(0)
			if err != nil {
				t.Fatalf("RevertCode error = %v, want no active snapshot entry", err)
			}
			if len(affected) != 0 {
				t.Fatalf("RevertCode affected = %v, want no files", affected)
			}
			tc.assertPath(t, path)
		})
	}
}

func TestEditFileDiscardsSnapshotWhenEditFailsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t, root)
	tracker := snapshotLifecycleTracker(t, path)

	_, err := NewEditFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "after",
	})

	if err == nil {
		t.Fatal("edit_file succeeded, want missing match error")
	}
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode error = %v, want no active snapshot entry", err)
	}
	if len(affected) != 0 {
		t.Fatalf("RevertCode affected = %v, want no files", affected)
	}
	assertFileContent(t, path, "before")
}

func TestEditFileDiscardsSnapshotWhenValidationFailsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t, root)
	tracker := snapshotLifecycleTracker(t, path)
	wrapped := &mutatingTransactionalSnapshotStore{
		Store: store,
		mutate: func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("external"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}

	_, err := NewEditFileWithSnapshot(wrapped, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "external",
		"new_string": "after",
	})

	var changedErr *FileChangedError
	if !errors.As(err, &changedErr) {
		t.Fatalf("edit_file error = %T %v, want *FileChangedError", err, err)
	}
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode error = %v, want no active snapshot entry", err)
	}
	if len(affected) != 0 {
		t.Fatalf("RevertCode affected = %v, want no files", affected)
	}
	assertFileContent(t, path, "external")
}

func TestFailedValidationDoesNotDiscardPreviouslyRetainedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t, root)
	tracker := snapshotLifecycleTracker(t, path)

	if _, err := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "after",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := NewEditFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "other",
	})
	if err == nil {
		t.Fatal("edit_file succeeded, want missing match error")
	}
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 1 || affected[0] != path {
		t.Fatalf("RevertCode affected = %v, want retained snapshot for %s", affected, path)
	}
	assertFileContent(t, path, "before")
}

func TestStagedExecutorDiscardsSnapshotWhenTargetChangesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	target := snapshotLifecycleFile(t, root, "target.txt", "before")
	secret := snapshotLifecycleFile(t, root, "secret.txt", "secret")
	alias := filepath.Join(root, "alias.txt")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	store := newSnapshotLifecycleStore(t, root)
	tracker := snapshotLifecycleTracker(t, target)
	wrapped := &mutatingTransactionalSnapshotStore{
		Store: store,
		mutate: func() {
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, alias); err != nil {
				t.Fatal(err)
			}
		},
	}

	results := NewStagedExecutor(wrapped, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
		stagedWrite(alias, "call-1", "after"),
	})

	assertStagedCanonicalChange(t, results, 0)
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode error = %v, want no active snapshot entry", err)
	}
	if len(affected) != 0 {
		t.Fatalf("RevertCode affected = %v, want no files", affected)
	}
	assertFileContent(t, target, "before")
	assertFileContent(t, secret, "secret")
}

type mutatingTransactionalSnapshotStore struct {
	*snapshot.Store
	mutate func()
}

func (s *mutatingTransactionalSnapshotStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	entryID, created, err := s.Store.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
	if err == nil && s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return entryID, created, err
}

func newSnapshotLifecycleStore(t *testing.T, projectRoot string) *snapshot.Store {
	t.Helper()
	store, err := snapshot.NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginNewSession(projectRoot); err != nil {
		t.Fatal(err)
	}
	if turn := store.BeginTurn(); turn != 1 {
		t.Fatalf("BeginTurn = %d, want 1", turn)
	}
	return store
}

func snapshotLifecycleFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func snapshotLifecycleTracker(t *testing.T, path string) *FileTracker {
	t.Helper()
	tracker := NewFileTracker()
	if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}
	return tracker
}
