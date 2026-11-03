package editpreview

import (
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestBuildReplacementAndLaterInsertion(t *testing.T) {
	oldString := "func send() {\n\toldA()\n\toldB()\n\toldC()\n\toldD()\n\toldE()\n\tkeepOne()\n\tkeepTwo()\n}"
	newString := "func send() {\n\tnewA()\n\tnewB()\n\tkeepOne()\n\tkeepTwo()\n\taddedA()\n\taddedB()\n\taddedC()\n}"
	preview := Build(oldString, newString, "Edited x.go (1 replacement, lines 84-92).")
	testassert.Require(t, preview != nil && len(preview.Hunks) == 1, "expected one hunk, got %#v", preview)

	got := renderPlain(preview)
	want := []string{
		"84  func send() {",
		"85 -\toldA()",
		"86 -\toldB()",
		"87 -\toldC()",
		"88 -\toldD()",
		"89 -\toldE()",
		"85 +\tnewA()",
		"86 +\tnewB()",
		"87  \tkeepOne()",
		"88  \tkeepTwo()",
		"89 +\taddedA()",
		"90 +\taddedB()",
		"91 +\taddedC()",
		"92  }",
	}
	testassert.Require(t, len(got) == len(want), "expected %d rows, got %d:\n%v", len(want), len(got), got)
	for i := range want {
		testassert.Require(t, got[i] == want[i], "row %d mismatch\nwant: %q\n got: %q", i, want[i], got[i])
	}
}

func TestBuildMultipleLineRanges(t *testing.T) {
	preview := Build("old", "new", "Edited x.go (2 replacements, lines 12-12, 40-40).")
	testassert.Require(t, preview != nil && len(preview.Hunks) == 2, "expected two hunks, got %#v", preview)
	testassert.Require(t, preview.Hunks[0].Rows[0].OldLine == 12 && preview.Hunks[1].Rows[0].OldLine == 40, "unexpected start lines: %#v", preview.Hunks)
}

func TestParseStartLinesMatchesEditFileSummaryFormat(t *testing.T) {
	starts := parseStartLines("Edited x.go (3 replacements, lines 12-15, 42-83, 201-205).")
	want := []int{12, 42, 201}
	testassert.Require(t, len(starts) == len(want), "expected %d starts, got %d: %#v", len(want), len(starts), starts)
	for i := range want {
		testassert.Require(t, starts[i] == want[i], "start %d = %d, want %d", i, starts[i], want[i])
	}
}

func renderPlain(preview *Preview) []string {
	width := 1
	for _, hunk := range preview.Hunks {
		for _, row := range hunk.Rows {
			if n := len(fmt.Sprintf("%d", DisplayLine(row))); n > width {
				width = n
			}
		}
	}
	var lines []string
	for _, hunk := range preview.Hunks {
		for _, row := range hunk.Rows {
			lines = append(lines, fmt.Sprintf("%*d %s%s", width, DisplayLine(row), Marker(row), row.Text))
		}
	}
	return lines
}
