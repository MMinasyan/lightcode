package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestWriteFileDescriptionIsWellFormed(t *testing.T) {
	tool := NewWriteFile(NewFileTracker(), config.ToolsConfig{})
	desc := tool.Description()
	if strings.Contains(desc, "}") {
		t.Fatalf("write_file description contains a stray closing brace:\n%s", desc)
	}
	if !strings.Contains(desc, "Writes a file to disk.") {
		t.Fatalf("write_file description missing its lead line:\n%s", desc)
	}
}

func TestWriteFileCreatesNewFileAndParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "new.txt")
	tracker := NewFileTracker()
	tool := NewWriteFile(tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "created",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want original path in success message", result)
	}
	assertFileContent(t, path, "created")
	if trackerHasRead(tracker, path) {
		t.Fatal("write_file created a read record for a new file")
	}
}

func TestWriteFileOverwritesAfterRecentReadAndRefreshesTracker(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	tool := NewWriteFile(tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want original path in success message", result)
	}
	assertFileContent(t, path, "after")
	if !trackerHasRead(tracker, path) {
		t.Fatal("tracker lost the original read record")
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("tracker treated old read as duplicate after overwrite, record=%+v", record)
	}
}

func TestWriteFileOverwritesUnreadExistingFile(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	tool := NewWriteFile(NewFileTracker(), config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want success", result)
	}
	assertFileContent(t, path, "after")
}

func TestWriteFileOverwritesAfterExternalModification(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	if err := os.WriteFile(path, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))
	tool := NewWriteFile(tracker, config.ToolsConfig{})

	_, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	assertFileContent(t, path, "after")
}

func TestWriteFileWritesContentExactlyIncludingTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact.txt")
	tool := NewWriteFile(nil, config.ToolsConfig{})

	if _, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "no trailing newline",
	}); err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	assertFileContent(t, path, "no trailing newline")

	if _, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "with trailing newline\n",
	}); err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	assertFileContent(t, path, "with trailing newline\n")
}

func TestWriteFileWithSnapshotSnapshotsBeforeWrite(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	store := &recordingSnapshotStore{turn: 9, before: map[string]string{}}
	tool := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want original path in success message", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].turn != 9 || store.calls[0].path != path {
		t.Fatalf("snapshot call = %+v, want turn=9 path=%q", store.calls[0], path)
	}
	if store.before[path] != "before" {
		t.Fatalf("snapshot saw %q, want pre-write content", store.before[path])
	}
	assertFileContent(t, path, "after")
}

func TestWriteFileWithSnapshotUsesCanonicalTargetAndRequestedResult(t *testing.T) {
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
	trackIdentityForPath(t, tracker, target, 1, 100)
	store := &recordingSnapshotStore{turn: 10, before: map[string]string{}}
	tool := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{})

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":    requested,
		"content": "after",
	})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+requested+"." {
		t.Fatalf("Execute result = %q, want requested path in success message", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].path != requested || store.calls[0].canonical != target {
		t.Fatalf("snapshot call = %+v, want original=%q canonical=%q", store.calls[0], requested, target)
	}
	if store.before[target] != "before" {
		t.Fatalf("snapshot saw %q, want pre-write content", store.before[target])
	}
	assertFileContent(t, target, "after")
	if dup, record := isDuplicateForPath(t, tracker, target, 1, 100); dup {
		t.Fatalf("canonical target old read still duplicates after overwrite, record=%+v", record)
	}
}
