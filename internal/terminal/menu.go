package terminal

import (
	"strings"
	"sync"
)

type MenuItem struct {
	Label      string
	Detail     string
	Selectable bool
	Current    bool
	Group      string
	Extra      any
}

type MenuResult struct {
	Selected int
	Extra    any
	Action   string
}

const defaultMenuFooter = "   ↑↓ navigate  Enter select  Esc cancel"

type TransientMenuFrame struct {
	rows int
}

func (f *TransientMenuFrame) Draw(out func(string), rendered string, width int) {
	if f.rows > 0 {
		out(EraseBlock(f.rows))
	} else {
		out("\r\x1b[2K")
	}
	out(rendered)
	out(nl)
	f.rows = renderedTerminalRows(rendered, width) + 1
}

func (f *TransientMenuFrame) Clear(out func(string)) {
	if f.rows <= 0 {
		return
	}
	out(EraseBlock(f.rows))
	f.rows = 0
}

func menuLineWidth(width int) int {
	if width < 30 {
		width = 30
	}
	lineWidth := width - 12
	if lineWidth > 56 {
		lineWidth = 56
	}
	if lineWidth < 20 {
		lineWidth = width - 2
	}
	return lineWidth
}

func renderedTerminalRows(rendered string, width int) int {
	lineWidth := menuLineWidth(width)
	rows := 0
	for _, line := range strings.Split(rendered, nl) {
		w := VisibleWidth(line)
		if w == 0 {
			rows++
			continue
		}
		rows += (w + lineWidth - 1) / lineWidth
	}
	if rows == 0 {
		return 1
	}
	return rows
}

func normalizeMenuText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func menuDisplayText(s string, maxW int) string {
	return Truncate(normalizeMenuText(s), maxW)
}

func RenderMenu(title string, items []MenuItem, selected int, width int, footer string) string {
	if width < 30 {
		width = 30
	}
	if footer == "" {
		footer = defaultMenuFooter
	}
	if selected < 0 || selected >= len(items) || !items[selected].Selectable {
		selected = FirstSelectableMenuItem(items)
	}
	lineWidth := menuLineWidth(width)
	var b strings.Builder
	b.WriteString(colorDim)
	b.WriteString(menuDisplayText("── "+title+" ──", lineWidth))
	b.WriteString(colorReset)
	b.WriteString(nl)

	for i, it := range items {
		label := normalizeMenuText(it.Label)
		detail := normalizeMenuText(it.Detail)

		if !it.Selectable {
			b.WriteString(colorDim)
			b.WriteString("   ")
			b.WriteString(Truncate(label, lineWidth-3))
			b.WriteString(colorReset)
			b.WriteString(nl)
			continue
		}

		if i == selected {
			b.WriteString(colorInv)
			b.WriteString("❯ ")
		} else {
			b.WriteString("  ")
		}

		if it.Current {
			b.WriteString(colorCyan)
			b.WriteString("● ")
			b.WriteString(colorReset)
			if i == selected {
				b.WriteString(colorInv)
			}
		}

		prefixW := 2
		if it.Current {
			prefixW = 4
		}
		avail := lineWidth - prefixW

		labelW := VisibleWidth(label)
		detailW := 0
		if detail != "" && labelW < avail-2 {
			detail = Truncate(detail, avail-labelW-2)
			detailW = VisibleWidth(detail) + 2
		} else {
			detail = ""
		}

		if labelW+detailW > avail && detailW > 0 {
			maxDetail := avail - labelW - 2
			if maxDetail < 3 || labelW > avail-2 {
				b.WriteString(Truncate(label, avail))
			} else {
				b.WriteString(label)
				b.WriteString(colorDim)
				b.WriteString("  ")
				b.WriteString(Truncate(detail, maxDetail))
				b.WriteString(colorReset)
				if i == selected {
					b.WriteString(colorInv)
				}
			}
		} else {
			b.WriteString(Truncate(label, avail))
			if detail != "" {
				b.WriteString(colorDim)
				b.WriteString("  ")
				b.WriteString(detail)
				b.WriteString(colorReset)
				if i == selected {
					b.WriteString(colorInv)
				}
			}
		}

		if i == selected {
			b.WriteString(colorReset)
		}
		b.WriteString(nl)
	}

	b.WriteString(colorDim)
	b.WriteString(menuDisplayText(footer, lineWidth))
	b.WriteString(colorReset)

	return b.String()
}

func FirstSelectableMenuItem(items []MenuItem) int {
	for i, it := range items {
		if it.Selectable {
			return i
		}
	}
	return -1
}

func ShowMenu(mu *sync.Mutex, out func(string), readKeyFn func() (KeyMsg, error), title string, items []MenuItem, width int) MenuResult {
	if width < 30 {
		width = 30
	}

	sel := FirstSelectableMenuItem(items)

	var frame TransientMenuFrame

	draw := func() {
		mu.Lock()
		frame.Draw(out, RenderMenu(title, items, sel, width, defaultMenuFooter), width)
		mu.Unlock()
	}

	draw()

	for {
		k, err := readKeyFn()
		if err != nil {
			return MenuResult{Selected: -1}
		}

		switch k.Special {
		case KeyEscape, KeyCtrlC:
			mu.Lock()
			frame.Clear(out)
			mu.Unlock()
			return MenuResult{Selected: -1}

		case KeyEnter:
			if sel >= 0 && sel < len(items) && items[sel].Selectable {
				mu.Lock()
				frame.Clear(out)
				mu.Unlock()
				return MenuResult{Selected: sel, Extra: items[sel].Extra}
			}

		case KeyUp:
			moved := false
			for i := sel - 1; i >= 0; i-- {
				if items[i].Selectable {
					sel = i
					moved = true
					break
				}
			}
			if moved {
				draw()
			}

		case KeyDown:
			moved := false
			for i := sel + 1; i < len(items); i++ {
				if items[i].Selectable {
					sel = i
					moved = true
					break
				}
			}
			if moved {
				draw()
			}
		}
	}
}

// ConfirmYN asks a Yes/No question and reports the choice, distinguishing a
// deliberate No from a failed key read: a read error means the terminal is
// exiting, and the caller must abort rather than treat it as an answer.
// Escape cancels as No, exactly as ShowMenu's cancel does. It reads keys
// itself because ShowMenu collapses a read error into selected -1, which
// would be indistinguishable from No.
func ConfirmYN(mu *sync.Mutex, out func(string), readKeyFn func() (KeyMsg, error), question string, width int) (bool, error) {
	items := []MenuItem{
		{Label: "Yes", Selectable: true},
		{Label: "No", Selectable: true},
	}
	if width < 30 {
		width = 30
	}
	sel := 0
	var frame TransientMenuFrame
	draw := func() {
		mu.Lock()
		frame.Draw(out, RenderMenu(question, items, sel, width, defaultMenuFooter), width)
		mu.Unlock()
	}
	draw()
	for {
		k, err := readKeyFn()
		if err != nil {
			return false, err
		}
		switch k.Special {
		case KeyEscape, KeyCtrlC:
			mu.Lock()
			frame.Clear(out)
			mu.Unlock()
			return false, nil
		case KeyEnter:
			mu.Lock()
			frame.Clear(out)
			mu.Unlock()
			return sel == 0, nil
		case KeyUp:
			if sel > 0 {
				sel--
				draw()
			}
		case KeyDown:
			if sel < len(items)-1 {
				sel++
				draw()
			}
		}
	}
}
