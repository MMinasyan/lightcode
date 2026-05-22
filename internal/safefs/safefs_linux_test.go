//go:build linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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

func TestOpenForWriteRefusesFIFOLeaf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	f, _, err := OpenForWrite(path, 0o644)
	if err == nil {
		f.Close()
		t.Fatal("OpenForWrite succeeded on FIFO leaf")
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

func TestRemoveLeafRemovesEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLeaf(dir); err != nil {
		t.Fatalf("RemoveLeaf(empty dir) = %v, want nil", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty dir lstat err = %v, want not exist", err)
	}
}

func TestRemoveLeafRefusesNonEmptyDirectoryAndPreservesChild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonempty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(child, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLeaf(dir); err == nil {
		t.Fatal("RemoveLeaf(non-empty dir) = nil, want refusal")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("non-empty dir stat = %v, isDir = %v; want intact directory", err, err == nil && info.IsDir())
	}
	data, err := os.ReadFile(child)
	if err != nil || string(data) != "content" {
		t.Fatalf("non-empty dir child content = %q, %v; want unchanged", data, err)
	}
}
