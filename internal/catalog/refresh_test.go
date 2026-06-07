package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserCostProtectionUsesConfiguredConfigPath(t *testing.T) {
	home := t.TempDir()
	defaultConfig := filepath.Join(home, ".lightcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(defaultConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultConfig, []byte(`{
  "providers": {
    "test": { "models": { "model": { "cost": { "input": 1 } } } }
  },
  "default_model": ""
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	customConfig := filepath.Join(t.TempDir(), "custom-config.json")
	if err := os.WriteFile(customConfig, []byte(`{
  "providers": {
    "test": { "models": { "model": { "cost": { "output": 2, "cache_read": 3 } } } }
  },
  "default_model": ""
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	protected := userCostProtectionForProviderAt(home, customConfig, "test")
	if protected["model"]["input"] {
		t.Fatalf("protected input from default config; want custom config only: %#v", protected)
	}
	if !protected["model"]["output"] || !protected["model"]["cache_read"] {
		t.Fatalf("protected = %#v, want custom config cost fields", protected)
	}
}
