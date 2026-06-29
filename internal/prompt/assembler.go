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
	Rebuilt  bool
	Warnings []Warning
}

type Spec struct {
	Size   string
	Body   string
	Memory bool
	Adapt  *adaptation.Adaptation
}

type Assembler struct {
	projectRoot  string
	home         string
	sessionStart time.Time

	cachedPrompt    string
	cachedRulesHash [32]byte
	cachedAdaptName string
	cachedSize      string
	cachedBody      string
	cachedMemory    bool
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

// AssembleFor builds the full system prompt for adapt (nil = baseline). The
// rules hash is computed exactly as for the baseline; the adaptation name is a
// companion cache key, so a cache hit requires both the rules and the name to
// be unchanged. An empty name (baseline) reproduces the baseline hit/miss
// cadence; a name change forces a rebuild.
func (a *Assembler) AssembleFor(adapt *adaptation.Adaptation) Result {
	return a.AssembleForSpec(Spec{Size: SizeFull, Memory: true, Adapt: adapt})
}

func (a *Assembler) AssembleForSpec(spec Spec) Result {
	spec = normalizeSpec(spec)
	var warnings []Warning

	var globalContent, projectContent string
	if spec.Size != SizeNone {
		var err error
		globalContent, err = readRulesFile(filepath.Join(a.home, ".lightcode"))
		if err != nil {
			warnings = append(warnings, Warning{Kind: WarnRulesReadError, Message: "Failed to read global rules file: " + err.Error()})
			globalContent = ""
		}

		projectContent, err = readRulesFile(a.projectRoot)
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

	h := sha256.Sum256([]byte(combined))
	adaptName := ""
	if spec.Adapt != nil && spec.Size != SizeNone {
		adaptName = spec.Adapt.Name
	}
	// Cache key is the prompt spec plus (rules hash, adaptation name).
	// defaultAdditions is a compile-time constant, so the name is sufficient;
	// a Name:"" adaptation carrying additions would be mis-cached against
	// baseline, but matched table entries always have names and Blocks shares
	// the same hazard.
	if a.cachedPrompt != "" &&
		h == a.cachedRulesHash &&
		adaptName == a.cachedAdaptName &&
		spec.Size == a.cachedSize &&
		spec.Body == a.cachedBody &&
		spec.Memory == a.cachedMemory {
		return Result{Prompt: a.cachedPrompt, Rebuilt: false, Warnings: warnings}
	}

	prompt := a.buildSpec(globalContent, projectContent, spec)

	a.cachedPrompt = prompt
	a.cachedRulesHash = h
	a.cachedAdaptName = adaptName
	a.cachedSize = spec.Size
	a.cachedBody = spec.Body
	a.cachedMemory = spec.Memory
	return Result{Prompt: prompt, Rebuilt: true, Warnings: warnings}
}

func (a *Assembler) build(globalRules, projectRules string, adapt *adaptation.Adaptation) string {
	return a.buildSpec(globalRules, projectRules, Spec{Size: SizeFull, Memory: true, Adapt: adapt})
}

func (a *Assembler) buildSpec(globalRules, projectRules string, spec Spec) string {
	spec = normalizeSpec(spec)
	var b strings.Builder

	if spec.Size == SizeNone {
		if spec.Memory {
			b.WriteString(strings.TrimSpace(memoryInstructionsSection))
			b.WriteString("\n\n")
		}
		writeAgentBody(&b, spec.Body)
		return strings.TrimSpace(b.String())
	}

	for _, s := range defaultSectionsForSize(spec.Size) {
		b.WriteString(strings.TrimSpace(s))
		b.WriteString("\n\n")
	}

	b.WriteString(renderEnvironment(a.projectRoot, a.sessionStart))
	b.WriteString("\n\n")

	if spec.Memory {
		b.WriteString(strings.TrimSpace(memoryInstructionsSection))
		b.WriteString("\n\n")
	}

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
