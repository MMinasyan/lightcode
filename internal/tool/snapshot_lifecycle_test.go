package tool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// revertLifecycleGroup runs the real whole-invocation restore producer over
// the fixture's known group directory, as production does.
func revertLifecycleGroup(t *testing.T, store testCodeGroupStore) (snapshot.RevertResult, error) {
	t.Helper()
	return snapshot.RevertCodeGroups([]string{store.groupDir}, 0)
}

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
			wrapped := &recordingCodeGroupStore{
				testCodeGroupStore: store,
				onSnapshot: func(call int) {
					if call == 1 {
						tc.mutate(t, path)
					}
				},
			}

			_, err := execWrite(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{
				"path":    path,
				"content": "after",
			})

			testassert.NoError(t, err)
			assertFileContent(t, path, "after")
			affected, err := revertLifecycleGroup(t, store)
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
	affected, err := revertLifecycleGroup(t, store)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0, "RevertCode affected = %v, want no files", affected)
	assertFileContent(t, path, "before")
}

func TestEditFileRetainsSnapshotWhenTargetChangesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	store := newSnapshotLifecycleStore(t)
	wrapped := &recordingCodeGroupStore{
		testCodeGroupStore: store,
		onSnapshot: func(call int) {
			if call == 1 {
				testassert.NoError(t, os.Remove(path))
				testassert.NoError(t, os.WriteFile(path, []byte("external"), 0o644))
			}
		},
	}

	_, err := execEdit(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{
		"path":       path,
		"old_string": "external",
		"new_string": "after",
	})

	testassert.NoError(t, err)
	assertFileContent(t, path, "after")
	affected, err := revertLifecycleGroup(t, store)
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
	affected, err := revertLifecycleGroup(t, store)
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

	affected, err := revertLifecycleGroup(t, store)
	testassert.NoError(t, err)
	testassert.Require(t, len(affected.Restored) == 0 && len(affected.Skipped) == 1 && affected.Skipped[0].Path == path, "RevertCode restored = %v, want none; RevertCode skipped = %+v, want skipped %s", affected.Restored, affected, path)
	assertFileContent(t, path, "first")
}

// TestRetainedPartialMutationRestoresOrSkipsByProducedBytes: every producer
// over the real store — existing-file and created-file rows. A mutation
// whose full intended bytes are known (completed Write byte count, or a Sync
// error after them) keeps a restorable produced identity; an uncertain
// outcome (truncate failure) retains the preimage with nil identity and the
// restore skips.
func TestRetainedPartialMutationRestoresOrSkipsByProducedBytes(t *testing.T) {
	for _, tc := range producerMutationCases() {
		for _, step := range []mutationFailureStep{failAfterTruncate, failAfterWrite, failAfterSync} {
			t.Run(tc.name+"_"+string(step), func(t *testing.T) {
				runProducerMutationCase(t, tc, step)
			})
		}
	}
}

type mutationFailureStep string

const (
	failAfterTruncate mutationFailureStep = "truncate"
	failAfterWrite    mutationFailureStep = "write"
	failAfterSync     mutationFailureStep = "sync"
	// failShortWriteError delegates only a strict prefix and returns an
	// injected error; failShortWriteNil delegates the same strict prefix and
	// returns nil error. Both are incomplete-produced-byte stimuli.
	failShortWriteError mutationFailureStep = "short-write-error"
	failShortWriteNil   mutationFailureStep = "short-write-nil"
	// failForeignWriteError and failForeignSync delegate the full own write,
	// then write distinct foreign bytes before returning the injected Write
	// or Sync error.
	failForeignWriteError mutationFailureStep = "foreign-write-error"
	failForeignSync       mutationFailureStep = "foreign-sync-error"
)

// foreignMutationBytes are the distinct outside-editor bytes written after
// the mutation file's own full write completes.
var foreignMutationBytes = []byte("FOREIGN")

type injectedMutationError string

func (e injectedMutationError) Error() string { return string(e) }

var errInjectedMutation = injectedMutationError("injected mutation error")

type failingMutationFile struct {
	mutationFile
	step   mutationFailureStep
	target string
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
	switch f.step {
	case failShortWriteError, failShortWriteNil:
		n, err := f.mutationFile.Write(p[:len(p)/2])
		if f.step == failShortWriteError {
			return n, errInjectedMutation
		}
		return n, err
	case failForeignWriteError:
		n, err := f.mutationFile.Write(p)
		if err != nil {
			return n, err
		}
		if err := os.WriteFile(f.target, foreignMutationBytes, 0o644); err != nil {
			return n, err
		}
		return n, errInjectedMutation
	}
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
	if f.step == failForeignSync {
		if err := os.WriteFile(f.target, foreignMutationBytes, 0o644); err != nil {
			return err
		}
		return errInjectedMutation
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
	openWriteTargetForMutationFunc = func(absPath string) (mutationFile, bool, error) {
		f, existed, err := prevWrite(absPath)
		if err == nil && filepath.Clean(absPath) == target {
			f = &failingMutationFile{mutationFile: f, step: step, target: target}
		}
		return f, existed, err
	}
	openExistingMutationFile = func(absPath string, flag int) (mutationFile, error) {
		f, err := prevExisting(absPath, flag)
		if err == nil && filepath.Clean(absPath) == target {
			f = &failingMutationFile{mutationFile: f, step: step, target: target}
		}
		return f, err
	}
	t.Cleanup(func() {
		openWriteTargetForMutationFunc = prevWrite
		openExistingMutationFile = prevExisting
	})
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
