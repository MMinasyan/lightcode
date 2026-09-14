package lsp

// The owner-lifetime contract tests: the install context threads through the
// manager, instance restarts derive their bounded start context from the
// context that started the instance, the shared client queries return
// observed caller cancellation with their partial content instead of
// swallowing it, and the canonical diagnostics read refuses unsafe leaves
// without substituted content.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

func TestManagerInstallThreadsContext(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	def := &server.Definition{
		Name: "fake",
		Install: func(installCtx context.Context, cacheDir string) error {
			close(started)
			<-installCtx.Done()
			return installCtx.Err()
		},
	}

	done := make(chan error, 1)
	go func() { done <- m.install(ctx, def) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the installer")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("install after cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("install never returned after the owner context was canceled")
	}
}

// TestRestartDerivesFromInstanceLifetime pins the restart lifetime rule for
// the idle/shutdown restart: a retired instance whose lifetime was canceled
// fails its restart instead of resurrecting the server from Background. The
// doomed restart's process dies before the server script can log a launch,
// so the launch log stays at the initial launch.
func TestRestartDerivesFromInstanceLifetime(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	lifetime, cancel := context.WithCancel(context.Background())
	inst := newFakeServerInstanceWithLifetime(t, log, "", 0, 0, "", "", lifetime)

	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("initial waitReady: %v", err)
	}
	cancel()
	inst.shutdown()
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state != stateShutdown {
		t.Fatalf("state after retirement = %d (%s), want %d (shutdown)", state, stateName(state), stateShutdown)
	}

	if err := inst.waitReady(context.Background()); err == nil {
		t.Fatal("waitReady after lifetime cancellation restarted the server; want the restart to fail")
	}
	inst.mu.Lock()
	state = inst.state
	inst.mu.Unlock()
	if state == stateReady {
		t.Fatal("the restart became ready under a canceled lifetime")
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server launches logged = %d, want 1: the restart under the canceled lifetime never served", n)
	}
}

// TestCrashRestartDerivesFromInstanceLifetime pins the crash-restart sibling:
// a ready instance whose lifetime is canceled and whose process then dies
// cannot be resurrected by the watcher's restart.
func TestCrashRestartDerivesFromInstanceLifetime(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	lifetime, cancel := context.WithCancel(context.Background())
	inst := newFakeServerInstanceWithLifetime(t, log, "", 0, 0, "", "", lifetime)

	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("initial waitReady: %v", err)
	}
	cancel()
	inst.mu.Lock()
	cmd := inst.cmd
	inst.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("ready instance has no process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the ready server: %v", err)
	}
	// The watcher counts the crash and runs its restart, which must fail
	// under the canceled lifetime and settle the instance back to idle.
	waitForRestartSettled(t, inst)
	if err := inst.waitReady(context.Background()); err == nil {
		t.Fatal("waitReady after the crash restart became ready under a canceled lifetime; want the restart to fail")
	}
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state == stateReady {
		t.Fatal("the crash restart became ready under a canceled lifetime")
	}
	if n := countLaunches(log); n != 1 {
		t.Fatalf("server launches logged = %d, want 1: the crash restart under the canceled lifetime never served", n)
	}
}

// waitForRestartSettled waits until the crash watcher has counted its restart
// and the instance has settled out of the killed attempt.
func waitForRestartSettled(t *testing.T, inst *instance) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		inst.mu.Lock()
		restarts, state := inst.restarts, inst.state
		inst.mu.Unlock()
		if restarts == 1 && state == stateIdle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart never settled: restarts=%d state=%d", restarts, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWorkspaceSymbolReturnsCanceledNotSwallowed pins the cancellation
// observation at the waitReady point: a canceled caller gets the symbols
// collected so far (none) with the cancellation error, never the settled
// no-symbols content.
func TestWorkspaceSymbolReturnsCanceledNotSwallowed(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, context.Background(), nil)
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()
	// A launch is permanently under way: the caller's waitReady blocks until
	// its own cancellation.
	inst.mu.Lock()
	inst.state = stateStarting
	inst.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewClient(m)
	content, err := client.WorkspaceSymbol(ctx, "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WorkspaceSymbol canceled = (%q, %v), want the cancellation error", content, err)
	}
	if content != "" {
		t.Fatalf("WorkspaceSymbol canceled content = %q, want the empty partial", content)
	}
}

// TestGetDiagnosticsCancellationReturnsPartial pins the cancellation
// observation at the sync wait: a canceled caller gets the could-not-check
// content accumulated so far with the cancellation error.
func TestGetDiagnosticsCancellationReturnsPartial(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	missing := filepath.Join(t.TempDir(), "missing.go")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client := NewClient(m)
		content, err := client.GetDiagnostics(ctx, []string{missing})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetDiagnostics canceled = (%q, %v), want the cancellation error", content, err)
		}
		if !strings.Contains(content, "(could not check)") {
			t.Errorf("GetDiagnostics canceled content = %q, want the partial could-not-check line", content)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

// TestGetDiagnosticsCanonicalRefusesReplacedSymlink pins the canonical read:
// a file replaced by a symlink fails the no-follow open, reports the retained
// could-not-check line, and never sends the substituted content to the
// server. The legacy reader on the same tree follows the symlink and syncs
// the target's content — the difference between the two readers.
func TestGetDiagnosticsCanonicalRefusesReplacedSymlink(t *testing.T) {
	reqLog := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_LSP_REQUESTS_LOG", reqLog)
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped := filepath.Join(dir, "swapped.go")
	if err := os.WriteFile(swapped, []byte("package swapped\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(swapped); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, swapped); err != nil {
		t.Fatal(err)
	}

	client := NewClient(m)
	canonical, err := client.GetDiagnosticsCanonical(context.Background(), []string{swapped})
	if err != nil {
		t.Fatalf("GetDiagnosticsCanonical: %v", err)
	}
	if !strings.Contains(canonical, "(could not check)") {
		t.Fatalf("GetDiagnosticsCanonical = %q, want the could-not-check line for the replaced symlink", canonical)
	}
	data, err := os.ReadFile(reqLog)
	if err != nil {
		t.Fatalf("read requests log: %v", err)
	}
	if strings.Contains(string(data), "didOpen") || strings.Contains(string(data), "didChange") {
		t.Fatalf("canonical read sent document sync for the symlink leaf: %s", data)
	}

	// The legacy reader keeps its current behavior: it follows the symlink
	// and syncs the target's content.
	legacy, err := client.GetDiagnostics(context.Background(), []string{swapped})
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	data, err = os.ReadFile(reqLog)
	if err != nil {
		t.Fatalf("read requests log: %v", err)
	}
	if !strings.Contains(string(data), "didOpen") && !strings.Contains(string(data), "didChange") {
		t.Fatalf("legacy read never synced the symlinked document (result %q)", legacy)
	}
}

// TestGetDiagnosticsCanonicalMissingLeafIsCouldNotCheck pins the missing-file
// sibling of the canonical read: a vanished leaf reports could-not-check
// rather than any substitute.
func TestGetDiagnosticsCanonicalMissingLeafIsCouldNotCheck(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	missing := filepath.Join(t.TempDir(), "gone.go")
	client := NewClient(m)
	got, err := client.GetDiagnosticsCanonical(context.Background(), []string{missing})
	if err != nil {
		t.Fatalf("GetDiagnosticsCanonical: %v", err)
	}
	if !strings.Contains(got, "(could not check)") {
		t.Fatalf("GetDiagnosticsCanonical missing leaf = %q, want the could-not-check line", got)
	}
	// The legacy reader reports the same retained could-not-check shape for
	// the same missing leaf — the shared body's one rendering, with each
	// reader's own underlying error text.
	legacy, err := client.GetDiagnostics(context.Background(), []string{missing})
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	if !strings.Contains(legacy, "(could not check)") || !strings.Contains(legacy, "no such file") {
		t.Fatalf("GetDiagnostics missing leaf = %q, want the retained could-not-check rendering", legacy)
	}
}
