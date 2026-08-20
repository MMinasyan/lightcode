package cli

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/snapshot"
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

func showMenu(mu *sync.Mutex, out func(string), readKeyFn func() (keyMsg, error), title string, items []menuItem, width int) menuResult {
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

// confirmYN asks a Yes/No question and reports the choice, distinguishing a
// deliberate No from a failed key read: a read error means the terminal is
// exiting, and the caller must abort rather than treat it as an answer.
// Escape cancels as No, exactly as showMenu's cancel does. It reads keys
// itself because showMenu collapses a read error into selected -1, which
// would be indistinguishable from No.
func confirmYN(mu *sync.Mutex, out func(string), readKeyFn func() (keyMsg, error), question string, width int) (bool, error) {
	items := []menuItem{
		{label: "Yes", selectable: true},
		{label: "No", selectable: true},
	}
	if width < 30 {
		width = 30
	}
	sel := 0
	var frame transientMenuFrame
	draw := func() {
		mu.Lock()
		frame.draw(out, renderMenu(question, items, sel, width, defaultMenuFooter), width)
		mu.Unlock()
	}
	draw()
	for {
		k, err := readKeyFn()
		if err != nil {
			return false, err
		}
		switch k.Special {
		case keyEscape, keyCtrlC:
			mu.Lock()
			frame.clear(out)
			mu.Unlock()
			return false, nil
		case keyEnter:
			mu.Lock()
			frame.clear(out)
			mu.Unlock()
			return sel == 0, nil
		case keyUp:
			if sel > 0 {
				sel--
				draw()
			}
		case keyDown:
			if sel < len(items)-1 {
				sel++
				draw()
			}
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

	cur := c.currentModel()

	var items []menuItem
	lastProvider := ""
	for _, entry := range models {
		providerLabel := entry.ProviderName
		if providerLabel == "" {
			providerLabel = entry.Provider
		}
		if entry.Provider != lastProvider {
			items = append(items, menuItem{label: providerLabel, selectable: false})
			lastProvider = entry.Provider
		}
		label := entry.DisplayName
		if label == "" {
			label = entry.Model
		}
		items = append(items, menuItem{
			label:      label,
			detail:     entry.Ref,
			selectable: true,
			current:    entry.Ref == cur.Ref,
			group:      entry.Provider,
			extra:      modelChoice{ref: entry.Ref, label: label},
		})
	}

	result := showMenu(c.mu, c.writeRaw, c.readKeyFn, "Model", items, c.currentWidth())
	if result.selected >= 0 {
		choice := result.extra.(modelChoice)
		sessionID, err := c.currentSession()
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		if err := c.agent.SwitchModelForSession(sessionID, choice.ref); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.modelRef = choice.ref
		c.printLine(renderSystemMsg(fmt.Sprintf("  model switched to %s", choice.ref)))
	}
}

type modelChoice struct {
	ref   string
	label string
}

func (c *CLI) showSessionMenu() {
	c.showSessionMenuInner("active")
}

func (c *CLI) showSessionMenuInner(state string) {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	sessions, err := c.scope.SessionList(state)
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}

	cur := c.currentSessionSummary()

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
			// The source is reserved immediately before the destination open
			// and released at the selection commit: a busy or transitioning
			// source refuses here with the owner's mutability error, and an
			// idle source cannot start a turn while the destination commits.
			// The release comes before the render, so a blocked render cannot
			// hold the source transitioning.
			release, err := c.reserveSelection()
			if err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			summary, err := c.agent.OpenSession(id)
			if err != nil {
				if !errors.Is(err, snapshot.ErrSessionContended) {
					release()
					c.printLine(renderErrorMsg(err.Error()))
					return
				}
				// The user named a session another process is driving: it
				// opens read-only. Present its durable transcript, committing
				// routing current only once the view is available.
				state, herr := c.owner.HydrateSession(id)
				if herr != nil {
					release()
					c.printLine(renderErrorMsg(herr.Error()))
					return
				}
				c.scope.SetProjectPath(state.Session.ProjectPath)
				c.setCurrentSessionID(state.Session.ID)
				c.sv().SetReadOnly(state.Session.ID)
				// The render is fed from the state this open returned rather
				// than re-read, so routing moves only once the view is in
				// hand: a presentation that cannot be produced leaves the
				// previous session and project in place.
				release()
				c.refreshFromHydration(state)
				return
			}
			c.setCurrentSessionID(summary.ID)
			release()
			c.refreshSession()
		}
	case "new":
		// The source is reserved immediately before the destination create
		// and released at the selection commit, exactly as the select branch
		// reserves it. Archive/delete are lifecycle operations and do not
		// reserve.
		release, err := c.reserveSelection()
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		id, err := c.scope.NewSession("primary")
		if err != nil {
			var committed *snapshot.CommittedMutationError
			switch {
			case id != "" && errors.As(err, &committed): // the destination was created durably before the failure: adopt it and reject typed after release+refresh
				c.setCurrentSessionID(id)
				release()
				c.refreshSession()
				c.printLine(renderErrorMsg(err.Error()))
			default: // a plain precommit error retains the source exactly as it was
				release()
				c.printLine(renderErrorMsg(err.Error()))
			}
			return
		}
		c.setCurrentSessionID(id)
		release()
		c.refreshSession()
	case "archive":
		if result.selected >= 0 {
			id := result.extra.(string)
			err := c.agent.SessionArchive(id)
			if err != nil {
				var committed *snapshot.CommittedMutationError
				switch {
				case errors.As(err, &committed): // the removal ran durably before failing: run exactly the success reconciliation — stale-safe, so a noncurrent target preserves current — then surface the rejection. A plain precommit error reconciles nothing.
					c.clearRemovedCurrent(id)
				}
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.printLine(renderSystemMsg("  session archived"))
			c.clearRemovedCurrent(id)
		}
	case "delete":
		if result.selected >= 0 {
			id := result.extra.(string)
			err := c.agent.SessionDelete(id)
			if err != nil {
				var committed *snapshot.CommittedMutationError
				switch {
				case errors.As(err, &committed): // the removal ran durably before failing: run exactly the success reconciliation — stale-safe, so a noncurrent target preserves current — then surface the rejection. A plain precommit error reconciles nothing.
					c.clearRemovedCurrent(id)
				}
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.printLine(renderSystemMsg("  session deleted"))
			c.clearRemovedCurrent(id)
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

	cur := c.scope.ProjectCurrent()

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

	result := showMenu(c.mu, c.writeRaw, c.readKeyFn, "Project", items, c.currentWidth())
	if result.selected >= 0 {
		path := result.extra.(string)
		c.printLine(renderSystemMsg("  switching to " + path + "..."))
		c.projectSwitch(path)
	}
}

// showRevertMenu runs the revert/fork picker flow. A failed key read inside
// the confirmation is returned to the caller rather than consumed: the read
// reports the exit error once the latch is set, and the command dispatch must
// hand it to the key handler so the loop unwinds instead of rendering another
// prompt. The pickers fail safe as menu cancels and are not errors.
func (c *CLI) showRevertMenu() error {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	msgs := c.sessionMessages()
	if len(msgs) == 0 {
		c.printLine(renderErrorMsg("no messages to revert"))
		return nil
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
		return nil
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

	result := showMenu(c.mu, c.writeRaw, c.readKeyFn, "Revert — pick turn", items, c.currentWidth())
	if result.selected < 0 {
		return nil
	}
	turn := result.extra.(int)

	actionItems := []menuItem{
		{label: "Revert code", detail: "restore files, keep history", selectable: true, extra: "code"},
		{label: "Revert history", detail: "truncate conversation", selectable: true, extra: "history"},
		{label: "Fork from here", detail: "new session from this point", selectable: true, extra: "fork"},
		{label: "Back", selectable: true, extra: "back"},
	}

	actionResult := showMenu(c.mu, c.writeRaw, c.readKeyFn, fmt.Sprintf("Turn %d — action", turn), actionItems, c.currentWidth())
	if actionResult.selected < 0 {
		return nil
	}

	action := actionResult.extra.(string)
	sessionID, err := c.currentSession()
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return nil
	}
	switch action {
	case "code":
		result, err := c.agent.ApplyTurnActionForSession(sessionID, turn, agent.TurnActionRevertCode, false)
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			c.printRevertSkipped(result)
			return nil
		}
		c.printLine(renderSystemMsg(fmt.Sprintf("  reverted code to before turn %d", turn)))
		c.printRevertSkipped(result)

	case "history":
		alsoCode, err := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?", c.currentWidth())
		if err != nil {
			// The confirmation read failed (the terminal is exiting): abort the
			// revert rather than performing it as a "no", and hand the error to
			// the caller so the loop unwinds.
			return err
		}
		result, err := c.agent.ApplyTurnActionForSession(sessionID, turn, agent.TurnActionRevertHistory, alsoCode)
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			c.printRevertSkipped(result)
			// The owner reconciles the loop to disk after a history revert,
			// even when the walk stopped partway, so the transcript the user
			// is looking at is stale; re-render it over the reconciled state.
			c.refreshSession()
			return nil
		}
		if result.Session.ID != "" {
			c.setCurrentSessionID(result.Session.ID)
		}
		c.refreshSession()
		c.printRevertSkipped(result)

	case "fork":
		alsoCode, err := confirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?", c.currentWidth())
		if err != nil {
			// The confirmation read failed (the terminal is exiting): abort the
			// fork rather than performing it as a "no", and hand the error to
			// the caller so the loop unwinds.
			return err
		}
		result, err := c.agent.ApplyTurnActionForSession(sessionID, turn, agent.TurnActionFork, alsoCode)
		if err != nil {
			var committed *snapshot.CommittedMutationError
			switch {
			case result.Session.ID != "" && errors.As(err, &committed): // The destination committed; adopt and refresh it, then show kept files before the error.
				c.setCurrentSessionID(result.Session.ID)
				c.refreshSession()
				c.printRevertSkipped(result)
				c.printLine(renderErrorMsg(err.Error()))
			default: // a plain precommit error retains the source; its partial result carries no adopted session
				c.printLine(renderErrorMsg(err.Error()))
			}
			return nil
		}
		if result.Session.ID != "" {
			c.setCurrentSessionID(result.Session.ID)
		}
		c.refreshSession()
		c.printRevertSkipped(result)
		// The fork committed; only the best-effort code revert may have
		// failed, which the result carries as a warning on the success path.
		if result.Warning != "" {
			c.printLine(renderErrorMsg(result.Warning))
		}

	case "back":
	}
	return nil
}

func (c *CLI) printRevertSkipped(result agent.TurnActionResult) {
	if len(result.SkippedFiles) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  kept %d file", len(result.SkippedFiles))
	if len(result.SkippedFiles) != 1 {
		b.WriteByte('s')
	}
	b.WriteString(" changed outside this session:")
	for _, skipped := range result.SkippedFiles {
		b.WriteString("\n  - ")
		b.WriteString(skipped.Path)
		if skipped.Reason != "" {
			b.WriteString(" (")
			b.WriteString(skipped.Reason)
			b.WriteByte(')')
		}
	}
	c.printLine(renderSystemMsg(b.String()))
}
