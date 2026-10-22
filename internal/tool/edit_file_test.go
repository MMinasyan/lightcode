package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// execEdit runs one edit through the exported Normalize* + Prepare* entries.
// An empty rootCanonical is bound from root inside the helper.
func execEdit(t *testing.T, root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (string, error) {
	t.Helper()
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return "", err
		}
	}
	norm, err := NormalizeEditArgs(args)
	if err != nil {
		return "", err
	}
	call, err := PrepareEditCall(root, rootCanonical, writeDirCanonical, opts, store, norm)
	if err != nil {
		return "", err
	}
	return call.Execute(context.Background())
}

func TestPreparedEditReplacesUniqueStringAndReportsLineRange(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "alpha\nbeta\ngamma")
	root := filepath.Dir(path)

	result, err := execEdit(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":       path,
		"old_string": "beta",
		"new_string": "delta",
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Edited "+path+" (1 replacement, lines 2-2).")
	assertFileContent(t, path, "alpha\ndelta\ngamma")
}

func TestPreparedEditReplaceAllReportsAllLineRanges(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "same\nother\nsame")
	root := filepath.Dir(path)

	result, err := execEdit(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":        path,
		"old_string":  "same",
		"new_string":  "done",
		"replace_all": true,
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Edited "+path+" (2 replacements, lines 1-1, 3-3).")
	assertFileContent(t, path, "done\nother\ndone")
}

func TestPreparedEditRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "same\nsame")
	root := filepath.Dir(path)

	_, err := execEdit(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":       path,
		"old_string": "same",
		"new_string": "done",
	})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "old_string matches 2 locations"), "Execute error = %v, want ambiguous-match error", err)
	assertFileContent(t, path, "same\nsame")
}

func TestPreparedEditValidatesOldString(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "content")
	root := filepath.Dir(path)

	tests := []struct {
		name      string
		oldString string
		newString string
		want      string
	}{
		{name: "empty", oldString: "", newString: "x", want: "old_string must not be empty"},
		{name: "identical", oldString: "content", newString: "content", want: "old_string and new_string are identical"},
		{name: "not found", oldString: "missing", newString: "x", want: "old_string not found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execEdit(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
				"path":       path,
				"old_string": tt.oldString,
				"new_string": tt.newString,
			})
			testassert.Require(t, err != nil && strings.Contains(err.Error(), tt.want), "Execute error = %v, want containing %q", err, tt.want)
			assertFileContent(t, path, "content")
		})
	}
}

func TestPreparedEditMultilineReplacementLineRange(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "start\nold\nend")
	root := filepath.Dir(path)

	result, err := execEdit(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":       path,
		"old_string": "old",
		"new_string": "new\nline",
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Edited "+path+" (1 replacement, lines 2-3).")
	assertFileContent(t, path, "start\nnew\nline\nend")
}

func TestPreparedEditWithSnapshotSnapshotsBeforeEdit(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	root := filepath.Dir(path)
	store := &recordingSnapshotStore{turn: 11, before: map[string]string{}}

	result, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":       path,
		"old_string": "before",
		"new_string": "after",
	})
	testassert.NoError(t, err)
	testassert.Equal(t, result, "Edited "+path+" (1 replacement, lines 1-1).")
	testassert.Equal(t, len(store.calls), 1)
	testassert.Require(t, store.calls[0].turn == 11 && store.calls[0].path == path, "snapshot call = %+v, want turn=11 path=%q", store.calls[0], path)
	testassert.Require(t, store.before[path] == "before", "snapshot saw %q, want pre-edit content", store.before[path])
	assertFileContent(t, path, "after")
}

func TestPreparedEditWithSnapshotUsesCanonicalTargetAndRequestedResult(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	target := filepath.Join(realDir, "file.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("before"), 0o644))
	linkDir := filepath.Join(root, "linked")
	testassert.NoError(t, os.Symlink(realDir, linkDir))
	requested := filepath.Join(linkDir, "file.txt")
	store := &recordingSnapshotStore{turn: 12, before: map[string]string{}}

	result, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":       requested,
		"old_string": "before",
		"new_string": "after",
	})

	testassert.NoError(t, err)
	testassert.Equal(t, result, "Edited "+requested+" (1 replacement, lines 1-1).")
	testassert.Equal(t, len(store.calls), 1)
	testassert.Require(t, store.calls[0].path == requested && store.calls[0].canonical == target, "snapshot call = %+v, want original=%q canonical=%q", store.calls[0], requested, target)
	testassert.Require(t, store.before[target] == "before", "snapshot saw %q, want pre-edit content", store.before[target])
	assertFileContent(t, target, "after")
}

func TestApplyEditPureHelper(t *testing.T) {
	edit, err := ApplyEdit("a\nb\na", "a", "z", true, "file.txt")
	testassert.NoError(t, err)
	testassert.Require(t, edit.UpdatedContent == "z\nb\nz" && edit.Summary == "Edited file.txt (2 replacements, lines 1-1, 3-3)." && edit.LineRanges == "1-1, 3-3" && edit.Count == 2, "ApplyEdit result = %+v, want all replacements and line ranges", edit)
}
