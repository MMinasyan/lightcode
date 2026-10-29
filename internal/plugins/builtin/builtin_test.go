package builtin

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

// TestShippedRegistrationHoldsFiveInOrder proves the shipped registration set
// is exactly the five registered plugins in registration order: the durable
// SQLite session store first, then the native tools, the bundled adaptation,
// the background jobs capability, and the child-session task tool. The full
// composition open over this set is exercised by the runtime integration
// suite.
func TestShippedRegistrationHoldsFiveInOrder(t *testing.T) {
	plugins := Plugins()
	testassert.Equal(t, len(plugins), 5)
	want := []string{"sqlite", "tools", "adaptation", "jobs", "tasks"}
	for i, p := range plugins {
		testassert.Check(t, p.ID == want[i], "plugin %d ID = %q, want %q", i, p.ID, want[i])
		testassert.Check(t, p.Open != nil, "plugin %q has no Open factory", p.ID)
	}
}
