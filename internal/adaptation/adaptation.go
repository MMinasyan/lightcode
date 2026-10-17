// Package adaptation defines per-model treatment. A single binding table maps
// a model id to an Adaptation that selects which tools the model is shown,
// adds coaching blocks and per-section additions to its system prompt, and
// overrides model-facing tool description text.
//
// The binding table in this package is the only place in the codebase that
// inspects the model-id string. Consumers stay model-blind: they receive a
// resolved *Adaptation (nil = baseline) and apply their own lever.
package adaptation

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

	// Blocks are coaching paragraphs appended to the system prompt after the
	// built-in sections and before user rules.
	Blocks []string

	// Additions are system-owned per-section prompt additions keyed by the
	// user-overridable section ids ("safety", "tone", "task_execution",
	// "language"). An addition for a section renders after the section main,
	// even when a user rules heading overrides that section. Nil or missing
	// entries mean no addition for that section.
	Additions map[string]string

	// ToolDescriptionReplacements overrides default placeholder replacements
	// applied to model-facing tool descriptions.
	ToolDescriptionReplacements map[string]string
}
