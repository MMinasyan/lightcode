package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestManagedEnvCrossInstanceOperations proves two independent managers (as
// two processes) serialize their .env read-modify-write through the shared
// lock: concurrent additions of distinct keys are all preserved, a managed
// removal takes effect, and an unmanaged removal is a no-op.
func TestManagedEnvCrossInstanceOperations(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	newMgr := func() *ManagedEnv {
		return &ManagedEnv{path: envPath, managed: map[string]struct{}{}}
	}

	keys := []string{"LIGHTCODE_TEST_A", "LIGHTCODE_TEST_B", "LIGHTCODE_TEST_C"}
	for _, k := range keys {
		key := k
		os.Unsetenv(key)
		t.Cleanup(func() { os.Unsetenv(key) })
	}

	// add/add: each manager sets a distinct key concurrently; the lock must
	// preserve every write.
	var wg sync.WaitGroup
	wg.Add(len(keys))
	for _, k := range keys {
		key := k
		go func() {
			defer wg.Done()
			if err := newMgr().Set(key, "v-"+key); err != nil {
				t.Errorf("Set %s: %v", key, err)
			}
		}()
	}
	wg.Wait()

	present, _, err := ReadDotEnvKeys(envPath)
	if err != nil {
		t.Fatalf("ReadDotEnvKeys: %v", err)
	}
	for _, k := range keys {
		if _, ok := present[k]; !ok {
			t.Fatalf("key %s lost from .env; present=%v", k, present)
		}
	}

	// A managed key is removed.
	remover := newMgr()
	remover.managed[keys[0]] = struct{}{}
	if err := remover.Remove(keys[0]); err != nil {
		t.Fatalf("Remove managed: %v", err)
	}
	present, _, _ = ReadDotEnvKeys(envPath)
	if _, ok := present[keys[0]]; ok {
		t.Fatalf("removed key %s still present", keys[0])
	}

	// An unmanaged removal (fresh manager does not own the key) is a no-op.
	before, _, _ := ReadDotEnvKeys(envPath)
	if err := newMgr().Remove(keys[1]); err != nil {
		t.Fatalf("Remove unmanaged: %v", err)
	}
	after, _, _ := ReadDotEnvKeys(envPath)
	if len(before) != len(after) {
		t.Fatalf("unmanaged removal changed the file: %v -> %v", before, after)
	}
	if _, ok := after[keys[1]]; !ok {
		t.Fatalf("unmanaged key %s wrongly removed", keys[1])
	}
}
