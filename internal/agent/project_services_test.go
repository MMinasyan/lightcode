package agent

import "testing"

// TestLSPManagerPerProject proves the owner keeps one LSP manager per project
// root: the same root shares a manager, and different roots get distinct ones.
func TestLSPManagerPerProject(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	rootA := t.TempDir()
	first := a.lspManagerFor(rootA)
	again := a.lspManagerFor(rootA)
	if first == nil || first != again {
		t.Fatalf("same project root must reuse one LSP manager: %p vs %p", first, again)
	}
	if other := a.lspManagerFor(t.TempDir()); other == first {
		t.Fatal("different project roots must have distinct LSP managers")
	}
}
