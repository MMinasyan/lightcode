package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

// TestFirstRunCreationPreservesExistingFiles verifies the retained first-run
// creator uses create-if-absent semantics: an existing user file is never
// overwritten. First-run config.json and agents.json creation is owned by the
// Runtime configuration service and its own skeleton tests.
func TestFirstRunCreationPreservesExistingFiles(t *testing.T) {
	t.Run("first_run_refuses_overwrite/env", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Cleanup(func() { os.Unsetenv("LIGHTCODE_TEST_USERKEY") })
		path := filepath.Join(home, ".lightcode", ".env")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		custom := "# user managed file\nLIGHTCODE_TEST_USERKEY=1\n"
		if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadDotEnv(); err != nil {
			t.Fatalf("LoadDotEnv: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != custom {
			t.Fatalf("existing .env was overwritten: %q", after)
		}
	})
}
