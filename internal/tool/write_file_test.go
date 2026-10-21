package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// execWrite runs one write through the exported Normalize* + Prepare* entries.
// An empty rootCanonical is bound from root inside the helper.
func execWrite(t *testing.T, root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (string, error) {
	t.Helper()
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return "", err
		}
	}
	norm, err := NormalizeWriteArgs(args)
	if err != nil {
		return "", err
	}
	call, err := PrepareWriteCall(root, rootCanonical, writeDirCanonical, opts, store, norm)
	if err != nil {
		return "", err
	}
	return call.Execute(context.Background())
}

func TestPreparedWriteCreatesNewFileAndParentDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "new.txt")

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":    path,
		"content": "created",
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Wrote "+path+".")
	assertFileContent(t, path, "created")
}

func TestPreparedWriteWritesContentExactlyIncludingTrailingNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "exact.txt")

	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
			"path":    path,
			"content": "no trailing newline",
		})
		testassert.NoError(t, err)
	}
	assertFileContent(t, path, "no trailing newline")

	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
			"path":    path,
			"content": "with trailing newline\n",
		})
		testassert.NoError(t, err)
	}
	assertFileContent(t, path, "with trailing newline\n")
}

func TestPreparedWriteWithSnapshotSnapshotsBeforeWrite(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	root := filepath.Dir(path)
	store := &recordingSnapshotStore{turn: 9, before: map[string]string{}}

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":    path,
		"content": "after",
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Wrote "+path+".")
	testassert.Equal(t, len(store.calls), 1)
	testassert.Require(t, store.calls[0].turn == 9 && store.calls[0].path == path, "snapshot call = %+v, want turn=9 path=%q", store.calls[0], path)
	testassert.Require(t, store.before[path] == "before", "snapshot saw %q, want pre-write content", store.before[path])
	assertFileContent(t, path, "after")
}

func TestPreparedWriteWithSnapshotUsesCanonicalTargetAndRequestedResult(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	target := filepath.Join(realDir, "file.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))
	linkDir := filepath.Join(root, "linked")
	testassert.NoError(t, os.Symlink(realDir, linkDir))
	requested := filepath.Join(linkDir, "file.txt")
	store := &recordingSnapshotStore{turn: 10, before: map[string]string{}}

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":    requested,
		"content": "after",
	})

	testassert.NoError(t, err)
	testassert.Equal(t, result, "Wrote "+requested+".")
	testassert.Equal(t, len(store.calls), 1)
	testassert.Require(t, store.calls[0].path == requested && store.calls[0].canonical == target, "snapshot call = %+v, want original=%q canonical=%q", store.calls[0], requested, target)
	testassert.Require(t, store.before[target] == "before", "snapshot saw %q, want pre-write content", store.before[target])
	assertFileContent(t, target, "after")
}
