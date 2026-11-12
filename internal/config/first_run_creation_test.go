package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Syntheresis/melta/internal/config"
	"github.com/Syntheresis/melta/internal/testassert"
)

// TestManagedEnvCaptureCreatesNothing verifies the explicit dotenv source is
// read-only until an owning write: capturing an absent file creates neither
// the file nor its parent directory. First-run config.json creation belongs to
// the root Runtime command.
func TestManagedEnvCaptureCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", ".env")
	managed := config.NewManagedEnv(path)
	captured, err := managed.Capture([]string{"MELTA_TEST_MISSING"})
	testassert.NoError(t, err)
	testassert.Require(t, !captured["MELTA_TEST_MISSING"].Defined, "absent file captured a defined value")
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "capture created the .env file: %v", err)
	}
	{
		_, err := os.Stat(filepath.Dir(path))
		testassert.Require(t, os.IsNotExist(err), "capture created the parent directory: %v", err)
	}
}
