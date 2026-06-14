package adaptation

import (
	"maps"
	"slices"
	"strings"
)

// Resolver maps a bare model id to its adaptation, returning nil for baseline.
// Match is the production Resolver; tests inject their own closure.
type Resolver func(modelID string) *Adaptation

// entry binds a glob pattern over the bare model id to an adaptation.
type entry struct {
	pattern string
	adapt   Adaptation
}

// table is the production binding table, ordered most-specific-first. It ships
// empty: every model resolves to baseline until per-model content is added in a
// later phase.
var table []entry

// defaultAdditions is the system-owned fallback for per-section additions.
// It ships empty so the assembled prompt is byte-identical to a build without
// this subsystem. Real content is added later, one section at a time.
var defaultAdditions map[string]string

// Match returns the adaptation bound to modelID, or nil for baseline. It is the
// production Resolver and inspects only the bare model id (never the
// provider-prefixed form).
func Match(modelID string) *Adaptation {
	return matchIn(table, modelID)
}

// SectionAddition returns the addition for section from the active adaptation
// if it exists and is non-empty, otherwise the default addition for that
// section (which may be empty). section is one of the four user-overridable
// section ids: "safety", "tone", "task_execution", "language".
func SectionAddition(adapt *Adaptation, section string) string {
	if adapt != nil && adapt.Additions != nil {
		if v := adapt.Additions[section]; v != "" {
			return v
		}
	}
	return defaultAdditions[section]
}

// matchIn returns an independent copy of the first entry in t whose pattern
// matches modelID, or nil. Matching is case-insensitive and walks t in order, so
// the table's most-specific-first ordering decides ties (first match wins). The
// result aliases nothing mutable in t — its slice fields are cloned — so a caller
// can never corrupt the table through the resolved adaptation. (The LeakPattern
// regexp is shared intentionally: it is read-only and safe for concurrent use.)
func matchIn(t []entry, modelID string) *Adaptation {
	id := strings.ToLower(modelID)
	for i := range t {
		if globMatch(strings.ToLower(t[i].pattern), id) {
			a := t[i].adapt
			a.ExcludeTools = slices.Clone(a.ExcludeTools)
			a.IncludeTools = slices.Clone(a.IncludeTools)
			a.Blocks = slices.Clone(a.Blocks)
			a.Additions = maps.Clone(a.Additions)
			return &a
		}
	}
	return nil
}

// globMatch reports whether glob matches the whole string s. '*' matches any
// run of characters, including '/' and the empty string; every other byte is
// literal. The match is anchored at both ends, so "gpt-5*" matches "gpt-5.4"
// but not "openai/gpt-5.4".
func globMatch(glob, s string) bool {
	// Linear scan with backtracking to the most recent '*'.
	var gi, si int
	star, starMatch := -1, 0
	for si < len(s) {
		switch {
		case gi < len(glob) && glob[gi] == s[si]:
			gi++
			si++
		case gi < len(glob) && glob[gi] == '*':
			star, starMatch = gi, si
			gi++
		case star != -1:
			gi = star + 1
			starMatch++
			si = starMatch
		default:
			return false
		}
	}
	for gi < len(glob) && glob[gi] == '*' {
		gi++
	}
	return gi == len(glob)
}
