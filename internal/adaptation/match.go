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

// Match returns the adaptation bound to modelID, or nil for baseline. It is the
// production Resolver and expects the model component of the active model ref;
// aggregator model IDs may still contain slashes. The table is loaded from
// the bundled data file in loader.go; the matcher itself is generic.
func Match(modelID string) *Adaptation {
	return matchIn(bundledTable, modelID)
}

// RenderToolDescription applies the active adaptation's literal replacement
// overrides to a model-facing tool description; a nil adaptation returns the
// description unchanged. A line that consists only of a replaced value is
// removed when that value resolves to an empty string.
func RenderToolDescription(description string, adapt *Adaptation) string {
	if adapt == nil || len(adapt.ToolDescriptionReplacements) == 0 {
		return description
	}
	replacements := adapt.ToolDescriptionReplacements

	lines := strings.Split(description, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if replacement, ok := replacements[trimmed]; ok && replacement == "" {
			lines = append(lines[:i], lines[i+1:]...)
			i--
			continue
		}
		for key, replacement := range replacements {
			line = strings.ReplaceAll(line, key, replacement)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// matchIn returns an independent copy of the first entry in t whose pattern
// matches modelID, or nil. Matching is case-insensitive and walks t in order, so
// the table's most-specific-first ordering decides ties (first match wins). The
// result aliases nothing mutable in t — its slice fields are cloned — so a caller
// can never corrupt the table through the resolved adaptation.
func matchIn(t []entry, modelID string) *Adaptation {
	id := strings.ToLower(modelID)
	for i := range t {
		if globMatch(strings.ToLower(t[i].pattern), id) {
			a := t[i].adapt
			a.ExcludeTools = slices.Clone(a.ExcludeTools)
			a.Append = slices.Clone(a.Append)
			a.ToolDescriptionReplacements = maps.Clone(a.ToolDescriptionReplacements)
			return &a
		}
	}
	return nil
}

// globMatch reports whether glob matches the whole string s. '*' matches any
// run of characters, including '/' and the empty string; every other byte is
// literal. The match is anchored at both ends, so "model-5*" matches
// "model-5.4" but not "provider/model-5.4"; a leading wildcard such as
// "*-5*" does match across '/'.
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
