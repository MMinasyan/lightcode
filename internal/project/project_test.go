package project

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestResolverEnsureCurrentAndSessionsRoot(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := NewResolver(home, projectRoot)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if resolver.ProjectRoot() != projectRoot {
		t.Fatalf("ProjectRoot = %q, want %q", resolver.ProjectRoot(), projectRoot)
	}
	if current, err := resolver.Current(); err != nil || current != nil {
		t.Fatalf("Current before Ensure = %+v, %v; want nil, nil", current, err)
	}
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if p.ID != projectID(projectRoot) || p.Path != projectRoot || p.Name != filepath.Base(projectRoot) {
		t.Fatalf("project = %+v", p)
	}
	if !strings.HasSuffix(resolver.SessionsRoot(p.ID), filepath.Join(p.ID, "sessions")) {
		t.Fatalf("SessionsRoot = %q", resolver.SessionsRoot(p.ID))
	}
	current, err := resolver.Current()
	if err != nil || current == nil || current.ID != p.ID {
		t.Fatalf("Current after Ensure = %+v, %v; want %s", current, err, p.ID)
	}
	again, err := resolver.Ensure()
	if err != nil || again.ID != p.ID {
		t.Fatalf("Ensure existing = %+v, %v; want %s", again, err, p.ID)
	}
}

func TestListFindSortAndTouch(t *testing.T) {
	root := t.TempDir()
	older := ensureForTest(t, root, filepath.Join(t.TempDir(), "old"))
	newer := ensureForTest(t, root, filepath.Join(t.TempDir(), "new"))
	// Force a deterministic activity ordering.
	setActivityForTest(t, root, older.ID, 1)
	setActivityForTest(t, root, newer.ID, 2)

	projects, err := List(root)
	if err != nil || len(projects) != 2 {
		t.Fatalf("List = %+v, %v; want 2", projects, err)
	}
	found, err := FindByPath(root, newer.Path)
	if err != nil || found == nil || found.ID != newer.ID {
		t.Fatalf("FindByPath = %+v, %v; want %s", found, err, newer.ID)
	}
	sorted, err := ListSortedByActivity(root)
	if err != nil || len(sorted) != 2 || sorted[0].ID != newer.ID || sorted[1].ID != older.ID {
		t.Fatalf("ListSortedByActivity = %+v, %v", sorted, err)
	}
	if err := TouchActivity(root, older.ID); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}
	found, err = FindByPath(root, older.Path)
	if err != nil || found.LastActivity < 1 {
		t.Fatalf("TouchActivity result = %+v, %v", found, err)
	}
	if got, err := List(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("List missing = %+v, %v; want nil, nil", got, err)
	}
}

// TestProjectIdentityIsDeterministicWithoutLegacyReuse: the same
// normalized path always yields the same p-<sha256> id, distinct paths yield
// distinct ids, and re-ensuring reuses the record without a second CreatedAt.
func TestProjectIdentityIsDeterministicWithoutLegacyReuse(t *testing.T) {
	root := t.TempDir()
	pathA := filepath.Join(t.TempDir(), "a")
	pathB := filepath.Join(t.TempDir(), "b")

	id := projectID(pathA)
	if !strings.HasPrefix(id, "p-") || len(id) != len("p-")+64 {
		t.Fatalf("id = %q; want p- plus 64 hex", id)
	}
	if projectID(pathA) != id {
		t.Fatal("projectID not deterministic for the same path")
	}
	if projectID(pathB) == id {
		t.Fatal("distinct paths produced the same id")
	}
	// Trailing-slash and dot segments normalize to the same identity.
	base, _ := normalizePath(pathA)
	trailing, _ := normalizePath(pathA + string(filepath.Separator))
	dotted, _ := normalizePath(filepath.Join(pathA, "."))
	if trailing != base || dotted != base || projectID(base) != id {
		t.Fatal("normalization mismatch for equivalent paths")
	}

	first, err := EnsureForPath(root, pathA)
	if err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if first.ID != id {
		t.Fatalf("ensured id = %q, want %q", first.ID, id)
	}
	second, err := EnsureForPath(root, pathA)
	if err != nil {
		t.Fatalf("re-ensure a: %v", err)
	}
	if second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("re-ensure changed record: %+v vs %+v", second, first)
	}
	entries, _ := os.ReadDir(root)
	projectDirs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			projectDirs++
		}
	}
	if projectDirs != 1 {
		t.Fatalf("project dirs = %d, want 1", projectDirs)
	}
}

func TestJSONHelpersRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	want := Project{ID: "id", Name: "n", Path: "/p", CreatedAt: time.Now().UTC().Format(time.RFC3339), LastActivity: 1}
	if err := writeJSON(path, want); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	var got Project
	if err := readJSON(path, &got); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if got != want {
		t.Fatalf("readJSON = %+v, want %+v", got, want)
	}
}

// TestProjectIdentityComesFromDirectory proves every project reader reports
// a record under its own directory's id: a record whose stored id names a
// different project is listed, found and ensured under the directory's id,
// never under the id it declares.
func TestProjectIdentityComesFromDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The record declares another project's id; its stored path stays correct.
	p := Project{ID: "p-declared-elsewhere", Name: "proj", Path: clean}
	if err := writeJSON(metaPathFor(root, id), p); err != nil {
		t.Fatal(err)
	}

	projects, err := List(root)
	if err != nil || len(projects) != 1 {
		t.Fatalf("List = %+v, %v; want one", projects, err)
	}
	if projects[0].ID != id {
		t.Fatalf("List id = %q, want the directory's id %q", projects[0].ID, id)
	}

	found, err := FindByPath(root, path)
	if err != nil || found == nil || found.ID != id {
		t.Fatalf("FindByPath = %+v, %v; want %s", found, err, id)
	}

	ensured, err := EnsureForPath(root, path)
	if err != nil || ensured == nil || ensured.ID != id {
		t.Fatalf("EnsureForPath = %+v, %v; want %s", ensured, err, id)
	}
}

// TestPathAwareReadersRejectRecordWhoseStoredPathDiffers proves FindByPath
// and EnsureForPath reject a record whose stored path is not the path
// searched for. The rejection is an error, never (nil, nil), which callers
// read as "no such project" and would answer by minting a second project
// over the corrupt record.
func TestPathAwareReadersRejectRecordWhoseStoredPathDiffers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Project{ID: id, Name: "proj", Path: filepath.Join(t.TempDir(), "other")}
	if err := writeJSON(metaPathFor(root, id), p); err != nil {
		t.Fatal(err)
	}

	if found, err := FindByPath(root, path); err == nil {
		t.Fatalf("FindByPath = %+v, nil error; want rejection of the stored path mismatch", found)
	}
	if _, err := EnsureForPath(root, path); err == nil {
		t.Fatal("EnsureForPath accepted a record whose stored path does not match")
	}
}

// TestProjectIdentityConcurrentEnsure: concurrent creators of the
// same path converge on one record with one deterministic id and one
// CreatedAt; neither misses the other and overwrites it.
func TestProjectIdentityConcurrentEnsure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	const workers = 16
	var wg sync.WaitGroup
	results := make([]*Project, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = EnsureForPath(root, path)
		}(i)
	}
	wg.Wait()

	wantID := projectID(path)
	var createdAt string
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].ID != wantID {
			t.Fatalf("worker %d id = %q, want %q", i, results[i].ID, wantID)
		}
		if createdAt == "" {
			createdAt = results[i].CreatedAt
		} else if results[i].CreatedAt != createdAt {
			t.Fatalf("divergent CreatedAt: %q vs %q", results[i].CreatedAt, createdAt)
		}
	}
	entries, _ := os.ReadDir(root)
	projectDirs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			projectDirs++
		}
	}
	if projectDirs != 1 {
		t.Fatalf("project dirs = %d, want 1", projectDirs)
	}
}

func ensureForTest(t *testing.T, root, path string) *Project {
	t.Helper()
	p, err := EnsureForPath(root, path)
	if err != nil {
		t.Fatalf("ensure %s: %v", path, err)
	}
	return p
}

func setActivityForTest(t *testing.T, root, id string, activity int64) {
	t.Helper()
	metaPath := metaPathFor(root, id)
	var p Project
	if err := readJSON(metaPath, &p); err != nil {
		t.Fatal(err)
	}
	p.LastActivity = activity
	if err := writeJSON(metaPath, p); err != nil {
		t.Fatal(err)
	}
}

// projectMetaLockHolderRootEnv and projectMetaLockHolderIDEnv select the
// child half of TestTouchActivityLockBlocksSecondProcess and name the
// project whose meta lock it holds.
const (
	projectMetaLockHolderRootEnv = "LIGHTCODE_PROJECT_META_LOCK_HOLDER_ROOT"
	projectMetaLockHolderIDEnv   = "LIGHTCODE_PROJECT_META_LOCK_HOLDER_ID"
)

// TestTouchActivityLockBlocksSecondProcess proves the project meta
// read-modify-write is serialized across processes, not just goroutines: the
// meta record is written by more than one process, so while a second process
// holds the record's own metaLockPath, this process's TouchActivity must not
// complete. WithLock excludes goroutines too, so a same-process test cannot
// tell flock from a sync.Mutex — which is why this test runs two processes.
// The child (a self-exec of the test binary) takes atomicfs.Acquire on the
// meta lock and holds it until the parent closes its stdin; the parent then
// observes that its own TouchActivity is blocked, releases the child, and
// observes the call complete and the record whole.
// The record is seeded with LastActivity far in the past so the write
// actually happens: TouchActivity returns without writing when now <=
// LastActivity, at second granularity, so a fresh record would make the call
// a no-op and the test vacuous.
func TestTouchActivityLockBlocksSecondProcess(t *testing.T) {
	const holdBound = 30 * time.Second
	const blockWindow = time.Second

	if root := os.Getenv(projectMetaLockHolderRootEnv); root != "" {
		// Child: hold the meta lock until the parent closes stdin.
		id := os.Getenv(projectMetaLockHolderIDEnv)
		l, err := atomicfs.Acquire(metaLockPath(root, id))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := l.Release(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	p := ensureForTest(t, root, path)
	setActivityForTest(t, root, p.ID, 1)

	holderCtx, holderCancel := context.WithTimeout(context.Background(), holdBound)
	defer holderCancel()
	cmd := exec.CommandContext(holderCtx, os.Args[0], "-test.run=^TestTouchActivityLockBlocksSecondProcess$")
	cmd.Env = append(os.Environ(),
		projectMetaLockHolderRootEnv+"="+root,
		projectMetaLockHolderIDEnv+"="+p.ID,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin pipe: %v", err)
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
	// One cleanup path for every outcome: close the child's stdin (releasing
	// its lock), reap it, and join the marker scanner at pipe EOF. The
	// bounded context kills the child at the deadline if it ever hangs, so a
	// started child cannot outlive the test. Repeating the call after the
	// success path is harmless.
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	reap := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		<-scannerDone
		return err
	}
	// fail kills the child (cancelling its context), reaps it, and only then
	// returns its stderr for diagnostics: the buffer is never read while the
	// child can still write to it.
	fail := func() string {
		holderCancel()
		_ = reap()
		return stderr.String()
	}
	defer func() { holderCancel(); _ = reap() }()

	select {
	case <-ready:
	case <-holderCtx.Done():
		t.Fatalf("child never held the meta lock within %v: %v\n%s", holdBound, holderCtx.Err(), fail())
	}

	// The writer goroutine announces itself immediately before invoking
	// TouchActivity, so the bounded wait below cannot be satisfied by a call
	// that never began.
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- TouchActivity(root, p.ID)
	}()

	<-started
	select {
	case err := <-done:
		t.Fatalf("TouchActivity returned %v while the child held the meta lock; only a process-local exclusion can do that", err)
	case <-time.After(blockWindow):
		// Blocked, as required.
	}

	if err := reap(); err != nil {
		t.Fatalf("child after release: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TouchActivity after the child released: %v", err)
		}
	case <-time.After(holdBound):
		t.Fatal("TouchActivity did not return after the child released the lock")
	}

	found, err := FindByPath(root, path)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if found.LastActivity <= 1 {
		t.Fatalf("LastActivity = %d, want the touch advanced past the seeded 1", found.LastActivity)
	}
}

func TestTryEnsureForPathReportsContentionWithoutPublication(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "projects")
	path := filepath.Join(home, "workspace")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := atomicfs.Acquire(identityLockPath(root, clean))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	got, ok, err := TryEnsureForPath(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if ok || got != nil {
		t.Fatalf("TryEnsureForPath = (%#v, %v, %v), want (nil, false, nil)", got, ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, projectID(clean), "meta.json")); !os.IsNotExist(err) {
		t.Fatalf("contended TryEnsureForPath published metadata: %v", err)
	}
}

func TestTryTouchActivityReportsContentionWithoutWaiting(t *testing.T) {
	home := t.TempDir()
	resolver, err := NewResolver(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	hold, err := atomicfs.Acquire(metaLockPath(resolver.Root(), p.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	ok, err := TryTouchActivity(resolver.Root(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("TryTouchActivity acquired a contended metadata lock")
	}
}

func TestTryEnsureForPathReportsAcquisitionErrorSeparatelyFromContention(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := TryEnsureForPath(root, filepath.Join(t.TempDir(), "project"))
	if err == nil {
		t.Fatal("TryEnsureForPath returned nil error for an uncreatable lock parent")
	}
	if ok {
		t.Fatal("TryEnsureForPath reported lock acquisition after a lock-parent error")
	}
}
