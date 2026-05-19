//go:build linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingRefusesSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	f, err := OpenExisting(link, os.O_RDONLY)
	if err == nil {
		f.Close()
		t.Fatal("OpenExisting succeeded through symlink leaf")
	}
}

func TestOpenForWriteCreatesParentsAndRefusesSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	if f, _, err := OpenForWrite(filepath.Join(root, "a", "b", "file.txt"), 0o644); err != nil {
		t.Fatalf("OpenForWrite create parents: %v", err)
	} else {
		f.Close()
	}

	f, _, err := OpenForWrite(filepath.Join(linkDir, "file.txt"), 0o644)
	if err == nil {
		f.Close()
		t.Fatal("OpenForWrite succeeded through symlink parent")
	}
}

func TestRemoveLeafUnlinksSymlinkItself(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLeaf(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link lstat err = %v, want not exist", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "content" {
		t.Fatalf("target content = %q, %v; want unchanged", data, err)
	}
}
