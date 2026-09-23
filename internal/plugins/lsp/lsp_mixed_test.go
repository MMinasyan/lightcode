package lsp

// The mixed-detection and plugin-restart rows: with markers for two language
// servers where exactly one is available, detection's mixed outcome still
// returns the available server's content with the unavailable one omitted;
// and a second plugin instance over the same data root re-detects and
// includes the same Session's changed files.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// TestMixedDetectionReturnsAvailableServerContent: the workspace carries the
// gopls and pyright markers; only gopls is available — the planted pyright
// binary crashes before readiness on every launch, so its crash loop
// exhausts the restart budget and the instance ends mapped but terminal
// failed. The client query returns the available server's content and omits
// the unavailable one (the retained partial-server behavior).
func TestMixedDetectionReturnsAvailableServerContent(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	pyrightLog := plantServer(t, fakeHome, "pyright", "pyright-langserver", 0, "1", "")
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	_, symbol := pluginUnderTest(t, dataDir)

	workspace := markerWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "pyproject.toml"), []byte("[project]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	prepared := symbol.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c", "workspace_symbol", `{"query":"Foo"}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("mixed detection query = %+v, want the available server's success", outcome.Result)
	}
	if !strings.Contains(outcome.Result.Content, "Foo") {
		t.Fatalf("mixed detection content = %q, want the available server's symbol", outcome.Result.Content)
	}
	waitForLog(t, requests, "workspace/symbol", 1)
	// The unavailable server went through its crash loop: the pyright binary
	// launched maxRestarts+1 times (the template writes "started" exactly
	// once per launch) and ended failed; no symbol query ever reached it.
	waitForLog(t, pyrightLog, "started", maxRestartLaunches)
	if n := logCount(t, pyrightLog, "workspace/symbol"); n != 0 {
		t.Fatalf("the unavailable server received %d symbol queries, want 0 (omitted)", n)
	}
}

// maxRestartLaunches is the launch count that exhausts the crash budget: the
// initial launch plus one restart per budgeted crash.
const maxRestartLaunches = 3 + 1

// TestRestartedPluginInstanceReincludesSessionFiles: after a first plugin
// instance's query, a second instance over the same data root re-detects and
// its diagnostics query includes the same Session's changed files.
func TestRestartedPluginInstanceReincludesSessionFiles(t *testing.T) {
	fakeHome, requests := fakeServerHome(t, 0)
	spy := &managerSpy{}
	spy.install(t, fakeHome)
	dataDir := t.TempDir()
	workspace := markerWorkspace(t)
	seedGroup(t, dataDir, pluginSessionID, groupOne, filepath.Join(workspace, "a.go"))
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("package w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uriA := fileURI(workspace, "a.go")

	// The first instance's query.
	plugin := Plugin()
	first, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	firstDiag := first.Values["diagnostics"].(runtime.Tool)
	prepared := firstDiag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c1", "diagnostics", `{}`))
	outcome := prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("first instance query = %+v, want success", outcome.Result)
	}
	waitForQuery(t, requests, uriA, 1)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// The restarted instance: fresh detection over the same data root and
	// home, and the same Session's changed files are included again.
	second, err := plugin.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondDiag := second.Values["diagnostics"].(runtime.Tool)
	prepared = secondDiag.Prepare(context.Background(), runtime.ToolContext{Workspace: workspace, AdmittedEntry: harness.EntryRef{SessionID: pluginSessionID}}, toolCall("c2", "diagnostics", `{}`))
	outcome = prepared.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("restarted query = %+v, want success", outcome.Result)
	}
	waitForQuery(t, requests, uriA, 2)
	if n := spy.count(); n != 2 {
		t.Fatalf("workspace managers constructed = %d, want one per instance", n)
	}
}
