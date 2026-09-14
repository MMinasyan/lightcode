package lsp

// The plugin contract tests. The workspace registry, detection-once
// semantics, binding revalidation, outcome mapping and the content cap are
// exercised against a real fake language server: the newManager seam
// redirects every workspace manager at a test home whose cache holds a
// planted fake gopls binary, so detection starts a real stdio server and the
// shared client queries it end to end. The server logs every launch and
// request as "method uri" lines, which is how the tests observe exactly
// which files were read and synced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// fakeServerTemplate is a minimal LSP server over stdio that logs every
// launch and request as "method uri" lines. {{LOG}} is the log path, {{DELAY}}
// the initialize delay in seconds (holds detection), and {{CRASH}} a crash
// mode ("1" exits after initialize, before readiness — the crash loop drives
// the instance into the terminal failed state).
const fakeServerTemplate = `#!/usr/bin/env python3
import json, sys, time

log = "{{LOG}}"
init_delay = {{DELAY}}
crash = "{{CRASH}}"

def send(obj):
    data = json.dumps(obj, separators=(",", ":")).encode()
    sys.stdout.buffer.write(b"Content-Length: %d\r\n\r\n" % len(data) + data)
    sys.stdout.buffer.flush()

with open(log, "a") as f:
    f.write("started\n")

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
    entry = method or "response"
    params = body.get("params") or {}
    doc = params.get("textDocument") if isinstance(params, dict) else None
    if isinstance(doc, dict) and "uri" in doc:
        entry += " " + doc["uri"]
    with open(log, "a") as f:
        f.write(entry + "\n")
    if method == "initialize":
        if init_delay:
            time.sleep(init_delay)
        send({"jsonrpc": "2.0", "id": body["id"], "result": {"capabilities": {}}})
    elif method == "initialized":
        if crash:
            sys.exit(1)
        send({"jsonrpc": "2.0", "method": "$/progress", "params": {"token": 0, "value": {"kind": "end"}}})
    elif method == "workspace/symbol":
        send({"jsonrpc": "2.0", "id": body["id"], "result": [
            {"name": "Foo", "kind": 12, "location": {"uri": "file:///w/foo.go", "range": {"start": {"line": 3}}}}
        ]})
    elif method == "shutdown":
        send({"jsonrpc": "2.0", "id": body["id"], "result": None})
    elif method == "exit":
        sys.exit(0)
`

// managerSpy records every workspace-manager construction the plugin makes
// through the newManager seam. With fakeHome nonempty it also redirects the
// manager at the fake server's home, so detection resolves the planted
// binary instead of anything on the host.
type managerSpy struct {
	mu    sync.Mutex
	roots []string
	homes []string
}

func (s *managerSpy) install(t *testing.T, fakeHome string) {
	t.Helper()
	old := newManager
	newManager = func(root, home string) *lsp.Manager {
		s.mu.Lock()
		s.roots = append(s.roots, root)
		s.homes = append(s.homes, home)
		s.mu.Unlock()
		if fakeHome != "" {
			return lsp.NewManager(root, fakeHome)
		}
		return lsp.NewManager(root, home)
	}
	t.Cleanup(func() { newManager = old })
}

func (s *managerSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.roots)
}

func (s *managerSpy) root() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roots[0]
}

func (s *managerSpy) home() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.homes[0]
}

// fakeServerHome plants the fake gopls binary where the manager's binary
// resolution finds it first: home/.cache/lightcode/lsp/gopls/gopls. It
// returns the fake home and the planted server's request-log path.
func fakeServerHome(t *testing.T, initDelay float64) (home, requests string) {
	t.Helper()
	home = t.TempDir()
	requests = plantServer(t, home, "gopls", "gopls", initDelay, "")
	return home, requests
}

// plantServer plants one fake server binary at the definition's cache
// directory (home/.cache/lightcode/lsp/<dirName>/<binaryName>) and returns
// its request-log path.
func plantServer(t *testing.T, home, dirName, binaryName string, initDelay float64, crash string) string {
	t.Helper()
	cacheDir := filepath.Join(home, ".cache", "lightcode", "lsp", dirName)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests := filepath.Join(t.TempDir(), "requests.log")
	script := strings.ReplaceAll(fakeServerTemplate, "{{LOG}}", requests)
	script = strings.ReplaceAll(script, "{{DELAY}}", fmt.Sprintf("%g", initDelay))
	script = strings.ReplaceAll(script, "{{CRASH}}", crash)
	if err := os.WriteFile(filepath.Join(cacheDir, binaryName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return requests
}

const (
	pluginSessionID = "0123456789abcdef0123456789abcdef"
	otherSessionID  = "ffffffffffffffffffffffffffffffff"

	groupOne = "11111111111111111111111111111111"
	groupTwo = "22222222222222222222222222222222"
)

func toolCall(id, name, arguments string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(arguments)}
}

// pluginUnderTest opens the plugin with the spy installed and returns the
// instance's two tool values.
func pluginUnderTest(t *testing.T, dataDir string) (runtime.Tool, runtime.Tool) {
	t.Helper()
	return openPlugin(t, Plugin(), dataDir)
}

func openPlugin(t *testing.T, plugin runtime.Plugin, dataDir string) (runtime.Tool, runtime.Tool) {
	t.Helper()
	inst, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("plugin Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst.Values["diagnostics"].(runtime.Tool), inst.Values["workspace_symbol"].(runtime.Tool)
}

// markerWorkspace builds a workspace root with the gopls detection marker.
func markerWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// seedGroup writes one code group's changed-file metadata the way a real
// group records it: DataDir/code/<session>/<group>/snapshots/1/<e>/meta.json.
func seedGroup(t *testing.T, dataDir, sessionID, groupID, canonical string) {
	t.Helper()
	entry := filepath.Join(dataDir, "code", sessionID, groupID, "snapshots", "1", "e")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "{\"original_path\":\"" + filepath.Base(canonical) + "\",\"canonical_path\":\"" + canonical + "\",\"existed\":true}"
	if err := os.WriteFile(filepath.Join(entry, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func logCount(t *testing.T, log, needle string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(data), needle)
}

// waitForLog fails the test unless the log reaches atLeast occurrences of
// needle within the deadline; a missing or empty log is the failure, never a
// success.
func waitForLog(t *testing.T, log, needle string, atLeast int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if logCount(t, log, needle) >= atLeast {
			return
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(log)
			t.Fatalf("%q never reached %d in %s; log:\n%s", needle, atLeast, log, string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForQuery(t *testing.T, log, uri string, atLeast int) {
	t.Helper()
	// A document sync is a didOpen (first open) or a didChange (later syncs
	// of the same document); the count spans both.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if logCount(t, log, "textDocument/didOpen "+uri)+logCount(t, log, "textDocument/didChange "+uri) >= atLeast {
			return
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(log)
			t.Fatalf("document sync for %s never reached the server; log:\n%s", uri, string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fileURI(root, name string) string {
	return "file://" + filepath.Join(root, name)
}

func TestPluginOpenStartsNothingAndResolvesHome(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	pluginUnderTest(t, t.TempDir())
	if spy.count() != 0 {
		t.Fatal("plugin Open constructed a workspace manager; detection must start only in authorized execution")
	}
}

func TestPluginOpenHomeFailureFailsConstruction(t *testing.T) {
	t.Setenv("HOME", "")
	plugin := Plugin()
	if _, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, runtime.Bindings{}); err == nil {
		t.Fatal("plugin Open without a resolvable home = nil error, want ordinary failed construction")
	}
}

func TestPluginCacheStaysHomeBased(t *testing.T) {
	// Hermetic: the plugin resolves its home from the faked HOME, whose
	// cache holds the planted fake gopls — no host cache or installer is
	// ever reached.
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c1", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "No files have been modified." {
		t.Fatalf("diagnostics = %+v, want the no-files success", outcome.Result)
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("workspace managers constructed = %d, want 1", n)
	}
	if spy.root() != workspace {
		t.Fatalf("manager root = %q, want the canonical workspace %q", spy.root(), workspace)
	}
	// The manager received the plugin's resolved home — the faked HOME — and
	// the retained cache root stays home-based, under it, independently of
	// DataDir.
	resolved, err := os.UserHomeDir()
	if err != nil || resolved != fakeHome {
		t.Fatalf("os.UserHomeDir under the faked HOME = (%q, %v), want %q", resolved, err, fakeHome)
	}
	if spy.home() != resolved {
		t.Fatalf("manager home = %q, want the plugin-resolved %q", spy.home(), resolved)
	}
	if cache := filepath.Join(spy.home(), ".cache", "lightcode", "lsp"); !strings.HasPrefix(cache, fakeHome+string(filepath.Separator)) {
		t.Fatalf("cache root %q is not under the resolved home %q", cache, fakeHome)
	}
}

// TestConcurrentFirstUsesJoinOneDetect: eight concurrent first calls
// construct exactly one workspace manager and share one detection. Hermetic:
// the plugin's home resolves into the fake home whose cache holds the
// planted fake gopls.
func TestConcurrentFirstUsesJoinOneDetect(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
			errs[i] = outcomeError(prepared.Execute(context.Background()))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("workspace managers constructed = %d, want 1 shared detection", n)
	}
}

func outcomeError(outcome harness.ToolOutcome) error {
	if outcome.Result.Status == model.ResultError || outcome.Result.Status == model.ResultDenied {
		return errors.New(outcome.Result.Content)
	}
	return nil
}

// TestDetectionOnceIncludingEmptyMap: after one call detected nothing (no
// markers, no servers), later calls reuse the same manager — including its
// empty instance map — without re-detection.
func TestDetectionOnceIncludingEmptyMap(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	_, symbol := pluginUnderTest(t, dataDir)
	workspace := t.TempDir() // no markers: detection finds nothing

	for i := 0; i < 3; i++ {
		prepared := symbol.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":"x"}`))
		outcome := prepared.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "No language servers available." {
			t.Fatalf("call %d = %+v, want the retained zero-instance content", i+1, outcome.Result)
		}
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("workspace managers constructed = %d, want 1: failed detection is not retried", n)
	}
}

// TestCanceledWaiterDoesNotCancelDetection: a caller canceled during the
// shared detection wait settles interrupted while the detection continues and
// a second caller succeeds against the started server.
func TestCanceledWaiterDoesNotCancelDetection(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0.4)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	_, symbol := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	first, cancelFirst := context.WithCancel(context.Background())
	prepared := symbol.Prepare(first, runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c1", "workspace_symbol", `{"query":"x"}`))
	done := make(chan harness.ToolOutcome, 1)
	go func() { done <- prepared.Execute(first) }()
	time.Sleep(100 * time.Millisecond)
	cancelFirst()

	select {
	case outcome := <-done:
		if outcome.Result.Status != model.ResultInterrupted {
			t.Fatalf("canceled waiter = %+v, want interrupted", outcome.Result)
		}
		if outcome.Result.Content == "" {
			t.Fatal("interrupted result carries empty content")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled waiter never settled")
	}

	// The detection continues: a second caller waits it out and queries the
	// started server.
	second := symbol.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c2", "workspace_symbol", `{"query":"x"}`))
	outcome := second.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("second caller = %+v, want success against the started server", outcome.Result)
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("workspace managers constructed = %d, want 1", n)
	}
	waitForLog(t, requests, "initialize", 1)
}

// TestDetectionUnderCanceledRuntimeYieldsNoUsableServers: the plugin's
// lifetime is canceled after Open but before the first authorized use;
// detection runs under it, no usable server results, and no later call
// resurrects one. The live-lifetime sibling proves the same fixtures serve
// real queries, so the assertion discriminates.
func TestDetectionUnderCanceledRuntimeYieldsNoUsableServers(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	workspace := markerWorkspace(t)
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(workspace, "a.go"))
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("package w\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lifetime, cancel := context.WithCancel(context.Background())
	inst, err := Plugin().Open(lifetime, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("plugin Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	diag := inst.Values["diagnostics"].(runtime.Tool)
	// The lifetime is canceled after Open but before the first authorized
	// use: detection runs under it, and the owner cancellation is what the
	// detection's servers inherit.
	cancel()

	for i := 0; i < 2; i++ {
		prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
		outcome := prepared.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "No language servers available for the modified files." {
			t.Fatalf("call %d under the canceled lifetime = %+v, want the no-usable-servers content", i+1, outcome.Result)
		}
	}
	if n := logCount(t, requests, "textDocument/didOpen"); n != 0 {
		t.Fatalf("document sync observed under the canceled lifetime: %d occurrences", n)
	}

	// The live-lifetime sibling: the same fixtures start the server and the
	// query reaches it.
	diagLive, _ := openPlugin(t, Plugin(), dataDir)
	prepared := diagLive.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("live-lifetime diagnostics = %+v, want success", outcome.Result)
	}
	waitForQuery(t, requests, fileURI(workspace, "a.go"), 1)
	if n := spy.count(); n != 2 {
		t.Fatalf("workspace managers constructed = %d, want one per instance", n)
	}
}

// TestCloseJoinsInFlightDetection: Close waits for the shared detection to
// finish before shutting the managers down, and every server the detection
// started is torn down.
func TestCloseJoinsInFlightDetection(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0.4)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	workspace := markerWorkspace(t)

	plugin := Plugin()
	inst, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("plugin Open: %v", err)
	}
	symbol := inst.Values["workspace_symbol"].(runtime.Tool)

	// Start detection as a first authorized use and wait until its server
	// process is launched: detection is now provably in flight (the 0.4s
	// initialize delay holds it).
	useCtx, cancelUse := context.WithCancel(context.Background())
	defer cancelUse()
	prepared := symbol.Prepare(useCtx, runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":"x"}`))
	go prepared.Execute(useCtx)
	waitForLog(t, requests, "started", 1)

	// Close while detection is in flight: it must join before returning.
	start := time.Now()
	if err := inst.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("Close returned after %s; it must join the in-flight detection (init delay 0.4s)", elapsed)
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("workspace managers constructed = %d, want 1", n)
	}
	waitForLog(t, requests, "initialize", 1)
}

// TestDeniedAndPreparedCallsStartNothing: preparation of both tools, and a
// failed canonical preparation — a symlink-loop workspace root the path
// resolver rejects — constructs no manager and reads nothing; the failed
// preparation is the immediate fixed denial.
func TestDeniedAndPreparedCallsStartNothing(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, symbol := pluginUnderTest(t, dataDir)
	workspace := t.TempDir()

	diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	symbol.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":"x"}`))

	// The symlink-loop workspace root: canonicalization fails and the
	// preparation is denied without any manager construction.
	loopA := filepath.Join(t.TempDir(), "loop-a")
	loopB := filepath.Join(t.TempDir(), "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}

	for _, tool := range []runtime.Tool{diag, symbol} {
		prepared := tool.Prepare(context.Background(), runtime.ToolContext{Workspace: loopA, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":"x"}`))
		if prepared.Immediate == nil {
			t.Fatalf("symlink-loop workspace preparation = %+v, want an immediate denied outcome", prepared)
		}
		if prepared.Immediate.Result.Status != model.ResultDenied || prepared.Immediate.Result.Content != "Permission denied." {
			t.Fatalf("symlink-loop workspace preparation = %+v, want the fixed denial", prepared.Immediate.Result)
		}
		if prepared.Execute != nil {
			t.Fatal("the denied preparation carries an executor")
		}
	}

	if n := spy.count(); n != 0 {
		t.Fatalf("workspace managers constructed = %d, want 0: denied calls start nothing", n)
	}
}

// TestDiagnosticsNoCursorAcrossCalls: two seeded groups from two "operations"
// are both queried on every call — no cursor, no group-shaped result, no
// checked-group set.
func TestDiagnosticsNoCursorAcrossCalls(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(workspace, "a.go"))
	seedGroup(t, dataDir, pluginSessionID, groupTwo, filepath.Join(workspace, "b.go"))
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("package w\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uriA, uriB := fileURI(workspace, "a.go"), fileURI(workspace, "b.go")

	for call := 1; call <= 2; call++ {
		prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
		outcome := prepared.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("call %d = %+v, want success", call, outcome.Result)
		}
		waitForQuery(t, requests, uriA, call)
		waitForQuery(t, requests, uriB, call)
	}
}

// TestDiagnosticsExcludesOtherSessions: another session's groups are never
// read.
func TestDiagnosticsExcludesOtherSessions(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(workspace, "a.go"))
	seedGroup(t, dataDir, otherSessionID, groupTwo, filepath.Join(workspace, "b.go"))
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("package w\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("diagnostics = %+v, want success", outcome.Result)
	}
	waitForQuery(t, requests, fileURI(workspace, "a.go"), 1)
	if n := logCount(t, requests, fileURI(workspace, "b.go")); n != 0 {
		t.Fatalf("the other session's file was synced: %d occurrences", n)
	}
}

// TestDiagnosticsDropsOutsideRootPaths: an outside-root canonical path is
// dropped un-read.
func TestDiagnosticsDropsOutsideRootPaths(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(workspace, "a.go"))
	outside := filepath.Join(t.TempDir(), "outside.go")
	seedGroup(t, dataDir, pluginSessionID, groupTwo, outside)
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("package w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("diagnostics = %+v, want success", outcome.Result)
	}
	waitForQuery(t, requests, fileURI(workspace, "a.go"), 1)
	if n := logCount(t, requests, "outside.go"); n != 0 {
		t.Fatal("the outside-root path was read despite being dropped")
	}
}

// TestSymlinkedWorkspaceBindsToRealRoot: a symlinked lexical Workspace binds
// to the real root and the tool succeeds against it.
func TestSymlinkedWorkspaceBindsToRealRoot(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	realRoot := markerWorkspace(t)
	if err := os.WriteFile(filepath.Join(realRoot, "a.go"), []byte("package w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(realRoot, "a.go"))
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: link, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	if prepared.CanonicalWorkspace != realRoot {
		t.Fatalf("CanonicalWorkspace = %q, want the real root %q", prepared.CanonicalWorkspace, realRoot)
	}
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("diagnostics = %+v, want success through the symlinked lexical workspace", outcome.Result)
	}
	waitForQuery(t, requests, fileURI(realRoot, "a.go"), 1)
	if spy.root() != realRoot {
		t.Fatalf("manager root = %q, want the canonical real root", spy.root())
	}
}

// TestPostAuthorizationBindingChangeFailsWithoutStart: after preparation, a
// repointed workspace symlink fails execution with an error outcome, starts
// nothing, and never substitutes the new root.
func TestPostAuthorizationBindingChangeFailsWithoutStart(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	t.Setenv("HOME", fakeHome)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	realRoot := markerWorkspace(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: link, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	if prepared.CanonicalWorkspace != realRoot {
		t.Fatalf("CanonicalWorkspace = %q, want %q", prepared.CanonicalWorkspace, realRoot)
	}

	// Repoint the lexical workspace at a different root after authorization.
	otherRoot := t.TempDir()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherRoot, link); err != nil {
		t.Fatal(err)
	}

	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultError {
		t.Fatalf("changed binding = %+v, want an error outcome", outcome.Result)
	}
	if !strings.Contains(outcome.Result.Content, "binding changed") {
		t.Fatalf("changed binding content = %q", outcome.Result.Content)
	}
	if n := spy.count(); n != 0 {
		t.Fatalf("workspace managers constructed = %d, want 0: a changed binding starts nothing", n)
	}
}

// TestCallerCancellationDuringQueryIsInterrupted: cancellation during the
// shared client's sync wait settles interrupted with the bounded partial
// content.
func TestCallerCancellationDuringQueryIsInterrupted(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	// One healthy file (its sync proves the opens completed) and one
	// replaced-symlink leaf (its canonical read fails, so partial
	// could-not-check content accumulates before the sync wait).
	healthy := filepath.Join(workspace, "a.go")
	if err := os.WriteFile(healthy, []byte("package w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, dataDir, pluginSessionID, groupOne, healthy)
	target := filepath.Join(t.TempDir(), "target.go")
	if err := os.WriteFile(target, []byte("package target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped := filepath.Join(workspace, "swapped.go")
	if err := os.WriteFile(swapped, []byte("placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(swapped); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, swapped); err != nil {
		t.Fatal(err)
	}
	seedGroup(t, dataDir, pluginSessionID, groupTwo, swapped)

	ctx, cancel := context.WithCancel(context.Background())
	prepared := diag.Prepare(ctx, runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	done := make(chan harness.ToolOutcome, 1)
	go func() { done <- prepared.Execute(ctx) }()
	// The healthy file's document sync proves the opens are done and the
	// shared body sits in its 500ms sync wait.
	waitForQuery(t, requests, fileURI(workspace, "a.go"), 1)
	cancel()

	select {
	case outcome := <-done:
		if outcome.Result.Status != model.ResultInterrupted {
			t.Fatalf("canceled query = %+v, want interrupted", outcome.Result)
		}
		if !strings.Contains(outcome.Result.Content, "(could not check)") {
			t.Fatalf("interrupted content = %q, want the bounded partial could-not-check line", outcome.Result.Content)
		}
		if len(outcome.Result.Content) > 15360 {
			t.Fatalf("interrupted content = %d bytes, want the bounded partial", len(outcome.Result.Content))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled query never settled")
	}
}

// TestDiagnosticsListingErrorRetainedText: a listing error settles as the
// retained content with success status.
func TestDiagnosticsListingErrorRetainedText(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: "not-a-valid-session-id"}}, toolCall("c", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "error: could not list changed files" {
		t.Fatalf("listing error = %+v, want the retained content with success status", outcome.Result)
	}
}

// TestDiagnosticsNoFiles: an empty data root settles as the retained no-files
// content.
func TestDiagnosticsNoFiles(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	diag, _ := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	prepared := diag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "No files have been modified." {
		t.Fatalf("no files = %+v, want the retained no-files content", outcome.Result)
	}
}

// TestWorkspaceSymbolEmptyQueryRetainedText.
func TestWorkspaceSymbolEmptyQueryRetainedText(t *testing.T) {
	fakeHome, _ := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	_, symbol := pluginUnderTest(t, dataDir)
	workspace := markerWorkspace(t)

	prepared := symbol.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":""}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "error: query is required" {
		t.Fatalf("empty query = %+v, want the retained rejection with success status", outcome.Result)
	}
}

// TestNormalizeContracts: the strict decode, the private-field stripping,
// the retained unrelated members and the consumed query field.
func TestNormalizeContracts(t *testing.T) {
	diag := diagnosticsTool{inst: &instance{}}
	symbol := workspaceSymbolTool{inst: &instance{}}
	tc := runtime.ToolContext{}

	got, err := diag.Normalize(tc, toolCall("c", "diagnostics", `{"_lightcode_receipt":"forged","unrelated":1}`))
	if err != nil {
		t.Fatalf("diagnostics Normalize: %v", err)
	}
	if string(got) != `{"unrelated":1}` {
		t.Fatalf("diagnostics normalized = %s, want the stripped retained members", got)
	}
	for _, bad := range []string{`null`, `[1]`, `{`, `{} trailing`} {
		if _, err := diag.Normalize(tc, toolCall("c", "diagnostics", bad)); err == nil {
			t.Errorf("diagnostics Normalize(%q) = nil error, want an argument-validation error", bad)
		}
	}

	got, err = symbol.Normalize(tc, toolCall("c", "workspace_symbol", `{"query":"x","_lightcode_receipt":"forged","other":"kept"}`))
	if err != nil {
		t.Fatalf("workspace_symbol Normalize: %v", err)
	}
	if !strings.Contains(string(got), `"query":"x"`) || !strings.Contains(string(got), `"other":"kept"`) || strings.Contains(string(got), "_lightcode_") {
		t.Fatalf("workspace_symbol normalized = %s", got)
	}
	for _, bad := range []string{`{}`, `{"query":null}`, `{"query":5}`, `{"query":["x"]}`} {
		if _, err := symbol.Normalize(tc, toolCall("c", "workspace_symbol", bad)); err == nil {
			t.Errorf("workspace_symbol Normalize(%q) = nil error, want an argument-validation error", bad)
		}
	}
	if _, err := symbol.Normalize(tc, toolCall("c", "workspace_symbol", `{"query":""}`)); err != nil {
		t.Fatalf("workspace_symbol Normalize empty string: %v", err)
	}
}

// TestDescriptionsAndPreparationShape: the updated diagnostics description,
// the verbatim workspace_symbol schema, and the one workspace.inspect pair.
func TestDescriptionsAndPreparationShape(t *testing.T) {
	diagDesc, err := diagnosticsTool{}.describe(runtime.Invocation{}, runtime.ToolConstraints{})
	if err != nil {
		t.Fatalf("diagnostics describe: %v", err)
	}
	if diagDesc.Definition.Description != "Check for compilation errors in the files this Session has changed. Call after editing to verify correctness. Returns errors only (not warnings)." {
		t.Fatalf("diagnostics description = %q", diagDesc.Definition.Description)
	}
	if diagDesc.Definition.Name != "diagnostics" || !diagDesc.Available {
		t.Fatalf("diagnostics definition = %+v", diagDesc.Definition)
	}

	symbolDesc, err := workspaceSymbolTool{}.describe(runtime.Invocation{}, runtime.ToolConstraints{})
	if err != nil {
		t.Fatalf("workspace_symbol describe: %v", err)
	}
	if symbolDesc.Definition.Description != "Search for symbols by name across the project. Returns matching functions, types, variables, etc. with their file locations." {
		t.Fatalf("workspace_symbol description = %q", symbolDesc.Definition.Description)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(symbolDesc.Definition.Parameters, &schema); err != nil {
		t.Fatalf("workspace_symbol schema: %v", err)
	}
	if _, ok := schema.Properties["query"]; !ok || len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("workspace_symbol schema = %s", symbolDesc.Definition.Parameters)
	}

	dataDir := t.TempDir()
	workspace := markerWorkspace(t)
	for _, tool := range []runtime.Tool{diagnosticsTool{inst: &instance{dataDir: dataDir}}, workspaceSymbolTool{inst: &instance{dataDir: dataDir}}} {
		prepared := tool.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "diagnostics", `{}`))
		if len(prepared.Permissions) != 1 {
			t.Fatalf("permissions = %+v, want exactly one pair", prepared.Permissions)
		}
		pair := prepared.Permissions[0]
		if pair.Permission != "workspace.inspect" || pair.Target != workspace {
			t.Fatalf("permission pair = %+v, want workspace.inspect on the canonical root", pair)
		}
		if prepared.CanonicalWorkspace != workspace || prepared.CanonicalWriteDir != "" {
			t.Fatalf("canonical binding = %q/%q, want the root and no write dir", prepared.CanonicalWorkspace, prepared.CanonicalWriteDir)
		}
	}
}

// TestContentCap: the at-limit and over-limit rows, the multi-byte character
// straddling the boundary, and the cap on error content.
func TestContentCap(t *testing.T) {
	if got := capModelContent(strings.Repeat("a", 15360)); got != strings.Repeat("a", 15360) {
		t.Fatal("at-limit content must be unchanged")
	}
	over := strings.Repeat("a", 15361)
	got := capModelContent(over)
	if len(got) != 15360 || !strings.HasSuffix(got, "\n[Output truncated]") {
		t.Fatalf("over-limit cap = %d bytes, want 15360 with the marker", len(got))
	}
	if !strings.HasPrefix(got, over[:15360-len("\n[Output truncated]")]) {
		t.Fatal("cap did not keep the longest fitting prefix")
	}

	// A multi-byte character straddling the boundary: the cut backs off to
	// the rune boundary and the result stays valid UTF-8. The first € starts
	// before the budget cut and covers it, so an unguarded slice would cut
	// mid-character.
	prefix := strings.Repeat("a", 15360-len(truncationMarker)-2)
	straddled := prefix + "€€€€€€€€€€" // each € is three bytes
	got = capModelContent(straddled)
	if len(got) > 15360 || !strings.HasSuffix(got, "\n[Output truncated]") {
		t.Fatalf("straddled cap = %d bytes, want the marker within 15360", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("cap produced invalid UTF-8")
	}
	if !strings.HasPrefix(got, prefix) {
		t.Fatal("cap lost the complete prefix")
	}

	// Error content is capped by the same rule.
	outcome := toolOutcome("c", context.Background(), "", errors.New(strings.Repeat("e", 20000)))
	if outcome.Result.Status != model.ResultError || len(outcome.Result.Content) != 15360 {
		t.Fatalf("error cap = %+v (%d bytes), want the capped error content", outcome.Result, len(outcome.Result.Content))
	}

	// Success content is capped by the same rule.
	outcome = toolOutcome("c", context.Background(), strings.Repeat("s", 20000), nil)
	if outcome.Result.Status != model.ResultSuccess || len(outcome.Result.Content) != 15360 {
		t.Fatalf("success cap = %+v (%d bytes), want the capped content", outcome.Result, len(outcome.Result.Content))
	}
}
