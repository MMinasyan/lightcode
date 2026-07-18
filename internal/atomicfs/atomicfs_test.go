package atomicfs

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestWriteReplacesAtomicallyWithMode(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "rec.json")

	if err := Write(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q, want first", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	if err := Write(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "second" {
		t.Fatalf("content after replace = %q, want second", got)
	}

	// No temp file is left behind in the destination directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "rec.json" {
			t.Fatalf("leftover entry %q in destination dir", e.Name())
		}
	}
}

// TestWriteDoesNotCreateDestinationDir proves Write is a replace: it fails
// rather than recreating a directory that was removed, so a record whose
// directory was concurrently deleted cannot be resurrected.
func TestWriteDoesNotCreateDestinationDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone", "rec.json")
	if err := Write(path, []byte("data"), 0o600); err == nil {
		t.Fatal("Write into a missing directory succeeded; want failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("Write recreated the missing directory: %v", err)
	}
}

func TestCreateExclusiveRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	created, err := CreateExclusive(path, []byte("mine"), 0o600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatal("first CreateExclusive: created = false, want true")
	}

	created, err = CreateExclusive(path, []byte("theirs"), 0o600)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Fatal("second CreateExclusive: created = true, want false")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "mine" {
		t.Fatalf("existing file overwritten: %q, want mine", got)
	}

	// No temp file is left behind after either call.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("unexpected dir entries: %v", entries)
	}
}

func TestTryAcquireReportsContention(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".locks", "rec.lock")

	held, ok, err := TryAcquire(lockPath)
	if err != nil || !ok {
		t.Fatalf("first TryAcquire: ok=%v err=%v, want true/nil", ok, err)
	}

	contender, ok, err := TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("second TryAcquire: %v", err)
	}
	if ok {
		contender.Release()
		t.Fatal("second TryAcquire acquired while first held; want contention")
	}

	if err := held.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	regained, ok, err := TryAcquire(lockPath)
	if err != nil || !ok {
		t.Fatalf("TryAcquire after release: ok=%v err=%v, want true/nil", ok, err)
	}
	regained.Release()
}

// TestWithLockSerializesReadModifyWrite proves the lock serializes a
// concurrent read-modify-write cycle so no addition is lost: N goroutines
// each increment a counter persisted through Write while holding the lock.
func TestWithLockSerializesReadModifyWrite(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	lockPath := filepath.Join(dir, ".locks", "counter.lock")
	if err := os.WriteFile(counter, []byte("0"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const workers = 24
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			err := WithLock(lockPath, func() error {
				raw, err := os.ReadFile(counter)
				if err != nil {
					return err
				}
				n, err := strconv.Atoi(string(raw))
				if err != nil {
					return err
				}
				return Write(counter, []byte(strconv.Itoa(n+1)), 0o600)
			})
			if err != nil {
				t.Errorf("withlock: %v", err)
			}
		}()
	}
	wg.Wait()

	raw, _ := os.ReadFile(counter)
	n, _ := strconv.Atoi(string(raw))
	if n != workers {
		t.Fatalf("counter = %d, want %d (lost updates)", n, workers)
	}
}
