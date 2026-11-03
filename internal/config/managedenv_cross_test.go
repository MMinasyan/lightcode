package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestManagedEnvCrossInstanceOperations proves two independent managers (as
// two processes) serialize their .env read-modify-write through the shared
// lock: concurrent additions of distinct keys are all preserved, a managed
// removal takes effect, and an unmanaged removal is a no-op.
func TestManagedEnvCrossInstanceOperations(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	newMgr := func() *ManagedEnv {
		return NewManagedEnvForTest(envPath)
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
			{
				err := newMgr().Set(key, "v-"+key)
				testassert.Check(t, err == nil, "Set %s: %v", key, err)
			}
		}()
	}
	wg.Wait()

	present, _, err := ReadDotEnvKeys(envPath)
	testassert.NoError(t, err)
	for _, k := range keys {
		{
			_, ok := present[k]
			testassert.Require(t, ok, "key %s lost from .env; present=%v", k, present)
		}
	}

	// A managed key is removed.
	remover := newMgr()
	remover.managed[keys[0]] = struct{}{}
	testassert.NoError(t, remover.Remove(keys[0]))
	present, _, _ = ReadDotEnvKeys(envPath)
	{
		_, ok := present[keys[0]]
		testassert.Require(t, !ok, "removed key %s still present", keys[0])
	}

	// An unmanaged removal (fresh manager does not own the key) is a no-op.
	before, _, _ := ReadDotEnvKeys(envPath)
	testassert.NoError(t, newMgr().Remove(keys[1]))
	after, _, _ := ReadDotEnvKeys(envPath)
	testassert.Require(t, len(before) == len(after), "unmanaged removal changed the file: %v -> %v", before, after)
	{
		_, ok := after[keys[1]]
		testassert.Require(t, ok, "unmanaged key %s wrongly removed", keys[1])
	}
}
