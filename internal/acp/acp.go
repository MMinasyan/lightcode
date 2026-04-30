package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/MMinasyan/lightcode/internal/agent"
)

// Request is an incoming JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an outgoing JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      any        `json:"id"`
	Result  any        `json:"result,omitempty"`
	Error   *RPCError  `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Notification is an outgoing JSON-RPC 2.0 notification (no id).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Runner drives the ACP stdio protocol.
type Runner struct {
	agent *agent.Agent
	mu    sync.Mutex
	out   io.Writer
}

// New creates an ACP Runner.
func New(a *agent.Agent) *Runner {
	return &Runner{agent: a, out: os.Stdout}
}

// Run reads JSON-RPC requests from stdin and dispatches them. It
// blocks until stdin is closed or ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r.agent.SetEventHandler(r.handleEvent)
	r.agent.Init(ctx)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			r.sendResponse(Response{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &RPCError{Code: -32700, Message: "parse error"},
			})
			continue
		}

		r.dispatch(ctx, req)
	}

	return scanner.Err()
}

func (r *Runner) dispatch(ctx context.Context, req Request) {
	switch req.Method {
	case "initialize":
		r.handleInitialize(req)
	case "session/new":
		r.handleSessionNew(req)
	case "session/prompt":
		r.handleSessionPrompt(ctx, req)
	case "session/cancel":
		r.agent.Cancel()
	case "session/current":
		r.respond(req.ID, r.agent.SessionCurrent())
	case "session/list":
		r.handleSessionList(req)
	case "session/switch":
		r.handleSessionSwitch(req)
	case "session/messages":
		r.respond(req.ID, r.agent.SessionMessages())
	case "session/archive":
		r.handleSessionArchive(req)
	case "session/delete":
		r.handleSessionDelete(req)
	case "session/fork":
		r.handleSessionFork(req)
	case "session/revert_code":
		r.handleRevertCode(req)
	case "session/revert_history":
		r.handleRevertHistory(req)
	case "model/current":
		r.respond(req.ID, r.agent.CurrentModel())
	case "model/list":
		r.respond(req.ID, r.agent.ModelList())
	case "model/switch":
		r.handleModelSwitch(req)
	case "snapshot/list":
		list, err := r.agent.SnapshotList()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
		} else {
			r.respond(req.ID, list)
		}
	case "tokens/usage":
		r.respond(req.ID, r.agent.TokenUsage())
	case "project/current":
		r.respond(req.ID, r.agent.ProjectCurrent())
	case "project/list":
		list, err := r.agent.ProjectList()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
		} else {
			r.respond(req.ID, list)
		}
	case "file/read":
		r.handleFileRead(req)
	case "permission/respond":
		r.handlePermissionRespond(req)
	case "permission/suggest":
		r.handlePermissionSuggest(req)
	case "permission/save":
		r.handlePermissionSave(req)
	case "compact":
		r.handleCompact(ctx, req)
	default:
		r.respondError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// --- Event handler ---

func (r *Runner) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		return
	}

	var method string
	var params any

	switch ev.Kind {
	case agent.EventTextDelta:
		method = "agent/message_chunk"
		params = map[string]any{"content": ev.Result}
	case agent.EventToolCallStart:
		method = "agent/tool_start"
		params = map[string]any{"id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args}
	case agent.EventToolCallEnd:
		method = "agent/tool_result"
		params = map[string]any{"id": ev.ToolCallID, "success": !ev.IsError, "output": ev.Result}
	case agent.EventUsage:
		method = "agent/usage"
		params = r.agent.TokenUsage()
	case agent.EventTurnStart:
		method = "agent/turn_start"
		params = map[string]any{"turn": ev.Turn}
	case agent.EventTurnEnd:
		method = "agent/turn_end"
		params = map[string]any{"turn": ev.Turn}
	case agent.EventError:
		method = "agent/error"
		params = map[string]any{"message": ev.Error, "turn": ev.Turn}
	case agent.EventPermissionRequest:
		method = "agent/permission_request"
		params = map[string]any{"id": ev.PermReq.ID, "tool": ev.PermReq.ToolName, "arg": ev.PermReq.Arg}
	case agent.EventCompactionStart:
		method = "agent/compaction_start"
	case agent.EventCompactionEnd:
		method = "agent/compaction_end"
	default:
		return
	}

	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

// --- Method handlers ---

func (r *Runner) handleInitialize(req Request) {
	r.respond(req.ID, map[string]any{
		"protocolVersion": 1,
		"serverInfo": map[string]any{
			"name":    "lightcode",
			"version": "0.3.0",
		},
		"capabilities": map[string]any{
			"sessions":    map[string]any{"list": true, "fork": true},
			"permissions": true,
			"models":      true,
		},
	})
}

func (r *Runner) handleSessionNew(req Request) {
	if err := r.agent.SessionNew(); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.pushSessionChanged()
	r.respond(req.ID, r.agent.SessionCurrent())
}

func (r *Runner) handleSessionPrompt(ctx context.Context, req Request) {
	var params struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	turn, err := r.agent.SendPrompt(ctx, params.Content)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"turn": turn})
}


func (r *Runner) handleSessionList(req Request) {
	var params struct {
		State string `json:"state"`
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}
	if params.State == "" {
		params.State = "active"
	}
	list, err := r.agent.SessionList(params.State)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, list)
}

func (r *Runner) handleSessionSwitch(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SessionSwitch(params.ID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.pushSessionChanged()
	r.respond(req.ID, r.agent.SessionCurrent())
}

func (r *Runner) handleSessionArchive(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	closedCurrent, err := r.agent.SessionArchive(params.ID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if closedCurrent {
		r.pushSessionChanged()
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleSessionDelete(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	closedCurrent, err := r.agent.SessionDelete(params.ID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if closedCurrent {
		r.pushSessionChanged()
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleSessionFork(req Request) {
	var params struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.ForkSession(params.Turn); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.pushSessionChanged()
	r.respond(req.ID, r.agent.SessionCurrent())
}

func (r *Runner) handleRevertCode(req Request) {
	var params struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.RevertCode(params.Turn); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleRevertHistory(req Request) {
	var params struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.RevertHistory(params.Turn); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.pushSessionChanged()
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleModelSwitch(req Request) {
	var params struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SwitchModel(params.Provider, params.Model); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, r.agent.CurrentModel())
}

func (r *Runner) handleFileRead(req Request) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	content, err := r.agent.ReadFileContent(params.Path)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"content": content})
}

func (r *Runner) handlePermissionSuggest(req Request) {
	var params struct {
		Tool string `json:"tool"`
		Arg  string `json:"arg"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	r.respond(req.ID, r.agent.PermissionSuggest(params.Tool, params.Arg))
}

func (r *Runner) handlePermissionSave(req Request) {
	var params struct {
		ID       string   `json:"id"`
		Patterns []string `json:"patterns"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SaveProjectPermission(params.ID, params.Patterns); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handlePermissionRespond(req Request) {
	var params struct {
		ID    string `json:"id"`
		Allow bool   `json:"allow"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.RespondPermission(params.ID, params.Allow); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleCompact(ctx context.Context, req Request) {
	if err := r.agent.CompactNow(ctx); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.pushSessionChanged()
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) pushSessionChanged() {
	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  "agent/session_changed",
		Params: map[string]any{
			"session":  r.agent.SessionCurrent(),
			"messages": r.agent.SessionMessages(),
			"tokens":   r.agent.TokenUsage(),
		},
	})
}

// --- Wire helpers ---

func (r *Runner) respond(id any, result any) {
	r.sendResponse(Response{JSONRPC: "2.0", ID: id, Result: result})
}

func (r *Runner) respondError(id any, code int, msg string) {
	r.sendResponse(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func (r *Runner) sendResponse(resp Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	r.out.Write(data)
}

func (r *Runner) sendNotification(n Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	data = append(data, '\n')
	r.out.Write(data)
}
