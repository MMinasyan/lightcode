package tool

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileTrackerTrackRecordsReadAndAllowsUnchangedIdentity(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	mtime := time.Unix(100, 0)
	setTrackerFileMtime(t, path, mtime)

	trackIdentityForPath(t, tracker, path, 1, 100)

	if !tracker.HasRead(path) {
		t.Fatal("HasRead returned false after Track")
	}
	if err := wasReadCheckForPath(t, tracker, path); err != nil {
		t.Fatalf("WasReadCheck after unchanged Track = %v", err)
	}
}

func TestFileTrackerWasReadCheckRequiresPriorRead(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	err := wasReadCheckForPath(t, tracker, path)
	var readErr *ReadRequiredError
	if !errors.As(err, &readErr) {
		t.Fatalf("WasReadCheck error = %T %v, want *ReadRequiredError", err, err)
	}
	if readErr.Path != path {
		t.Fatalf("ReadRequiredError path = %q, want %q", readErr.Path, path)
	}
}

func TestFileTrackerWasReadCheckDetectsChangedFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	trackIdentityForPath(t, tracker, path, 1, 100)

	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))

	err := wasReadCheckForPath(t, tracker, path)
	var changedErr *FileChangedError
	if !errors.As(err, &changedErr) {
		t.Fatalf("WasReadCheck error = %T %v, want *FileChangedError", err, err)
	}
	if changedErr.Path != path {
		t.Fatalf("FileChangedError path = %q, want %q", changedErr.Path, path)
	}
}

func TestFileTrackerWasReadCheckDetectsDeletedFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	trackIdentityForPath(t, tracker, path, 1, 100)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	err := wasReadCheckForPath(t, tracker, path)
	var changedErr *FileChangedError
	if !errors.As(err, &changedErr) {
		t.Fatalf("WasReadCheck error = %T %v, want *FileChangedError", err, err)
	}
}

func TestFileTrackerUpdateAfterWriteIdentityDoesNotAuthorizeUnreadFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker.UpdateAfterWriteIdentity(path, FileIdentityFromFileInfo(info))

	if tracker.HasRead(path) {
		t.Fatal("UpdateAfterWriteIdentity created read authorization for unread file")
	}
	err = wasReadCheckForPath(t, tracker, path)
	var readErr *ReadRequiredError
	if !errors.As(err, &readErr) {
		t.Fatalf("WasReadCheck error = %T %v, want *ReadRequiredError", err, err)
	}
}

func TestFileTrackerIsDuplicateMatchesMostRecentSameRange(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	mtime := time.Unix(100, 0)
	setTrackerFileMtime(t, path, mtime)

	trackIdentityForPath(t, tracker, path, 1, 100)
	trackIdentityForPath(t, tracker, path, 2, 50)
	trackIdentityForPath(t, tracker, path, 1, 100)

	dup, record := isDuplicateForPath(t, tracker, path, 1, 100)
	if !dup {
		t.Fatal("IsDuplicate returned false for same path/range/mtime")
	}
	if record.Path != path || record.Offset != 1 || record.Limit != 100 {
		t.Fatalf("duplicate record = %+v, want path=%q offset=1 limit=100", record, path)
	}
	if !record.Mtime.Equal(mtime) {
		t.Fatalf("duplicate mtime = %v, want %v", record.Mtime, mtime)
	}
}

func TestFileTrackerIsDuplicateRejectsDifferentRange(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	trackIdentityForPath(t, tracker, path, 1, 100)

	if dup, record := isDuplicateForPath(t, tracker, path, 2, 100); dup {
		t.Fatalf("IsDuplicate returned true for different offset, record=%+v", record)
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 50); dup {
		t.Fatalf("IsDuplicate returned true for different limit, record=%+v", record)
	}
}

func TestFileTrackerIsDuplicateRejectsChangedOrDeletedFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	trackIdentityForPath(t, tracker, path, 1, 100)

	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))

	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after mtime changed, record=%+v", record)
	}

	trackIdentityForPath(t, tracker, path, 1, 100)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after file deletion, record=%+v", record)
	}
}

func TestFileTrackerResetClearsReadAndDuplicateState(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	trackIdentityForPath(t, tracker, path, 1, 100)

	tracker.Reset()

	if tracker.HasRead(path) {
		t.Fatal("HasRead returned true after Reset")
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after Reset, record=%+v", record)
	}
	err := wasReadCheckForPath(t, tracker, path)
	var readErr *ReadRequiredError
	if !errors.As(err, &readErr) {
		t.Fatalf("WasReadCheck error after Reset = %T %v, want *ReadRequiredError", err, err)
	}
}

func TestFileTrackerConcurrentTrackAndWasReadCheck(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	trackIdentityForPath(t, tracker, path, 1, 100)

	const iterations = 200
	var tracks int32
	var checks int32
	done := make(chan struct{})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	id := FileIdentityFromFileInfo(info)
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			tracker.TrackIdentity(path, i, 100, id)
			atomic.AddInt32(&tracks, 1)
		}
	}()

	for i := 0; i < iterations; i++ {
		if err := wasReadCheckForPath(t, tracker, path); err != nil {
			t.Fatalf("WasReadCheck during concurrent Track = %v", err)
		}
		atomic.AddInt32(&checks, 1)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Track goroutine did not complete")
	}
	if got := atomic.LoadInt32(&tracks); got != iterations {
		t.Fatalf("Track iterations = %d, want %d", got, iterations)
	}
	if got := atomic.LoadInt32(&checks); got != iterations {
		t.Fatalf("WasReadCheck iterations = %d, want %d", got, iterations)
	}
}

func testTrackerFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// trackIdentityForPath stat-reads the file and records the identity on
// the tracker. Mirrors what the production read_file path does after
// opening a descriptor.
func trackIdentityForPath(t *testing.T, tracker *FileTracker, path string, offset, limit int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker.TrackIdentity(path, offset, limit, FileIdentityFromFileInfo(info))
}

// isDuplicateForPath constructs the current on-disk identity for path
// and delegates to IsDuplicateIdentity. Returns false on stat/read
// failure (e.g. deleted file) so test callers that rely on the
// "deleted file is not a duplicate" semantic continue to work.
func isDuplicateForPath(t *testing.T, tracker *FileTracker, path string, offset, limit int) (bool, ReadRecord) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return false, ReadRecord{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, ReadRecord{}
	}
	return tracker.IsDuplicateIdentity(path, offset, limit, FileIdentityFromFileInfoAndData(info, data))
}

// wasReadCheckForPath constructs the current on-disk identity for path
// and delegates to WasReadCheckIdentity. On stat/read failure it passes
// an empty identity so a previously-read path produces
// FileChangedError, matching the semantic test callers depended on.
func wasReadCheckForPath(t *testing.T, tracker *FileTracker, path string) error {
	t.Helper()
	info, statErr := os.Stat(path)
	if statErr != nil {
		return tracker.WasReadCheckIdentity(path, FileIdentity{})
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return tracker.WasReadCheckIdentity(path, FileIdentity{})
	}
	return tracker.WasReadCheckIdentity(path, FileIdentityFromFileInfoAndData(info, data))
}

func setTrackerFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// Path-opening tracker helpers must not be exported. Production code uses
// the *Identity variants which take identity from an already-open
// descriptor instead of opening a path (and following any symlinks).
func TestPR11Closure_DeadHelpersRemoved(t *testing.T) {
	typ := reflect.TypeOf(&FileTracker{})
	for _, name := range []string{
		"Track", "TrackMtime",
		"UpdateAfterWrite", "UpdateAfterWriteMtime",
		"IsDuplicate", "WasReadCheck",
		"IsDuplicateMtime", "WasRead", "WasReadCheckMtime",
	} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("forbidden helper %s still exported on *FileTracker", name)
		}
	}
}
