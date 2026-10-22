package tool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestApplyPatchPreviewMetadataEmitsEditPreviewFiles(t *testing.T) {
	dir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644))
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("bye"), 0o644))
	store := &applyPatchStore{turn: 1}

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta\n*** Delete File: a.txt")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.NoError(t, err)

	md := ApplyPatchPreviewMetadata(res.Previews)
	files, ok := editpreview.FilesFromMetadata(md)
	testassert.Require(t, ok, "edit_preview_files not present in metadata: %+v", md)
	testassert.Equal(t, len(files), 3)
	testassert.Require(t, files[0].Op == "A" && files[0].Path == "new.txt", "files[0] = %+v, want A new.txt", files[0])
	testassert.Require(t, len(files[0].Preview.Hunks) == 1 && len(files[0].Preview.Hunks[0].Rows) == 1, "files[0].Preview = %+v, want one hunk with one add row", files[0].Preview)
	testassert.Require(t, files[1].Op == "M" && files[1].Path == "x.txt", "files[1] = %+v, want M x.txt", files[1])
	testassert.Equal(t, len(files[1].Preview.Hunks), 1)
	wantRows := []editpreview.Row{
		{Kind: editpreview.KindContext, OldLine: 1, NewLine: 1, Text: "alpha"},
		{Kind: editpreview.KindRemove, OldLine: 2, Text: "BEFORE"},
		{Kind: editpreview.KindAdd, NewLine: 2, Text: "AFTER"},
		{Kind: editpreview.KindContext, OldLine: 3, NewLine: 3, Text: "beta"},
	}
	testassert.Equal(t, len(files[1].Preview.Hunks[0].Rows), len(wantRows))
	for i, r := range files[1].Preview.Hunks[0].Rows {
		testassert.Require(t, r == wantRows[i], "files[1] hunk row[%d] = %+v, want %+v", i, r, wantRows[i])
	}
	testassert.Require(t, files[2].Op == "D" && files[2].Path == "a.txt", "files[2] = %+v, want D a.txt", files[2])
}

func TestApplyPatchPreviewMetadataMoveTwoEntries(t *testing.T) {
	dir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "old.go"), []byte("a\nB\nc"), 0o644))
	store := &applyPatchStore{turn: 1}

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.NoError(t, err)

	md := ApplyPatchPreviewMetadata(res.Previews)
	files, ok := editpreview.FilesFromMetadata(md)
	testassert.Require(t, ok, "edit_preview_files not present: %+v", md)
	testassert.Equal(t, len(files), 2)
	testassert.Require(t, files[0].Op == "M" && files[0].Path == "new.go", "files[0] = %+v, want M new.go", files[0])
	testassert.Require(t, files[1].Op == "D" && files[1].Path == "old.go", "files[1] = %+v, want D old.go", files[1])
}

func TestApplyPatchFailureEmitsNoPreviewMetadata(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}

	input := applyPatchInput(t, "*** Delete File: ghost.txt")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil for delete-of-missing")
	{
		md := ApplyPatchPreviewMetadata(res.Previews)
		testassert.Require(t, md == nil, "metadata after failure = %+v, want nil", md)
	}
}

func TestEditPreviewFilesFromMetadataAbsent(t *testing.T) {
	{
		files, ok := editpreview.FilesFromMetadata(map[string]any{"other": 1})
		testassert.Require(t, !ok && files == nil, "got %v / %v, want nil / false", files, ok)
	}
}

func TestEditPreviewFilesFromMetadataWrongShape(t *testing.T) {
	{
		files, ok := editpreview.FilesFromMetadata(map[string]any{"edit_preview_files": "not an array"})
		testassert.Require(t, !ok && files == nil, "got %v / %v, want nil / false", files, ok)
	}
}
