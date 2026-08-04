package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestOwnerShutdown verifies owner shutdown closes turn admission,
// joins the in-flight turn (so it always completes rather than hanging on a
// cancelled turn), rejects new work, and is a shared, idempotent join.
func TestOwnerShutdown(t *testing.T) {
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHangingResponse(w, srvCtx)
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilBusy(t, a)

	// ShutdownOwner cancels the in-flight turn and joins it; it must complete.
	done := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ShutdownOwner did not complete: the in-flight-turn join hung")
	}

	// Admission is closed: a new turn is rejected.
	if _, err := a.Submit(ctx, "after"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("Submit after shutdown = %v, want errOwnerClosed", err)
	}

	// Shutdown is a shared, idempotent join: a second call also returns promptly.
	done2 := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("second ShutdownOwner did not return: shared join broken")
	}
}

// fakeLSPObserverScript is a minimal LSP server over stdio that records its pid
// in the file named by FAKE_LSP_PIDFILE and answers the initialize/shutdown/exit
// handshake. It lets the owner-shutdown test observe the LSP teardown
// behaviorally, with no accessor on the manager: when ShutdownOwner returns,
// the pid must already be killed and reaped. It ignores argv (the manager
// passes the definition's --stdio). FAKE_LSP_STUBBORN makes it accept the
// connection and never answer the shutdown request.
const fakeLSPObserverScript = `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["FAKE_LSP_PIDFILE"], "w") as f:
    f.write(str(os.getpid()))

stubborn = os.environ.get("FAKE_LSP_STUBBORN", "") != ""


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
        send({"jsonrpc": "2.0", "id": body["id"], "result": {"capabilities": {}}})
    elif method == "shutdown":
        if stubborn:
            continue
        send({"jsonrpc": "2.0", "id": body["id"], "result": None})
    elif method == "exit":
        sys.exit(0)
    elif method == "initialized":
        send({"jsonrpc": "2.0", "method": "$/progress",
              "params": {"token": 0, "value": {"kind": "end"}}})
`

// startCleanBranchAgentWithFakeLSP builds an agent whose project carries a
// detectable language server — the pyright marker plus a fake
// pyright-langserver on PATH — creates a live session, and waits for the
// server to start and register with the project's LSP manager, so the
// teardown deterministically shuts down a registered instance inside the
// join. stubborn makes the fake server never answer the shutdown request.
// It returns the agent, the server pid, the session id, and the session unit.
func startCleanBranchAgentWithFakeLSP(t *testing.T, home, projectRoot string, stubborn bool) (*Agent, int, string, *session) {
	t.Helper()
	fakeBinDir := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "lsp.pid")
	if err := os.WriteFile(filepath.Join(fakeBinDir, "pyright-langserver"), []byte(fakeLSPObserverScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if stubborn {
		t.Setenv("FAKE_LSP_STUBBORN", "1")
	}
	t.Setenv("FAKE_LSP_PIDFILE", pidfile)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(projectRoot, "requirements.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
	a.SetEventHandler(func(Event) {})
	ctx, cancel := context.WithCancel(context.Background())
	a.Init(ctx)
	t.Cleanup(func() { cancel(); a.ShutdownOwner() })

	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	unit := a.sessions[id]
	if unit == nil || unit.store == nil || !unit.store.Active() {
		t.Fatal("new session has no active store")
	}

	mgr := a.lspManagerFor(projectRoot)
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.AllInstances()) == 1 {
			if data, rerr := os.ReadFile(pidfile); rerr == nil {
				if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
					pid = p
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fake language server never started")
	}
	return a, pid, id, unit
}

// TestOwnerShutdownContractMatrix pins what owner shutdown releases, split by
// join outcome because the two branches require opposite things: after a clean
// shutdown no claim is held, a replacement owner in the same process can claim
// the session, and the LSP teardown has completed; a shutdown whose turn join
// timed out must retain the claims of still-running sessions and detach no
// store — releasing a claim under a live turn would let another process drive
// the same saved session, which is exactly what the active-process marker
// exists to prevent.
func TestOwnerShutdownContractMatrix(t *testing.T) {
	t.Run("join=clean", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()
		a, pid, id, unit := startCleanBranchAgentWithFakeLSP(t, home, projectRoot, false)

		// Clean shutdown: no turn is in flight, so the joins drain and every
		// resource is released.
		a.ShutdownOwner()

		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("language server process %d still alive when owner shutdown returned (kill err = %v): the LSP teardown did not complete inside the join", pid, err)
		}
		if unit.store.Active() {
			t.Fatal("session store still active after clean shutdown")
		}
		lock, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), unit.projectID, id)
		if err != nil {
			t.Fatalf("acquire session claim: %v", err)
		}
		if !ok {
			t.Fatal("session claim still held after clean shutdown")
		}
		_ = lock.Release()

		// A replacement owner in the same process can claim the session again.
		// Remove the detection marker first so the replacement's asynchronous
		// LSP detection starts nothing: its Detect goroutine can outlive this
		// subtest, and after the subtest the test framework has restored PATH,
		// so a still-running detection would miss the fake binary and install
		// the real server into the home being cleaned up.
		if err := os.Remove(filepath.Join(projectRoot, "requirements.txt")); err != nil {
			t.Fatal(err)
		}
		b := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		b.SetEventHandler(func(Event) {})
		bCtx, bCancel := context.WithCancel(context.Background())
		t.Cleanup(func() { bCancel(); b.ShutdownOwner() })
		if resumed := b.Init(bCtx); resumed != id {
			t.Fatalf("replacement owner resumed %q, want %q", resumed, id)
		}
	})

	// The unresponsive case: the language server accepts the connection and
	// answers initialize, but never answers the shutdown request, so the
	// teardown's shutdown call is held until its own bound. Joining the
	// teardown means the owner shutdown is delayed where it previously was
	// not — the deliberate trade of the rewire, which before left the teardown
	// untracked so servers could outlive the owner. What must hold is that the
	// delay stays bounded by the join the teardown is registered on: owner
	// shutdown still returns, within the existing background-join bound,
	// rather than hanging. The delay is acceptable because every host exits
	// immediately after ShutdownOwner returns, and the alternative is a
	// language server that survives the owner.
	t.Run("join=clean/server=unresponsive", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()
		a, _, id, unit := startCleanBranchAgentWithFakeLSP(t, home, projectRoot, true)

		done := make(chan struct{})
		go func() {
			a.ShutdownOwner()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownJoinTimeout + 2*time.Second):
			t.Fatal("owner shutdown hung with an unresponsive language server: the background join must bound the teardown")
		}
		// The clean-join release semantics are undisturbed by the delayed
		// teardown: the store is detached and its claim released.
		if unit.store.Active() {
			t.Fatal("session store still active after clean shutdown")
		}
		lock, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), unit.projectID, id)
		if err != nil {
			t.Fatalf("acquire session claim: %v", err)
		}
		if !ok {
			t.Fatal("session claim still held after clean shutdown")
		}
		_ = lock.Release()
	})

	t.Run("join=timeout", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Hold the model call open until the owner cancels the turn; the
			// request context case releases the handler when the client aborts,
			// and the explicit release lets the server close even when the turn
			// ends without cancelling the request.
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)

		if _, err := a.Submit(ctx, "first"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilBusy(t, a)

		id := a.session.store.SessionID()
		if id == "" {
			t.Fatal("no session id for the running turn")
		}
		unit := a.session
		rt := a.ensureRuntime()
		rt.transcriptMu.Lock()
		tr := rt.transcriptState[id]
		rt.transcriptMu.Unlock()
		if tr == nil {
			t.Fatal("no transcript coordinator for the running session")
		}
		// Wait for the turn's user display row to be delivered before parking
		// the coordinator: delivery runs under seqMu, so observing it proves
		// the drainer is free at park time.
		displayed := func() bool {
			for _, ev := range cap.snapshot() {
				if ev.Kind == EventUserMessageDisplay {
					return true
				}
			}
			return false
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if displayed() {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !displayed() {
			t.Fatal("user display event never delivered")
		}
		// Park the turn's commit feed under the coordinator's seqMu: the turn
		// returns from the provider call once shutdown cancels it, but cannot
		// finish its commit, so the turn join times out and shutdown must
		// retain the store's claim rather than detaching under the still-running
		// turn. The drainer blocks on the same lock only while it dispatches
		// the cancelled turn's interrupted-signal row, so the park is released
		// on the owner context's cancellation — which ShutdownOwner performs
		// only after the turn join times out — letting the background join
		// drain cleanly.
		tr.coord.seqMu.Lock()
		parked := true
		defer func() {
			if parked {
				tr.coord.seqMu.Unlock()
			}
		}()

		done := make(chan struct{})
		go func() {
			a.ShutdownOwner()
			close(done)
		}()
		select {
		case <-rt.ownerCtxDone():
			// The turn join has timed out; release the park so the drainer
			// finishes the interrupted-signal dispatch and the background
			// join drains instead of timing out.
			tr.coord.seqMu.Unlock()
			parked = false
		case <-time.After(shutdownJoinTimeout + 10*time.Second):
			t.Fatal("owner context was not cancelled after the turn join timeout")
		}
		select {
		case <-done:
		case <-time.After(shutdownJoinTimeout + 10*time.Second):
			t.Fatal("ShutdownOwner did not return after the join timeout")
		}

		// The dangerous branch: a still-running turn holds the store, so the
		// claim must be retained and no store detached.
		if !unit.store.Active() {
			t.Fatal("store detached although the turn join timed out")
		}
		lock, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), unit.projectID, id)
		if err != nil {
			t.Fatalf("acquire session claim: %v", err)
		}
		if ok {
			_ = lock.Release()
			t.Fatal("session claim released although the turn join timed out")
		}
	})
}
