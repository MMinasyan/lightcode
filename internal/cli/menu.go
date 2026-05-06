package cli

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type menuItem struct {
	label      string
	detail     string
	selectable bool
	current    bool
	group      string
	extra      any
}

type menuResult struct {
	selected int
	extra    any
	action   string
}

const defaultMenuFooter = "   ↑↓ navigate  Enter select  Esc cancel"

type transientMenuFrame struct {
	rows int
}

func (f *transientMenuFrame) draw(out func(string), rendered string, width int) {
	if f.rows > 0 {
		out(eraseBlock(f.rows))
	} else {
		out("\r\x1b[2K")
	}
	out(rendered)
	out(nl)
	f.rows = renderedTerminalRows(rendered, width) + 1
}

func (f *transientMenuFrame) clear(out func(string)) {
	if f.rows <= 0 {
		return
	}
	out(eraseBlock(f.rows))
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
		w := visibleWidth(line)
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
	return truncate(normalizeMenuText(s), maxW)
}

func renderMenu(title string, items []menuItem, selected int, width int, footer string) string {
	if width < 30 {
		width = 30
	}
	if footer == "" {
		footer = defaultMenuFooter
	}
	if selected < 0 || selected >= len(items) || !items[selected].selectable {
		selected = firstSelectableMenuItem(items)
	}
	lineWidth := menuLineWidth(width)
	var b strings.Builder
	b.WriteString(colorDim)
	b.WriteString(menuDisplayText("── "+title+" ──", lineWidth))
	b.WriteString(colorReset)
	b.WriteString(nl)

	for i, it := range items {
		label := normalizeMenuText(it.label)
		detail := normalizeMenuText(it.detail)

		if !it.selectable {
			b.WriteString(colorDim)
			b.WriteString("   ")
			b.WriteString(truncate(label, lineWidth-3))
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

		if it.current {
			b.WriteString(colorCyan)
			b.WriteString("● ")
			b.WriteString(colorReset)
			if i == selected {
				b.WriteString(colorInv)
			}
		}

		prefixW := 2
		if it.current {
			prefixW = 4
		}
		avail := lineWidth - prefixW

		labelW := visibleWidth(label)
		detailW := 0
		if detail != "" && labelW < avail-2 {
			detail = truncate(detail, avail-labelW-2)
			detailW = visibleWidth(detail) + 2
		} else {
			detail = ""
		}

		if labelW+detailW > avail && detailW > 0 {
			maxDetail := avail - labelW - 2
			if maxDetail < 3 || labelW > avail-2 {
				b.WriteString(truncate(label, avail))
			} else {
				b.WriteString(label)
				b.WriteString(colorDim)
				b.WriteString("  ")
				b.WriteString(truncate(detail, maxDetail))
				b.WriteString(colorReset)
				if i == selected {
					b.WriteString(colorInv)
				}
			}
		} else {
			b.WriteString(truncate(label, avail))
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

func menuRows(items []menuItem) int {
	return menuRowsForItemCount(len(items))
}

func menuRowsForItemCount(itemCount int) int {
	return itemCount + 2
}

func firstSelectableMenuItem(items []menuItem) int {
	for i, it := range items {
		if it.selectable {
			return i
		}
	}
	return -1
}

func showMenu(mu *sync.Mutex, out func(string), keyCh chan keyMsg, readKeyFn func() (keyMsg, error), title string, items []menuItem, width int) menuResult {
	if width < 30 {
		width = 30
	}

	sel := firstSelectableMenuItem(items)

	var frame transientMenuFrame

	draw := func() {
		mu.Lock()
		frame.draw(out, renderMenu(title, items, sel, width, defaultMenuFooter), width)
		mu.Unlock()
	}

	draw()

	for {
		k, err := readKeyFn()
		if err != nil {
			return menuResult{selected: -1}
		}

		switch k.Special {
		case keyEscape, keyCtrlC:
			mu.Lock()
			frame.clear(out)
			mu.Unlock()
			return menuResult{selected: -1}

		case keyEnter:
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				mu.Lock()
				frame.clear(out)
				mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra}
			}

		case keyUp:
			moved := false
			for i := sel - 1; i >= 0; i-- {
				if items[i].selectable {
					sel = i
					moved = true
					break
				}
			}
			if moved {
				draw()
			}

		case keyDown:
			moved := false
			for i := sel + 1; i < len(items); i++ {
				if items[i].selectable {
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

func confirmYN(mu *sync.Mutex, out func(string), readKeyFn func() (keyMsg, error), question string, width int) bool {
	items := []menuItem{
		{label: "Yes", selectable: true},
		{label: "No", selectable: true},
	}
	result := showMenu(mu, out, nil, readKeyFn, question, items, width)
	return result.selected == 0
}

func (c *CLI) showModelMenu() {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	models := c.agent.ModelList()
	if len(models) == 0 {
		c.printLine(renderErrorMsg("no models configured"))
		return
	}

	cur := c.agent.CurrentModel()

	var items []menuItem
	for _, pm := range models {
		items = append(items, menuItem{label: pm.Provider, selectable: false})
		for _, m := range pm.Models {
			items = append(items, menuItem{
				label:      m,
				selectable: true,
				current:    pm.Provider == cur.Provider && m == cur.Model,
				group:      pm.Provider,
				extra:      modelChoice{provider: pm.Provider, model: m},
			})
		}
	}

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Model", items, c.currentWidth())
	if result.selected >= 0 {
		choice := result.extra.(modelChoice)
		if err := c.agent.SwitchModel(choice.provider, choice.model); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.provider = choice.provider
		c.model = choice.model
		c.printLine(renderSystemMsg(fmt.Sprintf("  model switched to %s (%s)", choice.model, choice.provider)))
	}
}

type modelChoice struct {
	provider string
	model    string
}

func (c *CLI) showSessionMenu() {
	c.showSessionMenuInner("active")
}

func (c *CLI) showSessionMenuInner(state string) {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	sessions, err := c.agent.SessionList(state)
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}

	cur := c.agent.SessionCurrent()

	var items []menuItem
	if len(sessions) == 0 {
		items = append(items, menuItem{label: "(no sessions)", selectable: false})
	} else {
		for _, s := range sessions {
			detail := time.Unix(s.LastActivity, 0).Format("Jan 02 15:04")
			items = append(items, menuItem{
				label:      s.ID,
				detail:     detail,
				selectable: true,
				current:    s.ID == cur.ID,
				extra:      s.ID,
			})
		}
	}

	title := "Sessions"
	if state == "archived" {
		title = "Archived Sessions"
	}

	result := c.showSessionMenuWithActions(title, items, state)
	switch result.action {
	case "select":
		if result.selected >= 0 {
			id := result.extra.(string)
			if err := c.agent.SessionSwitch(id); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.refreshSession()
		}
	case "new":
		if err := c.agent.SessionNew(); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.refreshSession()
	case "archive":
		if result.selected >= 0 {
			id := result.extra.(string)
			closedCurrent, err := c.agent.SessionArchive(id)
			if err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.printLine(renderSystemMsg("  session archived"))
			if closedCurrent {
				c.refreshSession()
			}
		}
	case "delete":
		if result.selected >= 0 {
			id := result.extra.(string)
			closedCurrent, err := c.agent.SessionDelete(id)
			if err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.printLine(renderSystemMsg("  session deleted"))
			if closedCurrent {
				c.refreshSession()
			}
		}
	case "toggle":
		if state == "active" {
			c.showSessionMenuInner("archived")
		} else {
			c.showSessionMenuInner("active")
		}
	}
}

func (c *CLI) showSessionMenuWithActions(title string, items []menuItem, state string) menuResult {
	sel := firstSelectableMenuItem(items)

	var frame transientMenuFrame

	draw := func() {
		width := c.currentWidth()
		tabHint := "Tab: archived"
		if state == "archived" {
			tabHint = "Tab: active"
		}
		footer := "   ↑↓ navigate  Enter select  n:new  a:archive  d:delete  " + tabHint + "  Esc cancel"

		c.mu.Lock()
		frame.draw(c.writeRaw, renderMenu(title, items, sel, width, footer), width)
		c.mu.Unlock()
	}

	draw()

	for {
		k, err := c.readKeyFn()
		if err != nil {
			return menuResult{selected: -1}
		}

		switch k.Special {
		case keyEscape, keyCtrlC:
			c.mu.Lock()
			frame.clear(c.writeRaw)
			c.mu.Unlock()
			return menuResult{selected: -1}

		case keyEnter:
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				frame.clear(c.writeRaw)
				c.mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra, action: "select"}
			}

		case keyUp:
			moved := false
			for i := sel - 1; i >= 0; i-- {
				if items[i].selectable {
					sel = i
					moved = true
					break
				}
			}
			if moved {
				draw()
			}

		case keyDown:
			moved := false
			for i := sel + 1; i < len(items); i++ {
				if items[i].selectable {
					sel = i
					moved = true
					break
				}
			}
			if moved {
				draw()
			}

		case keyTab:
			c.mu.Lock()
			frame.clear(c.writeRaw)
			c.mu.Unlock()
			return menuResult{action: "toggle"}
		}

		switch k.Rune {
		case 'n':
			c.mu.Lock()
			frame.clear(c.writeRaw)
			c.mu.Unlock()
			return menuResult{action: "new"}
		case 'a':
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				frame.clear(c.writeRaw)
				c.mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra, action: "archive"}
			}
		case 'd':
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				frame.clear(c.writeRaw)
				c.mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra, action: "delete"}
			}
		}
	}
}

func (c *CLI) showProjectMenu() {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	projects, err := c.agent.ProjectList()
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}

	cur := c.agent.ProjectCurrent()

	var items []menuItem
	for _, p := range projects {
		detail := p.Path
		if len(detail) > 40 {
			detail = "..." + detail[len(detail)-37:]
		}
		items = append(items, menuItem{
			label:      p.Name,
			detail:     detail,
			selectable: true,
			current:    p.ID == cur.ID,
			extra:      p.Path,
		})
	}

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Project", items, c.currentWidth())
	if result.selected >= 0 {
		path := result.extra.(string)
		c.printLine(renderSystemMsg("  switching to " + path + "..."))
		c.projectSwitch(path)
	}
}

func (c *CLI) showRevertMenu() {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	msgs := c.agent.SessionMessages()
	if len(msgs) == 0 {
		c.printLine(renderErrorMsg("no messages to revert"))
		return
	}

	type userTurn struct {
		turn    int
		content string
	}
	var turns []userTurn
	for _, m := range msgs {
		if m.Type == "user" && m.Turn > 0 {
			turns = append(turns, userTurn{turn: m.Turn, content: m.Content})
		}
	}

	if len(turns) == 0 {
		c.printLine(renderErrorMsg("no user turns to revert"))
		return
	}

	var items []menuItem
	for _, t := range turns {
		items = append(items, menuItem{
			label:      fmt.Sprintf("Turn %d", t.turn),
			detail:     t.content,
			selectable: true,
			extra:      t.turn,
		})
	}

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Revert — pick turn", items, c.currentWidth())
	if result.selected < 0 {
		return
	}
	turn := result.extra.(int)

	actionItems := []menuItem{
		{label: "Revert code", detail: "restore files, keep history", selectable: true, extra: "code"},
		{label: "Revert history", detail: "truncate conversation", selectable: true, extra: "history"},
		{label: "Fork from here", detail: "new session from this point", selectable: true, extra: "fork"},
		{label: "Back", selectable: true, extra: "back"},
	}

	actionResult := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, fmt.Sprintf("Turn %d — action", turn), actionItems, c.currentWidth())
	if actionResult.selected < 0 {
		return
	}

	action := actionResult.extra.(string)
	switch action {
	case "code":
		if err := c.agent.RevertCode(turn - 1); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.printLine(renderSystemMsg(fmt.Sprintf("  reverted code to before turn %d", turn)))

	case "history":
		alsoCode := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?", c.currentWidth())
		if alsoCode {
			if err := c.agent.RevertCode(turn - 1); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
		}
		if err := c.agent.RevertHistory(turn - 1); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.refreshSession()

	case "fork":
		alsoCode := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?", c.currentWidth())
		if alsoCode {
			if err := c.agent.RevertCode(turn); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
		}
		if err := c.agent.ForkSession(turn); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.refreshSession()

	case "back":
	}
}
