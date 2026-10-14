package cliclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/internal/clientview"
	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

func (c *CLI) writer(output io.Writer) {
	for {
		select {
		case <-c.exit:
			return
		default:
		}
		var content string
		var action func()
		select {
		case <-c.exit:
			return
		case <-c.wake:
		case d := <-c.outputs:
			c.mu.Lock()
			if (d.actual || (d.connectionOnly && c.connectionCurrentLocked(d.stamp)) || c.currentLocked(d.stamp)) && (d.listGeneration == 0 || d.listGeneration == c.listGeneration) {
				content = d.text
			}
			c.mu.Unlock()
		case event := <-c.events:
			c.acceptEvent(event)
		case key := <-c.keys:
			id := ""
			if key.Special == terminal.KeyEnter {
				id = operationID()
			}
			// Admission and exit priority share the latch hold. EOF observed
			// before this hold forbids acting on an already buffered key.
			c.exitMu.Lock()
			if c.exiting {
				c.exitMu.Unlock()
				return
			}
			c.mu.Lock()
			action = c.keyLocked(key, id)
			c.notifyLocked()
			c.mu.Unlock()
			c.exitMu.Unlock()
		}
		if action != nil {
			action()
		}
		select {
		case <-c.exit:
			return
		default:
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		frame := c.frameLocked(content)
		c.mu.Unlock()
		if _, err := io.WriteString(output, frame); err != nil {
			c.requestExit(err)
			return
		}
	}
}

func (c *CLI) keyLocked(key terminal.KeyMsg, id string) func() {
	if c.menu != nil {
		return c.menuKeyLocked(key)
	}
	r := c.displayedLocked()
	active := r != nil && r.hydration != nil && r.hydration.ActiveOperation != nil
	s := c.stampLocked()
	switch key.Special {
	case terminal.KeyCtrlC, terminal.KeyEscape:
		if c.forkBoundary != nil {
			c.forkBoundary = nil
			c.forkRewind = false
			return nil
		}
		if active {
			return func() { c.control(s, "InterruptSession", r.id) }
		}
		if key.Special == terminal.KeyCtrlC {
			return func() { c.requestExit(nil) }
		}
	case terminal.KeyCtrlD:
		if !active {
			return func() { c.requestExit(nil) }
		}
	case terminal.KeyCtrlT:
		c.queued = true
	case terminal.KeyEnter:
		text := c.draft.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if strings.HasPrefix(text, "/") && c.forkBoundary == nil {
			c.draft.Clear()
			c.draftRevision++
			return func() { c.command(text) }
		}
		operation := "SubmitSession"
		if c.forkBoundary != nil {
			operation = "ForkSession"
		}
		a, err := c.formAttemptLocked(operation, id)
		if err != nil {
			return func() { c.report(s, terminal.RenderErrorMsg(err.Error()), false) }
		}
		c.history.Add(text)
		return func() { c.task(func() { c.runAttempt(a.id) }) }
	case terminal.KeyBackspace:
		if c.draft.DeleteBack() {
			c.draftRevision++
			c.history.Edited()
		}
	case terminal.KeyDelete:
		if c.draft.DeleteForward() {
			c.draftRevision++
			c.history.Edited()
		}
	case terminal.KeyLeft:
		c.draft.MoveLeft()
	case terminal.KeyRight:
		c.draft.MoveRight()
	case terminal.KeyHome:
		c.draft.MoveHome()
	case terminal.KeyEnd:
		c.draft.MoveEnd()
	case terminal.KeyUp:
		if text, ok := c.history.Prev(c.draft.String()); ok {
			c.draft.Set(text)
			c.draftRevision++
		}
	case terminal.KeyDown:
		if text, ok := c.history.Next(); ok {
			c.draft.Set(text)
			c.draftRevision++
		}
	case terminal.KeyTab:
		text := terminal.CompleteSlashCommand(c.draft.String(), commands)
		if text != c.draft.String() {
			c.draft.Set(text)
			c.draftRevision++
		}
	default:
		if key.Rune != 0 {
			c.draft.Insert(key.Rune)
			c.draftRevision++
			c.history.Edited()
		}
	}
	return nil
}

func renderItems(items []protocol.ConversationItem, width int) string {
	var b strings.Builder
	for _, item := range items {
		kind, _ := item.Discriminator()
		switch kind {
		case "input":
			input, err := item.AsInputItem()
			if err == nil {
				b.WriteString(terminal.RenderUserMsg(contentText(input.Content), width))
			}
		case "assistant":
			a, err := item.AsAssistantItem()
			if err != nil {
				continue
			}
			text := contentText(a.Content)
			if text != "" {
				b.WriteString(terminal.RenderAssistantMsg(text, width))
			}
			if a.Refusal != nil {
				b.WriteString(terminal.RenderWarningMsg("Refusal: " + *a.Refusal))
			}
			for _, call := range a.ToolCalls {
				var metadata map[string]any
				if call.Metadata != nil {
					_ = json.Unmarshal(*call.Metadata, &metadata)
				}
				b.WriteString(terminal.RenderToolCall(call.Name, call.Arguments, metadata))
				if call.Status != nil {
					content := ""
					if call.Content != nil {
						content = *call.Content
					}
					b.WriteString(terminal.RenderToolResult(call.Name, call.Arguments, content, *call.Status == protocol.ToolCallStatusSuccess, false, width, metadata))
				}
			}
		case "signal":
			s, err := item.AsSignalItem()
			if err == nil {
				b.WriteString(terminal.RenderSystemMsg(s.Content))
			}
		case "compaction":
			compact, err := item.AsCompactionItem()
			if err == nil {
				raw, _ := json.Marshal(compact)
				b.WriteString(terminal.RenderSystemMsg("Committed compaction: " + string(raw)))
			}
		case "operation_end":
			end, err := item.AsOperationEndItem()
			if err == nil {
				raw, _ := json.Marshal(end)
				b.WriteString(terminal.RenderSystemMsg(string(raw)))
			}
		}
	}
	return b.String()
}

func (c *CLI) frameLocked(content string) string {
	var b strings.Builder
	// Always erase the entire prior prompt before ANY output. It is transient,
	// not terminal scrollback or an authoritative conversation item.
	if c.promptBelow > 0 {
		b.WriteString(fmt.Sprintf("\x1b[%dB", c.promptBelow))
	}
	c.promptBelow = 0
	b.WriteString(terminal.EraseBlock(c.promptRows))
	c.promptRows = 0
	b.WriteString(content)
	if content == "\x1b[2J\x1b[3J\x1b[H" {
		c.renderedReader = nil
	}
	r := c.displayedLocked()
	if r != nil && r.hydration != nil {
		if c.renderedReader != r || c.renderedVersion != r.assignmentVersion {
			b.WriteString(terminal.RenderSystemMsg("── Authoritative refreshed range — " + r.id + " ──"))
			b.WriteString(renderItems(r.items, c.width))
			c.renderedReader, c.renderedVersion = r, r.assignmentVersion
		}
	}
	if c.menu != nil {
		var frame terminal.TransientMenuFrame
		rendered := terminal.RenderMenu(c.menu.title, c.menu.items, c.menu.selected, c.width, "↑↓ Enter Esc · session: n/new a/archive d/delete Tab/lifecycle")
		frame.Draw(func(text string) { b.WriteString(text) }, rendered, c.width)
		// RenderMenu bounds every line to width-12 (minimum native width 30).
		c.promptRows = strings.Count(rendered, "\r\n") + 2
		return b.String()
	}
	if r != nil && r.hydration != nil {
		h := r.hydration
		model := "not selected"
		if h.SelectedModel != nil {
			model = *h.SelectedModel
		}
		status := fmt.Sprintf("Future: %s / %s", h.Session.AgentType, model)
		if h.ActiveOperation != nil {
			op := h.ActiveOperation
			status += fmt.Sprintf(" · Active %s, captured conversation: %s / %s", op.RequestKind, op.AgentType, op.Model)
		}
		if c.childID != "" {
			status += " · child inspection, read only"
		}
		for _, line := range promptLines(status, max(1, c.width-1)) {
			b.WriteString(line + "\r\n")
			c.promptRows++
		}
	}
	if c.provisional != "" {
		label := "Model activity — provisional"
		if c.provisionalTruncated {
			label += " (truncated)"
		}
		for _, line := range promptLines(label+"\n"+c.provisional, max(1, c.width-1)) {
			b.WriteString(line + "\r\n")
			c.promptRows++
		}
	}
	prefix := "> "
	if c.queued {
		prefix = "queued> "
	}
	if c.forkBoundary != nil {
		prefix = "fork> "
	}
	lines := promptLines(prefix+c.draft.String(), max(1, c.width-1))
	b.WriteString(strings.Join(lines, "\r\n"))
	c.promptRows += len(lines)
	before := promptLines(prefix+c.draft.CursorText(), max(1, c.width-1))
	c.promptBelow = len(lines) - len(before)
	if c.promptBelow > 0 {
		b.WriteString(fmt.Sprintf("\x1b[%dA", c.promptBelow))
	}
	b.WriteString("\r")
	if column := terminal.VisibleWidth(before[len(before)-1]); column > 0 {
		b.WriteString(fmt.Sprintf("\x1b[%dC", column))
	}
	return b.String()
}

func promptLines(text string, width int) []string {
	lines := []string{""}
	column := 0
	for _, r := range text {
		if r == '\r' {
			continue
		}
		w := terminal.VisibleWidth(string(r))
		if r == '\n' || column+w > width {
			lines = append(lines, "")
			column = 0
		}
		if r != '\n' {
			lines[len(lines)-1] += string(r)
			column += w
		}
	}
	return lines
}

func (c *CLI) copyCanonical(s stamp) {
	c.mu.Lock()
	if !c.currentLocked(s) {
		c.mu.Unlock()
		return
	}
	r := c.displayedLocked()
	var text string
	found := false
	if r != nil {
		for i := len(r.items) - 1; i >= 0; i-- {
			kind, _ := r.items[i].Discriminator()
			if kind != "assistant" {
				continue
			}
			a, err := r.items[i].AsAssistantItem()
			if err != nil {
				continue
			}
			text = contentText(a.Content)
			if text == "" && a.Refusal != nil {
				text = *a.Refusal
			}
			found = true
			break
		}
	}
	c.mu.Unlock()
	if !found {
		c.report(s, terminal.RenderWarningMsg("No loaded authoritative assistant to copy."), false)
		return
	}
	var bytes bytes.Buffer
	if err := terminal.WriteClipboardOSC52(&bytes, text); err != nil {
		c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		return
	}
	c.report(s, bytes.String()+terminal.RenderSystemMsg("Copied authoritative assistant."), false)
}

func (c *CLI) progressLocked(scope protocol.Scope, text string) {
	attribution, err := scope.AsRuntimeScope()
	r := c.displayedLocked()
	if err != nil || r == nil {
		return
	}
	if attribution.SessionId == nil || *attribution.SessionId != r.id {
		return
	}
	if r.hydration == nil || r.hydration.ActiveOperation == nil || attribution.OperationId == nil || *attribution.OperationId != r.hydration.ActiveOperation.OperationId {
		r.dirty = true
		c.kickReaderLocked(r)
		return
	}
	c.provisional += text
	if len(c.provisional) > 64*1024 {
		start := len(c.provisional) - 64*1024
		for start < len(c.provisional) && !utf8.RuneStart(c.provisional[start]) {
			start++
		}
		c.provisional = c.provisional[start:]
		c.provisionalTruncated = true
	}
	c.notifyLocked()
}
func (c *CLI) acceptEvent(delivery eventDelivery) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.conn != delivery.conn {
		return
	}
	event := delivery.event
	kind, err := event.Discriminator()
	if err != nil {
		c.repairLocked(delivery.conn)
		return
	}
	switch kind {
	case "text_delta":
		e, err := event.AsTextDeltaEvent()
		if err == nil {
			c.progressLocked(e.Scope, e.Content)
		}
	case "refusal_delta":
		e, err := event.AsRefusalDeltaEvent()
		if err == nil {
			c.progressLocked(e.Scope, "[Refusal] "+e.Content)
		}
	case "tool_started":
		e, err := event.AsToolStartedEvent()
		if err == nil {
			c.progressLocked(e.Scope, "\nTool activity "+e.CallId+": "+e.Name+"\n")
		}
	case "tool_finished":
		e, err := event.AsToolFinishedEvent()
		if err == nil {
			c.progressLocked(e.Scope, "\nTool activity "+e.CallId+": "+string(e.Status)+"\n")
		}
	case "session_changed":
		e, err := event.AsSessionChangedEvent()
		if err != nil {
			return
		}
		if _, err := clientview.CompareSessionRevision(e.SessionRevision, e.SessionRevision); err != nil || e.SessionRevision.InstanceId != delivery.conn.InstanceID() {
			c.repairLocked(delivery.conn)
			return
		}
		scope, _ := e.Scope.AsRuntimeScope()
		for _, r := range []*sessionReader{c.root, c.child} {
			if r == nil || scope.SessionId == nil || r.id != *scope.SessionId {
				continue
			}
			order := 1
			if r.hydration != nil {
				order, err = clientview.CompareSessionRevision(e.SessionRevision, r.hydration.SessionRevision)
			}
			if err != nil {
				c.repairLocked(delivery.conn)
				return
			}
			if order > 0 {
				r.dirty = true
				c.kickReaderLocked(r)
			}
		}
	case "configuration_changed":
		e, err := event.AsConfigurationChangedEvent()
		if err != nil {
			return
		}
		floor := protocol.ConfigurationRevision{InstanceId: delivery.conn.InstanceID(), Generation: "0"}
		if c.configuration != nil {
			floor = c.configuration.ConfigurationRevision
		}
		order, err := clientview.CompareConfigurationRevision(e.ConfigurationRevision, floor)
		if err != nil {
			c.repairLocked(delivery.conn)
			return
		}
		if order > 0 {
			c.globalsDirty = true
			c.kickGlobalsLocked()
		}
		for _, r := range []*sessionReader{c.root, c.child} {
			if r == nil {
				continue
			}
			order = 1
			if r.hydration != nil {
				order, err = clientview.CompareConfigurationRevision(e.ConfigurationRevision, r.hydration.ConfigurationRevision)
			}
			if err != nil {
				c.repairLocked(delivery.conn)
				return
			}
			if order > 0 {
				r.dirty = true
				c.kickReaderLocked(r)
			}
		}
	case "warning_changed":
		e, err := event.AsWarningChangedEvent()
		if err != nil {
			return
		}
		floor := protocol.WarningsRevision{InstanceId: delivery.conn.InstanceID(), Revision: "0"}
		if c.warnings != nil {
			floor = c.warnings.WarningsRevision
		}
		order, err := clientview.CompareWarningsRevision(e.WarningsRevision, floor)
		if err != nil {
			c.repairLocked(delivery.conn)
			return
		}
		if order > 0 {
			c.globalsDirty = true
			c.kickGlobalsLocked()
		}
		for _, r := range []*sessionReader{c.root, c.child} {
			if r == nil {
				continue
			}
			order = 1
			if r.hydration != nil {
				order, err = clientview.CompareWarningsRevision(e.WarningsRevision, r.hydration.WarningsRevision)
			}
			if err != nil {
				c.repairLocked(delivery.conn)
				return
			}
			if order > 0 {
				r.dirty = true
				c.kickReaderLocked(r)
			}
		}
	default:
		e, err := event.AsScopeEvent()
		if err != nil {
			c.repairLocked(delivery.conn)
			return
		}
		scope, err := e.Scope.AsRuntimeScope()
		if err != nil {
			c.repairLocked(delivery.conn)
			return
		}
		if scope.Workspace != nil && *scope.Workspace != c.workspace {
			return
		}
		for _, r := range []*sessionReader{c.root, c.child} {
			if r != nil && (scope.SessionId == nil || *scope.SessionId == r.id) {
				r.dirty = true
				c.kickReaderLocked(r)
			}
		}
	}
}
