// Package adaptation defines per-model treatment. A single binding table maps
// a model id to an Adaptation that selects which tools the model is shown,
// adds coaching blocks to its system prompt, and carries the pattern that
// recognizes tool calls the provider failed to parse out of the model's text.
//
// The binding table in this package is the only place in the codebase that
// inspects the model-id string. Consumers stay model-blind: they receive a
// resolved *Adaptation (nil = baseline) and apply their own lever. The table
// ships empty, so every model resolves to baseline until per-model content is
// added in a later phase.
package adaptation

import "regexp"

// Adaptation bundles the levers a per-model treatment can pull. A nil
// *Adaptation means baseline (no adaptation); every field is optional, and a
// zero-value Adaptation is a valid no-op.
type Adaptation struct {
	// Name identifies the adaptation. It participates in the prompt cache key,
	// so a switch between adaptations (or to/from baseline) forces a rebuild.
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
}
