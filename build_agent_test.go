package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

// The legacy agent bootstrap keeps nil managed state when the dotenv load
// fails after partial injection, so a key already injected before the
// failure keeps its prior key-source classification: external, not managed.
func TestBuildAgentDotenvErrorKeepsNilManagedState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIGHTCODE_CONFIG", "")

	key := "LIGHTCODE_TEST_BUILD_AGENT_KEY"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}

	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configJSON := fmt.Sprintf(`{
  "providers": {
    "build-agent": {
      "name": "Build Agent Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": %q },
      "discovery": false,
      "models": {
        "build-agent-model": { "name": "Build Agent Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  }
}`, key)
	if err := os.WriteFile(filepath.Join(lightcodeDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid line followed by an oversized line: the key is injected, then
	// the dotenv scanner fails.
	body := key + "=injected-value\nHUGE=" + strings.Repeat("A", bufio.MaxScanTokenSize+1) + "\n"
	if err := os.WriteFile(filepath.Join(lightcodeDir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ag, err := buildAgent()
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if got := os.Getenv(key); got != "injected-value" {
		t.Fatalf("env %s = %q, want the value injected before the dotenv error", key, got)
	}
	for _, st := range ag.ProviderList() {
		if st.ID != "build-agent" {
			continue
		}
		if st.KeySource != config.KeySourceExternal {
			t.Fatalf("KeySource = %q, want %q (nil managed state keeps the injected key external)", st.KeySource, config.KeySourceExternal)
		}
		return
	}
	t.Fatal("provider build-agent not in ProviderList")
}
