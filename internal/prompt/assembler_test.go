package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// testStart pins the rendered session start so prompts built in different calls
// are byte-comparable.
var testStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// assembleSpec renders one prompt through a fresh stateless service.
func assembleSpec(root, home string, spec Spec) Result {
	return NewService(home).Assemble(root, testStart, spec)
}

// assembleFull renders the default full prompt for adapt (nil = baseline).
func assembleFull(root, home string, adapt *adaptation.Adaptation) Result {
	return assembleSpec(root, home, Spec{Size: SizeFull, Adapt: adapt})
}

// buildFull renders the full body directly from rules strings, without
// reading rules files — used by the section-composition tests.
func buildFull(root, globalRules, projectRules string, adapt *adaptation.Adaptation) string {
	return buildSpec(root, testStart, globalRules, projectRules, Spec{Size: SizeFull, Adapt: adapt})
}

func TestDetectOverrides(t *testing.T) {
	got := detectOverrides("# Safety\ntext\n## Tone\n### Task Execution\n# other\n")
	testassert.Require(t, got["safety"] && got["tone"] && got["task_execution"] && !got["language"], "detectOverrides = %+v, want safety and tone; detectOverrides = %+v, want task_execution; detectOverrides false positives = %+v", got, got, got)
	for _, rules := range []string{"## Task Execution", "## task_execution", "## task-execution", "## Task\tExecution", "## Task  Execution", "## Task Execution ##"} {
		testassert.Require(t, detectOverrides(rules)["task_execution"], "detectOverrides(%q) did not match task_execution", rules)
	}
	{
		got := detectOverrides("## Environment")
		testassert.Require(t, !got["safety"] && !got["tone"] && !got["task_execution"] && !got["language"], "environment heading caused override = %+v", got)
	}
}

func TestBuildIncludesBaseEnvironmentAndRules(t *testing.T) {
	prompt := buildFull("/work/project", "global rules", "project rules", nil)
	for _, want := range []string{"Working directory: /work/project", "global rules", "project rules"} {
		testassert.Require(t, strings.Contains(prompt, want), "prompt missing %q\n%s", want, prompt)
	}
	withoutOverride := buildFull("/work/project", "", "", nil)
	withOverride := buildFull("/work/project", "# Safety\ncustom", "", nil)
	testassert.Require(t, !strings.Contains(withOverride, strings.TrimSpace(safetySection)) || strings.Contains(withoutOverride, strings.TrimSpace(safetySection)), "sanity check failed for safety section")
	testassert.Require(t, !strings.Contains(withOverride, strings.TrimSpace(safetySection)), "safety section was not overridden by # Safety rules")
}

func TestBuildIncludesReadableSectionHeadings(t *testing.T) {
	prompt := buildFull("/work/project", "", "", nil)
	for _, want := range []string{
		"## Identity",
		"## Core Rules",
		"## Rules File Guide",
		"## Compaction Awareness",
		"## Environment",
		"## Safety",
		"## Tone",
		"## Task Execution",
		"## Language",
	} {
		testassert.Require(t, strings.Contains(prompt, want), "prompt missing heading %q\n%s", want, prompt)
	}
	idxEnv := strings.Index(prompt, "## Environment")
	idxWorkingDir := strings.Index(prompt, "Working directory: /work/project")
	testassert.Require(t, idxEnv >= 0 && idxWorkingDir >= 0 && idxEnv <= idxWorkingDir, "environment heading not before working directory: heading=%d workingDir=%d", idxEnv, idxWorkingDir)
}

func TestBuildTaskExecutionOverrideUsesReadableHeading(t *testing.T) {
	withoutOverride := buildFull("/work/project", "", "", nil)
	withOverride := buildFull("/work/project", "## Task Execution\ncustom task rules", "", nil)
	testassert.Require(t, strings.Contains(withoutOverride, strings.TrimSpace(taskExecutionSection)), "sanity check failed for task execution section")
	testassert.Require(t, !strings.Contains(withOverride, strings.TrimSpace(taskExecutionSection)), "task execution section was not overridden by ## Task Execution rules")
	testassert.Require(t, strings.Contains(withOverride, "custom task rules"), "custom task execution rules missing")
}

func TestAssemblerWarnings(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	first := assembleFull(projectRoot, home, nil)
	testassert.Require(t, hasWarning(first.Warnings, WarnRulesNotFound), "warnings = %+v, want rules_not_found", first.Warnings)

	testassert.NoError(t, os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte(strings.Repeat("x", 20001)), 0o600))
	second := assembleFull(projectRoot, home, nil)
	testassert.Require(t, hasWarning(second.Warnings, WarnRulesTooLarge), "warnings = %+v, want rules_too_large", second.Warnings)
	// The service reads rules fresh every call: the warning reflects the current
	// on-disk state, not a cached earlier result.
	testassert.Require(t, !hasWarning(second.Warnings, WarnRulesNotFound), "warnings = %+v, want no rules_not_found after AGENTS.md written", second.Warnings)
}

// TestServiceIsStatelessAcrossProjectsAndSessions verifies the shared service
// holds no per-call state: it renders each caller's own project root and session
// start, and two projects with different rules stay isolated.
func TestServiceIsStatelessAcrossProjectsAndSessions(t *testing.T) {
	home := t.TempDir()
	projectA := t.TempDir()
	projectB := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(projectA, "AGENTS.md"), []byte("PROJECT_A_RULES"), 0o600))
	testassert.NoError(t, os.WriteFile(filepath.Join(projectB, "AGENTS.md"), []byte("PROJECT_B_RULES"), 0o600))
	svc := NewService(home)
	spec := Spec{Size: SizeFull}

	// Different project roots: each prompt renders its own working directory and
	// rules; neither leaks the other's.
	a := svc.Assemble(projectA, testStart, spec).Prompt
	b := svc.Assemble(projectB, testStart, spec).Prompt
	testassert.Require(t, strings.Contains(a, "Working directory: "+projectA) && strings.Contains(a, "PROJECT_A_RULES") && !strings.Contains(a, projectB) && !strings.Contains(a, "PROJECT_B_RULES"), "project A prompt missing its own root/rules\n%s; project A prompt leaked project B state\n%s", a, a)
	testassert.Require(t, strings.Contains(b, "Working directory: "+projectB) && strings.Contains(b, "PROJECT_B_RULES"), "project B prompt missing its own root/rules\n%s", b)

	// Re-rendering project A after project B yields byte-identical output: no state
	// carried over from the intervening call.
	testassert.Equal(t, svc.Assemble(projectA, testStart, spec).Prompt, a)

	// Distinct session starts render distinct environment lines for the same root.
	otherStart := testStart.Add(90 * time.Minute)
	early := svc.Assemble(projectA, testStart, spec).Prompt
	late := svc.Assemble(projectA, otherStart, spec).Prompt
	testassert.Require(t, early != late, "distinct session starts produced identical prompts")
	testassert.Require(t, strings.Contains(early, testStart.Format("2006-01-02 15:04:05 MST")) && strings.Contains(late, otherStart.Format("2006-01-02 15:04:05 MST")), "session start not rendered per call")
}

// TestAssembledBaselinePromptDoesNotAdvertiseMemoryTools is the Step-2 prompt
// regression: with memory removed, the assembled baseline prompt must not advertise
// the deleted save_memory / search_memory / search_history tools or any memory
// tooling. Before removal the rules-file guide and the embedded memory instructions
// directed the model to use memory tools; both are gone now, so no assembled prompt
// (full/simple/none) may reintroduce them.
func TestAssembledBaselinePromptDoesNotAdvertiseMemoryTools(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	for _, size := range []string{SizeFull, SizeSimple, SizeNone} {
		prompt := assembleSpec(root, home, Spec{Size: size}).Prompt
		for _, forbidden := range []string{
			"save_memory",
			"search_memory",
			"search_history",
			"memory tools",
			"Memory Instructions",
		} {
			testassert.Require(t, !strings.Contains(prompt, forbidden), "assembled %s prompt still advertises removed memory behavior (%q):\n%s", size, forbidden, prompt)
		}
	}
}

func TestReadRulesFilePrefersAgentsThenClaude(t *testing.T) {
	dir := t.TempDir()
	{
		got, err := readRulesFile(dir)
		testassert.Require(t, err == nil && got == "", "empty readRulesFile = %q, %v", got, err)
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o600))
	{
		got, err := readRulesFile(dir)
		testassert.Require(t, err == nil && got == "claude", "CLAUDE readRulesFile = %q, %v", got, err)
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o600))
	{
		got, err := readRulesFile(dir)
		testassert.Require(t, err == nil && got == "agents", "AGENTS readRulesFile = %q, %v", got, err)
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
