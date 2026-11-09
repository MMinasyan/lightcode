package adaptation

import (
	"slices"
	"strings"
)

// entry binds a glob pattern over the bare model id to an adaptation. bundled
// marks the extension-owned rows whose append paragraphs apply only to
// full-mode prompts; configured user entries apply their explicit levers in
// every mode.
type entry struct {
	pattern string
	adapt   Adaptation
	bundled bool
}

// newToolDescriptionReplacer builds the one literal replacement pass for a
// matched rule's description map. Keys apply in one pass ordered by descending
// byte length then lexical order, so the longest key wins at each position and
// replacement text is never processed again. An empty or nil map yields the
// stdlib zero-pair identity replacer.
func newToolDescriptionReplacer(replacements map[string]string) *strings.Replacer {
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		pairs = append(pairs, key, replacements[key])
	}
	return strings.NewReplacer(pairs...)
}

// matchEntryIn returns the first entry in t whose pattern matches modelID, or
// nil. Matching is case-insensitive and walks t in order, so the table's order
// decides ties (first match wins). It is the one matcher used by the bundled
// data oracles and the native extension's user-before-bundled table.
func matchEntryIn(t []entry, modelID string) *entry {
	id := strings.ToLower(modelID)
	for i := range t {
		if globMatch(strings.ToLower(t[i].pattern), id) {
			return &t[i]
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
