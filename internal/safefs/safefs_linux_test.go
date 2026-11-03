//go:build linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"golang.org/x/sys/unix"
)

func TestOpenExistingRefusesSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("content"), 0o644))
	testassert.NoError(t, os.Symlink(target, link))
	f, err := OpenExisting(link, os.O_RDONLY)
	if err == nil {
		f.Close()
		t.Fatal("OpenExisting succeeded through symlink leaf")
	}
}

func TestOpenForWriteCreatesParentsAndRefusesSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	testassert.NoError(t, os.Mkdir(realDir, 0o755))
	linkDir := filepath.Join(root, "link")
	testassert.NoError(t, os.Symlink(realDir, linkDir))

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

// A writerless FIFO must not block the leaf openat: the O_NONBLOCK open
// settles immediately and requireRegularFD rejects the leaf.
func TestOpenExistingWriterlessFIFORejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := OpenExisting(path, os.O_RDONLY)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		testassert.Require(t, err != nil, "OpenExisting succeeded on writerless FIFO leaf")
		testassert.Require(t, errors.Is(err, ErrNonRegular), "OpenExisting writerless FIFO err = %v, want ErrNonRegular", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OpenExisting blocked on writerless FIFO; want immediate ErrNonRegular")
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
	testassert.NoError(t, os.WriteFile(outside, []byte("outside-content"), 0o644))
	hardlinked := filepath.Join(root, "hardlinked.txt")
	testassert.NoError(t, os.Link(outside, hardlinked))

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
	testassert.NoError(t, os.WriteFile(target, []byte("content"), 0o644))
	testassert.NoError(t, os.Symlink(target, link))
	testassert.NoError(t, RemoveLeaf(link))
	{
		_, err := os.Lstat(link)
		testassert.Require(t, errors.Is(err, os.ErrNotExist), "link lstat err = %v, want not exist", err)
	}
	data, err := os.ReadFile(target)
	testassert.Require(t, err == nil && string(data) == "content", "target content = %q, %v; want unchanged", data, err)
}

func TestRemoveLeafRefusesHardlinkedRegularLeafAndPreservesLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	testassert.NoError(t, os.WriteFile(outside, []byte("outside-content"), 0o644))
	hardlinked := filepath.Join(root, "hardlinked.txt")
	testassert.NoError(t, os.Link(outside, hardlinked))

	err := RemoveLeaf(hardlinked)
	testassert.Require(t, err != nil, "RemoveLeaf succeeded on hardlinked regular leaf; want refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	{
		_, err := os.Lstat(hardlinked)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(outside)
		testassert.Require(t, err == nil && string(got) == "outside-content", "outside file = %q, %v; want unchanged", got, err)
	}
}

func TestRemoveLeafRefusesHardlinkedSymlinkLeafAndPreservesLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("target-content"), 0o644))
	link := filepath.Join(root, "link.txt")
	testassert.NoError(t, os.Symlink(target, link))
	hardlinkedSymlink := filepath.Join(root, "hardlinked-link.txt")
	testassert.NoError(t, os.Link(link, hardlinkedSymlink))
	assertHardlinkedSymlink(t, link)
	assertHardlinkedSymlink(t, hardlinkedSymlink)

	err := RemoveLeaf(hardlinkedSymlink)
	testassert.Require(t, err != nil, "RemoveLeaf succeeded on hardlinked symlink leaf; want refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	{
		_, err := os.Lstat(link)
		testassert.NoError(t, err)
	}
	{
		_, err := os.Lstat(hardlinkedSymlink)
		testassert.NoError(t, err)
	}
	{
		got, err := os.ReadFile(target)
		testassert.Require(t, err == nil && string(got) == "target-content", "target file = %q, %v; want unchanged", got, err)
	}
}

func TestRemoveLeafRemovesEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	testassert.NoError(t, os.Mkdir(dir, 0o755))
	testassert.NoError(t, RemoveLeaf(dir))
	{
		_, err := os.Lstat(dir)
		testassert.Require(t, errors.Is(err, os.ErrNotExist), "empty dir lstat err = %v, want not exist", err)
	}
}

func TestRemoveLeafRefusesNonEmptyDirectoryAndPreservesChild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonempty")
	testassert.NoError(t, os.Mkdir(dir, 0o755))
	child := filepath.Join(dir, "file.txt")
	testassert.NoError(t, os.WriteFile(child, []byte("content"), 0o644))
	{
		err := RemoveLeaf(dir)
		testassert.Require(t, err != nil, "RemoveLeaf(non-empty dir) = nil, want refusal")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("non-empty dir stat = %v, isDir = %v; want intact directory", err, err == nil && info.IsDir())
	}
	data, err := os.ReadFile(child)
	testassert.Require(t, err == nil && string(data) == "content", "non-empty dir child content = %q, %v; want unchanged", data, err)
}

func assertHardlinkedSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	testassert.Require(t, err == nil, "lstat %s: %v", path, err)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s mode = %s, want symlink", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s stat type = %T, want *syscall.Stat_t", path, info.Sys())
	}
	testassert.Require(t, st.Nlink > 1, "%s symlink nlink = %d, want > 1", path, st.Nlink)
}
