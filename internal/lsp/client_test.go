package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestWorkspaceSymbolAllFailedInstancesIsError: when every mapped instance is
// unavailable, the fan-out returns the one fixed unavailability error, never a
// clean no-symbols result that reads as an answer.
func TestWorkspaceSymbolAllFailedInstancesIsError(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, context.Background(), nil)
	inst.mu.Lock()
	inst.state = stateFailed
	inst.mu.Unlock()
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	content, err := NewClient(m).WorkspaceSymbol(context.Background(), "x")
	testassert.Require(t, err != nil && content == "" && err.Error() == "No language servers available for the symbol query.", "WorkspaceSymbol with only failed servers = (%q, %v), want the fixed unavailability error", content, err)
}

// TestClientNoInstancesMessages: with no mapped instance at all, the symbol
// entry returns the retained text as an ordinary error, while the diagnostics
// entry follows the uniform per-path rule — every requested path becomes its
// could-not-check line, and the plain unavailability text is only the empty
// input's fallback.
func TestClientNoInstancesMessages(t *testing.T) {
	client := NewClient(NewManager(t.TempDir(), t.TempDir()))
	got, err := client.WorkspaceSymbol(context.Background(), "x")
	testassert.Require(t, err != nil && got == "" && err.Error() == "No language servers available.", "WorkspaceSymbol no instances = %q, %v", got, err)
	got, err = client.GetDiagnosticsCanonical(context.Background(), []string{"main.go"})
	testassert.Require(t, err != nil && got == "" && err.Error() == "main.go: no language server for this file (could not check)", "GetDiagnosticsCanonical unsupported path = %q, %v", got, err)
	got, err = client.GetDiagnosticsCanonical(context.Background(), nil)
	testassert.Require(t, err != nil && got == "" && err.Error() == "No language servers available for the modified files.", "GetDiagnosticsCanonical empty input = %q, %v", got, err)
}

// TestGetDiagnosticsCanonicalDeadServerIsNotCleanResult pins the sample-time
// liveness rule: a server that accepted the document sync and then died — its
// crash loop exhausting the restart budget, every restart clearing the
// diagnostic cache — is not sampled as a healthy empty result. Its synced path
// becomes a could-not-check failure and, with nothing live left to sample, the
// collected failure is the ordinary error, never the clean no-error text. A
// distinct pyright instance that is live but has no synchronized path at all —
// its only requested path is a missing leaf refused before any RPC — cannot
// supply a successful sample either: a live server can only speak for the
// documents it was actually given.
func TestGetDiagnosticsCanonicalDeadServerIsNotCleanResult(t *testing.T) {
	reqLog := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_LSP_REQUESTS_LOG", reqLog)
	log := filepath.Join(t.TempDir(), "launches.log")
	// The first launch serves the sync and dies on the didOpen it receives;
	// every later launch crashes before readiness, so the instance can never
	// serve the sample.
	inst := newFakeServerInstance(t, log, "", 0, 0, "after-open", "")
	testassert.NoError(t, inst.waitReady(context.Background()))
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	// The live-but-unsynchronized sibling: ready with an open attempt
	// channel but no process of its own and no synchronized document.
	pyDef := server.ForExtension(".py")
	pyright := newInstance(pyDef, m.projectRoot, m.home, context.Background(), nil)
	pyright.mu.Lock()
	pyright.state = stateReady
	pyright.procDone = make(chan struct{})
	pyright.mu.Unlock()
	m.mu.Lock()
	m.instances[pyDef.Name] = pyright
	m.mu.Unlock()

	healthy := filepath.Join(t.TempDir(), "healthy.go")
	testassert.NoError(t, os.WriteFile(healthy, []byte("package healthy\n"), 0o600))
	missingPy := filepath.Join(t.TempDir(), "missing.py")

	content, err := NewClient(m).GetDiagnosticsCanonical(context.Background(), []string{healthy, missingPy})
	testassert.Require(t, err != nil && content == "", "GetDiagnosticsCanonical with a live but unsynchronized server = (%q, %v), want the could-not-check error, never a successful sample", content, err)
	testassert.Require(t, strings.Contains(err.Error(), "(could not check)") && strings.Contains(err.Error(), "unavailable") && strings.Contains(err.Error(), "missing.py") && strings.Contains(err.Error(), "healthy.go"), "GetDiagnosticsCanonical error = %q, want the unavailable could-not-check lines; GetDiagnosticsCanonical error = %q, want both the refused pyright leaf and the dead server's synced path", err, err)

	// The crash loop really ran to exhaustion over the served sync: the
	// document reached the first launch, and every later launch died before
	// readiness.
	data, err := os.ReadFile(reqLog)
	testassert.Require(t, err == nil && strings.Contains(string(data), "textDocument/didOpen"), "the document sync never reached the server (log %q, err %v)", data, err)
	waitForState(t, inst, stateFailed)
	testassert.Equal(t, countLaunches(log), maxRestarts+1)
}
