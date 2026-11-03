package terminal

import (
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestRenderEditPreviewColorsOnlyGutter(t *testing.T) {
	preview := &editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: []editpreview.Row{
		{Kind: editpreview.KindRemove, OldLine: 12, Text: "removed()"},
		{Kind: editpreview.KindAdd, NewLine: 12, Text: "added()"},
		{Kind: editpreview.KindContext, OldLine: 13, NewLine: 13, Text: "kept()"},
	}}}}
	rendered := RenderEditPreview(preview, "  ", 80, true)

	testassert.Require(t, strings.Contains(rendered, "  "+colorRed+"12 -"+colorReset+colorDim+"removed()"+colorReset) && strings.Contains(rendered, "  "+colorGreen+"12 +"+colorReset+colorDim+"added()"+colorReset) && strings.Contains(rendered, "  "+colorDim+"13  "+colorReset+colorDim+"kept()"+colorReset) && !strings.Contains(rendered, colorRed+"removed()") && !strings.Contains(rendered, colorGreen+"added()"), "removed gutter was not red or spacing is wrong: %q; added gutter was not green or spacing is wrong: %q; context gutter was not dim or spacing is wrong: %q; code content should not be colored: %q", rendered, rendered, rendered, rendered)
}

func TestRenderEditPreviewCollapsesUnlessExpanded(t *testing.T) {
	preview := &editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: []editpreview.Row{
		{Kind: editpreview.KindRemove, OldLine: 1, Text: "one"},
		{Kind: editpreview.KindAdd, NewLine: 1, Text: "two"},
		{Kind: editpreview.KindContext, OldLine: 2, NewLine: 2, Text: "three"},
		{Kind: editpreview.KindAdd, NewLine: 3, Text: "four"},
		{Kind: editpreview.KindRemove, OldLine: 4, Text: "five"},
		{Kind: editpreview.KindAdd, NewLine: 4, Text: "six"},
		{Kind: editpreview.KindRemove, OldLine: 5, Text: "seven"},
		{Kind: editpreview.KindAdd, NewLine: 5, Text: "eight"},
		{Kind: editpreview.KindContext, OldLine: 6, NewLine: 6, Text: "nine"},
		{Kind: editpreview.KindAdd, NewLine: 7, Text: "ten"},
		{Kind: editpreview.KindRemove, OldLine: 8, Text: "eleven"},
	}}}}

	collapsed := RenderEditPreview(preview, "", 80, false)
	testassert.Require(t, !strings.Contains(collapsed, "eleven") && strings.Contains(collapsed, "(1 more line)"), "collapsed preview should hide eleventh row: %q; collapsed preview missing more-lines marker: %q", collapsed, collapsed)
	{
		got, want := strings.Count(collapsed, nl), collapsedEditPreviewLines+1
		testassert.Require(t, got == want, "collapsed preview lines = %d, want %d: %q", got, want, collapsed)
	}

	expanded := RenderEditPreview(preview, "", 80, true)
	testassert.Require(t, strings.Contains(expanded, "eleven") && !strings.Contains(expanded, "more lines"), "expanded preview missing eleventh row: %q; expanded preview should not show more-lines marker: %q", expanded, expanded)
}

func TestRenderEditPreviewCollapsedSkipsHunkGaps(t *testing.T) {
	preview := &editpreview.Preview{Hunks: []editpreview.Hunk{
		{Rows: []editpreview.Row{
			{Kind: editpreview.KindRemove, OldLine: 1, Text: "one"},
			{Kind: editpreview.KindAdd, NewLine: 1, Text: "two"},
			{Kind: editpreview.KindContext, OldLine: 2, NewLine: 2, Text: "three"},
			{Kind: editpreview.KindAdd, NewLine: 3, Text: "four"},
			{Kind: editpreview.KindRemove, OldLine: 4, Text: "five"},
		}},
		{Rows: []editpreview.Row{
			{Kind: editpreview.KindAdd, NewLine: 10, Text: "six"},
			{Kind: editpreview.KindContext, OldLine: 11, NewLine: 11, Text: "seven"},
			{Kind: editpreview.KindRemove, OldLine: 12, Text: "eight"},
			{Kind: editpreview.KindAdd, NewLine: 12, Text: "nine"},
			{Kind: editpreview.KindContext, OldLine: 13, NewLine: 13, Text: "ten"},
			{Kind: editpreview.KindAdd, NewLine: 14, Text: "eleven"},
		}},
	}}

	collapsed := RenderEditPreview(preview, "", 80, false)
	{
		got, want := strings.Count(collapsed, nl), collapsedEditPreviewLines+1
		testassert.Require(t, got == want, "collapsed preview terminal lines = %d, want %d: %q", got, want, collapsed)
	}
	testassert.Require(t, !strings.Contains(collapsed, nl+nl) && !strings.Contains(collapsed, "eleven"), "collapsed preview should not include hunk gap: %q; collapsed preview should hide eleventh row: %q", collapsed, collapsed)
}

func TestRenderOutputPreviewShowsMoreLinesMarker(t *testing.T) {
	rendered := renderOutputPreview("1\n2\n3\n4\n5\n6\n7", "  ", 80, colorDim, false)
	testassert.Require(t, !strings.Contains(rendered, "6") && !strings.Contains(rendered, "7") && strings.Contains(rendered, "(2 more lines)"), "collapsed output should hide extra lines: %q; collapsed output missing more-lines marker: %q", rendered, rendered)

	expanded := renderOutputPreview("1\n2\n3\n4\n5\n6\n7", "  ", 80, colorDim, true)
	testassert.Require(t, strings.Contains(expanded, "6") && !strings.Contains(expanded, "more lines"), "expanded output should show all lines without marker: %q", expanded)
}

func TestRenderToolCallShowsWriteLineCount(t *testing.T) {
	rendered := RenderToolCall("write_file", `{"path":"x.go","content":"one\ntwo\n"}`, nil)
	testassert.Require(t, strings.Contains(rendered, colorGreen+"+2"+colorCyan), "write_file header missing green inserted-line count: %q", rendered)
}

func TestRenderToolCallShowsEditChangeCounts(t *testing.T) {
	metadata := map[string]any{"edit_preview": &editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: []editpreview.Row{
		{Kind: editpreview.KindRemove, OldLine: 1, Text: "old"},
		{Kind: editpreview.KindAdd, NewLine: 1, Text: "new"},
		{Kind: editpreview.KindAdd, NewLine: 2, Text: "extra"},
	}}}}}

	rendered := RenderToolCall("edit_file", `{"path":"x.go"}`, metadata)
	testassert.Require(t, strings.Contains(rendered, colorGreen+"+2"+colorCyan) && strings.Contains(rendered, colorRed+"-1"+colorCyan), "edit_file header missing green inserted-line count: %q; edit_file header missing red deleted-line count: %q", rendered, rendered)
}

func TestRenderToolCallPreservesMultilineCommandHeader(t *testing.T) {
	rendered := RenderToolCall("run_command", `{"command":"cd /tmp && git commit -m \"Title\n\n    - one\n    - two with a deliberately long line that should not be shortened by the header renderer\""}`, nil)
	testassert.Require(t, strings.Contains(rendered, "Title"+nl+"     (3 more lines)") && !strings.Contains(rendered, `Title - one - two`) && !strings.Contains(rendered, "- one") && !strings.Contains(rendered, "- two") && !strings.Contains(rendered, "..."), "tool header should collapse multiline command with continuation marker: %q; tool header should not collapse multiline command into one line: %q; tool header should hide continuation command lines: %q; tool header should not truncate command lines: %q", rendered, rendered, rendered, rendered)
}

func TestExtractJSONString(t *testing.T) {
	cases := []struct{ json, key, want string }{
		{`{"path":"/tmp/x.go"}`, "path", "/tmp/x.go"},
		{`{"command":"echo \"hello\""}`, "command", `echo "hello"`},
		{`{"path":"a\nb"}`, "path", "a\nb"},
		{`{"path":"a\rb"}`, "path", "a\rb"},
		{`{"path":"a\tb"}`, "path", "a\tb"},
		{`{"path":"a\\b"}`, "path", `a\b`},
		{`{"path":"\u0041\u0042\u0043"}`, "path", "ABC"},
		{`{"path":"\u00e9"}`, "path", "\u00e9"},
		{`{}`, "path", ""},
		{`not json`, "path", ""},
		{`{"other":1}`, "path", ""},
		{`{"path":"ok"} trailing`, "path", ""},
	}
	for _, tc := range cases {
		got := extractJSONString(tc.json, tc.key)
		testassert.Check(t, got == tc.want, "extractJSONString(%q, %q) = %q, want %q", tc.json, tc.key, got, tc.want)
	}
}

func TestExtractJSONNumber(t *testing.T) {
	cases := []struct{ json, key, want string }{
		{`{"seconds":5}`, "seconds", "5"},
		{`{"seconds":1.5}`, "seconds", "1.5"},
		{`{"seconds":1e3}`, "seconds", "1e3"},
		{`{"seconds":1.5e2}`, "seconds", "1.5e2"},
		{`{}`, "seconds", ""},
		{`not json`, "seconds", ""},
		{`{"other":"text"}`, "seconds", ""},
		{`{"seconds":5} trailing`, "seconds", ""},
		{`{"seconds":"5"}`, "seconds", ""},
		{`{"seconds":"1.5"}`, "seconds", ""},
	}
	for _, tc := range cases {
		got := extractJSONNumber(tc.json, tc.key)
		testassert.Check(t, got == tc.want, "extractJSONNumber(%q, %q) = %q, want %q", tc.json, tc.key, got, tc.want)
	}
}

func TestFormatToolArgsSleepQuotedNumberFallback(t *testing.T) {
	got := FormatToolArgs("sleep", `{"seconds":"5"}`)
	testassert.Check(t, got == `{"seconds":"5"}`, "FormatToolArgs sleep with quoted number = %q, want raw args fallback", got)

	got = FormatToolArgs("sleep", `{"seconds":5}`)
	testassert.Check(t, got == "5s", "FormatToolArgs sleep with real number = %q, want %q", got, "5s")
}
