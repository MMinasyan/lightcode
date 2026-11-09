package melta

import (
	"context"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// surfaceRequest is the captured pure input of one composed prompt and tool
// surface: the host-supplied identity and global rules directory, the
// preparation's Workspace and session-start environment, the selected
// definition's prompt fields, the captured Invocation, hard constraints, and
// session identity, and the compiled tool specs in the agent's selection
// order after first-occurrence dedupe.
type surfaceRequest struct {
	hostIdentity   string
	globalRulesDir string
	workspace      string
	sessionStart   time.Time
	promptSize     prompt.Mode
	promptBody     string
	invocation     Invocation
	model          model.ModelRef
	constraints    harness.ToolConstraints
	identity       harness.SessionIdentity
	agentInputs    harness.AgentInputs
	toolSpecs      []toolDeclaration
	shell          string
}

// surfaceResult is one composed prompt with its captured rule diagnostics and
// the exact baseline assembly inputs the prompt was rendered from.
type surfaceResult struct {
	Prompt   string
	Assembly prompt.AssemblyInput
	Warnings []prompt.Warning
}

// composeSurface converts the captured inputs into one final owned prompt and
// the eligible tool catalog, with no harness or storage I/O. The prompt reads
// the explicitly supplied global and Workspace rules and assembles the
// baseline through the public pure assembler. The catalog walks the tool specs
// in the agent's selection order after first-occurrence dedupe: every selected
// eligible tool is included — an exclusion can never revive a tool the hard
// constraints made ineligible. A zero session start samples the compose time
// once, so the resulting prompt is captured once.
func composeSurface(ctx context.Context, req surfaceRequest) (surfaceResult, []model.ToolDefinition, error) {
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
		input.Shell = req.shell
		input.OSDescription = captureOSDescription()
	}
	assembled, err := prompt.Assemble(input)
	if err != nil {
		return surfaceResult{}, nil, err
	}
	result := surfaceResult{Prompt: assembled, Assembly: input, Warnings: rules.Warnings}

	catalog := make([]model.ToolDefinition, 0, len(req.toolSpecs))
	for _, spec := range req.toolSpecs {
		description, err := spec.describe(ctx, req.invocation, req.model, req.constraints, req.identity, req.agentInputs)
		if err != nil {
			return surfaceResult{}, nil, err
		}
		if !description.Available {
			continue
		}
		catalog = append(catalog, description.Definition)
	}
	return result, catalog, nil
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
