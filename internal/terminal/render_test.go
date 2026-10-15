package terminal

import (
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
)

func TestRenderEditPreviewColorsOnlyGutter(t *testing.T) {
	preview := &editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: []editpreview.Row{
		{Kind: editpreview.KindRemove, OldLine: 12, Text: "removed()"},
		{Kind: editpreview.KindAdd, NewLine: 12, Text: "added()"},
		{Kind: editpreview.KindContext, OldLine: 13, NewLine: 13, Text: "kept()"},
	}}}}
	rendered := RenderEditPreview(preview, "  ", 80, true)

	if !strings.Contains(rendered, "  "+colorRed+"12 -"+colorReset+colorDim+"removed()"+colorReset) {
		t.Fatalf("removed gutter was not red or spacing is wrong: %q", rendered)
	}
	if !strings.Contains(rendered, "  "+colorGreen+"12 +"+colorReset+colorDim+"added()"+colorReset) {
		t.Fatalf("added gutter was not green or spacing is wrong: %q", rendered)
	}
	if !strings.Contains(rendered, "  "+colorDim+"13  "+colorReset+colorDim+"kept()"+colorReset) {
		t.Fatalf("context gutter was not dim or spacing is wrong: %q", rendered)
	}
	if strings.Contains(rendered, colorRed+"removed()") || strings.Contains(rendered, colorGreen+"added()") {
		t.Fatalf("code content should not be colored: %q", rendered)
	}
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
	if strings.Contains(collapsed, "eleven") {
		t.Fatalf("collapsed preview should hide eleventh row: %q", collapsed)
	}
	if !strings.Contains(collapsed, "(1 more line)") {
		t.Fatalf("collapsed preview missing more-lines marker: %q", collapsed)
	}
	if got, want := strings.Count(collapsed, nl), collapsedEditPreviewLines+1; got != want {
		t.Fatalf("collapsed preview lines = %d, want %d: %q", got, want, collapsed)
	}

	expanded := RenderEditPreview(preview, "", 80, true)
	if !strings.Contains(expanded, "eleven") {
		t.Fatalf("expanded preview missing eleventh row: %q", expanded)
	}
	if strings.Contains(expanded, "more lines") {
		t.Fatalf("expanded preview should not show more-lines marker: %q", expanded)
	}
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
	if got, want := strings.Count(collapsed, nl), collapsedEditPreviewLines+1; got != want {
		t.Fatalf("collapsed preview terminal lines = %d, want %d: %q", got, want, collapsed)
	}
	if strings.Contains(collapsed, nl+nl) {
		t.Fatalf("collapsed preview should not include hunk gap: %q", collapsed)
	}
	if strings.Contains(collapsed, "eleven") {
		t.Fatalf("collapsed preview should hide eleventh row: %q", collapsed)
	}
}

func TestRenderOutputPreviewShowsMoreLinesMarker(t *testing.T) {
	rendered := renderOutputPreview("1\n2\n3\n4\n5\n6\n7", "  ", 80, colorDim, false)
	if strings.Contains(rendered, "6") || strings.Contains(rendered, "7") {
		t.Fatalf("collapsed output should hide extra lines: %q", rendered)
	}
	if !strings.Contains(rendered, "(2 more lines)") {
		t.Fatalf("collapsed output missing more-lines marker: %q", rendered)
	}

	expanded := renderOutputPreview("1\n2\n3\n4\n5\n6\n7", "  ", 80, colorDim, true)
	if !strings.Contains(expanded, "6") || strings.Contains(expanded, "more lines") {
		t.Fatalf("expanded output should show all lines without marker: %q", expanded)
	}
}

func TestRenderToolCallShowsWriteLineCount(t *testing.T) {
	rendered := RenderToolCall("write_file", `{"path":"x.go","content":"one\ntwo\n"}`, nil)
	if !strings.Contains(rendered, colorGreen+"+2"+colorCyan) {
		t.Fatalf("write_file header missing green inserted-line count: %q", rendered)
	}
}

func TestRenderToolCallShowsEditChangeCounts(t *testing.T) {
	metadata := map[string]any{"edit_preview": &editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: []editpreview.Row{
		{Kind: editpreview.KindRemove, OldLine: 1, Text: "old"},
		{Kind: editpreview.KindAdd, NewLine: 1, Text: "new"},
		{Kind: editpreview.KindAdd, NewLine: 2, Text: "extra"},
	}}}}}

	rendered := RenderToolCall("edit_file", `{"path":"x.go"}`, metadata)
	if !strings.Contains(rendered, colorGreen+"+2"+colorCyan) {
		t.Fatalf("edit_file header missing green inserted-line count: %q", rendered)
	}
	if !strings.Contains(rendered, colorRed+"-1"+colorCyan) {
		t.Fatalf("edit_file header missing red deleted-line count: %q", rendered)
	}
}

func TestRenderToolCallPreservesMultilineCommandHeader(t *testing.T) {
	rendered := RenderToolCall("run_command", `{"command":"cd /tmp && git commit -m \"Title\n\n    - one\n    - two with a deliberately long line that should not be shortened by the header renderer\""}`, nil)
	if !strings.Contains(rendered, "Title"+nl+"     (3 more lines)") {
		t.Fatalf("tool header should collapse multiline command with continuation marker: %q", rendered)
	}
	if strings.Contains(rendered, `Title - one - two`) {
		t.Fatalf("tool header should not collapse multiline command into one line: %q", rendered)
	}
	if strings.Contains(rendered, "- one") || strings.Contains(rendered, "- two") {
		t.Fatalf("tool header should hide continuation command lines: %q", rendered)
	}
	if strings.Contains(rendered, "...") {
		t.Fatalf("tool header should not truncate command lines: %q", rendered)
	}
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
		if got != tc.want {
			t.Errorf("extractJSONString(%q, %q) = %q, want %q", tc.json, tc.key, got, tc.want)
		}
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
		if got != tc.want {
			t.Errorf("extractJSONNumber(%q, %q) = %q, want %q", tc.json, tc.key, got, tc.want)
		}
	}
}

func TestFormatToolArgsSleepQuotedNumberFallback(t *testing.T) {
	got := FormatToolArgs("sleep", `{"seconds":"5"}`)
	if got != `{"seconds":"5"}` {
		t.Errorf("FormatToolArgs sleep with quoted number = %q, want raw args fallback", got)
	}

	got = FormatToolArgs("sleep", `{"seconds":5}`)
	if got != "5s" {
		t.Errorf("FormatToolArgs sleep with real number = %q, want %q", got, "5s")
	}
}
