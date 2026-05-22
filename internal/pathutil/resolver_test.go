package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFilePathPreservesMultipleMissingParents(t *testing.T) {
	realRoot := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, linkDir); err != nil {
		t.Fatal(err)
	}

	requested := filepath.Join(linkDir, "a", "b", "new.txt")
	resolved, err := ResolveFilePath(requested)
	if err != nil {
		t.Fatalf("ResolveFilePath error = %v", err)
	}

	want := filepath.Join(realRoot, "a", "b", "new.txt")
	if resolved.CanonicalPath != want {
		t.Fatalf("CanonicalPath = %q, want %q", resolved.CanonicalPath, want)
	}
	if resolved.LeafExists {
		t.Fatal("LeafExists = true, want false")
	}
}
