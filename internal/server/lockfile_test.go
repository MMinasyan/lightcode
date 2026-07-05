package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockFilePathWriteReadRemove(t *testing.T) {
	home := t.TempDir()
	path := Path(home)
	wantPath := filepath.Join(home, ".lightcode", "owner.lock")
	if path != wantPath {
		t.Fatalf("Path() = %q, want %q", path, wantPath)
	}

	lf := LockFile{Port: 4321, Token: "token", PID: os.Getpid()}
	if err := Write(home, lf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("owner dir not created: %v", err)
	}
	if err := Write(home, lf); err == nil {
		t.Fatal("second Write error = nil, want exclusive-create error")
	}
	got, err := Read(home)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != lf {
		t.Fatalf("Read() = %+v, want %+v", got, lf)
	}
	if IsStale(got) {
		t.Fatal("lockfile for current process reported stale")
	}
	if err := Remove(home); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Read(home); err == nil {
		t.Fatal("Read after Remove error = nil, want error")
	}
}

func TestLockFileReadMalformedAndStalePID(t *testing.T) {
	home := t.TempDir()
	path := Path(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home); err == nil {
		t.Fatal("Read malformed lockfile error = nil, want error")
	}
	if !IsStale(LockFile{PID: 99999999}) {
		t.Fatal("nonexistent PID reported live")
	}
}
