package tool

import (
	"slices"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

// DefaultHidden is the optional interface a tool implements to be withheld from
// the baseline tool advertisement. A tool that reports true is shown to a model
// only when an adaptation's IncludeTools names it; tools without this method are
// always advertised (subject to an adaptation's ExcludeTools).
type DefaultHidden interface {
	DefaultHidden() bool
}

// toolDef renders a tool in the shape every OpenAI-compatible endpoint expects.
func toolDef(t Tool, adapt *adaptation.Adaptation) openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        t.Name(),
			Description: adaptation.RenderToolDescription(t.Description(), adapt),
			Parameters:  t.ParametersSchema(),
		},
	}
}

// AdvertisedTools returns the tool definitions shown to a model under adapt, in
// registration order. The set is the registered tools that are not DefaultHidden,
// minus adapt.ExcludeTools, plus adapt.IncludeTools (an include reveals a hidden
// tool; an include of an unregistered name is a no-op because only r.order is
// emitted). With adapt nil the set is every non-DefaultHidden registered tool, so
// on a registry with no DefaultHidden tool it equals OpenAITools().
func (r *Registry) AdvertisedTools(adapt *adaptation.Adaptation) []openai.Tool {
	out := make([]openai.Tool, 0, len(r.order))
	for _, name := range r.order {
		if r.Advertises(name, adapt) {
			out = append(out, toolDef(r.tools[name], adapt))
		}
	}
	return out
}

// Advertises reports whether the named tool is shown to a model under adapt. An
// unregistered name is never advertised. Any tool named in adapt.ExcludeTools is
// withheld; a DefaultHidden tool is advertised only when adapt.IncludeTools names
// it; every other registered tool is advertised.
func (r *Registry) Advertises(name string, adapt *adaptation.Adaptation) bool {
	if _, ok := r.tools[name]; !ok {
		return false
	}
	if adapt != nil && slices.Contains(adapt.ExcludeTools, name) {
		return false
	}
	if r.defaultHidden(name) {
		return adapt != nil && slices.Contains(adapt.IncludeTools, name)
	}
	return true
}

// defaultHidden reports whether the named tool opts out of the baseline
// advertisement. It unwraps permission/capability wrappers (which do not forward
// arbitrary methods) to reach the implementing tool, mirroring capabilityTool.
func (r *Registry) defaultHidden(name string) bool {
	t, ok := r.capabilityTool(name)
	if !ok {
		return false
	}
	h, ok := t.(DefaultHidden)
	return ok && h.DefaultHidden()
}
