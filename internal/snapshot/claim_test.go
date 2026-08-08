package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// claimExclusivityChildMode is the sole selector for this test's child
// branch: the parent runs the same binary with this flag set, so inherited
// environment values alone can never put a parent test process into child
// mode. The value chooses the child's role; the root/session parameters ride
// environment values.
var claimExclusivityChildMode = flag.String("lightcode.claim-exclusivity-child", "", "run as a claim-exclusivity test child: holder or challenger")

func TestSessionIDPathBoundary(t *testing.T) {
	for _, id := range []string{"a1b2c3d4", "session-1", "abc123"} {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "/abs", "../escape", "sub/dir", string(filepath.Separator)} {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("ValidateSessionID(%q) = nil, want error", id)
		}
	}
	// A traversal id is rejected before any filesystem effect.
	if _, _, err := AcquireSessionClaim(t.TempDir(), "p-x", ".."); err == nil {
		t.Error("AcquireSessionClaim(..) = nil error, want rejection")
	}
}

func TestSessionClaimLifecycle(t *testing.T) {
	root := t.TempDir()
	projectID := "p-claim"
	sid := "sess1"

	claim, ok, err := AcquireSessionClaim(root, projectID, sid)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// Contention: a second claim on the same session fails without an error.
	if c2, ok2, err2 := AcquireSessionClaim(root, projectID, sid); err2 != nil || ok2 {
		if c2 != nil {
			c2.Release()
		}
		t.Fatalf("contended claim: ok=%v err=%v, want false/nil", ok2, err2)
	}
	// A different session is independently claimable while the first is held.
	if c3, ok3, err3 := AcquireSessionClaim(root, projectID, "other"); err3 != nil || !ok3 {
		t.Fatalf("other-session claim: ok=%v err=%v", ok3, err3)
	} else {
		c3.Release()
	}
	// Release lets the same session be reacquired.
	if err := claim.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	c4, ok4, err4 := AcquireSessionClaim(root, projectID, sid)
	if err4 != nil || !ok4 {
		t.Fatalf("reacquire after release: ok=%v err=%v", ok4, err4)
	}
	c4.Release()
}

// TestStoreLoadSessionClaimsExclusively proves that a store claims its
// session on load/begin and releases it on Close: a second owner cannot load
// the same session while the first drives it, but can once the first closes.
func TestStoreLoadSessionClaimsExclusively(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-two-owner"
	root := filepath.Join(projectsRoot, projectID, "sessions")

	first, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	if err := first.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	sid := first.SessionID()

	second, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	if err := second.LoadSession(sid); !errors.Is(err, ErrSessionContended) {
		t.Fatalf("second owner LoadSession = %v, want ErrSessionContended", err)
	}

	if _, err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Close released the claim, so it is reacquirable.
	claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, sid)
	if err != nil || !ok {
		t.Fatalf("claim not released after Close: ok=%v err=%v", ok, err)
	}
	claim.Release()
}

// TestSweepClaimsCandidatesAndSkipsContended proves the sweep claims each
// candidate before mutating and skips one that is contended (driven by a live
// holder), while archiving a free, eligible session.
func TestSweepClaimsCandidatesAndSkipsContended(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")

	writeSession := func(id string, meta SessionMeta) {
		dir := filepath.Join(sessionsRoot, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
			t.Fatal(err)
		}
	}
	writeSession("free1", SessionMeta{ID: "free1", State: StateActive, LastActivity: 1})
	writeSession("held1", SessionMeta{ID: "held1", State: StateActive, LastActivity: 1})

	// Simulate a live holder driving held1.
	held, ok, err := AcquireSessionClaim(projectsRoot, projectID, "held1")
	if err != nil || !ok {
		t.Fatalf("hold claim: ok=%v err=%v", ok, err)
	}
	defer held.Release()

	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 3650}
	if _, _, err := SweepAllProjects(projectsRoot, cfg, nil, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if fm, _ := LoadSessionMeta(sessionsRoot, "free1"); effectiveState(fm.State) != StateArchived {
		t.Fatalf("free session not archived: %q", fm.State)
	}
	if hm, _ := LoadSessionMeta(sessionsRoot, "held1"); effectiveState(hm.State) != StateActive {
		t.Fatalf("contended session was swept: %q", hm.State)
	}
}

// TestSessionClaimReleasedOnHolderExit proves a claim held by a process that
// exits without unlocking is reacquirable: the OS drops the flock on process
// exit, so a killed holder never strands a session.
func TestSessionClaimReleasedOnHolderExit(t *testing.T) {
	root := os.Getenv("LIGHTCODE_CLAIM_HOLDER_ROOT")
	if root != "" {
		// Child process: acquire the claim and exit while still holding it.
		_, ok, err := AcquireSessionClaim(root, "p-killed", "sess-killed")
		if err != nil || !ok {
			os.Exit(2)
		}
		os.Exit(0)
	}

	root = t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionClaimReleasedOnHolderExit$")
	cmd.Env = append(os.Environ(), "LIGHTCODE_CLAIM_HOLDER_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("holder subprocess failed: %v\n%s", err, out)
	}
	claim, ok, err := AcquireSessionClaim(root, "p-killed", "sess-killed")
	if err != nil || !ok {
		t.Fatalf("reacquire after holder exit: ok=%v err=%v", ok, err)
	}
	claim.Release()
}

// contendedChallengerExit is the dedicated exit status a challenger child
// uses only when its load was refused by a typed/wrapped ErrSessionContended;
// any other load failure exits with a different status.
const contendedChallengerExit = 3

// TestLoadSessionClaimExclusiveAcrossProcesses proves the load claim is
// exclusive across real processes, not just within one: while one self-exec
// child process holds a session's claim, a second child's LoadSession of the
// same session is refused with ErrSessionContended, and succeeds once the
// holder exits. The parent asserts the challenger's dedicated contended exit
// status — the typed classification — and keeps the child's output as
// diagnostic only, so a generic non-wrapped error that renders the same text
// fails this test. A process-local mutex in acquireClaimLocked — or a store
// that never claims — lets the second child load while the first still holds;
// a goroutine test cannot tell flock from a sync.Mutex, which is why this
// test runs two processes. Every subprocess interaction is bounded by a
// five-second context deadline, so a regression that blocks instead of
// returning cannot hang the test.
func TestLoadSessionClaimExclusiveAcrossProcesses(t *testing.T) {
	const projectID = "p-two-process"
	const bound = 5 * time.Second

	if mode := *claimExclusivityChildMode; mode != "" {
		// Child process, selected only by the flag; the root/session
		// parameters ride environment values. Build the store with non-empty
		// project context, as the plan requires: acquireClaimLocked is a
		// no-op for a store with empty projectsRoot or projectID, and such a
		// store never contends.
		projectsRoot := os.Getenv("LIGHTCODE_CLAIM_EXCLUSIVITY_ROOT")
		sid := os.Getenv("LIGHTCODE_CLAIM_EXCLUSIVITY_SID")
		store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := store.LoadSession(sid); err != nil {
			// Only a typed/wrapped ErrSessionContended gets the dedicated
			// contended status; a generic error with the same rendered text
			// exits with a different status, which the parent rejects.
			if errors.Is(err, ErrSessionContended) {
				fmt.Fprintf(os.Stderr, "contended: %v\n", err)
				os.Exit(contendedChallengerExit)
			}
			fmt.Fprintf(os.Stderr, "load: %v\n", err)
			os.Exit(4)
		}
		if mode == "challenger" {
			os.Exit(0)
		}
		// Holder: print the ready marker and hold the claim until the parent
		// closes stdin, then exit.
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}

	projectsRoot := t.TempDir()
	root := filepath.Join(projectsRoot, projectID, "sessions")

	// Seed a persisted session with one completed turn, then release it: Close
	// keeps a session that has a complete turn and drops the claim, so the
	// directory stays loadable by a fresh store.
	seed, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := seed.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	turn := seed.BeginTurn()
	mustAppendMessage(t, seed, turn, `{"role":"user","content":"seeded"}`)
	if err := seed.MarkTurnComplete(turn); err != nil {
		t.Fatalf("seed complete: %v", err)
	}
	sid := seed.SessionID()
	if sid == "" {
		t.Fatal("seed session id is empty")
	}
	if _, err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// Every challenger run carries its own five-second bound, so a load that
	// blocks instead of returning is killed at the deadline and reported. The
	// child flag is the sole branch selector; the parameters ride env.
	runChallenger := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoadSessionClaimExclusiveAcrossProcesses$", "-lightcode.claim-exclusivity-child=challenger")
		cmd.Env = append(os.Environ(),
			"LIGHTCODE_CLAIM_EXCLUSIVITY_ROOT="+projectsRoot,
			"LIGHTCODE_CLAIM_EXCLUSIVITY_SID="+sid,
		)
		return cmd.CombinedOutput()
	}

	// Child A loads the session, prints its ready marker, and holds its claim
	// until the parent closes stdin. The holder carries the same deadline, so
	// a holder that never becomes ready is killed at the bound instead of
	// hanging the test.
	holderCtx, holderCancel := context.WithTimeout(context.Background(), bound)
	defer holderCancel()
	cmdA := exec.CommandContext(holderCtx, os.Args[0], "-test.run=^TestLoadSessionClaimExclusiveAcrossProcesses$", "-lightcode.claim-exclusivity-child=holder")
	cmdA.Env = append(os.Environ(),
		"LIGHTCODE_CLAIM_EXCLUSIVITY_ROOT="+projectsRoot,
		"LIGHTCODE_CLAIM_EXCLUSIVITY_SID="+sid,
	)
	stdinA, err := cmdA.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin pipe: %v", err)
	}
	stdoutA, err := cmdA.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout pipe: %v", err)
	}
	var stderrA bytes.Buffer
	cmdA.Stderr = &stderrA
	if err := cmdA.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}

	// One cleanup path, registered only after a successful start, that reaps
	// the holder exactly once: it closes holder stdin once, waits for the
	// holder (the context deadline kills it if it hangs), and joins the
	// stdout scanner at pipe EOF so it cannot outlive the test. The normal
	// success path closes stdin through this same helper without prematurely
	// cancelling the holder and verifies its clean exit. Failure paths cancel
	// the holder context and call the helper before any stderr is read, and
	// the deferred cleanup stays idempotent, so every failure path after a
	// successful start cancels, reaps, and drains the holder before the test
	// returns.
	ready := make(chan string, 1)
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdoutA)
		line := ""
		if scanner.Scan() {
			line = scanner.Text()
		}
		ready <- line
	}()
	var (
		waitOnce  sync.Once
		stdinOnce sync.Once
		waitErr   error
	)
	waitHolder := func() error {
		waitOnce.Do(func() {
			stdinOnce.Do(func() { _ = stdinA.Close() })
			waitErr = cmdA.Wait()
			<-scannerDone
		})
		return waitErr
	}
	// failHolder kills and reaps the holder, joins the scanner, and only then
	// returns its stderr for the diagnostic: the buffer is never read while
	// the holder can still write to it.
	failHolder := func() string {
		holderCancel()
		_ = waitHolder()
		return stderrA.String()
	}
	defer func() { holderCancel(); _ = waitHolder() }()

	select {
	case line := <-ready:
		if line != "ready" {
			t.Fatalf("holder did not become ready: %q\n%s", line, failHolder())
		}
	case <-holderCtx.Done():
		t.Fatalf("holder did not become ready within %v: %v\n%s", bound, holderCtx.Err(), failHolder())
	}

	// While A holds the claim, the second process's load must be refused —
	// observed, not raced: the holder is known to hold, so a success here is
	// a missing or process-local claim. The challenger's dedicated contended
	// exit status is what is asserted; its output is diagnostic only.
	if out, err := runChallenger(); err == nil {
		t.Fatalf("challenger load succeeded while the holder held the claim:\n%s", out)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != contendedChallengerExit {
		t.Fatalf("challenger while held = %v\n%s, want dedicated contended exit status %d", err, out, contendedChallengerExit)
	}

	// Releasing the holder (closing its stdin through the cleanup helper)
	// must let the same challenger load succeed.
	if err := waitHolder(); err != nil {
		t.Fatalf("holder after release: %v\n%s", err, stderrA.String())
	}
	if out, err := runChallenger(); err != nil {
		t.Fatalf("challenger after release = %v\n%s", err, out)
	}
}
