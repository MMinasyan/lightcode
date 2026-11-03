package tool

import (
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestParsePatchEnvelopeErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  error
	}{
		{"empty", "", errApplyPatchEmpty},
		{"whitespace only spaces", "   \n  \n", errApplyPatchEmpty},
		{"whitespace only tabs and newlines", "\t\n  \t\n", errApplyPatchEmpty},
		{"missing begin", "*** Add File: foo\n+content\n*** End Patch", errApplyPatchNoBegin},
		{"missing end", "*** Begin Patch\n*** Add File: foo\n+content", errApplyPatchNoEnd},
		{"wrong begin marker", "*** Begin Patches\n*** End Patch", errApplyPatchNoBegin},
		{"wrong end marker", "*** Begin Patch\n*** End Patches", errApplyPatchNoEnd},
		{"no operations", "*** Begin Patch\n*** End Patch", errApplyPatchNoOps},
		{"garbage before begin", "garbage\n*** Begin Patch\n*** End Patch", errApplyPatchNoBegin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.input)
			testassert.Require(t, errors.Is(err, tc.want), "parsePatch(%q) err = %v, want %v", tc.input, err, tc.want)
		})
	}
}

func TestParsePatchAddFile(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: new.txt\n+hello\n+world\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops), 1)
	op := p.ops[0]
	testassert.Require(t, op.kind == opAdd && op.path == "new.txt" && len(op.hunks) == 2, "got %v, want %v; got %v, want %v; got %v, want %v", op.kind, opAdd, op.path, "new.txt", len(op.hunks), 2)
	testassert.Require(t, op.hunks[0].lines[0].Kind == lineAdd && op.hunks[0].lines[0].Text == "hello", "hunks[0] = %+v, want add/hello", op.hunks[0])
	testassert.Require(t, op.hunks[1].lines[0].Kind == lineAdd && op.hunks[1].lines[0].Text == "world", "hunks[1] = %+v, want add/world", op.hunks[1])
}

func TestParsePatchAddFileEmptyContentLine(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: new.txt\n+a\n+\n+b\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops), 1)
	testassert.Equal(t, len(p.ops[0].hunks), 3)
	testassert.Equal(t, p.ops[0].hunks[1].lines[0].Text, "")
}

func TestParsePatchUpdateFile(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a.go\n@@ func Foo\n ctx\n-old\n+new\n ctx2\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops), 1)
	op := p.ops[0]
	testassert.Require(t, op.kind == opUpdate && op.path == "a.go" && op.movePath == "" && len(op.hunks) == 1, "got %v, want %v; got %v, want %v; got %v, want %v; got %v, want %v", op.kind, opUpdate, op.path, "a.go", op.movePath, "", len(op.hunks), 1)
	h := op.hunks[0]
	testassert.Equal(t, h.anchor, "func Foo")
	want := []hunkLine{
		{Kind: lineContext, Text: "ctx"},
		{Kind: lineRemove, Text: "old"},
		{Kind: lineAdd, Text: "new"},
		{Kind: lineContext, Text: "ctx2"},
	}
	testassert.Equal(t, len(h.lines), len(want))
	for i, l := range h.lines {
		testassert.Require(t, l == want[i], "hunk lines[%d] = %+v, want %+v", i, l, want[i])
	}
}

func TestParsePatchUpdateFileNoAnchor(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a.go\n@@\n-removed\n+added\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, p.ops[0].hunks[0].anchor, "")
}

func TestParsePatchDeleteFile(t *testing.T) {
	input := "*** Begin Patch\n*** Delete File: old.txt\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops), 1)
	op := p.ops[0]
	testassert.Require(t, op.kind == opDelete && op.path == "old.txt" && len(op.hunks) == 0, "got %v, want %v; got %v, want %v; got %v, want %v", op.kind, opDelete, op.path, "old.txt", len(op.hunks), 0)
}

func TestParsePatchUpdateWithMove(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@ package main\n-func main() {}\n+func main() { run() }\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	op := p.ops[0]
	testassert.Require(t, op.kind == opUpdate && op.path == "old.go" && op.movePath == "new.go" && len(op.hunks) == 1, "got %v, want %v; got %v, want %v; got %v, want %v; got %v, want %v", op.kind, opUpdate, op.path, "old.go", op.movePath, "new.go", len(op.hunks), 1)
}

func TestParsePatchMultipleOps(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+x\n*** Update File: b\n@@\n-c\n+d\n*** Delete File: c\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops), 3)
	testassert.Require(t, p.ops[0].kind == opAdd && p.ops[0].path == "a", "ops[0] = %+v, want Add a", p.ops[0])
	testassert.Require(t, p.ops[1].kind == opUpdate && p.ops[1].path == "b", "ops[1] = %+v, want Update b", p.ops[1])
	testassert.Require(t, p.ops[2].kind == opDelete && p.ops[2].path == "c", "ops[2] = %+v, want Delete c", p.ops[2])
}

func TestParsePatchMultipleHunksOneFile(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@ first\n-old1\n+new1\n@@ second\n-old2\n+new2\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	op := p.ops[0]
	testassert.Equal(t, len(op.hunks), 2)
	testassert.Require(t, op.hunks[0].anchor == "first" && op.hunks[1].anchor == "second", "anchors = %q, %q, want first, second", op.hunks[0].anchor, op.hunks[1].anchor)
}

func TestParsePatchPreservesPrefixCharactersInContent(t *testing.T) {
	// An Add line's content can start with `+`, `-`, or ` `; the parser must
	// strip exactly one prefix byte and preserve the rest verbatim.
	input := "*** Begin Patch\n*** Add File: a\n+++counter\n+--option\n+ indented\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops[0].hunks), 3)
	want := []string{"++counter", "--option", " indented"}
	for i, h := range p.ops[0].hunks {
		testassert.Require(t, h.lines[0].Kind == lineAdd, "hunks[%d] kind = %d, want lineAdd", i, h.lines[0].Kind)
		testassert.Require(t, h.lines[0].Text == want[i], "hunks[%d] text = %q, want %q", i, h.lines[0].Text, want[i])
	}
}

func TestParsePatchPreservesRawCR(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+line1\r\n+line2\r\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, len(p.ops[0].hunks), 2)
	testassert.Equal(t, p.ops[0].hunks[0].lines[0].Text, "line1\r")
	testassert.Equal(t, p.ops[0].hunks[1].lines[0].Text, "line2\r")
}

func TestParsePatchPreservesCRInUpdateHunk(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@\n ctx\r\n-old\r\n+new\r\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	h := p.ops[0].hunks[0]
	want := []string{"ctx\r", "old\r", "new\r"}
	testassert.Equal(t, len(h.lines), len(want))
	for i, l := range h.lines {
		testassert.Require(t, l.Text == want[i], "hunk lines[%d] text = %q, want %q", i, l.Text, want[i])
	}
}

func TestParsePatchRefusals(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		sentinel error
		contains string
	}{
		{
			name:     "TestParsePatchDuplicatePath",
			input:    "*** Begin Patch\n*** Add File: a\n+x\n*** Add File: a\n+y\n*** End Patch",
			sentinel: errApplyPatchDupPath,
			contains: "a",
		},
		{
			name:     "TestParsePatchMoveDestDuplicatesExistingPath",
			input:    "*** Begin Patch\n*** Add File: b\n+z\n*** Update File: a\n*** Move to: b\n@@\n-x\n+y\n*** End Patch",
			sentinel: errApplyPatchDupPath,
		},
		{
			name:     "TestParsePatchAddFileBadBodyLine",
			input:    "*** Begin Patch\n*** Add File: a\nbad\n*** End Patch",
			sentinel: errApplyPatchAddBody,
		},
		{
			name:     "TestParsePatchAddFileEmptyBody",
			input:    "*** Begin Patch\n*** Add File: a\n*** End Patch",
			contains: "no content",
		},
		{
			name:     "TestParsePatchUpdateNoHunks",
			input:    "*** Begin Patch\n*** Update File: a\n*** End Patch",
			contains: "no hunks",
		},
		{
			name:     "TestParsePatchHunkBadLine",
			input:    "*** Begin Patch\n*** Update File: a\n@@\nctx\nbad\n*** End Patch",
			sentinel: errApplyPatchHunkLine,
		},
		{
			name:     "TestParsePatchHunkEmptyLine",
			input:    "*** Begin Patch\n*** Update File: a\n@@\nctx\n\nmore\n*** End Patch",
			sentinel: errApplyPatchHunkLine,
		},
		{
			name:     "TestParsePatchMissingHeaderBeforeHunk",
			input:    "*** Begin Patch\n@@\nctx\n*** End Patch",
			sentinel: errApplyPatchHunkExpected,
		},
		{
			name:     "TestParsePatchUnknownHeader",
			input:    "*** Begin Patch\n*** Rename File: a\n*** End Patch",
			sentinel: errApplyPatchUnknownHead,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.input)
			testassert.Require(t, err != nil, "parsePatch err = nil, want non-nil")
			testassert.Require(t, tc.sentinel == nil || errors.Is(err, tc.sentinel), "parsePatch err = %v, want %v", err, tc.sentinel)
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("parsePatch err = %q, want it to contain %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestParsePatchTrailingNewlineTolerated(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+x\n*** End Patch\n"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Require(t, len(p.ops) == 1 && p.ops[0].path == "a", "ops = %+v, want single Add of a", p.ops)
}

func TestParsePatchAnchorsWithLeadingTrailingSpaces(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@   spaced anchor  \n-x\n+y\n*** End Patch"
	p, err := parsePatch(input)
	testassert.NoError(t, err)
	testassert.Equal(t, p.ops[0].hunks[0].anchor, "spaced anchor")
}
