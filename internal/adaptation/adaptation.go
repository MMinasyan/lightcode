// Package adaptation defines per-model treatment. A single binding table maps
// a model id to an Adaptation that selects which tools the model is shown,
// appends ordered parts to its prompt text, and overrides model-facing tool
// description text.
//
// The binding table in this package is the only place in the codebase that
// inspects the model-id string. Consumers stay model-blind: they receive a
// resolved *Adaptation (nil = baseline) and apply their own lever.
package adaptation

import "strings"

// Adaptation bundles the levers a per-model treatment can pull. A nil
// *Adaptation means baseline (no adaptation); every field is optional, and a
// zero-value Adaptation is a valid no-op.
type Adaptation struct {
	// ExcludeTools lists registered tool names hidden from this model.
	ExcludeTools []string

	// Append lists prompt parts appended to the incoming prompt in declared
	// order, joined verbatim by one blank line. Empty and whitespace parts are
	// preserved.
	Append []string

	// ToolDescriptionReplacements rewrites the model-facing tool description
	// text: inside each line, every occurrence of each key is replaced by its
	// value, and a key naming a whole line with an empty value removes that
	// line. The rules are text-only — the same input line gets the same
	// transformation whichever tool carries it.
	ToolDescriptionReplacements map[string]string
}

// TransformPrompt appends every ordered part to text, joined verbatim by one
// blank line. The incoming text is exact, and a nil or zero-value adaptation
// returns it unchanged.
func TransformPrompt(text string, adapt *Adaptation) string {
	if adapt == nil {
		return text
	}
	parts := make([]string, 0, len(adapt.Append)+1)
	if text != "" {
		parts = append(parts, text)
	}
	parts = append(parts, adapt.Append...)
	return strings.Join(parts, "\n\n")
}
