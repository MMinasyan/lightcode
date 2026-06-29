package tool

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileTrackerTrackRecordsReadIdentity(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	mtime := time.Unix(100, 0)
	setTrackerFileMtime(t, path, mtime)

	trackIdentityForPath(t, tracker, path, 1, 100)

	if !trackerHasRead(tracker, path) {
		t.Fatal("tracker has no read identity after TrackIdentity")
	}
	dup, record := isDuplicateForPath(t, tracker, path, 1, 100)
	if !dup {
		t.Fatal("IsDuplicate returned false for unchanged tracked identity")
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
		t.Fatalf("IsDuplicate returned true after file changed, record=%+v", record)
	}

	trackIdentityForPath(t, tracker, path, 1, 100)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after file deletion, record=%+v", record)
	}
}

func TestFileTrackerResetClearsDuplicateState(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	trackIdentityForPath(t, tracker, path, 1, 100)

	tracker.Reset()

	if trackerHasRead(tracker, path) {
		t.Fatal("tracker retained read identity after Reset")
	}
	if dup, record := isDuplicateForPath(t, tracker, path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after Reset, record=%+v", record)
	}
}

func TestFileTrackerConcurrentTrackAndDuplicateCheck(t *testing.T) {
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
		isDuplicateForPath(t, tracker, path, 1, 100)
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
		t.Fatalf("duplicate checks = %d, want %d", got, iterations)
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

func trackIdentityForPath(t *testing.T, tracker *FileTracker, path string, offset, limit int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker.TrackIdentity(path, offset, limit, FileIdentityFromFileInfoAndData(info, data))
}

func isDuplicateForPath(t *testing.T, tracker *FileTracker, path string, offset, limit int) (bool, ReadRecord) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return tracker.IsDuplicateIdentity(path, offset, limit, FileIdentity{})
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tracker.IsDuplicateIdentity(path, offset, limit, FileIdentity{})
	}
	return tracker.IsDuplicateIdentity(path, offset, limit, FileIdentityFromFileInfoAndData(info, data))
}

func trackerHasRead(tracker *FileTracker, path string) bool {
	for _, record := range tracker.Snapshot() {
		if record.Path == path {
			return true
		}
	}
	return false
}

func setTrackerFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
