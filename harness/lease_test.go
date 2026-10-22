package harness

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// leaseCoordinator acquires one Session's coordinator through the production
// acquisition path and holds that lease until the test ends: the explicit
// lease a controlled private-execution fixture takes before it inspects or
// drives private coordinator state. A held lease pins the graph for its whole
// interval, so a fixture that observes a released graph must not hold one
// across its release window: a held lease would mask a missing production pin.
func leaseCoordinator(t *testing.T, h *Harness, sessionID string) *coordinator {
	t.Helper()
	c, release := leaseCoordinatorNow(t, h, sessionID)
	t.Cleanup(release)
	return c
}

// leaseCoordinatorNow is leaseCoordinator for a fixture that owns its lease
// lifetime: the caller must invoke the returned release exactly once.
func leaseCoordinatorNow(t *testing.T, h *Harness, sessionID string) (*coordinator, func()) {
	t.Helper()
	c, release, err := h.acquireCoordinator(context.Background(), sessionID)
	testassert.Require(t, err == nil, "lease session %q: %v", sessionID, err)
	return c, release
}
