package catalog

import (
	"testing"

	"github.com/Syntheresis/melta/internal/testassert"
)

// TestUserCostProtectionUsesCapturedInput proves the refresh merge reads the
// caller's already-captured user provider layer and never a config path.
func TestUserCostProtectionUsesCapturedInput(t *testing.T) {
	userRaw := map[string]any{
		"test": map[string]any{
			"models": map[string]any{
				"model": map[string]any{"cost": map[string]any{"output": 2, "cache_read": 3}},
			},
		},
	}
	protected := userCostProtection(userRaw, "test")
	testassert.Require(t, protected["model"]["output"] && protected["model"]["cache_read"] && !protected["model"]["input"], "protected = %#v, want only the captured user cost fields", protected)
}
