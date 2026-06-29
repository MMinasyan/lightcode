package tool

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// ReadRecord records a read_file call for deduplication.
type ReadRecord struct {
	Path     string
	Offset   int
	Limit    int
	Identity FileIdentity
	Mtime    time.Time
}

// FileIdentity records the kernel identity of a file observed through an
// already-open descriptor.
type FileIdentity struct {
	Mtime   time.Time
	Ctime   time.Time
	Dev     uint64
	Ino     uint64
	Mode    os.FileMode
	Hash    [32]byte
	Valid   bool
	HasHash bool
}

// FileTracker tracks read_file observations for duplicate suppression.
type FileTracker struct {
	mu         sync.Mutex
	generation uint64
	reads      []ReadRecord // ordered by time, earliest first
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{}
}

// TrackIdentity records a read using identity metadata from the already-opened file.
func (t *FileTracker) TrackIdentity(path string, offset, limit int, identity FileIdentity) {
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()
	t.trackIdentity(path, offset, limit, identity, generation)
}

func (t *FileTracker) trackIdentity(path string, offset, limit int, identity FileIdentity, generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generation {
		return
	}
	t.reads = append(t.reads, ReadRecord{
		Path:     path,
		Offset:   offset,
		Limit:    limit,
		Identity: identity,
		Mtime:    identity.Mtime,
	})
}

// Reset clears all tracked reads. Call this when visible conversation history
// changes so hidden or old reads cannot authorize future edits.
func (t *FileTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.reads = nil
}

// Snapshot returns a copy of the tracked read state.
func (t *FileTracker) Snapshot() []ReadRecord {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ReadRecord(nil), t.reads...)
}

// Restore resets the tracker to a prior read snapshot.
func (t *FileTracker) Restore(reads []ReadRecord) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.reads = append([]ReadRecord(nil), reads...)
}

// IsDuplicateIdentity checks duplicate status against identity metadata from an already-opened file.
func (t *FileTracker) IsDuplicateIdentity(path string, offset, limit int, current FileIdentity) (bool, ReadRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.reads) - 1; i >= 0; i-- {
		r := t.reads[i]
		if r.Path == path && r.Offset == offset && r.Limit == limit {
			if identityMatches(r.Identity, current) {
				return true, r
			}
			return false, ReadRecord{}
		}
	}
	return false, ReadRecord{}
}

func FileIdentityFromFileInfo(info os.FileInfo) FileIdentity {
	if info == nil {
		return FileIdentity{}
	}
	identity := FileIdentity{
		Mtime: info.ModTime(),
		Mode:  info.Mode(),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity.Dev = uint64(stat.Dev)
		identity.Ino = uint64(stat.Ino)
		identity.Ctime = time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
		identity.Valid = true
	}
	return identity
}

func FileIdentityFromFileInfoAndData(info os.FileInfo, data []byte) FileIdentity {
	identity := FileIdentityFromFileInfo(info)
	identity.Hash = sha256.Sum256(data)
	identity.HasHash = true
	return identity
}

func FileIdentityFromOpenFile(f *os.File, info os.FileInfo) (FileIdentity, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return FileIdentity{}, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return FileIdentity{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentityFromFileInfoAndData(info, data), nil
}

func identityMatches(expected, current FileIdentity) bool {
	if expected.Valid || current.Valid {
		if !(expected.Valid &&
			current.Valid &&
			expected.Dev == current.Dev &&
			expected.Ino == current.Ino &&
			expected.Mode == current.Mode &&
			expected.Mtime.Equal(current.Mtime) &&
			expected.Ctime.Equal(current.Ctime) &&
			current.Mode.IsRegular()) {
			return false
		}
		if expected.HasHash {
			return current.HasHash && expected.Hash == current.Hash
		}
		return true
	}
	return expected.Mtime.Equal(current.Mtime)
}
