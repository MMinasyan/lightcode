package cliclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

func (c *CLI) invalidateListLocked() {
	c.listGeneration++
	if c.menu != nil && (c.menu.kind == "session" || c.menu.kind == "child") {
		c.menu = nil
	}
}
func (c *CLI) reportList(s stamp, generation uint64, text string) {
	c.mu.Lock()
	if !c.currentLocked(s) || generation != c.listGeneration {
		c.mu.Unlock()
		return
	}
	overflow := false
	select {
	case c.outputs <- delivery{stamp: s, text: text, listGeneration: generation}:
	default:
		overflow = true
	}
	c.mu.Unlock()
	if overflow {
		c.requestExit(errors.New("terminal output overflow"))
	}
}

func (c *CLI) showMenu(s stamp, kind, title string, items []terminal.MenuItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLocked(s) {
		return
	}
	c.menu = &menu{stamp: s, kind: kind, title: title, items: items, selected: terminal.FirstSelectableMenuItem(items)}
	c.notifyLocked()
}
func (c *CLI) workspaceMenu(s stamp) {
	var workspaces []protocol.Workspace
	_, err := call(s, "ListWorkspaces", "", nil, nil, &workspaces)
	if err != nil {
		c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		return
	}
	items := make([]terminal.MenuItem, 0, len(workspaces))
	for _, w := range workspaces {
		items = append(items, terminal.MenuItem{Label: w.DisplayName, Detail: w.Root, Selectable: true, Extra: w.Root})
	}
	c.showMenu(s, "workspace", "Workspace — /project <directory> also accepts a new directory", items)
}
func (c *CLI) childrenMenu(s stamp) {
	c.mu.Lock()
	if !c.currentLocked(s) {
		c.mu.Unlock()
		return
	}
	c.listGeneration++
	generation := c.listGeneration
	c.mu.Unlock()
	var children []protocol.Session
	for _, lifecycle := range []protocol.SessionLifecycle{"open", "archived"} {
		var list []protocol.Session
		_, err := call(s, "ListSessions", "", nil, protocol.ListSessionsParams{Workspace: s.workspace, Lifecycle: lifecycle}, &list)
		c.mu.Lock()
		current := c.currentLocked(s) && generation == c.listGeneration
		c.mu.Unlock()
		if !current {
			return
		}
		if err != nil {
			c.reportList(s, generation, terminal.RenderErrorMsg(err.Error()))
			return
		}
		for _, child := range list {
			if child.ParentSessionId != nil && *child.ParentSessionId == s.root {
				children = append(children, child)
			}
		}
	}
	c.mu.Lock()
	if !c.currentLocked(s) || generation != c.listGeneration {
		c.mu.Unlock()
		return
	}
	items := make([]terminal.MenuItem, 0, len(children))
	seen := make(map[string]bool)
	for _, child := range children {
		seen[child.SessionId] = true
		items = append(items, terminal.MenuItem{Label: child.SessionId, Detail: string(child.Lifecycle) + " — inspect only", Selectable: true, Extra: child.SessionId})
	}
	if c.root != nil && c.root.hydration != nil {
		for _, member := range c.root.hydration.Background {
			if member.Kind == protocol.BackgroundMemberKindChild && !seen[member.Id] {
				items = append(items, terminal.MenuItem{Label: member.Id, Detail: "live child — inspect only", Selectable: true, Extra: member.Id})
			}
			if member.Kind == protocol.BackgroundMemberKindJob {
				items = append(items, terminal.MenuItem{Label: "Job " + member.Id, Detail: "live background member"})
			}
		}
	}
	c.menu = &menu{stamp: s, kind: "child", title: "Root-owned children and Jobs", items: items, selected: terminal.FirstSelectableMenuItem(items), listGeneration: generation}
	c.notifyLocked()
	c.mu.Unlock()
}

// agentSourceDetail always exposes the full source tuple beside the display
// name using the existing menu detail text; names are not globally unique.
func agentSourceDetail(agent protocol.Agent) string {
	source := agent.Definition.ExtensionId + "/" + agent.Definition.CapabilityId
	if agent.Description == "" {
		return source
	}
	return source + " — " + agent.Description
}

func (c *CLI) selectionMenu(s stamp, name, arg string) {
	if arg != "" {
		c.setSelection(s, name, arg)
		return
	}
	var items []terminal.MenuItem
	if name == "/agent" {
		var configuration protocol.ConfigurationView
		_, err := call(s, "GetConfiguration", "", nil, nil, &configuration)
		if err != nil {
			c.report(s, terminal.RenderErrorMsg(err.Error()), false)
			return
		}
		if len(configuration.Agents) == 0 {
			return
		}
		for _, agent := range configuration.Agents {
			items = append(items, terminal.MenuItem{Label: agent.Name, Detail: agentSourceDetail(agent), Selectable: true, Extra: agent.Definition})
		}
	} else {
		var models protocol.ModelList
		_, err := call(s, "ListModels", "", nil, protocol.ListModelsParams{}, &models)
		if err != nil {
			c.report(s, terminal.RenderErrorMsg(err.Error()), false)
			return
		}
		for _, model := range models.Models {
			if !model.Hidden && !model.ProviderHidden {
				items = append(items, terminal.MenuItem{Label: model.Ref, Detail: model.DisplayName, Selectable: true, Extra: model.Ref})
			}
		}
	}
	title := "This Session's future Agent type"
	if name == "/model" {
		c.mu.Lock()
		resolved := c.currentLocked(s) && c.root != nil && c.root.hydration != nil && c.root.hydration.Session.AgentSelection.Inputs != nil
		c.mu.Unlock()
		if resolved {
			title = "This Session's future model"
		} else {
			title = "Type-wide future model — all Sessions using this type"
		}
	}
	c.showMenu(s, name, title, items)
}

// definitionByName resolves one CLI name shorthand against the complete
// source-qualified roster: exactly one match sends its full reference, while
// zero matches report the definition as unavailable and several report the
// ambiguity. Neither failure sends a selection.
func (c *CLI) definitionByName(s stamp, name string) (protocol.DefinitionRef, bool) {
	var configuration protocol.ConfigurationView
	_, err := call(s, "GetConfiguration", "", nil, nil, &configuration)
	if err != nil {
		c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		return protocol.DefinitionRef{}, false
	}
	matches := make([]protocol.DefinitionRef, 0, 1)
	for _, agent := range configuration.Agents {
		if agent.Name == name {
			matches = append(matches, agent.Definition)
		}
	}
	switch len(matches) {
	case 0:
		c.report(s, terminal.RenderErrorMsg(fmt.Sprintf("agent definition %q is unavailable", name)), false)
		return protocol.DefinitionRef{}, false
	case 1:
		return matches[0], true
	default:
		c.report(s, terminal.RenderErrorMsg(fmt.Sprintf("agent definition %q is ambiguous across %d providers", name, len(matches))), false)
		return protocol.DefinitionRef{}, false
	}
}

func (c *CLI) setSelection(s stamp, name string, value any) {
	c.mu.Lock()
	if !c.currentLocked(s) || c.root == nil || c.root.hydration == nil || c.childID != "" {
		c.mu.Unlock()
		return
	}
	selection := c.root.hydration.Session.AgentSelection
	c.mu.Unlock()
	var raw []byte
	var err error
	if name == "/agent" {
		var definition protocol.DefinitionRef
		switch typed := value.(type) {
		case protocol.DefinitionRef:
			definition = typed
		case string:
			resolved, ok := c.definitionByName(s, typed)
			if !ok {
				return
			}
			definition = resolved
		default:
			return
		}
		raw, err = call(s, "SetSessionAgentType", s.root, protocol.SetSessionAgentTypeRequest{AgentSelection: protocol.AgentSelection{Definition: &definition}}, nil, nil)
	} else if modelRef, ok := value.(string); !ok {
		return
	} else if selection.Inputs != nil {
		raw, err = call(s, "SetSessionModel", s.root, protocol.SetSessionModelRequest{Model: modelRef}, nil, nil)
	} else if selection.Definition != nil && selection.Definition.ExtensionId == "agents" && selection.Definition.CapabilityId == "agents.definitions" {
		arguments, marshalErr := json.Marshal(struct {
			DefinitionID string `json:"definition_id"`
			Model        string `json:"model"`
		}{DefinitionID: selection.Definition.DefinitionId, Model: modelRef})
		if marshalErr != nil {
			c.report(s, terminal.RenderErrorMsg(marshalErr.Error()), false)
			return
		}
		raw, err = call(s, "UpdateConfigurationSettings", "", protocol.UpdateSettingsRequest{ExtensionEdit: &protocol.ExtensionEdit{
			ExtensionId: "agents",
			EditId:      "set_model",
			Arguments:   string(arguments),
		}}, nil, nil)
	} else if selection.Definition != nil {
		// A read-only or foreign named resolver offers no supplied model edit.
		// Report the same meaning as the desktop refusal and send nothing.
		c.report(s, terminal.RenderErrorMsg("This definition does not offer a model edit."), false)
		return
	} else {
		return
	}
	c.report(s, resultText(raw, err), true)
	c.mu.Lock()
	if c.currentLocked(s) {
		if name != "/model" || selection.Inputs == nil {
			c.globalsDirty = true
			c.kickGlobalsLocked()
		}
		c.refreshReadersLocked()
	}
	c.mu.Unlock()
}

func (c *CLI) inputMenu(s stamp, revert bool) {
	c.mu.Lock()
	if !c.currentLocked(s) || c.childID != "" || c.root == nil {
		c.mu.Unlock()
		return
	}
	r := c.root
	if r.chain != nil {
		done := r.chain.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-s.ctx.Done():
			return
		}
		c.mu.Lock()
		if !c.currentLocked(s) || c.root != r {
			c.mu.Unlock()
			return
		}
	}
	var items []terminal.MenuItem
	for _, item := range r.items {
		kind, _ := item.Discriminator()
		if kind != "input" {
			continue
		}
		input, err := item.AsInputItem()
		if err != nil || (revert && input.OperationId == nil) {
			continue
		}
		items = append(items, terminal.MenuItem{Label: contentText(input.Content), Detail: input.ItemId, Selectable: true, Extra: input})
	}
	if r.cursor != nil {
		items = append(items, terminal.MenuItem{Label: "Older inputs", Selectable: true, Extra: "older"})
	}
	c.mu.Unlock()
	kind, title := "fork", "Replace an input — fork does not restore code"
	if revert {
		kind, title = "revert", "Rewind code before an owned input; conversation unchanged"
	}
	c.showMenu(s, kind, title, items)
}
func (c *CLI) olderInputMenu(s stamp, revert bool) {
	c.mu.Lock()
	if !c.currentLocked(s) || c.root == nil {
		c.mu.Unlock()
		return
	}
	r := c.root
	r.pendingOlder = true
	c.kickReaderLocked(r)
	var done <-chan struct{}
	if r.chain != nil {
		done = r.chain.done
	}
	c.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-s.ctx.Done():
			return
		}
	}
	c.inputMenu(s, revert)
}
func (c *CLI) attemptsMenu(s stamp) {
	c.mu.Lock()
	var items []terminal.MenuItem
	if c.currentLocked(s) {
		for _, a := range c.attempts {
			if a.session == s.root && a.stamp.workspace == s.workspace && strings.HasPrefix(a.outcome, "unknown") {
				items = append(items, terminal.MenuItem{Label: a.operation + " " + a.id, Detail: "Outcome unknown", Selectable: true, Extra: a.id})
			}
		}
	}
	c.mu.Unlock()
	c.showMenu(s, "attempts", "Outstanding attempts for this root", items)
}

func (c *CLI) menuKeyLocked(key terminal.KeyMsg) func() {
	m := c.menu
	if !c.currentLocked(m.stamp) || (m.listGeneration != 0 && m.listGeneration != c.listGeneration) {
		c.menu = nil
		return nil
	}
	if key.Special == terminal.KeyEscape || key.Special == terminal.KeyCtrlC {
		c.menu = nil
		return nil
	}
	if key.Special == terminal.KeyCtrlD {
		return nil
	}
	if key.Special == terminal.KeyUp || key.Special == terminal.KeyDown {
		step := 1
		if key.Special == terminal.KeyUp {
			step = -1
		}
		for i := m.selected + step; i >= 0 && i < len(m.items); i += step {
			if m.items[i].Selectable {
				m.selected = i
				break
			}
		}
		return nil
	}
	if m.kind == "session" {
		if key.Special == terminal.KeyTab {
			c.menu = nil
			lifecycle := "archived"
			if m.lifecycle == "archived" {
				lifecycle = "open"
			}
			return func() { c.task(func() { c.refreshList(m.stamp, lifecycle, true) }) }
		}
		if key.Rune == 'n' {
			c.menu = nil
			return func() { c.command("/new") }
		}
		if (key.Rune == 'a' || key.Rune == 'd') && m.selected >= 0 && m.selected < len(m.items) {
			if session, ok := m.items[m.selected].Extra.(protocol.Session); ok {
				c.menu = nil
				return func() { c.archiveDelete(m.stamp, session.SessionId, key.Rune == 'd') }
			}
		}
	}
	if key.Special != terminal.KeyEnter || m.selected < 0 || m.selected >= len(m.items) {
		return nil
	}
	extra := m.items[m.selected].Extra
	c.menu = nil
	switch m.kind {
	case "session":
		if session, ok := extra.(protocol.Session); ok {
			return func() { c.selectRoot(session.SessionId) }
		}
		return func() { c.attemptsMenu(m.stamp) }
	case "workspace":
		return func() { c.selectWorkspace(extra.(string)) }
	case "child":
		return func() { c.inspectChild(extra.(string)) }
	case "/agent", "/model":
		return func() { c.task(func() { c.setSelection(m.stamp, m.kind, extra) }) }
	case "fork", "revert":
		if extra == "older" {
			return func() { c.task(func() { c.olderInputMenu(m.stamp, m.kind == "revert") }) }
		}
		input := extra.(protocol.InputItem)
		if m.kind == "revert" {
			return func() {
				c.task(func() {
					raw, err := call(m.stamp, "RevertSessionCode", m.stamp.root, protocol.RevertCodeRequest{BoundaryOperationId: *input.OperationId}, nil, nil)
					c.report(m.stamp, resultText(raw, err), true)
				})
			}
		}
		c.forkBoundary = &input
		c.draft.Set(contentText(input.Content))
		c.draftRevision++
		if input.OperationId != nil {
			c.menu = &menu{stamp: m.stamp, kind: "fork-choice", title: "Fork replacement", items: []terminal.MenuItem{{Label: "Fork only", Selectable: true, Extra: false}, {Label: "Rewind source code, then Fork (best effort)", Selectable: true, Extra: true}}, selected: 0}
		}
	case "fork-choice":
		c.forkRewind = extra.(bool)
	case "attempts":
		id := extra.(string)
		items := []terminal.MenuItem{{Label: "Abandon " + id, Selectable: true, Extra: id}}
		if a := c.attempts[id]; a != nil && a.outcome == "unknown" {
			items = append([]terminal.MenuItem{{Label: "Retry " + id, Selectable: true, Extra: id}}, items...)
		}
		c.menu = &menu{stamp: m.stamp, kind: "attempt-action", title: "Retry may duplicate buffered input", items: items, selected: 0}
	case "attempt-action":
		id := extra.(string)
		if strings.HasPrefix(m.items[m.selected].Label, "Abandon ") {
			delete(c.attempts, id)
		} else {
			return func() { c.retryAttempt(id) }
		}
	}
	return nil
}
