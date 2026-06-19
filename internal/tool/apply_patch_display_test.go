package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

func TestApplyPatchDisplayMetadataEmitsEditPreviewFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta\n*** Delete File: a.txt")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}

	md := tool.DisplayMetadata(context.Background(), nil, "")
	files, ok := editpreview.FilesFromMetadata(md)
	if !ok {
		t.Fatalf("edit_preview_files not present in metadata: %+v", md)
	}
	if len(files) != 3 {
		t.Fatalf("files len = %d, want 3 (Add, Update, Delete)", len(files))
	}
	if files[0].Op != "A" || files[0].Path != "new.txt" {
		t.Fatalf("files[0] = %+v, want A new.txt", files[0])
	}
	if len(files[0].Preview.Hunks) != 1 || len(files[0].Preview.Hunks[0].Rows) != 1 {
		t.Fatalf("files[0].Preview = %+v, want one hunk with one add row", files[0].Preview)
	}
	if files[1].Op != "M" || files[1].Path != "x.txt" {
		t.Fatalf("files[1] = %+v, want M x.txt", files[1])
	}
	if len(files[1].Preview.Hunks) != 1 {
		t.Fatalf("files[1].Preview.Hunks len = %d, want 1", len(files[1].Preview.Hunks))
	}
	wantRows := []editpreview.Row{
		{Kind: editpreview.KindContext, OldLine: 1, NewLine: 1, Text: "alpha"},
		{Kind: editpreview.KindRemove, OldLine: 2, Text: "BEFORE"},
		{Kind: editpreview.KindAdd, NewLine: 2, Text: "AFTER"},
		{Kind: editpreview.KindContext, OldLine: 3, NewLine: 3, Text: "beta"},
	}
	if len(files[1].Preview.Hunks[0].Rows) != len(wantRows) {
		t.Fatalf("files[1] hunk rows len = %d, want %d", len(files[1].Preview.Hunks[0].Rows), len(wantRows))
	}
	for i, r := range files[1].Preview.Hunks[0].Rows {
		if r != wantRows[i] {
			t.Fatalf("files[1] hunk row[%d] = %+v, want %+v", i, r, wantRows[i])
		}
	}
	if files[2].Op != "D" || files[2].Path != "a.txt" {
		t.Fatalf("files[2] = %+v, want D a.txt", files[2])
	}
}

func TestApplyPatchDisplayMetadataMoveTwoEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	if _, err := tool.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}

	md := tool.DisplayMetadata(context.Background(), nil, "")
	files, ok := editpreview.FilesFromMetadata(md)
	if !ok {
		t.Fatalf("edit_preview_files not present: %+v", md)
	}
	if len(files) != 2 {
		t.Fatalf("files len = %d, want 2 (M new.go, D old.go)", len(files))
	}
	if files[0].Op != "M" || files[0].Path != "new.go" {
		t.Fatalf("files[0] = %+v, want M new.go", files[0])
	}
	if files[1].Op != "D" || files[1].Path != "old.go" {
		t.Fatalf("files[1] = %+v, want D old.go", files[1])
	}
}

func TestApplyPatchDisplayMetadataClearsStash(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi")
	if _, err := tool.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatal(err)
	}
	if md := tool.DisplayMetadata(context.Background(), nil, ""); md == nil {
		t.Fatal("first DisplayMetadata = nil, want metadata")
	}
	if md := tool.DisplayMetadata(context.Background(), nil, ""); md != nil {
		t.Fatalf("second DisplayMetadata = %+v, want nil (stash cleared)", md)
	}
}

func TestApplyPatchDisplayMetadataFailureNil(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Delete File: ghost.txt")
	if _, err := tool.Execute(context.Background(), map[string]any{"input": input}); err == nil {
		t.Fatal("Execute err = nil, want non-nil for delete-of-missing")
	}
	if md := tool.DisplayMetadata(context.Background(), nil, ""); md != nil {
		t.Fatalf("DisplayMetadata after failure = %+v, want nil", md)
	}
}

func TestEditPreviewFilesFromMetadataAbsent(t *testing.T) {
	if files, ok := editpreview.FilesFromMetadata(map[string]any{"other": 1}); ok || files != nil {
		t.Fatalf("got %v / %v, want nil / false", files, ok)
	}
}

func TestEditPreviewFilesFromMetadataWrongShape(t *testing.T) {
	if files, ok := editpreview.FilesFromMetadata(map[string]any{"edit_preview_files": "not an array"}); ok || files != nil {
		t.Fatalf("got %v / %v, want nil / false", files, ok)
	}
}
