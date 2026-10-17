package lsp

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

// TestWorkspaceSymbolAllFailedInstancesIsError: when every mapped instance is
// unavailable, the fan-out returns an ordinary error, never a clean
// no-symbols result that reads as an answer.
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
	if err == nil {
		t.Fatalf("WorkspaceSymbol with only failed servers = (%q, nil), want an ordinary unavailability error", content)
	}
}

// TestClientNoInstancesMessages: with no mapped instance at all, the symbol
// entry returns the retained text as an ordinary error, while the diagnostics
// entry follows the uniform per-path rule — every requested path becomes its
// could-not-check line, and the plain unavailability text is only the empty
// input's fallback.
func TestClientNoInstancesMessages(t *testing.T) {
	client := NewClient(NewManager(t.TempDir(), t.TempDir()))
	got, err := client.WorkspaceSymbol(context.Background(), "x")
	if err == nil || got != "" || err.Error() != "No language servers available." {
		t.Fatalf("WorkspaceSymbol no instances = %q, %v", got, err)
	}
	got, err = client.GetDiagnosticsCanonical(context.Background(), []string{"main.go"})
	if err == nil || got != "" || err.Error() != "main.go: no language server for this file (could not check)" {
		t.Fatalf("GetDiagnosticsCanonical unsupported path = %q, %v", got, err)
	}
	got, err = client.GetDiagnosticsCanonical(context.Background(), nil)
	if err == nil || got != "" || err.Error() != "No language servers available for the modified files." {
		t.Fatalf("GetDiagnosticsCanonical empty input = %q, %v", got, err)
	}
}
