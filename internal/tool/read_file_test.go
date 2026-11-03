package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func readFileTestFile(t *testing.T, name, content string) string {
	t.Helper()
	return readFileTestFileBytes(t, name, []byte(content))
}

func readFileTestFileBytes(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	testassert.NoError(t, os.WriteFile(path, data, 0o644))
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
	testassert.NoError(t, err)

	want := strings.Join([]string{
		"2\ttwo",
		"3\tthree",
		"(Showing lines 2-3 of 4.)",
	}, "\n")
	testassert.Equal(t, result, want)
}

func TestPreparedReadDefaultLimitAndFooter(t *testing.T) {
	path := readFileTestFile(t, "lines.txt", "one\ntwo\nthree")
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 2}, map[string]any{
		"path":   path,
		"offset": json.Number("-10"),
		"limit":  json.Number("0"),
	})
	testassert.NoError(t, err)

	want := strings.Join([]string{
		"1\tone",
		"2\ttwo",
		"(Showing lines 1-2 of 3. Use offset=3 to continue.)",
	}, "\n")
	testassert.Equal(t, result, want)
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
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "appears to be a binary file"), "Execute error = %v, want binary-file error", err)
		})
	}
}

func TestPreparedReadToleratesInvalidUTF8Text(t *testing.T) {
	path := readFileTestFileBytes(t, "invalid.txt", []byte{'o', 'k', 0xff, '\n', 'n', 'e', 'x', 't'})
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result, "1\tok") && strings.Contains(result, "2\tnext"), "Execute result = %q, want line-numbered fallback text", result)
}

func TestPreparedReadTruncatesLongLinesAndOutputBytes(t *testing.T) {
	path := readFileTestFile(t, "long.txt", "abcdef\nsecond line")
	root := filepath.Dir(path)

	result, err := execRead(t, root, "", config.ToolsConfig{
		ReadMaxLines:     500,
		ReadLineMaxChars: 3,
		MaxOutputBytes:   24,
	}, map[string]any{"path": path})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result, "1\tabc... [truncated]") && strings.Contains(result, "Output truncated at 24 bytes"), "Execute result = %q, want per-line truncation; Execute result = %q, want byte-cap truncation footer", result, result)
}
