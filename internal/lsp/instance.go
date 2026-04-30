package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/lsp/jsonrpc"
	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

const (
	stateStarting = iota
	stateReady
	stateFailed
	stateShutdown

	maxRestarts   = 3
	idleTimeout   = 30 * time.Minute
	readyTimeout  = 30 * time.Second
	shutdownWait  = 5 * time.Second
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
	readyOnce sync.Once
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

func (inst *instance) start(ctx context.Context) error {
	binary := server.ResolveBinary(inst.home, inst.def)
	if binary == "" {
		return fmt.Errorf("binary not found: %s", inst.def.Command)
	}

	cmd := exec.Command(binary, inst.def.Args...)
	cmd.Dir = inst.projectRoot
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", inst.def.Name, err)
	}

	inst.mu.Lock()
	inst.cmd = cmd
	inst.procDone = make(chan struct{})
	inst.state = stateStarting
	inst.readyCh = make(chan struct{})
	inst.readyOnce = sync.Once{}
	inst.openedVer = make(map[string]int)
	inst.mu.Unlock()

	inst.diagMu.Lock()
	inst.diagnostics = make(map[string][]protocol.Diagnostic)
	inst.diagMu.Unlock()

	rpc := jsonrpc.NewClient(stdin, stdout, stderr, inst.handleNotification)
	rpc.Start()

	inst.mu.Lock()
	inst.rpc = rpc
	inst.mu.Unlock()

	pid := os.Getpid()
	initParams := protocol.InitializeParams{
		ProcessID: &pid,
		RootURI:   protocol.URIFromPath(inst.projectRoot),
		Capabilities: protocol.ClientCapabilities{
			Window: protocol.WindowClientCapabilities{WorkDoneProgress: true},
		},
	}

	result, err := rpc.Call(ctx, "initialize", initParams)
	if err != nil {
		rpc.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("initialize %s: %w", inst.def.Name, err)
	}
	_ = result

	if err := rpc.Notify("initialized", struct{}{}); err != nil {
		rpc.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("initialized notification: %w", err)
	}

	inst.resetIdle()

	go inst.watchProcess()

	return nil
}

func (inst *instance) handleNotification(method string, params json.RawMessage) {
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
			inst.markReady()
		}

	case "window/logMessage", "window/showMessage":
		// ignore
	}
}

func (inst *instance) markReady() {
	inst.readyOnce.Do(func() {
		inst.mu.Lock()
		inst.state = stateReady
		inst.mu.Unlock()
		close(inst.readyCh)
	})
}

func (inst *instance) watchProcess() {
	inst.mu.Lock()
	cmd := inst.cmd
	procDone := inst.procDone
	inst.mu.Unlock()

	if cmd == nil {
		return
	}
	cmd.Wait()

	inst.mu.Lock()
	close(procDone)

	if inst.state == stateFailed || inst.state == stateShutdown {
		inst.mu.Unlock()
		return
	}

	rpc := inst.rpc
	inst.mu.Unlock()

	if rpc != nil && !rpc.IsClosed() {
		rpc.Close()
	}

	inst.mu.Lock()
	if inst.restarts >= maxRestarts {
		inst.state = stateFailed
		inst.mu.Unlock()
		if inst.onCrash != nil {
			inst.onCrash(inst.def.Name)
		}
		return
	}
	inst.restarts++
	inst.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := inst.start(ctx); err != nil {
		inst.mu.Lock()
		inst.state = stateFailed
		inst.mu.Unlock()
		if inst.onCrash != nil {
			inst.onCrash(inst.def.Name)
		}
	}
}

func (inst *instance) waitReady(ctx context.Context) error {
	inst.mu.Lock()
	state := inst.state
	ch := inst.readyCh
	inst.mu.Unlock()

	if state == stateReady {
		return nil
	}
	if state == stateFailed {
		return fmt.Errorf("%s is unavailable", inst.def.Name)
	}
	if state == stateShutdown {
		if err := inst.restart(ctx); err != nil {
			return fmt.Errorf("%s restart failed: %w", inst.def.Name, err)
		}
		inst.mu.Lock()
		ch = inst.readyCh
		inst.mu.Unlock()
	}

	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()

	select {
	case <-ch:
		return nil
	case <-timer.C:
		inst.markReady()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (inst *instance) restart(ctx context.Context) error {
	inst.mu.Lock()
	inst.state = stateStarting
	inst.restarts = 0
	inst.mu.Unlock()
	return inst.start(ctx)
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
	inst.state = stateShutdown
	inst.mu.Unlock()

	if rpc == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	rpc.Call(ctx, "shutdown", nil)
	rpc.Notify("exit", nil)
	rpc.Close()

	if cmd != nil && cmd.Process != nil {
		select {
		case <-procDone:
		case <-time.After(shutdownWait):
			cmd.Process.Kill()
			<-procDone
		}
	}

	inst.mu.Lock()
	if inst.rpc == rpc {
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
