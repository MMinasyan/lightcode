package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
)

type replState int32

const (
	stateIdle       replState = 0
	stateStreaming   replState = 1
	statePermission replState = 2
)

type permState struct {
	mu      sync.Mutex
	pending bool
	id      string
	tool    string
	arg     string
}

type suggestState struct {
	id          string
	suggestions []agent.PermissionSuggestion
}

type turnUsage struct {
	cache, input, output int
	estimated            bool
}

type CLI struct {
	svc              *agent.Agent
	state            int32
	perm             permState
	suggState        *suggestState
	compactionManual bool
	permEnteredAt    int64 // atomic, UnixNano — when PERMISSION state was entered

	outMu sync.Mutex
	out   *os.File

	ctx    context.Context
	cancel context.CancelFunc
	sigCh  chan os.Signal

	sessionMu sync.Mutex
	session   agent.SessionSummary
	messages  []agent.DisplayMessage
	tokens    agent.TokenReport
	turnUsage turnUsage
}

func New(svc *agent.Agent) *CLI {
	return &CLI{
		svc:   svc,
		out:   os.Stdout,
		sigCh: make(chan os.Signal, 1),
	}
}

func (c *CLI) Run(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	c.ctx = ctx
	c.cancel = cancel
	defer cancel()

	signal.Notify(c.sigCh, syscall.SIGINT)
	defer signal.Stop(c.sigCh)

	c.svc.SetEventHandler(c.handleEvent)
	c.svc.Init(ctx)

	c.printBanner()
	return c.repl()
}

func (c *CLI) printBanner() {
	proj := c.svc.ProjectCurrent()
	model := c.svc.CurrentModel()
	session := c.svc.SessionCurrent()

	c.writef("Lightcode — project: %s\n", c.svc.ProjectName())
	c.writef("Model: %s/%s", model.Provider, model.Model)
	if session.ID != "" {
		c.writef(" | Session: %s", shortID(session.ID))
	}
	c.writeln("")

	if session.ID == "" {
		c.writeln("No active session — your first message will create one.")
	}
	if proj.ID == "" {
		c.writeln(colorYellow("Warning: no project detected in current directory."))
	}
	c.writeln("Type /help for commands\n")
}

func (c *CLI) repl() error {
	lineCh := make(chan string)
	eofCh := make(chan struct{})

	scanner := bufio.NewScanner(os.Stdin)

	go func() {
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-c.ctx.Done():
				return
			}
		}
		close(eofCh)
	}()

	c.printPrompt()

	for {
		select {
		case <-c.ctx.Done():
			return nil

		case <-eofCh:
			return c.handleEOF()

		case <-c.sigCh:
			c.handleSIGINT()

		case line := <-lineCh:
			if c.processLine(line) {
				c.printPrompt()
			}
		}
	}
}

func (c *CLI) handleSIGINT() {
	switch replState(atomic.LoadInt32(&c.state)) {
	case stateIdle:
		c.writeln("\nExiting.")
		c.cancel()

	case stateStreaming:
		c.writeln("\nCancelling...")
		if err := c.svc.Cancel(); err != nil {
			c.writeError(fmt.Sprintf("cancel: %v", err))
		}

	case statePermission:
		c.perm.mu.Lock()
		id := c.perm.id
		c.perm.pending = false
		c.perm.mu.Unlock()
		c.suggState = nil

		if err := c.svc.RespondPermission(id, false); err != nil {
			c.writeError(fmt.Sprintf("deny permission: %v", err))
		}
		c.writeln("\nPermission denied.")
		atomic.StoreInt32(&c.state, int32(stateStreaming))
	}
}

func (c *CLI) handleEOF() error {
	switch replState(atomic.LoadInt32(&c.state)) {
	case statePermission:
		c.perm.mu.Lock()
		id := c.perm.id
		c.perm.pending = false
		c.perm.mu.Unlock()
		_ = c.svc.RespondPermission(id, false)
		c.writeln("\nPermission denied. Exiting.")
	default:
		c.writeln("\nExiting.")
	}
	c.cancel()
	return nil
}

func (c *CLI) processLine(line string) bool {
	line = strings.TrimSpace(line)

	switch replState(atomic.LoadInt32(&c.state)) {
	case stateStreaming:
		return false

	case statePermission:
		c.handlePermissionInput(line)
		return false

	case stateIdle:
		if line == "" {
			return true
		}
		if strings.HasPrefix(line, "/") {
			handleCommand(c, line)
			return true
		}
		c.sendPrompt(line)
		return false
	}
	return true
}

func (c *CLI) sendPrompt(content string) {
	_, err := c.svc.SendPrompt(c.ctx, content)
	if err != nil {
		c.writeError(fmt.Sprintf("send: %v", err))
	}
}

func (c *CLI) handlePermissionInput(line string) {
	if c.suggState != nil {
		c.handleSuggestPatternInput(line)
		return
	}

	enteredAt := atomic.LoadInt64(&c.permEnteredAt)
	if time.Since(time.Unix(0, enteredAt)) < 100*time.Millisecond {
		c.perm.mu.Lock()
		tool := c.perm.tool
		arg := c.perm.arg
		c.perm.mu.Unlock()
		c.writeRaw(renderPermissionPrompt(tool, arg))
		return
	}

	line = strings.TrimSpace(strings.ToLower(line))

	if line != "y" && line != "n" && line != "p" {
		c.perm.mu.Lock()
		tool := c.perm.tool
		arg := c.perm.arg
		c.perm.mu.Unlock()
		c.writeRaw(renderPermissionPrompt(tool, arg))
		return
	}

	c.perm.mu.Lock()
	id := c.perm.id
	tool := c.perm.tool
	arg := c.perm.arg
	c.perm.pending = false
	c.perm.mu.Unlock()

	switch line {
	case "y":
		if err := c.svc.RespondPermission(id, true); err != nil {
			c.writeError(fmt.Sprintf("allow: %v", err))
		} else {
			c.writeln("Allowed.")
		}
		atomic.StoreInt32(&c.state, int32(stateStreaming))

	case "n":
		if err := c.svc.RespondPermission(id, false); err != nil {
			c.writeError(fmt.Sprintf("deny: %v", err))
		} else {
			c.writeln("Denied.")
		}
		atomic.StoreInt32(&c.state, int32(stateStreaming))

	case "p":
		suggestions := c.svc.PermissionSuggest(tool, arg)
		if len(suggestions) == 0 {
			c.writeln("No suggestions available. Use y/n.")
			c.perm.mu.Lock()
			c.perm.pending = true
			c.perm.id = id
			c.perm.tool = tool
			c.perm.arg = arg
			c.perm.mu.Unlock()
			c.writeRaw(renderPermissionPrompt(tool, arg))
			return
		}
		c.suggState = &suggestState{id: id, suggestions: suggestions}
		c.writeRaw(renderSuggestions(suggestions))
	}
}

func (c *CLI) handleSuggestPatternInput(line string) {
	line = strings.TrimSpace(line)
	ss := c.suggState

	if line == "" {
		c.suggState = nil
		c.perm.mu.Lock()
		tool := c.perm.tool
		arg := c.perm.arg
		c.perm.pending = true
		c.perm.mu.Unlock()
		c.writeRaw(renderPermissionPrompt(tool, arg))
		return
	}

	selected, err := parsePatternSelection(line, len(ss.suggestions))
	if err != nil {
		c.writeError(fmt.Sprintf("invalid selection: %v", err))
		c.writeRaw("Select patterns (e.g. 1,3 or Enter to cancel): ")
		return
	}

	patterns := make([]string, len(selected))
	for i, idx := range selected {
		patterns[i] = ss.suggestions[idx].Rule
	}

	if err := c.svc.SaveProjectPermission(ss.id, patterns); err != nil {
		c.writeError(fmt.Sprintf("save permission: %v", err))
	} else {
		c.writeln("Patterns saved. Request allowed.")
	}
	c.suggState = nil
	atomic.StoreInt32(&c.state, int32(stateStreaming))
}

// --- Event handler (called from agent goroutines) ---

func (c *CLI) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		switch ev.Kind {
		case agent.EventTextDelta:
			c.writef("%s %s", subagentPrefix(ev), ev.Result)
		case agent.EventToolCallStart:
			c.writeln(renderSubagentToolStart(ev))
		case agent.EventToolCallEnd:
			c.writeln(renderSubagentToolEnd(ev))
		case agent.EventSubagentStart:
			c.writef("%s Started\n", subagentPrefix(ev))
		}
		return
	}

	switch ev.Kind {
	case agent.EventTextDelta:
		c.writeRaw(ev.Result)

	case agent.EventToolCallStart:
		c.writeln(renderToolStart(ev.ToolName, ev.Args))

	case agent.EventToolCallEnd:
		c.writeln(renderToolEnd(ev.IsError, ev.Result))

	case agent.EventUsage:
		c.sessionMu.Lock()
		c.turnUsage.cache += ev.Cache
		c.turnUsage.input += ev.Input
		c.turnUsage.output += ev.Output
		if !ev.UsageKnown {
			c.turnUsage.estimated = true
		}
		c.sessionMu.Unlock()

	case agent.EventTurnStart:
		c.sessionMu.Lock()
		c.turnUsage = turnUsage{}
		c.sessionMu.Unlock()
		atomic.StoreInt32(&c.state, int32(stateStreaming))
		c.writef("\n--- Turn %d ---\n", ev.Turn)

	case agent.EventTurnEnd:
		c.writeln("")
		c.sessionMu.Lock()
		usage := c.turnUsage
		c.tokens = c.svc.TokenUsage()
		c.sessionMu.Unlock()
		c.writeln(renderTurnUsage(usage))
		if ev.Cancelled {
			c.writeln(colorYellow("Turn cancelled."))
		}
		atomic.StoreInt32(&c.state, int32(stateIdle))
		c.printPrompt()

	case agent.EventError:
		c.writeln(colorRed(fmt.Sprintf("Error (turn %d): %s", ev.Turn, ev.Error)))

	case agent.EventPermissionRequest:
		c.perm.mu.Lock()
		c.perm.pending = true
		c.perm.id = ev.PermReq.ID
		c.perm.tool = ev.PermReq.ToolName
		c.perm.arg = ev.PermReq.Arg
		c.perm.mu.Unlock()
		atomic.StoreInt64(&c.permEnteredAt, time.Now().UnixNano())
		atomic.StoreInt32(&c.state, int32(statePermission))
		c.writeRaw(renderPermissionPrompt(ev.PermReq.ToolName, ev.PermReq.Arg))

	case agent.EventCompactionStart:
		if c.compactionManual {
			c.writeln(colorYellow("Compacting..."))
		} else {
			c.writeln(colorYellow("Context nearly full — compacting..."))
		}

	case agent.EventCompactionEnd:
		c.compactionManual = false
		c.writeln("Done.")
		c.emitSessionChanged()

	case agent.EventWarning:
		for _, w := range ev.Warnings {
			c.writeln(colorYellow(fmt.Sprintf("Warning [%s]: %s", w.Kind, w.Message)))
		}
	}
}

func (c *CLI) emitSessionChanged() {
	c.sessionMu.Lock()
	c.session = c.svc.SessionCurrent()
	c.messages = c.svc.SessionMessages()
	c.tokens = c.svc.TokenUsage()
	c.sessionMu.Unlock()
}

// --- Project switching ---

func (c *CLI) projectSwitch(targetPath string) error {
	if targetPath == "" {
		return fmt.Errorf("empty target path")
	}
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	if abs == c.svc.ProjectRoot() {
		return nil
	}

	_ = c.svc.Cancel()
	for i := 0; i < 200; i++ {
		if !c.svc.Busy() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if c.svc.Store().Active() {
		c.svc.Store().Close()
	}

	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary: %w", err)
	}
	cmd := exec.Command(bin, "cli")
	cmd.Dir = abs
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	c.cancel()
	os.Exit(0)
	return nil
}

// --- Output helpers ---

func (c *CLI) writeln(s string) {
	c.outMu.Lock()
	fmt.Fprintln(c.out, s)
	c.outMu.Unlock()
}

func (c *CLI) writef(format string, args ...any) {
	c.outMu.Lock()
	fmt.Fprintf(c.out, format, args...)
	c.outMu.Unlock()
}

func (c *CLI) writeRaw(s string) {
	c.outMu.Lock()
	fmt.Fprint(c.out, s)
	c.outMu.Unlock()
}

func (c *CLI) writeError(msg string) {
	c.writeln(colorRed("Error: " + msg))
}

func (c *CLI) printPrompt() {
	switch replState(atomic.LoadInt32(&c.state)) {
	case stateIdle:
		c.writeRaw("lightcode> ")
	case stateStreaming:
		c.writeRaw("(working...) ")
	case statePermission:
		c.writeRaw("allow? [y/n/p]: ")
	}
}
