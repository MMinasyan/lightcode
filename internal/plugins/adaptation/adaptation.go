// Package adaptation declares Lightcode's per-model adaptation plugin: one
// Runtime-scoped ModelAdaptation export resolved from the bundled binding
// table in internal/adaptation. It is an ordinary composition unit selected
// through static registration — the same Plugin/Instance contract an
// externally supplied capability implements, with no additional Runtime
// authority, no settings, and no Close.
package adaptation

import (
	"context"

	modeladapt "github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const exportID = "model_adaptation"

// Plugin returns the Runtime-scoped plugin whose single export is declared
// exactly as the public ModelAdaptation contract. It has no dependencies and
// no settings validator.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    "adaptation",
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.Spec[runtime.ModelAdaptation](exportID),
		},
		Open: open,
	}
}

// resolver adapts the bundled binding table to the public contract. Match
// receives the model component of the ref alone and returns an independent
// copy; nil is the baseline.
type resolver struct{}

func (resolver) Resolve(ref model.ModelRef) (runtime.Adaptation, error) {
	match := modeladapt.Match(ref.Model)
	if match == nil {
		return runtime.Adaptation{}, nil
	}
	return runtime.Adaptation{
		ExcludeTools:                match.ExcludeTools,
		IncludeTools:                match.IncludeTools,
		Blocks:                      match.Blocks,
		Additions:                   match.Additions,
		ToolDescriptionReplacements: match.ToolDescriptionReplacements,
	}, nil
}

func open(ctx context.Context, _ runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	return runtime.Instance{Values: map[string]any{exportID: resolver{}}}, nil
}
