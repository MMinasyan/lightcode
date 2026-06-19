package tool

import (
	"strings"
	"testing"
)

func TestLocateExactMatch(t *testing.T) {
	lines := []string{"foo", "bar", "baz"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "bar"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 1 {
		t.Fatalf("start = %d, want 1", start)
	}
}

func TestLocateForwardScan(t *testing.T) {
	lines := []string{"foo", "bar", "foo", "bar"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}, {kind: lineContext, text: "bar"}}}

	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0 (first hit)", start)
	}

	start, err = locate(lines, h, "p", 2)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 2 {
		t.Fatalf("start = %d, want 2 (hit at/after cursor=2)", start)
	}
}

func TestLocateLevel2TrailingWhitespace(t *testing.T) {
	lines := []string{"foo   ", "bar", "baz"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateLevel3LeadingAndTrailingWhitespace(t *testing.T) {
	lines := []string{"  foo  ", "bar", "baz"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateLevel4UnicodeDashes(t *testing.T) {
	lines := []string{"foo\u2014bar", "baz"} // em-dash
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo-bar"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateLevel4UnicodeQuotes(t *testing.T) {
	lines := []string{"say \u201Chello\u201D", "baz"} // curly double quotes
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "say \"hello\""}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateLevel4ExoticSpaces(t *testing.T) {
	// non-breaking space (U+00A0) on each side
	lines := []string{"\u00A0foo\u00A0", "bar"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateLowerLevelBeatsHigher(t *testing.T) {
	// Level 1 matches at position 0 ("foo"); level-2 fuzzy match at position 2.
	// First hit wins → start = 0.
	lines := []string{"foo", "bar", "foo   "}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0 (first exact match wins, not the trailing-ws match)", start)
	}
}

func TestLocateAnchorAdvancesCursor(t *testing.T) {
	lines := []string{"foo", "anchor", "bar", "baz"}
	h := hunk{
		anchor: "anchor",
		lines:  []hunkLine{{kind: lineContext, text: "bar"}},
	}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 2 {
		t.Fatalf("start = %d, want 2 (cursor advanced past anchor at index 1)", start)
	}
}

func TestLocateAnchorDoesNotNarrowRegion(t *testing.T) {
	// Pattern appears many lines after the anchor; should still match.
	lines := []string{"anchor", "skip1", "skip2", "skip3", "match"}
	h := hunk{
		anchor: "anchor",
		lines:  []hunkLine{{kind: lineContext, text: "match"}},
	}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 4 {
		t.Fatalf("start = %d, want 4", start)
	}
}

func TestLocateAnchorFoundViaFuzzyLadder(t *testing.T) {
	lines := []string{"anchor   ", "match"}
	h := hunk{
		anchor: "anchor",
		lines:  []hunkLine{{kind: lineContext, text: "match"}},
	}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 1 {
		t.Fatalf("start = %d, want 1", start)
	}
}

func TestLocateEOFTrailingEmptyRetry(t *testing.T) {
	// File has no trailing newline, so strings.Split gives len 2.
	// Pattern's last context line is empty. Without the EOF retry the
	// pattern would not match; the retry drops the trailing "" and finds it.
	lines := []string{"foo", "bar"}
	h := hunk{lines: []hunkLine{
		{kind: lineContext, text: "foo"},
		{kind: lineContext, text: "bar"},
		{kind: lineContext, text: ""},
	}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v (EOF empty retry should have dropped the trailing empty)", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
	match, err := locateHunk(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locateHunk err = %v", err)
	}
	if match.start != 0 || match.realMatchedLen != 2 || !match.syntheticTrailingEmpty {
		t.Fatalf("locateHunk = %+v, want start 0 len 2 synthetic trailing empty", match)
	}
}

func TestLocateEOFTrailingEmptyRetryDoesNotMatchBeforeEOF(t *testing.T) {
	lines := []string{"alpha", "beta", "charlie"}
	h := hunk{lines: []hunkLine{
		{kind: lineContext, text: "alpha"},
		{kind: lineRemove, text: ""},
	}}
	_, err := locate(lines, h, "p", 0)
	if err == nil {
		t.Fatal("locate err = nil, want non-EOF trailing-empty retry to fail")
	}
	if !strings.HasPrefix(err.Error(), "Failed to find expected lines in p:") {
		t.Fatalf("err = %q, want expected-lines failure", err.Error())
	}
}

func TestLocateNoEOFTrailingEmptyWhenFileHasIt(t *testing.T) {
	// File has a trailing newline; pattern's last context line is empty;
	// the empty matches lines[2]="". The full pattern matches without retry.
	lines := []string{"foo", "bar", ""}
	h := hunk{lines: []hunkLine{
		{kind: lineContext, text: "foo"},
		{kind: lineContext, text: "bar"},
		{kind: lineContext, text: ""},
	}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
	match, err := locateHunk(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locateHunk err = %v", err)
	}
	if match.start != 0 || match.realMatchedLen != 3 || match.syntheticTrailingEmpty {
		t.Fatalf("locateHunk = %+v, want start 0 len 3 no synthetic line", match)
	}
}

func TestLocatePatternNotFound(t *testing.T) {
	lines := []string{"foo", "bar"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "missing"}}}
	_, err := locate(lines, h, "p", 0)
	if err == nil {
		t.Fatal("locate err = nil, want non-nil")
	}
	if !strings.HasPrefix(err.Error(), "Failed to find expected lines in p:") {
		t.Fatalf("err = %q, want prefix %q", err.Error(), "Failed to find expected lines in p:")
	}
}

func TestLocateAnchorNotFound(t *testing.T) {
	lines := []string{"foo", "bar"}
	h := hunk{
		anchor: "missing",
		lines:  []hunkLine{{kind: lineContext, text: "foo"}},
	}
	_, err := locate(lines, h, "p", 0)
	if err == nil {
		t.Fatal("locate err = nil, want non-nil")
	}
	if !strings.HasPrefix(err.Error(), "Failed to find context 'missing' in p") {
		t.Fatalf("err = %q, want prefix %q", err.Error(), "Failed to find context 'missing' in p")
	}
}

func TestLocateMixedContextAndRemove(t *testing.T) {
	// Pattern is built from context + remove lines only (add lines excluded).
	// The file's current state carries the lines the patch will remove.
	lines := []string{"alpha", "BEFORE", "beta"}
	h := hunk{lines: []hunkLine{
		{kind: lineContext, text: "alpha"},
		{kind: lineRemove, text: "BEFORE"},
		{kind: lineAdd, text: "REPLACEMENT"}, // not in pattern
		{kind: lineContext, text: "beta"},
	}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateNoContextOrRemove(t *testing.T) {
	// Hunk with only add lines: no pattern to match; return the cursor.
	lines := []string{"foo", "bar"}
	h := hunk{lines: []hunkLine{{kind: lineAdd, text: "anything"}}}
	start, err := locate(lines, h, "p", 0)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestLocateCursorClamped(t *testing.T) {
	// Negative cursor clamps to 0; cursor past end clamps to len.
	lines := []string{"foo", "bar"}
	h := hunk{lines: []hunkLine{{kind: lineContext, text: "foo"}}}

	start, err := locate(lines, h, "p", -5)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0 (cursor clamped)", start)
	}

	_, err = locate(lines, h, "p", 100)
	if err == nil {
		t.Fatal("locate err = nil, want non-nil (pattern cannot be located past end)")
	}
}
