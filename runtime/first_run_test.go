package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestInitializeFirstRunConfigurationCreatesFirstRunDocument pins the
// Runtime host's first-run initialization: a missing document is created
// exclusively with the shipped skeleton, and an existing user document is
// never rewritten.
func TestInitializeFirstRunConfigurationCreatesFirstRunDocument(t *testing.T) {
	t.Run("missing document", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		testassert.NoError(t, InitializeFirstRunConfiguration(path))
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, string(data) == mainConfigSkeleton, "first-run document = %q, want the shipped skeleton", data)
		info, err := os.Stat(path)
		testassert.NoError(t, err)
		testassert.Require(t, info.Mode().Perm() == 0o600, "first-run mode = %o, want 0600", info.Mode().Perm())
	})

	t.Run("existing document", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		custom := []byte(`{"providers":{"custom":{}}}`)
		testassert.NoError(t, os.WriteFile(path, custom, 0o600))
		testassert.NoError(t, InitializeFirstRunConfiguration(path))
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, string(data) == string(custom), "existing document = %q, want it untouched", data)
	})
}
