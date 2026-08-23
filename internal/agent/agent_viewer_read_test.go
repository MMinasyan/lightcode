package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestProjectPathForSessionUsesSessionProjectAuthority(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	root, err := a.ProjectPathForSession(id)
	if err != nil {
		t.Fatalf("ProjectPathForSession(%q): %v", id, err)
	}
	if root != a.ProjectRoot() {
		t.Fatalf("ProjectPathForSession(%q) = %q, want %q", id, root, a.ProjectRoot())
	}
	if _, err := a.ProjectPathForSession("missing"); err == nil {
		t.Fatalf("ProjectPathForSession(missing) = %v, want an error", err)
	}
}

// Agent.ReadFileContent (and the adapter paths that share it) must
// enforce the project-root boundary: no absolute outside-project paths,
// no `..` escape, no symlink escape, no hardlinks, no sensitive-name
// leaves; ordinary project files and canonical-inside symlinked-project
// access remain allowed.
func TestPR11Closure_ViewerReadEnforcesBoundary(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	// Setup. Files used by sub-tests.
	insideFile := filepath.Join(a.projectRoot, "ok.txt")
	if err := os.WriteFile(insideFile, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	escapingLink := filepath.Join(a.projectRoot, "escape.txt")
	if err := os.Symlink(outsideSecret, escapingLink); err != nil {
		t.Fatal(err)
	}

	hardlinked := filepath.Join(a.projectRoot, "hardlinked.txt")
	if err := os.Link(outsideSecret, hardlinked); err != nil {
		t.Fatal(err)
	}

	sensitive := filepath.Join(a.projectRoot, ".env")
	if err := os.WriteFile(sensitive, []byte("API_KEY=secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlinked path inside the project whose canonical target also stays
	// inside — must remain readable.
	aliasInside := filepath.Join(a.projectRoot, "aka.txt")
	if err := os.Symlink(insideFile, aliasInside); err != nil {
		t.Fatal(err)
	}

	// Raw relative `..` string built by concatenation so filepath.Join in the
	// test setup does not clean it away — ReadFileContent must see the literal
	// `..` and resolve it against projectRoot before deciding.
	relativeDotdotEscape := "../" + filepath.Base(outsideDir) + "/secret.txt"

	cases := []struct {
		name       string
		path       string
		wantReject bool
	}{
		{name: "ordinary_project_file_accepted", path: insideFile, wantReject: false},
		{name: "relative_inside_accepted", path: "ok.txt", wantReject: false},
		{name: "symlinked_path_accepted", path: aliasInside, wantReject: false},
		{name: "absolute_outside_rejected", path: outsideSecret, wantReject: true},
		{name: "relative_dotdot_escape_rejected", path: relativeDotdotEscape, wantReject: true},
		{name: "escaping_symlink_rejected", path: escapingLink, wantReject: true},
		{name: "hardlink_to_outside_rejected", path: hardlinked, wantReject: true},
		{name: "sensitive_leaf_rejected", path: sensitive, wantReject: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, err := a.ReadFileContent(tc.path)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("ReadFileContent(%q) succeeded with content %q; want boundary refusal", tc.path, content)
				}
				if strings.Contains(content, "outside-secret") || strings.Contains(content, "API_KEY=secret") {
					t.Fatalf("ReadFileContent(%q) leaked sensitive content despite returning error", tc.path)
				}
			} else {
				if err != nil {
					t.Fatalf("ReadFileContent(%q) error = %v; want success", tc.path, err)
				}
			}
		})
	}
}

// Agent.projectRoot itself reached via a symlink (e.g. Lightcode launched
// from a symlinked working directory). A file inside the real project,
// addressed via the symlinked root path, must still read because canonical
// resolution stays inside the project.
func TestPR11Closure_ViewerReadAcceptsSymlinkedProjectRoot(t *testing.T) {
	home := t.TempDir()
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
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	realProject := t.TempDir()
	symlinkRoot := filepath.Join(t.TempDir(), "linked-project")
	if err := os.Symlink(realProject, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(realProject, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside-via-symlink-root"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := New(Config{Cfg: cfg, ProjectRoot: symlinkRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	content, err := a.ReadFileContent(filepath.Join(symlinkRoot, "inside.txt"))
	if err != nil {
		t.Fatalf("ReadFileContent via symlinked projectRoot = %v; want success", err)
	}
	if content != "inside-via-symlink-root" {
		t.Fatalf("content = %q, want %q", content, "inside-via-symlink-root")
	}
}
