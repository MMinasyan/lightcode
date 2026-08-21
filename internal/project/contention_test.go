package project

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
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

// projectContentionRole selects the child half of the two-process project
// rows below. Only the role rides the flag; every other parameter rides the
// environment, so one child entry point serves every row.
var projectContentionRole = flag.String("lightcode.project-contention-child", "", "run as a project contention child: ensure or touch")

const (
	projectContentionRootEnv = "LIGHTCODE_PROJECT_CONTENTION_ROOT"
	projectContentionPathEnv = "LIGHTCODE_PROJECT_CONTENTION_PATH"
	projectContentionIDEnv   = "LIGHTCODE_PROJECT_CONTENTION_ID"
	projectContentionModeEnv = "LIGHTCODE_PROJECT_CONTENTION_MODE"
)

const (
	roleEnsure = "ensure"
	roleTouch  = "touch"
)

// Child modes. complete and hold both perform the real write; hold keeps the
// process alive afterwards so a second writer can be sequenced against a
// still-running first one. fail turns the pre-publication temp sync into an
// ordinary handled failure. park blocks inside that same temp sync so the
// parent can kill the process between a complete temp and its rename.
const (
	modeComplete = "complete"
	modeHold     = "hold"
	modeFail     = "fail"
	modePark     = "park"
)

// The child announces exactly two markers on stdout. writeEntryMarker fires
// the moment production reaches its real temp sync, which is the only proof
// that the process performed the write rather than returning through a
// no-op branch; a passing final state proves nothing about which process
// wrote it. doneMarker fires when the operation returned as its mode
// requires.
const (
	writeEntryMarker = "write-entry"
	doneMarker       = "done"
)

// contentionBound bounds every child, marker wait and reader loop, so no
// regression that blocks instead of returning can hang the package.
const contentionBound = 30 * time.Second

// projectContentionChild runs the child half of a row and never returns: it
// is the first statement of every parent test, and the parent selects it
// with the role flag. The temp-sync hook is the row's whole instrument — it
// announces the real write entry and, by mode, completes it, fails it, or
// parks in it.
func projectContentionChild() {
	role := *projectContentionRole
	if role == "" {
		return
	}
	root := os.Getenv(projectContentionRootEnv)
	path := os.Getenv(projectContentionPathEnv)
	id := os.Getenv(projectContentionIDEnv)
	mode := os.Getenv(projectContentionModeEnv)

	injected := errors.New("injected project temp sync failure")
	parked := make(chan struct{})
	atomicfs.SyncFileFunc = func(f *os.File) error {
		fmt.Println(writeEntryMarker)
		switch mode {
		case modeFail:
			return injected
		case modePark:
			<-parked // released only by this process being killed
			return nil
		default:
			return f.Sync()
		}
	}

	var err error
	switch role {
	case roleEnsure:
		_, err = EnsureForPath(root, path)
	case roleTouch:
		err = TouchActivity(root, id)
	default:
		fmt.Fprintf(os.Stderr, "unknown project contention role %q\n", role)
		os.Exit(2)
	}

	switch mode {
	case modeFail:
		if !errors.Is(err, injected) {
			fmt.Fprintf(os.Stderr, "%s error = %v, want the injected temp sync failure\n", role, err)
			os.Exit(3)
		}
	case modePark:
		fmt.Fprintf(os.Stderr, "%s returned %v; the parked temp sync must never complete\n", role, err)
		os.Exit(4)
	default:
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", role, err)
			os.Exit(5)
		}
	}
	fmt.Println(doneMarker)
	if mode == modeHold {
		// Stay alive until the parent closes stdin, so the next writer runs
		// against a first writer that is still a live process.
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}

// contentionChild is one self-exec child process of a two-process row. It
// owns a single reap path: the deadline kills a child that hangs, the marker
// scanner is joined at pipe EOF before the wait, and stderr is read only
// after the child can no longer write to it.
type contentionChild struct {
	name    string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	markers chan string
	scanEnd chan struct{}
	stderr  *bytes.Buffer
	seen    []string
	once    sync.Once
	waitErr error
}

// startContentionChild self-execs testName with the given role and
// environment. The marker channel is far larger than the handful of markers
// a child emits, so the scanner never blocks on a parent that has not
// drained it yet.
func startContentionChild(t *testing.T, name, testName, role string, env ...string) *contentionChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contentionBound)
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+testName+"$",
		"-lightcode.project-contention-child="+role,
	)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("%s stdin pipe: %v", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("%s stdout pipe: %v", name, err)
	}
	c := &contentionChild{
		name:    name,
		cmd:     cmd,
		cancel:  cancel,
		stdin:   stdin,
		markers: make(chan string, 64),
		scanEnd: make(chan struct{}),
		stderr:  &bytes.Buffer{},
	}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		defer close(c.scanEnd)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			c.markers <- scanner.Text()
		}
		close(c.markers)
	}()
	t.Cleanup(func() { c.cancel(); _ = c.reap() })
	return c
}

// reap joins the marker scanner at pipe EOF and then waits for the process,
// exactly once, so every failure path can reap without racing the scanner.
func (c *contentionChild) reap() error {
	c.once.Do(func() {
		<-c.scanEnd
		for line := range c.markers {
			c.seen = append(c.seen, line)
		}
		c.waitErr = c.cmd.Wait()
	})
	return c.waitErr
}

// diagnose kills and reaps the child and only then returns its stderr: the
// buffer is never read while the child can still write to it.
func (c *contentionChild) diagnose() string {
	c.cancel()
	_ = c.reap()
	return c.stderr.String()
}

// await blocks until the child announces want, failing the test when it
// exits or the bound elapses first.
func (c *contentionChild) await(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(contentionBound)
	for {
		select {
		case line, ok := <-c.markers:
			if !ok {
				t.Fatalf("%s exited before announcing %q; markers=%v\n%s", c.name, want, c.seen, c.diagnose())
			}
			c.seen = append(c.seen, line)
			if line == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not announce %q within %v; markers=%v\n%s", c.name, want, contentionBound, c.seen, c.diagnose())
		}
	}
}

// finish releases a held child and requires a clean exit.
func (c *contentionChild) finish(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	if err := c.reap(); err != nil {
		t.Fatalf("%s exited with %v\n%s", c.name, err, c.stderr.String())
	}
}

// kill terminates a parked child, standing in for a process that dies
// between a complete temp and its rename. It proves process and reader
// behavior at that point; it is not a power-loss simulation.
func (c *contentionChild) kill(t *testing.T) {
	t.Helper()
	c.cancel()
	_ = c.reap()
}

// count returns how many times the child announced marker.
func (c *contentionChild) count(marker string) int {
	n := 0
	for _, line := range c.seen {
		if line == marker {
			n++
		}
	}
	return n
}

// projectObservations is what the lock-free reader loop saw across a row.
type projectObservations struct {
	reads      int
	absent     int
	present    int
	violations []string
}

type projectReaderLoop struct {
	stop chan struct{}
	out  chan projectObservations
}

// startProjectReaders runs FindByPath and List in a loop for the whole life
// of a row, taking no lock, so it observes exactly what a concurrent reader
// process would. Every record it sees must satisfy check. An absent record is
// a violation unless the row's target starts absent: a present target is
// replaced by rename, so it must never disappear even for one read. Activity
// is required to be monotonic across observations.
func startProjectReaders(root, path string, allowAbsent bool, check func(*Project) string) *projectReaderLoop {
	l := &projectReaderLoop{stop: make(chan struct{}), out: make(chan projectObservations, 1)}
	go func() {
		var obs projectObservations
		var high int64
		deadline := time.Now().Add(contentionBound)
		for {
			select {
			case <-l.stop:
				l.out <- obs
				return
			default:
			}
			if time.Now().After(deadline) {
				obs.violations = append(obs.violations, "reader loop passed its deadline")
				l.out <- obs
				return
			}
			obs.reads++
			found, err := FindByPath(root, path)
			switch {
			case err != nil:
				obs.violations = append(obs.violations, "FindByPath: "+err.Error())
			case found == nil:
				obs.absent++
				if !allowAbsent {
					obs.violations = append(obs.violations, "FindByPath: the present record disappeared")
				}
			default:
				obs.present++
				if msg := check(found); msg != "" {
					obs.violations = append(obs.violations, "FindByPath: "+msg)
				}
				if found.LastActivity < high {
					obs.violations = append(obs.violations, fmt.Sprintf("activity moved backward: %d after %d", found.LastActivity, high))
				} else {
					high = found.LastActivity
				}
			}
			listed, err := List(root)
			if err != nil {
				obs.violations = append(obs.violations, "List: "+err.Error())
			}
			seen := false
			for i := range listed {
				if listed[i].Path != path {
					continue
				}
				seen = true
				if msg := check(&listed[i]); msg != "" {
					obs.violations = append(obs.violations, "List: "+msg)
				}
			}
			if !seen && !allowAbsent {
				obs.violations = append(obs.violations, "List: the present record disappeared")
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return l
}

func (l *projectReaderLoop) finish(t *testing.T) projectObservations {
	t.Helper()
	close(l.stop)
	obs := <-l.out
	if len(obs.violations) > 0 {
		t.Fatalf("lock-free readers observed disallowed project states: %s", strings.Join(obs.violations, "; "))
	}
	if obs.reads == 0 {
		t.Fatal("the lock-free reader loop performed no read")
	}
	return obs
}

// completeProjectCheck rejects any record that is not the complete
// deterministic record for path, so a torn or foreign read is a violation
// rather than a passing observation.
func completeProjectCheck(t *testing.T, path string) func(*Project) string {
	t.Helper()
	clean, err := normalizePath(path)
	if err != nil {
		t.Fatalf("normalizePath: %v", err)
	}
	wantID := projectID(clean)
	wantName := filepath.Base(clean)
	return func(p *Project) string {
		if p.ID != wantID {
			return fmt.Sprintf("id = %q, want %q", p.ID, wantID)
		}
		if p.Path != clean {
			return fmt.Sprintf("path = %q, want %q", p.Path, clean)
		}
		if p.Name != wantName {
			return fmt.Sprintf("name = %q, want %q", p.Name, wantName)
		}
		if p.LastActivity <= 0 {
			return fmt.Sprintf("last_activity = %d, want a published value", p.LastActivity)
		}
		if _, err := time.Parse(time.RFC3339, p.CreatedAt); err != nil {
			return fmt.Sprintf("created_at %q is not a complete timestamp", p.CreatedAt)
		}
		return ""
	}
}

// projectTemps returns every meta.json temp beside the record. A handled
// failure must leave none; a killed writer may leave exactly one, complete
// and ignored by every reader.
func projectTemps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "meta.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temps: %v", err)
	}
	return matches
}

// requireCompleteTemp requires exactly one temp orphan holding the complete
// record its producer was about to publish.
func requireCompleteTemp(t *testing.T, dir, wantID, wantPath string) {
	t.Helper()
	temps := projectTemps(t, dir)
	if len(temps) != 1 {
		t.Fatalf("temps = %v, want exactly the killed writer's orphan", temps)
	}
	data, err := os.ReadFile(temps[0])
	if err != nil {
		t.Fatalf("read orphan: %v", err)
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("orphan %s is not a complete record: %v", temps[0], err)
	}
	if p.ID != wantID {
		t.Fatalf("orphan id = %q, want %q", p.ID, wantID)
	}
	if msg := completeProjectCheck(t, wantPath)(&p); msg != "" {
		t.Fatalf("orphan record = %+v: %s", p, msg)
	}
}

func projectChildEnv(root, path, id, mode string) []string {
	return []string{
		projectContentionRootEnv + "=" + root,
		projectContentionPathEnv + "=" + path,
		projectContentionIDEnv + "=" + id,
		projectContentionModeEnv + "=" + mode,
	}
}

// TestProjectAbsentTwoProcessEnsureConverges runs two real EnsureForPath
// processes against an absent record. Creation is serialized under the
// identity lock, so exactly one of them performs the metadata write and the
// other observes the record the first published: the row requires one real
// creator write entry, both processes to complete, and one complete
// deterministic record. Throughout, lock-free FindByPath/List readers may see
// only an absent or a complete record — never a partial one.
func TestProjectAbsentTwoProcessEnsureConverges(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	id := projectID(path)

	readers := startProjectReaders(root, path, true, completeProjectCheck(t, path))
	env := projectChildEnv(root, path, id, modeComplete)
	first := startContentionChild(t, "ensure-a", "TestProjectAbsentTwoProcessEnsureConverges", roleEnsure, env...)
	second := startContentionChild(t, "ensure-b", "TestProjectAbsentTwoProcessEnsureConverges", roleEnsure, env...)
	first.await(t, doneMarker)
	second.await(t, doneMarker)
	first.finish(t)
	second.finish(t)
	obs := readers.finish(t)

	if writes := first.count(writeEntryMarker) + second.count(writeEntryMarker); writes != 1 {
		t.Fatalf("creator write entries = %d, want exactly one real metadata write", writes)
	}
	if obs.present == 0 {
		t.Fatal("readers never observed the published record")
	}
	found, err := FindByPath(root, path)
	if err != nil || found == nil {
		t.Fatalf("FindByPath = %+v, %v; want the converged record", found, err)
	}
	if msg := completeProjectCheck(t, path)(found); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	entries, _ := os.ReadDir(root)
	dirs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			dirs++
		}
	}
	if dirs != 1 {
		t.Fatalf("project dirs = %d, want the single converged record", dirs)
	}
	if temps := projectTemps(t, filepath.Join(root, id)); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after two completed processes", temps)
	}
}

// TestProjectAbsentHandledSyncFailurePublishesNothing runs a first
// EnsureForPath process whose pre-publication temp sync fails. The failure is
// ordinary and handled: the process leaves neither meta.json nor a temp, and
// a second process publishes the complete record. Readers see absent until
// the second publishes and complete afterwards.
func TestProjectAbsentHandledSyncFailurePublishesNothing(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	id := projectID(path)
	dir := filepath.Join(root, id)

	readers := startProjectReaders(root, path, true, completeProjectCheck(t, path))
	failing := startContentionChild(t, "ensure-fail", "TestProjectAbsentHandledSyncFailurePublishesNothing", roleEnsure,
		projectChildEnv(root, path, id, modeFail)...)
	failing.await(t, writeEntryMarker)
	failing.await(t, doneMarker)
	failing.finish(t)

	if _, err := os.Stat(filepath.Join(dir, "meta.json")); !os.IsNotExist(err) {
		t.Fatalf("meta.json stat error = %v, want not exist after the handled failure", err)
	}
	if temps := projectTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}

	second := startContentionChild(t, "ensure-ok", "TestProjectAbsentHandledSyncFailurePublishesNothing", roleEnsure,
		projectChildEnv(root, path, id, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	obs := readers.finish(t)

	if obs.absent == 0 {
		t.Fatal("readers never observed the absent record before publication")
	}
	found, err := FindByPath(root, path)
	if err != nil || found == nil {
		t.Fatalf("FindByPath = %+v, %v; want the second process's record", found, err)
	}
	if msg := completeProjectCheck(t, path)(found); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	if temps := projectTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second published", temps)
	}
}

// TestProjectAbsentKilledWriterReleasesLockAndSecondPublishes kills a first
// EnsureForPath process while it is parked inside its real temp sync: the
// temp holds the complete record but the rename has not happened. The row
// requires the identity lock to be released by the process's death, the
// second process to publish, and the only residue to be that one complete
// temp, which no reader ever surfaces.
func TestProjectAbsentKilledWriterReleasesLockAndSecondPublishes(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)

	readers := startProjectReaders(root, path, true, completeProjectCheck(t, path))
	parked := startContentionChild(t, "ensure-parked", "TestProjectAbsentKilledWriterReleasesLockAndSecondPublishes", roleEnsure,
		projectChildEnv(root, path, id, modePark)...)
	parked.await(t, writeEntryMarker)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); !os.IsNotExist(err) {
		t.Fatalf("meta.json stat error = %v, want not exist while the writer is parked", err)
	}
	parked.kill(t)
	// The residue is the directory the killed writer created plus its one
	// complete temp; both are ignored by every reader, which is what the
	// reader loop's absent observations below prove.
	if info, statErr := os.Stat(filepath.Join(dir, "sessions")); statErr != nil || !info.IsDir() {
		t.Fatalf("killed writer's project dir: info=%v err=%v", info, statErr)
	}
	requireCompleteTemp(t, dir, id, clean)

	second := startContentionChild(t, "ensure-ok", "TestProjectAbsentKilledWriterReleasesLockAndSecondPublishes", roleEnsure,
		projectChildEnv(root, path, id, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	obs := readers.finish(t)

	if obs.absent == 0 {
		t.Fatal("readers never observed the absent record while the first writer was parked")
	}
	found, err := FindByPath(root, path)
	if err != nil || found == nil {
		t.Fatalf("FindByPath = %+v, %v; want the second process's record", found, err)
	}
	if msg := completeProjectCheck(t, path)(found); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	requireCompleteTemp(t, dir, id, clean)
}

// TestProjectPresentTwoProcessTouchWrites proves the present-target row
// cannot pass through a no-op. TouchActivity writes only when the current
// second is past the stored one, so the seeded record starts far in the past
// and the second process is started only after the next Unix second begins.
// Both processes must announce their own real temp sync, the first stays
// alive while the second writes, and the final record is complete and
// monotonic. Which write survives is a last-writer-wins outcome and is not
// asserted; that both processes wrote is.
func TestProjectPresentTwoProcessTouchWrites(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	seeded := ensureForTest(t, root, path)
	setActivityForTest(t, root, seeded.ID, 1)
	dir := filepath.Join(root, seeded.ID)

	readers := startProjectReaders(root, path, false, completeProjectCheck(t, path))
	first := startContentionChild(t, "touch-a", "TestProjectPresentTwoProcessTouchWrites", roleTouch,
		projectChildEnv(root, path, seeded.ID, modeHold)...)
	first.await(t, writeEntryMarker)
	first.await(t, doneMarker)

	afterFirst, err := FindByPath(root, path)
	if err != nil || afterFirst == nil {
		t.Fatalf("FindByPath after the first write = %+v, %v", afterFirst, err)
	}
	if afterFirst.LastActivity <= 1 {
		t.Fatalf("last_activity = %d, want the first write past the seeded 1", afterFirst.LastActivity)
	}
	// The second process can only write once its own second is past the
	// stored one; until then TouchActivity is a no-op by contract.
	for time.Now().Unix() <= afterFirst.LastActivity {
		time.Sleep(20 * time.Millisecond)
	}

	second := startContentionChild(t, "touch-b", "TestProjectPresentTwoProcessTouchWrites", roleTouch,
		projectChildEnv(root, path, seeded.ID, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	first.finish(t)
	readers.finish(t)

	if got := first.count(writeEntryMarker); got != 1 {
		t.Fatalf("first process write entries = %d, want exactly one real write", got)
	}
	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process write entries = %d, want exactly one real write", got)
	}
	final, err := FindByPath(root, path)
	if err != nil || final == nil {
		t.Fatalf("FindByPath = %+v, %v", final, err)
	}
	if msg := completeProjectCheck(t, path)(final); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	if final.LastActivity <= afterFirst.LastActivity {
		t.Fatalf("last_activity = %d, want the second write past the first's %d", final.LastActivity, afterFirst.LastActivity)
	}
	if temps := projectTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after two completed writes", temps)
	}
}

// TestProjectPresentHandledSyncFailurePreservesRecord fails the first
// TouchActivity process at its pre-publication temp sync. The old record
// stays intact, no temp is left, and a second process completes its own
// write. The record is seeded far in the past so both calls are real writes
// rather than the no-op TouchActivity performs when the stored second is
// already current.
func TestProjectPresentHandledSyncFailurePreservesRecord(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	seeded := ensureForTest(t, root, path)
	setActivityForTest(t, root, seeded.ID, 1)
	dir := filepath.Join(root, seeded.ID)

	readers := startProjectReaders(root, path, false, completeProjectCheck(t, path))
	failing := startContentionChild(t, "touch-fail", "TestProjectPresentHandledSyncFailurePreservesRecord", roleTouch,
		projectChildEnv(root, path, seeded.ID, modeFail)...)
	failing.await(t, writeEntryMarker)
	failing.await(t, doneMarker)
	failing.finish(t)

	preserved, err := FindByPath(root, path)
	if err != nil || preserved == nil {
		t.Fatalf("FindByPath after the handled failure = %+v, %v", preserved, err)
	}
	if preserved.LastActivity != 1 {
		t.Fatalf("last_activity = %d, want the old record's seeded 1", preserved.LastActivity)
	}
	if temps := projectTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}

	second := startContentionChild(t, "touch-ok", "TestProjectPresentHandledSyncFailurePreservesRecord", roleTouch,
		projectChildEnv(root, path, seeded.ID, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	readers.finish(t)

	final, err := FindByPath(root, path)
	if err != nil || final == nil {
		t.Fatalf("FindByPath = %+v, %v", final, err)
	}
	if msg := completeProjectCheck(t, path)(final); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	if final.LastActivity <= 1 {
		t.Fatalf("last_activity = %d, want the second process's write", final.LastActivity)
	}
	if temps := projectTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second write", temps)
	}
}

// TestProjectPresentKilledWriterKeepsOldRecordReadable kills the first
// TouchActivity process while it is parked inside its real temp sync and
// holding the meta lock. The old record stays readable throughout, the lock
// is released by the process's death, a second process completes its write,
// and the only residue is the killed process's one complete temp.
func TestProjectPresentKilledWriterKeepsOldRecordReadable(t *testing.T) {
	projectContentionChild()

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")
	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	seeded := ensureForTest(t, root, path)
	setActivityForTest(t, root, seeded.ID, 1)
	dir := filepath.Join(root, seeded.ID)

	readers := startProjectReaders(root, path, false, completeProjectCheck(t, path))
	parked := startContentionChild(t, "touch-parked", "TestProjectPresentKilledWriterKeepsOldRecordReadable", roleTouch,
		projectChildEnv(root, path, seeded.ID, modePark)...)
	parked.await(t, writeEntryMarker)

	held, err := FindByPath(root, path)
	if err != nil || held == nil {
		t.Fatalf("FindByPath while the writer is parked = %+v, %v", held, err)
	}
	if held.LastActivity != 1 {
		t.Fatalf("last_activity = %d while parked, want the old record's seeded 1", held.LastActivity)
	}
	parked.kill(t)
	requireCompleteTemp(t, dir, seeded.ID, clean)

	second := startContentionChild(t, "touch-ok", "TestProjectPresentKilledWriterKeepsOldRecordReadable", roleTouch,
		projectChildEnv(root, path, seeded.ID, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	readers.finish(t)

	final, err := FindByPath(root, path)
	if err != nil || final == nil {
		t.Fatalf("FindByPath = %+v, %v", final, err)
	}
	if msg := completeProjectCheck(t, path)(final); msg != "" {
		t.Fatalf("final record: %s", msg)
	}
	if final.LastActivity <= 1 {
		t.Fatalf("last_activity = %d, want the second process's write", final.LastActivity)
	}
	requireCompleteTemp(t, dir, seeded.ID, clean)
}
