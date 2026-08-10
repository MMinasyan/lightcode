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
		// Permanent close lands every instance in stateFailed (final and
		// unclaimable), not the restartable idle-retirement shutdown state.
		if state != stateFailed {
			t.Errorf("server %d state = %d (%s), want %d (failed)", i, state, stateName(state), stateFailed)
		}
		if rpc != nil || cmd != nil {
			t.Errorf("server %d handles not cleared: rpc = %v, cmd = %v", i, rpc, cmd)
		}
	}
}

func TestStartServerAfterClosedRejectsBeforeStart(t *testing.T) {
	// A server whose start would only be discovered after the fact must be
	// rejected before instance.start: permanent close refuses before OS start,
	// so no process is ever launched after close. The old launched-then-killed
	// path is gone.
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
	// script, so the test can assert no process was ever launched.
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
	t.Logf("startServer after closed took %s", elapsed)
	// Rejection happens before any start or negotiation, so it is prompt.
	if elapsed >= 2*time.Second {
		t.Fatalf("startServer after closed took %s, want an immediate rejection", elapsed)
	}

	// No process was ever launched: the wrapper never wrote the pidfile.
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("pidfile exists after rejected start: a process was launched after close (stat err = %v)", err)
	}
	// And no instance was registered.
	m.mu.Lock()
	_, mapped := m.instances[def.Name]
	m.mu.Unlock()
	if mapped {
		t.Fatal("startServer after closed registered an instance")
	}
}

// TestStartServerAfterClosedSkipsInstall covers the LSP close-before-install
// sibling: a server whose binary is missing and whose Install would otherwise
// run must be rejected by the early admission check before any resource or
// process start — no Install call, no instance mapping, no warning or signal,
// and no process launch. The map-time closed check stays as the positive
// sibling for starts admitted before close (TestStartServerCloseRaceMapped
// InstanceIsTornDown).
func TestStartServerAfterClosedSkipsInstall(t *testing.T) {
	home := t.TempDir() // no fake binary anywhere: ResolveBinary finds nothing
	installed := false
	def := &server.Definition{
		Name:    "missing",
		Command: "missing-lsp",
		Args:    []string{},
		Install: func(cacheDir string) error {
			installed = true
			return nil
		},
	}

	m := NewManager(t.TempDir(), home)
	var warnings, signals atomic.Int64
	m.SetWarningHandler(func(string, string) { warnings.Add(1) })
	m.SetSignalHandler(func(string) { signals.Add(1) })

	m.CloseAdmission()
	start := time.Now()
	m.startServer(context.Background(), def)
	elapsed := time.Since(start)
	t.Logf("startServer after CloseAdmission with missing binary took %s", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("startServer after CloseAdmission took %s, want an immediate rejection", elapsed)
	}
	if installed {
		t.Fatal("Install was called after CloseAdmission: the early admission check did not reject")
	}
	if warnings.Load() != 0 || signals.Load() != 0 {
		t.Fatalf("warnings=%d signals=%d after rejected start, want none", warnings.Load(), signals.Load())
	}
	m.mu.Lock()
	_, mapped := m.instances[def.Name]
	m.mu.Unlock()
	if mapped {
		t.Fatal("startServer after CloseAdmission mapped an instance")
	}
}

// TestStartServerCloseRaceProvisionalMappingHoldsTerminalAdmission proves the
// deterministic barrier after provisional mapping: with the start held
// at the post-handoff probe, permanent close (ShutdownAll) returns promptly
// while the start is still blocked, and once released the start cannot launch
// an OS process (no pidfile) because the mapped instance is already terminal
// stateFailed. Removing the provisional map insertion makes close miss the
// instance and the released start launch it — the regression this test pins.
func TestStartServerCloseRaceProvisionalMappingHoldsTerminalAdmission(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cacheDir, "fake-lsp.py")
	if err := os.WriteFile(real, []byte(fakeServerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// The wrapper records the launched process's pid before exec'ing the real
	// script, so the test can assert no process was ever launched.
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
	t.Cleanup(func() { m.ShutdownAll() })

	// Hold the start at the post-handoff probe.
	probeEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	oldProbe := startServerProbe
	startServerProbe = func() { once.Do(func() { close(probeEntered) }); <-release }
	t.Cleanup(func() { startServerProbe = oldProbe })

	started := make(chan struct{})
	go func() {
		m.startServer(context.Background(), def)
		close(started)
	}()
	select {
	case <-probeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("startServer never reached the post-handoff probe")
	}

	// Permanent close while the start is blocked after the handoff must
	// return promptly and must not wait on the probe.
	start := time.Now()
	m.ShutdownAll()
	elapsed := time.Since(start)
	t.Logf("ShutdownAll with a probe-blocked start took %s", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("ShutdownAll took %s, want a prompt permanent close", elapsed)
	}

	close(release)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("startServer did not return after the probe was released")
	}

	// The released start could not launch an OS process: the mapped instance
	// was already terminal, so inst.start refused before cmd.Start.
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("pidfile exists: the released start launched a process after permanent close (stat err = %v)", err)
	}
	if n := countLaunches(log); n != 0 {
		t.Fatalf("server processes launched = %d, want 0", n)
	}
	m.mu.Lock()
	inst := m.instances[def.Name]
	m.mu.Unlock()
	if inst == nil {
		t.Fatal("provisionally mapped instance missing after permanent close")
	}
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state != stateFailed {
		t.Fatalf("mapped instance state = %d (%s), want %d (failed)", state, stateName(state), stateFailed)
	}
}

// TestStartServerCloseRaceMappedInstanceIsTornDown covers the "LSP start wins
// before permanent close" axis: a server whose start was admitted (the
// instance is provisionally mapped) before ShutdownAll begins is in the close
// snapshot, so ShutdownAll must kill and reap its process before returning —
// even though the start itself is still in flight (blocked in initialize).
// On the old branch the instance was mapped only after start completed, so
// ShutdownAll's snapshot missed it and the process survived the call.
func TestStartServerCloseRaceMappedInstanceIsTornDown(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", "fake")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cacheDir, "fake-lsp.py")
	if err := os.WriteFile(real, []byte(fakeServerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(cacheDir, "fake-lsp")
	wrapperScript := "#!/bin/sh\necho $$ > \"${FAKE_PIDFILE:?}\"\nexec python3 \"$(dirname \"$0\")/fake-lsp.py\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "lsp.pid")
	t.Setenv("FAKE_PIDFILE", pidfile)

	// init_delay holds the start in the initialize call after the process has
	// launched and the instance has been provisionally mapped.
	log := filepath.Join(t.TempDir(), "launches.log")
	def := &server.Definition{
		Name:    "fake",
		Command: "fake-lsp",
		Args:    []string{log, "", "2", "0", "", "1"},
	}

	m := NewManager(t.TempDir(), home)
	started := make(chan struct{})
	go func() {
		m.startServer(context.Background(), def)
		close(started)
	}()

	// Wait for the provisional mapping: the admission handoff that puts the
	// mid-start instance into the close snapshot.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, mapped := m.instances[def.Name]
		m.mu.Unlock()
		if mapped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.mu.Lock()
	_, mapped := m.instances[def.Name]
	m.mu.Unlock()
	if !mapped {
		t.Fatal("startServer never mapped the instance")
	}
	// The process is running (the wrapper wrote the pidfile).
	var pid int
	deadline = time.Now().Add(5 * time.Second)
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
		t.Fatal("language server process never launched")
	}

	start := time.Now()
	m.ShutdownAll()
	elapsed := time.Since(start)
	t.Logf("ShutdownAll with a mid-start mapped server took %s", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("ShutdownAll took %s, want a prompt teardown (no negotiation with the unresponsive server)", elapsed)
	}

	// The mapped mid-start instance's process is killed and reaped inside the
	// call: kill(pid, 0) returns ESRCH only once reaped.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("mid-start server process %d still alive after ShutdownAll returned (kill err = %v)", pid, err)
	}
	// The instance ended terminal and the failed start cleans the mapping.
	<-started
	m.mu.Lock()
	_, stillMapped := m.instances[def.Name]
	m.mu.Unlock()
	if stillMapped {
		t.Fatal("failed start left its instance mapped")
	}
}
