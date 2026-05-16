package tool

import (
	"os"
	"sync"
	"time"
)

var statFile = os.Stat

// ReadRecord records a read_file call for deduplication and edit enforcement.
type ReadRecord struct {
	Path   string
	Offset int
	Limit  int
	Mtime  time.Time
}

// FileTracker tracks which files have been read and their mtimes.
// It is populated from conversation history on session load and
// updated by read_file executions.
type FileTracker struct {
	mu         sync.Mutex
	generation uint64
	reads      []ReadRecord         // ordered by time, earliest first
	mtimes     map[string]time.Time // path -> last read mtime
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{
		mtimes: make(map[string]time.Time),
	}
}

// Track records that a file was read at the given mtime.
func (t *FileTracker) Track(path string, offset, limit int) {
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()

	info, err := statFile(path)
	mtime := time.Time{}
	if err == nil {
		mtime = info.ModTime()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generation {
		return
	}
	t.mtimes[path] = mtime
	t.reads = append(t.reads, ReadRecord{
		Path:   path,
		Offset: offset,
		Limit:  limit,
		Mtime:  mtime,
	})
}

// UpdateAfterWrite refreshes the tracked mtime for a file that was already
// read-authorized. Writes must not create read authorization by themselves.
func (t *FileTracker) UpdateAfterWrite(path string) {
	t.mu.Lock()
	generation := t.generation
	if _, ok := t.mtimes[path]; !ok {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	info, err := statFile(path)
	if err != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.generation {
		return
	}
	if _, ok := t.mtimes[path]; !ok {
		return
	}
	t.mtimes[path] = info.ModTime()
}

// Reset clears all tracked reads. Call this when visible conversation history
// changes so hidden or old reads cannot authorize future edits.
func (t *FileTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.reads = nil
	t.mtimes = make(map[string]time.Time)
}

// IsDuplicate checks if path+offset+limit was already read AND
// the file hasn't changed on disk since that read.
func (t *FileTracker) IsDuplicate(path string, offset, limit int) (bool, ReadRecord) {
	info, err := statFile(path)
	if err != nil {
		return false, ReadRecord{}
	}
	currentMtime := info.ModTime()

	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.reads) - 1; i >= 0; i-- {
		r := t.reads[i]
		if r.Path == path && r.Offset == offset && r.Limit == limit {
			if r.Mtime.Equal(currentMtime) {
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
	mtime, ok := t.mtimes[path]
	return mtime, ok
}

// WasReadCheck checks if path was read and hasn't changed since.
// Returns nil if OK, or an error describing the problem.
func (t *FileTracker) WasReadCheck(path string) error {
	t.mu.Lock()
	lastReadMtime, wasRead := t.mtimes[path]
	t.mu.Unlock()
	if !wasRead {
		return &ReadRequiredError{Path: path}
	}
	info, err := statFile(path)
	if err != nil {
		return &FileChangedError{Path: path}
	}
	if !info.ModTime().Equal(lastReadMtime) {
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
			if _, exists := t.mtimes[m.Path]; !exists {
				t.mtimes[m.Path] = time.Time{} // zero = "was read, but not in this session"
			}
		}
	}
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
