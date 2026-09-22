package builtin

import "testing"

// TestShippedRegistrationHoldsSixInOrder proves the shipped registration set
// is exactly the six registered plugins in registration order: the durable
// SQLite session store first, then the native tools, the bundled adaptation,
// the LSP tools, the background jobs capability, and the child-session task
// tool. The full composition open over this set is exercised by the runtime
// integration suite.
func TestShippedRegistrationHoldsSixInOrder(t *testing.T) {
	plugins := Plugins()
	if len(plugins) != 6 {
		t.Fatalf("Plugins() returned %d plugins, want six", len(plugins))
	}
	want := []string{"sqlite", "tools", "adaptation", "lsp", "jobs", "tasks"}
	for i, p := range plugins {
		if p.ID != want[i] {
			t.Errorf("plugin %d ID = %q, want %q", i, p.ID, want[i])
		}
		if p.Open == nil {
			t.Errorf("plugin %q has no Open factory", p.ID)
		}
	}
}
