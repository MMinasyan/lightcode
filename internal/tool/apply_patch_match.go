package tool

import (
	"fmt"
	"strings"
)

// locate finds a hunk's pattern in lines, scanning forward from cursor.
// The pattern is the hunk's context + remove lines in order. If the hunk
// has an @@ anchor, it is located first and the cursor is advanced past
// it; the pattern is then located starting at the advanced cursor. The
// anchor does not narrow the search region — the pattern may appear at
// any position at or after the advanced cursor.
//
// Returns the index in lines where the first line of the pattern matches.
// On failure, returns one of Codex's two error strings, with path included
// for the model-facing message:
//
//	Failed to find context '<line>' in <path>     (anchor not located)
//	Failed to find expected lines in <path>:     (pattern not located)
//
// path is used only for error messages.
//
// The four-level fuzzy ladder runs at each candidate position; the first
// level at which every line in the pattern matches wins. The levels are:
// (1) exact, (2) ignore trailing whitespace, (3) ignore leading and
// trailing whitespace, (4) Unicode-normalize typographic variants
// (dashes, fancy quotes, exotic spaces → ASCII) then trim.
func locate(lines []string, h hunk, path string, cursor int) (int, error) {
	c := cursor
	if c < 0 {
		c = 0
	}
	if c > len(lines) {
		c = len(lines)
	}

	if h.anchor != "" {
		idx, err := locateLine(lines, h.anchor, path, c)
		if err != nil {
			return 0, err
		}
		c = idx + 1
	}

	pattern := patternFromHunk(h)
	if len(pattern) == 0 {
		// Update hunks with no context or remove lines carry no pattern
		// to match; treat the advanced cursor as the start position.
		return c, nil
	}
	return locateSeq(lines, pattern, path, c)
}

func patternFromHunk(h hunk) []string {
	out := make([]string, 0, len(h.lines))
	for _, hl := range h.lines {
		if hl.kind == lineContext || hl.kind == lineRemove {
			out = append(out, hl.text)
		}
	}
	return out
}

func locateLine(lines []string, target, path string, cursor int) (int, error) {
	if target == "" {
		return cursor, nil
	}
	for i := cursor; i < len(lines); i++ {
		if matchAtLevel(lines[i], target, 1) ||
			matchAtLevel(lines[i], target, 2) ||
			matchAtLevel(lines[i], target, 3) ||
			matchAtLevel(lines[i], target, 4) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("Failed to find context '%s' in %s", target, path)
}

func locateSeq(lines []string, pattern []string, path string, cursor int) (int, error) {
	if len(pattern) == 0 {
		return cursor, nil
	}
	c := cursor
	if c < 0 {
		c = 0
	}
	if c > len(lines) {
		c = len(lines)
	}

	maxStart := len(lines) - len(pattern)
	if maxStart >= c {
		for start := c; start <= maxStart; start++ {
			if matchPatternAt(lines, pattern, start) {
				return start, nil
			}
		}
	}

	// End-of-file empty-line retry: if the pattern's last line is empty,
	// drop it and retry. This handles the common case of a file with no
	// trailing newline whose pattern's trailing context line is blank.
	if last := pattern[len(pattern)-1]; last == "" {
		trimmed := pattern[:len(pattern)-1]
		maxStartTrim := len(lines) - len(trimmed)
		if maxStartTrim >= c {
			for start := c; start <= maxStartTrim; start++ {
				if matchPatternAt(lines, trimmed, start) {
					return start, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("Failed to find expected lines in %s:\n%s", path, strings.Join(pattern, "\n"))
}

func matchPatternAt(lines, pattern []string, start int) bool {
	for level := 1; level <= 4; level++ {
		ok := true
		for j, p := range pattern {
			if !matchAtLevel(lines[start+j], p, level) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func matchAtLevel(line, target string, level int) bool {
	switch level {
	case 1:
		return line == target
	case 2:
		return strings.TrimRight(line, " \t") == strings.TrimRight(target, " \t")
	case 3:
		return strings.TrimSpace(line) == strings.TrimSpace(target)
	case 4:
		return normalizeFuzzy(line) == normalizeFuzzy(target)
	}
	return false
}

// normalizeFuzzy replaces typographic variants with their ASCII equivalents
// and trims surrounding whitespace. This is the level-4 normalization used
// by the fuzzy ladder: dashes (em-dash, en-dash, minus, horizontal bar),
// curly single/double quotes, and exotic Unicode spaces all collapse to
// their ASCII counterparts before comparison.
func normalizeFuzzy(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\u2014', '\u2013', '\u2012', '\u2010', '\u2212':
			b.WriteByte('-')
		case '\u2018', '\u2019', '\u201A', '\u201B', '\u0060', '\u00B4':
			b.WriteByte('\'')
		case '\u201C', '\u201D', '\u201E', '\u201F', '\u00AB', '\u00BB':
			b.WriteByte('"')
		case ' ', '\t', '\u00A0', '\u2000', '\u2001', '\u2002', '\u2003',
			'\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009',
			'\u200A', '\u202F', '\u205F', '\u3000':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
