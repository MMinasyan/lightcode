package atomicfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestWriteSyncFailureBeforePublicationPreservesExistingContent proves a
// pre-publication temp sync failure on a present target is an ordinary
// failed write that leaves the old complete contents intact: Write returns
// the error, removes the temp, and the destination still holds the bytes it
// held before the call. The sibling
// TestWriteSyncFailureBeforePublicationAborts starts from an absent path, so
// its assertions cannot distinguish removing the temp from removing the
// target — only a present target can see the difference.
func TestWriteSyncFailureBeforePublicationPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")
	if err := Write(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	injected := errors.New("injected temp sync failure")
	SyncFileFunc = func(*os.File) error { return injected }
	defer func() { SyncFileFunc = nil }()

	err := Write(path, []byte("replacement"), 0o600)
	if err == nil {
		t.Fatal("Write succeeded; want the injected temp sync failure")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read destination: %v", readErr)
	}
	if string(got) != "original" {
		t.Fatalf("destination = %q after the failed write, want original", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "rec.json" {
		t.Fatalf("leftover entries after the aborted write: %v", entries)
	}
}

// TestCreateExclusiveSyncFailureBeforePublicationPreservesExistingContent
// proves the same present-target rule for the link-based publisher:
// CreateExclusive returns the error, leaves no temp behind (its defer removes
// it), and the existing file keeps its original contents.
func TestCreateExclusiveSyncFailureBeforePublicationPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")

	created, err := CreateExclusive(path, []byte("original"), 0o600)
	if err != nil || !created {
		t.Fatalf("seed create: (%v, %v)", created, err)
	}

	injected := errors.New("injected temp sync failure")
	SyncFileFunc = func(*os.File) error { return injected }
	defer func() { SyncFileFunc = nil }()

	created, err = CreateExclusive(path, []byte("replacement"), 0o600)
	if err == nil {
		t.Fatal("CreateExclusive succeeded; want the injected temp sync failure")
	}
	if created {
		t.Fatal("created = true on a failed write")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read destination: %v", readErr)
	}
	if string(got) != "original" {
		t.Fatalf("destination = %q after the failed write, want original", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "rec.json" {
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

// crashChildDestEnv selects the child half of
// TestCrashBeforeRenameLeavesReadableOrphanTemp and names the destination it
// publishes to; crashChildCreateEnv selects the CreateExclusive shape.
const (
	crashChildDestEnv   = "LIGHTCODE_ATOMICFS_CRASH_DEST"
	crashChildCreateEnv = "LIGHTCODE_ATOMICFS_CRASH_CREATE"
)

// TestCrashBeforeRenameLeavesReadableOrphanTemp proves the crash-before-
// rename state: when a writer process dies while its temp holds the complete
// new data but before Close and rename/link have run, the destination is
// untouched and the temp survives as an orphan — residue a writer's crash
// leaves behind, which readers ignore. This test asserts both halves of
// "harmless": the old content is intact (or the new record absent) and the
// orphan is present and readable. A self-exec child installs SyncFileFunc as
// a function that prints a ready marker and blocks forever; the hook fires
// after the data is written to the temp and before Close and rename/link, so
// the parent, on seeing the marker, kills the child and inspects exactly
// that state. Both publisher shapes run: Write (rename-based, destination
// seeded present) and CreateExclusive (link-based, destination absent).
func TestCrashBeforeRenameLeavesReadableOrphanTemp(t *testing.T) {
	if dest := os.Getenv(crashChildDestEnv); dest != "" {
		// Child: block the publication inside the temp sync, leaving the temp
		// on disk with complete data and the destination untouched.
		block := make(chan struct{})
		SyncFileFunc = func(*os.File) error {
			fmt.Println("synced")
			<-block
			return nil
		}
		if os.Getenv(crashChildCreateEnv) == "1" {
			if _, err := CreateExclusive(dest, []byte("replacement"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "create: %v\n", err)
				os.Exit(2)
			}
		} else {
			if err := Write(dest, []byte("replacement"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "write: %v\n", err)
				os.Exit(2)
			}
		}
		os.Exit(3) // unreachable: the hook blocks forever
	}

	t.Run("write", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "rec.json")
		if err := Write(dest, []byte("original"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		runCrashChild(t, dir, dest, false)
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read destination: %v", err)
		}
		if string(got) != "original" {
			t.Fatalf("destination = %q after the crash, want original", got)
		}
		orphan := readOrphan(t, dir)
		if string(orphan) != "replacement" {
			t.Fatalf("orphan = %q, want the complete replacement data", orphan)
		}
	})
	t.Run("create", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "rec.json")
		runCrashChild(t, dir, dest, true)
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("destination exists after the crash: %v", err)
		}
		orphan := readOrphan(t, dir)
		if string(orphan) != "replacement" {
			t.Fatalf("orphan = %q, want the complete replacement data", orphan)
		}
	})
}

// runCrashChild spawns the child half of
// TestCrashBeforeRenameLeavesReadableOrphanTemp, waits for its ready marker
// (the temp sync is in flight), kills it mid-publication, and reaps it.
func runCrashChild(t *testing.T, dir, dest string, create bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashBeforeRenameLeavesReadableOrphanTemp$")
	cmd.Env = append(os.Environ(), crashChildDestEnv+"="+dest)
	if create {
		cmd.Env = append(cmd.Env, crashChildCreateEnv+"=1")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "synced" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-scannerDone
		t.Fatalf("child never reached the temp sync\n%s", stderr.String())
	}
	// The temp is on disk with complete data; kill the child there. The child
	// is always reaped — also when Kill reports the process already exited —
	// and the scanner joined before the test can fail, so neither the child
	// nor the goroutine outlives the test. The kill is intentional, so the
	// non-nil Wait error is expected.
	killErr := cmd.Process.Kill()
	_ = cmd.Wait()
	<-scannerDone
	if killErr != nil {
		t.Fatalf("kill child: %v", killErr)
	}
}

// readOrphan returns the single *.tmp-* orphan in dir, failing the test when
// none (or more than one) exists.
func readOrphan(t *testing.T, dir string) []byte {
	t.Helper()
	orphans, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatalf("glob orphans: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %v, want exactly one", orphans)
	}
	data, err := os.ReadFile(orphans[0])
	if err != nil {
		t.Fatalf("read orphan %s: %v", orphans[0], err)
	}
	return data
}
