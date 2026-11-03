// Package prompt assembles the baseline system prompt from explicit caller
// inputs. It owns no ambient person, location, environment or clock: rules
// reading binds the caller's directories, and assembly renders only the
// supplied identity, instructions, rule text and captured environment values.
package prompt

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed core_rules.md
var coreRulesSection string

//go:embed rules_file_guide.md
var rulesFileGuideSection string

//go:embed compaction_awareness.md
var compactionAwarenessSection string

//go:embed safety.md
var safetySection string

//go:embed tone.md
var toneSection string

//go:embed task_execution.md
var taskExecutionSection string

//go:embed language.md
var languageSection string

var overridableOrder = []string{"safety", "tone", "task_execution", "language"}

var overridableSections = map[string]string{
	"safety":         safetySection,
	"tone":           toneSection,
	"task_execution": taskExecutionSection,
	"language":       languageSection,
}

// Mode selects one of the three baseline prompt sizes. It is a closed set:
// every other value, including the empty string, is invalid input.
type Mode string

const (
	ModeNone   Mode = "none"
	ModeSimple Mode = "simple"
	ModeFull   Mode = "full"

	WarnRulesTooLarge  = "rules_too_large"
	WarnRulesNotFound  = "rules_not_found"
	WarnRulesReadError = "rules_read_error"
)

// ErrInvalidInput is the typed invalid-input failure for an unsupported mode.
var ErrInvalidInput = errors.New("prompt: invalid input")

const agentPromptHeading = "## Your Role and Instructions"

// Warning is one non-fatal rule-reading diagnostic.
type Warning struct {
	Kind    string
	Message string
}

// Rules is the captured rule text one assembly consumes. Warnings carries the
// diagnostics of the read that produced it.
type Rules struct {
	GlobalText    string
	WorkspaceText string
	Warnings      []Warning
}

// AssemblyInput is the complete pure input of one baseline prompt. Identity is
// the application-supplied persona; none mode renders Instructions alone.
// OS, Shell and OSDescription are captured environment values rendered by the
// full and simple sizes; OS is the platform identifier.
type AssemblyInput struct {
	Identity      string
	Instructions  string
	Mode          Mode
	Rules         Rules
	Workspace     string
	SessionStart  time.Time
	OS            string
	Shell         string
	OSDescription string
}

// ReadRules reads the current global and Workspace AGENTS.md text for mode.
// None reads nothing and yields no diagnostics. An empty global directory
// performs no global lookup. Ordinary read problems become warnings while an
// unsupported mode fails with ErrInvalidInput before any read.
func ReadRules(globalDir, workspace string, mode Mode) (Rules, error) {
	if !validMode(mode) {
		return Rules{}, fmt.Errorf("%w: unsupported mode %q", ErrInvalidInput, mode)
	}
	var rules Rules
	if mode == ModeNone {
		return rules, nil
	}

	if globalDir != "" {
		content, err := readRulesFile(globalDir)
		if err != nil {
			rules.Warnings = append(rules.Warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read global rules file: " + err.Error()})
			content = ""
		}
		rules.GlobalText = content
	}

	workspaceContent, err := readRulesFile(workspace)
	if err != nil {
		rules.Warnings = append(rules.Warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read project rules file: " + err.Error()})
		workspaceContent = ""
	}
	rules.WorkspaceText = workspaceContent

	if rules.WorkspaceText == "" && rules.GlobalText == "" {
		rules.Warnings = append(rules.Warnings, Warning{Kind: WarnRulesNotFound, Message: "No AGENTS.md found"})
	}

	combined := rules.GlobalText + "\x00" + rules.WorkspaceText
	if len(combined) > 20000 {
		rules.Warnings = append(rules.Warnings, Warning{Kind: WarnRulesTooLarge, Message: fmt.Sprintf("Rules file exceeds 20,000 characters (%d chars). Consider trimming it.", len(combined))})
	}
	return rules, nil
}

// Assemble renders the complete baseline prompt for input. It performs no I/O
// and reads no ambient value: equal inputs produce equal output. None returns
// the trimmed Instructions alone; full and simple retain the baseline section
// order, separators, rules suppression and role-heading normalization.
func Assemble(input AssemblyInput) (string, error) {
	if !validMode(input.Mode) {
		return "", fmt.Errorf("%w: unsupported mode %q", ErrInvalidInput, input.Mode)
	}
	if input.Mode == ModeNone {
		return strings.TrimSpace(input.Instructions), nil
	}
	return buildSpec(input), nil
}

func validMode(mode Mode) bool {
	switch mode {
	case ModeNone, ModeSimple, ModeFull:
		return true
	default:
		return false
	}
}

func buildSpec(input AssemblyInput) string {
	var b strings.Builder

	for _, section := range defaultSectionsForMode(input) {
		b.WriteString(strings.TrimSpace(section))
		b.WriteString("\n\n")
	}

	b.WriteString(renderEnvironment(input))
	b.WriteString("\n\n")

	rulesContent := strings.TrimSpace(input.Rules.GlobalText + "\n\n" + input.Rules.WorkspaceText)
	overridden := detectOverrides(rulesContent)
	for _, name := range overridableOrderForMode(input.Mode) {
		if !overridden[name] {
			b.WriteString(strings.TrimSpace(overridableSections[name]))
			b.WriteString("\n\n")
		}
	}

	writeAgentBody(&b, input.Instructions)

	trimmed := strings.TrimSpace(rulesContent)
	if trimmed != "" {
		b.WriteString(trimmed)
		b.WriteString("\n\n")
	}

	return strings.TrimSpace(b.String())
}

func defaultSectionsForMode(input AssemblyInput) []string {
	if input.Mode == ModeSimple {
		return []string{
			input.Identity,
			coreRulesSection,
			compactionAwarenessSection,
		}
	}
	return []string{
		input.Identity,
		coreRulesSection,
		rulesFileGuideSection,
		compactionAwarenessSection,
	}
}

func overridableOrderForMode(mode Mode) []string {
	if mode == ModeSimple {
		return []string{"safety"}
	}
	return overridableOrder
}

func writeAgentBody(b *strings.Builder, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	if !startsWithAgentPromptHeading(body) {
		b.WriteString(agentPromptHeading)
		b.WriteString("\n\n")
	}
	b.WriteString(body)
	b.WriteString("\n\n")
}

func startsWithAgentPromptHeading(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return false
		}
		headingText := strings.TrimLeft(line, "#")
		headingText = strings.TrimSpace(headingText)
		headingText = strings.TrimSpace(strings.TrimRight(headingText, "#"))
		headingText = strings.ToLower(headingText)
		headingText = strings.ReplaceAll(headingText, "-", " ")
		headingText = strings.ReplaceAll(headingText, "_", " ")
		headingText = strings.Join(strings.Fields(headingText), " ")
		return headingText == "your role and instructions"
	}
	return false
}

func renderEnvironment(input AssemblyInput) string {
	return fmt.Sprintf("## Environment\n\nWorking directory: %s\nPlatform: %s\nShell: %s\nOS: %s\nSession started: %s",
		input.Workspace,
		input.OS,
		input.Shell,
		input.OSDescription,
		input.SessionStart.Format("2006-01-02 15:04:05 MST"),
	)
}

func readRulesFile(dir string) (string, error) {
	path := filepath.Join(dir, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

func detectOverrides(rulesContent string) map[string]bool {
	result := map[string]bool{
		"safety":         false,
		"tone":           false,
		"task_execution": false,
		"language":       false,
	}
	for _, line := range strings.Split(rulesContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		headingText := strings.TrimLeft(trimmed, "#")
		headingText = strings.TrimSpace(headingText)
		headingText = strings.TrimSpace(strings.TrimRight(headingText, "#"))
		headingText = strings.ToLower(headingText)
		headingText = strings.ReplaceAll(headingText, "-", " ")
		headingText = strings.ReplaceAll(headingText, "_", " ")
		headingText = strings.Join(strings.Fields(headingText), "_")
		if _, ok := result[headingText]; ok {
			result[headingText] = true
		}
	}
	return result
}
