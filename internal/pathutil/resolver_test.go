package pathutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Syntheresis/melta/internal/testassert"
)

func TestResolveFilePathPreservesMultipleMissingParents(t *testing.T) {
	realRoot := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	testassert.NoError(t, os.Symlink(realRoot, linkDir))

	requested := filepath.Join(linkDir, "a", "b", "new.txt")
	resolved, err := ResolveFilePath(requested)
	testassert.NoError(t, err)

	want := filepath.Join(realRoot, "a", "b", "new.txt")
	testassert.Equal(t, resolved.CanonicalPath, want)
	testassert.Require(t, !resolved.LeafExists, "LeafExists = true, want false")
}
