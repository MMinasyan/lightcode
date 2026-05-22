package tool

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// ReadRecord records a read_file call for deduplication and edit enforcement.
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

// FileTracker tracks which files have been read and their mtimes.
// It is populated from conversation history on session load and
// updated by read_file executions.
type FileTracker struct {
	mu         sync.Mutex
	generation uint64
	reads      []ReadRecord            // ordered by time, earliest first
	identities map[string]FileIdentity // path -> last read identity
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{
		identities: make(map[string]FileIdentity),
	}
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
	t.identities[path] = identity
	t.reads = append(t.reads, ReadRecord{
		Path:     path,
		Offset:   offset,
		Limit:    limit,
		Identity: identity,
		Mtime:    identity.Mtime,
	})
}

// UpdateAfterWriteIdentity refreshes the tracked identity using metadata from
// the already-opened file after a successful write.
func (t *FileTracker) UpdateAfterWriteIdentity(path string, identity FileIdentity) {
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()
	t.updateAfterWriteIdentity(path, identity, generation)
}

func (t *FileTracker) updateAfterWriteIdentity(path string, identity FileIdentity, generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generation {
		return
	}
	if _, ok := t.identities[path]; !ok {
		return
	}
	t.identities[path] = identity
}

// Reset clears all tracked reads. Call this when visible conversation history
// changes so hidden or old reads cannot authorize future edits.
func (t *FileTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.reads = nil
	t.identities = make(map[string]FileIdentity)
}

// IsDuplicateMtime checks duplicate status against metadata from an already-opened file.
func (t *FileTracker) IsDuplicateMtime(path string, offset, limit int, currentMtime time.Time) (bool, ReadRecord) {
	return t.IsDuplicateIdentity(path, offset, limit, FileIdentity{Mtime: currentMtime})
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

// WasRead returns the mtime from the most recent read of path.
// If the file was never read, returns (zero, false).
func (t *FileTracker) WasRead(path string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	identity, ok := t.identities[path]
	return identity.Mtime, ok
}

func (t *FileTracker) HasRead(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.identities[path]
	return ok
}

// WasReadCheckMtime checks read authorization against metadata from an already-opened file.
func (t *FileTracker) WasReadCheckMtime(path string, currentMtime time.Time) error {
	return t.WasReadCheckIdentity(path, FileIdentity{Mtime: currentMtime})
}

// WasReadCheckIdentity checks read authorization against identity metadata from an already-opened file.
func (t *FileTracker) WasReadCheckIdentity(path string, current FileIdentity) error {
	t.mu.Lock()
	lastReadIdentity, wasRead := t.identities[path]
	t.mu.Unlock()
	if !wasRead {
		return &ReadRequiredError{Path: path}
	}
	if !identityMatches(lastReadIdentity, current) {
		return &FileChangedError{Path: path}
	}
	return nil
}

// PopulateFromMessages scans historical tool result messages and marks
// read_file paths as having been read. Since we don't have mtime in
// persisted messages, we mark them with a zero time, which means the
// "was read" check passes but "modified since" check always fails
// (forces a re-read on first edit after session load).
func (t *FileTracker) PopulateFromMessages(messages []PersistedMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range messages {
		if m.Role == "tool" && m.ToolName == "read_file" && m.Path != "" {
			if _, exists := t.identities[m.Path]; !exists {
				t.identities[m.Path] = FileIdentity{} // no identity = force reread
			}
		}
	}
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

// PersistedMessage is a simplified message for tracker population.
type PersistedMessage struct {
	Role     string
	ToolName string
	Path     string
}

// ReadRequiredError is returned when a file was not read before editing.
type ReadRequiredError struct {
	Path string
}

func (e *ReadRequiredError) Error() string {
	return "file " + e.Path + " has not been read yet. You must read it before editing or overwriting."
}

// FileChangedError is returned when a file was modified since last read.
type FileChangedError struct {
	Path string
}

func (e *FileChangedError) Error() string {
	return "file " + e.Path + " has been modified since your last read. Read it again before editing."
}
