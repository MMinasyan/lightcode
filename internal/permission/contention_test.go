package permission

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

// permissionContentionRole selects the child half of the two-process
// permissions rows below. Only the role rides the flag; every other parameter
// rides the environment, so one child entry point serves every row.
var permissionContentionRole = flag.String("lightcode.permission-contention-child", "", "run as a permissions contention child: save")

const (
	permissionContentionRootEnv    = "LIGHTCODE_PERM_CONTENTION_ROOT"
	permissionContentionProjectEnv = "LIGHTCODE_PERM_CONTENTION_PROJECT"
	permissionContentionRuleEnv    = "LIGHTCODE_PERM_CONTENTION_RULE"
	permissionContentionModeEnv    = "LIGHTCODE_PERM_CONTENTION_MODE"
)

const roleSave = "save"

// Child modes. complete performs the real merge and publication. fail turns
// the pre-publication temp sync into an ordinary handled failure. park blocks
// inside that same temp sync, holding the permissions lock, so the parent can
// kill the process between a complete temp and its rename.
const (
	modeComplete = "complete"
	modeFail     = "fail"
	modePark     = "park"
)

// The child announces exactly two markers on stdout. writeEntryMarker fires
// the moment production reaches its real temp sync, which is the only proof
// that this process performed the merge write; a final file holding both
// rules does not say which process wrote which. doneMarker fires when the
// operation returned as its mode requires.
const (
	writeEntryMarker = "write-entry"
	doneMarker       = "done"
)

// contentionBound bounds every child, marker wait and reader loop, so no
// regression that blocks instead of returning can hang the package.
const contentionBound = 30 * time.Second

// permissionContentionChild runs the child half of a row and never returns:
// it is the first statement of every parent test, and the parent selects it
// with the role flag. The temp-sync hook is the row's whole instrument — it
// announces the real write entry and, by mode, completes it, fails it, or
// parks in it while the permissions lock is held.
func permissionContentionChild() {
	role := *permissionContentionRole
	if role == "" {
		return
	}
	root := os.Getenv(permissionContentionRootEnv)
	projectID := os.Getenv(permissionContentionProjectEnv)
	rule := os.Getenv(permissionContentionRuleEnv)
	mode := os.Getenv(permissionContentionModeEnv)

	injected := errors.New("injected permissions temp sync failure")
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

	if role != roleSave {
		fmt.Fprintf(os.Stderr, "unknown permissions contention role %q\n", role)
		os.Exit(2)
	}
	err := SaveLocal(root, projectID, Rules{Allow: []string{rule}})

	switch mode {
	case modeFail:
		if !errors.Is(err, injected) {
			fmt.Fprintf(os.Stderr, "SaveLocal error = %v, want the injected temp sync failure\n", err)
			os.Exit(3)
		}
	case modePark:
		fmt.Fprintf(os.Stderr, "SaveLocal returned %v; the parked temp sync must never complete\n", err)
		os.Exit(4)
	default:
		if err != nil {
			fmt.Fprintf(os.Stderr, "SaveLocal: %v\n", err)
			os.Exit(5)
		}
	}
	fmt.Println(doneMarker)
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
		"-lightcode.permission-contention-child="+role,
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

// finish reaps the child and requires a clean exit.
func (c *contentionChild) finish(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	if err := c.reap(); err != nil {
		t.Fatalf("%s exited with %v\n%s", c.name, err, c.stderr.String())
	}
}

// kill terminates a parked child, standing in for a process that dies
// between a complete temp and its rename. It proves process, lock and reader
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

// permissionObservations is what the lock-free reader loop saw across a row.
type permissionObservations struct {
	reads      int
	empty      int
	populated  int
	violations []string
}

type permissionReaderLoop struct {
	stop chan struct{}
	out  chan permissionObservations
}

// startPermissionReaders runs LoadLocal in a loop for the whole life of a
// row, taking no lock, so it observes exactly what a concurrent reader
// process would. Every observation must parse, must hold only rules the row
// can produce, must keep every rule it has already published, and must keep
// every seeded rule once the record exists.
func startPermissionReaders(root, projectID string, seed []string, allowed map[string]bool) *permissionReaderLoop {
	l := &permissionReaderLoop{stop: make(chan struct{}), out: make(chan permissionObservations, 1)}
	go func() {
		var obs permissionObservations
		observed := make(map[string]bool)
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
			got, err := LoadLocal(root, projectID)
			if err != nil {
				obs.violations = append(obs.violations, "LoadLocal: "+err.Error())
				time.Sleep(time.Millisecond)
				continue
			}
			current := make(map[string]bool, len(got.Allow))
			for _, rule := range got.Allow {
				if !allowed[rule] {
					obs.violations = append(obs.violations, fmt.Sprintf("unexpected rule %q", rule))
				}
				current[rule] = true
			}
			if len(got.Allow) == 0 {
				obs.empty++
			} else {
				obs.populated++
				for _, rule := range seed {
					if !current[rule] {
						obs.violations = append(obs.violations, fmt.Sprintf("seeded rule %q disappeared", rule))
					}
				}
			}
			for rule := range observed {
				if !current[rule] {
					obs.violations = append(obs.violations, fmt.Sprintf("published rule %q disappeared", rule))
				}
			}
			for rule := range current {
				observed[rule] = true
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return l
}

func (l *permissionReaderLoop) finish(t *testing.T) permissionObservations {
	t.Helper()
	close(l.stop)
	obs := <-l.out
	if len(obs.violations) > 0 {
		t.Fatalf("lock-free readers observed disallowed permissions states: %s", strings.Join(obs.violations, "; "))
	}
	if obs.reads == 0 {
		t.Fatal("the lock-free reader loop performed no read")
	}
	return obs
}

// permissionTemps returns every permissions.json temp beside the record. A
// handled failure must leave none; a killed writer may leave exactly one,
// complete and ignored by every reader.
func permissionTemps(t *testing.T, root, projectID string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, projectID, "permissions.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temps: %v", err)
	}
	return matches
}

// requireCompleteTemp requires exactly one temp orphan holding the complete
// merged record its producer was about to publish.
func requireCompleteTemp(t *testing.T, root, projectID string, want []string) {
	t.Helper()
	temps := permissionTemps(t, root, projectID)
	if len(temps) != 1 {
		t.Fatalf("temps = %v, want exactly the killed writer's orphan", temps)
	}
	data, err := os.ReadFile(temps[0])
	if err != nil {
		t.Fatalf("read orphan: %v", err)
	}
	var rules Rules
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("orphan %s is not a complete record: %v", temps[0], err)
	}
	if strings.Join(rules.Allow, ",") != strings.Join(want, ",") {
		t.Fatalf("orphan Allow = %v, want %v", rules.Allow, want)
	}
}

func permissionChildEnv(root, projectID, rule, mode string) []string {
	return []string{
		permissionContentionRootEnv + "=" + root,
		permissionContentionProjectEnv + "=" + projectID,
		permissionContentionRuleEnv + "=" + rule,
		permissionContentionModeEnv + "=" + mode,
	}
}

func allowedSet(rules ...string) map[string]bool {
	set := make(map[string]bool, len(rules))
	for _, rule := range rules {
		set[rule] = true
	}
	return set
}

// requireAllowSet requires the published record to hold exactly want, in any
// order, so a lost update is a failure rather than a shorter passing list.
func requireAllowSet(t *testing.T, root, projectID string, want ...string) {
	t.Helper()
	got, err := LoadLocal(root, projectID)
	if err != nil {
		t.Fatalf("LoadLocal: %v", err)
	}
	if len(got.Allow) != len(want) {
		t.Fatalf("Allow = %v, want the %d surviving rules %v", got.Allow, len(want), want)
	}
	have := allowedSet(got.Allow...)
	for _, rule := range want {
		if !have[rule] {
			t.Fatalf("Allow = %v, missing %q", got.Allow, rule)
		}
	}
}

// TestPermissionsAbsentTwoProcessSaveBothSurvive runs two real SaveLocal
// processes against an absent record, each adding a distinct rule. The
// read-merge-write cycle is serialized across processes, so both perform
// their own real write and both additions survive. Throughout, lock-free
// LoadLocal readers may see only an empty record or a parseable union of the
// two additions — never malformed bytes and never a rule that vanishes.
func TestPermissionsAbsentTwoProcessSaveBothSurvive(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-absent-complete"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()

	readers := startPermissionReaders(root, projectID, nil, allowedSet(ruleA, ruleB))
	first := startContentionChild(t, "save-a", "TestPermissionsAbsentTwoProcessSaveBothSurvive", roleSave,
		permissionChildEnv(root, projectID, ruleA, modeComplete)...)
	second := startContentionChild(t, "save-b", "TestPermissionsAbsentTwoProcessSaveBothSurvive", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	first.await(t, doneMarker)
	second.await(t, doneMarker)
	first.finish(t)
	second.finish(t)
	obs := readers.finish(t)

	if got := first.count(writeEntryMarker); got != 1 {
		t.Fatalf("first process write entries = %d, want exactly one real write", got)
	}
	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process write entries = %d, want exactly one real write", got)
	}
	if obs.populated == 0 {
		t.Fatal("readers never observed the published record")
	}
	requireAllowSet(t, root, projectID, ruleA, ruleB)
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after two completed processes", temps)
	}
}

// TestPermissionsAbsentHandledSyncFailurePublishesNothing fails the first
// SaveLocal process at its pre-publication temp sync against an absent
// record: nothing is published, no temp is left, and a second process
// completes its own addition.
func TestPermissionsAbsentHandledSyncFailurePublishesNothing(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-absent-failure"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()

	readers := startPermissionReaders(root, projectID, nil, allowedSet(ruleA, ruleB))
	failing := startContentionChild(t, "save-fail", "TestPermissionsAbsentHandledSyncFailurePublishesNothing", roleSave,
		permissionChildEnv(root, projectID, ruleA, modeFail)...)
	failing.await(t, writeEntryMarker)
	failing.await(t, doneMarker)
	failing.finish(t)

	if _, err := os.Stat(localPath(root, projectID)); !os.IsNotExist(err) {
		t.Fatalf("permissions.json stat error = %v, want not exist after the handled failure", err)
	}
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}

	second := startContentionChild(t, "save-ok", "TestPermissionsAbsentHandledSyncFailurePublishesNothing", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	obs := readers.finish(t)

	if obs.empty == 0 {
		t.Fatal("readers never observed the empty record before publication")
	}
	requireAllowSet(t, root, projectID, ruleB)
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second published", temps)
	}
}

// TestPermissionsAbsentKilledWriterReleasesLockAndSecondCompletes kills the
// first SaveLocal process while it is parked inside its real temp sync and
// holding the permissions lock. The lock is released by the process's death,
// a second process completes its own addition, and the only residue is that
// one complete temp, which no reader ever surfaces.
func TestPermissionsAbsentKilledWriterReleasesLockAndSecondCompletes(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-absent-crash"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()

	readers := startPermissionReaders(root, projectID, nil, allowedSet(ruleA, ruleB))
	parked := startContentionChild(t, "save-parked", "TestPermissionsAbsentKilledWriterReleasesLockAndSecondCompletes", roleSave,
		permissionChildEnv(root, projectID, ruleA, modePark)...)
	parked.await(t, writeEntryMarker)
	if _, err := os.Stat(localPath(root, projectID)); !os.IsNotExist(err) {
		t.Fatalf("permissions.json stat error = %v, want not exist while the writer is parked", err)
	}
	parked.kill(t)
	requireCompleteTemp(t, root, projectID, []string{ruleA})

	second := startContentionChild(t, "save-ok", "TestPermissionsAbsentKilledWriterReleasesLockAndSecondCompletes", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	obs := readers.finish(t)

	if obs.empty == 0 {
		t.Fatal("readers never observed the empty record while the first writer was parked")
	}
	requireAllowSet(t, root, projectID, ruleB)
	requireCompleteTemp(t, root, projectID, []string{ruleA})
}

// TestPermissionsPresentTwoProcessSaveBothSurvive seeds a rule and then runs
// two real SaveLocal processes, each adding a distinct rule. Both perform
// their own real write, the seed and both additions survive the serialized
// read-merge-write, and readers never lose the seed.
func TestPermissionsPresentTwoProcessSaveBothSurvive(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-present-complete"
	const seedRule = "run_command(seed)"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()
	if err := SaveLocal(root, projectID, Rules{Allow: []string{seedRule}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	readers := startPermissionReaders(root, projectID, []string{seedRule}, allowedSet(seedRule, ruleA, ruleB))
	first := startContentionChild(t, "save-a", "TestPermissionsPresentTwoProcessSaveBothSurvive", roleSave,
		permissionChildEnv(root, projectID, ruleA, modeComplete)...)
	second := startContentionChild(t, "save-b", "TestPermissionsPresentTwoProcessSaveBothSurvive", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	first.await(t, doneMarker)
	second.await(t, doneMarker)
	first.finish(t)
	second.finish(t)
	readers.finish(t)

	if got := first.count(writeEntryMarker); got != 1 {
		t.Fatalf("first process write entries = %d, want exactly one real write", got)
	}
	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process write entries = %d, want exactly one real write", got)
	}
	requireAllowSet(t, root, projectID, seedRule, ruleA, ruleB)
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after two completed processes", temps)
	}
}

// TestPermissionsPresentHandledSyncFailurePreservesSeed fails the first
// SaveLocal process at its pre-publication temp sync against a seeded
// record: the seed is preserved untouched, no temp is left, and a second
// process's merge completes on top of it.
func TestPermissionsPresentHandledSyncFailurePreservesSeed(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-present-failure"
	const seedRule = "run_command(seed)"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()
	if err := SaveLocal(root, projectID, Rules{Allow: []string{seedRule}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	readers := startPermissionReaders(root, projectID, []string{seedRule}, allowedSet(seedRule, ruleA, ruleB))
	failing := startContentionChild(t, "save-fail", "TestPermissionsPresentHandledSyncFailurePreservesSeed", roleSave,
		permissionChildEnv(root, projectID, ruleA, modeFail)...)
	failing.await(t, writeEntryMarker)
	failing.await(t, doneMarker)
	failing.finish(t)

	requireAllowSet(t, root, projectID, seedRule)
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}

	second := startContentionChild(t, "save-ok", "TestPermissionsPresentHandledSyncFailurePreservesSeed", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	readers.finish(t)

	requireAllowSet(t, root, projectID, seedRule, ruleB)
	if temps := permissionTemps(t, root, projectID); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second merge", temps)
	}
}

// TestPermissionsPresentKilledWriterKeepsSeedReadable kills the first
// SaveLocal process while it is parked inside its real temp sync and holding
// the permissions lock on a seeded record. The seed stays readable
// throughout, the lock is released by the process's death, a second process
// completes its merge, and the only residue is the killed process's one
// complete temp.
func TestPermissionsPresentKilledWriterKeepsSeedReadable(t *testing.T) {
	permissionContentionChild()

	const projectID = "p-present-crash"
	const seedRule = "run_command(seed)"
	const ruleA = "run_command(a)"
	const ruleB = "run_command(b)"
	root := t.TempDir()
	if err := SaveLocal(root, projectID, Rules{Allow: []string{seedRule}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	readers := startPermissionReaders(root, projectID, []string{seedRule}, allowedSet(seedRule, ruleA, ruleB))
	parked := startContentionChild(t, "save-parked", "TestPermissionsPresentKilledWriterKeepsSeedReadable", roleSave,
		permissionChildEnv(root, projectID, ruleA, modePark)...)
	parked.await(t, writeEntryMarker)
	requireAllowSet(t, root, projectID, seedRule)
	parked.kill(t)
	requireCompleteTemp(t, root, projectID, []string{seedRule, ruleA})

	second := startContentionChild(t, "save-ok", "TestPermissionsPresentKilledWriterKeepsSeedReadable", roleSave,
		permissionChildEnv(root, projectID, ruleB, modeComplete)...)
	second.await(t, writeEntryMarker)
	second.await(t, doneMarker)
	second.finish(t)
	readers.finish(t)

	requireAllowSet(t, root, projectID, seedRule, ruleB)
	requireCompleteTemp(t, root, projectID, []string{seedRule, ruleA})
}
