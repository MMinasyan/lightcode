package tool

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileTrackerTrackRecordsReadAndMtime(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	mtime := time.Unix(100, 0)
	setTrackerFileMtime(t, path, mtime)

	tracker.Track(path, 1, 100)

	got, ok := tracker.WasRead(path)
	if !ok {
		t.Fatal("WasRead returned false after Track")
	}
	if !got.Equal(mtime) {
		t.Fatalf("WasRead mtime = %v, want %v", got, mtime)
	}
	if err := tracker.WasReadCheck(path); err != nil {
		t.Fatalf("WasReadCheck after unchanged Track = %v", err)
	}
}

func TestFileTrackerWasReadCheckRequiresPriorRead(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	err := tracker.WasReadCheck(path)
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
	tracker.Track(path, 1, 100)

	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))

	err := tracker.WasReadCheck(path)
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
	tracker.Track(path, 1, 100)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	err := tracker.WasReadCheck(path)
	var changedErr *FileChangedError
	if !errors.As(err, &changedErr) {
		t.Fatalf("WasReadCheck error = %T %v, want *FileChangedError", err, err)
	}
}

func TestFileTrackerUpdateAfterWriteRefreshesTrackedMtime(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "before")
	initialMtime := time.Unix(100, 0)
	updatedMtime := time.Unix(200, 0)
	setTrackerFileMtime(t, path, initialMtime)
	tracker.Track(path, 1, 100)

	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, updatedMtime)

	tracker.UpdateAfterWrite(path)

	got, ok := tracker.WasRead(path)
	if !ok {
		t.Fatal("WasRead returned false after UpdateAfterWrite")
	}
	if !got.Equal(updatedMtime) {
		t.Fatalf("mtime after UpdateAfterWrite = %v, want %v", got, updatedMtime)
	}
	if err := tracker.WasReadCheck(path); err != nil {
		t.Fatalf("WasReadCheck after UpdateAfterWrite = %v", err)
	}
}

func TestFileTrackerUpdateAfterWriteDoesNotAuthorizeUnreadFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	tracker.UpdateAfterWrite(path)

	if _, ok := tracker.WasRead(path); ok {
		t.Fatal("UpdateAfterWrite created read authorization for unread file")
	}
	err := tracker.WasReadCheck(path)
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

	tracker.Track(path, 1, 100)
	tracker.Track(path, 2, 50)
	tracker.Track(path, 1, 100)

	dup, record := tracker.IsDuplicate(path, 1, 100)
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
	tracker.Track(path, 1, 100)

	if dup, record := tracker.IsDuplicate(path, 2, 100); dup {
		t.Fatalf("IsDuplicate returned true for different offset, record=%+v", record)
	}
	if dup, record := tracker.IsDuplicate(path, 1, 50); dup {
		t.Fatalf("IsDuplicate returned true for different limit, record=%+v", record)
	}
}

func TestFileTrackerIsDuplicateRejectsChangedOrDeletedFile(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "before")
	setTrackerFileMtime(t, path, time.Unix(100, 0))
	tracker.Track(path, 1, 100)

	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(200, 0))

	if dup, record := tracker.IsDuplicate(path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after mtime changed, record=%+v", record)
	}

	tracker.Track(path, 1, 100)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if dup, record := tracker.IsDuplicate(path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after file deletion, record=%+v", record)
	}
}

func TestFileTrackerResetClearsReadAndDuplicateState(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	tracker.Track(path, 1, 100)

	tracker.Reset()

	if _, ok := tracker.WasRead(path); ok {
		t.Fatal("WasRead returned true after Reset")
	}
	if dup, record := tracker.IsDuplicate(path, 1, 100); dup {
		t.Fatalf("IsDuplicate returned true after Reset, record=%+v", record)
	}
	err := tracker.WasReadCheck(path)
	var readErr *ReadRequiredError
	if !errors.As(err, &readErr) {
		t.Fatalf("WasReadCheck error after Reset = %T %v, want *ReadRequiredError", err, err)
	}
}

func TestFileTrackerConcurrentTrackAndWasReadCheck(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")
	tracker.Track(path, 1, 100)

	const iterations = 200
	var tracks int32
	var checks int32
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			tracker.Track(path, i, 100)
			atomic.AddInt32(&tracks, 1)
		}
	}()

	for i := 0; i < iterations; i++ {
		if err := tracker.WasReadCheck(path); err != nil {
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

func TestTrackDoesNotHoldLockWhileStatPending(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	origStatFile := statFile
	statStarted := make(chan struct{})
	releaseStat := make(chan struct{})
	statDone := make(chan struct{})
	statFile = func(name string) (os.FileInfo, error) {
		close(statStarted)
		<-releaseStat
		info, err := origStatFile(name)
		close(statDone)
		return info, err
	}
	t.Cleanup(func() {
		statFile = origStatFile
	})

	trackDone := make(chan struct{})
	go func() {
		tracker.Track(path, 1, 100)
		close(trackDone)
	}()

	<-statStarted

	resetDone := make(chan struct{})
	go func() {
		tracker.Reset()
		close(resetDone)
	}()

	select {
	case <-resetDone:
	case <-time.After(200 * time.Millisecond):
		close(releaseStat)
		<-statDone
		t.Fatal("Reset blocked while Track was waiting in os.Stat")
	}

	close(releaseStat)
	<-trackDone
}

func TestTrackDoesNotRecordReadStartedBeforeReset(t *testing.T) {
	tracker := NewFileTracker()
	path := testTrackerFile(t, "content")

	origStatFile := statFile
	statStarted := make(chan struct{})
	releaseStat := make(chan struct{})
	statFile = func(name string) (os.FileInfo, error) {
		close(statStarted)
		<-releaseStat
		return origStatFile(name)
	}
	t.Cleanup(func() {
		statFile = origStatFile
	})

	trackDone := make(chan struct{})
	go func() {
		tracker.Track(path, 1, 100)
		close(trackDone)
	}()

	<-statStarted
	tracker.Reset()
	close(releaseStat)
	<-trackDone

	if err := tracker.WasReadCheck(path); err == nil {
		t.Fatal("WasReadCheck succeeded for a read that started before Reset")
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

func setTrackerFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
