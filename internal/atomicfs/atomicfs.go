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

// SyncFileFunc, when non-nil, replaces the temp-file sync inside Write and
// CreateExclusive; its return value is that sync's result. It exists so
// tests can inject a failure at the sync point without touching the
// filesystem. Nil in production.
var SyncFileFunc func(*os.File) error

// SyncDirFunc, when non-nil, replaces the directory sync performed by
// SyncDir; its return value is that sync's result. Nil in production.
var SyncDirFunc func(string) error

// ReleaseFunc, when non-nil, replaces the unlock performed by Lock.Release;
// its return value is that unlock's result. It exists so tests can inject a
// release failure without touching the filesystem. Nil in production;
// test-only.
var ReleaseFunc func(*Lock) error

// Write atomically replaces path with data. It writes a temp file in the
// destination directory, sets its mode, and renames it over path. The
// temp lives in the same directory so the rename is a same-filesystem
// atomic operation; on any failure the temp is removed and path is left
// unchanged. The temp is synced before the rename and the destination
// directory after it, so the replacement survives a crash. Write does not
// create the destination directory: it is a replace, so the directory must
// already exist. A record whose directory was concurrently removed
// therefore fails to publish rather than resurrecting the directory.
// Callers that create a record use CreateExclusive or create the directory
// explicitly first.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfs: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfs: write temp for %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfs: chmod temp for %s: %w", path, err)
	}
	if err := syncTemp(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfs: sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfs: close temp for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfs: rename temp onto %s: %w", path, err)
	}
	// The file is published and complete; a directory-sync failure cannot be
	// compensated by the caller, so report it and return success.
	if err := SyncDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
	}
	return nil
}

// CreateExclusive publishes data at path only when path does not already
// exist. It writes a temp file then hard-links it to path; if path already
// exists the link fails and CreateExclusive returns (false, nil) without
// touching the existing file. It returns (true, nil) when it created path.
// This lets a first-run creator never overwrite a file a concurrent
// process (or user) may have just written. Before it publishes anything it
// syncs every ancestor level of the destination directory, from the
// directory's parent upward: CreateExclusive is the only publisher that
// creates directories, and a record whose own entry is durable but whose
// directory chain is not is unreachable after a crash. The chain is synced
// unconditionally, because the caller may have created the directory
// itself first; a failure of any of those syncs returns the error with
// nothing published. The temp is synced before the link; after a successful
// link the destination directory is synced so the new entry survives a
// crash.
func CreateExclusive(path string, data []byte, perm os.FileMode) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("atomicfs: create dir for %s: %w", path, err)
	}
	// The destination directory's ancestors may have been created moments
	// ago, by this call's MkdirAll or by the caller itself; the entry naming
	// a directory is only durable once its parent is synced. Sync every
	// level from the directory's parent upward before anything is published,
	// so a crash cannot leave the new record unreachable. Unconditional:
	// the caller may have created the directory without syncing it.
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if err := SyncDir(d); err != nil {
			return false, fmt.Errorf("atomicfs: sync ancestor %s: %w", d, err)
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("atomicfs: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("atomicfs: write temp for %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("atomicfs: chmod temp for %s: %w", path, err)
	}
	if err := syncTemp(tmp); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("atomicfs: sync temp for %s: %w", path, err)
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
	// The file is published and complete; a directory-sync failure cannot be
	// compensated by the caller, so report it and return success.
	if err := SyncDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
	}
	return true, nil
}

// syncTemp syncs the temp file before it is published, so the written data
// is durable on disk before the rename or link that exposes it.
func syncTemp(f *os.File) error {
	if SyncFileFunc != nil {
		return SyncFileFunc(f)
	}
	return f.Sync()
}

// SyncDir syncs the directory that contains a published entry, making the
// rename or link that created the entry durable across a crash. It is
// exported because other packages publish records through the same
// protocol.
func SyncDir(dir string) error {
	if SyncDirFunc != nil {
		return SyncDirFunc(dir)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("atomicfs: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("atomicfs: sync dir %s: %w", dir, err)
	}
	return nil
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
	if ReleaseFunc != nil {
		return ReleaseFunc(l)
	}
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
