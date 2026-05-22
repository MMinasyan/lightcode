package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
)

// App.ReadFileContent (the Wails-bound surface) is a passthrough to
// agent.ReadFileContent. The Wails adapter must propagate the agent's
// boundary refusal — under the bug, an outside-project path returns
// content; under the fix, an error is returned and content is empty.
func TestPR11Closure_AppReadFileContentPropagatesViewerBoundaryRefusal(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ag, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	app := &App{svc: ag}

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	content, err := app.ReadFileContent(outsideSecret)
	if err == nil {
		t.Fatalf("App.ReadFileContent(%q) succeeded with content %q; want boundary refusal propagated from agent", outsideSecret, content)
	}
	if strings.Contains(content, "outside-secret") {
		t.Fatalf("App.ReadFileContent(%q) leaked outside content despite returning error", outsideSecret)
	}
}
