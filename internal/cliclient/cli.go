// Package cliclient is the terminal presentation of the shared Runtime protocol.
package cliclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
	"golang.org/x/term"
)

type Options struct {
	Startup   clienthost.Options
	Workspace string
}

type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("terminal exit %d", e.Code) }
func (e ExitError) ExitCode() int { return e.Code }

type stamp struct {
	conn      *client.Client
	ctx       context.Context
	selection uint64
	workspace string
	root      string
}

type delivery struct {
	stamp          stamp
	text           string
	actual         bool
	connectionOnly bool
	listGeneration uint64
}
type eventDelivery struct {
	conn  *client.Client
	event protocol.Event
}

type CLI struct {
	mu                         sync.Mutex
	opts                       Options
	ctx                        context.Context
	cancel                     context.CancelFunc
	closed                     bool
	wg                         sync.WaitGroup
	conn                       *client.Client
	connCtx                    context.Context
	connCancel                 context.CancelFunc
	connecting                 bool
	selection                  uint64
	workspace, rootID, childID string
	root, child                *sessionReader
	listGeneration             uint64
	sessions                   []protocol.Session
	configuration              *protocol.ConfigurationView
	warnings                   *protocol.WarningsSnapshot
	globalsDirty, globalsBusy  bool
	draft                      *terminal.InputLine
	draftRevision              uint64
	history                    terminal.InputHistory
	queued                     bool
	forkBoundary               *protocol.InputItem
	forkRewind                 bool
	attempts                   map[string]*attempt
	unknownCreates             map[string]bool
	menu                       *menu
	width, promptRows          int
	promptBelow                int
	renderedReader             *sessionReader
	renderedVersion            uint64
	provisional                string
	provisionalTruncated       bool
	wake                       chan struct{}
	outputs                    chan delivery
	events                     chan eventDelivery
	keys                       chan terminal.KeyMsg
	exitMu                     sync.Mutex
	exit                       chan struct{}
	exitErr                    error
	exiting                    bool
}

func New(opts Options) *CLI {
	return &CLI{opts: opts, workspace: opts.Workspace, draft: terminal.NewInputLine(), history: terminal.NewInputHistory(),
		attempts: make(map[string]*attempt), unknownCreates: make(map[string]bool), width: 80,
		wake: make(chan struct{}, 1), outputs: make(chan delivery, 256), events: make(chan eventDelivery, 256),
		keys: make(chan terminal.KeyMsg, 4096), exit: make(chan struct{})}
}

func (c *CLI) requestExit(err error) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if !c.exiting {
		c.exiting = true
		c.exitErr = err
		close(c.exit)
	}
}

// enqueueKey admits one key under bounded backpressure: a full queue makes the
// input reader wait for the writer instead of ending the client, and a latched
// exit releases that wait. The exit latch is read, never held, across the wait.
func (c *CLI) enqueueKey(key terminal.KeyMsg) {
	c.exitMu.Lock()
	exiting := c.exiting
	c.exitMu.Unlock()
	if exiting {
		return
	}
	select {
	case c.keys <- key:
	case <-c.exit:
	}
}

func (c *CLI) readInput(input *os.File) {
	var buf [256]byte
	for {
		n, err := input.Read(buf[:])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) {
				c.requestExit(nil)
			} else {
				c.requestExit(err)
			}
			return
		}
		if n == 0 {
			c.requestExit(nil)
			return
		}
		for _, key := range terminal.ParseInputBytes(buf[:n]) {
			c.enqueueKey(key)
		}
		select {
		case <-c.exit:
			return
		default:
		}
	}
}

func (c *CLI) Run(ctx context.Context, input *os.File, output io.Writer) error {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	intCh, termCh, resizeCh := make(chan os.Signal, 2), make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(intCh, syscall.SIGINT)
	signal.Notify(termCh, syscall.SIGTERM)
	signal.Notify(resizeCh, syscall.SIGWINCH)
	defer signal.Stop(intCh)
	defer signal.Stop(termCh)
	defer signal.Stop(resizeCh)
	go func() {
		for {
			select {
			case sig := <-intCh:
				c.handleSignal(sig, int(input.Fd()))
			case sig := <-termCh:
				c.handleSignal(sig, int(input.Fd()))
			case sig := <-resizeCh:
				c.handleSignal(sig, int(input.Fd()))
			case <-c.ctx.Done():
				return
			}
		}
	}()
	var old *term.State
	if term.IsTerminal(int(input.Fd())) {
		var err error
		old, err = term.MakeRaw(int(input.Fd()))
		if err != nil {
			c.requestExit(err)
		}
		if w, _, err := term.GetSize(int(input.Fd())); err == nil && w > 0 {
			c.mu.Lock()
			c.width = w
			c.mu.Unlock()
		}
	}
	go c.readInput(input)
	go c.writer(output)
	c.task(func() {
		workspace, err := filepath.Abs(c.opts.Workspace)
		if err != nil {
			c.requestExit(err)
			return
		}
		c.mu.Lock()
		c.workspace = workspace
		c.mu.Unlock()
		c.connect(false, nil)
	})
	select {
	case <-c.exit:
	case <-ctx.Done():
		c.requestExit(ctx.Err())
	}
	c.mu.Lock()
	c.closed = true
	c.cancel()
	if c.connCancel != nil {
		c.connCancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
	// Native stdin/stdout may never return. Restoration does not join or write
	// through either of them; all cancellable protocol tasks are still joined.
	if old != nil {
		_ = term.Restore(int(input.Fd()), old)
	}
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	return c.exitErr
}

func (c *CLI) handleSignal(sig os.Signal, fd int) {
	switch sig {
	case syscall.SIGWINCH:
		if w, _, err := term.GetSize(fd); err == nil && w > 0 {
			c.mu.Lock()
			c.width = w
			c.notifyLocked()
			c.mu.Unlock()
		}
	case syscall.SIGINT:
		c.mu.Lock()
		r := c.displayedLocked()
		active := r != nil && r.hydration != nil && r.hydration.ActiveOperation != nil
		var id string
		if active {
			id = r.id
		}
		s := c.stampLocked()
		c.mu.Unlock()
		if active {
			c.control(s, "InterruptSession", id)
		} else {
			c.requestExit(ExitError{Code: 130})
		}
	case syscall.SIGTERM:
		c.requestExit(ExitError{Code: 130})
	}
}

func (c *CLI) task(fn func()) { c.mu.Lock(); c.taskLocked(fn); c.mu.Unlock() }
func (c *CLI) taskLocked(fn func()) {
	if c.closed {
		return
	}
	c.wg.Add(1)
	go func() { defer c.wg.Done(); fn() }()
}
func (c *CLI) notifyLocked() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *CLI) stampLocked() stamp {
	return stamp{c.conn, c.connCtx, c.selection, c.workspace, c.rootID}
}
func (c *CLI) currentLocked(s stamp) bool {
	return !c.closed && s.conn != nil && c.conn == s.conn && c.selection == s.selection
}

// connectionCurrentLocked is the Runtime-global validity scope: the serving
// connection alone, never the local Session selection.
func (c *CLI) connectionCurrentLocked(s stamp) bool {
	return !c.closed && s.conn != nil && c.conn == s.conn
}
func (c *CLI) displayedLocked() *sessionReader {
	if c.child != nil {
		return c.child
	}
	return c.root
}
func (c *CLI) clearProgressLocked() { c.provisional = ""; c.provisionalTruncated = false }

func (c *CLI) report(s stamp, text string, actual bool) {
	c.mu.Lock()
	if c.closed || (!actual && !c.currentLocked(s)) {
		c.mu.Unlock()
		return
	}
	overflow := false
	select {
	case c.outputs <- delivery{stamp: s, text: text, actual: actual}:
	default:
		overflow = true
	}
	c.mu.Unlock()
	if overflow {
		c.requestExit(errors.New("terminal output overflow"))
	}
}

// reportConnection delivers one Runtime-global outcome whose validity is its
// serving connection alone; a superseded connection's queued text is dropped.
func (c *CLI) reportConnection(s stamp, text string) {
	c.mu.Lock()
	if !c.connectionCurrentLocked(s) {
		c.mu.Unlock()
		return
	}
	overflow := false
	select {
	case c.outputs <- delivery{stamp: s, text: text, connectionOnly: true}:
	default:
		overflow = true
	}
	c.mu.Unlock()
	if overflow {
		c.requestExit(errors.New("terminal output overflow"))
	}
}

func (c *CLI) connect(repair bool, expected *client.Client) {
	c.mu.Lock()
	connCtx, cancel, ok := c.beginConnectionLocked(expected)
	c.mu.Unlock()
	if ok {
		c.openConnection(connCtx, cancel, repair)
	}
}

func (c *CLI) beginConnectionLocked(expected *client.Client) (context.Context, context.CancelFunc, bool) {
	if c.closed || c.connecting || (expected != nil && c.conn != expected) {
		return nil, nil, false
	}
	c.connecting = true
	if c.connCancel != nil {
		c.connCancel()
	}
	c.conn = nil
	c.resetReadersLocked()
	c.configuration = nil
	c.warnings = nil
	c.globalsDirty = true
	c.menu = nil
	c.sessions = nil
	c.listGeneration++
	connCtx, cancel := context.WithCancel(c.ctx)
	c.connCtx, c.connCancel = connCtx, cancel
	c.notifyLocked()
	return connCtx, cancel, true
}

func (c *CLI) openConnection(connCtx context.Context, cancel context.CancelFunc, repair bool) {
	conn, err := clienthost.Connect(connCtx, c.opts.Startup)
	if err != nil {
		cancel()
		c.mu.Lock()
		c.connecting = false
		c.mu.Unlock()
		c.requestExit(err)
		return
	}
	events, errs := conn.Events(connCtx)
	select {
	case err = <-errs:
		cancel()
		c.mu.Lock()
		c.connecting = false
		c.mu.Unlock()
		c.requestExit(fmt.Errorf("event stream establishment: %w", err))
		return
	default:
	}
	c.mu.Lock()
	if c.closed {
		c.connecting = false
		c.mu.Unlock()
		cancel()
		return
	}
	c.conn = conn
	c.connecting = false
	s := c.stampLocked()
	c.kickGlobalsLocked()
	rootID, childID := c.rootID, c.childID
	if rootID != "" {
		c.root = c.newReaderLocked(rootID)
		c.kickReaderLocked(c.root)
	}
	if childID != "" {
		c.child = c.newReaderLocked(childID)
		c.kickReaderLocked(c.child)
	}
	c.taskLocked(func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case err, ok := <-errs:
				if ok && err != nil {
					c.repair(conn)
				}
				return
			case event, ok := <-events:
				if !ok {
					c.repair(conn)
					return
				}
				select {
				case c.events <- eventDelivery{conn, event}:
				default:
					c.repair(conn)
					return
				}
			}
		}
	})
	c.mu.Unlock()
	if repair {
		c.report(s, terminal.RenderSystemMsg("Connection repaired — authoritative reads; missed activity is not replayed."), false)
	}
	if rootID == "" {
		c.compose(s)
	} else {
		c.refreshList(s, "open", false)
	}
}

func (c *CLI) repair(conn *client.Client) {
	c.mu.Lock()
	c.repairLocked(conn)
	c.mu.Unlock()
}

func (c *CLI) repairLocked(conn *client.Client) {
	ctx, cancel, ok := c.beginConnectionLocked(conn)
	if ok {
		c.taskLocked(func() { c.openConnection(ctx, cancel, true) })
	}
}
