package tool

import (
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestLocateScenarios(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		h         hunk
		cursor    int
		start     int
		wantErr   bool
		errPrefix string
	}{
		{
			name:   "TestLocateExactMatch",
			lines:  []string{"foo", "bar", "baz"},
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "bar"}}},
			cursor: 0,
			start:  1,
		},
		{
			name:   "TestLocateForwardScan first match",
			lines:  []string{"foo", "bar", "foo", "bar"},
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}, {Kind: lineContext, Text: "bar"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateForwardScan second match",
			lines:  []string{"foo", "bar", "foo", "bar"},
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}, {Kind: lineContext, Text: "bar"}}},
			cursor: 2,
			start:  2,
		},
		{
			name:   "TestLocateLevel2TrailingWhitespace",
			lines:  []string{"foo   ", "bar", "baz"},
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateLevel3LeadingAndTrailingWhitespace",
			lines:  []string{"  foo  ", "bar", "baz"},
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateLevel4UnicodeDashes",
			lines:  []string{"foo\u2014bar", "baz"}, // em-dash
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo-bar"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateLevel4UnicodeQuotes",
			lines:  []string{"say \u201Chello\u201D", "baz"}, // curly double quotes
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "say \"hello\""}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateLevel4ExoticSpaces",
			lines:  []string{"\u00A0foo\u00A0", "bar"}, // non-breaking space (U+00A0) on each side
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateLowerLevelBeatsHigher",
			lines:  []string{"foo", "bar", "foo   "}, // Level 1 matches at 0; level-2 fuzzy match at 2.
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:  "TestLocateAnchorAdvancesCursor",
			lines: []string{"foo", "anchor", "bar", "baz"},
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "bar"}},
			},
			cursor: 0,
			start:  2,
		},
		{
			name:  "TestLocateAnchorDoesNotNarrowRegion",
			lines: []string{"anchor", "skip1", "skip2", "skip3", "match"}, // Pattern appears many lines after the anchor.
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "match"}},
			},
			cursor: 0,
			start:  4,
		},
		{
			name:  "TestLocateAnchorFoundViaFuzzyLadder",
			lines: []string{"anchor   ", "match"},
			h: hunk{
				anchor: "anchor",
				lines:  []hunkLine{{Kind: lineContext, Text: "match"}},
			},
			cursor: 0,
			start:  1,
		},
		{
			name:  "TestLocateEOFTrailingEmptyRetryDoesNotMatchBeforeEOF",
			lines: []string{"alpha", "beta", "charlie"},
			h: hunk{lines: []hunkLine{
				{Kind: lineContext, Text: "alpha"},
				{Kind: lineRemove, Text: ""},
			}},
			cursor:    0,
			wantErr:   true,
			errPrefix: "Failed to find expected lines in p:",
		},
		{
			name:      "TestLocatePatternNotFound",
			lines:     []string{"foo", "bar"},
			h:         hunk{lines: []hunkLine{{Kind: lineContext, Text: "missing"}}},
			cursor:    0,
			wantErr:   true,
			errPrefix: "Failed to find expected lines in p:",
		},
		{
			name:  "TestLocateAnchorNotFound",
			lines: []string{"foo", "bar"},
			h: hunk{
				anchor: "missing",
				lines:  []hunkLine{{Kind: lineContext, Text: "foo"}},
			},
			cursor:    0,
			wantErr:   true,
			errPrefix: "Failed to find context 'missing' in p",
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
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateNoContextOrRemove",
			lines:  []string{"foo", "bar"}, // Hunk with only add lines: no pattern to match; return the cursor.
			h:      hunk{lines: []hunkLine{{Kind: lineAdd, Text: "anything"}}},
			cursor: 0,
			start:  0,
		},
		{
			name:   "TestLocateCursorClamped negative cursor",
			lines:  []string{"foo", "bar"}, // Negative cursor clamps to 0.
			h:      hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor: -5,
			start:  0,
		},
		{
			name:    "TestLocateCursorClamped past end",
			lines:   []string{"foo", "bar"}, // Cursor past end clamps to len.
			h:       hunk{lines: []hunkLine{{Kind: lineContext, Text: "foo"}}},
			cursor:  100,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, err := locate(tc.lines, tc.h, "p", tc.cursor)
			if tc.wantErr {
				testassert.Require(t, err != nil, "locate(cursor=%d) err = nil, want non-nil", tc.cursor)
				if tc.errPrefix != "" && !strings.HasPrefix(err.Error(), tc.errPrefix) {
					t.Fatalf("locate(cursor=%d) err = %q, want prefix %q", tc.cursor, err.Error(), tc.errPrefix)
				}
				return
			}
			testassert.Require(t, err == nil, "locate(cursor=%d) err = %v", tc.cursor, err)
			testassert.Require(t, start == tc.start, "locate(cursor=%d) start = %d, want %d", tc.cursor, start, tc.start)
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
	testassert.NoError(t, err)
	testassert.Equal(t, start, 0)
	match, err := locateHunk(lines, h, "p", 0)
	testassert.NoError(t, err)
	testassert.Require(t, match.start == 0 && match.realMatchedLen == 2 && match.syntheticTrailingEmpty, "locateHunk = %+v, want start 0 len 2 synthetic trailing empty", match)
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
	testassert.NoError(t, err)
	testassert.Equal(t, start, 0)
	match, err := locateHunk(lines, h, "p", 0)
	testassert.NoError(t, err)
	testassert.Require(t, match.start == 0 && match.realMatchedLen == 3 && !match.syntheticTrailingEmpty, "locateHunk = %+v, want start 0 len 3 no synthetic line", match)
}
