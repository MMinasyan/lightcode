package permission

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestSaveLocalHappyPath(t *testing.T) {
	root := t.TempDir()
	pid := "proj-1"

	// SaveLocal doesn't create directories — caller must ensure they exist.
	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	rules := Rules{
		Allow: []string{"write_file(/src/**)"},
		Deny:  []string{"read_file(//etc/passwd)"},
	}
	if err := SaveLocal(root, pid, rules); err != nil {
		t.Fatal("SaveLocal:", err)
	}

	path := filepath.Join(root, pid, "permissions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	var got Rules
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal("Unmarshal:", err)
	}
	if len(got.Allow) != 1 || got.Allow[0] != "write_file(/src/**)" {
		t.Fatalf("Allow = %v, want [write_file(/src/**)]", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "read_file(//etc/passwd)" {
		t.Fatalf("Deny = %v, want [read_file(//etc/passwd)]", got.Deny)
	}
}

func TestLoadLocalHappyPath(t *testing.T) {
	root := t.TempDir()
	pid := "proj-2"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a known file
	permissions := Rules{
		Allow: []string{"write_file(/src/main.go)"},
		Ask:   []string{"run_command(npm run *)"},
	}
	if err := SaveLocal(root, pid, permissions); err != nil {
		t.Fatal("SaveLocal:", err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal("LoadLocal:", err)
	}
	if len(got.Allow) != 1 || got.Allow[0] != "write_file(/src/main.go)" {
		t.Fatalf("Allow = %v", got.Allow)
	}
	if len(got.Ask) != 1 || got.Ask[0] != "run_command(npm run *)" {
		t.Fatalf("Ask = %v", got.Ask)
	}
}

func TestLoadLocalMissingFile(t *testing.T) {
	root := t.TempDir()
	got, err := LoadLocal(root, "nonexistent")
	if err != nil {
		t.Fatal("LoadLocal should not error for missing file:", err)
	}
	if len(got.Allow) != 0 || len(got.Deny) != 0 || len(got.Ask) != 0 {
		t.Fatalf("expected empty rules, got %+v", got)
	}
}

func TestLoadLocalEmptyArgs(t *testing.T) {
	got, err := LoadLocal("", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow != nil || got.Deny != nil || got.Ask != nil {
		t.Fatalf("expected nil slices, got %+v", got)
	}

	got, err = LoadLocal("root", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow != nil {
		t.Fatalf("expected nil Allow, got %v", got.Allow)
	}
}

func TestProjectIDScoping(t *testing.T) {
	root := t.TempDir()

	for _, pid := range []string{"projA", "projB"} {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Write rules for project A
	if err := SaveLocal(root, "projA", Rules{
		Allow: []string{"write_file(/src/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	// Write different rules for project B
	if err := SaveLocal(root, "projB", Rules{
		Deny: []string{"read_file(/secret/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	rulesA, err := LoadLocal(root, "projA")
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := LoadLocal(root, "projB")
	if err != nil {
		t.Fatal(err)
	}

	if len(rulesA.Allow) != 1 {
		t.Fatalf("projA Allow = %v, expected 1 entry", rulesA.Allow)
	}
	if len(rulesA.Deny) != 0 {
		t.Fatalf("projA Deny = %v, expected 0", rulesA.Deny)
	}
	if len(rulesB.Deny) != 1 {
		t.Fatalf("projB Deny = %v, expected 1 entry", rulesB.Deny)
	}
	if len(rulesB.Allow) != 0 {
		t.Fatalf("projB Allow = %v, expected 0", rulesB.Allow)
	}
}

func TestSaveLocalMergesExistingRules(t *testing.T) {
	root := t.TempDir()
	pid := "proj-merge"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// First save
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"write_file(/src/a.go)"},
		Deny:  []string{"read_file(/secret/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second save with overlapping + new entries
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"write_file(/src/a.go)", "write_file(/src/b.go)"},
		Deny:  []string{"read_file(/secret/**)", "read_file(//etc/shadow)"},
		Ask:   []string{"run_command(make *)"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal(err)
	}

	// Allow should have 2 (no duplicates)
	if len(got.Allow) != 2 {
		t.Fatalf("Allow = %v, expected 2 entries (no duplicates)", got.Allow)
	}
	// Deny should have 2
	if len(got.Deny) != 2 {
		t.Fatalf("Deny = %v, expected 2 entries", got.Deny)
	}
	// Ask should have 1
	if len(got.Ask) != 1 {
		t.Fatalf("Ask = %v, expected 1 entry", got.Ask)
	}
}

func TestSaveLocalMergePreservesOrder(t *testing.T) {
	root := t.TempDir()
	pid := "proj-order"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// First save
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"a", "b"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second save with b (existing) + c (new) + a (existing)
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"b", "c", "a"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	// mergeUnique preserves existing order and appends new items.
	// existing=[a,b], add=[b,c,a] → result=[a,b,c] (b deduped, c new, a deduped)
	if len(got.Allow) != 3 || got.Allow[0] != "a" || got.Allow[1] != "b" || got.Allow[2] != "c" {
		t.Fatalf("Allow = %v, want [a b c]", got.Allow)
	}
}

func TestLoadLocalMalformedFile(t *testing.T) {
	root := t.TempDir()
	pid := "proj-bad"
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.json"), []byte("NOT JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadLocal(root, pid)
	if err == nil {
		t.Fatal("LoadLocal should return error for malformed file")
	}
}

func TestLoadLocalProjectIDScoping(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "projX"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Save to projX
	if err := SaveLocal(root, "projX", Rules{
		Allow: []string{"write_file(/x.go)"},
	}); err != nil {
		t.Fatal(err)
	}

	// projY should be empty (no file exists)
	got, err := LoadLocal(root, "projY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Allow) != 0 {
		t.Fatalf("projY Allow = %v, want empty", got.Allow)
	}
}

// TestTrySaveLocalRows covers the one-attempt TrySaveLocal contract: success
// performs the bounded read-merge-write, contention returns (false, nil)
// without any payload operation, and empty arguments are an executed no-op.
func TestTrySaveLocalRows(t *testing.T) {
	t.Run("success_merges", func(t *testing.T) {
		root := t.TempDir()
		pid := "proj-try-ok"
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := SaveLocal(root, pid, Rules{Allow: []string{"orig"}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ok, err := TrySaveLocal(root, pid, Rules{Allow: []string{"added"}})
		if !ok || err != nil {
			t.Fatalf("TrySaveLocal = (%v, %v), want (true, nil)", ok, err)
		}
		got, err := LoadLocal(root, pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Allow) != 2 || got.Allow[0] != "orig" || got.Allow[1] != "added" {
			t.Fatalf("Allow = %v, want [orig added]", got.Allow)
		}
	})
	t.Run("contention_performs_no_payload", func(t *testing.T) {
		root := t.TempDir()
		pid := "proj-try-contend"
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		held, ok, err := atomicfs.TryAcquire(lockPath(root, pid))
		if err != nil || !ok {
			t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
		}
		defer held.Release()
		got, err := TrySaveLocal(root, pid, Rules{Allow: []string{"added"}})
		if got {
			t.Fatal("TrySaveLocal acquired while the lock was held")
		}
		if err != nil {
			t.Fatalf("TrySaveLocal contention err = %v, want nil", err)
		}
		if _, statErr := os.Stat(localPath(root, pid)); !os.IsNotExist(statErr) {
			t.Fatalf("contended TrySaveLocal created permissions.json: %v", statErr)
		}
	})
	t.Run("empty_args_noop", func(t *testing.T) {
		ok, err := TrySaveLocal("", "pid", Rules{Allow: []string{"x"}})
		if !ok || err != nil {
			t.Fatalf("TrySaveLocal empty args = (%v, %v), want (true, nil)", ok, err)
		}
	})
}

// permLockHolderRootEnv selects the child half of
// TestSaveLocalLockBlocksSecondProcess and names the projects root whose
// permissions lock it holds.
const permLockHolderRootEnv = "LIGHTCODE_PERM_LOCK_HOLDER_ROOT"

// TestSaveLocalLockBlocksSecondProcess proves the permissions read-modify-
// write is serialized across processes, not just goroutines: the permissions
// file is written by more than one process, so while a second process holds
// the record's own lock path, this process's SaveLocal must not complete.
// WithLock excludes goroutines too, so a same-process test cannot tell flock
// from a sync.Mutex — which is why this test runs two processes. The child
// (a self-exec of the test binary) takes atomicfs.Acquire on the permissions
// lock and holds it until the parent closes its stdin; the parent then
// observes that its own SaveLocal is blocked, releases the child, and
// observes the call complete and the record whole.
func TestSaveLocalLockBlocksSecondProcess(t *testing.T) {
	const pid = "proj-lock"
	const holdBound = 30 * time.Second
	const blockWindow = time.Second

	if root := os.Getenv(permLockHolderRootEnv); root != "" {
		// Child: hold the permissions lock until the parent closes stdin.
		l, err := atomicfs.Acquire(lockPath(root, pid))
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
	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveLocal(root, pid, Rules{Allow: []string{"orig"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	holderCtx, holderCancel := context.WithTimeout(context.Background(), holdBound)
	defer holderCancel()
	cmd := exec.CommandContext(holderCtx, os.Args[0], "-test.run=^TestSaveLocalLockBlocksSecondProcess$")
	cmd.Env = append(os.Environ(), permLockHolderRootEnv+"="+root)
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
		t.Fatalf("child never held the permissions lock within %v: %v\n%s", holdBound, holderCtx.Err(), fail())
	}

	// The writer goroutine announces itself immediately before invoking
	// SaveLocal, so the bounded wait below cannot be satisfied by a call that
	// never began.
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- SaveLocal(root, pid, Rules{Allow: []string{"added"}})
	}()

	<-started
	select {
	case err := <-done:
		t.Fatalf("SaveLocal returned %v while the child held the permissions lock; only a process-local exclusion can do that", err)
	case <-time.After(blockWindow):
		// Blocked, as required.
	}

	if err := reap(); err != nil {
		t.Fatalf("child after release: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SaveLocal after the child released: %v", err)
		}
	case <-time.After(holdBound):
		t.Fatal("SaveLocal did not return after the child released the lock")
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatalf("LoadLocal: %v", err)
	}
	if len(got.Allow) != 2 || got.Allow[0] != "orig" || got.Allow[1] != "added" {
		t.Fatalf("Allow = %v, want [orig added]", got.Allow)
	}
}
