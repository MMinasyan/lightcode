package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectOverrides(t *testing.T) {
	got := detectOverrides("# Safety\ntext\n## Tone\n# other\n")
	if !got["safety"] || !got["tone"] {
		t.Fatalf("detectOverrides = %+v, want safety and tone", got)
	}
	if got["task_execution"] || got["language"] {
		t.Fatalf("detectOverrides false positives = %+v", got)
	}
}

func TestBuildIncludesBaseEnvironmentAndRules(t *testing.T) {
	a := New("/work/project", t.TempDir())
	prompt := a.build("global rules", "project rules", nil)
	for _, want := range []string{"Working directory: /work/project", "global rules", "project rules"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n%s", want, prompt)
		}
	}
	withoutOverride := a.build("", "", nil)
	withOverride := a.build("# Safety\ncustom", "", nil)
	if strings.Contains(withOverride, strings.TrimSpace(safetySection)) && !strings.Contains(withoutOverride, strings.TrimSpace(safetySection)) {
		t.Fatal("sanity check failed for safety section")
	}
	if strings.Contains(withOverride, strings.TrimSpace(safetySection)) {
		t.Fatal("safety section was not overridden by # Safety rules")
	}
}

func TestAssemblerWarningsAndCache(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	a := New(projectRoot, home)
	first := a.Assemble()
	if !first.Rebuilt {
		t.Fatal("first Assemble Rebuilt = false, want true")
	}
	if !hasWarning(first.Warnings, WarnRulesNotFound) {
		t.Fatalf("warnings = %+v, want rules_not_found", first.Warnings)
	}
	second := a.Assemble()
	if second.Rebuilt {
		t.Fatal("second Assemble Rebuilt = true, want cached false")
	}

	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte(strings.Repeat("x", 20001)), 0o600); err != nil {
		t.Fatal(err)
	}
	third := a.Assemble()
	if !third.Rebuilt {
		t.Fatal("Assemble after rules change Rebuilt = false, want true")
	}
	if !hasWarning(third.Warnings, WarnRulesTooLarge) {
		t.Fatalf("warnings = %+v, want rules_too_large", third.Warnings)
	}
}

func TestReadRulesFilePrefersAgentsThenClaude(t *testing.T) {
	dir := t.TempDir()
	if got, err := readRulesFile(dir); err != nil || got != "" {
		t.Fatalf("empty readRulesFile = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readRulesFile(dir); err != nil || got != "claude" {
		t.Fatalf("CLAUDE readRulesFile = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readRulesFile(dir); err != nil || got != "agents" {
		t.Fatalf("AGENTS readRulesFile = %q, %v", got, err)
	}
}

func hasWarning(warnings []Warning, kind string) bool {
	for _, w := range warnings {
		if w.Kind == kind {
			return true
		}
	}
	return false
}
