package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

// testStart pins the rendered session start so prompts built in different calls
// are byte-comparable.
var testStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// assembleSpec renders one prompt through a fresh stateless service.
func assembleSpec(root, home string, spec Spec) Result {
	return NewService(home).Assemble(root, testStart, spec)
}

// assembleFull renders the default full/memory-on prompt for adapt (nil = baseline).
func assembleFull(root, home string, adapt *adaptation.Adaptation) Result {
	return assembleSpec(root, home, Spec{Size: SizeFull, Memory: true, Adapt: adapt})
}

// buildFull renders the full/memory-on body directly from rules strings, without
// reading rules files — used by the section-composition tests.
func buildFull(root, globalRules, projectRules string, adapt *adaptation.Adaptation) string {
	return buildSpec(root, testStart, globalRules, projectRules, Spec{Size: SizeFull, Memory: true, Adapt: adapt})
}

func TestDetectOverrides(t *testing.T) {
	got := detectOverrides("# Safety\ntext\n## Tone\n### Task Execution\n# other\n")
	if !got["safety"] || !got["tone"] {
		t.Fatalf("detectOverrides = %+v, want safety and tone", got)
	}
	if !got["task_execution"] {
		t.Fatalf("detectOverrides = %+v, want task_execution", got)
	}
	if got["language"] {
		t.Fatalf("detectOverrides false positives = %+v", got)
	}
	for _, rules := range []string{"## Task Execution", "## task_execution", "## task-execution", "## Task\tExecution", "## Task  Execution", "## Task Execution ##"} {
		if !detectOverrides(rules)["task_execution"] {
			t.Fatalf("detectOverrides(%q) did not match task_execution", rules)
		}
	}
	if got := detectOverrides("## Environment"); got["safety"] || got["tone"] || got["task_execution"] || got["language"] {
		t.Fatalf("environment heading caused override = %+v", got)
	}
}

func TestBuildIncludesBaseEnvironmentAndRules(t *testing.T) {
	prompt := buildFull("/work/project", "global rules", "project rules", nil)
	for _, want := range []string{"Working directory: /work/project", "global rules", "project rules"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n%s", want, prompt)
		}
	}
	withoutOverride := buildFull("/work/project", "", "", nil)
	withOverride := buildFull("/work/project", "# Safety\ncustom", "", nil)
	if strings.Contains(withOverride, strings.TrimSpace(safetySection)) && !strings.Contains(withoutOverride, strings.TrimSpace(safetySection)) {
		t.Fatal("sanity check failed for safety section")
	}
	if strings.Contains(withOverride, strings.TrimSpace(safetySection)) {
		t.Fatal("safety section was not overridden by # Safety rules")
	}
}

func TestBuildIncludesReadableSectionHeadings(t *testing.T) {
	prompt := buildFull("/work/project", "", "", nil)
	for _, want := range []string{
		"## Identity",
		"## Core Rules",
		"## Rules File Guide",
		"## Compaction Awareness",
		"## Environment",
		"## Memory Instructions",
		"## Safety",
		"## Tone",
		"## Task Execution",
		"## Language",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing heading %q\n%s", want, prompt)
		}
	}
	idxEnv := strings.Index(prompt, "## Environment")
	idxWorkingDir := strings.Index(prompt, "Working directory: /work/project")
	if idxEnv < 0 || idxWorkingDir < 0 || idxEnv > idxWorkingDir {
		t.Fatalf("environment heading not before working directory: heading=%d workingDir=%d", idxEnv, idxWorkingDir)
	}
}

func TestBuildTaskExecutionOverrideUsesReadableHeading(t *testing.T) {
	withoutOverride := buildFull("/work/project", "", "", nil)
	withOverride := buildFull("/work/project", "## Task Execution\ncustom task rules", "", nil)
	if !strings.Contains(withoutOverride, strings.TrimSpace(taskExecutionSection)) {
		t.Fatal("sanity check failed for task execution section")
	}
	if strings.Contains(withOverride, strings.TrimSpace(taskExecutionSection)) {
		t.Fatal("task execution section was not overridden by ## Task Execution rules")
	}
	if !strings.Contains(withOverride, "custom task rules") {
		t.Fatal("custom task execution rules missing")
	}
}

func TestAssemblerWarnings(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	first := assembleFull(projectRoot, home, nil)
	if !hasWarning(first.Warnings, WarnRulesNotFound) {
		t.Fatalf("warnings = %+v, want rules_not_found", first.Warnings)
	}

	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte(strings.Repeat("x", 20001)), 0o600); err != nil {
		t.Fatal(err)
	}
	second := assembleFull(projectRoot, home, nil)
	if !hasWarning(second.Warnings, WarnRulesTooLarge) {
		t.Fatalf("warnings = %+v, want rules_too_large", second.Warnings)
	}
	// The service reads rules fresh every call: the warning reflects the current
	// on-disk state, not a cached earlier result.
	if hasWarning(second.Warnings, WarnRulesNotFound) {
		t.Fatalf("warnings = %+v, want no rules_not_found after AGENTS.md written", second.Warnings)
	}
}

// TestServiceIsStatelessAcrossProjectsAndSessions verifies the shared service
// holds no per-call state: it renders each caller's own project root and session
// start, and two projects with different rules stay isolated.
func TestServiceIsStatelessAcrossProjectsAndSessions(t *testing.T) {
	home := t.TempDir()
	projectA := t.TempDir()
	projectB := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectA, "AGENTS.md"), []byte("PROJECT_A_RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectB, "AGENTS.md"), []byte("PROJECT_B_RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(home)
	spec := Spec{Size: SizeFull, Memory: true}

	// Different project roots: each prompt renders its own working directory and
	// rules; neither leaks the other's.
	a := svc.Assemble(projectA, testStart, spec).Prompt
	b := svc.Assemble(projectB, testStart, spec).Prompt
	if !strings.Contains(a, "Working directory: "+projectA) || !strings.Contains(a, "PROJECT_A_RULES") {
		t.Fatalf("project A prompt missing its own root/rules\n%s", a)
	}
	if strings.Contains(a, projectB) || strings.Contains(a, "PROJECT_B_RULES") {
		t.Fatalf("project A prompt leaked project B state\n%s", a)
	}
	if !strings.Contains(b, "Working directory: "+projectB) || !strings.Contains(b, "PROJECT_B_RULES") {
		t.Fatalf("project B prompt missing its own root/rules\n%s", b)
	}

	// Re-rendering project A after project B yields byte-identical output: no state
	// carried over from the intervening call.
	if again := svc.Assemble(projectA, testStart, spec).Prompt; again != a {
		t.Fatalf("re-render of project A differs after an intervening project B call\n--- first ---\n%s\n--- again ---\n%s", a, again)
	}

	// Distinct session starts render distinct environment lines for the same root.
	otherStart := testStart.Add(90 * time.Minute)
	early := svc.Assemble(projectA, testStart, spec).Prompt
	late := svc.Assemble(projectA, otherStart, spec).Prompt
	if early == late {
		t.Fatal("distinct session starts produced identical prompts")
	}
	if !strings.Contains(early, testStart.Format("2006-01-02 15:04:05 MST")) ||
		!strings.Contains(late, otherStart.Format("2006-01-02 15:04:05 MST")) {
		t.Fatal("session start not rendered per call")
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
