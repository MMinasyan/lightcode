package prompt

import (
	"crypto/sha256"
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

//go:embed memory_instructions.md
var memoryInstructionsSection string

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
	WarnRulesTooLarge        = "rules_too_large"
	WarnRulesNotFound        = "rules_not_found"
	WarnRulesReadError       = "rules_read_error"
	WarnLSPInstallFailed     = "lsp_install_failed"
	WarnLSPServerUnavailable = "lsp_server_unavailable"
)

type Warning struct {
	Kind    string
	Message string
}

type Result struct {
	Prompt   string
	Rebuilt  bool
	Warnings []Warning
}

type Assembler struct {
	projectRoot  string
	home         string
	sessionStart time.Time

	cachedPrompt    string
	cachedRulesHash [32]byte
	cachedAdaptName string
}

func New(projectRoot, home string) *Assembler {
	return &Assembler{
		projectRoot:  projectRoot,
		home:         home,
		sessionStart: time.Now(),
	}
}

// Assemble builds the baseline system prompt (no adaptation).
func (a *Assembler) Assemble() Result { return a.AssembleFor(nil) }

// AssembleFor builds the system prompt for adapt (nil = baseline). The rules
// hash is computed exactly as for the baseline; the adaptation name is a
// companion cache key, so a cache hit requires both the rules and the name to be
// unchanged. An empty name (baseline) reproduces the baseline hit/miss cadence;
// a name change forces a rebuild.
func (a *Assembler) AssembleFor(adapt *adaptation.Adaptation) Result {
	var warnings []Warning

	globalContent, err := readRulesFile(filepath.Join(a.home, ".lightcode"))
	if err != nil {
		warnings = append(warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read global rules file: " + err.Error()})
		globalContent = ""
	}

	projectContent, err := readRulesFile(a.projectRoot)
	if err != nil {
		warnings = append(warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read project rules file: " + err.Error()})
		projectContent = ""
	}

	if projectContent == "" && globalContent == "" {
		warnings = append(warnings, Warning{Kind: WarnRulesNotFound, Message: "No AGENTS.md found"})
	}

	combined := globalContent + "\x00" + projectContent

	if len(combined) > 20000 {
		warnings = append(warnings, Warning{Kind: WarnRulesTooLarge, Message: fmt.Sprintf("Rules file exceeds 20,000 characters (%d chars). Consider trimming it.", len(combined))})
	}

	h := sha256.Sum256([]byte(combined))
	adaptName := ""
	if adapt != nil {
		adaptName = adapt.Name
	}
	// Cache key is (rules hash, adaptation name). defaultAdditions is a
	// compile-time constant, so the name is sufficient; a Name:"" adaptation
	// carrying additions would be mis-cached against baseline, but matched
	// table entries always have names and Blocks shares the same hazard.
	if a.cachedPrompt != "" && h == a.cachedRulesHash && adaptName == a.cachedAdaptName {
		return Result{Prompt: a.cachedPrompt, Rebuilt: false, Warnings: warnings}
	}

	prompt := a.build(globalContent, projectContent, adapt)

	a.cachedPrompt = prompt
	a.cachedRulesHash = h
	a.cachedAdaptName = adaptName
	return Result{Prompt: prompt, Rebuilt: true, Warnings: warnings}
}

func (a *Assembler) build(globalRules, projectRules string, adapt *adaptation.Adaptation) string {
	var b strings.Builder

	for _, s := range []string{
		identitySection,
		coreRulesSection,
		rulesFileGuideSection,
		compactionAwarenessSection,
	} {
		b.WriteString(strings.TrimSpace(s))
		b.WriteString("\n\n")
	}

	b.WriteString(renderEnvironment(a.projectRoot, a.sessionStart))
	b.WriteString("\n\n")

	b.WriteString(strings.TrimSpace(memoryInstructionsSection))
	b.WriteString("\n\n")

	rulesContent := strings.TrimSpace(globalRules + "\n\n" + projectRules)
	overridden := detectOverrides(rulesContent)
	for _, name := range overridableOrder {
		if !overridden[name] {
			b.WriteString(strings.TrimSpace(overridableSections[name]))
			b.WriteString("\n\n")
		}
		// System territory: renders even when a user heading overrides the main
		// (D5: additions are system-owned). Empty defaults/adaptations write
		// nothing, so the baseline prompt is unchanged.
		if add := adaptation.SectionAddition(adapt, name); add != "" {
			b.WriteString(strings.TrimSpace(add))
			b.WriteString("\n\n")
		}
	}

	// Adaptation coaching blocks sit after the built-in sections and before user
	// rules. Baseline (nil/empty) inserts nothing, so the prompt is unchanged.
	if adapt != nil && len(adapt.Blocks) > 0 {
		for _, block := range adapt.Blocks {
			b.WriteString(strings.TrimSpace(block))
			b.WriteString("\n\n")
		}
	}

	trimmed := strings.TrimSpace(rulesContent)
	if trimmed != "" {
		b.WriteString(trimmed)
		b.WriteString("\n\n")
	}

	return strings.TrimSpace(b.String())
}

func renderEnvironment(projectRoot string, sessionStart time.Time) string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "unknown"
	}
	return fmt.Sprintf("Working directory: %s\nPlatform: %s\nShell: %s\nOS: %s\nSession started: %s",
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
		headingText = strings.ToLower(headingText)
		if _, ok := result[headingText]; ok {
			result[headingText] = true
		}
	}
	return result
}
