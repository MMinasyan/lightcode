package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockFilePathWriteReadRemove(t *testing.T) {
	home := t.TempDir()
	projectID := "project-1"
	path := Path(home, projectID)
	wantPath := filepath.Join(home, ".lightcode", "daemon", projectID+".lock")
	if path != wantPath {
		t.Fatalf("Path() = %q, want %q", path, wantPath)
	}

	lf := LockFile{Port: 4321, Token: "token", PID: os.Getpid()}
	if err := Write(home, projectID, lf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("daemon dir not created: %v", err)
	}
	got, err := Read(home, projectID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != lf {
		t.Fatalf("Read() = %+v, want %+v", got, lf)
	}
	if IsStale(got) {
		t.Fatal("lockfile for current process reported stale")
	}
	if err := Remove(home, projectID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Read(home, projectID); err == nil {
		t.Fatal("Read after Remove error = nil, want error")
	}
}

func TestLockFileReadMalformedAndStalePID(t *testing.T) {
	home := t.TempDir()
	projectID := "bad"
	path := Path(home, projectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home, projectID); err == nil {
		t.Fatal("Read malformed lockfile error = nil, want error")
	}
	if !IsStale(LockFile{PID: 99999999}) {
		t.Fatal("nonexistent PID reported live")
	}
}
