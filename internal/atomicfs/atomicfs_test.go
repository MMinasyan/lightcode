package atomicfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestWriteSyncFailureBeforePublicationAborts proves a pre-publication temp
// sync failure is an ordinary failed write: Write returns the error, leaves
// no temp behind, and does not publish the file.
func TestWriteSyncFailureBeforePublicationAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")

	injected := errors.New("injected temp sync failure")
	SyncFileFunc = func(*os.File) error { return injected }
	defer func() { SyncFileFunc = nil }()

	err := Write(path, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("Write succeeded; want the injected temp sync failure")
	}
	if !strings.Contains(err.Error(), "sync temp") {
		t.Fatalf("error = %v, want a temp sync error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("path exists after the aborted write: %v", statErr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftover entries after the aborted write: %v", entries)
	}
}

// TestWriteDirSyncFailureAfterPublicationReturnsSuccess proves a
// post-publication directory sync failure cannot be compensated: Write
// returns nil, the file stays published, and the diagnostic is emitted to
// stderr.
func TestWriteDirSyncFailureAfterPublicationReturnsSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")

	injected := errors.New("injected dir sync failure")
	var synced []string
	SyncDirFunc = func(d string) error { synced = append(synced, d); return injected }
	defer func() { SyncDirFunc = nil }()
	stderr := captureStderr(t)

	if err := Write(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("Write returned %v; want nil after publication", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("published file unreadable: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("published content = %q, want data", got)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("dir syncs = %v, want [%s]", synced, dir)
	}
	if out := stderr(); !strings.Contains(out, "injected dir sync failure") {
		t.Fatalf("stderr = %q, want the dir sync diagnostic", out)
	}
}

// TestCreateExclusiveSyncFailureBeforePublicationAborts proves the same
// pre-publication rule for the link-based publisher: CreateExclusive
// returns the error, leaves no temp behind (its defer removes it), and
// does not publish the file.
func TestCreateExclusiveSyncFailureBeforePublicationAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")

	injected := errors.New("injected temp sync failure")
	SyncFileFunc = func(*os.File) error { return injected }
	defer func() { SyncFileFunc = nil }()

	created, err := CreateExclusive(path, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("CreateExclusive succeeded; want the injected temp sync failure")
	}
	if created {
		t.Fatal("created = true on a failed write")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("path exists after the aborted write: %v", statErr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftover entries after the aborted write: %v", entries)
	}
}

// TestCreateExclusiveDirSyncFailureAfterPublicationReturnsSuccess proves
// the post-publication rule for the link-based publisher: CreateExclusive
// returns (true, nil), the file stays published, and the diagnostic is
// emitted to stderr. The hook fails only the destination directory's sync,
// so the pre-publication ancestor syncs succeed and the file is published.
func TestCreateExclusiveDirSyncFailureAfterPublicationReturnsSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")

	injected := errors.New("injected dir sync failure")
	var synced []string
	SyncDirFunc = func(d string) error {
		synced = append(synced, d)
		if d == dir {
			return injected
		}
		return nil
	}
	defer func() { SyncDirFunc = nil }()
	stderr := captureStderr(t)

	created, err := CreateExclusive(path, []byte("data"), 0o600)
	if err != nil || !created {
		t.Fatalf("CreateExclusive = (%v, %v); want (true, nil)", created, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("published file unreadable: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("published content = %q, want data", got)
	}
	// The ancestors are synced before publication; the destination directory
	// is synced after it, last.
	if len(synced) == 0 || synced[len(synced)-1] != dir {
		t.Fatalf("dir syncs = %v, want destination dir %s synced last", synced, dir)
	}
	if out := stderr(); !strings.Contains(out, "injected dir sync failure") {
		t.Fatalf("stderr = %q, want the dir sync diagnostic", out)
	}
}

// TestCreateExclusiveExistingPathPerformsNoDirSync proves the os.IsExist
// branch publishes nothing and so performs no destination-directory sync:
// the ancestor chain is synced before the link attempt (as in every call),
// but after the link fails nothing else is synced. The temp sync has
// necessarily already run (the target's presence is discoverable only when
// the link fails).
func TestCreateExclusiveExistingPathPerformsNoDirSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirSyncs := 0
	SyncDirFunc = func(d string) error {
		if d == dir {
			dirSyncs++
		}
		return nil
	}
	defer func() { SyncDirFunc = nil }()
	tempSyncs := 0
	SyncFileFunc = func(*os.File) error { tempSyncs++; return nil }
	defer func() { SyncFileFunc = nil }()

	created, err := CreateExclusive(path, []byte("theirs"), 0o600)
	if err != nil || created {
		t.Fatalf("CreateExclusive = (%v, %v); want (false, nil)", created, err)
	}
	if dirSyncs != 0 {
		t.Fatalf("destination dir syncs = %d, want 0 (nothing was published)", dirSyncs)
	}
	if tempSyncs != 1 {
		t.Fatalf("temp syncs = %d, want 1 (the temp is always synced before the link attempt)", tempSyncs)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "mine" {
		t.Fatalf("existing file changed: %q, want mine", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "rec.json" {
		t.Fatalf("unexpected dir entries: %v", entries)
	}
}

// TestCreateExclusiveSyncsAncestorChainWhenCallerMadeTheDirectory proves a
// first-run env-file publication leaves every directory level above the
// destination durable even when this call created none of them: the env
// file's config directory is made by the caller before CreateExclusive
// runs, so the entry naming that directory is only durable once each
// ancestor above it is synced. The hook observes every level from the
// destination directory's parent upward, and the destination directory
// itself is synced after publication, last.
func TestCreateExclusiveSyncsAncestorChainWhenCallerMadeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, ".env")

	var synced []string
	SyncDirFunc = func(d string) error {
		synced = append(synced, d)
		return nil
	}
	defer func() { SyncDirFunc = nil }()

	created, err := CreateExclusive(path, []byte("#OPENAI_API_KEY=\n"), 0o600)
	if err != nil || !created {
		t.Fatalf("CreateExclusive = (%v, %v); want (true, nil)", created, err)
	}

	// Every level from the destination directory's parent upward must have
	// been synced before publication.
	for d := filepath.Dir(configDir); ; d = filepath.Dir(d) {
		found := false
		for _, s := range synced {
			if s == d {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ancestor %s was not synced; synced = %v", d, synced)
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	// The destination directory's own sync follows publication, last.
	if len(synced) == 0 || synced[len(synced)-1] != configDir {
		t.Fatalf("synced = %v, want destination dir %s synced last", synced, configDir)
	}
}

// TestCreateExclusiveAncestorSyncFailurePublishesNothing proves an injected
// failure of one of the pre-publication ancestor syncs aborts the call like
// the other pre-publication failures: the error is returned, the file is
// not published, and no temp is left behind (the ancestor chain is synced
// before the temp is even created). The chain is walked from the
// destination directory's parent upward, so the parent is synced before the
// failing level.
func TestCreateExclusiveAncestorSyncFailurePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", ".env")

	injected := errors.New("injected ancestor sync failure")
	var synced []string
	SyncDirFunc = func(d string) error {
		synced = append(synced, d)
		if d == dir {
			return injected
		}
		return nil
	}
	defer func() { SyncDirFunc = nil }()

	created, err := CreateExclusive(path, []byte("#OPENAI_API_KEY=\n"), 0o600)
	if err == nil {
		t.Fatal("CreateExclusive succeeded; want the injected ancestor sync failure")
	}
	if !strings.Contains(err.Error(), "injected ancestor sync failure") {
		t.Fatalf("error = %v, want the injected ancestor sync failure", err)
	}
	if created {
		t.Fatal("created = true on an aborted publication")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("path exists after the aborted publication: %v", statErr)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "a", "b"))
	if len(entries) != 0 {
		t.Fatalf("leftover entries after the aborted publication: %v", entries)
	}
	// The walk started at the destination directory's parent and stopped at
	// the next level up, where the failure was injected.
	if len(synced) != 2 || synced[0] != filepath.Join(dir, "a") || synced[1] != dir {
		t.Fatalf("synced = %v, want [%s %s] before the failure", synced, filepath.Join(dir, "a"), dir)
	}
}

// captureStderr redirects os.Stderr into a temp file for the test's
// duration and returns a func that reads what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})
	return func() string {
		data, _ := os.ReadFile(f.Name())
		return string(data)
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
