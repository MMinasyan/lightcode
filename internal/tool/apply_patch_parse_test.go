package tool

import (
	"errors"
	"strings"
	"testing"
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
			if !errors.Is(err, tc.want) {
				t.Fatalf("parsePatch(%q) err = %v, want %v", tc.input, err, tc.want)
			}
		})
	}
}

func TestParsePatchAddFile(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: new.txt\n+hello\n+world\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(p.ops))
	}
	op := p.ops[0]
	if op.kind != opAdd {
		t.Fatalf("op.kind = %d, want opAdd", op.kind)
	}
	if op.path != "new.txt" {
		t.Fatalf("op.path = %q, want new.txt", op.path)
	}
	if len(op.hunks) != 2 {
		t.Fatalf("hunks = %d, want 2 (one per + line)", len(op.hunks))
	}
	if op.hunks[0].lines[0].kind != lineAdd || op.hunks[0].lines[0].text != "hello" {
		t.Fatalf("hunks[0] = %+v, want add/hello", op.hunks[0])
	}
	if op.hunks[1].lines[0].kind != lineAdd || op.hunks[1].lines[0].text != "world" {
		t.Fatalf("hunks[1] = %+v, want add/world", op.hunks[1])
	}
}

func TestParsePatchAddFileEmptyContentLine(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: new.txt\n+a\n+\n+b\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(p.ops))
	}
	if len(p.ops[0].hunks) != 3 {
		t.Fatalf("hunks = %d, want 3", len(p.ops[0].hunks))
	}
	if got := p.ops[0].hunks[1].lines[0].text; got != "" {
		t.Fatalf("hunks[1] text = %q, want empty (a bare + line is an empty content line)", got)
	}
}

func TestParsePatchUpdateFile(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a.go\n@@ func Foo\n ctx\n-old\n+new\n ctx2\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(p.ops))
	}
	op := p.ops[0]
	if op.kind != opUpdate {
		t.Fatalf("op.kind = %d, want opUpdate", op.kind)
	}
	if op.path != "a.go" {
		t.Fatalf("op.path = %q, want a.go", op.path)
	}
	if op.movePath != "" {
		t.Fatalf("op.movePath = %q, want empty", op.movePath)
	}
	if len(op.hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(op.hunks))
	}
	h := op.hunks[0]
	if h.anchor != "func Foo" {
		t.Fatalf("hunk anchor = %q, want func Foo", h.anchor)
	}
	want := []hunkLine{
		{kind: lineContext, text: "ctx"},
		{kind: lineRemove, text: "old"},
		{kind: lineAdd, text: "new"},
		{kind: lineContext, text: "ctx2"},
	}
	if len(h.lines) != len(want) {
		t.Fatalf("hunk lines = %d, want %d", len(h.lines), len(want))
	}
	for i, l := range h.lines {
		if l != want[i] {
			t.Fatalf("hunk lines[%d] = %+v, want %+v", i, l, want[i])
		}
	}
}

func TestParsePatchUpdateFileNoAnchor(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a.go\n@@\n-removed\n+added\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if got := p.ops[0].hunks[0].anchor; got != "" {
		t.Fatalf("hunk anchor = %q, want empty", got)
	}
}

func TestParsePatchDeleteFile(t *testing.T) {
	input := "*** Begin Patch\n*** Delete File: old.txt\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(p.ops))
	}
	op := p.ops[0]
	if op.kind != opDelete {
		t.Fatalf("op.kind = %d, want opDelete", op.kind)
	}
	if op.path != "old.txt" {
		t.Fatalf("op.path = %q, want old.txt", op.path)
	}
	if len(op.hunks) != 0 {
		t.Fatalf("hunks = %d, want 0", len(op.hunks))
	}
}

func TestParsePatchUpdateWithMove(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@ package main\n-func main() {}\n+func main() { run() }\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	op := p.ops[0]
	if op.kind != opUpdate {
		t.Fatalf("op.kind = %d, want opUpdate", op.kind)
	}
	if op.path != "old.go" {
		t.Fatalf("op.path = %q, want old.go", op.path)
	}
	if op.movePath != "new.go" {
		t.Fatalf("op.movePath = %q, want new.go", op.movePath)
	}
	if len(op.hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(op.hunks))
	}
}

func TestParsePatchMultipleOps(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+x\n*** Update File: b\n@@\n-c\n+d\n*** Delete File: c\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops) != 3 {
		t.Fatalf("ops = %d, want 3", len(p.ops))
	}
	if p.ops[0].kind != opAdd || p.ops[0].path != "a" {
		t.Fatalf("ops[0] = %+v, want Add a", p.ops[0])
	}
	if p.ops[1].kind != opUpdate || p.ops[1].path != "b" {
		t.Fatalf("ops[1] = %+v, want Update b", p.ops[1])
	}
	if p.ops[2].kind != opDelete || p.ops[2].path != "c" {
		t.Fatalf("ops[2] = %+v, want Delete c", p.ops[2])
	}
}

func TestParsePatchMultipleHunksOneFile(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@ first\n-old1\n+new1\n@@ second\n-old2\n+new2\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	op := p.ops[0]
	if len(op.hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(op.hunks))
	}
	if op.hunks[0].anchor != "first" || op.hunks[1].anchor != "second" {
		t.Fatalf("anchors = %q, %q, want first, second", op.hunks[0].anchor, op.hunks[1].anchor)
	}
}

func TestParsePatchPreservesPrefixCharactersInContent(t *testing.T) {
	// An Add line's content can start with `+`, `-`, or ` `; the parser must
	// strip exactly one prefix byte and preserve the rest verbatim.
	input := "*** Begin Patch\n*** Add File: a\n+++counter\n+--option\n+ indented\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops[0].hunks) != 3 {
		t.Fatalf("hunks = %d, want 3", len(p.ops[0].hunks))
	}
	want := []string{"++counter", "--option", " indented"}
	for i, h := range p.ops[0].hunks {
		if h.lines[0].kind != lineAdd {
			t.Fatalf("hunks[%d] kind = %d, want lineAdd", i, h.lines[0].kind)
		}
		if h.lines[0].text != want[i] {
			t.Fatalf("hunks[%d] text = %q, want %q", i, h.lines[0].text, want[i])
		}
	}
}

func TestParsePatchPreservesRawCR(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+line1\r\n+line2\r\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if len(p.ops[0].hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(p.ops[0].hunks))
	}
	if got := p.ops[0].hunks[0].lines[0].text; got != "line1\r" {
		t.Fatalf("hunks[0] text = %q, want %q (\\r must ride inside the line)", got, "line1\r")
	}
	if got := p.ops[0].hunks[1].lines[0].text; got != "line2\r" {
		t.Fatalf("hunks[1] text = %q, want %q", got, "line2\r")
	}
}

func TestParsePatchPreservesCRInUpdateHunk(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@\n ctx\r\n-old\r\n+new\r\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	h := p.ops[0].hunks[0]
	want := []string{"ctx\r", "old\r", "new\r"}
	if len(h.lines) != len(want) {
		t.Fatalf("hunk lines = %d, want %d", len(h.lines), len(want))
	}
	for i, l := range h.lines {
		if l.text != want[i] {
			t.Fatalf("hunk lines[%d] text = %q, want %q", i, l.text, want[i])
		}
	}
}

func TestParsePatchDuplicatePath(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+x\n*** Add File: a\n+y\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchDupPath) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchDupPath)
	}
	if !strings.Contains(err.Error(), "a") {
		t.Fatalf("err = %q, want it to mention the duplicated path", err)
	}
}

func TestParsePatchMoveDestDuplicatesExistingPath(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: b\n+z\n*** Update File: a\n*** Move to: b\n@@\n-x\n+y\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchDupPath) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchDupPath)
	}
}

func TestParsePatchAddFileBadBodyLine(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\nbad\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchAddBody) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchAddBody)
	}
}

func TestParsePatchAddFileEmptyBody(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n*** End Patch"
	_, err := parsePatch(input)
	if err == nil || !strings.Contains(err.Error(), "no content") {
		t.Fatalf("parsePatch err = %v, want a no-content error", err)
	}
}

func TestParsePatchUpdateNoHunks(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n*** End Patch"
	_, err := parsePatch(input)
	if err == nil || !strings.Contains(err.Error(), "no hunks") {
		t.Fatalf("parsePatch err = %v, want a no-hunks error", err)
	}
}

func TestParsePatchHunkBadLine(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@\nctx\nbad\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchHunkLine) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchHunkLine)
	}
}

func TestParsePatchHunkEmptyLine(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@\nctx\n\nmore\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchHunkLine) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchHunkLine)
	}
}

func TestParsePatchMissingHeaderBeforeHunk(t *testing.T) {
	input := "*** Begin Patch\n@@\nctx\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchHunkExpected) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchHunkExpected)
	}
}

func TestParsePatchUnknownHeader(t *testing.T) {
	input := "*** Begin Patch\n*** Rename File: a\n*** End Patch"
	_, err := parsePatch(input)
	if !errors.Is(err, errApplyPatchUnknownHead) {
		t.Fatalf("parsePatch err = %v, want %v", err, errApplyPatchUnknownHead)
	}
}

func TestParsePatchTrailingNewlineTolerated(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: a\n+x\n*** End Patch\n"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v, want nil (trailing newline tolerated)", err)
	}
	if len(p.ops) != 1 || p.ops[0].path != "a" {
		t.Fatalf("ops = %+v, want single Add of a", p.ops)
	}
}

func TestParsePatchAnchorsWithLeadingTrailingSpaces(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: a\n@@   spaced anchor  \n-x\n+y\n*** End Patch"
	p, err := parsePatch(input)
	if err != nil {
		t.Fatalf("parsePatch err = %v", err)
	}
	if got := p.ops[0].hunks[0].anchor; got != "spaced anchor" {
		t.Fatalf("anchor = %q, want %q (anchor should be trimmed)", got, "spaced anchor")
	}
}
