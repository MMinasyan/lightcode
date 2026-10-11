package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/terminal"
)

func (c *CLI) showModelMenu() {
	c.mu.Lock()
	c.stopAnimationLocked()
	c.mu.Unlock()

	models := c.agent.ModelList()
	if len(models) == 0 {
		c.printLine(terminal.RenderErrorMsg("no models configured"))
		return
	}

	cur := c.currentModel()

	var items []terminal.MenuItem
	lastProvider := ""
	for _, entry := range models {
		providerLabel := entry.ProviderName
		if providerLabel == "" {
			providerLabel = entry.Provider
		}
		if entry.Provider != lastProvider {
			items = append(items, terminal.MenuItem{Label: providerLabel, Selectable: false})
			lastProvider = entry.Provider
		}
		label := entry.DisplayName
		if label == "" {
			label = entry.Model
		}
		items = append(items, terminal.MenuItem{
			Label:      label,
			Detail:     entry.Ref,
			Selectable: true,
			Current:    entry.Ref == cur.Ref,
			Group:      entry.Provider,
			Extra:      modelChoice{ref: entry.Ref, label: label},
		})
	}

	result := terminal.ShowMenu(c.mu, c.writeRaw, c.readKeyFn, "Model", items, c.currentWidth())
	if result.Selected >= 0 {
		choice := result.Extra.(modelChoice)
		sessionID, err := c.currentSession()
		if err != nil {
			c.printLine(terminal.RenderErrorMsg(err.Error()))
			return
		}
		if err := c.agent.SwitchModelForSession(sessionID, choice.ref); err != nil {
			c.printLine(terminal.RenderErrorMsg(err.Error()))
			return
		}
		c.modelRef = choice.ref
		c.printLine(terminal.RenderSystemMsg(fmt.Sprintf("  model switched to %s", choice.ref)))
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
		c.printLine(terminal.RenderErrorMsg(err.Error()))
		return
	}

	cur := c.currentSessionSummary()

	var items []terminal.MenuItem
	if len(sessions) == 0 {
		items = append(items, terminal.MenuItem{Label: "(no sessions)", Selectable: false})
	} else {
		for _, s := range sessions {
			detail := time.Unix(s.LastActivity, 0).Format("Jan 02 15:04")
			items = append(items, terminal.MenuItem{
				Label:      s.ID,
				Detail:     detail,
				Selectable: true,
				Current:    s.ID == cur.ID,
				Extra:      s.ID,
			})
		}
	}

	title := "Sessions"
	if state == "archived" {
		title = "Archived Sessions"
	}

	result := c.showSessionMenuWithActions(title, items, state)
	switch result.Action {
	case "select":
		if result.Selected >= 0 {
			id := result.Extra.(string)
			// The source is reserved immediately before the destination open
			// and released at the selection commit: a busy or transitioning
			// source refuses here with the owner's mutability error, and an
			// idle source cannot start a turn while the destination commits.
			// The release comes before the render, so a blocked render cannot
			// hold the source transitioning.
			release, err := c.reserveSelection()
			if err != nil {
				c.printLine(terminal.RenderErrorMsg(err.Error()))
				return
			}
			summary, err := c.agent.OpenSession(id)
			if err != nil {
				if !errors.Is(err, snapshot.ErrSessionContended) {
					release()
					c.printLine(terminal.RenderErrorMsg(err.Error()))
					return
				}
				// The user named a session another process is driving: it
				// opens read-only. Present its durable transcript, committing
				// routing current only once the view is available.
				state, herr := c.owner.HydrateSession(id)
				if herr != nil {
					release()
					c.printLine(terminal.RenderErrorMsg(herr.Error()))
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
			c.printLine(terminal.RenderErrorMsg(err.Error()))
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
				c.printLine(terminal.RenderErrorMsg(err.Error()))
			default: // a plain precommit error retains the source exactly as it was
				release()
				c.printLine(terminal.RenderErrorMsg(err.Error()))
			}
			return
		}
		c.setCurrentSessionID(id)
		release()
		c.refreshSession()
	case "archive":
		if result.Selected >= 0 {
			id := result.Extra.(string)
			err := c.agent.SessionArchive(id)
			if err != nil {
				var committed *snapshot.CommittedMutationError
				switch {
				case errors.As(err, &committed): // the removal ran durably before failing: run exactly the success reconciliation — stale-safe, so a noncurrent target preserves current — then surface the rejection. A plain precommit error reconciles nothing.
					c.clearRemovedCurrent(id)
				}
				c.printLine(terminal.RenderErrorMsg(err.Error()))
				return
			}
			c.printLine(terminal.RenderSystemMsg("  session archived"))
			c.clearRemovedCurrent(id)
		}
	case "delete":
		if result.Selected >= 0 {
			id := result.Extra.(string)
			err := c.agent.SessionDelete(id)
			if err != nil {
				var committed *snapshot.CommittedMutationError
				switch {
				case errors.As(err, &committed): // the removal ran durably before failing: run exactly the success reconciliation — stale-safe, so a noncurrent target preserves current — then surface the rejection. A plain precommit error reconciles nothing.
					c.clearRemovedCurrent(id)
				}
				c.printLine(terminal.RenderErrorMsg(err.Error()))
				return
			}
			c.printLine(terminal.RenderSystemMsg("  session deleted"))
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

func (c *CLI) showSessionMenuWithActions(title string, items []terminal.MenuItem, state string) terminal.MenuResult {
	sel := terminal.FirstSelectableMenuItem(items)

	var frame terminal.TransientMenuFrame

	draw := func() {
		width := c.currentWidth()
		tabHint := "Tab: archived"
		if state == "archived" {
			tabHint = "Tab: active"
		}
		footer := "   ↑↓ navigate  Enter select  n:new  a:archive  d:delete  " + tabHint + "  Esc cancel"

		c.mu.Lock()
		frame.Draw(c.writeRaw, terminal.RenderMenu(title, items, sel, width, footer), width)
		c.mu.Unlock()
	}

	draw()

	for {
		k, err := c.readKeyFn()
		if err != nil {
			return terminal.MenuResult{Selected: -1}
		}

		switch k.Special {
		case terminal.KeyEscape, terminal.KeyCtrlC:
			c.mu.Lock()
			frame.Clear(c.writeRaw)
			c.mu.Unlock()
			return terminal.MenuResult{Selected: -1}

		case terminal.KeyEnter:
			if sel >= 0 && sel < len(items) && items[sel].Selectable {
				c.mu.Lock()
				frame.Clear(c.writeRaw)
				c.mu.Unlock()
				return terminal.MenuResult{Selected: sel, Extra: items[sel].Extra, Action: "select"}
			}

		case terminal.KeyUp:
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

		case terminal.KeyDown:
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

		case terminal.KeyTab:
			c.mu.Lock()
			frame.Clear(c.writeRaw)
			c.mu.Unlock()
			return terminal.MenuResult{Action: "toggle"}
		}

		switch k.Rune {
		case 'n':
			c.mu.Lock()
			frame.Clear(c.writeRaw)
			c.mu.Unlock()
			return terminal.MenuResult{Action: "new"}
		case 'a':
			if sel >= 0 && sel < len(items) && items[sel].Selectable {
				c.mu.Lock()
				frame.Clear(c.writeRaw)
				c.mu.Unlock()
				return terminal.MenuResult{Selected: sel, Extra: items[sel].Extra, Action: "archive"}
			}
		case 'd':
			if sel >= 0 && sel < len(items) && items[sel].Selectable {
				c.mu.Lock()
				frame.Clear(c.writeRaw)
				c.mu.Unlock()
				return terminal.MenuResult{Selected: sel, Extra: items[sel].Extra, Action: "delete"}
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
		c.printLine(terminal.RenderErrorMsg(err.Error()))
		return
	}

	cur, err := c.scope.ProjectCurrent()
	if err != nil {
		c.printLine(terminal.RenderErrorMsg(err.Error()))
		return
	}

	var items []terminal.MenuItem
	for _, p := range projects {
		detail := p.Path
		if len(detail) > 40 {
			detail = "..." + detail[len(detail)-37:]
		}
		items = append(items, terminal.MenuItem{
			Label:      p.Name,
			Detail:     detail,
			Selectable: true,
			Current:    p.ID == cur.ID,
			Extra:      p.Path,
		})
	}

	result := terminal.ShowMenu(c.mu, c.writeRaw, c.readKeyFn, "Project", items, c.currentWidth())
	if result.Selected >= 0 {
		path := result.Extra.(string)
		c.printLine(terminal.RenderSystemMsg("  switching to " + path + "..."))
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
		c.printLine(terminal.RenderErrorMsg("no messages to revert"))
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
		c.printLine(terminal.RenderErrorMsg("no user turns to revert"))
		return nil
	}

	var items []terminal.MenuItem
	for _, t := range turns {
		items = append(items, terminal.MenuItem{
			Label:      fmt.Sprintf("Turn %d", t.turn),
			Detail:     t.content,
			Selectable: true,
			Extra:      t.turn,
		})
	}

	result := terminal.ShowMenu(c.mu, c.writeRaw, c.readKeyFn, "Revert — pick turn", items, c.currentWidth())
	if result.Selected < 0 {
		return nil
	}
	turn := result.Extra.(int)

	actionItems := []terminal.MenuItem{
		{Label: "Revert code", Detail: "restore files, keep history", Selectable: true, Extra: "code"},
		{Label: "Fork from here", Detail: "new session from this point", Selectable: true, Extra: "fork"},
		{Label: "Back", Selectable: true, Extra: "back"},
	}

	actionResult := terminal.ShowMenu(c.mu, c.writeRaw, c.readKeyFn, fmt.Sprintf("Turn %d — action", turn), actionItems, c.currentWidth())
	if actionResult.Selected < 0 {
		return nil
	}

	action := actionResult.Extra.(string)
	sessionID, err := c.currentSession()
	if err != nil {
		c.printLine(terminal.RenderErrorMsg(err.Error()))
		return nil
	}
	switch action {
	case "code":
		result, err := c.agent.ApplyTurnActionForSession(sessionID, turn, agent.TurnActionRevertCode, false)
		if err != nil {
			c.printLine(terminal.RenderErrorMsg(err.Error()))
			c.printRevertSkipped(result)
			return nil
		}
		c.printLine(terminal.RenderSystemMsg(fmt.Sprintf("  reverted code to before turn %d", turn)))
		c.printRevertSkipped(result)

	case "fork":
		alsoCode, err := terminal.ConfirmYN(c.mu, c.writeRaw, c.readKeyFn, "also revert code?", c.currentWidth())
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
				c.printLine(terminal.RenderErrorMsg(err.Error()))
			default: // a plain precommit error retains the source; its partial result carries no adopted session
				c.printLine(terminal.RenderErrorMsg(err.Error()))
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
			c.printLine(terminal.RenderErrorMsg(result.Warning))
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
	c.printLine(terminal.RenderSystemMsg(b.String()))
}
