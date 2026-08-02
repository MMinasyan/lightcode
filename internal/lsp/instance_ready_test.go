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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

// fakeServerScript is a minimal LSP server over stdio that records every
// launch in a log file. Args: log path, optional fail marker (the server
// exits before logging when the file exists), optional delay in seconds before
// answering initialize, optional delay in seconds before sending the work-done
// progress notification that marks the server ready (a negative delay
// suppresses it entirely), a crash mode ("first" crashes only the very first
// launch, "always" every launch, both after initialize and before readiness),
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
        if crash == "always" or (crash == "first" and first_launch):
            sys.exit(1)
        if ready_delay:
            time.sleep(ready_delay)
        send({"jsonrpc": "2.0", "method": "$/progress",
              "params": {"token": 0, "value": {"kind": "end"}}})
`

func newFakeServerInstance(t *testing.T, log, failMarker string, initDelay, readyDelay float64, crash, stubborn string) *instance {
	t.Helper()
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cacheDir, "fake-lsp")
	if err := os.WriteFile(script, []byte(fakeServerScript), 0o755); err != nil {
		t.Fatal(err)
	}
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
	}, t.TempDir(), home, nil)
	t.Cleanup(func() { inst.shutdown() })
	return inst
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

	if err := <-second; err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first caller: %v", err)
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server processes launched = %d, want 1", n)
	}
}

func TestWaitReadyFailedStartAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "launches.log")
	failMarker := filepath.Join(dir, "fail")
	inst := newFakeServerInstance(t, log, failMarker, 0, 0, "", "")
	if err := os.WriteFile(failMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}

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
		if err == nil {
			t.Errorf("caller %d: error = nil after failed start, want a failure reported", i)
		}
	}

	// A failed start attempt ends in the idle state, not the failed one: the
	// two outcomes are distinct, and only idle keeps the instance claimable.
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state != stateIdle {
		t.Fatalf("instance state after failed start = %d, want %d (idle)", state, stateIdle)
	}

	// A failed start attempt must leave the instance usable: once the cause
	// is removed a fresh wait claims a new launch and reaches ready.
	if err := os.Remove(failMarker); err != nil {
		t.Fatal(err)
	}
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady after failed start: %v", err)
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server processes launched after retry = %d, want 1", n)
	}
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

	if err := <-second; err != nil {
		t.Fatalf("second caller: %v, want ready despite the first caller cancelling", err)
	}
	if err := <-first; err != context.Canceled {
		t.Fatalf("first caller: error = %v, want context.Canceled", err)
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server processes launched = %d, want 1", n)
	}
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
	if err == nil {
		t.Fatal("waitReady error = nil, want a timeout reporting the state")
	}
	if !strings.Contains(err.Error(), "starting") {
		t.Fatalf("waitReady error = %q, want it to name the state", err)
	}
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

	if err := inst.waitReady(context.Background()); err == nil {
		t.Fatal("waitReady error = nil for the crashed launch, want a failure")
	}

	// The restart must recover the instance: a fresh wait reaches ready.
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady after the restart: %v", err)
	}
	if n := countLaunches(log); n != 2 {
		t.Fatalf("server processes launched = %d, want 2 (crashed attempt plus restart)", n)
	}
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
		if time.Now().After(deadline) {
			t.Fatalf("instance state = %d, want %d", state, want)
		}
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
	if err := inst.waitReady(context.Background()); err == nil {
		t.Fatal("waitReady error = nil for the crashed launch, want a failure")
	}
	waitForState(t, inst, stateFailed)

	// A later wait on the failed instance must not launch another process.
	if err := inst.waitReady(context.Background()); err == nil {
		t.Fatal("waitReady on the failed instance: error = nil, want unavailable")
	}
	if _, err := inst.start(context.Background()); err != nil {
		t.Fatalf("start on the failed instance: %v, want a refused no-op", err)
	}
	if n := countLaunches(log); n != maxRestarts+1 {
		t.Fatalf("server processes launched = %d, want %d (the crash loop)", n, maxRestarts+1)
	}
}

func TestSupersededWatcherIsInert(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}

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
	if state != stateStarting {
		t.Fatalf("state = %d, want %d (starting): the superseded watcher touched state", state, stateStarting)
	}
	if restarts != 0 {
		t.Fatalf("restarts = %d, want 0: the superseded watcher counted a crash", restarts)
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server processes launched = %d, want 1: the superseded watcher restarted", n)
	}
}

func TestStartRefusedWhileReady(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")

	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}

	// A launch is already running: starting again must be refused rather than
	// launch a second process over the live one.
	if _, err := inst.start(context.Background()); err != nil {
		t.Fatalf("start on a ready instance: %v, want a refused no-op", err)
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server processes launched = %d, want 1", n)
	}
}

func TestSupersededShutdownLeavesNewAttemptIntact(t *testing.T) {
	old := shutdownWait
	shutdownWait = 200 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })

	log := filepath.Join(t.TempDir(), "launches.log")
	// The stubborn server never answers the shutdown request, so the teardown
	// stays open long enough for a newer attempt to become current.
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "1")

	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}

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
		if time.Now().After(deadline) {
			t.Fatal("shutdown never captured the attempt")
		}
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
	if state != stateStarting {
		t.Fatalf("state = %d, want %d (starting): shutdown ended the newer attempt", state, stateStarting)
	}
	if rpc == nil || cmd == nil {
		t.Fatal("shutdown cleared the newer attempt's rpc or cmd")
	}
	if procDone != fresh {
		t.Fatal("shutdown replaced the newer attempt's procDone")
	}
}
