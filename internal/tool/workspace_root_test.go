package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestFileToolsResolveRelativePathsFromWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	existing := filepath.Join(root, "existing.txt")
	testassert.NoError(t, os.WriteFile(existing, []byte("old\n"), 0o600))

	out, err := execRead(t, root, "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": "existing.txt"})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(out, "old"), "read output = %q, want root file content", out)

	{
		_, err := execEdit(t, root, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{
			"path":       "existing.txt",
			"old_string": "old",
			"new_string": "new",
		})
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(existing)
		testassert.Require(t, err == nil && string(got) == "new\n", "root existing.txt = %q, %v; want edited root file", got, err)
	}

	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"path": "created.txt", "content": "created"})
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(filepath.Join(root, "created.txt"))
		testassert.Require(t, err == nil && string(got) == "created", "root created.txt = %q, %v; want created root file", got, err)
	}
	{
		_, err := os.Stat(filepath.Join(other, "created.txt"))
		testassert.Require(t, os.IsNotExist(err), "created.txt was written relative to cwd; stat err = %v", err)
	}
}

func TestRunCommandUsesWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	out, err := RunForegroundCommand(context.Background(), "pwd", root, 2, 15360, 5000, t.TempDir(), os.Environ())
	testassert.NoError(t, err)
	testassert.Require(t, strings.TrimSpace(out) == root, "pwd = %q, want workspace root %q", out, root)
}
