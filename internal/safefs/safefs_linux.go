//go:build linux

package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenExisting opens path without following any symlink in the parent
// components or final leaf.
func OpenExisting(path string, flag int) (*os.File, error) {
	parent, base, err := openParent(path, false, 0)
	if err != nil {
		return nil, err
	}
	defer closeFD(parent)
	fd, err := unix.Openat(parent, base, flag|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// OpenForWrite opens an existing regular file for replacement or creates a
// missing leaf. Missing parent directories are created through checked
// no-follow traversal. The returned bool reports whether the file existed.
func OpenForWrite(path string, perm os.FileMode) (*os.File, bool, error) {
	parent, base, err := openParent(path, true, 0o755)
	if err != nil {
		return nil, false, err
	}
	defer closeFD(parent)

	fd, err := unix.Openat(parent, base, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		if err := requireRegularFD(fd, path); err != nil {
			closeFD(fd)
			return nil, false, err
		}
		return os.NewFile(uintptr(fd), path), true, nil
	}
	if err != unix.ENOENT {
		return nil, false, &os.PathError{Op: "openat", Path: path, Err: err}
	}

	fd, err = unix.Openat(parent, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err == nil {
		return os.NewFile(uintptr(fd), path), false, nil
	}
	if err != unix.EEXIST {
		return nil, false, &os.PathError{Op: "openat", Path: path, Err: err}
	}

	fd, err = unix.Openat(parent, base, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	if err := requireRegularFD(fd, path); err != nil {
		closeFD(fd)
		return nil, false, err
	}
	return os.NewFile(uintptr(fd), path), true, nil
}

// RemoveLeaf removes only the final path entry. A symlink leaf is unlinked as
// a symlink; it is never followed.
func RemoveLeaf(path string) error {
	parent, base, err := openParent(path, false, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return err
	}
	defer closeFD(parent)
	if err := unix.Unlinkat(parent, base, 0); err != nil {
		if err == unix.EISDIR || err == unix.EPERM {
			if dirErr := unix.Unlinkat(parent, base, unix.AT_REMOVEDIR); dirErr != nil {
				return &os.PathError{Op: "unlinkat", Path: path, Err: dirErr}
			}
			return nil
		}
		return &os.PathError{Op: "unlinkat", Path: path, Err: err}
	}
	return nil
}

func openParent(path string, createDirs bool, dirPerm os.FileMode) (int, string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return -1, "", fmt.Errorf("safefs: path must be absolute: %s", path)
	}
	base := filepath.Base(clean)
	if base == "." || base == string(filepath.Separator) {
		return -1, "", fmt.Errorf("safefs: invalid path: %s", path)
	}

	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: string(filepath.Separator), Err: err}
	}

	dir := filepath.Dir(clean)
	parts := splitAbs(dir)
	current := string(filepath.Separator)
	for _, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err == unix.ENOENT && createDirs {
			if mkErr := unix.Mkdirat(fd, part, uint32(dirPerm.Perm())); mkErr != nil && mkErr != unix.EEXIST {
				closeFD(fd)
				return -1, "", &os.PathError{Op: "mkdirat", Path: filepath.Join(current, part), Err: mkErr}
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			closeFD(fd)
			return -1, "", &os.PathError{Op: "openat", Path: filepath.Join(current, part), Err: err}
		}
		closeFD(fd)
		fd = next
		current = filepath.Join(current, part)
	}

	return fd, base, nil
}

func closeFD(fd int) {
	_ = unix.Close(fd)
}

func requireRegularFD(fd int, path string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("safefs: non-regular file target: %s", path)
	}
	return nil
}

func splitAbs(path string) []string {
	path = filepath.Clean(path)
	path = strings.TrimPrefix(path, string(filepath.Separator))
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, string(filepath.Separator))
}
