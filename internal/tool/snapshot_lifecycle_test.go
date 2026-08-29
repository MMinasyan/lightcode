package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

func TestWriteFileRetainsSnapshotWhenTargetChangesBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "deleted",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
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

			if err != nil {
				t.Fatalf("write_file error = %v", err)
			}
			assertFileContent(t, path, "after")
			affected, err := store.RevertCode(0)
			if err != nil {
				t.Fatalf("RevertCode error = %v", err)
			}
			if len(affected.Restored) != 1 || affected.Restored[0] != path {
				t.Fatalf("RevertCode affected = %v, want restored %s", affected, path)
			}
			assertFileContent(t, path, "before")
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
	if len(affected.Restored) != 0 {
		t.Fatalf("RevertCode affected = %v, want no files", affected)
	}
	assertFileContent(t, path, "before")
}

func TestEditFileRetainsSnapshotWhenTargetChangesBeforeMutation(t *testing.T) {
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

	if err != nil {
		t.Fatalf("edit_file error = %v", err)
	}
	assertFileContent(t, path, "after")
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode error = %v", err)
	}
	if len(affected.Restored) != 1 || affected.Restored[0] != path {
		t.Fatalf("RevertCode affected = %v, want restored %s", affected, path)
	}
	assertFileContent(t, path, "before")
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
	if len(affected.Restored) != 1 || affected.Restored[0] != path {
		t.Fatalf("RevertCode affected = %v, want retained snapshot for %s", affected, path)
	}
	assertFileContent(t, path, "before")
}

func TestWriteFileSameTurnSecondWriteControlsSnapshotIdentity(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t, root)
	tool := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{})

	if _, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "first",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "second",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 0 {
		t.Fatalf("RevertCode restored = %v, want none", affected.Restored)
	}
	if len(affected.Skipped) != 1 || affected.Skipped[0].Path != path {
		t.Fatalf("RevertCode skipped = %+v, want skipped %s", affected.Skipped, path)
	}
	assertFileContent(t, path, "first")
}

func TestConcurrentSameTurnWritesKeepLatestSnapshotIdentity(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := &blockingRecordSnapshotStore{
		Store:              newSnapshotLifecycleStore(t, root),
		firstRecordStarted: make(chan struct{}),
		releaseFirstRecord: make(chan struct{}),
		secondLockAttempt:  make(chan struct{}),
	}
	tool := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{})

	firstDone := make(chan error, 1)
	go func() {
		_, err := tool.Execute(context.Background(), map[string]any{
			"path":    path,
			"content": "first",
		})
		firstDone <- err
	}()

	<-store.firstRecordStarted
	secondDone := make(chan error, 1)
	go func() {
		_, err := tool.Execute(context.Background(), map[string]any{
			"path":    path,
			"content": "second",
		})
		secondDone <- err
	}()

	secondFinishedBeforeRelease := false
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second write before first release: %v", err)
		}
		secondFinishedBeforeRelease = true
	case <-store.secondLockAttempt:
	case <-time.After(time.Second):
		t.Fatal("second write neither completed nor reached the snapshot mutation lock")
	}

	close(store.releaseFirstRecord)
	if err := <-firstDone; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !secondFinishedBeforeRelease {
		if err := <-secondDone; err != nil {
			t.Fatalf("second write: %v", err)
		}
	}

	assertFileContent(t, path, "second")
	affected, err := store.RevertCode(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected.Restored) != 1 || affected.Restored[0] != path {
		t.Fatalf("RevertCode restored = %v, want %s", affected.Restored, path)
	}
	if len(affected.Skipped) != 0 {
		t.Fatalf("RevertCode skipped = %+v, want none", affected.Skipped)
	}
	assertFileContent(t, path, "before")
}

func TestRetainedPartialMutationRecordsCurrentIdentityAndReverts(t *testing.T) {
	steps := []mutationFailureStep{failAfterTruncate, failAfterWrite, failAfterSync}
	cases := []struct {
		name string
		run  func(t *testing.T, root, path string, store *snapshot.Store) error
	}{
		{
			name: "immediate_write",
			run: func(t *testing.T, _ string, path string, store *snapshot.Store) error {
				t.Helper()
				_, err := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "immediate_edit",
			run: func(t *testing.T, _ string, path string, store *snapshot.Store) error {
				t.Helper()
				_, err := NewEditFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
		{
			name: "apply_patch",
			run: func(t *testing.T, root, _ string, store *snapshot.Store) error {
				t.Helper()
				tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, root)
				input := applyPatchInput(t, "*** Update File: file.txt\n@@\n-before\n+after")
				_, err := runApplyPatch(t, tool, map[string]any{"input": input})
				return err
			},
		},
	}

	for _, tc := range cases {
		for _, step := range steps {
			t.Run(fmt.Sprintf("%s_%s", tc.name, step), func(t *testing.T) {
				root := t.TempDir()
				path := snapshotLifecycleFile(t, root, "file.txt", "before")
				store := newSnapshotLifecycleStore(t, root)
				withMutationFailure(t, path, step)

				err := tc.run(t, root, path, store)

				if err == nil {
					t.Fatalf("%s with %s succeeded, want injected I/O failure", tc.name, step)
				}
				affected, revErr := store.RevertCode(0)
				if revErr != nil {
					t.Fatalf("RevertCode error = %v", revErr)
				}
				if len(affected.Restored) != 1 || affected.Restored[0] != path {
					t.Fatalf("RevertCode affected = %+v, want restored %s", affected, path)
				}
				if len(affected.Skipped) != 0 {
					t.Fatalf("RevertCode skipped = %+v, want none", affected.Skipped)
				}
				assertFileContent(t, path, "before")
			})
		}
	}
}

func TestRetainedPartialCreatedFileMutationRecordsCurrentIdentityAndDeletesOnRevert(t *testing.T) {
	steps := []mutationFailureStep{failAfterTruncate, failAfterWrite, failAfterSync}
	cases := []struct {
		name string
		run  func(t *testing.T, root, path string, store *snapshot.Store) error
	}{
		{
			name: "immediate_write_create",
			run: func(t *testing.T, _ string, path string, store *snapshot.Store) error {
				t.Helper()
				_, err := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "apply_patch_add",
			run: func(t *testing.T, root, _ string, store *snapshot.Store) error {
				t.Helper()
				tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, root)
				input := applyPatchInput(t, "*** Add File: created.txt\n+after")
				_, err := runApplyPatch(t, tool, map[string]any{"input": input})
				return err
			},
		},
	}

	for _, tc := range cases {
		for _, step := range steps {
			t.Run(fmt.Sprintf("%s_%s", tc.name, step), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "created.txt")
				store := newSnapshotLifecycleStore(t, root)
				withMutationFailure(t, path, step)

				err := tc.run(t, root, path, store)

				if err == nil {
					t.Fatalf("%s with %s succeeded, want injected I/O failure", tc.name, step)
				}
				affected, revErr := store.RevertCode(0)
				if revErr != nil {
					t.Fatalf("RevertCode error = %v", revErr)
				}
				if len(affected.Restored) != 1 || affected.Restored[0] != path {
					t.Fatalf("RevertCode affected = %+v, want restored %s", affected, path)
				}
				if len(affected.Skipped) != 0 {
					t.Fatalf("RevertCode skipped = %+v, want none", affected.Skipped)
				}
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Fatalf("created file should be removed after RevertCode, stat err = %v", statErr)
				}
			})
		}
	}
}

type mutationFailureStep string

const (
	failAfterTruncate mutationFailureStep = "truncate"
	failAfterWrite    mutationFailureStep = "write"
	failAfterSync     mutationFailureStep = "sync"
)

type injectedMutationError string

func (e injectedMutationError) Error() string { return string(e) }

var errInjectedMutation = injectedMutationError("injected mutation error")

type failingMutationFile struct {
	mutationFile
	step mutationFailureStep
}

func (f *failingMutationFile) Truncate(size int64) error {
	err := f.mutationFile.Truncate(size)
	if err != nil {
		return err
	}
	if f.step == failAfterTruncate {
		return errInjectedMutation
	}
	return nil
}

func (f *failingMutationFile) Write(p []byte) (int, error) {
	n, err := f.mutationFile.Write(p)
	if err != nil {
		return n, err
	}
	if f.step == failAfterWrite {
		return n, errInjectedMutation
	}
	return n, nil
}

func (f *failingMutationFile) Sync() error {
	err := f.mutationFile.Sync()
	if err != nil {
		return err
	}
	if f.step == failAfterSync {
		return errInjectedMutation
	}
	return nil
}

func withMutationFailure(t *testing.T, target string, step mutationFailureStep) {
	t.Helper()
	target = filepath.Clean(target)
	prevWrite := openWriteTargetForMutationFunc
	prevExisting := openExistingMutationFile
	openWriteTargetForMutationFunc = func(absPath string, tracker *FileTracker) (mutationFile, bool, bool, error) {
		f, existed, started, err := prevWrite(absPath, tracker)
		if err == nil && filepath.Clean(absPath) == target {
			f = &failingMutationFile{mutationFile: f, step: step}
		}
		return f, existed, started, err
	}
	openExistingMutationFile = func(absPath string, flag int) (mutationFile, error) {
		f, err := prevExisting(absPath, flag)
		if err == nil && filepath.Clean(absPath) == target {
			f = &failingMutationFile{mutationFile: f, step: step}
		}
		return f, err
	}
	t.Cleanup(func() {
		openWriteTargetForMutationFunc = prevWrite
		openExistingMutationFile = prevExisting
	})
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

type blockingRecordSnapshotStore struct {
	*snapshot.Store
	mu                 sync.Mutex
	records            int
	lockAttempts       int
	firstRecordStarted chan struct{}
	releaseFirstRecord chan struct{}
	secondLockAttempt  chan struct{}
}

func (s *blockingRecordSnapshotStore) LockSnapshotMutation(turn int, entryID string) (func(), error) {
	s.mu.Lock()
	s.lockAttempts++
	attempt := s.lockAttempts
	s.mu.Unlock()

	if attempt == 2 {
		close(s.secondLockAttempt)
	}
	return s.Store.LockSnapshotMutation(turn, entryID)
}

func (s *blockingRecordSnapshotStore) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	s.mu.Lock()
	s.records++
	record := s.records
	s.mu.Unlock()

	if record == 1 {
		close(s.firstRecordStarted)
		<-s.releaseFirstRecord
	}
	return s.Store.RecordSnapshotContent(turn, entryID, content)
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
