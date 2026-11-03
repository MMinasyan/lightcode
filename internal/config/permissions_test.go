package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestWorkspacePermissionDirID proves the retained storage-key derivation:
// the ID is p- plus the full lowercase SHA-256 hex of the cleaned absolute
// lexical path, identical for equivalent lexical forms, distinct for distinct
// paths, and independent of any project record or filesystem state.
func TestWorkspacePermissionDirID(t *testing.T) {
	got, err := WorkspacePermissionDirID("/tmp/demo")
	testassert.NoError(t, err)
	{
		want := "p-84a8cd7d7a26dbdf6a21211d423355ec1989088651da3ce8fb577dc9d95ffae4"
		testassert.Equal(t, got, want)
	}

	for _, equivalent := range []string{"/tmp/demo/", "/tmp/./demo", "/tmp/../tmp/demo"} {
		same, err := WorkspacePermissionDirID(equivalent)
		testassert.Require(t, err == nil, "WorkspacePermissionDirID(%q): %v", equivalent, err)
		testassert.Require(t, same == got, "ID(%q) = %q, want the same cleaned-absolute derivation %q", equivalent, same, got)
	}

	other, err := WorkspacePermissionDirID("/tmp/other")
	testassert.NoError(t, err)
	testassert.Require(t, other != got && strings.HasPrefix(other, "p-") && len(other) == len("p-")+64, "ID(/tmp/other) = %q, want a distinct p-<64 hex> storage key", other)
}

// TestProjectsDir proves the inventory root stays home-based.
func TestProjectsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := ProjectsDir()
	testassert.NoError(t, err)
	home, err := os.UserHomeDir()
	testassert.NoError(t, err)
	{
		want := filepath.Join(home, ".lightcode", "projects")
		testassert.Equal(t, root, want)
	}
}
