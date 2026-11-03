package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/runtime"
)

// The shipped registration set composes through the real construction path
// beside the directly built baseline services. This suite lives in the
// external package so the plugin registration import never cycles into the
// runtime owner.

// TestBuiltinRegistrationComposes proves the shipped optional registration
// set — the bundled adaptation and the child-session task tool — composes
// through the real construction path beside the directly constructed
// baseline store, jobs, and tools: the complete composition opens over the
// included SQLite database, reload publishes, and shutdown converges leaving
// exactly the included store's files.
func TestBuiltinRegistrationComposes(t *testing.T) {
	ctx := context.Background()
	home, dataDir, configDir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "internal", "catalog", "builtin", entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
	configPath := filepath.Join(configDir, "config.json")
	testassert.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`), 0o600))

	r, err := runtime.OpenForTest(ctx, dataDir, configPath, builtin.Plugins())
	testassert.NoError(t, err)
	{
		revision, err := r.Reload(ctx)
		testassert.Require(t, err == nil && revision == "2", "Reload = (%q, %v), want 2", revision, err)
	}
	testassert.NoError(t, r.Close(ctx))
	{
		got := sortedDataRootNames(t, dataDir)
		testassert.Require(t, slices.Equal(got, []string{"lightcode.db", "runtime.lock"}), "data root after shutdown = %v, want exactly the included store's files and the ownership lock", got)
	}
}

func sortedDataRootNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	testassert.Require(t, err == nil, "read dir %s: %v", dir, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}
