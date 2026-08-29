package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

//go:embed identity.md
var identitySection string

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

const (
	SizeNone   = "none"
	SizeSimple = "simple"
	SizeFull   = "full"

	WarnRulesTooLarge        = "rules_too_large"
	WarnRulesNotFound        = "rules_not_found"
	WarnRulesReadError       = "rules_read_error"
	WarnLSPInstallFailed     = "lsp_install_failed"
	WarnLSPServerUnavailable = "lsp_server_unavailable"
)

const agentPromptHeading = "## Your Role and Instructions"

type Warning struct {
	Kind    string
	Message string
}

type Result struct {
	Prompt   string
	Warnings []Warning
}

type Spec struct {
	Size  string
	Body  string
	Adapt *adaptation.Adaptation
}

// Service assembles system prompts. It is stateless: it owns no cache and no
// per-session state, so one instance is shared across every unit. Simple/full
// calls read current global and project rules; none skips rules reads. Every
// call renders the caller's project root and session start, returning one
// immutable prompt. Callers compare it with their own last installed prompt.
type Service struct {
	home string
}

func NewService(home string) *Service {
	return &Service{home: home}
}

// Assemble builds the system prompt for one unit: its project root, its fixed
// session start, and the given spec. For the simple and full sizes it reads
// the current global and project rules fresh on every call (none skips rules
// reads) and holds nothing between calls.
func (s *Service) Assemble(projectRoot string, sessionStart time.Time, spec Spec) Result {
	spec = normalizeSpec(spec)
	var warnings []Warning

	var globalContent, projectContent string
	if spec.Size != SizeNone {
		var err error
		globalContent, err = readRulesFile(filepath.Join(s.home, ".lightcode"))
		if err != nil {
			warnings = append(warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read global rules file: " + err.Error()})
			globalContent = ""
		}

		projectContent, err = readRulesFile(projectRoot)
		if err != nil {
			warnings = append(warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read project rules file: " + err.Error()})
			projectContent = ""
		}

		if projectContent == "" && globalContent == "" {
			warnings = append(warnings, Warning{Kind: WarnRulesNotFound, Message: "No AGENTS.md found"})
		}
	}

	combined := globalContent + "\x00" + projectContent

	if spec.Size != SizeNone && len(combined) > 20000 {
		warnings = append(warnings, Warning{Kind: WarnRulesTooLarge, Message: fmt.Sprintf("Rules file exceeds 20,000 characters (%d chars). Consider trimming it.", len(combined))})
	}

	return Result{
		Prompt:   buildSpec(projectRoot, sessionStart, globalContent, projectContent, spec),
		Warnings: warnings,
	}
}

func buildSpec(projectRoot string, sessionStart time.Time, globalRules, projectRules string, spec Spec) string {
	spec = normalizeSpec(spec)
	var b strings.Builder

	if spec.Size == SizeNone {
		writeAgentBody(&b, spec.Body)
		return strings.TrimSpace(b.String())
	}

	for _, s := range defaultSectionsForSize(spec.Size) {
		b.WriteString(strings.TrimSpace(s))
		b.WriteString("\n\n")
	}

	b.WriteString(renderEnvironment(projectRoot, sessionStart))
	b.WriteString("\n\n")

	rulesContent := strings.TrimSpace(globalRules + "\n\n" + projectRules)
	overridden := detectOverrides(rulesContent)
	for _, name := range overridableOrderForSize(spec.Size) {
		if !overridden[name] {
			b.WriteString(strings.TrimSpace(overridableSections[name]))
			b.WriteString("\n\n")
		}
		// System territory: renders even when a user heading overrides the main
		// (D5: additions are system-owned). Empty defaults/adaptations write
		// nothing, so the baseline prompt is unchanged.
		if add := adaptation.SectionAddition(spec.Adapt, name); add != "" {
			b.WriteString(strings.TrimSpace(add))
			b.WriteString("\n\n")
		}
	}

	// Adaptation coaching blocks sit after the built-in sections and before user
	// rules. Baseline (nil/empty) inserts nothing, so the prompt is unchanged.
	if spec.Adapt != nil && len(spec.Adapt.Blocks) > 0 {
		for _, block := range spec.Adapt.Blocks {
			b.WriteString(strings.TrimSpace(block))
			b.WriteString("\n\n")
		}
	}

	writeAgentBody(&b, spec.Body)

	trimmed := strings.TrimSpace(rulesContent)
	if trimmed != "" {
		b.WriteString(trimmed)
		b.WriteString("\n\n")
	}

	return strings.TrimSpace(b.String())
}

func normalizeSpec(spec Spec) Spec {
	switch spec.Size {
	case SizeNone, SizeSimple, SizeFull:
	default:
		spec.Size = SizeFull
	}
	spec.Body = strings.TrimSpace(spec.Body)
	if spec.Size == SizeNone {
		spec.Adapt = nil
	}
	return spec
}

func defaultSectionsForSize(size string) []string {
	if size == SizeSimple {
		return []string{
			identitySection,
			coreRulesSection,
			compactionAwarenessSection,
		}
	}
	return []string{
		identitySection,
		coreRulesSection,
		rulesFileGuideSection,
		compactionAwarenessSection,
	}
}

func overridableOrderForSize(size string) []string {
	if size == SizeSimple {
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

func renderEnvironment(projectRoot string, sessionStart time.Time) string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "unknown"
	}
	return fmt.Sprintf("## Environment\n\nWorking directory: %s\nPlatform: %s\nShell: %s\nOS: %s\nSession started: %s",
		projectRoot,
		runtime.GOOS,
		shell,
		osDescription(),
		sessionStart.Format("2006-01-02 15:04:05 MST"),
	)
}

func osDescription() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					name := strings.TrimPrefix(line, "PRETTY_NAME=")
					return strings.Trim(name, `"`)
				}
			}
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

func readRulesFile(dir string) (string, error) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
	}
	return "", nil
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
