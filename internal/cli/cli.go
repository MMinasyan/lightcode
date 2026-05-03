package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/MMinasyan/lightcode/internal/agent"
)

type cliState int

const (
	stateIdle cliState = iota
	stateStreaming
	statePermission
	stateMenu
)

type CLI struct {
	agent *agent.Agent
	out   io.Writer
	mu    *sync.Mutex

	width     int
	oldState *term.State
	rawFd    int

	state      cliState
	busy       bool
	input      *inputLine
	keyCh      chan keyMsg
	events     chan agent.Event
	readKeyFn  func() (keyMsg, error)
	ctx        context.Context

	provider string
	model    string

	animStop  chan struct{}
	animLabel string

	streamStarted bool
	streamNeedsNL bool
	streamBuf     strings.Builder
	afterToolEnd  bool

	msgQueue []string

	permQueue []*agent.PermissionRequest

	toolExpanded bool
	promptLines  int

	messages []displayEntry

	compacting bool
}

func New(a *agent.Agent) *CLI {
	return &CLI{
		agent: a,
		out:   os.Stdout,
		mu:    &sync.Mutex{},
		input: newInputLine(),
		keyCh: make(chan keyMsg, 64),
		events: make(chan agent.Event, 256),
		width: 80,
	}
}

func (c *CLI) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rawFd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(rawFd)
	if err != nil {
		return fmt.Errorf("terminal raw mode: %w", err)
	}
	c.oldState = oldState
	c.rawFd = rawFd

	defer c.restoreTerminal()

	if w, _, err := term.GetSize(rawFd); err == nil && w > 0 {
		c.width = w
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGWINCH:
					if w, _, err := term.GetSize(rawFd); err == nil && w > 0 {
						c.mu.Lock()
						c.width = w
						c.mu.Unlock()
					}
				case syscall.SIGINT:
					c.mu.Lock()
					st := c.state
					c.mu.Unlock()
					if st == stateStreaming || st == statePermission {
						_ = c.agent.Cancel()
					} else {
						term.Restore(rawFd, oldState)
						fmt.Fprint(os.Stdout, "\r\n\x1b[?25h")
						os.Exit(130)
					}
				case syscall.SIGTERM:
					term.Restore(rawFd, oldState)
					fmt.Fprint(os.Stdout, "\r\n\x1b[?25h")
					os.Exit(130)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	c.ctx = ctx
	c.readKeyFn = func() (keyMsg, error) {
		select {
		case k := <-c.keyCh:
			return k, nil
		case <-ctx.Done():
			return keyMsg{}, ctx.Err()
		}
	}

	c.agent.SetEventHandler(func(ev agent.Event) {
		c.events <- ev
	})

	c.agent.Init(ctx)

	c.refreshState()

	msgs := c.agent.SessionMessages()
	c.messages = buildDisplayMsgs(msgs)

	c.printHeader()

	for _, m := range c.messages {
		c.printDisplayEntry(m)
	}

	c.printInputPrompt()

	go c.readKeys(ctx)

	c.mainLoop(ctx)

	return nil
}

func (c *CLI) readKeys(ctx context.Context) {
	var buf [256]byte
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := os.Stdin.Read(buf[:])
		if err != nil {
			return
		}
		keys := parseInputBytes(buf[:n])
		for _, k := range keys {
			select {
			case c.keyCh <- k:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *CLI) mainLoop(ctx context.Context) {
	for {
		select {
		case k := <-c.keyCh:
			c.handleKey(k)
		case ev := <-c.events:
			c.handleEvent(ev)
		case <-ctx.Done():
			return
		}
	}
}

func (c *CLI) handleKey(k keyMsg) {
	switch c.state {
	case stateIdle:
		c.handleKeyIdle(k)
	case stateStreaming:
		c.handleKeyStreaming(k)
	case statePermission:
		c.handleKeyPermission(k)
	case stateMenu:
	}
}

func (c *CLI) handleKeyIdle(k keyMsg) {
	switch k.Special {
	case keyEnter:
		text := c.input.String()
		c.input.Clear()
		c.mu.Lock()
		if c.promptLines > 1 {
			c.writeRaw(eraseBlock(c.promptLines))
		} else {
			c.writeRaw("\r\x1b[2K")
		}
		c.promptLines = 0
		c.mu.Unlock()

		if text == "" {
			c.printInputPrompt()
			return
		}

		if strings.HasPrefix(text, "/") {
			c.printLine(colorDim + "> " + text + colorReset)
			c.dispatchCommand(text)
			c.printInputPrompt()
			return
		}

		c.submitInput(text)

	case keyBackspace:
		if c.input.DeleteBack() {
			c.printInputPrompt()
		}

	case keyDelete:
		if c.input.DeleteForward() {
			c.printInputPrompt()
		}

	case keyTab:
		completed := completeSlashCommand(c.input.String())
		c.input.Set(completed)
		c.printInputPrompt()

	case keyLeft:
		if c.input.MoveLeft() {
			c.printInputPrompt()
		}

	case keyRight:
		if c.input.MoveRight() {
			c.printInputPrompt()
		}

	case keyHome:
		c.input.MoveHome()
		c.printInputPrompt()

	case keyEnd:
		c.input.MoveEnd()
		c.printInputPrompt()

	case keyCtrlC, keyCtrlD:
		c.restoreTerminal()
		os.Exit(0)

	default:
		if k.Rune != 0 {
			c.input.Insert(k.Rune)
			c.printInputPrompt()
		}
	}
}

func (c *CLI) handleKeyStreaming(k keyMsg) {
	switch k.Special {
	case keyCtrlC, keyEscape:
		_ = c.agent.Cancel()
	case keyEnter:
		text := c.input.String()
		if text != "" {
			c.input.Clear()
			if strings.HasPrefix(text, "/") {
				c.handleSlashWhileBusy(text)
			} else {
				c.msgQueue = append(c.msgQueue, text)
			}
		}
	case keyBackspace:
		c.input.DeleteBack()
	case keyDelete:
		c.input.DeleteForward()
	case keyLeft:
		c.input.MoveLeft()
	case keyRight:
		c.input.MoveRight()
	case keyHome:
		c.input.MoveHome()
	case keyEnd:
		c.input.MoveEnd()
	default:
		if k.Rune != 0 {
			c.input.Insert(k.Rune)
		}
	}
}

func (c *CLI) handleKeyPermission(k keyMsg) {
	if len(c.permQueue) == 0 {
		return
	}

	req := c.permQueue[0]

	switch k.Rune {
	case 'y', 'Y':
		c.popAndRespond(req.ID, true)
	case 'n', 'N':
		c.popAndRespond(req.ID, false)
	case 'p', 'P':
		c.showPermissionSuggestions(req)
	}
	switch k.Special {
	case keyCtrlC, keyEscape:
		c.popAndRespond(req.ID, false)
	}
}

func (c *CLI) popAndRespond(id string, allow bool) {
	c.permQueue = c.permQueue[1:]
	_ = c.agent.RespondPermission(id, allow)

	if len(c.permQueue) > 0 {
		c.printPermissionBlock(c.permQueue[0])
	} else {
		c.mu.Lock()
		c.state = stateStreaming
		c.mu.Unlock()
	}
}

func (c *CLI) showPermissionSuggestions(req *agent.PermissionRequest) {
	suggestions := c.agent.PermissionSuggest(req.ToolName, req.Arg)
	if len(suggestions) == 0 {
		c.printLine(renderErrorMsg("no suggestions available"))
		return
	}

	c.printLine(nl + colorDim + "  ── Allow for project ──" + colorReset)
	for i, s := range suggestions {
		c.printLine(fmt.Sprintf("  %d %s", i+1, s.Label))
	}
	c.printLine(colorDim + "  Enter numbers (e.g. 1,3) or Esc to cancel" + colorReset)

	var input strings.Builder
	for {
		k, err := c.readKeyFn()
		if err != nil {
			c.popAndRespond(req.ID, false)
			return
		}

		switch k.Special {
		case keyEscape, keyCtrlC:
			c.mu.Lock()
			c.writeRaw(eraseBlock(len(suggestions) + 3))
			c.mu.Unlock()
			c.printPermissionBlock(req)
			return
		case keyEnter:
			selected := parseSelectionNumbers(input.String(), len(suggestions))
			if len(selected) == 0 {
				c.popAndRespond(req.ID, false)
				return
			}
			var patterns []string
			for _, idx := range selected {
				patterns = append(patterns, suggestions[idx-1].Rule)
			}
			if err := c.agent.SaveProjectPermission(req.ID, patterns); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				c.printPermissionBlock(req)
				return
			}
			c.permQueue = c.permQueue[1:]
			if len(c.permQueue) > 0 {
				c.printPermissionBlock(c.permQueue[0])
			} else {
				c.mu.Lock()
				c.state = stateStreaming
				c.mu.Unlock()
			}
			return
		case keyBackspace:
			s := input.String()
			if len(s) > 0 {
				input.Reset()
				input.WriteString(s[:len(s)-1])
				c.mu.Lock()
				c.writeRaw("\r\x1b[2K  " + input.String())
				c.mu.Unlock()
			}
		default:
			if k.Rune != 0 {
				input.WriteRune(k.Rune)
				c.mu.Lock()
				c.writeRaw(string(k.Rune))
				c.mu.Unlock()
			}
		}
	}
}

func parseSelectionNumbers(s string, max int) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err == nil && n >= 1 && n <= max {
			result = append(result, n)
		}
	}
	return result
}

func (c *CLI) handleEvent(ev agent.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ev.SubagentSessionID != "" {
		c.handleSubagentEvent(ev)
		return
	}

	switch ev.Kind {
	case agent.EventTurnStart:
		c.stopAnimationLocked()
		c.busy = true
		c.state = stateStreaming
		c.streamStarted = false
		c.streamBuf.Reset()
		c.afterToolEnd = false

	case agent.EventTextDelta:
		if !c.streamStarted {
			c.streamStarted = true
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
			if c.afterToolEnd {
				c.writeRaw(nl)
				c.afterToolEnd = false
			}
		}
		text := strings.ReplaceAll(ev.Result, "\n", "\r\n")
		c.writeRaw(text)
		c.streamBuf.WriteString(ev.Result)
		c.streamNeedsNL = !strings.HasSuffix(ev.Result, "\n")

	case agent.EventToolCallStart:
		if c.streamStarted && c.streamNeedsNL {
			c.writeRaw("\r\n")
		}
		if c.streamStarted {
			c.finalizeStreamBufLocked()
			c.streamStarted = false
		} else {
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
		}
		c.messages = append(c.messages, displayEntry{
			typ:  "tool",
			name: ev.ToolName,
			args: ev.Args,
		})
		c.writeRaw(renderToolCall(ev.ToolName, ev.Args))
		c.startAnimationLocked("Running")

	case agent.EventToolCallEnd:
		c.stopAnimationLocked()
		c.writeRaw("\r\x1b[2K")
		for i := len(c.messages) - 1; i >= 0; i-- {
			if c.messages[i].typ == "tool" && c.messages[i].name == ev.ToolName && !c.messages[i].done {
				c.messages[i].done = true
				c.messages[i].success = !ev.IsError
				c.messages[i].result = ev.Result
				break
			}
		}
		c.writeRaw(renderToolResult(ev.Result, !ev.IsError, c.toolExpanded, c.width))
		c.afterToolEnd = true
		if c.busy {
			c.startAnimationLocked("Thinking")
		}

	case agent.EventTurnEnd:
		c.stopAnimationLocked()
		if c.streamStarted && c.streamNeedsNL {
			c.writeRaw("\r\n")
		}
		c.writeRaw("\r\x1b[2K")
		if c.streamStarted {
			c.finalizeStreamBufLocked()
		}
		if ev.Cancelled {
			c.writeRaw(renderSystemMsg("  interrupted"))
		}
		c.busy = false
		c.state = stateIdle
		c.streamStarted = false
		c.permQueue = nil
		if len(c.msgQueue) > 0 {
			c.flushQueueLocked()
		} else {
			c.printInputPromptLocked()
		}

	case agent.EventError:
		c.stopAnimationLocked()
		c.writeRaw("\r\x1b[2K")
		c.writeRaw(renderErrorMsg(ev.Error))
		c.busy = false
		c.state = stateIdle
		c.printInputPromptLocked()

	case agent.EventPermissionRequest:
		c.permQueue = append(c.permQueue, ev.PermReq)
		if len(c.permQueue) == 1 {
			c.state = statePermission
			c.printPermissionBlockLocked(ev.PermReq)
		}

	case agent.EventUsage:

	case agent.EventCompactionStart:
		c.compacting = true

	case agent.EventCompactionEnd:
		c.compacting = false
		c.refreshSessionLocked()

	case agent.EventWarning:
		c.writeRaw("\r\x1b[2K")
		for _, w := range ev.Warnings {
			c.writeRaw(renderWarningMsg(w.Kind + ": " + w.Message))
		}
		if c.state == stateIdle {
			c.printInputPromptLocked()
		}
	}
}

func (c *CLI) handleSubagentEvent(ev agent.Event) {
	tag := fmt.Sprintf("task%d", ev.TaskIndex)
	prefix := fmt.Sprintf("[%s] ", tag)

	switch ev.Kind {
	case agent.EventSubagentStart:
		c.writeRaw(renderSubagentMsg(tag, "started"))
	case agent.EventTextDelta:
		c.writeRaw(prefix + strings.ReplaceAll(ev.Result, "\n", "\r\n"+prefix) + "\r\n")
	case agent.EventToolCallStart:
		c.writeRaw(renderSubagentMsg(tag, fmt.Sprintf("⟩ %s  %s", ev.ToolName, formatToolArgs(ev.ToolName, ev.Args))))
	case agent.EventToolCallEnd:
		result := truncate(ev.Result, 200)
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		c.writeRaw(renderSubagentMsg(tag, fmt.Sprintf("%s: %s", status, result)))
	}
}

func (c *CLI) finalizeStreamBufLocked() {
	text := c.streamBuf.String()
	if text != "" {
		trimmed := strings.TrimRight(text, "\n")
		rawLines := strings.Split(trimmed, "\n")
		rows := 0
		for _, rl := range rawLines {
			w := visibleWidth(rl)
			if c.width > 0 && w > c.width {
				rows += (w + c.width - 1) / c.width
			} else {
				rows++
			}
		}
		c.writeRaw(eraseBlock(rows + 1))
		c.writeRaw(renderAssistantMsg(text, c.width))

		c.messages = append(c.messages, displayEntry{
			typ:     "assistant",
			content: text,
		})
	}
	c.streamBuf.Reset()
}

func (c *CLI) refreshState() {
	m := c.agent.CurrentModel()
	c.provider = m.Provider
	c.model = m.Model
}

func (c *CLI) printHeader() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printHeaderLocked()
}

func (c *CLI) printHeaderLocked() {
	session := c.agent.SessionCurrent()
	sid := session.ID
	if sid == "" {
		sid = "(no session)"
	}
	header := fmt.Sprintf("  %s  %s  %s", c.agent.ProjectName(), sid, c.model)
	c.printLineLocked(colorDim + header + colorReset)
	c.printLineLocked("")
}

func (c *CLI) printInputPrompt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printInputPromptLocked()
}

func (c *CLI) printInputPromptLocked() {
	if c.promptLines > 1 {
		c.writeRaw(eraseBlock(c.promptLines))
	} else {
		c.writeRaw("\r\x1b[2K")
	}
	text := c.input.String()
	c.writeRaw("> " + text)

	promptLen := 2 + visibleWidth(text)
	if c.width > 0 && promptLen > c.width {
		c.promptLines = (promptLen + c.width - 1) / c.width
	} else {
		c.promptLines = 1
	}
}

func (c *CLI) printLine(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printLineLocked(s)
}

func (c *CLI) printLineLocked(s string) {
	c.writeRaw(s)
	if !strings.HasSuffix(s, nl) {
		c.writeRaw(nl)
	}
}

func (c *CLI) printDisplayEntry(e displayEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printDisplayEntryLocked(e)
}

func (c *CLI) printDisplayEntryLocked(e displayEntry) {
	switch e.typ {
	case "user":
		c.printLineLocked(renderUserMsg(e.content, c.width))
	case "assistant":
		c.printLineLocked(renderAssistantMsg(e.content, c.width))
	case "tool":
		c.printLineLocked(renderToolCall(e.name, e.args))
		if e.done {
			c.printLineLocked(renderToolResult(e.result, e.success, c.toolExpanded, c.width))
		}
	case "system":
		c.printLineLocked(renderSystemMsg(e.content))
	}
}

func (c *CLI) printPermissionBlock(req *agent.PermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printPermissionBlockLocked(req)
}

func (c *CLI) printPermissionBlockLocked(req *agent.PermissionRequest) {
	c.writeRaw(nl)
	c.writeRaw(colorDim + " ┌─ Permission Required ─" + strings.Repeat("─", max(0, c.width-26)) + colorReset + nl)
	c.writeRaw(colorDim + " │ " + colorCyan + req.ToolName + colorReset + nl)
	argLine := truncate(req.Arg, c.width-4)
	c.writeRaw(colorDim + " │ " + colorReset + argLine + nl)
	c.writeRaw(colorDim + " │" + colorReset + nl)
	c.writeRaw(colorDim + " │ y — allow  n — deny  p — project rules" + colorReset + nl)
	c.writeRaw(colorDim + " └" + strings.Repeat("─", c.width-2) + colorReset + nl)
}

func (c *CLI) submitInput(text string) {
	c.mu.Lock()
	c.submitInputLocked(text)
	c.mu.Unlock()
}

func (c *CLI) submitInputLocked(text string) {
	c.printLineLocked(renderUserMsg(text, c.width))
	c.startAnimationLocked("Thinking")

	go func() {
		turn, err := c.agent.SendPrompt(c.ctx, text)
		if err != nil {
			c.mu.Lock()
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
			c.writeRaw(renderErrorMsg(err.Error()))
			c.busy = false
			c.state = stateIdle
			c.printInputPromptLocked()
			c.mu.Unlock()
			return
		}
		_ = turn
	}()
}

func (c *CLI) flushQueue() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushQueueLocked()
}

func (c *CLI) flushQueueLocked() {
	if len(c.msgQueue) == 0 {
		return
	}

	queue := c.msgQueue
	c.msgQueue = nil

	for _, text := range queue[:len(queue)-1] {
		c.printLineLocked(renderUserMsg(text, c.width))
		if _, err := c.agent.AppendUserMessage(text); err != nil {
			c.printLineLocked(renderErrorMsg(err.Error()))
		}
	}

	last := queue[len(queue)-1]
	c.submitInputLocked(last)
}

func (c *CLI) refreshSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshSessionLocked()
}

func (c *CLI) refreshSessionLocked() {
	c.refreshState()
	msgs := c.agent.SessionMessages()
	c.messages = buildDisplayMsgs(msgs)

	c.printLineLocked("")
	c.printHeaderLocked()
	for _, m := range c.messages {
		c.printDisplayEntryLocked(m)
	}
	c.printInputPromptLocked()
}

func (c *CLI) handleSlashWhileBusy(text string) {
	cmd := strings.Fields(text)[0]

	c.mu.Lock()
	label := c.animLabel
	c.stopAnimationLocked()
	c.writeRaw("\r\x1b[2K")

	switch cmd {
	case "/help":
		c.mu.Unlock()
		c.cmdHelp()
		c.mu.Lock()
	case "/context":
		c.mu.Unlock()
		c.cmdContext()
		c.mu.Lock()
	case "/copy":
		c.mu.Unlock()
		c.cmdCopy()
		c.mu.Lock()
	default:
		if cmd == "/model" || cmd == "/session" || cmd == "/project" || cmd == "/new" ||
			cmd == "/resume" || cmd == "/revert" || cmd == "/fork" ||
			cmd == "/compact" || cmd == "/exit" {
			c.writeRaw(renderErrorMsg("cannot run this command while a turn is running"))
		} else {
			c.writeRaw(renderErrorMsg(fmt.Sprintf("unknown command: %s", cmd)))
		}
	}

	c.startAnimationLocked(label)
	c.mu.Unlock()
}

func (c *CLI) dispatchCommand(text string) {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/help":
		c.cmdHelp()
	case "/model":
		c.showModelMenu()
	case "/session":
		c.showSessionMenu()
	case "/project":
		c.showProjectMenu()
	case "/new":
		if err := c.agent.SessionNew(); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.refreshSession()
	case "/resume":
		c.cmdResume(parts)
	case "/revert":
		c.showRevertMenu()
	case "/fork":
		c.showRevertMenu()
	case "/context":
		c.cmdContext()
	case "/compact":
		c.cmdCompact()
	case "/copy":
		c.cmdCopy()
	case "/exit":
		c.restoreTerminal()
		os.Exit(0)
	default:
		c.printLine(renderErrorMsg(fmt.Sprintf("unknown command: %s", cmd)))
	}
}

func (c *CLI) cmdHelp() {
	c.printLine("")
	c.printLine(colorDim + "  Commands:" + colorReset)
	c.printLine("  /help          show this help")
	c.printLine("  /model         switch model")
	c.printLine("  /session       list/switch sessions")
	c.printLine("  /project       switch project")
	c.printLine("  /new           start new session")
	c.printLine("  /resume [id]   resume session")
	c.printLine("  /revert        revert code/history/fork")
	c.printLine("  /fork          same as /revert")
	c.printLine("  /context       show token usage")
	c.printLine("  /compact       compact context")
	c.printLine("  /copy          copy last response to clipboard")
	c.printLine("  /exit          exit lightcode")
	c.printLine("")
	c.printLine(colorDim + "  Keys:" + colorReset)
	c.printLine("  Ctrl+C/D       exit (idle) / cancel (streaming)")
	c.printLine("  Escape         cancel (streaming)")
	c.printLine("  Tab            autocomplete slash command")
}

func (c *CLI) cmdResume(parts []string) {
	if len(parts) > 1 {
		id := parts[1]
		if err := c.agent.SessionSwitch(id); err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.refreshSession()
		return
	}

	sessions, err := c.agent.SessionList("active")
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}
	if len(sessions) == 0 {
		c.printLine(renderErrorMsg("no active sessions"))
		return
	}

	if err := c.agent.SessionSwitch(sessions[0].ID); err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}
	c.refreshSession()
}

func (c *CLI) cmdContext() {
	report := c.agent.TokenUsage()

	c.printLine("")
	if report.ContextWindow > 0 {
		pct := float64(report.ContextUsed) / float64(report.ContextWindow) * 100
		c.printLine(fmt.Sprintf("  context: %.0f%% (%s / %s)", pct, fmtTokens(report.ContextUsed), fmtTokens(report.ContextWindow)))
	}

	c.printLine("")
	c.printLine(colorDim + "  model                  ⚡ cache    ↑ input    ↓ output" + colorReset)
	c.printLine(colorDim + "  ─────────────────────────────────────────────────────" + colorReset)
	for _, e := range report.PerModel {
		name := e.Model
		if len(name) > 22 {
			name = name[:19] + "..."
		}
		c.printLine(fmt.Sprintf("  %-22s %10s %10s %10s", name, fmtTokens(e.Cache), fmtTokens(e.Input), fmtTokens(e.Output)))
	}
	if len(report.PerModel) > 1 {
		c.printLine(colorDim + "  ─────────────────────────────────────────────────────" + colorReset)
		c.printLine(fmt.Sprintf("  %-22s %10s %10s %10s", "total", fmtTokens(report.Total.Cache), fmtTokens(report.Total.Input), fmtTokens(report.Total.Output)))
	}
}

func (c *CLI) cmdCompact() {
	if c.busy {
		c.printLine(renderErrorMsg("cannot compact while a turn is running"))
		return
	}

	c.mu.Lock()
	c.busy = true
	c.state = stateStreaming
	c.mu.Unlock()

	c.startAnimation("Compacting")

	go func() {
		err := c.agent.CompactNow(c.ctx)
		c.mu.Lock()
		c.stopAnimationLocked()
		c.writeRaw("\r\x1b[2K")
		c.busy = false
		c.state = stateIdle
		c.mu.Unlock()

		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			c.printInputPrompt()
		}
	}()
}

func (c *CLI) cmdCopy() {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].typ == "assistant" && c.messages[i].content != "" {
			if err := writeClipboard(c.out, c.messages[i].content); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				return
			}
			c.printLine(renderSystemMsg("copied to clipboard"))
			return
		}
	}
	c.printLine(renderErrorMsg("no assistant response to copy"))
}

func (c *CLI) projectSwitch(targetPath string) {
	_ = c.agent.Cancel()
	for i := 0; i < 200; i++ {
		if !c.agent.Busy() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if c.agent.Store().Active() {
		c.agent.Store().Close()
	}

	c.restoreTerminal()
	if err := c.relaunchIn(targetPath); err != nil {
		fmt.Fprintf(os.Stderr, "relaunch: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func (c *CLI) restoreTerminal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oldState != nil {
		term.Restore(c.rawFd, c.oldState)
		c.oldState = nil
	}
	c.writeRaw("\x1b[?25h")
}

func (c *CLI) startAnimation(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startAnimationLocked(label)
}

func (c *CLI) startAnimationLocked(label string) {
	c.stopAnimationLocked()
	c.animLabel = label
	stop := make(chan struct{})
	c.animStop = stop

	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		c.mu.Lock()
		c.writeRaw("\x1b[?25l")
		c.mu.Unlock()
		for {
			select {
			case <-stop:
				c.mu.Lock()
				c.writeRaw("\x1b[?25h")
				c.mu.Unlock()
				return
			case <-time.After(80 * time.Millisecond):
				c.mu.Lock()
				if c.animStop == nil {
					c.mu.Unlock()
					return
				}
				c.writeRaw(fmt.Sprintf("\r\x1b[2K%s %s", frames[i%len(frames)], label))
				c.mu.Unlock()
				i++
			}
		}
	}()
}

func (c *CLI) stopAnimationLocked() {
	if c.animStop != nil {
		close(c.animStop)
		c.animStop = nil
	}
}

func (c *CLI) writeRaw(s string) {
	fmt.Fprint(c.out, s)
}
