// Package atomicfs provides the shared publication protocol for
// ~/.lightcode state files: atomic replace, exclusive create, and a
// cross-process advisory lock. Readers of a file written through Write
// always observe either the old complete contents or the new complete
// contents, never a partial write. Records that are read-modify-write
// (permissions, managed env, project metadata) serialize their whole
// cycle under a lock so concurrent writers cannot lose an update.
package atomicfs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// Write atomically replaces path with data. It writes a temp file in the
// destination directory, sets its mode, and renames it over path. The
// temp lives in the same directory so the rename is a same-filesystem
// atomic operation; on any failure the temp is removed and path is left
// unchanged. Write does not create the destination directory: it is a
// replace, so the directory must already exist. A record whose directory
// was concurrently removed therefore fails to publish rather than
// resurrecting the directory. Callers that create a record use
// CreateExclusive or create the directory explicitly first.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfs: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomicfs: write temp for %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomicfs: chmod temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicfs: close temp for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicfs: rename temp onto %s: %w", path, err)
	}
	return nil
}

// CreateExclusive publishes data at path only when path does not already
// exist. It writes a temp file then hard-links it to path; if path already
// exists the link fails and CreateExclusive returns (false, nil) without
// touching the existing file. It returns (true, nil) when it created path.
// This lets a first-run creator never overwrite a file a concurrent
// process (or user) may have just written.
func CreateExclusive(path string, data []byte, perm os.FileMode) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("atomicfs: create dir for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("atomicfs: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Errorf("atomicfs: write temp for %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return false, fmt.Errorf("atomicfs: chmod temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("atomicfs: close temp for %s: %w", path, err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("atomicfs: link temp onto %s: %w", path, err)
	}
	return true, nil
}

// Lock is a held cross-process advisory lock backed by a .lock sidecar.
type Lock struct {
	fl *flock.Flock
}

// Acquire blocks until it holds an exclusive advisory lock at lockPath,
// creating the parent directory (0700). It is used to serialize the whole
// read-modify-write cycle of a shared record.
func Acquire(lockPath string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("atomicfs: create lock dir for %s: %w", lockPath, err)
	}
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("atomicfs: lock %s: %w", lockPath, err)
	}
	return &Lock{fl: fl}, nil
}

// TryAcquire attempts a non-blocking exclusive lock at lockPath. On
// contention it returns (nil, false, nil); the caller treats that as
// "another process holds it", not a failure.
func TryAcquire(lockPath string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, false, fmt.Errorf("atomicfs: create lock dir for %s: %w", lockPath, err)
	}
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("atomicfs: trylock %s: %w", lockPath, err)
	}
	if !locked {
		return nil, false, nil
	}
	return &Lock{fl: fl}, true, nil
}

// Release drops the lock and closes its file descriptor.
func (l *Lock) Release() error {
	if l == nil || l.fl == nil {
		return nil
	}
	return l.fl.Unlock()
}

// WithLock runs fn while holding the exclusive lock at lockPath, releasing
// it afterward. It is the standard wrapper for a serialized read-modify-write
// publication.
func WithLock(lockPath string, fn func() error) error {
	l, err := Acquire(lockPath)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}
