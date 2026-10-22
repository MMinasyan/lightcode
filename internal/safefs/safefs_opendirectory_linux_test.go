//go:build linux

package safefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestOpenDirectoryOpensRealDirectory(t *testing.T) {
	root := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644))
	testassert.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	f, err := OpenDirectory(root)
	testassert.NoError(t, err)
	defer f.Close()
	entries, err := f.ReadDir(-1)
	testassert.NoError(t, err)
	testassert.Equal(t, len(entries), 2)
}

func TestOpenDirectoryOpensFilesystemRoot(t *testing.T) {
	f, err := OpenDirectory("/")
	testassert.NoError(t, err)
	defer f.Close()
	{
		_, err := f.ReadDir(-1)
		testassert.NoError(t, err)
	}
}

func TestOpenDirectoryRefusesSymlinkLeafWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "real")
	testassert.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(root, "link")
	testassert.NoError(t, os.Symlink(real, link))
	f, err := OpenDirectory(link)
	if err == nil {
		f.Close()
		t.Fatal("OpenDirectory followed a symlink leaf")
	}
}

func TestOpenDirectoryRefusesRegularFileLeaf(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	testassert.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	f, err := OpenDirectory(file)
	if err == nil {
		f.Close()
		t.Fatal("OpenDirectory accepted a regular-file leaf")
	}
}

func TestOpenDirectoryRefusesMissingLeaf(t *testing.T) {
	{
		_, err := OpenDirectory(filepath.Join(t.TempDir(), "missing"))
		testassert.Require(t, err != nil, "OpenDirectory accepted a missing leaf")
	}
}
