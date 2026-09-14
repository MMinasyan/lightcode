//go:build linux

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDirectoryOpensRealDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := OpenDirectory(root)
	if err != nil {
		t.Fatalf("OpenDirectory error = %v", err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

func TestOpenDirectoryOpensFilesystemRoot(t *testing.T) {
	f, err := OpenDirectory("/")
	if err != nil {
		t.Fatalf("OpenDirectory(/) error = %v", err)
	}
	defer f.Close()
	if _, err := f.ReadDir(-1); err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
}

func TestOpenDirectoryRefusesSymlinkLeafWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f, err := OpenDirectory(link)
	if err == nil {
		f.Close()
		t.Fatal("OpenDirectory followed a symlink leaf")
	}
}

func TestOpenDirectoryRefusesRegularFileLeaf(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenDirectory(file)
	if err == nil {
		f.Close()
		t.Fatal("OpenDirectory accepted a regular-file leaf")
	}
}

func TestOpenDirectoryRefusesMissingLeaf(t *testing.T) {
	if _, err := OpenDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("OpenDirectory accepted a missing leaf")
	}
}
