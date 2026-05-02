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

func showMenu(mu *sync.Mutex, out func(string), keyCh chan keyMsg, readKeyFn func() (keyMsg, error), title string, items []menuItem, width int) menuResult {
	if width < 30 {
		width = 30
	}

	sel := 0
	for i, it := range items {
		if it.selectable {
			sel = i
			break
		}
	}

	lines := 0

	draw := func() {
		var b strings.Builder
		b.WriteString(colorDim)
		b.WriteString("── ")
		b.WriteString(title)
		b.WriteString(" ")
		b.WriteString(strings.Repeat("─", max(0, width-visibleWidth(title)-6)))
		b.WriteString(colorReset)
		b.WriteString(nl)

		for i, it := range items {
			if !it.selectable {
				b.WriteString(colorDim)
				b.WriteString("   ")
				b.WriteString(truncate(it.label, width-5))
				b.WriteString(colorReset)
				b.WriteString(nl)
				continue
			}

			if i == sel {
				b.WriteString(colorInv)
				b.WriteString("❯ ")
			} else {
				b.WriteString("  ")
			}

			if it.current {
				b.WriteString(colorCyan)
				b.WriteString("● ")
				b.WriteString(colorReset)
				if i == sel {
					b.WriteString(colorInv)
				}
			}

			prefixW := 2
			if it.current {
				prefixW = 4
			}
			avail := width - prefixW - 2

			labelW := visibleWidth(it.label)
			detailW := 0
			if it.detail != "" {
				detailW = visibleWidth(it.detail) + 2
			}

			if labelW+detailW > avail && detailW > 0 {
				maxDetail := avail - labelW - 2
				if maxDetail < 3 {
					b.WriteString(truncate(it.label, avail-3))
				} else {
					b.WriteString(it.label)
					b.WriteString(colorDim)
					b.WriteString("  ")
					b.WriteString(truncate(it.detail, maxDetail))
					b.WriteString(colorReset)
					if i == sel {
						b.WriteString(colorInv)
					}
				}
			} else {
				b.WriteString(it.label)
				if it.detail != "" {
					b.WriteString(colorDim)
					b.WriteString("  ")
					b.WriteString(it.detail)
					b.WriteString(colorReset)
					if i == sel {
						b.WriteString(colorInv)
					}
				}
			}

			if i == sel {
				b.WriteString(colorReset)
			}
			b.WriteString(nl)
		}

		b.WriteString(colorDim)
		b.WriteString("   ↑↓ navigate  Enter select  Esc cancel")
		b.WriteString(colorReset)

		mu.Lock()
		out(eraseBlock(lines))
		out(b.String())
		mu.Unlock()

		lines = len(items) + 2
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
			out(eraseBlock(lines))
			mu.Unlock()
			return menuResult{selected: -1}

		case keyEnter:
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				mu.Lock()
				out(eraseBlock(lines))
				mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra}
			}

		case keyUp:
			for i := sel - 1; i >= 0; i-- {
				if items[i].selectable {
					sel = i
					break
				}
			}
			draw()

		case keyDown:
			for i := sel + 1; i < len(items); i++ {
				if items[i].selectable {
					sel = i
					break
				}
			}
			draw()
		}
	}
}

func confirmYN(mu *sync.Mutex, out func(string), readKeyFn func() (keyMsg, error), question string) bool {
	mu.Lock()
	out(colorDim + "  " + question + " (y/n)" + colorReset + nl)
	mu.Unlock()

	for {
		k, err := readKeyFn()
		if err != nil {
			return false
		}
		switch k.Rune {
		case 'y', 'Y':
			return true
		case 'n', 'N':
			return false
		}
		switch k.Special {
		case keyEscape, keyCtrlC:
			return false
		}
	}
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

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Model", items, c.width)
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
	if c.width < 30 {
		c.width = 30
	}

	sel := 0
	for i, it := range items {
		if it.selectable {
			sel = i
			break
		}
	}

	lines := 0

	draw := func() {
		var b strings.Builder
		b.WriteString(colorDim)
		b.WriteString("── ")
		b.WriteString(title)
		b.WriteString(" ")
		b.WriteString(strings.Repeat("─", max(0, c.width-visibleWidth(title)-6)))
		b.WriteString(colorReset)
		b.WriteString(nl)

		for i, it := range items {
			if !it.selectable {
				b.WriteString(colorDim)
				b.WriteString("   ")
				b.WriteString(truncate(it.label, c.width-5))
				b.WriteString(colorReset)
				b.WriteString(nl)
				continue
			}

			if i == sel {
				b.WriteString(colorInv)
				b.WriteString("❯ ")
			} else {
				b.WriteString("  ")
			}

			if it.current {
				b.WriteString(colorCyan)
				b.WriteString("● ")
				b.WriteString(colorReset)
				if i == sel {
					b.WriteString(colorInv)
				}
			}

			prefixW := 2
			if it.current {
				prefixW = 4
			}
			avail := c.width - prefixW - 2

			labelW := visibleWidth(it.label)
			detailW := 0
			if it.detail != "" {
				detailW = visibleWidth(it.detail) + 2
			}

			if labelW+detailW > avail && detailW > 0 {
				maxDetail := avail - labelW - 2
				if maxDetail < 3 {
					b.WriteString(truncate(it.label, avail-3))
				} else {
					b.WriteString(it.label)
					b.WriteString(colorDim)
					b.WriteString("  ")
					b.WriteString(truncate(it.detail, maxDetail))
					b.WriteString(colorReset)
					if i == sel {
						b.WriteString(colorInv)
					}
				}
			} else {
				b.WriteString(it.label)
				if it.detail != "" {
					b.WriteString(colorDim)
					b.WriteString("  ")
					b.WriteString(it.detail)
					b.WriteString(colorReset)
					if i == sel {
						b.WriteString(colorInv)
					}
				}
			}

			if i == sel {
				b.WriteString(colorReset)
			}
			b.WriteString(nl)
		}

		tabHint := "Tab: archived"
		if state == "archived" {
			tabHint = "Tab: active"
		}
		b.WriteString(colorDim)
		b.WriteString("   ↑↓ navigate  Enter select  n:new  a:archive  d:delete  ")
		b.WriteString(tabHint)
		b.WriteString("  Esc cancel")
		b.WriteString(colorReset)

		c.mu.Lock()
		c.writeRaw(eraseBlock(lines))
		c.writeRaw(b.String())
		c.mu.Unlock()

		lines = len(items) + 2
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
			c.writeRaw(eraseBlock(lines))
			c.mu.Unlock()
			return menuResult{selected: -1}

		case keyEnter:
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				c.writeRaw(eraseBlock(lines))
				c.mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra, action: "select"}
			}

		case keyUp:
			for i := sel - 1; i >= 0; i-- {
				if items[i].selectable {
					sel = i
					break
				}
			}
			draw()

		case keyDown:
			for i := sel + 1; i < len(items); i++ {
				if items[i].selectable {
					sel = i
					break
				}
			}
			draw()

		case keyTab:
			c.mu.Lock()
			c.writeRaw(eraseBlock(lines))
			c.mu.Unlock()
			return menuResult{action: "toggle"}
		}

		switch k.Rune {
		case 'n':
			c.mu.Lock()
			c.writeRaw(eraseBlock(lines))
			c.mu.Unlock()
			return menuResult{action: "new"}
		case 'a':
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				c.writeRaw(eraseBlock(lines))
				c.mu.Unlock()
				return menuResult{selected: sel, extra: items[sel].extra, action: "archive"}
			}
		case 'd':
			if sel >= 0 && sel < len(items) && items[sel].selectable {
				c.mu.Lock()
				c.writeRaw(eraseBlock(lines))
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

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Project", items, c.width)
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
			content := m.Content
			if len(content) > 60 {
				content = content[:57] + "..."
			}
			turns = append(turns, userTurn{turn: m.Turn, content: content})
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

	result := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, "Revert — pick turn", items, c.width)
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

	actionResult := showMenu(c.mu, c.writeRaw, c.keyCh, c.readKeyFn, fmt.Sprintf("Turn %d — action", turn), actionItems, c.width)
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
		alsoCode := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?")
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
		alsoCode := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?")
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
