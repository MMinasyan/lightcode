package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestEditFileReplacesUniqueStringAndReportsLineRange(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "alpha\nbeta\ngamma")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	tool := NewEditFile(tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "beta",
		"new_string": "delta",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Edited "+path+" (1 replacement, lines 2-2)." {
		t.Fatalf("Execute result = %q, want one-replacement summary", result)
	}
	assertFileContent(t, path, "alpha\ndelta\ngamma")
	if err := tracker.WasReadCheck(path); err != nil {
		t.Fatalf("WasReadCheck after edit = %v", err)
	}
}

func TestEditFileReplaceAllReportsAllLineRanges(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "same\nother\nsame")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	tool := NewEditFile(tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":        path,
		"old_string":  "same",
		"new_string":  "done",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Edited "+path+" (2 replacements, lines 1-1, 3-3)." {
		t.Fatalf("Execute result = %q, want plural summary with both line ranges", result)
	}
	assertFileContent(t, path, "done\nother\ndone")
}

func TestEditFileRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "same\nsame")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	tool := NewEditFile(tracker, config.ToolsConfig{})

	_, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "same",
		"new_string": "done",
	})
	if err == nil || !strings.Contains(err.Error(), "old_string matches 2 locations") {
		t.Fatalf("Execute error = %v, want ambiguous-match error", err)
	}
	assertFileContent(t, path, "same\nsame")
}

func TestEditFileValidatesOldString(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "content")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	tool := NewEditFile(tracker, config.ToolsConfig{})

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
			_, err := tool.Execute(context.Background(), map[string]any{
				"path":       path,
				"old_string": tt.oldString,
				"new_string": tt.newString,
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Execute error = %v, want containing %q", err, tt.want)
			}
			assertFileContent(t, path, "content")
		})
	}
}

func TestEditFileRequiresReadBeforeEdit(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	tool := NewEditFile(NewFileTracker(), config.ToolsConfig{})

	_, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "before",
		"new_string": "after",
	})
	var readErr *ReadRequiredError
	if !errors.As(err, &readErr) {
		t.Fatalf("Execute error = %T %v, want *ReadRequiredError", err, err)
	}
	assertFileContent(t, path, "before")
}

func TestEditFileRejectsEditAfterExternalModification(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	if err := os.WriteFile(path, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))
	tool := NewEditFile(tracker, config.ToolsConfig{})

	_, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "external",
		"new_string": "after",
	})
	var changedErr *FileChangedError
	if !errors.As(err, &changedErr) {
		t.Fatalf("Execute error = %T %v, want *FileChangedError", err, err)
	}
	assertFileContent(t, path, "external")
}

func TestEditFileMultilineReplacementLineRange(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "start\nold\nend")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	tool := NewEditFile(tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "old",
		"new_string": "new\nline",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Edited "+path+" (1 replacement, lines 2-3)." {
		t.Fatalf("Execute result = %q, want multiline replacement range", result)
	}
	assertFileContent(t, path, "start\nnew\nline\nend")
}

func TestEditFileWithSnapshotSnapshotsBeforeEdit(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	store := &recordingSnapshotStore{turn: 11, before: map[string]string{}}
	tool := NewEditFileWithSnapshot(store, tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":       path,
		"old_string": "before",
		"new_string": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Edited "+path+" (1 replacement, lines 1-1)." {
		t.Fatalf("Execute result = %q, want edit summary", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].turn != 11 || store.calls[0].path != path {
		t.Fatalf("snapshot call = %+v, want turn=11 path=%q", store.calls[0], path)
	}
	if store.before[path] != "before" {
		t.Fatalf("snapshot saw %q, want pre-edit content", store.before[path])
	}
	assertFileContent(t, path, "after")
}

func TestEditFileWithSnapshotUsesCanonicalTargetAndRequestedResult(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	target := filepath.Join(realDir, "file.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(linkDir, "file.txt")
	tracker := NewFileTracker()
	tracker.Track(target, 1, 100)
	store := &recordingSnapshotStore{turn: 12, before: map[string]string{}}
	tool := NewEditFileWithSnapshot(store, tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":       requested,
		"old_string": "before",
		"new_string": "after",
	})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Edited "+requested+" (1 replacement, lines 1-1)." {
		t.Fatalf("Execute result = %q, want requested path in edit summary", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].path != requested || store.calls[0].canonical != target {
		t.Fatalf("snapshot call = %+v, want original=%q canonical=%q", store.calls[0], requested, target)
	}
	if store.before[target] != "before" {
		t.Fatalf("snapshot saw %q, want pre-edit content", store.before[target])
	}
	assertFileContent(t, target, "after")
}

func TestApplyEditAndApplyWritePureHelpers(t *testing.T) {
	edit, err := ApplyEdit("a\nb\na", "a", "z", true, "file.txt")
	if err != nil {
		t.Fatalf("ApplyEdit error = %v", err)
	}
	if edit.UpdatedContent != "z\nb\nz" || edit.Summary != "Edited file.txt (2 replacements, lines 1-1, 3-3)." || edit.LineRanges != "1-1, 3-3" || edit.Count != 2 {
		t.Fatalf("ApplyEdit result = %+v, want all replacements and line ranges", edit)
	}
	write := ApplyWrite("content", "file.txt")
	if write.UpdatedContent != "content" || write.Summary != "Wrote file.txt." {
		t.Fatalf("ApplyWrite result = %+v, want content and summary", write)
	}
}
