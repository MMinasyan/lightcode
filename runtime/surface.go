package runtime

import (
	"maps"
	"slices"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/model"
)

// surfaceRequest is the captured pure input of one composed prompt and tool
// surface: the Runtime's once-resolved home, the preparation's Workspace and
// session-start environment, the selected definition's prompt fields, the
// captured Invocation and hard constraints, the composition's tool specs in
// the agent's selection order after first-occurrence dedupe, the bound
// ModelAdaptation (nil when unselected), and the active model ref.
type surfaceRequest struct {
	home         string
	workspace    string
	sessionStart time.Time
	promptSize   string
	promptBody   string
	invocation   Invocation
	constraints  ToolConstraints
	toolSpecs    []CapabilitySpec
	adaptation   ModelAdaptation
	modelRef     model.ModelRef
}

// composeSurface converts the captured inputs into one final owned prompt and
// advertised tool surface, with no harness or storage I/O. A non-nil
// adaptation resolves exactly once for the model ref; its error aborts
// preparation. The public value converts once into internal/prompt's existing
// pure input (containers copied, no second matcher), the prompt assembles
// through the shared Service with every retained invariant (none skips rules
// and adaptation; simple and full share the one assembly), and the advertised
// surface walks the tool specs in the agent's selection order after
// first-occurrence dedupe: eligible tools minus the
// excluded names (exclusion always wins over inclusion), minus default-hidden
// tools unless included — an include can never revive a tool the hard
// constraints made ineligible. Every advertised description renders through
// the existing adaptation rendering helper. A zero session start samples the
// compose time once, so the resulting prompt is captured once.
func composeSurface(req surfaceRequest) (prompt.Result, []model.ToolDefinition, error) {
	var resolved Adaptation
	var conv *adaptation.Adaptation
	if req.adaptation != nil {
		value, err := req.adaptation.Resolve(req.modelRef)
		if err != nil {
			return prompt.Result{}, nil, err
		}
		resolved = value
		conv = &adaptation.Adaptation{
			ExcludeTools:                slices.Clone(resolved.ExcludeTools),
			IncludeTools:                slices.Clone(resolved.IncludeTools),
			Blocks:                      slices.Clone(resolved.Blocks),
			Additions:                   maps.Clone(resolved.Additions),
			ToolDescriptionReplacements: maps.Clone(resolved.ToolDescriptionReplacements),
		}
	}

	sessionStart := req.sessionStart
	if sessionStart.IsZero() {
		sessionStart = time.Now()
	}
	result := prompt.NewService(req.home).Assemble(req.workspace, sessionStart, prompt.Spec{Size: req.promptSize, Body: req.promptBody, Adapt: conv})

	advertised := make([]model.ToolDefinition, 0, len(req.toolSpecs))
	for _, spec := range req.toolSpecs {
		description, err := spec.describe(req.invocation, req.constraints)
		if err != nil {
			return prompt.Result{}, nil, err
		}
		if !description.Available {
			continue
		}
		name := description.Definition.Name
		if slices.Contains(resolved.ExcludeTools, name) || (description.DefaultHidden && !slices.Contains(resolved.IncludeTools, name)) {
			continue
		}
		definition := description.Definition
		definition.Description = adaptation.RenderToolDescription(definition.Description, conv)
		advertised = append(advertised, definition)
	}
	return result, advertised, nil
}
