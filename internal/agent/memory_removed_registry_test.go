package agent

import "testing"

// TestRootRegistryLacksMemoryTools is the Step-2 memory root-tool-surface
// regression. Before memory was removed, save_memory, search_memory, and
// search_history were registered and model-advertised on every owner registry,
// so a forged call reached the memory tool. After removal they must be absent
// from the registry and never advertised: a forged dispatch returns the ordinary
// unknown-tool result instead of reaching any memory behavior.
func TestRootRegistryLacksMemoryTools(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		if _, ok := a.registry.Get(name); ok {
			t.Fatalf("root registry still contains removed memory tool %q", name)
		}
		if a.registry.Advertises(name, nil) {
			t.Fatalf("root registry still advertises removed memory tool %q", name)
		}
	}
}
