package builtin

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

// TestShippedRegistrationHoldsSixInOrder proves the shipped registration set
// is exactly the six registered plugins in registration order: the durable
// SQLite session store first, then the native tools, the bundled adaptation,
// the LSP tools, the background jobs capability, and the child-session task
// tool. The full composition open over this set is exercised by the runtime
// integration suite.
func TestShippedRegistrationHoldsSixInOrder(t *testing.T) {
	plugins := Plugins()
	testassert.Equal(t, len(plugins), 6)
	want := []string{"sqlite", "tools", "adaptation", "lsp", "jobs", "tasks"}
	for i, p := range plugins {
		testassert.Check(t, p.ID == want[i], "plugin %d ID = %q, want %q", i, p.ID, want[i])
		testassert.Check(t, p.Open != nil, "plugin %q has no Open factory", p.ID)
	}
}
