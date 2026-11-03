package builtin

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

// TestShippedRegistrationHoldsTwoInOrder proves the shipped registration set
// is exactly the two declared plugins in registration order: the bundled
// adaptation and the child-session task tool. The session store, the standard
// tools, and the background jobs service are constructed directly by the
// Runtime owner and are not plugin declarations. The full composition open over
// this set is exercised by the runtime integration suite.
func TestShippedRegistrationHoldsTwoInOrder(t *testing.T) {
	plugins := Plugins()
	testassert.Equal(t, len(plugins), 2)
	want := []string{"adaptation", "tasks"}
	for i, p := range plugins {
		testassert.Check(t, p.ID == want[i], "plugin %d ID = %q, want %q", i, p.ID, want[i])
		testassert.Check(t, p.Open != nil, "plugin %q has no Open factory", p.ID)
	}
}
