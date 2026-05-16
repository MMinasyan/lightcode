package tool

import (
	"os"
	"testing"
	"time"
)

func TestTrackDoesNotHoldLockWhileStatPending(t *testing.T) {
	tracker := NewFileTracker()
	path := t.TempDir() + "/file.txt"
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	path := t.TempDir() + "/file.txt"
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

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
