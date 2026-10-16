package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

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

// execRead runs one read through the exported Normalize* + Prepare* entries.
// An empty rootCanonical is bound from root inside the helper.
func execRead(t *testing.T, root, rootCanonical string, cfg config.ToolsConfig, args map[string]any) (string, error) {
	t.Helper()
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return "", err
		}
	}
	norm, err := NormalizeReadArgs(args, cfg.ReadMaxLines)
	if err != nil {
		return "", err
	}
	call, err := PrepareReadCall(root, rootCanonical, cfg, norm)
	if err != nil {
		return "", err
	}
	return call.Execute(context.Background())
}

func TestPreparedReadLineRangesAndFooter(t *testing.T) {
	path := readFileTestFile(t, "lines.txt", "one\ntwo\nthree\nfour")
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{
		"path":   path,
		"offset": json.Number("2"),
		"limit":  json.Number("2"),
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

func TestPreparedReadDefaultLimitAndFooter(t *testing.T) {
	path := readFileTestFile(t, "lines.txt", "one\ntwo\nthree")
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 2}, map[string]any{
		"path":   path,
		"offset": json.Number("-10"),
		"limit":  json.Number("0"),
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

func TestPreparedReadRejectsBinaryFiles(t *testing.T) {
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
			root := filepath.Dir(path)

			_, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
			if err == nil || !strings.Contains(err.Error(), "appears to be a binary file") {
				t.Fatalf("Execute error = %v, want binary-file error", err)
			}
		})
	}
}

func TestPreparedReadToleratesInvalidUTF8Text(t *testing.T) {
	path := readFileTestFileBytes(t, "invalid.txt", []byte{'o', 'k', 0xff, '\n', 'n', 'e', 'x', 't'})
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "1\tok") || !strings.Contains(result, "2\tnext") {
		t.Fatalf("Execute result = %q, want line-numbered fallback text", result)
	}
}

func TestPreparedReadTruncatesLongLinesAndOutputBytes(t *testing.T) {
	path := readFileTestFile(t, "long.txt", "abcdef\nsecond line")
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{
		ReadMaxLines:     500,
		ReadLineMaxChars: 3,
		MaxOutputBytes:   24,
	}, map[string]any{"path": path})
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
