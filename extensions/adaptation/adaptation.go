// Package adaptation owns Melta's ordinary native per-model adaptation
// extension. Ordered configured match entries precede the bundled data rows;
// the first match decides. A matched entry may replace the complete prompt,
// append ordered paragraphs, exclude currently visible tools, and rewrite
// literal model-facing tool description text through the extension's single
// preparation hook.
//
// The matcher in this package is the only place in the codebase that inspects
// the model-id string. Consumers stay model-blind: they receive a resolved
// *Adaptation (nil = baseline) and apply their own lever.
package adaptation

import "strings"

// Adaptation bundles the levers a per-model treatment can pull. A nil
// *Adaptation means baseline (no adaptation); every field is optional, and a
// zero-value Adaptation is a valid no-op.
type Adaptation struct {
	// Prompt replaces the complete incoming prompt text when present. A
	// present empty value replaces the prompt with empty text; nil leaves the
	// incoming text in place.
	Prompt *string

	// ExcludeTools lists visible tool names excluded from this model's
	// advertisement. The matched view is the incoming visible tool subset
	// minus these names.
	ExcludeTools []string

	// Append lists prompt parts appended to the incoming or explicitly
	// replaced prompt in declared order, joined verbatim by one blank line.
	// Empty and whitespace parts are preserved.
	Append []string

	// ToolDescriptionReplacements rewrites the model-facing tool description
	// text with one literal, nonrecursive replacement pass. Keys apply once,
	// longest first, and an empty value erases only the matched substring. The
	// rules are text-only — the same input text gets the same transformation
	// whichever tool carries it.
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
