package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
				testassert.NoError(t, os.Remove(path))
			},
		},
		{
			name: "recreated",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				testassert.NoError(t, os.Remove(path))
				testassert.NoError(t, os.WriteFile(path, []byte("external"), 0o644))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := snapshotLifecycleFile(t, root, "file.txt", "before")
			store := newSnapshotLifecycleStore(t)
			wrapped := &mutatingTransactionalSnapshotStore{
				testCodeGroupStore: store,
				mutate: func() {
					tc.mutate(t, path)
				},
			}

			_, err := execWrite(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{
				"path":    path,
				"content": "after",
			})

			testassert.NoError(t, err)
			assertFileContent(t, path, "after")
			affected, err := store.RevertCode(0)
			testassert.NoError(t, err)
			testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path, "RevertCode affected = %v, want restored %s", affected, path)
			assertFileContent(t, path, "before")
		})
	}
}

func TestEditFileDiscardsSnapshotWhenEditFailsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t)

	_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "after",
	})

	testassert.Require(t, err != nil, "edit_file succeeded, want missing match error")
	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0, "RevertCode affected = %v, want no files", affected)
	assertFileContent(t, path, "before")
}

func TestEditFileRetainsSnapshotWhenTargetChangesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t)
	wrapped := &mutatingTransactionalSnapshotStore{
		testCodeGroupStore: store,
		mutate: func() {
			testassert.NoError(t, os.Remove(path))
			testassert.NoError(t, os.WriteFile(path, []byte("external"), 0o644))
		},
	}

	_, err := execEdit(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{
		"path":       path,
		"old_string": "external",
		"new_string": "after",
	})

	testassert.NoError(t, err)
	assertFileContent(t, path, "after")
	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path, "RevertCode affected = %v, want restored %s", affected, path)
	assertFileContent(t, path, "before")
}

func TestFailedValidationDoesNotDiscardPreviouslyRetainedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t)

	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
			"path":    path,
			"content": "after",
		})
		testassert.NoError(t, err)
	}
	_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":       path,
		"old_string": "missing",
		"new_string": "other",
	})
	testassert.Require(t, err != nil, "edit_file succeeded, want missing match error")
	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path, "RevertCode affected = %v, want retained snapshot for %s", affected, path)
	assertFileContent(t, path, "before")
}

func TestWriteFileSameTurnSecondWriteControlsSnapshotIdentity(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t)

	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
			"path":    path,
			"content": "first",
		})
		testassert.NoError(t, err)
	}
	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
			"path":    path,
			"content": "second",
		})
		testassert.NoError(t, err)
	}
	testassert.NoError(t, os.WriteFile(path, []byte("first"), 0o644))

	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == path, "RevertCode restored = %v, want none; RevertCode skipped = %+v, want skipped %s", affected.Restored, affected, path)
	assertFileContent(t, path, "first")
}

func TestConcurrentSameTurnWritesKeepLatestSnapshotIdentity(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := &blockingRecordSnapshotStore{
		testCodeGroupStore: newSnapshotLifecycleStore(t),
		firstRecordStarted: make(chan struct{}),
		releaseFirstRecord: make(chan struct{}),
		secondLockAttempt:  make(chan struct{}),
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
			"path":    path,
			"content": "first",
		})
		firstDone <- err
	}()

	<-store.firstRecordStarted
	secondDone := make(chan error, 1)
	go func() {
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
			"path":    path,
			"content": "second",
		})
		secondDone <- err
	}()

	secondFinishedBeforeRelease := false
	select {
	case err := <-secondDone:
		testassert.NoError(t, err)
		secondFinishedBeforeRelease = true
	case <-store.secondLockAttempt:
	case <-time.After(time.Second):
		t.Fatal("second write neither completed nor reached the snapshot mutation lock")
	}

	close(store.releaseFirstRecord)
	testassert.NoError(t, <-firstDone)
	if !secondFinishedBeforeRelease {
		testassert.NoError(t, <-secondDone)
	}

	assertFileContent(t, path, "second")
	affected, err := store.RevertCode(0)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path && len(affected.Skipped) == 0, "RevertCode restored = %v, want %s; RevertCode skipped = %+v, want none", affected.Restored, path, affected.Skipped)
	assertFileContent(t, path, "before")
}

func TestRetainedPartialMutationRecordsCurrentIdentityAndReverts(t *testing.T) {
	steps := []mutationFailureStep{failAfterTruncate, failAfterWrite, failAfterSync}
	cases := []struct {
		name string
		run  func(t *testing.T, root, path string, store testCodeGroupStore) error
	}{
		{
			name: "immediate_write",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				t.Helper()
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "immediate_edit",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				t.Helper()
				_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
		{
			name: "apply_patch",
			run: func(t *testing.T, root, _ string, store testCodeGroupStore) error {
				t.Helper()
				input := applyPatchInput(t, "*** Update File: file.txt\n@@\n-before\n+after")
				_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
				return err
			},
		},
	}

	for _, tc := range cases {
		for _, step := range steps {
			t.Run(fmt.Sprintf("%s_%s", tc.name, step), func(t *testing.T) {
				root := t.TempDir()
				path := snapshotLifecycleFile(t, root, "file.txt", "before")
				store := newSnapshotLifecycleStore(t)
				withMutationFailure(t, path, step)

				err := tc.run(t, root, path, store)

				testassert.Require(t, err != nil, "%s with %s succeeded, want injected I/O failure", tc.name, step)
				affected, revErr := store.RevertCode(0)
				testassert.Require(t, revErr == nil, "RevertCode error = %v", revErr)
				testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path && len(affected.Skipped) == 0, "RevertCode affected = %+v, want restored %s; RevertCode skipped = %+v, want none", affected, path, affected.Skipped)
				assertFileContent(t, path, "before")
			})
		}
	}
}

func TestRetainedPartialCreatedFileMutationRecordsCurrentIdentityAndDeletesOnRevert(t *testing.T) {
	steps := []mutationFailureStep{failAfterTruncate, failAfterWrite, failAfterSync}
	cases := []struct {
		name string
		run  func(t *testing.T, root, path string, store testCodeGroupStore) error
	}{
		{
			name: "immediate_write_create",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				t.Helper()
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "apply_patch_add",
			run: func(t *testing.T, root, _ string, store testCodeGroupStore) error {
				t.Helper()
				input := applyPatchInput(t, "*** Add File: created.txt\n+after")
				_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
				return err
			},
		},
	}

	for _, tc := range cases {
		for _, step := range steps {
			t.Run(fmt.Sprintf("%s_%s", tc.name, step), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "created.txt")
				store := newSnapshotLifecycleStore(t)
				withMutationFailure(t, path, step)

				err := tc.run(t, root, path, store)

				testassert.Require(t, err != nil, "%s with %s succeeded, want injected I/O failure", tc.name, step)
				affected, revErr := store.RevertCode(0)
				testassert.Require(t, revErr == nil, "RevertCode error = %v", revErr)
				testassert.Require(t, len(affected.Restored) == 1 && affected.Restored[0] == path && len(affected.Skipped) == 0, "RevertCode affected = %+v, want restored %s; RevertCode skipped = %+v, want none", affected, path, affected.Skipped)
				{
					_, statErr := os.Lstat(path)
					testassert.Require(t, os.IsNotExist(statErr), "created file should be removed after RevertCode, stat err = %v", statErr)
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
	openWriteTargetForMutationFunc = func(absPath string) (mutationFile, bool, bool, error) {
		f, existed, started, err := prevWrite(absPath)
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
	testCodeGroupStore
	mutate func()
}

func (s *mutatingTransactionalSnapshotStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	entryID, created, err := s.testCodeGroupStore.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
	if err == nil && s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return entryID, created, err
}

type blockingRecordSnapshotStore struct {
	testCodeGroupStore
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
	return s.testCodeGroupStore.LockSnapshotMutation(turn, entryID)
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
	return s.testCodeGroupStore.RecordSnapshotContent(turn, entryID, content)
}

// newSnapshotLifecycleStore opens one real artifact code group with explicit
// group and turn identity for the snapshot-lifecycle rows.
func newSnapshotLifecycleStore(t *testing.T) testCodeGroupStore {
	t.Helper()
	return newCodeGroupStore(t, filepath.Join(t.TempDir(), "group"), 1)
}

func snapshotLifecycleFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	testassert.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
