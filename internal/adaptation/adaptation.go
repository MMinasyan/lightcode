// Package adaptation defines per-model treatment. A single binding table maps
// a model id to an Adaptation that selects which tools the model is shown,
// adds coaching blocks to its system prompt, and carries the pattern that
// recognizes tool calls the provider failed to parse out of the model's text.
//
// The binding table in this package is the only place in the codebase that
// inspects the model-id string. Consumers stay model-blind: they receive a
// resolved *Adaptation (nil = baseline) and apply their own lever.
package adaptation

import "regexp"

// Adaptation bundles the levers a per-model treatment can pull. A nil
// *Adaptation means baseline (no adaptation); every field is optional, and a
// zero-value Adaptation is a valid no-op.
type Adaptation struct {
	// Name identifies the adaptation for the binding table. It does not affect
	// assembly: prompt bytes change only through Blocks and Additions, so a
	// name-only switch produces an identical prompt that the unit's
	// last-installed-prompt comparison leaves installed.
	Name string

	// ExcludeTools lists registered tool names hidden from this model.
	ExcludeTools []string

	// IncludeTools lists registered tool names surfaced to this model,
	// including tools hidden from the baseline advertisement. An include of a
	// name that is not registered is a silent no-op.
	IncludeTools []string

	// LeakPattern, when set, matches tool-call-shaped text the provider failed
	// to parse into a structured call. A match drives leak recovery.
	LeakPattern *regexp.Regexp

	// Blocks are coaching paragraphs appended to the system prompt after the
	// built-in sections and before user rules.
	Blocks []string

	// Additions are system-owned per-section prompt additions keyed by the
	// user-overridable section ids ("safety", "tone", "task_execution",
	// "language"). An addition for a section renders after the section main,
	// even when a user rules heading overrides that section. Nil or missing
	// entries mean "fall back to the default addition for this section".
	Additions map[string]string

	// ToolDescriptionReplacements overrides default placeholder replacements
	// applied to model-facing tool descriptions.
	ToolDescriptionReplacements map[string]string
}
