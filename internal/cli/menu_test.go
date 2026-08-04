package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestShowMenuDoesNotRedrawAtNavigationBounds(t *testing.T) {
	keys := []keyMsg{
		{Special: keyDown},
		{Special: keyUp},
		{Special: keyEscape},
	}
	next := 0
	readKey := func() (keyMsg, error) {
		k := keys[next]
		next++
		return k, nil
	}

	var out bytes.Buffer
	items := []menuItem{
		{label: "Turn 1", detail: `write 3 test files. all the .txt files, containing only "test"`, selectable: true},
	}

	result := showMenu(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "Revert - pick turn", items, 80)
	if result.selected != -1 {
		t.Fatalf("selected = %d, want cancel", result.selected)
	}
	if got := bytes.Count(out.Bytes(), []byte("Revert - pick turn")); got != 1 {
		t.Fatalf("menu rendered %d times, want 1\n%s", got, out.String())
	}
}

func TestShowMenuRedrawUsesRenderedLineCount(t *testing.T) {
	keys := []keyMsg{
		{Special: keyDown},
		{Special: keyEscape},
	}
	next := 0
	readKey := func() (keyMsg, error) {
		k := keys[next]
		next++
		return k, nil
	}

	var out bytes.Buffer
	items := []menuItem{
		{label: "Turn 1", selectable: true},
		{label: "Turn 2", selectable: true},
	}

	_ = showMenu(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "Revert - pick turn", items, 80)
	rendered := out.String()
	if strings.Contains(rendered, "\x1b7") || strings.Contains(rendered, "\x1b8") || strings.Contains(rendered, "\x1b[J") {
		t.Fatalf("menu redraw should not use cursor save/restore anchors:\n%q", rendered)
	}
	if got := strings.Count(rendered, "\x1b[1A"); got != 8 {
		t.Fatalf("cursor-up clears = %d, want 8 for redraw+exit of 5-row menu frame\n%q", got, rendered)
	}
}

func TestConfirmYNDistinguishesReadErrorFromNo(t *testing.T) {
	t.Run("enter_yes", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (keyMsg, error) { return keyMsg{Special: keyEnter}, nil }
		got, err := confirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		if err != nil {
			t.Fatalf("confirmYN err = %v, want nil", err)
		}
		if !got {
			t.Fatal("confirmYN = false, want true on Enter at Yes")
		}
	})

	t.Run("enter_no", func(t *testing.T) {
		keys := []keyMsg{{Special: keyDown}, {Special: keyEnter}}
		next := 0
		readKey := func() (keyMsg, error) {
			k := keys[next]
			next++
			return k, nil
		}
		var out bytes.Buffer
		got, err := confirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		if err != nil {
			t.Fatalf("confirmYN err = %v, want nil", err)
		}
		if got {
			t.Fatal("confirmYN = true, want false on Enter at No")
		}
	})

	t.Run("escape_is_no", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (keyMsg, error) { return keyMsg{Special: keyEscape}, nil }
		got, err := confirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		if err != nil {
			t.Fatalf("confirmYN err = %v, want nil", err)
		}
		if got {
			t.Fatal("confirmYN = true, want false on Escape")
		}
	})

	t.Run("read_error_reported", func(t *testing.T) {
		var out bytes.Buffer
		readKey := func() (keyMsg, error) { return keyMsg{}, ExitError{Code: 130} }
		got, err := confirmYN(&sync.Mutex{}, func(s string) { out.WriteString(s) }, readKey, "also revert code?", 80)
		if err == nil {
			t.Fatal("confirmYN err = nil, want the read error")
		}
		if got {
			t.Fatal("confirmYN = true, want false with the error")
		}
	})
}

func TestRenderMenuConstrainsLineWidths(t *testing.T) {
	width := 90
	rendered := renderMenu(
		"Very long menu title that should not wrap",
		[]menuItem{
			{label: "group label that should be truncated before wrapping", selectable: false},
			{label: "selectable label without detail that should be truncated before wrapping", selectable: true},
			{label: "Turn 1", detail: `write 3 test files. all the .txt files, containing only "test"`, selectable: true},
		},
		1,
		width,
		"   long footer hint that should also be truncated before wrapping",
	)

	lines := strings.Split(rendered, nl)
	if got, want := len(lines), menuRowsForItemCount(3); got != want {
		t.Fatalf("rendered rows = %d, want %d\n%s", got, want, rendered)
	}
	for _, line := range lines {
		if got := visibleWidth(line); got > menuLineWidth(width) {
			t.Fatalf("line width = %d, want <= %d: %q\n%s", got, menuLineWidth(width), line, rendered)
		}
	}
}

func TestRenderMenuNormalizesAndTruncatesMultilineText(t *testing.T) {
	rendered := renderMenu(
		"Revert\npick\tturn",
		[]menuItem{
			{label: "Turn\n1", detail: "first line\nsecond\tline\rthird line that is long enough to truncate", selectable: true},
		},
		0,
		50,
		"footer\nhint",
	)

	if strings.Contains(rendered, "\nsecond") || strings.Contains(rendered, "\rthird") || strings.Contains(rendered, "\t") {
		t.Fatalf("menu text was not normalized:\n%q", rendered)
	}
	if !strings.Contains(rendered, "first line second") {
		t.Fatalf("normalized detail missing from menu:\n%q", rendered)
	}
	if got, want := len(strings.Split(rendered, nl)), menuRowsForItemCount(1); got != want {
		t.Fatalf("rendered rows = %d, want %d\n%s", got, want, rendered)
	}
	for _, line := range strings.Split(rendered, nl) {
		if got := visibleWidth(line); got > menuLineWidth(50) {
			t.Fatalf("line width = %d, want <= %d: %q\n%s", got, menuLineWidth(50), line, rendered)
		}
	}
}

func TestMenuLineWidthKeepsWideTerminalsConservative(t *testing.T) {
	if got, want := menuLineWidth(90), 56; got != want {
		t.Fatalf("menuLineWidth(90) = %d, want %d", got, want)
	}
	rendered := renderMenu(
		"Revert - pick turn",
		[]menuItem{
			{label: "Turn 1", detail: `write 3 test files. all the .txt files, containing only "test"`, selectable: true},
		},
		0,
		90,
		defaultMenuFooter,
	)
	for _, line := range strings.Split(rendered, nl) {
		if got := visibleWidth(line); got > 56 {
			t.Fatalf("line width = %d, want <= 56: %q\n%s", got, line, rendered)
		}
	}
}

func TestRenderMenuTitleDoesNotUseFullWidthRule(t *testing.T) {
	rendered := renderMenu(
		"Revert - pick turn",
		[]menuItem{{label: "Turn 1", selectable: true}},
		0,
		90,
		defaultMenuFooter,
	)
	firstLine := strings.Split(rendered, nl)[0]
	if strings.Count(firstLine, "─") > 4 {
		t.Fatalf("title should not render a full-width rule: %q", firstLine)
	}
}
