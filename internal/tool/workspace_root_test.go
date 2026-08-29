package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestFileToolsResolveRelativePathsFromWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	existing := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existing, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tracker := NewFileTracker()
	read := NewReadFileAtRoot(config.ToolsConfig{ReadMaxLines: 500}, tracker, root)
	out, err := read.Execute(context.Background(), map[string]any{"path": "existing.txt"})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "old") {
		t.Fatalf("read output = %q, want root file content", out)
	}

	edit := NewEditFileAtRoot(tracker, config.ToolsConfig{}, root)
	if _, err := edit.Execute(context.Background(), map[string]any{
		"path":       "existing.txt",
		"old_string": "old",
		"new_string": "new",
	}); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "new\n" {
		t.Fatalf("root existing.txt = %q, %v; want edited root file", got, err)
	}

	write := NewWriteFileAtRoot(nil, config.ToolsConfig{}, root)
	if _, err := write.Execute(context.Background(), map[string]any{"path": "created.txt", "content": "created"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "created.txt")); err != nil || string(got) != "created" {
		t.Fatalf("root created.txt = %q, %v; want created root file", got, err)
	}
	if _, err := os.Stat(filepath.Join(other, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("created.txt was written relative to cwd; stat err = %v", err)
	}
}

func TestRunCommandUsesWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	cmd := NewRunCommandAtRoot(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), root, nil)
	out, err := cmd.Execute(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if strings.TrimSpace(out) != root {
		t.Fatalf("pwd = %q, want workspace root %q", out, root)
	}
}
