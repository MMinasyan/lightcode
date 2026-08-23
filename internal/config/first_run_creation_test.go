package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/config"
)

// TestFirstRunCreationPreservesExistingFiles verifies that first-run creators
// use create-if-absent semantics: an existing file (a concurrent user's, or
// one written by the other independent creator) is never overwritten.
func TestFirstRunCreationPreservesExistingFiles(t *testing.T) {
	// markPreserved appends a JSON-insignificant trailing newline so the file
	// differs byte-for-byte from anything a creator would freshly write; a
	// creator that overwrote it would drop the extra byte.
	markPreserved := func(t *testing.T, path string) []byte {
		t.Helper()
		created, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		marked := append(append([]byte(nil), created...), '\n')
		if err := os.WriteFile(path, marked, 0o600); err != nil {
			t.Fatal(err)
		}
		return marked
	}

	t.Run("first_run_refuses_overwrite/config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if _, err := config.Load(path); err != nil {
			t.Fatalf("initial Load: %v", err)
		}
		marked := markPreserved(t, path)
		if _, err := config.Load(path); err != nil {
			t.Fatalf("reload: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(marked) {
			t.Fatal("existing config.json was overwritten on second Load")
		}
	})

	t.Run("first_run_refuses_overwrite/agents", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agents.json")
		if _, err := agents.Load(path); err != nil {
			t.Fatalf("initial Load: %v", err)
		}
		marked := markPreserved(t, path)
		if _, err := agents.Load(path); err != nil {
			t.Fatalf("reload: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(marked) {
			t.Fatal("existing agents.json was overwritten on second Load")
		}
	})

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
