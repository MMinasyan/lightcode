package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestUserCostProtectionUsesConfiguredConfigPath(t *testing.T) {
	home := t.TempDir()
	defaultConfig := filepath.Join(home, ".lightcode", "config.json")
	testassert.NoError(t, os.MkdirAll(filepath.Dir(defaultConfig), 0o700))
	testassert.NoError(t, os.WriteFile(defaultConfig, []byte(`{
  "providers": {
    "test": { "models": { "model": { "cost": { "input": 1 } } } }
  },
  "default_model": ""
}`), 0o600))

	customConfig := filepath.Join(t.TempDir(), "custom-config.json")
	testassert.NoError(t, os.WriteFile(customConfig, []byte(`{
  "providers": {
    "test": { "models": { "model": { "cost": { "output": 2, "cache_read": 3 } } } }
  },
  "default_model": ""
}`), 0o600))

	protected := userCostProtectionForProviderAt(home, customConfig, "test")
	testassert.Require(t, !protected["model"]["input"] && protected["model"]["output"] && protected["model"]["cache_read"], "protected input from default config; want custom config only: %#v; protected = %#v, want custom config cost fields", protected, protected)
}
