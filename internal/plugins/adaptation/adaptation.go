// Package adaptation declares Lightcode's per-model adaptation plugin as one
// ordinary Runtime-scoped PreparationHook contribution selected through static
// registration. It retains the bundled model matching and applies its
// exclusions, full-mode prompt append and tool-description replacements
// through the same closed consequence every other preparation contribution
// uses. It has no settings and no Close.
package adaptation

import (
	"context"

	modeladapt "github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/runtime"
)

// CapabilityID is the plugin's one declared preparation capability.
const CapabilityID = "model_adaptation"

// Plugin returns the Runtime-scoped plugin whose single export is declared as
// the public PreparationHook contract. It has no dependencies and no settings
// validator.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    "adaptation",
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.Spec[runtime.PreparationHook](CapabilityID),
		},
		Open: open,
	}
}

// hook applies the bundled treatment for the input's model component. The
// resolved guidance append paragraphs apply only to full-mode prompts; tool
// exclusions and description replacements apply in every mode.
type hook struct{}

func (hook) Prepare(_ context.Context, input runtime.PreparationInput) (runtime.PreparationConsequence, error) {
	match := modeladapt.Match(input.Model.Model)
	if match == nil {
		return runtime.PreparationConsequence{}, nil
	}
	var consequence runtime.PreparationConsequence
	if len(match.Append) > 0 && input.Assembly.Mode == prompt.ModeFull {
		prompt := modeladapt.TransformPrompt(input.Prompt, &modeladapt.Adaptation{Append: match.Append})
		consequence.Prompt = &prompt
	}
	if len(match.ExcludeTools) > 0 {
		excluded := make(map[string]bool, len(match.ExcludeTools))
		for _, name := range match.ExcludeTools {
			excluded[name] = true
		}
		visible := make([]string, 0, len(input.Catalog))
		for _, definition := range input.Catalog {
			if !excluded[definition.Name] {
				visible = append(visible, definition.Name)
			}
		}
		consequence.ToolNames = visible
	}
	if len(match.ToolDescriptionReplacements) > 0 {
		replacements := &modeladapt.Adaptation{ToolDescriptionReplacements: match.ToolDescriptionReplacements}
		descriptions := make(map[string]string)
		for _, definition := range input.Catalog {
			rendered := modeladapt.RenderToolDescription(definition.Description, replacements)
			if rendered != definition.Description {
				descriptions[definition.Name] = rendered
			}
		}
		if len(descriptions) > 0 {
			consequence.Descriptions = descriptions
		}
	}
	return consequence, nil
}

func open(ctx context.Context, _ runtime.ScopeInfo) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	return runtime.Instance{Values: map[string]any{CapabilityID: hook{}}}, nil
}
