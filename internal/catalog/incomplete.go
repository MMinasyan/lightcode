package catalog

// IncompleteReason explains why a model cannot be used yet.
type IncompleteReason int

const (
	// ReasonMissingContextWindow means context_window is unset.
	ReasonMissingContextWindow IncompleteReason = iota
)

// Incomplete describes a model entry missing required capacity metadata.
type Incomplete struct {
	Ref     ModelRef
	Reasons []IncompleteReason
}

// Incomplete returns capacity fields missing from the model. Only
// context_window is required; max_output_tokens is optional and only acts
// as a request cap when set, so missing it does not block use.
func (m *Model) Incomplete() (Incomplete, bool) {
	if m == nil {
		return Incomplete{}, false
	}
	var reasons []IncompleteReason
	if m.ContextWindow == 0 {
		reasons = append(reasons, ReasonMissingContextWindow)
	}
	if len(reasons) == 0 {
		return Incomplete{}, false
	}
	return Incomplete{Ref: ModelRef{Model: m.ID}, Reasons: reasons}, true
}

// IncompleteModels returns all incomplete models in the catalog.
func (c *Catalog) IncompleteModels() []Incomplete {
	var incomplete []Incomplete
	for _, ref := range c.AllModels() {
		_, model, err := c.LookupOrIncomplete(ref)
		if err != nil {
			continue
		}
		entry, ok := model.Incomplete()
		if !ok {
			continue
		}
		entry.Ref = ref
		incomplete = append(incomplete, entry)
	}
	return incomplete
}
