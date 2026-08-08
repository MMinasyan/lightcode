package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// silentLSPObserverScript is a minimal LSP server over stdio that records its
// pid in the file named by FAKE_LSP_PIDFILE and then never answers any
// request. A detection running this server stays blocked in the initialize
// call until its context is cancelled, which is what keeps the Detect
// goroutine alive across a test's shutdown window.
const silentLSPObserverScript = `#!/usr/bin/env python3
import os
import sys

with open(os.environ["FAKE_LSP_PIDFILE"], "w") as f:
    f.write(str(os.getpid()))

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
    stdin.read(length)
`

// startDetectionAgent builds an agent whose project carries a detectable
// language server — the pyright marker plus a fake silent pyright-langserver
// on PATH — and swaps the project's LSP manager warning handler for one that
// closes stalled and then blocks until release is closed. The manager already
// exists (the constructor creates it), so Init's detection start covers it.
// The swapped handler blocks the Detect goroutine in its terminal stretch —
// the start-failure warning after its context is cancelled — which is what
// lets a test observe shutdown's background join waiting on an in-flight
// detection. It returns the agent, the pidfile the fake server writes at
// start, the stalled channel, and the release function.
func startDetectionAgent(t *testing.T, home, projectRoot string) (*Agent, string, <-chan struct{}, func()) {
	t.Helper()
	fakeBinDir := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "lsp.pid")
	if err := os.WriteFile(filepath.Join(fakeBinDir, "pyright-langserver"), []byte(silentLSPObserverScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LSP_PIDFILE", pidfile)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(projectRoot, "requirements.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
	a.SetEventHandler(func(Event) {})
	stalled := make(chan struct{})
	releaseCh := make(chan struct{})
	var stalledOnce, releaseOnce sync.Once
	a.lspManagerFor(projectRoot).SetWarningHandler(func(string, string) {
		stalledOnce.Do(func() { close(stalled) })
		<-releaseCh
	})
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	return a, pidfile, stalled, release
}

// TestDetectRunningAtShutdownIsJoined asserts that a detection still running
// when shutdown starts is joined rather than abandoned. The fake server never
// answers, so the Detect goroutine is blocked in the initialize call when
// shutdown begins; its swapped warning handler then blocks it in the terminal
// stretch after the owner context is cancelled, and the background join must
// wait on that block — ShutdownOwner must not return while the detection is
// still running. The ordering is asserted directly: the shutdown barrier
// fires while detection is provably still in flight, and shutdown is observed
// to wait on the blocked handler before it is released.
func TestDetectRunningAtShutdownIsJoined(t *testing.T) {
	home, projectRoot := t.TempDir(), t.TempDir()
	a, pidfile, stalled, release := startDetectionAgent(t, home, projectRoot)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	t.Cleanup(func() { release(); a.ShutdownOwner() })

	// Detection is in flight once the fake server is up: the script records
	// its pid at start and then holds the initialize call unanswered.
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidfile); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
				pid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fake language server never started")
	}

	// The shutdown barrier fires one statement before closed is published,
	// and the owner context is cancelled only after that, so the in-flight
	// detection must not have completed yet: its terminal warning must not
	// have fired.
	shutdownStarted := make(chan struct{})
	a.shutdownBarrierHook = func() { close(shutdownStarted) }
	defer func() { a.shutdownBarrierHook = nil }()
	shutdownDone := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(shutdownDone)
	}()
	<-shutdownStarted
	select {
	case <-stalled:
		t.Fatal("detection completed before shutdown started: nothing was in flight to join")
	default:
	}

	// The detection reaches its terminal stretch — the blocked warning
	// handler — and the background join must wait on it: ShutdownOwner must
	// not return while the Detect goroutine is still running.
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("detection never reached its terminal stretch")
	}
	select {
	case <-shutdownDone:
		t.Fatal("ShutdownOwner returned while detection was still running: the Detect goroutine was abandoned rather than joined")
	case <-time.After(2 * time.Second):
	}
	release()
	<-shutdownDone

	// The joined detection finished its cleanup inside the join: the fake
	// server process is dead and reaped when shutdown returns.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("language server process %d still alive when owner shutdown returned: detection cleanup did not complete inside the join", pid)
	}
	if !a.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}
}

// TestInitWithCancelledHostContextJoinsDetection asserts that detection is
// registered on the background group and joined by shutdown even when the
// host context is already cancelled: the cancelled context drives shutdown
// while the detection blocks in its swapped warning handler, and the join
// must wait on it. The ordering of the watcher spawn against the detection
// block in initOnceLocked — what makes the race structurally impossible
// rather than benignly won — is asserted by
// TestInitRegistersDetectionBeforeHostCancelWatcher, not re-established
// here.
func TestInitWithCancelledHostContextJoinsDetection(t *testing.T) {
	home, projectRoot := t.TempDir(), t.TempDir()
	a, pidfile, stalled, release := startDetectionAgent(t, home, projectRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Init(ctx)
	t.Cleanup(func() { release(); a.ShutdownOwner() })

	// The cancelled host context has driven ShutdownOwner by now; the
	// detection started under it fails immediately and blocks in the warning
	// handler, and the background join must still be waiting on it.
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("detection never started under Init")
	}
	shutdownDone := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("ShutdownOwner returned while detection was still running: the Detect goroutine was abandoned rather than joined")
	case <-time.After(2 * time.Second):
	}
	release()
	<-shutdownDone

	if data, err := os.ReadFile(pidfile); err == nil {
		if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
			if err := syscall.Kill(p, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("language server process %d still alive when owner shutdown returned: detection cleanup did not complete inside the join", p)
			}
		}
	}
	if !a.ShutdownOwner() {
		t.Fatal("clean shutdown reported abandoned in-flight work")
	}
}

// TestInitRegistersDetectionBeforeHostCancelWatcher pins the ordering of the
// two blocks in initOnceLocked: detection registers on the background group
// before the host-cancel watcher goroutine is started, so an already-cancelled
// host context cannot let the watcher run ShutdownOwner — and join the
// background group — before detection has registered on it. Exception,
// recorded per the contract-test rule: the wait group's counter is unreadable,
// so the registration leaves no trace a behavioural test could observe, and
// the ordering is pinned structurally on initOnceLocked's own body — the file
// mentions both markers elsewhere, so a file-wide comparison would not be
// measuring this function. Reversing the two blocks fails the test.
func TestInitRegistersDetectionBeforeHostCancelWatcher(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	body, ok := extractFunctionBody(string(src), "func (rt *runtime) initOnceLocked(")
	if !ok {
		t.Fatal("initOnceLocked not found")
	}
	det := strings.Index(body, "a.startDetectLocked(e)")
	watcher := strings.Index(body, "a.ShutdownOwner()")
	if det < 0 || watcher < 0 || det > watcher {
		t.Fatalf("initOnceLocked must register detection (a.startDetectLocked) before starting the host-cancel watcher (a.ShutdownOwner); detection@%d watcher@%d", det, watcher)
	}
}

// extractFunctionBody returns the brace-delimited body of the first function
// whose definition line starts with prefix. It does not understand strings or
// comments containing braces, so callers should pass production code only.
func extractFunctionBody(source, prefix string) (string, bool) {
	idx := strings.Index(source, prefix)
	if idx < 0 {
		return "", false
	}
	rest := source[idx:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 1
	for i := open + 1; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open+1 : i], true
			}
		}
	}
	return "", false
}
