package harness

import (
	"context"
	"testing"
)

// leaseCoordinator acquires one Session's coordinator through the production
// acquisition path as a metadata-only use and holds that lease until the test
// ends: the explicit lease a controlled private-execution fixture takes before
// it inspects or drives private coordinator state. A cache-pressure fixture
// must not hold one across its pressure window: a held lease pins the graph
// and would mask a missing production pin.
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
	c, release, err := h.acquireCoordinator(context.Background(), sessionID, false)
	if err != nil {
		t.Fatalf("lease session %q: %v", sessionID, err)
	}
	return c, release
}

// cachedGraphIDs returns the inactive cached graph identities in LRU order,
// oldest first, under the registry mutex alone: no lease is taken, so a
// cache-pressure fixture observes retention without pinning it.
func cachedGraphIDs(h *Harness) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, h.lru.Len())
	for e := h.lru.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*coordinator).sessionID)
	}
	return out
}
