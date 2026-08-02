package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/jsonrpc"
	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

const (
	stateIdle = iota
	stateStarting
	stateReady
	stateFailed
	stateShutdown

	maxRestarts = 3
	idleTimeout = 30 * time.Minute
)

// readyTimeout and shutdownWait are variables so tests can shorten the waits;
// they never change outside tests.
var (
	readyTimeout = 30 * time.Second
	shutdownWait = 5 * time.Second
)

type instance struct {
	def         *server.Definition
	projectRoot string
	home        string

	mu        sync.Mutex
	state     int
	rpc       *jsonrpc.Client
	cmd       *exec.Cmd
	procDone  chan struct{}
	restarts  int
	readyCh   chan struct{}
	idleTimer *time.Timer
	openedVer map[string]int

	diagMu      sync.RWMutex
	diagnostics map[string][]protocol.Diagnostic

	onCrash func(name string)
}

func newInstance(def *server.Definition, projectRoot, home string, onCrash func(string)) *instance {
	return &instance{
		def:         def,
		projectRoot: projectRoot,
		home:        home,
		diagnostics: make(map[string][]protocol.Diagnostic),
		openedVer:   make(map[string]int),
		readyCh:     make(chan struct{}),
		onCrash:     onCrash,
	}
}

// start claims a fresh launch and returns the ready channel of the attempt it
// claimed (or of the current attempt when it refuses to claim). The returned
// channel is the one the caller must wait on.
func (inst *instance) start(ctx context.Context) (chan struct{}, error) {
	inst.mu.Lock()
	switch inst.state {
	case stateStarting, stateFailed, stateReady:
		// Another launch is already under way, the instance failed and is not
		// startable again, or a launch is already running: do not launch.
		ch := inst.readyCh
		inst.mu.Unlock()
		return ch, nil
	}
	inst.state = stateStarting
	inst.readyCh = make(chan struct{})
	readyCh := inst.readyCh
	inst.mu.Unlock()

	binary := server.ResolveBinary(inst.home, inst.def)
	if binary == "" {
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("binary not found: %s", inst.def.Command)
	}

	cmd := exec.Command(binary, inst.def.Args...)
	cmd.Dir = inst.projectRoot
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("start %s: %w", inst.def.Name, err)
	}

	inst.mu.Lock()
	inst.cmd = cmd
	inst.procDone = make(chan struct{})
	procDone := inst.procDone
	inst.openedVer = make(map[string]int)
	inst.mu.Unlock()

	inst.diagMu.Lock()
	inst.diagnostics = make(map[string][]protocol.Diagnostic)
	inst.diagMu.Unlock()

	rpc := jsonrpc.NewClient(stdin, stdout, stderr, func(method string, params json.RawMessage) {
		// Bind the handler to this launch's ready channel so a notification
		// from an attempt that has since been replaced can never mark a newer
		// attempt ready.
		inst.handleNotification(method, params, readyCh)
	})
	rpc.Start()

	inst.mu.Lock()
	inst.rpc = rpc
	inst.mu.Unlock()

	pid := os.Getpid()
	initParams := buildInitializeParams(pid, inst.projectRoot)

	result, err := rpc.Call(ctx, "initialize", initParams)
	if err != nil {
		rpc.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(procDone)
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("initialize %s: %w", inst.def.Name, err)
	}
	_ = result

	if err := rpc.Notify("initialized", struct{}{}); err != nil {
		rpc.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(procDone)
		inst.endAttempt(readyCh)
		return nil, fmt.Errorf("initialized notification: %w", err)
	}

	inst.resetIdle()

	// #nosec G118 -- watchProcess is tied to the LSP process lifetime (cmd.Wait), not the start ctx; cleanup is via instance shutdown.
	go inst.watchProcess(cmd, procDone)

	return readyCh, nil
}

func buildInitializeParams(pid int, projectRoot string) protocol.InitializeParams {
	rootURI := protocol.URIFromPath(projectRoot)
	return protocol.InitializeParams{
		ProcessID: &pid,
		RootURI:   rootURI,
		Capabilities: protocol.ClientCapabilities{
			Workspace: protocol.WorkspaceClientCapabilities{WorkspaceFolders: true},
			Window:    protocol.WindowClientCapabilities{WorkDoneProgress: true},
		},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: rootURI, Name: filepath.Base(projectRoot)},
		},
	}
}

func (inst *instance) handleNotification(method string, params json.RawMessage, readyCh chan struct{}) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		inst.diagMu.Lock()
		inst.diagnostics[p.URI] = p.Diagnostics
		inst.diagMu.Unlock()

	case "$/progress":
		var p protocol.ProgressParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		var val protocol.WorkDoneProgressValue
		if err := json.Unmarshal(p.Value, &val); err != nil {
			return
		}
		if val.Kind == "end" {
			inst.markReady(readyCh)
		}

	case "window/logMessage", "window/showMessage":
		// ignore
	}
}

// markReady marks the launch that owns readyCh as ready. Under one lock hold
// it verifies the launch is still the current one and only then sets ready and
// closes the channel, so a notification from a replaced, failed or already
// finished attempt does nothing.
func (inst *instance) markReady(readyCh chan struct{}) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.readyCh != readyCh || inst.state != stateStarting {
		return
	}
	inst.state = stateReady
	close(readyCh)
}

// setTerminal records the given state and releases any caller waiting on the
// current ready channel. A failed start attempt ends by returning the instance
// to idle, keeping it claimable. The failed state is reserved for crash-budget
// exhaustion and is final: no later transition overwrites it, so shutdown
// cannot turn a crashed instance back into a restartable one.
func (inst *instance) setTerminal(state int) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.setTerminalLocked(state)
}

// setTerminalLocked performs the transition while inst.mu is held, closing the
// current ready channel when the previous state was starting.
func (inst *instance) setTerminalLocked(state int) {
	prev := inst.state
	if prev == stateFailed {
		return
	}
	inst.state = state
	if prev == stateStarting {
		close(inst.readyCh)
	}
}

// endAttempt returns the instance to idle, ending the launch that installed
// readyCh. It is a no-op unless that launch is still the current one and still
// starting, so it can neither clobber a shutdown nor a newer attempt. The
// identity check and the transition share one lock hold.
func (inst *instance) endAttempt(readyCh chan struct{}) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.readyCh != readyCh || inst.state != stateStarting {
		return
	}
	inst.setTerminalLocked(stateIdle)
}

func stateName(state int) string {
	switch state {
	case stateIdle:
		return "idle"
	case stateStarting:
		return "starting"
	case stateReady:
		return "ready"
	case stateFailed:
		return "failed"
	case stateShutdown:
		return "shutdown"
	}
	return fmt.Sprintf("unknown (%d)", state)
}

// watchProcess waits for the process of the attempt it was bound to at
// creation and applies that attempt's crash accounting. A watcher whose
// attempt has since been replaced is inert: it closes only the procDone it was
// bound to and touches nothing else.
func (inst *instance) watchProcess(cmd *exec.Cmd, procDone chan struct{}) {
	_ = cmd.Wait()

	inst.mu.Lock()
	close(procDone)

	// The attempt this watcher launched must still be the current one: if a
	// shutdown and restart replaced it, the watcher is superseded and must
	// not touch state, count a crash, or restart anything. The check and
	// everything it guards happen in this one hold.
	if inst.procDone != procDone {
		inst.mu.Unlock()
		return
	}
	if inst.state == stateFailed || inst.state == stateShutdown {
		inst.mu.Unlock()
		return
	}
	if rpc := inst.rpc; rpc != nil && !rpc.IsClosed() {
		rpc.Close()
	}
	if inst.restarts >= maxRestarts {
		// Budget exhausted: the terminal transition lands in the same hold
		// that checked the budget, so a shutdown cannot slip in between and
		// leave the state claimable.
		inst.setTerminalLocked(stateFailed)
		inst.mu.Unlock()
		if inst.onCrash != nil {
			inst.onCrash(inst.def.Name)
		}
		return
	}
	inst.restarts++
	// The attempt that just died is over: return the instance to idle so its
	// waiters are released and the restart below can claim it. Failed cannot
	// be used here — it is final and unclaimable by design.
	inst.setTerminalLocked(stateIdle)
	inst.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := inst.start(ctx); err != nil {
		// The restart attempt failed; start() already ended it by returning
		// the instance to idle, so it stays claimable.
		if inst.onCrash != nil {
			inst.onCrash(inst.def.Name)
		}
	}
}

func (inst *instance) waitReady(ctx context.Context) error {
	inst.mu.Lock()
	switch inst.state {
	case stateReady:
		inst.mu.Unlock()
		return nil
	case stateStarting:
		ch := inst.readyCh
		inst.mu.Unlock()
		return inst.waitForReady(ctx, ch)
	case stateFailed:
		// Failed is final: the restart budget is spent, so an ordinary wait
		// must not launch another process.
		inst.mu.Unlock()
		return fmt.Errorf("%s is unavailable", inst.def.Name)
	case stateShutdown:
		// Starting an instance that was shut down is an explicit restart and
		// gets a fresh crash budget; crash-restarts keep counting toward the
		// limit.
		inst.restarts = 0
	}
	inst.mu.Unlock()

	// Idle or shutdown: start a fresh launch. The start context is decoupled
	// from the caller so cancelling this wait does not abort the launch for
	// other callers, and bounded like the crash-restart path.
	startCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := inst.start(startCtx)
	if err != nil {
		return fmt.Errorf("%s start failed: %w", inst.def.Name, err)
	}
	return inst.waitForReady(ctx, ch)
}

func (inst *instance) waitForReady(ctx context.Context, ch chan struct{}) error {
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()

	select {
	case <-ch:
	case <-timer.C:
		// Report the state as it reads now rather than success; the attempt
		// is left alone — if the server announces readiness later the attempt
		// completes normally, and if it never does the idle path reaps it.
		inst.mu.Lock()
		state := inst.state
		inst.mu.Unlock()
		return fmt.Errorf("%s: timed out waiting for the server to become ready (state: %s)", inst.def.Name, stateName(state))
	case <-ctx.Done():
		return ctx.Err()
	}

	// The channel this caller waited on must still be current: if a newer
	// launch replaced it, the tracked launch ended without becoming ready and
	// the newer launch's state is not this caller's outcome.
	inst.mu.Lock()
	state := inst.state
	current := inst.readyCh
	inst.mu.Unlock()
	if state == stateReady && current == ch {
		return nil
	}
	return fmt.Errorf("%s is unavailable", inst.def.Name)
}

func (inst *instance) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	inst.mu.Lock()
	rpc := inst.rpc
	state := inst.state
	inst.mu.Unlock()

	if state == stateFailed || state == stateShutdown || rpc == nil {
		return nil, fmt.Errorf("%s is unavailable", inst.def.Name)
	}

	inst.resetIdle()

	result, err := rpc.Call(ctx, method, params)
	if err != nil {
		if rpc.IsClosed() {
			return nil, fmt.Errorf("%s connection lost: %w", inst.def.Name, err)
		}
		return nil, err
	}
	return result, nil
}

func (inst *instance) openFile(ctx context.Context, absPath string) error {
	inst.mu.Lock()
	ver := inst.openedVer[absPath]
	rpc := inst.rpc
	inst.mu.Unlock()

	if rpc == nil {
		return fmt.Errorf("%s is unavailable", inst.def.Name)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	if ver > 0 {
		newVer := ver + 1
		if err := rpc.Notify("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{
				"uri":     protocol.URIFromPath(absPath),
				"version": newVer,
			},
			"contentChanges": []map[string]any{
				{"text": string(content)},
			},
		}); err != nil {
			return err
		}
		inst.mu.Lock()
		inst.openedVer[absPath] = newVer
		inst.mu.Unlock()
		return nil
	}

	params := protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        protocol.URIFromPath(absPath),
			LanguageID: inst.def.LanguageID,
			Version:    1,
			Text:       string(content),
		},
	}
	if err := rpc.Notify("textDocument/didOpen", params); err != nil {
		return err
	}

	inst.mu.Lock()
	inst.openedVer[absPath] = 1
	inst.mu.Unlock()
	return nil
}

func (inst *instance) fileDiagnostics(uri string) []protocol.Diagnostic {
	inst.diagMu.RLock()
	defer inst.diagMu.RUnlock()
	return inst.diagnostics[uri]
}

func (inst *instance) resetIdle() {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
	}
	inst.idleTimer = time.AfterFunc(idleTimeout, func() {
		inst.shutdown()
	})
}

func (inst *instance) shutdown() {
	inst.mu.Lock()
	rpc := inst.rpc
	cmd := inst.cmd
	procDone := inst.procDone
	if inst.idleTimer != nil {
		inst.idleTimer.Stop()
		inst.idleTimer = nil
	}
	// The capture and the transition share one hold, so the transition can
	// only end the attempt captured above; a newer attempt that becomes
	// current later is left intact.
	inst.setTerminalLocked(stateShutdown)
	inst.mu.Unlock()

	if rpc == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	_, _ = rpc.Call(ctx, "shutdown", nil)
	_ = rpc.Notify("exit", nil)
	rpc.Close()

	if cmd != nil && cmd.Process != nil {
		select {
		case <-procDone:
		case <-time.After(shutdownWait):
			_ = cmd.Process.Kill()
			<-procDone
		}
	}

	// Clear the captured attempt's handles only if it is still the current
	// one — procDone is the field this shutdown actually waited on and the
	// identity of the attempt it captured.
	inst.mu.Lock()
	if inst.procDone == procDone {
		inst.rpc = nil
		inst.cmd = nil
		inst.openedVer = make(map[string]int)
		inst.mu.Unlock()

		inst.diagMu.Lock()
		inst.diagnostics = make(map[string][]protocol.Diagnostic)
		inst.diagMu.Unlock()
	} else {
		inst.mu.Unlock()
	}
}
