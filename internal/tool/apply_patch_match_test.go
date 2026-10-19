package tool

import (
	"strings"
	"testing"
)

func TestLocateScenarios(t *testing.T) {
	type probe struct {
		cursor    int
		start     int
		wantErr   bool
		errPrefix string
	}
	cases := []struct {
		name   string
		lines  []string
		h      hunk
		probes []probe
	}{
		{
			name:  "TestLocateExactMatch",
			lines: []string{"foo", "bar", "baz"},
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "bar"}}},
			probes: []probe{
				{cursor: 0, start: 1},
			},
		},
		{
			name:  "TestLocateForwardScan",
			lines: []string{"foo", "bar", "foo", "bar"},
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}, {Kind: lineContext, Text: "bar"}}},
			probes: []probe{
				{cursor: 0, start: 0},
				{cursor: 2, start: 2},
			},
		},
		{
			name:  "TestLocateLevel2TrailingWhitespace",
			lines: []string{"foo   ", "bar", "baz"},
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateLevel3LeadingAndTrailingWhitespace",
			lines: []string{"  foo  ", "bar", "baz"},
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateLevel4UnicodeDashes",
			lines: []string{"foo\u2014bar", "baz"}, // em-dash
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo-bar"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateLevel4UnicodeQuotes",
			lines: []string{"say \u201Chello\u201D", "baz"}, // curly double quotes
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "say \"hello\""}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateLevel4ExoticSpaces",
			lines: []string{"\u00A0foo\u00A0", "bar"}, // non-breaking space (U+00A0) on each side
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateLowerLevelBeatsHigher",
			lines: []string{"foo", "bar", "foo   "}, // Level 1 matches at 0; level-2 fuzzy match at 2.
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateAnchorAdvancesCursor",
			lines: []string{"foo", "anchor", "bar", "baz"},
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "bar"}},
			},
			probes: []probe{
				{cursor: 0, start: 2},
			},
		},
		{
			name:  "TestLocateAnchorDoesNotNarrowRegion",
			lines: []string{"anchor", "skip1", "skip2", "skip3", "match"}, // Pattern appears many lines after the anchor.
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "match"}},
			},
			probes: []probe{
				{cursor: 0, start: 4},
			},
		},
		{
			name:  "TestLocateAnchorFoundViaFuzzyLadder",
			lines: []string{"anchor   ", "match"},
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "match"}},
			},
			probes: []probe{
				{cursor: 0, start: 1},
			},
		},
		{
			name:  "TestLocateEOFTrailingEmptyRetryDoesNotMatchBeforeEOF",
			lines: []string{"alpha", "beta", "charlie"},
			h: hunk{lines: []hunkLine{
				{Kind: lineContext, Text: "alpha"},
				{Kind: lineRemove, Text: ""},
			}},
			probes: []probe{
				{cursor: 0, wantErr: true, errPrefix: "Failed to find expected lines in p:"},
			},
		},
		{
			name:  "TestLocatePatternNotFound",
			lines: []string{"foo", "bar"},
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "missing"}}},
			probes: []probe{
				{cursor: 0, wantErr: true, errPrefix: "Failed to find expected lines in p:"},
			},
		},
		{
			name:  "TestLocateAnchorNotFound",
			lines: []string{"foo", "bar"},
			h: hunk{
				anchor: "missing",
				lines:  []hunkLine{{Kind: lineContext, Text: "foo"}},
			},
			probes: []probe{
				{cursor: 0, wantErr: true, errPrefix: "Failed to find context 'missing' in p"},
			},
		},
		{
			name:  "TestLocateMixedContextAndRemove",
			lines: []string{"alpha", "BEFORE", "beta"}, // File carries the lines the patch will remove.
			h: hunk{lines: []hunkLine{
				{Kind: lineContext, Text: "alpha"},
				{Kind: lineRemove, Text: "BEFORE"},
				{Kind: lineAdd, Text: "REPLACEMENT"}, // not in pattern
				{Kind: lineContext, Text: "beta"},
			}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateNoContextOrRemove",
			lines: []string{"foo", "bar"}, // Hunk with only add lines: no pattern to match; return the cursor.
			h:     hunk{lines: []hunkLine{{Kind: lineAdd, Text: "anything"}}},
			probes: []probe{
				{cursor: 0, start: 0},
			},
		},
		{
			name:  "TestLocateCursorClamped",
			lines: []string{"foo", "bar"}, // Negative cursor clamps to 0; cursor past end clamps to len.
			h:     hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			probes: []probe{
				{cursor: -5, start: 0},
				{cursor: 100, wantErr: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range tc.probes {
				start, err := locate(tc.lines, tc.h, "p", p.cursor)
				if p.wantErr {
					if err == nil {
						t.Fatalf("locate(cursor=%d) err = nil, want non-nil", p.cursor)
					}
					if p.errPrefix != "" && !strings.HasPrefix(err.Error(), p.errPrefix) {
						t.Fatalf("locate(cursor=%d) err = %q, want prefix %q", p.cursor, err.Error(), p.errPrefix)
					}
					continue
				}
				if err != nil {
					t.Fatalf("locate(cursor=%d) err = %v", p.cursor, err)
				}
				if start != p.start {
					t.Fatalf("locate(cursor=%d) start = %d, want %d", p.cursor, start, p.start)
				}
			}
		})
	}
}

func TestLocateEOFTrailingEmptyRetry(t *testing.T) {
	// File has no trailing newline, so strings.Split gives len 2.
	// Pattern's last context line is empty. Without the EOF retry the
	// pattern would not match; the retry drops the trailing "" and finds it.
	lines := []string{"foo", "bar"}
	h := hunk{lines: []hunkLine{
		{Kind: lineContext, Text: "foo"},
		{Kind: lineContext, Text: "bar"},
		{Kind: lineContext, Text: ""},
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

func TestLocateNoEOFTrailingEmptyWhenFileHasIt(t *testing.T) {
	// File has a trailing newline; pattern's last context line is empty;
	// the empty matches lines[2]="". The full pattern matches without retry.
	lines := []string{"foo", "bar", ""}
	h := hunk{lines: []hunkLine{
		{Kind: lineContext, Text: "foo"},
		{Kind: lineContext, Text: "bar"},
		{Kind: lineContext, Text: ""},
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
