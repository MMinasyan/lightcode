package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspacePermissionDirID proves the retained storage-key derivation:
// the ID is p- plus the full lowercase SHA-256 hex of the cleaned absolute
// lexical path, identical for equivalent lexical forms, distinct for distinct
// paths, and independent of any project record or filesystem state.
func TestWorkspacePermissionDirID(t *testing.T) {
	got, err := WorkspacePermissionDirID("/tmp/demo")
	if err != nil {
		t.Fatalf("WorkspacePermissionDirID: %v", err)
	}
	if want := "p-84a8cd7d7a26dbdf6a21211d423355ec1989088651da3ce8fb577dc9d95ffae4"; got != want {
		t.Fatalf("ID = %q, want the pinned SHA-256 derivation %q", got, want)
	}

	for _, equivalent := range []string{"/tmp/demo/", "/tmp/./demo", "/tmp/../tmp/demo"} {
		same, err := WorkspacePermissionDirID(equivalent)
		if err != nil {
			t.Fatalf("WorkspacePermissionDirID(%q): %v", equivalent, err)
		}
		if same != got {
			t.Fatalf("ID(%q) = %q, want the same cleaned-absolute derivation %q", equivalent, same, got)
		}
	}

	other, err := WorkspacePermissionDirID("/tmp/other")
	if err != nil {
		t.Fatalf("WorkspacePermissionDirID(/tmp/other): %v", err)
	}
	if other == got || !strings.HasPrefix(other, "p-") || len(other) != len("p-")+64 {
		t.Fatalf("ID(/tmp/other) = %q, want a distinct p-<64 hex> storage key", other)
	}
}

// TestProjectsDir proves the inventory root stays home-based.
func TestProjectsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := ProjectsDir()
	if err != nil {
		t.Fatalf("ProjectsDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if want := filepath.Join(home, ".lightcode", "projects"); root != want {
		t.Fatalf("ProjectsDir = %q, want %q", root, want)
	}
}
