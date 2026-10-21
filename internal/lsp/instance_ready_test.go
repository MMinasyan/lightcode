package lsp

// These tests exercise the instance's start/wait protocol at package level.
// No adapter-facing entry point exposes an *instance — the runtime only hands
// out per-file handles — so the protocol can only be exercised on the package's
// own types. They drive a real server process (a small stdio script) so that
// process counts, failures, cancellation and timeouts are observed without
// adding any test-only hooks to production code.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
	"github.com/MMinasyan/lightcode/internal/lsp/server"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// fakeServerScript is a minimal LSP server over stdio that records every
// launch in a log file. Args: log path, optional fail marker (the server
// exits before logging when the file exists), optional delay in seconds before
// answering initialize, optional delay in seconds before sending the work-done
// progress notification that marks the server ready (a negative delay
// suppresses it entirely), a crash mode ("first" crashes only the very first
// launch, "always" every launch, both after initialize and before readiness;
// "after-open" lets the first launch become ready and then crashes it on its
// first didOpen, with every later launch crashing before readiness),
// and a stubborn flag that makes the server never answer the shutdown
// request.
const fakeServerScript = `#!/usr/bin/env python3
import json
import os
import sys
import time

log = sys.argv[1]
fail_marker = sys.argv[2] if len(sys.argv) > 2 else ""
init_delay = float(sys.argv[3]) if len(sys.argv) > 3 and sys.argv[3] else 0.0
ready_delay = float(sys.argv[4]) if len(sys.argv) > 4 and sys.argv[4] else 0.0
crash = sys.argv[5] if len(sys.argv) > 5 and sys.argv[5] else ""
stubborn = sys.argv[6] if len(sys.argv) > 6 and sys.argv[6] else ""
requests_log = os.environ.get("FAKE_LSP_REQUESTS_LOG", "")

first_launch = False
if crash:
    try:
        first_launch = os.path.getsize(log) == 0
    except OSError:
        first_launch = True

if fail_marker and os.path.exists(fail_marker):
    sys.exit(1)

with open(log, "a") as f:
    f.write("started\n")


def send(obj):
    data = json.dumps(obj, separators=(",", ":")).encode()
    sys.stdout.buffer.write(b"Content-Length: %d\r\n\r\n" % len(data) + data)
    sys.stdout.buffer.flush()


stdin = sys.stdin.buffer
while True:
    length = None
    while True:
        line = stdin.readline()
        if not line:
            sys.exit(0)
        if line in (b"\r\n", b"\n"):
            break
        if line.lower().startswith(b"content-length:"):
            length = int(line.split(b":", 1)[1])
    if length is None:
        continue
    body = json.loads(stdin.read(length))
    method = body.get("method")
    if requests_log:
        with open(requests_log, "a") as f:
            f.write(method + "\n")
    if method == "initialize":
        if init_delay:
            time.sleep(init_delay)
        send({"jsonrpc": "2.0", "id": body["id"], "result": {"capabilities": {}}})
    elif method == "shutdown":
        if stubborn:
            continue
        send({"jsonrpc": "2.0", "id": body["id"], "result": None})
    elif method == "exit":
        sys.exit(0)
    elif method == "initialized":
        if ready_delay < 0:
            continue
        if crash == "always" or (crash == "first" and first_launch) or (crash == "after-open" and not first_launch):
            sys.exit(1)
        if ready_delay:
            time.sleep(ready_delay)
        send({"jsonrpc": "2.0", "method": "$/progress",
              "params": {"token": 0, "value": {"kind": "end"}}})
    elif method == "textDocument/didOpen":
        if crash == "after-open":
            sys.exit(1)
`

func newFakeServerInstance(t *testing.T, log, failMarker string, initDelay, readyDelay float64, crash, stubborn string) *instance {
	return newFakeServerInstanceWithLifetime(t, log, failMarker, initDelay, readyDelay, crash, stubborn, context.Background())
}

// newFakeServerInstanceWithLifetime is the same helper with the instance's
// restart lifetime under the test's control: restarts derive their bounded
// start context from it.
func newFakeServerInstanceWithLifetime(t *testing.T, log, failMarker string, initDelay, readyDelay float64, crash, stubborn string, lifetime context.Context) *instance {
	t.Helper()
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	testassert.NoError(t, os.MkdirAll(cacheDir, 0o755))
	script := filepath.Join(cacheDir, "fake-lsp")
	testassert.NoError(t, os.WriteFile(script, []byte(fakeServerScript), 0o755))
	inst := newInstance(&server.Definition{
		Name:    "fake",
		Command: "fake-lsp",
		Args: []string{
			log,
			failMarker,
			strconv.FormatFloat(initDelay, 'f', -1, 64),
			strconv.FormatFloat(readyDelay, 'f', -1, 64),
			crash,
			stubborn,
		},
	}, t.TempDir(), home, lifetime, nil)
	t.Cleanup(func() { inst.shutdown() })
	return inst
}

// newPIDRecordingFakeServer builds the fake server behind a shell wrapper
// that records the launched process's pid before exec'ing the real script, so
// a test can observe whether a process was launched and whether it was
// reaped. The pidfile and the launch log use separate temporary directories.
func newPIDRecordingFakeServer(t *testing.T, initDelay string) (home, pidfile, log string, def *server.Definition) {
	t.Helper()
	home = t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	testassert.NoError(t, os.MkdirAll(cacheDir, 0o755))
	real := filepath.Join(cacheDir, "fake-lsp.py")
	testassert.NoError(t, os.WriteFile(real, []byte(fakeServerScript), 0o755))
	wrapper := filepath.Join(cacheDir, "fake-lsp")
	wrapperScript := "#!/bin/sh\necho $$ > \"${FAKE_PIDFILE:?}\"\nexec python3 \"$(dirname \"$0\")/fake-lsp.py\" \"$@\"\n"
	testassert.NoError(t, os.WriteFile(wrapper, []byte(wrapperScript), 0o755))
	pidfile = filepath.Join(t.TempDir(), "lsp.pid")
	t.Setenv("FAKE_PIDFILE", pidfile)
	log = filepath.Join(t.TempDir(), "launches.log")
	def = &server.Definition{
		Name:    "fake",
		Command: "fake-lsp",
		Args:    []string{log, "", initDelay, "0", "", "1"},
	}
	return home, pidfile, log, def
}

func countLaunches(log string) int {
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte("started\n"))
}

func waitForLaunches(t *testing.T, log string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countLaunches(log) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server process not launched within 10s")
}

func TestWaitReadyConcurrentCallsStartOneProcess(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	// The readiness notification is delayed so the first launch stays in
	// flight while the second caller joins.
	inst := newFakeServerInstance(t, log, "", 0, 0.5, "", "")

	// Barrier: the second caller is released only once the first launch has
	// begun, so it reliably reaches the start decision mid-launch instead of
	// racing it.
	first := make(chan error, 1)
	go func() { first <- inst.waitReady(context.Background()) }()
	waitForLaunches(t, log, 1)

	second := make(chan error, 1)
	go func() { second <- inst.waitReady(context.Background()) }()

	testassert.NoError(t, <-second)
	testassert.NoError(t, <-first)
	testassert.Equal(t, countLaunches(log), 1)
}

func TestWaitReadyFailedStartAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "launches.log")
	failMarker := filepath.Join(dir, "fail")
	inst := newFakeServerInstance(t, log, failMarker, 0, 0, "", "")
	testassert.NoError(t, os.WriteFile(failMarker, nil, 0o644))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = inst.waitReady(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		testassert.Check(t, err != nil, "caller %d: error = nil after failed start, want a failure reported", i)
	}

	// A failed start attempt ends in the idle state, not the failed one: the
	// two outcomes are distinct, and only idle keeps the instance claimable.
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	testassert.Equal(t, state, stateIdle)

	// A failed start attempt must leave the instance usable: once the cause
	// is removed a fresh wait claims a new launch and reaches ready.
	testassert.NoError(t, os.Remove(failMarker))
	testassert.NoError(t, inst.waitReady(context.Background()))
	testassert.Equal(t, countLaunches(log), 1)
}

func TestWaitReadyCancelDoesNotAbortStart(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	// The initialize answer is delayed so the start is provably in flight when
	// the first caller cancels, and the ready notification is delayed so a
	// second caller is still waiting when it lands.
	inst := newFakeServerInstance(t, log, "", 1.0, 0.5, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan error, 1)
	go func() { first <- inst.waitReady(ctx) }()
	waitForLaunches(t, log, 1)

	cancel()
	second := make(chan error, 1)
	go func() { second <- inst.waitReady(context.Background()) }()

	testassert.NoError(t, <-second)
	testassert.Equal(t, <-first, context.Canceled)
	testassert.Equal(t, countLaunches(log), 1)
}

func TestWaitReadyTimeoutReportsStateNotSuccess(t *testing.T) {
	old := readyTimeout
	readyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { readyTimeout = old })

	log := filepath.Join(t.TempDir(), "launches.log")
	// A negative ready delay makes the server never send the work-done
	// progress notification, so the instance stays starting.
	inst := newFakeServerInstance(t, log, "", 0, -1, "", "")

	err := inst.waitReady(context.Background())
	testassert.Require(t, err != nil, "waitReady error = nil, want a timeout reporting the state")
	testassert.Require(t, strings.Contains(err.Error(), "starting"), "waitReady error = %q, want it to name the state", err)
}

func TestWaitReadyRecoversAfterCrashBeforeReady(t *testing.T) {
	// The first launch crashes after initialize and before announcing
	// readiness; the caller of the dying attempt is released with a failure
	// and the instance restarts instead of staying stuck in the crashed
	// launch. A short timeout turns a stuck instance into a fast failure.
	old := readyTimeout
	readyTimeout = 2 * time.Second
	t.Cleanup(func() { readyTimeout = old })

	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "first", "")

	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady error = nil for the crashed launch, want a failure")
	}

	// The restart must recover the instance: a fresh wait reaches ready.
	testassert.NoError(t, inst.waitReady(context.Background()))
	testassert.Equal(t, countLaunches(log), 2)
}

func waitForState(t *testing.T, inst *instance, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		inst.mu.Lock()
		state := inst.state
		inst.mu.Unlock()
		if state == want {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "instance state = %d, want %d", state, want)
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailedInstanceStaysFailed(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	// Every launch crashes before announcing readiness, so the crash loop
	// exhausts the restart budget and the instance ends failed.
	inst := newFakeServerInstance(t, log, "", 0, 0, "always", "")

	// The first wait rides the initial launch and is released with its
	// failure once the attempt dies.
	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady error = nil for the crashed launch, want a failure")
	}
	waitForState(t, inst, stateFailed)

	// A later wait on the failed instance must not launch another process.
	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady on the failed instance: error = nil, want unavailable")
	}
	{
		_, err := inst.start(context.Background())
		testassert.NoError(t, err)
	}
	testassert.Equal(t, countLaunches(log), maxRestarts+1)
}

func TestSupersededWatcherIsInert(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	testassert.NoError(t, inst.waitReady(context.Background()))

	// Reproduce the window in which a shutdown followed by a wait replaces
	// the attempt: kill the running process while holding the lock, so the
	// old watcher wakes to find a newer attempt already installed. The lock
	// hold makes the ordering deterministic — the watcher cannot run its
	// post-exit path until the swap below has landed.
	inst.mu.Lock()
	oldProcDone := inst.procDone
	if err := inst.cmd.Process.Kill(); err != nil {
		inst.mu.Unlock()
		t.Fatalf("kill the running server: %v", err)
	}
	inst.state = stateStarting
	inst.readyCh = make(chan struct{})
	fresh := make(chan struct{})
	close(fresh)
	inst.procDone = fresh
	inst.mu.Unlock()

	// The old watcher wakes once the killed process exits; wait until it has
	// run its post-exit path (it closes the procDone it captured).
	select {
	case <-oldProcDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded watcher never ran")
	}
	// Give a watcher that fails to notice the supersession a moment to apply
	// its accounting, so the assertions below catch it.
	time.Sleep(250 * time.Millisecond)

	inst.mu.Lock()
	state := inst.state
	restarts := inst.restarts
	inst.mu.Unlock()
	testassert.Equal(t, state, stateStarting)
	testassert.Equal(t, restarts, 0)
	testassert.Equal(t, countLaunches(log), 1)
}

func TestStartRefusedWhileReady(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	testassert.NoError(t, inst.waitReady(context.Background()))

	// A launch is already running: starting again must be refused rather than
	// launch a second process over the live one.
	{
		_, err := inst.start(context.Background())
		testassert.NoError(t, err)
	}
	testassert.Equal(t, countLaunches(log), 1)
}

// TestStartAfterPermanentCloseSelfReaps proves the close-loser path: a start
// whose process was already launched when permanent close won must kill and
// reap that process itself — it is the sole cmd.Wait owner before procDone and
// watchProcess are installed — and return the unavailable error, so no
// launched process is ever left untracked. The barrier holds the start
// between cmd.Start and the post-launch attempt-identity recheck, and the
// waitCommand seam counts exactly one wait on this side of the handoff.
func TestStartAfterPermanentCloseSelfReaps(t *testing.T) {
	home, pidfile, log, def := newPIDRecordingFakeServer(t, "0")
	inst := newInstance(def, t.TempDir(), home, context.Background(), nil)
	t.Cleanup(func() { inst.shutdown() })

	// Count every instance-owned process wait through the seam.
	var waits atomic.Int64
	oldWait := waitCommand
	waitCommand = func(cmd *exec.Cmd) error { waits.Add(1); return oldWait(cmd) }
	t.Cleanup(func() { waitCommand = oldWait })

	// Hold the start after cmd.Start and before the attempt-identity recheck.
	launched := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	oldProbe := startProbe
	startProbe = func() { once.Do(func() { close(launched) }); <-release }
	t.Cleanup(func() { startProbe = oldProbe })

	started := make(chan error, 1)
	go func() { _, err := inst.start(context.Background()); started <- err }()
	select {
	case <-launched:
	case <-time.After(10 * time.Second):
		t.Fatal("start never reached the post-launch identity recheck")
	}

	// The process is running when permanent close wins. Wait for the launch
	// log too: the wrapper writes the pidfile before exec'ing the script, so
	// the log write is what proves the server process itself is up.
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countLaunches(log) >= 1 {
			if data, err := os.ReadFile(pidfile); err == nil {
				if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
					pid = p
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	testassert.Require(t, pid != 0, "language server process never launched")

	inst.closeAdmission()
	close(release)

	{
		err := <-started
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "unavailable"), "start after permanent close = %v, want the unavailable error", err)
	}
	// The close loser's process is gone and reaped by the start goroutine
	// itself: kill(pid, 0) returns ESRCH only once reaped.
	{
		err := syscall.Kill(pid, 0)
		testassert.Require(t, errors.Is(err, syscall.ESRCH), "close-loser process %d still alive after start returned (kill err = %v)", pid, err)
	}
	// Exactly one wait happened, and it was this start goroutine's: procDone
	// and the watcher were never installed, so no other waiter exists.
	testassert.Equal(t, waits.Load(), 1)
	inst.mu.Lock()
	state := inst.state
	procDone := inst.procDone
	cmd := inst.cmd
	inst.mu.Unlock()
	if state != stateFailed {
		t.Fatalf("state = %d (%s), want %d (failed)", state, stateName(state), stateFailed)
	}
	testassert.Require(t, procDone == nil && cmd == nil, "close loser installed handles: procDone = %v, cmd = %v", procDone, cmd)
	// Permanent close cannot restart: a later ordinary start is a refused
	// no-op and launches no process.
	{
		_, err := inst.start(context.Background())
		testassert.NoError(t, err)
	}
	testassert.Equal(t, countLaunches(log), 1)
}

// TestOpenFileCanonicalAfterPermanentCloseRefusesWithoutProtocolWrite proves
// the retained-instance terminal admission on the canonical open: after
// permanent close wins on a ready instance, openFileCanonical must return the
// unavailable error before reading the file or sending didOpen — no protocol
// write reaches the server.
func TestOpenFileCanonicalAfterPermanentCloseRefusesWithoutProtocolWrite(t *testing.T) {
	reqLog := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_LSP_REQUESTS_LOG", reqLog)
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	inst.closeAdmission()

	target := filepath.Join(t.TempDir(), "existing.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("content"), 0o600))
	{
		err := inst.openFileCanonical(context.Background(), target)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "unavailable"), "openFileCanonical after permanent close = %v, want the unavailable error", err)
	}
	// No protocol write: the server saw the startup initialize/initialized
	// requests but never a didOpen.
	data, err := os.ReadFile(reqLog)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(data), "textDocument/didOpen"), "openFileCanonical after permanent close sent didOpen to the server")
}

// TestIdleRetirementRestartsWithFreshBudget is the positive idle-retirement
// sibling of permanent close: a ready instance retired by the idle path
// reaches ordinary stateShutdown, and a later ordinary waitReady restarts it
// with a fresh crash budget — the allowed restart row that permanent close's
// final stateFailed must not collapse.
func TestIdleRetirementRestartsWithFreshBudget(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	testassert.NoError(t, inst.waitReady(context.Background()))
	// The idle-retirement path: retire the ready instance without permanent
	// close.
	inst.shutdown()
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state != stateShutdown {
		t.Fatalf("state after idle retirement = %d (%s), want %d (shutdown)", state, stateName(state), stateShutdown)
	}

	// A later ordinary request restarts the retired instance.
	testassert.NoError(t, inst.waitReady(context.Background()))
	inst.mu.Lock()
	state = inst.state
	restarts := inst.restarts
	inst.mu.Unlock()
	if state != stateReady {
		t.Fatalf("state after restart = %d (%s), want %d (ready)", state, stateName(state), stateReady)
	}
	testassert.Equal(t, restarts, 0)
	testassert.Equal(t, countLaunches(log), 2)
}

// TestPermanentCloseWatcherReapsWithoutRestart is the permanent-close watcher
// sibling: close admission wins on a ready instance, then its process exits;
// the watcher reaps it (the sole post-handoff waiter, counted through the
// waitCommand seam) but must not relaunch it, leaving stateFailed and exactly
// one launch. It is the forbidden restart row for permanent close.
func TestPermanentCloseWatcherReapsWithoutRestart(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	// Count every instance-owned process wait through the seam from before
	// the initial launch: the watcher installed by that launch is the only
	// waiter that should ever reap.
	var waits atomic.Int64
	oldWait := waitCommand
	waitCommand = func(cmd *exec.Cmd) error { waits.Add(1); return oldWait(cmd) }
	t.Cleanup(func() { waitCommand = oldWait })

	testassert.NoError(t, inst.waitReady(context.Background()))
	inst.closeAdmission()
	inst.mu.Lock()
	procDone := inst.procDone
	cmd := inst.cmd
	inst.mu.Unlock()
	testassert.Require(t, procDone != nil && cmd != nil, "ready instance has no procDone or cmd")

	// The process exits after permanent close: the watcher reaps it.
	testassert.NoError(t, cmd.Process.Kill())
	select {
	case <-procDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never reaped the killed process")
	}
	// Exactly one wait happened after the procDone/watcher handoff: the
	// watcher's own. No teardown path, restart, or second waiter touched the
	// process.
	testassert.Equal(t, waits.Load(), 1)

	inst.mu.Lock()
	state := inst.state
	restarts := inst.restarts
	inst.mu.Unlock()
	if state != stateFailed {
		t.Fatalf("state = %d (%s), want %d (failed)", state, stateName(state), stateFailed)
	}
	testassert.Equal(t, restarts, 0)
	testassert.Equal(t, countLaunches(log), 1)
	// A later ordinary request refuses: permanent close cannot restart.
	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady after permanent close: error = nil, want unavailable")
	}
	testassert.Equal(t, countLaunches(log), 1)
}

func TestSupersededShutdownLeavesNewAttemptIntact(t *testing.T) {
	old := shutdownWait
	shutdownWait = 200 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })

	log := filepath.Join(t.TempDir(), "launches.log")
	// The stubborn server never answers the shutdown request, so the teardown
	// stays open long enough for a newer attempt to become current.
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "1")

	testassert.NoError(t, inst.waitReady(context.Background()))

	done := make(chan struct{})
	go func() { inst.shutdown(); close(done) }()

	// Wait until shutdown has captured the running attempt (the capture and
	// the terminal transition share one lock hold), then install a newer
	// attempt while shutdown is blocked waiting for the server to answer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		inst.mu.Lock()
		state := inst.state
		inst.mu.Unlock()
		if state == stateShutdown {
			break
		}
		testassert.Require(t, !time.Now().After(deadline), "shutdown never captured the attempt")
		time.Sleep(10 * time.Millisecond)
	}

	inst.mu.Lock()
	inst.state = stateStarting
	inst.readyCh = make(chan struct{})
	fresh := make(chan struct{})
	close(fresh)
	inst.procDone = fresh
	inst.mu.Unlock()

	<-done

	inst.mu.Lock()
	state := inst.state
	rpc := inst.rpc
	cmd := inst.cmd
	procDone := inst.procDone
	inst.mu.Unlock()
	testassert.Equal(t, state, stateStarting)
	testassert.Require(t, rpc != nil && cmd != nil, "shutdown cleared the newer attempt's rpc or cmd")
	testassert.Require(t, procDone == fresh, "shutdown replaced the newer attempt's procDone")
}

// TestSupersededShutdownKeepsReplacementDiagnostics is the stale-attempt
// negative control: a teardown of an attempt that has been superseded must not
// clear the replacement ready attempt's diagnostics. The stubborn server keeps
// the shutdown open while the replacement attempt is installed and its
// diagnostic cache seeded; when the superseded teardown finishes, that cache
// must still hold the seeded error, and the captured attempt's process must
// still be killed and reaped by its own watcher.
func TestSupersededShutdownKeepsReplacementDiagnostics(t *testing.T) {
	old := shutdownWait
	shutdownWait = 200 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })

	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "1")

	// Count every instance-owned process wait from before the initial launch:
	// the captured attempt's watcher is the one that must reap it.
	var waits atomic.Int64
	oldWait := waitCommand
	waitCommand = func(cmd *exec.Cmd) error { waits.Add(1); return oldWait(cmd) }
	t.Cleanup(func() { waitCommand = oldWait })

	testassert.NoError(t, inst.waitReady(context.Background()))
	inst.mu.Lock()
	pid := inst.cmd.Process.Pid
	inst.mu.Unlock()

	done := make(chan struct{})
	go func() { inst.shutdown(); close(done) }()

	// Wait until shutdown has captured the running attempt (the capture and
	// the terminal transition share one lock hold), then install a ready
	// replacement attempt while shutdown is blocked waiting for the server
	// to answer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		inst.mu.Lock()
		state := inst.state
		inst.mu.Unlock()
		if state == stateShutdown {
			break
		}
		testassert.Require(t, !time.Now().After(deadline), "shutdown never captured the attempt")
		time.Sleep(10 * time.Millisecond)
	}

	replacementDone := make(chan struct{})
	inst.mu.Lock()
	inst.state = stateReady
	inst.readyCh = make(chan struct{})
	inst.procDone = replacementDone
	inst.mu.Unlock()
	// The replacement is the only owner of its attempt channel; close it for
	// the helper's cleanup teardown so it never waits on a live attempt.
	t.Cleanup(func() { close(replacementDone) })

	uri := protocol.URIFromPath(filepath.Join(t.TempDir(), "replacement.go"))
	severity := protocol.SeverityError
	inst.diagMu.Lock()
	inst.diagnostics = map[string][]protocol.Diagnostic{
		uri: {{
			Severity: &severity,
			Message:  "replacement error",
			Range:    protocol.Range{Start: protocol.Position{Line: 4}},
		}},
	}
	inst.diagMu.Unlock()

	<-done

	diags := inst.fileDiagnostics(uri)
	testassert.Require(t, len(diags) == 1 && diags[0].Message == "replacement error", "diagnostics after the superseded shutdown = %+v, want the replacement attempt's cached error", diags)
	// The captured attempt's process was killed and reaped inside the
	// teardown: kill(pid, 0) returns ESRCH only once reaped, and the single
	// wait is the watcher's own reap.
	{
		err := syscall.Kill(pid, 0)
		testassert.Require(t, errors.Is(err, syscall.ESRCH), "captured attempt process %d still alive after the superseded shutdown (kill err = %v)", pid, err)
	}
	testassert.Equal(t, waits.Load(), 1)
}

// TestStartWithCancelledContextLaunchesNothing proves the pre-spawn guard: a
// start whose supplied context is already cancelled refuses the attempt
// before binary resolution, pipes or spawn, ends it so the instance stays
// claimable, and returns the context error. The startProbe seam counts
// successful cmd.Start invocations, so the count does not depend on whether
// the child writes its launch log before it is killed.
func TestStartWithCancelledContextLaunchesNothing(t *testing.T) {
	var starts atomic.Int64
	oldProbe := startProbe
	startProbe = func() { starts.Add(1) }
	t.Cleanup(func() { startProbe = oldProbe })

	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	{
		_, err := inst.start(ctx)
		testassert.Require(t, errors.Is(err, context.Canceled), "start with a cancelled context: error = %v, want context.Canceled", err)
	}
	testassert.Equal(t, starts.Load(), 0)
	testassert.Equal(t, countLaunches(log), 0)

	// The refused attempt ended the launch it claimed, so the instance is
	// claimable again: a live start still works.
	testassert.NoError(t, inst.waitReady(context.Background()))
	testassert.Equal(t, countLaunches(log), 1)
}
