package prompt_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/prompt"
)

func assemblyInput(workspace string, mode prompt.Mode, rules prompt.Rules) prompt.AssemblyInput {
	return prompt.AssemblyInput{
		Identity:      "## Identity\n\nCALLER_IDENTITY_MARKER",
		Instructions:  "Do the task.",
		Mode:          mode,
		Rules:         rules,
		Workspace:     workspace,
		SessionStart:  testStart,
		OS:            "testos",
		Shell:         "/bin/testsh",
		OSDescription: "TestOS/testarch",
	}
}

func assembleInput(t *testing.T, input prompt.AssemblyInput) string {
	t.Helper()
	assembled, err := prompt.Assemble(input)
	testassert.NoError(t, err)
	return assembled
}

func assertOrder(t *testing.T, text string, markers ...string) {
	t.Helper()
	previous := -1
	for _, marker := range markers {
		index := strings.Index(text, marker)
		testassert.Require(t, index >= 0, "missing order marker %q\n%s", marker, text)
		testassert.Require(t, index >= previous, "marker %q at %d appeared before previous marker at %d\n%s", marker, index, previous, text)
		previous = index
	}
}

func TestAssembleFullBaselineSectionsEnvironmentAndRules(t *testing.T) {
	workspace := t.TempDir()
	rules := prompt.Rules{GlobalText: "GLOBAL_RULES_MARKER", WorkspaceText: "WORKSPACE_RULES_MARKER"}
	assembled := assembleInput(t, assemblyInput(workspace, prompt.ModeFull, rules))

	for _, want := range []string{
		"## Identity",
		"CALLER_IDENTITY_MARKER",
		"## Core Rules",
		"## Rules File Guide",
		"## Compaction Awareness",
		"## Environment",
		"Working directory: " + workspace,
		"Platform: testos",
		"Shell: /bin/testsh",
		"OS: TestOS/testarch",
		"Session started: " + testStart.Format("2006-01-02 15:04:05 MST"),
		"## Safety",
		"## Tone",
		"## Task Execution",
		"## Language",
		"## Your Role and Instructions",
		"Do the task.",
		"GLOBAL_RULES_MARKER",
		"WORKSPACE_RULES_MARKER",
	} {
		testassert.Require(t, strings.Contains(assembled, want), "full prompt missing %q\n%s", want, assembled)
	}
	assertOrder(t, assembled,
		"## Identity",
		"CALLER_IDENTITY_MARKER",
		"## Core Rules",
		"## Rules File Guide",
		"## Compaction Awareness",
		"## Environment",
		"Working directory: "+workspace,
		"## Safety",
		"## Tone",
		"## Task Execution",
		"## Language",
		"## Your Role and Instructions",
		"Do the task.",
		"GLOBAL_RULES_MARKER",
		"WORKSPACE_RULES_MARKER",
	)
}

func TestAssembleSimpleBaselineSections(t *testing.T) {
	workspace := t.TempDir()
	rules := prompt.Rules{WorkspaceText: "SIMPLE_WORKSPACE_RULES"}
	assembled := assembleInput(t, assemblyInput(workspace, prompt.ModeSimple, rules))

	for _, want := range []string{
		"## Identity",
		"CALLER_IDENTITY_MARKER",
		"## Core Rules",
		"## Compaction Awareness",
		"## Environment",
		"Working directory: " + workspace,
		"## Safety",
		"## Your Role and Instructions",
		"Do the task.",
		"SIMPLE_WORKSPACE_RULES",
	} {
		testassert.Require(t, strings.Contains(assembled, want), "simple prompt missing %q\n%s", want, assembled)
	}
	for _, forbidden := range []string{"## Rules File Guide", "## Tone", "## Task Execution", "## Language"} {
		testassert.Require(t, !strings.Contains(assembled, forbidden), "simple prompt contains omitted section %q\n%s", forbidden, assembled)
	}
	assertOrder(t, assembled,
		"## Identity",
		"## Core Rules",
		"## Compaction Awareness",
		"## Environment",
		"Working directory: "+workspace,
		"## Safety",
		"## Your Role and Instructions",
		"SIMPLE_WORKSPACE_RULES",
	)
}

func TestAssembleNoneReturnsTrimmedInstructionsAlone(t *testing.T) {
	workspace := t.TempDir()
	rules := prompt.Rules{GlobalText: "GLOBAL_RULES_MARKER", WorkspaceText: "WORKSPACE_RULES_MARKER"}
	input := assemblyInput(workspace, prompt.ModeNone, rules)
	input.Instructions = "\n  Only compact this transcript.  \n"

	assembled := assembleInput(t, input)
	testassert.Equal(t, assembled, "Only compact this transcript.")
	for _, forbidden := range []string{
		"CALLER_IDENTITY_MARKER",
		"## Identity",
		"## Core Rules",
		"## Environment",
		"## Safety",
		"GLOBAL_RULES_MARKER",
		"WORKSPACE_RULES_MARKER",
	} {
		testassert.Require(t, !strings.Contains(assembled, forbidden), "none prompt contains omitted content %q\n%s", forbidden, assembled)
	}
}

func TestAssembleHeadingSuppression(t *testing.T) {
	const (
		safetyBody   = "Before modifying or deleting files, consider whether the action is reversible."
		toneBody     = "Be direct and concise."
		taskBody     = "For simple, well-defined tasks: execute immediately."
		languageBody = "Respond in the same language the user writes in."
	)
	sections := map[string]string{
		"safety":         safetyBody,
		"tone":           toneBody,
		"task_execution": taskBody,
		"language":       languageBody,
	}
	cases := []struct {
		name       string
		mode       prompt.Mode
		global     string
		workspace  string
		suppressed []string
	}{
		{"level 1 safety", prompt.ModeFull, "", "# Safety\n\nUSER_SAFETY_MARKER", []string{"safety"}},
		{"level 2 tone", prompt.ModeFull, "", "## Tone\n\nUSER_TONE_MARKER", []string{"tone"}},
		{"level 3 task execution", prompt.ModeFull, "", "### Task Execution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task execution", prompt.ModeFull, "", "## Task Execution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task underscore", prompt.ModeFull, "", "## task_execution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task hyphen", prompt.ModeFull, "", "## task-execution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task tab", prompt.ModeFull, "", "## Task\tExecution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task multiple spaces", prompt.ModeFull, "", "## Task  Execution\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"task closing hashes", prompt.ModeFull, "", "## Task Execution ##\n\nUSER_TASK_MARKER", []string{"task_execution"}},
		{"combined headings", prompt.ModeFull, "", "# Safety\ntext\n## Tone\n### Task Execution\n# other\n\nCOMBINED_USER_MARKER", []string{"safety", "tone", "task_execution"}},
		{"global safety suppression", prompt.ModeFull, "# Safety\n\nGLOBAL_SAFETY_MARKER", "", []string{"safety"}},
		{"simple safety suppression", prompt.ModeSimple, "", "# Safety\n\nSIMPLE_SAFETY_MARKER", []string{"safety"}},
		{"unrelated other heading", prompt.ModeFull, "", "## Other\n\nUSER_OTHER_MARKER", nil},
		{"unrelated environment heading", prompt.ModeFull, "", "## Environment\n\nUSER_ENVIRONMENT_MARKER", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := prompt.Rules{GlobalText: tc.global, WorkspaceText: tc.workspace}
			assembled := assembleInput(t, assemblyInput(t.TempDir(), tc.mode, rules))
			suppressed := make(map[string]bool, len(tc.suppressed))
			for _, name := range tc.suppressed {
				suppressed[name] = true
			}
			modeSections := sections
			if tc.mode == prompt.ModeSimple {
				modeSections = map[string]string{"safety": safetyBody}
			}
			for name, body := range modeSections {
				if suppressed[name] {
					testassert.Require(t, !strings.Contains(assembled, body), "built-in %s content survived its matching user heading in %q\n%s", name, tc.workspace+tc.global, assembled)
					continue
				}
				testassert.Require(t, strings.Contains(assembled, body), "unrelated built-in %s content was suppressed by %q\n%s", name, tc.workspace+tc.global, assembled)
			}
			userText := tc.workspace
			if userText == "" {
				userText = tc.global
			}
			testassert.Require(t, strings.Contains(assembled, strings.TrimSpace(userText)), "user rules text is missing\n%s", assembled)
		})
	}
}

func TestAssembleInstructionHeadingNormalization(t *testing.T) {
	workspace := t.TempDir()
	cases := []struct {
		instructions string
		headings     int
	}{
		{"Do the task.", 1},
		{"## Your Role and Instructions\n\nDo the task.", 1},
		{"# your-role-and-instructions\n\nDo the task.", 0},
	}
	for _, tc := range cases {
		input := assemblyInput(workspace, prompt.ModeFull, prompt.Rules{})
		input.Instructions = tc.instructions
		assembled := assembleInput(t, input)
		testassert.Require(t, strings.Count(assembled, "## Your Role and Instructions") == tc.headings, "instructions %q produced heading count %d, want %d\n%s", tc.instructions, strings.Count(assembled, "## Your Role and Instructions"), tc.headings, assembled)
		testassert.Require(t, strings.Contains(assembled, "Do the task."), "instructions %q lost their body\n%s", tc.instructions, assembled)
	}
}

func TestAssembleUsesOnlyExplicitInputs(t *testing.T) {
	home := t.TempDir()
	testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
	writeRules(t, filepath.Join(home, ".lightcode"), "AMBIENT_HOME_RULES")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/ambient-shell")

	workspace := t.TempDir()
	input := assemblyInput(workspace, prompt.ModeFull, prompt.Rules{WorkspaceText: "EXPLICIT_RULES"})
	input.Identity = ""
	input.Shell = "caller-shell"
	assembled := assembleInput(t, input)

	for _, forbidden := range []string{"AMBIENT_HOME_RULES", "Lightcode, an agentic coding assistant", "/ambient-shell"} {
		testassert.Require(t, !strings.Contains(assembled, forbidden), "assembly carried implicit input %q\n%s", forbidden, assembled)
	}
	testassert.Require(t, strings.Contains(assembled, "Shell: caller-shell") && strings.Contains(assembled, "EXPLICIT_RULES"), "assembly lost an explicit input\n%s", assembled)

	ambientRules := readRules(t, "", workspace, prompt.ModeFull)
	testassert.Require(t, ambientRules.GlobalText == "" && !strings.Contains(ambientRules.GlobalText, "AMBIENT_HOME_RULES"), "ReadRules looked up the ambient home: %+v", ambientRules)
}

func TestAssembleDoesNotRereadWorkspaceRules(t *testing.T) {
	workspace := t.TempDir()
	input := assemblyInput(workspace, prompt.ModeFull, prompt.Rules{})
	writeRules(t, workspace, "LATE_WORKSPACE_RULES")
	assembled := assembleInput(t, input)
	testassert.Require(t, !strings.Contains(assembled, "LATE_WORKSPACE_RULES"), "pure assembly read the Workspace instead of the captured rules")
}

func TestAssembleIsDeterministicForEqualInputs(t *testing.T) {
	workspace := t.TempDir()
	rules := readRules(t, t.TempDir(), workspace, prompt.ModeFull)
	firstInput := assemblyInput(workspace, prompt.ModeFull, rules)
	firstInput.SessionStart = testStart
	secondInput := assemblyInput(workspace, prompt.ModeFull, rules)
	secondInput.SessionStart = testStart.Add(90 * time.Minute)
	first := assembleInput(t, firstInput)
	second := assembleInput(t, secondInput)
	testassert.Require(t, first != second, "distinct SessionStart values produced identical prompts")
	testassert.Require(t, strings.Contains(first, "Session started: "+testStart.Format("2006-01-02 15:04:05 MST")), "first prompt lost its SessionStart\n%s", first)
	testassert.Require(t, strings.Contains(second, "Session started: "+secondInput.SessionStart.Format("2006-01-02 15:04:05 MST")), "second prompt lost its SessionStart\n%s", second)
	for i := 0; i < 5; i++ {
		testassert.Equal(t, assembleInput(t, firstInput), first)
		testassert.Equal(t, assembleInput(t, secondInput), second)
	}
}

func TestAssembleInvalidModeFails(t *testing.T) {
	for _, mode := range []prompt.Mode{"", "bogus"} {
		input := assemblyInput(t.TempDir(), mode, prompt.Rules{WorkspaceText: "RULES"})
		assembled, err := prompt.Assemble(input)
		testassert.Require(t, errors.Is(err, prompt.ErrInvalidInput) && assembled == "", "Assemble(%q) = (%q, %v), want ErrInvalidInput and no prompt", mode, assembled, err)
	}
}

func TestAssembleNoneSampleTimeIsNotRead(t *testing.T) {
	input := assemblyInput(t.TempDir(), prompt.ModeNone, prompt.Rules{})
	input.SessionStart = time.Time{}
	assembled := assembleInput(t, input)
	testassert.Equal(t, assembled, "Do the task.")
}
