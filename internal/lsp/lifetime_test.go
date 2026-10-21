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
	"github.com/MMinasyan/lightcode/internal/testassert"
	"golang.org/x/sys/unix"
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
		testassert.Require(t, errors.Is(err, context.Canceled), "install after cancellation = %v, want context.Canceled", err)
	case <-time.After(5 * time.Second):
		t.Fatal("install never returned after the owner context was canceled")
	}
}

// TestRestartDerivesFromInstanceLifetime pins the restart lifetime rule for
// the idle/shutdown restart: a retired instance whose lifetime was canceled
// cannot be resurrected from Background, because its launch context derives
// from the lifetime and is already done. start() refuses it at the pre-spawn
// check before any process is created, so the launch log stays at the initial
// launch.
func TestRestartDerivesFromInstanceLifetime(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	lifetime, cancel := context.WithCancel(context.Background())
	inst := newFakeServerInstanceWithLifetime(t, log, "", 0, 0, "", "", lifetime)

	testassert.NoError(t, inst.waitReady(context.Background()))
	cancel()
	inst.shutdown()
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	if state != stateShutdown {
		t.Fatalf("state after retirement = %d (%s), want %d (shutdown)", state, stateName(state), stateShutdown)
	}

	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady after lifetime cancellation restarted the server; want the restart to fail")
	}
	inst.mu.Lock()
	state = inst.state
	inst.mu.Unlock()
	testassert.Require(t, state != stateReady, "the restart became ready under a canceled lifetime")
	testassert.Equal(t, countLaunches(log), 1)
}

// TestCrashRestartDerivesFromInstanceLifetime pins the crash-restart sibling:
// a ready instance whose lifetime is canceled and whose process then dies
// cannot be resurrected by the watcher's restart.
func TestCrashRestartDerivesFromInstanceLifetime(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	lifetime, cancel := context.WithCancel(context.Background())
	inst := newFakeServerInstanceWithLifetime(t, log, "", 0, 0, "", "", lifetime)

	testassert.NoError(t, inst.waitReady(context.Background()))
	cancel()
	inst.mu.Lock()
	cmd := inst.cmd
	inst.mu.Unlock()
	testassert.Require(t, cmd != nil && cmd.Process != nil, "ready instance has no process")
	testassert.NoError(t, cmd.Process.Kill())
	// The watcher counts the crash and runs its restart, which must fail
	// under the canceled lifetime and settle the instance back to idle.
	waitForRestartSettled(t, inst)
	{
		err := inst.waitReady(context.Background())
		testassert.Require(t, err != nil, "waitReady after the crash restart became ready under a canceled lifetime; want the restart to fail")
	}
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	testassert.Require(t, state != stateReady, "the crash restart became ready under a canceled lifetime")
	testassert.Equal(t, countLaunches(log), 1)
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
		testassert.Require(t, !time.Now().After(deadline), "restart never settled: restarts=%d state=%d", restarts, state)
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
	testassert.Require(t, errors.Is(err, context.Canceled), "WorkspaceSymbol canceled = (%q, %v), want the cancellation error", content, err)
	testassert.Equal(t, content, "")
}

// TestGetDiagnosticsCanonicalCancellationReturnsPartial pins the cancellation
// observation at the sample window: a canceled caller gets the could-not-check
// content accumulated so far with the cancellation error. The instance mutex
// gates the query — it cannot pass its first waitReady while the test holds
// the lock — so the cancellation is issued before the query can reach the
// sample window and the window's cancellation case, the only ready case when
// the query gets there, is its outcome. The healthy file synchronizes (no
// refusal line for it) while the missing leaf accumulates its refusal, so the
// returned partial proves the query reached the window instead of the
// nothing-synchronized gate.
func TestGetDiagnosticsCanonicalCancellationReturnsPartial(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	dir := t.TempDir()
	healthy := filepath.Join(dir, "healthy.go")
	testassert.NoError(t, os.WriteFile(healthy, []byte("package healthy\n"), 0o600))
	missing := filepath.Join(dir, "missing.go")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	inst.mu.Lock()
	go func() {
		defer close(done)
		client := NewClient(m)
		content, err := client.GetDiagnosticsCanonical(ctx, []string{missing, healthy})
		testassert.Check(t, errors.Is(err, context.Canceled), "GetDiagnosticsCanonical canceled = (%q, %v), want the cancellation error", content, err)
		testassert.Check(t, strings.Contains(content, "(could not check)"), "GetDiagnosticsCanonical canceled content = %q, want the partial could-not-check line", content)
		testassert.Check(t, strings.Contains(content, "missing.go") && !strings.Contains(content, "healthy.go"), "GetDiagnosticsCanonical canceled content = %q, want the missing leaf's refusal only", content)
	}()
	cancel()
	inst.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the canceled query never settled")
	}
}

// TestGetDiagnosticsCanonicalRefusesReplacedSymlink pins the canonical read:
// a file replaced by a symlink fails the no-follow open, reports the retained
// could-not-check line through the all-failed error, and never sends the
// substituted content to the server.
func TestGetDiagnosticsCanonicalRefusesReplacedSymlink(t *testing.T) {
	reqLog := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_LSP_REQUESTS_LOG", reqLog)
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	testassert.NoError(t, os.WriteFile(outside, []byte("package outside\n"), 0o600))
	swapped := filepath.Join(dir, "swapped.go")
	testassert.NoError(t, os.WriteFile(swapped, []byte("package swapped\n"), 0o600))
	testassert.NoError(t, os.Remove(swapped))
	testassert.NoError(t, os.Symlink(outside, swapped))

	client := NewClient(m)
	content, err := client.GetDiagnosticsCanonical(context.Background(), []string{swapped})
	testassert.Require(t, err != nil && content == "", "GetDiagnosticsCanonical = (%q, %v), want the all-failed error for the replaced symlink", content, err)
	testassert.Require(t, strings.Contains(err.Error(), "(could not check)"), "GetDiagnosticsCanonical error = %q, want the could-not-check refusal line", err)
	data, err := os.ReadFile(reqLog)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(data), "didOpen") && !strings.Contains(string(data), "didChange"), "canonical read sent document sync for the symlink leaf: %s", data)
}

// TestGetDiagnosticsCanonicalWriterlessFIFOIsCouldNotCheck pins the
// non-blocking leaf open on the canonical read: a writerless FIFO leaf
// cannot block the openat — the no-follow open settles immediately, none
// synchronizes, and the retained could-not-check line is the all-failed error.
func TestGetDiagnosticsCanonicalWriterlessFIFOIsCouldNotCheck(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	fifo := filepath.Join(t.TempDir(), "fifo.go")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	type outcome struct {
		content string
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		client := NewClient(m)
		content, err := client.GetDiagnosticsCanonical(context.Background(), []string{fifo})
		done <- outcome{content, err}
	}()
	select {
	case got := <-done:
		testassert.Require(t, got.err != nil && got.content == "", "GetDiagnosticsCanonical writerless FIFO = (%q, %v), want the all-failed error", got.content, got.err)
		testassert.Require(t, strings.Contains(got.err.Error(), "(could not check)"), "GetDiagnosticsCanonical error = %q, want the could-not-check refusal line", got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("GetDiagnosticsCanonical blocked on the writerless FIFO leaf; want the could-not-check line")
	}
}

// TestGetDiagnosticsCanonicalRefusedDocumentsAreCouldNotCheck pins the
// usable-partial rule for refused documents: a vanished leaf and an
// unsupported path can contribute no cached diagnostics, while a
// synchronizable regular sibling still produces the explicit failed-path lines
// and the sampled result.
func TestGetDiagnosticsCanonicalRefusedDocumentsAreCouldNotCheck(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	dir := t.TempDir()
	healthy := filepath.Join(dir, "healthy.go")
	testassert.NoError(t, os.WriteFile(healthy, []byte("package healthy\n"), 0o600))
	missing := filepath.Join(dir, "gone.go")
	unsupported := filepath.Join(dir, "notes.txt")
	testassert.NoError(t, os.WriteFile(unsupported, []byte("notes\n"), 0o600))
	client := NewClient(m)
	got, err := client.GetDiagnosticsCanonical(context.Background(), []string{missing, unsupported, healthy})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(got, "gone.go") && strings.Contains(got, "notes.txt") && strings.Contains(got, "(could not check)"), "GetDiagnosticsCanonical = %q, want the failed-path lines for the refused documents; GetDiagnosticsCanonical = %q, want the could-not-check rendering of the refused paths", got, got)
}

// TestGetDiagnosticsCanonicalRefusesHardlinkedLeaf pins the hardlink sibling
// of the canonical read: a leaf whose link count is greater than one fails the
// no-follow open, reports the retained could-not-check line through the
// all-failed error, and never sends the aliased content to the server.
func TestGetDiagnosticsCanonicalRefusesHardlinkedLeaf(t *testing.T) {
	reqLog := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_LSP_REQUESTS_LOG", reqLog)
	log := filepath.Join(t.TempDir(), "launches.log")
	inst := newFakeServerInstance(t, log, "", 0, 0, "", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	dir := t.TempDir()
	original := filepath.Join(dir, "original.go")
	testassert.NoError(t, os.WriteFile(original, []byte("package original\n"), 0o600))
	aliased := filepath.Join(dir, "aliased.go")
	testassert.NoError(t, os.Link(original, aliased))

	client := NewClient(m)
	content, err := client.GetDiagnosticsCanonical(context.Background(), []string{aliased})
	testassert.Require(t, err != nil && content == "", "GetDiagnosticsCanonical = (%q, %v), want the all-failed error for the hardlinked leaf", content, err)
	testassert.Require(t, strings.Contains(err.Error(), "(could not check)") && strings.Contains(err.Error(), "hardlink"), "GetDiagnosticsCanonical error = %q, want the could-not-check refusal line", err)
	data, err := os.ReadFile(reqLog)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(data), "didOpen") && !strings.Contains(string(data), "didChange"), "canonical read sent document sync for the hardlinked leaf: %s", data)
}
