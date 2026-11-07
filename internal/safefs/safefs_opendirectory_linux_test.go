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

func TestEnsureDirectoryCreatesOwnedTreeWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "extensions", "abc", "cache")
	testassert.NoError(t, EnsureDirectory(owned, 0o700))
	{
		info, err := os.Stat(owned)
		testassert.Require(t, err == nil && info.IsDir(), "EnsureDirectory(%s) = %v", owned, err)
	}
	// Existing real directories are idempotent.
	testassert.NoError(t, EnsureDirectory(owned, 0o700))

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	testassert.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))

	leafLink := filepath.Join(root, "leaf-link")
	testassert.NoError(t, os.Symlink(outside, leafLink))
	if err := EnsureDirectory(leafLink, 0o700); err == nil {
		t.Fatal("EnsureDirectory followed a symlink leaf")
	}
	ancestorLink := filepath.Join(root, "ancestor")
	testassert.NoError(t, os.Symlink(outside, ancestorLink))
	if err := EnsureDirectory(filepath.Join(ancestorLink, "child"), 0o700); err == nil {
		t.Fatal("EnsureDirectory followed a symlink ancestor")
	}
	{
		data, err := os.ReadFile(sentinel)
		testassert.Require(t, err == nil && string(data) == "keep", "outside sentinel = (%q, %v), want it untouched", data, err)
	}
	file := filepath.Join(root, "file")
	testassert.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	if err := EnsureDirectory(file, 0o700); err == nil {
		t.Fatal("EnsureDirectory accepted a regular-file leaf")
	}
}
