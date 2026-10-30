package runtime

import (
	"maps"
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// surfaceRequest is the captured pure input of one composed prompt and tool
// surface: the host-supplied identity and global rules directory, the
// preparation's Workspace and session-start environment, the selected
// definition's prompt fields, the captured Invocation, hard constraints, and
// session identity, the compiled tool specs in the agent's selection order
// after first-occurrence dedupe, the bound ModelAdaptation (nil when
// unselected), and the active model ref.
type surfaceRequest struct {
	hostIdentity   string
	globalRulesDir string
	workspace      string
	sessionStart   time.Time
	promptSize     prompt.Mode
	promptBody     string
	invocation     Invocation
	constraints    ToolConstraints
	identity       harness.SessionIdentity
	toolSpecs      []CapabilitySpec
	adaptation     ModelAdaptation
	modelRef       model.ModelRef
}

// surfaceResult is one composed prompt with its captured rule diagnostics.
type surfaceResult struct {
	Prompt   string
	Warnings []prompt.Warning
}

// composeSurface converts the captured inputs into one final owned prompt and
// advertised tool surface, with no harness or storage I/O. A non-nil
// adaptation resolves exactly once for the model ref and prompt mode; its
// error aborts preparation. The prompt reads the explicitly supplied global
// and Workspace rules, assembles the baseline through the public pure
// assembler, then applies the resolved adaptation's plain text transformation
// last: the default assembly never reinserts text after a replacement. The
// advertised surface walks the tool specs in the agent's selection order after
// first-occurrence dedupe: every selected eligible tool is advertised minus
// the excluded names — an exclusion can never revive a tool the hard
// constraints made ineligible. Every advertised description renders through
// the existing adaptation rendering helper. A zero session start samples the
// compose time once, so the resulting prompt is captured once.
func composeSurface(req surfaceRequest) (surfaceResult, []model.ToolDefinition, error) {
	var resolved Adaptation
	var conv *adaptation.Adaptation
	if req.adaptation != nil {
		value, err := req.adaptation.Resolve(req.modelRef, req.promptSize)
		if err != nil {
			return surfaceResult{}, nil, err
		}
		resolved = value
		conv = &adaptation.Adaptation{
			ExcludeTools:                slices.Clone(resolved.ExcludeTools),
			Prompt:                      cloneString(resolved.Prompt),
			Append:                      slices.Clone(resolved.Append),
			ToolDescriptionReplacements: maps.Clone(resolved.ToolDescriptionReplacements),
		}
	}

	sessionStart := req.sessionStart
	if sessionStart.IsZero() {
		sessionStart = time.Now()
	}
	rules, err := prompt.ReadRules(req.globalRulesDir, req.workspace, req.promptSize)
	if err != nil {
		return surfaceResult{}, nil, err
	}
	input := prompt.AssemblyInput{
		Identity:     req.hostIdentity,
		Instructions: req.promptBody,
		Mode:         req.promptSize,
		Rules:        rules,
		Workspace:    req.workspace,
		SessionStart: sessionStart,
	}
	if req.promptSize != prompt.ModeNone {
		input.OS = goruntime.GOOS
		input.Shell = captureShell()
		input.OSDescription = captureOSDescription()
	}
	assembled, err := prompt.Assemble(input)
	if err != nil {
		return surfaceResult{}, nil, err
	}
	result := surfaceResult{Prompt: adaptation.TransformPrompt(assembled, conv), Warnings: rules.Warnings}

	advertised := make([]model.ToolDefinition, 0, len(req.toolSpecs))
	for _, spec := range req.toolSpecs {
		description, err := spec.describe(req.invocation, req.constraints, req.identity)
		if err != nil {
			return surfaceResult{}, nil, err
		}
		if !description.Available {
			continue
		}
		name := description.Definition.Name
		if slices.Contains(resolved.ExcludeTools, name) {
			continue
		}
		definition := description.Definition
		definition.Description = adaptation.RenderToolDescription(definition.Description, conv)
		advertised = append(advertised, definition)
	}
	return result, advertised, nil
}

// captureShell samples the process shell once per composed surface.
func captureShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "unknown"
	}
	return shell
}

// captureOSDescription samples the platform description once per composed
// surface.
func captureOSDescription() string {
	if goruntime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					name := strings.TrimPrefix(line, "PRETTY_NAME=")
					return strings.Trim(name, `"`)
				}
			}
		}
	}
	return goruntime.GOOS + "/" + goruntime.GOARCH
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
