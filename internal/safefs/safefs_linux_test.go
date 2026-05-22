//go:build linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// OpenExisting and OpenForWrite must refuse a leaf whose hardlink count
// > 1 so a hardlinked path inside the project cannot read or mutate an
// inode shared with locations outside the project boundary.
func TestPR11Closure_OpenExistingAndOpenForWriteRefuseHardlinkLeaf(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	hardlinked := filepath.Join(root, "hardlinked.txt")
	if err := os.Link(outside, hardlinked); err != nil {
		t.Fatal(err)
	}

	t.Run("OpenExisting", func(t *testing.T) {
		f, err := OpenExisting(hardlinked, os.O_RDONLY)
		if err == nil {
			f.Close()
			t.Fatal("OpenExisting succeeded on hardlinked leaf; want refusal")
		}
	})

	t.Run("OpenForWrite", func(t *testing.T) {
		f, _, err := OpenForWrite(hardlinked, 0o644)
		if err == nil {
			f.Close()
			t.Fatal("OpenForWrite succeeded on hardlinked leaf; want refusal")
		}
	})
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

func TestRemoveLeafRefusesHardlinkedRegularLeafAndPreservesLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	hardlinked := filepath.Join(root, "hardlinked.txt")
	if err := os.Link(outside, hardlinked); err != nil {
		t.Fatal(err)
	}

	err := RemoveLeaf(hardlinked)
	if err == nil {
		t.Fatal("RemoveLeaf succeeded on hardlinked regular leaf; want refusal")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hardlink") && !strings.Contains(msg, "link count") && !strings.Contains(msg, "nlink") {
		t.Fatalf("error %v does not mention hardlink/link count/nlink", err)
	}
	if _, err := os.Lstat(hardlinked); err != nil {
		t.Fatalf("hardlinked leaf lstat = %v; want preserved", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside-content" {
		t.Fatalf("outside file = %q, %v; want unchanged", got, err)
	}
}

func TestRemoveLeafRefusesHardlinkedSymlinkLeafAndPreservesLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("target-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	hardlinkedSymlink := filepath.Join(root, "hardlinked-link.txt")
	if err := os.Link(link, hardlinkedSymlink); err != nil {
		t.Fatal(err)
	}
	assertHardlinkedSymlink(t, link)
	assertHardlinkedSymlink(t, hardlinkedSymlink)

	err := RemoveLeaf(hardlinkedSymlink)
	if err == nil {
		t.Fatal("RemoveLeaf succeeded on hardlinked symlink leaf; want refusal")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hardlink") && !strings.Contains(msg, "link count") && !strings.Contains(msg, "nlink") {
		t.Fatalf("error %v does not mention hardlink/link count/nlink", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("original symlink lstat = %v; want preserved", err)
	}
	if _, err := os.Lstat(hardlinkedSymlink); err != nil {
		t.Fatalf("hardlinked symlink lstat = %v; want preserved", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "target-content" {
		t.Fatalf("target file = %q, %v; want unchanged", got, err)
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

func assertHardlinkedSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s mode = %s, want symlink", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s stat type = %T, want *syscall.Stat_t", path, info.Sys())
	}
	if st.Nlink <= 1 {
		t.Fatalf("%s symlink nlink = %d, want > 1", path, st.Nlink)
	}
}
