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
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

type cliState int

const (
	stateIdle cliState = iota
	stateStreaming
	statePermission
	stateMenu
)

type CLI struct {
	// agent is the concrete local owner this adapter constructs and shuts down;
	// owner is the same object typed for the concrete-only surface (ShutdownOwner).
	// Most CLI code uses the AdapterService interface, so agent keeps that type.
	agent agent.AdapterService
	owner *agent.Agent
	scope *agent.AdapterScope
	out   io.Writer
	mu    *sync.Mutex

	viewOnce sync.Once
	view     *agent.SessionView

	width    int
	oldState *term.State
	rawFd    int

	state     cliState
	busy      bool
	input     *inputLine
	history   inputHistory
	readKeyFn func() (keyMsg, error)
	ctx       context.Context

	// Exit latch: requestExit stores the exit error and closes exitLatch once. Every
	// key read (mainLoop and nested menus) and mainLoop's select observe it, so a
	// signal or stdin EOF unwinds all of them and reaches shutdown.
	exitOnce  sync.Once
	exitLatch chan struct{}
	exitErr   error

	// Key ingest spine: readKeys appends parsed keys here and wakes a reader; the key
	// reader (mainLoop or a nested menu) pops without blocking on a bounded channel,
	// so a stalled mainLoop can never wedge the reader mid-send. Closed on stdin EOF
	// and host exit.
	keyMu     sync.Mutex
	keyFrames []keyMsg
	keyWake   chan struct{}
	keyClosed bool

	// Event delivery spine: the owner callback appends every event here and wakes
	// mainLoop, which is the sole drainer. A synchronous owner call may emit more
	// events than the former bounded channel held while mainLoop is stalled, so the
	// callback only appends and never blocks. Producers never render.
	eventMu     sync.Mutex
	eventFrames []agent.Event
	eventWake   chan struct{}
	eventClosed bool

	modelRef string

	animStop  chan struct{}
	animLabel string

	streamStarted       bool
	streamDisplayActive bool
	streamNeedsNL       bool
	streamBuf           strings.Builder
	streamVisibleBuf    strings.Builder

	// activeToolID is the tool whose header is the most-recently-rendered
	// terminal line (set on ToolCallStart, consumed by its ToolCallEnd,
	// cleared by any other terminal-writing event). A ToolCallEnd matching it
	// is an inline completion (safe to rewrite in place with a cursor-up);
	// a non-matching end (e.g. a staged-flush result arriving at turn end,
	// far below its row) is rendered as a fresh block instead.
	activeToolID string

	// queueDisplay mirrors the backend-owned input queue (updated from
	// EventQueueChanged / QueueSnapshot); lastQueueVersion drops stale snapshots.
	queueDisplay     []agent.QueuedItem
	lastQueueVersion int

	permQueue       []*agent.PermissionRequest
	permSelected    int
	permPromptLines int
	permFrame       transientMenuFrame

	toolExpanded bool
	promptLines  int

	messages []displayEntry

	compacting          bool
	lastWarningSnapshot map[string]bool
}

func New(a *agent.Agent) *CLI {
	c := &CLI{
		out:                 os.Stdout,
		mu:                  &sync.Mutex{},
		input:               newInputLine(),
		history:             newInputHistory(),
		keyWake:             make(chan struct{}, 1),
		eventWake:           make(chan struct{}, 1),
		exitLatch:           make(chan struct{}),
		width:               80,
		lastWarningSnapshot: make(map[string]bool),
	}
	// A nil concrete agent must leave the interface field nil (not an interface
	// wrapping a nil pointer), so tests that construct a headless CLI still see no
	// owner.
	if a != nil {
		c.agent = a
		c.owner = a
		c.scope = agent.NewAdapterScope(a, a.ProjectRoot())
	}
	return c
}

// enqueueEvent appends one owner event and wakes mainLoop. It never blocks or
// renders, so it is safe to call from the owner's event callback while a
// synchronous owner call is in flight and mainLoop is stalled.
func (c *CLI) enqueueEvent(ev agent.Event) {
	c.eventMu.Lock()
	if c.eventClosed {
		c.eventMu.Unlock()
		return
	}
	c.eventFrames = append(c.eventFrames, ev)
	c.eventMu.Unlock()
	select {
	case c.eventWake <- struct{}{}:
	default:
	}
}

// drainEvents renders every queued event in FIFO order. It runs on mainLoop's
// goroutine and pops under eventMu but renders (handleEvent takes the render mutex)
// without holding it, so the callback can keep appending while events render.
func (c *CLI) drainEvents() {
	for {
		c.eventMu.Lock()
		if len(c.eventFrames) == 0 {
			c.eventMu.Unlock()
			return
		}
		ev := c.eventFrames[0]
		c.eventFrames = c.eventFrames[1:]
		c.eventMu.Unlock()
		c.handleEvent(ev)
	}
}

// closeEvents rejects further owner events. Shutdown joins the owner before calling
// it, so a cancelled turn's terminal events are admitted during the join rather than
// dropped; the CLI is exiting, so those final events are not rendered.
func (c *CLI) closeEvents() {
	c.eventMu.Lock()
	c.eventClosed = true
	c.eventMu.Unlock()
}

// enqueueKey appends one parsed key and wakes a reader. It never blocks, so a
// stalled mainLoop cannot wedge the key reader in a send and keep it from its next
// stdin read.
func (c *CLI) enqueueKey(k keyMsg) {
	c.keyMu.Lock()
	if c.keyClosed {
		c.keyMu.Unlock()
		return
	}
	c.keyFrames = append(c.keyFrames, k)
	c.keyMu.Unlock()
	select {
	case c.keyWake <- struct{}{}:
	default:
	}
}

// popKey removes the next buffered key, reporting whether one was available. Once
// the exit latch is set it reports empty even with keys buffered, so mainLoop and
// nested menus stop dequeuing and unwind to shutdown; the latch check is under keyMu,
// so it is atomic with requestExit's latch close and no key is dequeued after exit.
// This is the exit's "stop ingress" boundary: a key already dequeued when the latch
// closes completes its handling (pop and handling cannot be atomic — handling blocks
// on nested menus and owner calls), then the next read observes the latch and unwinds.
func (c *CLI) popKey() (keyMsg, bool) {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	select {
	case <-c.exitLatch:
		return keyMsg{}, false
	default:
	}
	if len(c.keyFrames) == 0 {
		return keyMsg{}, false
	}
	k := c.keyFrames[0]
	c.keyFrames = c.keyFrames[1:]
	return k, true
}

// closeKeys rejects further keys and wakes any waiting reader so it re-checks and
// observes the exit latch. Called on stdin EOF and at host exit.
func (c *CLI) closeKeys() {
	c.keyMu.Lock()
	c.keyClosed = true
	c.keyMu.Unlock()
	select {
	case c.keyWake <- struct{}{}:
	default:
	}
}

// nextKey is the single key source shared by mainLoop and every nested menu. popKey
// enforces exit priority (it reports empty once the latch is set), so a returned key
// is always one the exit latch had not yet claimed; otherwise nextKey reports the
// exit error, or the context error on cancellation.
func (c *CLI) nextKey(ctx context.Context) (keyMsg, error) {
	for {
		if k, ok := c.popKey(); ok {
			return k, nil
		}
		select {
		case <-c.exitLatch:
			return keyMsg{}, c.exitErr
		default:
		}
		select {
		case <-c.keyWake:
		case <-c.exitLatch:
			return keyMsg{}, c.exitErr
		case <-ctx.Done():
			return keyMsg{}, ctx.Err()
		}
	}
}

type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit %d", e.Code)
}

func (e ExitError) ExitCode() int {
	return e.Code
}

func (c *CLI) requestExit(err error) {
	c.exitOnce.Do(func() {
		// Close the latch under keyMu so it is atomic with popKey's latch check: a
		// key read either observes the open latch and pops, or observes the closed
		// latch and unwinds — never pops a key after exit was requested.
		c.keyMu.Lock()
		c.exitErr = err
		close(c.exitLatch)
		c.keyMu.Unlock()
	})
}

func (c *CLI) sv() *agent.SessionView {
	c.viewOnce.Do(func() { c.view = agent.NewSessionView(c.agent) })
	return c.view
}

func (c *CLI) setCurrentSessionID(id string) {
	c.sv().SetCurrent(id)
}

func (c *CLI) currentSessionID() string {
	return c.sv().Current()
}

func (c *CLI) currentSession() (string, error) {
	return c.sv().CurrentOrErr()
}

func (c *CLI) clearRemovedCurrent(id string) {
	if c.sv().RemovedCurrent(id) {
		c.refreshSession()
	}
}

func (c *CLI) resolveSessionID(id string) (string, error) {
	return c.sv().Resolve(id)
}

func (c *CLI) acceptsSessionEvent(sessionID string) bool {
	return c.sv().AcceptsSessionEvent(sessionID)
}

func (c *CLI) acceptsEvent(ev agent.Event) bool {
	return c.sv().AcceptsEvent(ev)
}

func (c *CLI) acceptsSubagentEvent(ev agent.Event) bool {
	return c.sv().AcceptsSubagentEvent(ev)
}

func (c *CLI) acceptsSubagentEventForCurrent(current string, ev agent.Event) bool {
	return c.sv().AcceptsSubagentEventForCurrent(current, ev)
}

func (c *CLI) liveCurrentSessionID() string {
	return c.sv().LiveCurrent()
}

func (c *CLI) currentSessionSummary() agent.SessionSummary {
	return c.sv().CurrentSummary()
}

func (c *CLI) currentModel() agent.ModelInfo {
	id, err := c.currentSession()
	if err != nil {
		return agent.ModelInfo{}
	}
	model, err := c.agent.CurrentModelForSession(id)
	if err != nil {
		return agent.ModelInfo{}
	}
	return model
}

func (c *CLI) tokenUsage() agent.TokenReport {
	return c.sv().TokenUsage()
}

func (c *CLI) sessionMessages() []agent.DisplayMessage {
	id := c.currentSessionID()
	if id == "" {
		return nil
	}
	if _, err := c.agent.SessionSummaryForSession(id); err != nil {
		c.setCurrentSessionID("")
		return nil
	}
	msgs, err := c.agent.SessionMessagesFor(id)
	if err != nil {
		return nil
	}
	return msgs
}

func (c *CLI) queueSnapshot() agent.QueueState {
	id := c.currentSessionID()
	if id == "" {
		return agent.QueueState{}
	}
	q, err := c.agent.QueueSnapshotForSession(id)
	if err != nil {
		return agent.QueueState{}
	}
	return q
}

func (c *CLI) cancelCurrent() {
	if id, err := c.currentSession(); err == nil {
		_ = c.agent.CancelSession(id)
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
						c.cancelCurrent()
					} else {
						fmt.Fprint(os.Stdout, "\r\n\x1b[?25h")
						c.requestExit(ExitError{Code: 130})
					}
				case syscall.SIGTERM:
					fmt.Fprint(os.Stdout, "\r\n\x1b[?25h")
					c.requestExit(ExitError{Code: 130})
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	c.ctx = ctx
	c.readKeyFn = func() (keyMsg, error) { return c.nextKey(ctx) }

	c.agent.SetEventHandler(c.enqueueEvent)

	c.agent.Init(ctx)
	sessionID := ""
	if sessions, err := c.scope.SessionList("active"); err == nil && len(sessions) > 0 {
		if summary, err := c.agent.OpenSession(sessions[0].ID); err == nil {
			sessionID = summary.ID
		}
	}
	if sessionID == "" {
		if id, err := c.scope.NewSession("primary"); err == nil {
			sessionID = id
		}
	}
	c.setCurrentSessionID(sessionID)

	c.refreshState()

	msgs := c.sessionMessages()
	c.messages = buildDisplayMsgs(msgs)
	c.seedInputHistory()

	c.printHeader()

	for _, m := range c.messages {
		c.printDisplayEntry(m)
	}

	c.printInputPrompt()

	go c.readKeys(ctx)

	err = c.mainLoop(ctx)

	// Teardown in the one host-shutdown order: stop ingress (close key admission and
	// cancel the input/signal goroutines), join the owner so a cancelled turn's
	// terminal events are still admitted, then close delivery. restoreTerminal runs
	// via the deferred cleanup, after the owner join.
	c.closeKeys()
	cancel()
	if c.owner != nil {
		c.owner.ShutdownOwner()
	}
	c.closeEvents()
	return err
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
			// stdin ended: set the exit latch BEFORE closing key admission, so the
			// wake closeKeys issues can never let mainLoop or a nested menu pop and
			// act on a buffered key while the latch still looks open. The latch is
			// established first, so a full key backlog unwinds to shutdown instead.
			c.requestExit(ExitError{Code: 0})
			c.closeKeys()
			return
		}
		for _, k := range parseInputBytes(buf[:n]) {
			c.enqueueKey(k)
		}
	}
}

func (c *CLI) mainLoop(ctx context.Context) error {
	for {
		select {
		case <-c.keyWake:
			// Drain buffered keys. popKey reports empty once the exit latch is set,
			// so this loop stops draining at exit and the outer select returns.
			for {
				k, ok := c.popKey()
				if !ok {
					break
				}
				if err := c.handleKey(k); err != nil {
					return err
				}
			}
		case <-c.eventWake:
			c.drainEvents()
		case <-c.exitLatch:
			return c.exitErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *CLI) handleKey(k keyMsg) error {
	switch c.state {
	case stateIdle:
		return c.handleKeyIdle(k)
	case stateStreaming:
		c.handleKeyStreaming(k)
	case statePermission:
		c.handleKeyPermission(k)
	case stateMenu:
	}
	return nil
}

func (c *CLI) handleInputEdit(k keyMsg, redrawPrompt bool) bool {
	switch k.Special {
	case keyBackspace:
		if c.input.DeleteBack() {
			c.history.Edited()
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	case keyDelete:
		if c.input.DeleteForward() {
			c.history.Edited()
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	case keyLeft:
		if c.input.MoveLeft() {
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	case keyRight:
		if c.input.MoveRight() {
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	case keyHome:
		c.input.MoveHome()
		if redrawPrompt {
			c.printInputPrompt()
		}
		return true
	case keyEnd:
		c.input.MoveEnd()
		if redrawPrompt {
			c.printInputPrompt()
		}
		return true
	case keyUp:
		if text, ok := c.history.Prev(c.input.String()); ok {
			c.input.Set(text)
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	case keyDown:
		if text, ok := c.history.Next(); ok {
			c.input.Set(text)
			if redrawPrompt {
				c.printInputPrompt()
			}
		}
		return true
	default:
		if k.Rune != 0 {
			c.input.Insert(k.Rune)
			c.history.Edited()
			if redrawPrompt {
				c.printInputPrompt()
			}
			return true
		}
		return false
	}
}

func (c *CLI) handleKeyIdle(k keyMsg) error {
	if c.handleInputEdit(k, true) {
		return nil
	}
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
			return nil
		}
		c.history.Add(text)

		if strings.HasPrefix(text, "/") {
			c.printLine(colorDim + "> " + text + colorReset)
			if err := c.dispatchCommand(text); err != nil {
				return err
			}
			c.printInputPrompt()
			return nil
		}

		c.submitInput(text)

	case keyTab:
		before := c.input.String()
		completed := completeSlashCommand(c.input.String())
		c.input.Set(completed)
		if completed != before {
			c.history.Edited()
		}
		c.printInputPrompt()

	case keyCtrlC, keyCtrlD:
		return ExitError{Code: 0}
	}
	return nil
}

func (c *CLI) handleKeyStreaming(k keyMsg) {
	if c.handleInputEdit(k, false) {
		return
	}
	switch k.Special {
	case keyCtrlC, keyEscape:
		c.cancelCurrent()
	case keyEnter:
		text := c.input.String()
		if text != "" {
			c.input.Clear()
			c.history.Add(text)
			if strings.HasPrefix(text, "/") {
				c.handleSlashWhileBusy(text)
			} else {
				// Queue via the backend; it appends and emits queue_changed.
				c.submitToBackend(text)
			}
		}
	}
}

func (c *CLI) handleKeyPermission(k keyMsg) {
	if len(c.permQueue) == 0 {
		return
	}

	req := c.permQueue[0]

	switch k.Special {
	case keyUp:
		if c.permSelected > 0 {
			c.permSelected--
			c.redrawPermissionBlock(req)
		}
		return
	case keyDown:
		if c.permSelected < permissionActionCount(req)-1 {
			c.permSelected++
			c.redrawPermissionBlock(req)
		}
		return
	case keyEnter:
		c.choosePermission(req)
		return
	case keyCtrlC, keyEscape:
		c.popAndRespond(req, false)
		return
	}

	switch k.Rune {
	case 'y', 'Y':
		c.popAndRespond(req, true)
	case 'n', 'N':
		c.popAndRespond(req, false)
	case 'p', 'P':
		if !req.DisableProjectSave {
			c.showPermissionSuggestions(req)
		}
	case 'a', 'A':
		if req.CanAllowAll {
			c.popAndRespondAction(req, "allow_all")
		}
	}
}

func (c *CLI) choosePermission(req *agent.PermissionRequest) {
	actions := permissionActions(req)
	if c.permSelected < 0 || c.permSelected >= len(actions) {
		return
	}
	action, _ := actions[c.permSelected].extra.(string)
	switch action {
	case "allow":
		c.popAndRespond(req, true)
	case "deny":
		c.popAndRespond(req, false)
	case "project":
		c.showPermissionSuggestions(req)
	case "allow_all":
		c.popAndRespondAction(req, "allow_all")
	}
}

func (c *CLI) popAndRespond(req *agent.PermissionRequest, allow bool) {
	action := "deny"
	if allow {
		action = "allow"
	}
	c.popAndRespondAction(req, action)
}

func (c *CLI) popAndRespondAction(req *agent.PermissionRequest, action string) {
	c.mu.Lock()
	c.erasePermissionBlockLocked()
	c.advancePermissionQueueLocked()
	c.mu.Unlock()

	sessionID, err := c.resolveSessionID(req.SessionID)
	if err != nil {
		return
	}
	_ = c.agent.RespondPermissionActionForSession(sessionID, req.ID, action)
}

func (c *CLI) showPermissionSuggestions(req *agent.PermissionRequest) {
	suggestArg := req.Arg
	if req.ResolvedArg != "" {
		suggestArg = req.ResolvedArg
	}
	var (
		suggestions []agent.PermissionSuggestion
		err         error
	)
	if req.ProjectID != "" {
		suggestions, err = c.agent.PermissionSuggestForProject(req.ProjectID, req.ToolName, suggestArg)
	} else {
		sessionID, resolveErr := c.resolveSessionID(req.SessionID)
		if resolveErr != nil {
			err = resolveErr
		} else {
			suggestions, err = c.agent.PermissionSuggestForSession(sessionID, req.ToolName, suggestArg)
		}
	}
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		c.printPermissionBlock(req)
		return
	}
	if len(suggestions) == 0 {
		c.mu.Lock()
		c.erasePermissionBlockLocked()
		c.mu.Unlock()
		c.printLine(renderErrorMsg("no suggestions available"))
		c.printPermissionBlock(req)
		return
	}

	c.mu.Lock()
	c.erasePermissionBlockLocked()
	c.mu.Unlock()

	var suggestionFrame transientMenuFrame
	renderSuggestions := func(selected int) {
		c.mu.Lock()
		width := c.currentWidth()
		suggestionFrame.draw(c.writeRaw, renderPermissionSuggestions(req, suggestions, selected, width), width)
		c.mu.Unlock()
	}
	redrawSuggestions := func(selected int) {
		c.mu.Lock()
		width := c.currentWidth()
		suggestionFrame.draw(c.writeRaw, renderPermissionSuggestions(req, suggestions, selected, width), width)
		c.mu.Unlock()
	}
	eraseSuggestions := func() {
		c.mu.Lock()
		suggestionFrame.clear(c.writeRaw)
		c.mu.Unlock()
	}

	selected := 0
	renderSuggestions(selected)

	for {
		k, err := c.readKeyFn()
		if err != nil {
			c.popAndRespond(req, false)
			return
		}

		switch k.Special {
		case keyEscape, keyCtrlC:
			eraseSuggestions()
			c.printPermissionBlock(req)
			return
		case keyEnter:
			patterns := []string{suggestions[selected].Rule}
			eraseSuggestions()
			sessionID, err := c.resolveSessionID(req.SessionID)
			if err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				c.printPermissionBlock(req)
				return
			}
			if err := c.agent.SaveProjectPermissionForSession(sessionID, req.ID, patterns); err != nil {
				c.printLine(renderErrorMsg(err.Error()))
				c.printPermissionBlock(req)
				return
			}
			c.mu.Lock()
			c.advancePermissionQueueLocked()
			c.mu.Unlock()
			return
		case keyUp:
			if selected > 0 {
				selected--
				redrawSuggestions(selected)
			}
		case keyDown:
			if selected < len(suggestions)-1 {
				selected++
				redrawSuggestions(selected)
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
	if !c.acceptsEvent(ev) {
		return
	}

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
		c.streamDisplayActive = false
		c.streamBuf.Reset()
		c.streamVisibleBuf.Reset()

	case agent.EventTextDelta:
		c.activeToolID = ""
		if !c.streamDisplayActive {
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
			c.streamDisplayActive = true
		}
		c.streamStarted = true
		text := strings.ReplaceAll(ev.Result, "\n", "\r\n")
		c.writeRaw(text)
		c.streamBuf.WriteString(ev.Result)
		c.streamVisibleBuf.WriteString(ev.Result)
		c.streamNeedsNL = !strings.HasSuffix(ev.Result, "\n")

	case agent.EventToolCallStart:
		if c.streamDisplayActive && c.streamNeedsNL {
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
			id:   ev.ToolCallID,
			name: ev.ToolName,
			args: ev.Args,
		})
		c.activeToolID = ev.ToolCallID
		c.writeRaw(renderToolCall(ev.ToolName, ev.Args, nil))
		c.startAnimationLocked("Running")

	case agent.EventToolCallEnd:
		c.stopAnimationLocked()
		inline := ev.ToolCallID == c.activeToolID
		c.activeToolID = ""
		// Model update is last-end-wins (no !done guard): a staged edit emits
		// a second ToolCallEnd (the real result, at flush) after its stage-time
		// "Staged." end; the real result must overwrite the row to match reload.
		var row *displayEntry
		for i := len(c.messages) - 1; i >= 0; i-- {
			if c.messages[i].typ == "tool" && c.messages[i].id == ev.ToolCallID {
				c.messages[i].done = true
				c.messages[i].success = !ev.IsError
				c.messages[i].result = ev.Result
				if ev.ToolName != "" {
					c.messages[i].name = ev.ToolName
				}
				if ev.Args != "" {
					c.messages[i].args = ev.Args
				}
				c.messages[i].metadata = ev.Metadata
				row = &c.messages[i]
				break
			}
		}
		if inline {
			// The tool header is the line just rendered: rewrite it in place.
			c.writeRaw("\r\x1b[2K")
			if ev.ToolName == "edit_file" {
				if _, hasSummary := toolChangeSummary(ev.ToolName, ev.Args, ev.Metadata); hasSummary {
					c.writeRaw("\x1b[1A\r\x1b[2K")
					c.writeRaw(renderToolCall(ev.ToolName, ev.Args, ev.Metadata))
				}
			}
			c.writeRaw(renderToolResult(ev.ToolName, ev.Args, ev.Result, !ev.IsError, c.toolExpanded, c.width, ev.Metadata))
		} else {
			// Late completion (e.g. a staged-flush result arriving at turn end,
			// far below its row): render a fresh block without cursor-relative
			// rewrite. Render the header from the MODEL row (the reload-equivalent
			// source), since the staged-flush ToolCallEnd carries no Args and the
			// row retains the original path from ToolCallStart.
			name, args, meta := ev.ToolName, ev.Args, ev.Metadata
			if row != nil {
				name, args, meta = row.name, row.args, row.metadata
			}
			c.writeRaw("\r\x1b[2K")
			c.writeRaw(renderToolCall(name, args, meta))
			c.writeRaw(renderToolResult(name, args, ev.Result, !ev.IsError, c.toolExpanded, c.width, meta))
		}
		c.writeRaw(nl)
		if c.busy {
			c.startAnimationLocked("Thinking")
		}

	case agent.EventBackgroundProcessComplete:
		c.activeToolID = ""
		if c.streamDisplayActive && c.streamNeedsNL {
			c.writeRaw("\r\n")
		}
		if c.streamStarted {
			c.finalizeStreamBufLocked()
			c.streamStarted = false
		} else {
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
		}
		success := !ev.IsError
		id := ""
		if ev.BackgroundProcess != nil {
			id = ev.BackgroundProcess.ID
		}
		c.messages = append(c.messages, displayEntry{
			typ:     "background_process",
			id:      id,
			done:    true,
			success: success,
			result:  ev.Result,
			bg:      ev.BackgroundProcess,
		})
		c.writeRaw(renderBackgroundProcessCall(ev.BackgroundProcess, success))
		if result := renderToolResult("background_process", "", ev.Result, success, c.toolExpanded, c.width, nil); result != "" {
			c.writeRaw(result)
			c.writeRaw(nl)
		}
		if c.busy {
			c.startAnimationLocked("Thinking")
		}

	case agent.EventUserMessageDisplay:
		c.activeToolID = ""
		if c.streamDisplayActive && c.streamNeedsNL {
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
			typ:     "user",
			content: ev.Result,
			turn:    ev.Turn,
		})
		c.printLineLocked(renderUserMsg(ev.Result, c.width))
		if c.busy {
			c.startAnimationLocked("Thinking")
		}

	case agent.EventGenericSystemSignal:
		c.activeToolID = ""
		if c.streamDisplayActive && c.streamNeedsNL {
			c.writeRaw("\r\n")
		}
		if c.streamStarted {
			c.finalizeStreamBufLocked()
			c.streamStarted = false
		} else {
			c.stopAnimationLocked()
			c.writeRaw("\r\x1b[2K")
		}
		content := "System: " + ev.Result
		c.messages = append(c.messages, displayEntry{
			typ:     "system",
			content: content,
		})
		c.printLineLocked(renderSystemMsg(content))
		if c.busy {
			c.startAnimationLocked("Thinking")
		}

	case agent.EventQueueChanged:
		// Backend-owned queue: keep the display mirror current (drop stale
		// snapshots by version). The CLI does not render the queue inline.
		if ev.QueueVersion > c.lastQueueVersion {
			c.queueDisplay = ev.Queue
			c.lastQueueVersion = ev.QueueVersion
		}

	case agent.EventTurnEnd:
		c.activeToolID = ""
		c.stopAnimationLocked()
		c.erasePermissionBlockLocked()
		if c.streamDisplayActive && c.streamNeedsNL {
			c.writeRaw("\r\n")
		}
		c.writeRaw("\r\x1b[2K")
		if c.streamStarted {
			c.finalizeStreamBufLocked()
		}
		c.busy = false
		c.state = stateIdle
		c.streamStarted = false
		c.streamDisplayActive = false
		c.permQueue = nil
		c.permSelected = 0
		c.permPromptLines = 0
		c.permFrame = transientMenuFrame{}
		// Queue draining is backend-owned: the agent auto-drains after the turn
		// ends and emits turn_start for the next turn. The CLI just re-prompts.
		if ev.RefreshSession {
			c.refreshSessionLocked()
		} else {
			c.printInputPromptLocked()
		}

	case agent.EventError:
		c.activeToolID = ""
		c.stopAnimationLocked()
		c.erasePermissionBlockLocked()
		c.writeRaw("\r\x1b[2K")
		c.writeRaw(renderErrorMsg(ev.Error))
		c.busy = false
		c.state = stateIdle
		c.printInputPromptLocked()

	case agent.EventPermissionRequest:
		c.activeToolID = ""
		c.stopAnimationLocked()
		c.writeRaw("\r\x1b[2K")
		c.permQueue = append(c.permQueue, ev.PermReq)
		if len(c.permQueue) == 1 {
			c.state = statePermission
			c.permSelected = 0
			c.printPermissionBlockLocked(ev.PermReq)
		}

	case agent.EventUsage:

	case agent.EventCompactionStart:
		c.compacting = true

	case agent.EventCompactionEnd:
		c.compacting = false
		if ev.RefreshSession {
			c.refreshSessionLocked()
		}

	case agent.EventWarning:
		c.activeToolID = ""
		c.writeRaw("\r\x1b[2K")
		current := make(map[string]bool, len(ev.Warnings))
		for _, w := range ev.Warnings {
			key := warningKey(w)
			current[key] = true
			if c.lastWarningSnapshot[key] {
				continue
			}
			c.writeRaw(renderWarningMsg(w.Kind + ": " + w.Message))
		}
		c.lastWarningSnapshot = current
		if c.state == stateIdle {
			c.printInputPromptLocked()
		}
	}
}

func warningKey(w agent.PromptWarning) string {
	return w.Kind + "\x00" + w.Message
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
		c.writeRaw(renderSubagentMsg(tag, fmt.Sprintf("▸ %s  %s", ev.ToolName, formatToolArgs(ev.ToolName, ev.Args))))
	case agent.EventToolCallEnd:
		result := truncate(ev.Result, 200)
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		line := fmt.Sprintf("%s: %s", status, result)
		if !ev.IsError && ev.ToolName == "edit_file" {
			line = status
			if preview, ok := editpreview.FromMetadata(ev.Metadata); ok {
				line += "\n" + strings.TrimRight(renderEditPreview(preview, "", c.width, false), "\r\n")
			}
		}
		c.writeRaw(renderSubagentMsg(tag, line))
	case agent.EventBackgroundProcessComplete:
		result := truncate(ev.Result, 200)
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		if ev.BackgroundProcess == nil {
			c.writeRaw(renderSubagentMsg(tag, fmt.Sprintf("background_process %s: %s", status, result)))
			return
		}
		line := fmt.Sprintf(
			"background_process %s %s %s exit %d: %s",
			ev.BackgroundProcess.ID,
			status,
			ev.BackgroundProcess.Reason,
			ev.BackgroundProcess.ExitCode,
			result,
		)
		c.writeRaw(renderSubagentMsg(tag, line))
	}
}

func (c *CLI) finalizeStreamBufLocked() {
	text := c.streamBuf.String()
	if text != "" {
		rows := terminalRowsForText(c.streamVisibleBuf.String(), c.width)
		if rows > 0 {
			c.writeRaw(eraseBlock(rows + 1))
		}
		c.writeRaw(renderAssistantMsg(text, c.width))

		c.messages = append(c.messages, displayEntry{
			typ:     "assistant",
			content: text,
		})
	}
	c.streamBuf.Reset()
	c.streamVisibleBuf.Reset()
	c.streamDisplayActive = false
}

func terminalRowsForText(text string, width int) int {
	if text == "" {
		return 0
	}
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return 1
	}
	rawLines := strings.Split(trimmed, "\n")
	rows := 0
	for _, rl := range rawLines {
		w := visibleWidth(rl)
		if width > 0 && w > width {
			rows += (w + width - 1) / width
		} else {
			rows++
		}
	}
	return rows
}

func (c *CLI) refreshState() {
	m := c.currentModel()
	c.modelRef = m.Ref
	if c.modelRef == "" && m.Provider != "" && m.Model != "" {
		c.modelRef = m.Provider + "/" + m.Model
	}
}

func (c *CLI) currentWidth() int {
	if c.rawFd > 0 {
		if w, _, err := term.GetSize(c.rawFd); err == nil && w > 0 {
			c.width = w
		}
	}
	if c.width < 30 {
		return 30
	}
	return c.width
}

func (c *CLI) seedInputHistory() {
	var entries []string
	for _, m := range c.messages {
		if m.typ == "user" && strings.TrimSpace(m.content) != "" {
			entries = append(entries, m.content)
		}
	}
	c.history.Reset(entries)
}

func (c *CLI) printHeader() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printHeaderLocked()
}

func (c *CLI) printHeaderLocked() {
	session := c.currentSessionSummary()
	sid := session.ID
	if sid == "" {
		sid = "(no session)"
	}
	header := fmt.Sprintf("  %s  %s  %s", c.scope.ProjectName(), sid, c.modelRef)
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
	cursorLen := 2 + visibleWidth(c.input.CursorText())
	endLen := 2 + visibleWidth(text)
	if back := endLen - cursorLen; back > 0 {
		c.writeRaw(fmt.Sprintf("\x1b[%dD", back))
	}

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
		c.printLineLocked(renderToolCall(e.name, e.args, e.metadata))
		if e.done {
			result := renderToolResult(e.name, e.args, e.result, e.success, c.toolExpanded, c.width, e.metadata)
			if result != "" {
				c.printLineLocked(result)
			}
			c.writeRaw(nl)
		}
	case "background_process":
		c.printLineLocked(renderBackgroundProcessCall(e.bg, e.success))
		if e.done {
			result := renderToolResult("background_process", "", e.result, e.success, c.toolExpanded, c.width, nil)
			if result != "" {
				c.printLineLocked(result)
			}
			c.writeRaw(nl)
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
	width := c.currentWidth()
	c.permFrame.draw(c.writeRaw, renderPermissionPrompt(req, c.permSelected, width), width)
	c.permPromptLines = permissionPromptRows(req)
}

func (c *CLI) erasePermissionBlockLocked() {
	c.permFrame.clear(c.writeRaw)
	c.permPromptLines = 0
}

func (c *CLI) advancePermissionQueueLocked() {
	if len(c.permQueue) > 0 {
		c.permQueue = c.permQueue[1:]
	}
	c.permSelected = 0
	if len(c.permQueue) > 0 {
		c.state = statePermission
		c.printPermissionBlockLocked(c.permQueue[0])
		return
	}
	c.state = stateStreaming
}

func (c *CLI) redrawPermissionBlock(req *agent.PermissionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printPermissionBlockLocked(req)
}

func (c *CLI) submitInput(text string) {
	c.mu.Lock()
	c.submitInputLocked(text)
	c.mu.Unlock()
}

func (c *CLI) submitInputLocked(text string) {
	// Idle submit: the backend starts the turn immediately (direct start).
	c.startAnimationLocked("Thinking")
	c.submitToBackend(text)
}

// submitToBackend routes input through the agent's single Submit entry point.
// The agent decides whether to start a turn or enqueue and emits queue_changed;
// the CLI does not own the queue. On error it renders the message and, if idle,
// restores the input prompt. Runs the call off the lock in a goroutine.
func (c *CLI) submitToBackend(text string) {
	go func() {
		sessionID, err := c.currentSession()
		if err == nil {
			_, err = c.agent.SubmitToSession(c.ctx, sessionID, text)
		}
		if err != nil {
			c.mu.Lock()
			c.handleSubmitErrorLocked(text, err)
			c.mu.Unlock()
		}
	}()
}

func (c *CLI) handleSubmitErrorLocked(text string, err error) {
	c.stopAnimationLocked()
	c.writeRaw("\r\x1b[2K")
	c.writeRaw(renderErrorMsg(err.Error()))
	if c.input != nil && c.input.String() == "" {
		c.input.Set(text)
	}
	if !c.busy {
		c.state = stateIdle
		c.printInputPromptLocked()
	}
}

func (c *CLI) refreshSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshSessionLocked()
}

func (c *CLI) refreshSessionLocked() {
	c.refreshState()
	msgs := c.sessionMessages()
	c.messages = buildDisplayMsgs(msgs)
	q := c.queueSnapshot()
	c.queueDisplay = q.Items
	c.lastQueueVersion = q.Version
	c.seedInputHistory()

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
	case "/clear":
		c.mu.Unlock()
		c.cmdClear()
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

func (c *CLI) dispatchCommand(text string) error {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/help":
		c.cmdHelp()
	case "/clear":
		c.cmdClear()
	case "/model":
		c.showModelMenu()
	case "/session":
		c.showSessionMenu()
	case "/project":
		c.showProjectMenu()
	case "/new":
		id, err := c.scope.NewSession("primary")
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return nil
		}
		c.setCurrentSessionID(id)
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
		return ExitError{Code: 0}
	default:
		c.printLine(renderErrorMsg(fmt.Sprintf("unknown command: %s", cmd)))
	}
	return nil
}

func (c *CLI) cmdHelp() {
	c.printLine("")
	c.printLine(colorDim + "  Commands:" + colorReset)
	c.printLine("  /help          show this help")
	c.printLine("  /clear         clear terminal output")
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

func (c *CLI) cmdClear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearTerminalLocked()
	c.printHeaderLocked()
}

func (c *CLI) clearTerminalLocked() {
	if c.promptLines > 1 {
		c.writeRaw(eraseBlock(c.promptLines))
	} else {
		c.writeRaw("\r\x1b[2K")
	}
	c.promptLines = 0
	c.streamDisplayActive = false
	c.streamVisibleBuf.Reset()
	c.streamNeedsNL = false
	c.writeRaw("\x1b[2J\x1b[3J\x1b[H")
}

func (c *CLI) cmdResume(parts []string) {
	if len(parts) > 1 {
		id := parts[1]
		summary, err := c.agent.OpenSession(id)
		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			return
		}
		c.setCurrentSessionID(summary.ID)
		c.refreshSession()
		return
	}

	sessions, err := c.scope.SessionList("active")
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}
	if len(sessions) == 0 {
		c.printLine(renderErrorMsg("no active sessions"))
		return
	}

	summary, err := c.agent.OpenSession(sessions[0].ID)
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}
	c.setCurrentSessionID(summary.ID)
	c.refreshSession()
}

func (c *CLI) cmdContext() {
	report := c.tokenUsage()

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
	sessionID := c.liveCurrentSessionID()
	if sessionID == "" {
		c.printLine(renderErrorMsg("no current session"))
		return
	}

	c.mu.Lock()
	c.compacting = true
	c.state = stateStreaming
	c.mu.Unlock()

	c.startAnimation("Compacting")

	go func() {
		err := c.agent.CompactNowForSession(c.ctx, sessionID)
		c.mu.Lock()
		idle := c.finishCompactLocked()
		c.mu.Unlock()

		if err != nil {
			c.printLine(renderErrorMsg(err.Error()))
			if idle {
				c.printInputPrompt()
			}
		}
	}()
}

func (c *CLI) finishCompactLocked() bool {
	c.stopAnimationLocked()
	c.writeRaw("\r\x1b[2K")
	c.compacting = false
	if c.busy {
		return false
	}
	c.state = stateIdle
	return true
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
	summary, err := c.scope.OpenOrCreateSession(targetPath)
	if err != nil {
		c.printLine(renderErrorMsg(err.Error()))
		return
	}
	c.scope.SetProjectPath(targetPath)
	c.setCurrentSessionID(summary.ID)
	c.refreshSession()
}

func (c *CLI) restoreTerminal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oldState != nil {
		_ = term.Restore(c.rawFd, c.oldState)
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
