package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

func TestManagerForFileAndAllInstances(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	if got := m.ForFile("main.go"); got != nil {
		t.Fatalf("ForFile without instance = %+v, want nil", got)
	}
	if got := m.ForFile("README"); got != nil {
		t.Fatalf("ForFile without extension = %+v, want nil", got)
	}
	if got := m.ForFile("file.unknown"); got != nil {
		t.Fatalf("ForFile unknown extension = %+v, want nil", got)
	}

	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, nil)
	m.instances[def.Name] = inst
	if got := m.ForFile("main.go"); got != inst {
		t.Fatalf("ForFile(main.go) = %+v, want inserted instance", got)
	}
	all := m.AllInstances()
	if len(all) != 1 || all[0] != inst {
		t.Fatalf("AllInstances = %+v, want inserted instance", all)
	}
}

func TestManagerConcurrentHandlersAndInstances(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, nil)
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	var warnings atomic.Int64
	var signals atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.SetWarningHandler(func(string, string) { warnings.Add(1) })
				m.SetSignalHandler(func(string) { signals.Add(1) })
				m.emitWarning("kind", "message")
				m.emitSignal("signal")
				_ = m.ForFile("main.go")
				_ = m.AllInstances()
			}
		}()
	}
	wg.Wait()
	if warnings.Load() == 0 || signals.Load() == 0 {
		t.Fatalf("handlers were not called: warnings=%d signals=%d", warnings.Load(), signals.Load())
	}
}

func TestManagerWarningAndSignalHandlers(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	var warningKind, warningMessage, signal string
	m.SetWarningHandler(func(kind, message string) { warningKind, warningMessage = kind, message })
	m.SetSignalHandler(func(content string) { signal = content })

	m.emitWarning("kind", "message")
	if warningKind != "kind" || warningMessage != "message" {
		t.Fatalf("warning = %q/%q", warningKind, warningMessage)
	}
	m.emitSignal("hello")
	if signal != "hello" {
		t.Fatalf("signal = %q, want raw payload", signal)
	}
}

func TestManagerShutdownAllKillsUnresponsiveServers(t *testing.T) {
	// Two servers never answer the shutdown request and one answers it, so
	// whichever order the manager map is walked, the teardown meets an
	// unresponsive server and must still tear down every instance.
	type server struct {
		inst     *instance
		pid      int
		stubborn bool
	}
	servers := make([]server, 3)
	for i := range servers {
		log := filepath.Join(t.TempDir(), "launches.log")
		stubborn := ""
		if i < 2 {
			stubborn = "1"
		}
		inst := newFakeServerInstance(t, log, "", 0, 0, "", stubborn)
		if err := inst.waitReady(context.Background()); err != nil {
			t.Fatalf("waitReady server %d: %v", i, err)
		}
		servers[i] = server{inst: inst, pid: inst.cmd.Process.Pid, stubborn: stubborn == "1"}
	}

	m := NewManager(t.TempDir(), t.TempDir())
	for i, s := range servers {
		// The manager map is keyed by def name in production, but any key
		// works here; all three definitions are named "fake" so the shared
		// fake binary resolves.
		m.instances[fmt.Sprintf("fake-%d", i)] = s.inst
	}

	start := time.Now()
	m.ShutdownAll()
	elapsed := time.Since(start)
	t.Logf("ShutdownAll with %d unresponsive servers took %s", 2, elapsed)
	// The negotiate path waits shutdownWait per unresponsive instance before
	// killing it; the teardown must kill instead, well below that.
	if elapsed >= 2*time.Second {
		t.Fatalf("ShutdownAll took %s, want a prompt teardown (the negotiate path waited %s per unresponsive server)", elapsed, shutdownWait)
	}

	for i, s := range servers {
		// The process must be gone and reaped, not merely left to the owner's
		// own process exit: kill(pid, 0) returns ESRCH only once reaped.
		if err := syscall.Kill(s.pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("server %d (stubborn=%v) process %d still alive after ShutdownAll: kill err = %v", i, s.stubborn, s.pid, err)
		}
		s.inst.mu.Lock()
		state := s.inst.state
		rpc := s.inst.rpc
		cmd := s.inst.cmd
		s.inst.mu.Unlock()
		if state != stateShutdown {
			t.Errorf("server %d state = %d (%s), want %d (shutdown)", i, state, stateName(state), stateShutdown)
		}
		if rpc != nil || cmd != nil {
			t.Errorf("server %d handles not cleared: rpc = %v, cmd = %v", i, rpc, cmd)
		}
	}
}

func TestStartServerAfterClosedKillsInsteadOfNegotiating(t *testing.T) {
	// A server that finishes starting and only then discovers the manager is
	// closed must be disposed of the same way the teardown does: killed and
	// reaped, not negotiated with. The server never answers the shutdown
	// request, so the old negotiate path would be held for its full bound
	// after the owner has stopped waiting for anything.
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cacheDir, "fake-lsp.py")
	if err := os.WriteFile(real, []byte(fakeServerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// A wrapper records the launched process's pid before exec'ing the real
	// script, so the test can assert the process is actually gone and reaped.
	wrapper := filepath.Join(cacheDir, "fake-lsp")
	wrapperScript := "#!/bin/sh\necho $$ > \"${FAKE_PIDFILE:?}\"\nexec python3 \"$(dirname \"$0\")/fake-lsp.py\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "lsp.pid")
	t.Setenv("FAKE_PIDFILE", pidfile)

	log := filepath.Join(t.TempDir(), "launches.log")
	def := &server.Definition{
		Name:    "fake",
		Command: "fake-lsp",
		Args:    []string{log, "", "0", "0", "", "1"},
	}

	m := NewManager(t.TempDir(), home)
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	start := time.Now()
	m.startServer(context.Background(), def)
	elapsed := time.Since(start)
	t.Logf("startServer after closed with an unresponsive server took %s", elapsed)
	// The negotiate path waits shutdownWait before killing; this path must
	// kill instead, well below that.
	if elapsed >= 2*time.Second {
		t.Fatalf("startServer after closed took %s, want a prompt teardown (the negotiate path waited %s)", elapsed, shutdownWait)
	}

	// The process must be gone and reaped, not merely left to the owner's own
	// process exit: kill(pid, 0) returns ESRCH only once reaped.
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid = %q, err = %v", data, err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("language server process %d still alive after startServer returned (kill err = %v)", pid, err)
	}
}
