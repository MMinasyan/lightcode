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

func TestReadFileLineRangesAndFooter(t *testing.T) {
	path := readFileTestFile(t, "lines.txt", "one\ntwo\nthree\nfour")
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, NewFileTracker())

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":   path,
		"offset": float64(2),
		"limit":  float64(2),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}

	want := strings.Join([]string{
		"2\ttwo",
		"3\tthree",
		"(Showing lines 2-3 of 4.)",
	}, "\n")
	if result != want {
		t.Fatalf("read_file output = %q, want %q", result, want)
	}
}

func TestReadFileDefaultLimitAndInvalidPagination(t *testing.T) {
	path := readFileTestFile(t, "lines.txt", "one\ntwo\nthree")
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 2}, nil)

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":   path,
		"offset": float64(-10),
		"limit":  float64(0),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}

	want := strings.Join([]string{
		"1\tone",
		"2\ttwo",
		"(Showing lines 1-2 of 3. Use offset=3 to continue.)",
	}, "\n")
	if result != want {
		t.Fatalf("read_file output = %q, want %q", result, want)
	}
}

func TestReadFileTracksSuccessfulReadsAndDeduplicatesUnchangedRange(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "alpha\nbeta")
	mtime := time.Unix(100, 0)
	setTrackerFileMtime(t, path, mtime)
	tracker := NewFileTracker()
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker)

	result, err := tool.Execute(context.Background(), map[string]any{
		"path":   path,
		"offset": float64(1),
		"limit":  float64(2),
	})
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	if !strings.Contains(result, "1\talpha") {
		t.Fatalf("first Execute result = %q, want file content", result)
	}
	if err := wasReadCheckForPath(t, tracker, path); err != nil {
		t.Fatalf("WasReadCheck after read = %v", err)
	}

	result, err = tool.Execute(context.Background(), map[string]any{
		"path":   path,
		"offset": float64(1),
		"limit":  float64(2),
	})
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if result != "File unchanged since last read. The content from the earlier read in this conversation is still current." {
		t.Fatalf("second Execute result = %q, want dedup notice", result)
	}
}

func TestReadFileChangedFileBypassesDedupAndRetracks(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	tracker := NewFileTracker()
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker)

	if _, err := tool.Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))

	result, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if result != "1\tafter" {
		t.Fatalf("second Execute result = %q, want changed content", result)
	}
	if err := wasReadCheckForPath(t, tracker, path); err != nil {
		t.Fatalf("WasReadCheck after changed read = %v", err)
	}
}

func TestReadFileRejectsBinaryFiles(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "null byte", data: []byte{'a', 0, 'b'}},
		{name: "non printable ratio", data: []byte{1, 2, 3, 4, 'x'}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := readFileTestFileBytes(t, "binary.bin", tt.data)
			tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, NewFileTracker())

			_, err := tool.Execute(context.Background(), map[string]any{"path": path})
			if err == nil || !strings.Contains(err.Error(), "appears to be a binary file") {
				t.Fatalf("Execute error = %v, want binary-file error", err)
			}
		})
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, nil)

	_, err := tool.Execute(context.Background(), map[string]any{"path": dir})
	if err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("Execute error = %v, want non-regular target error", err)
	}
}

func TestReadFileToleratesInvalidUTF8Text(t *testing.T) {
	path := readFileTestFileBytes(t, "invalid.txt", []byte{'o', 'k', 0xff, '\n', 'n', 'e', 'x', 't'})
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, nil)

	result, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "1\tok") || !strings.Contains(result, "2\tnext") {
		t.Fatalf("Execute result = %q, want line-numbered fallback text", result)
	}
}

func TestReadFileTruncatesLongLinesAndOutputBytes(t *testing.T) {
	path := readFileTestFile(t, "long.txt", "abcdef\nsecond line")
	tool := NewReadFile(config.ToolsConfig{
		ReadMaxLines:     500,
		ReadLineMaxChars: 3,
		MaxOutputBytes:   24,
	}, nil)

	result, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "1\tabc... [truncated]") {
		t.Fatalf("Execute result = %q, want per-line truncation", result)
	}
	if !strings.Contains(result, "Output truncated at 24 bytes") {
		t.Fatalf("Execute result = %q, want byte-cap truncation footer", result)
	}
}

func TestReadFileFollowsSymlinkAndTracksRealPath(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(realPath, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker)

	result, err := tool.Execute(context.Background(), map[string]any{"path": linkPath})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "1\ttarget" {
		t.Fatalf("Execute result = %q, want symlink target content", result)
	}
	if err := wasReadCheckForPath(t, tracker, realPath); err != nil {
		t.Fatalf("real path WasReadCheck = %v", err)
	}
	var readErr *ReadRequiredError
	if err := wasReadCheckForPath(t, tracker, linkPath); !errors.As(err, &readErr) {
		t.Fatalf("link path WasReadCheck = %T %v, want *ReadRequiredError", err, err)
	}
}

func TestReadFileReturnsSuggestionsForMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.local.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, nil)

	result, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "confg.json")})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "read_file: file not found:") || !strings.Contains(result, "Did you mean:") || !strings.Contains(result, "config.json") {
		t.Fatalf("Execute result = %q, want not-found suggestions", result)
	}
}

func readFileTestFile(t *testing.T, name, content string) string {
	t.Helper()
	return readFileTestFileBytes(t, name, []byte(content))
}

func readFileTestFileBytes(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
