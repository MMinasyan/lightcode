package terminal

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestShowMenuDoesNotRedrawAtNavigationBounds(t *testing.T) {
	keys := []KeyMsg{
		{Special: KeyDown},
		{Special: KeyUp},
		{Special: KeyEscape},
	}
	next := 0
	readKey := func() (KeyMsg, error) {
		k := keys[next]
		next++
		return k, nil
	}

	var out bytes.Buffer
	items := []MenuItem{
		{Label: "Turn 1", Detail: `write 3 test files. all the .txt files, containing only "test"`, Selectable: true},
	}

	result := ShowMenu(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "Revert - pick turn", items, 80)
	testassert.Equal(t, result.Selected, -1)
	if got := bytes.Count(out.Bytes(), []byte("Revert - pick turn")); got != 1 {
		t.Fatalf("menu rendered %d times, want 1\n%s", got, out.String())
	}
}

func TestShowMenuRedrawUsesRenderedLineCount(t *testing.T) {
	keys := []KeyMsg{
		{Special: KeyDown},
		{Special: KeyEscape},
	}
	next := 0
	readKey := func() (KeyMsg, error) {
		k := keys[next]
		next++
		return k, nil
	}

	var out bytes.Buffer
	items := []MenuItem{
		{Label: "Turn 1", Selectable: true},
		{Label: "Turn 2", Selectable: true},
	}

	_ = ShowMenu(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "Revert - pick turn", items, 80)
	rendered := out.String()
	testassert.Require(t, !strings.Contains(rendered, "\x1b7") && !strings.Contains(rendered, "\x1b8") && !strings.Contains(rendered, "\x1b[J"), "menu redraw should not use cursor save/restore anchors:\n%q", rendered)
	{
		got := strings.Count(rendered, "\x1b[1A")
		testassert.Require(t, got == 8, "cursor-up clears = %d, want 8 for redraw+exit of 5-row menu frame\n%q", got, rendered)
	}
}

func TestConfirmYNDistinguishesReadErrorFromNo(t *testing.T) {
	t.Run("enter_yes", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (KeyMsg, error) { return KeyMsg{Special: KeyEnter}, nil }
		got, err := ConfirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		testassert.NoError(t, err)
		testassert.Require(t, got, "ConfirmYN = false, want true on Enter at Yes")
	})

	t.Run("enter_no", func(t *testing.T) {
		keys := []KeyMsg{{Special: KeyDown}, {Special: KeyEnter}}
		next := 0
		readKey := func() (KeyMsg, error) {
			k := keys[next]
			next++
			return k, nil
		}
		var out bytes.Buffer
		got, err := ConfirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		testassert.NoError(t, err)
		testassert.Require(t, !got, "ConfirmYN = true, want false on Enter at No")
	})

	t.Run("escape_is_no", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (KeyMsg, error) { return KeyMsg{Special: KeyEscape}, nil }
		got, err := ConfirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		testassert.NoError(t, err)
		testassert.Require(t, !got, "ConfirmYN = true, want false on Escape")
	})

	t.Run("read_error_reported", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (KeyMsg, error) { return KeyMsg{}, errors.New("read failed") }
		got, err := ConfirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		testassert.Require(t, err != nil, "ConfirmYN err = nil, want the read error")
		testassert.Require(t, !got, "ConfirmYN = true, want false with the error")
	})
}

func TestRenderMenuConstrainsLineWidths(t *testing.T) {
	width := 90
	rendered := RenderMenu(
		"Very long menu title that should not wrap",
		[]MenuItem{
			{Label: "group label that should be truncated before wrapping", Selectable: false},
			{Label: "selectable label without detail that should be truncated before wrapping", Selectable: true},
			{Label: "Turn 1", Detail: `write 3 test files. all the .txt files, containing only "test"`, Selectable: true},
		},
		1,
		width,
		"   long footer hint that should also be truncated before wrapping",
	)

	lines := strings.Split(rendered, nl)
	// 3 items plus the title and footer rows.
	{
		got, want := len(lines), 5
		testassert.Require(t, got == want, "rendered rows = %d, want %d\n%s", got, want, rendered)
	}
	for _, line := range lines {
		if got := VisibleWidth(line); got > menuLineWidth(width) {
			t.Fatalf("line width = %d, want <= %d: %q\n%s", got, menuLineWidth(width), line, rendered)
		}
	}
}

func TestRenderMenuNormalizesAndTruncatesMultilineText(t *testing.T) {
	rendered := RenderMenu(
		"Revert\npick\tturn",
		[]MenuItem{
			{Label: "Turn\n1", Detail: "first line\nsecond\tline\rthird line that is long enough to truncate", Selectable: true},
		},
		0,
		50,
		"footer\nhint",
	)

	testassert.Require(t, !strings.Contains(rendered, "\nsecond") && !strings.Contains(rendered, "\rthird") && !strings.Contains(rendered, "\t") && strings.Contains(rendered, "first line second"), "menu text was not normalized:\n%q; normalized detail missing from menu:\n%q", rendered, rendered)
	// 1 item plus the title and footer rows.
	{
		got, want := len(strings.Split(rendered, nl)), 3
		testassert.Require(t, got == want, "rendered rows = %d, want %d\n%s", got, want, rendered)
	}
	for _, line := range strings.Split(rendered, nl) {
		if got := VisibleWidth(line); got > menuLineWidth(50) {
			t.Fatalf("line width = %d, want <= %d: %q\n%s", got, menuLineWidth(50), line, rendered)
		}
	}
}

func TestMenuLineWidthKeepsWideTerminalsConservative(t *testing.T) {
	{
		got, want := menuLineWidth(90), 56
		testassert.Equal(t, got, want)
	}
	rendered := RenderMenu(
		"Revert - pick turn",
		[]MenuItem{
			{Label: "Turn 1", Detail: `write 3 test files. all the .txt files, containing only "test"`, Selectable: true},
		},
		0,
		90,
		defaultMenuFooter,
	)
	for _, line := range strings.Split(rendered, nl) {
		{
			got := VisibleWidth(line)
			testassert.Require(t, got <= 56, "line width = %d, want <= 56: %q\n%s", got, line, rendered)
		}
	}
}

func TestRenderMenuTitleDoesNotUseFullWidthRule(t *testing.T) {
	rendered := RenderMenu(
		"Revert - pick turn",
		[]MenuItem{{Label: "Turn 1", Selectable: true}},
		0,
		90,
		defaultMenuFooter,
	)
	firstLine := strings.Split(rendered, nl)[0]
	testassert.Require(t, strings.Count(firstLine, "─") <= 4, "title should not render a full-width rule: %q", firstLine)
}
