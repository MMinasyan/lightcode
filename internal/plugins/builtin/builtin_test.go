package builtin

import "testing"

// TestShippedRegistrationHoldsFiveInOrder proves the shipped registration set
// is exactly the five registered plugins in registration order: the durable
// SQLite session store first, then the native tools, the bundled adaptation,
// the LSP tools, and the background jobs capability. The full composition
// open over this set is exercised by the runtime integration suite.
func TestShippedRegistrationHoldsFiveInOrder(t *testing.T) {
	plugins := Plugins()
	if len(plugins) != 5 {
		t.Fatalf("Plugins() returned %d plugins, want five", len(plugins))
	}
	want := []string{"sqlite", "tools", "adaptation", "lsp", "jobs"}
	for i, p := range plugins {
		if p.ID != want[i] {
			t.Errorf("plugin %d ID = %q, want %q", i, p.ID, want[i])
		}
		if p.Open == nil {
			t.Errorf("plugin %q has no Open factory", p.ID)
		}
	}
}
