// Package adaptation declares Lightcode's per-model adaptation plugin as one
// ordinary Harness-scoped preparation hook contribution selected through the
// ordinary Agent capability list. It retains the bundled model matching and
// applies its exclusions, full-mode prompt append and tool-description
// replacements through the same closed consequence every other preparation
// contribution uses. It has no settings and no Close.
package adaptation

import (
	"context"

	"github.com/MMinasyan/lightcode/extension"
	modeladapt "github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/prompt"
)

// CapabilityID is the plugin's one declared preparation capability.
const CapabilityID = "model_adaptation"

const configSchema = `{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}`

// Native returns the Harness-scoped native whose single contribution is the
// prepare-boundary hook. It has no dependencies and no settings.
func Native() extension.Native {
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "adaptation",
			Version:      "1",
			ConfigSchema: []byte(configSchema),
			Components: []extension.Component{{
				ID:       "adaptation",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{CapabilityID},
			}},
			Capabilities: []extension.Capability{
				extension.HookCapability(CapabilityID, extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure)),
			},
		},
		Open: open,
	}
}

// hook applies the bundled treatment for the input's model component. The
// resolved guidance append paragraphs apply only to full-mode prompts; tool
// exclusions and description replacements apply in every mode.
type hook struct{}

func (hook) Prepare(_ context.Context, _ extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
	match := modeladapt.Match(input.Model.Model)
	if match == nil {
		return extension.PreparationConsequence{}, nil
	}
	var consequence extension.PreparationConsequence
	if len(match.Append) > 0 && input.Assembly.Mode == prompt.ModeFull {
		replaced := modeladapt.TransformPrompt(input.Prompt, &modeladapt.Adaptation{Append: match.Append})
		consequence.Prompt = &replaced
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

func open(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
	if err := ctx.Err(); err != nil {
		return extension.Instance{}, err
	}
	return extension.Instance{Capabilities: map[string]extension.Implementation{
		CapabilityID: {Preparation: hook{}.Prepare},
	}}, nil
}
