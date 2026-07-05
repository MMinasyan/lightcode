package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
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

type turnActionParams struct {
	SessionID      string `json:"session_id"`
	Turn           int    `json:"turn"`
	AlsoRevertCode bool   `json:"alsoRevertCode"`
}

// Runner drives the ACP stdio protocol.
type Runner struct {
	agent agent.AdapterService
	mu    sync.Mutex
	out   io.Writer

	viewOnce sync.Once
	view     *agent.SessionView
}

// New creates an ACP Runner.
func New(a agent.AdapterService) *Runner {
	return &Runner{agent: a, out: os.Stdout}
}

// Run reads JSON-RPC requests from stdin and dispatches them. It
// blocks until stdin is closed or ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r.agent.SetEventHandler(r.handleEvent)
	r.agent.Init(ctx)
	if lifecycle, ok := r.agent.(interface {
		AttachAdapter(context.Context) error
		DetachAdapter(context.Context) error
	}); ok {
		if err := lifecycle.AttachAdapter(ctx); err == nil {
			defer lifecycle.DetachAdapter(context.Background())
		}
	}
	sessionID := ""
	if sessions, err := r.agent.SessionList("active"); err == nil && len(sessions) > 0 {
		if summary, err := r.agent.OpenSession(sessions[0].ID); err == nil {
			sessionID = summary.ID
		}
	}
	if sessionID == "" {
		if id, err := r.agent.NewSession("", "primary"); err == nil {
			sessionID = id
		}
	}
	r.setCurrentSessionID(sessionID)

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
		r.handleSessionCancel(req)
	case "session/current":
		r.respond(req.ID, r.currentSessionSummary())
	case "session/list":
		r.handleSessionList(req)
	case "session/switch":
		r.handleSessionSwitch(req)
	case "session/messages":
		r.handleSessionMessages(req)
	case "queue/list":
		r.handleQueueList(req)
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
		r.handleModelCurrent(req)
	case "model/list":
		r.respond(req.ID, r.agent.ModelList())
	case "model/switch":
		r.handleModelSwitch(req)
	case "snapshot/list":
		sessionID := r.liveCurrentSessionID()
		if sessionID == "" {
			r.respondError(req.ID, -32000, "no current session")
			return
		}
		list, err := r.agent.SnapshotListForSession(sessionID)
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
		} else {
			r.respond(req.ID, list)
		}
	case "tokens/usage":
		r.handleTokenUsage(req)
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
	case "warnings/current":
		r.respond(req.ID, warningSnapshot(r.agent.CurrentWarnings()))
	default:
		r.respondError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// --- Event handler ---

func (r *Runner) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		return
	}
	if !r.acceptsSessionEvent(ev.SessionID) {
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
		params = map[string]any{"id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args, "success": !ev.IsError, "output": ev.Result, "metadata": ev.Metadata}
	case agent.EventBackgroundProcessComplete:
		method = "agent/background_process_complete"
		if ev.BackgroundProcess != nil {
			params = map[string]any{
				"id":       ev.BackgroundProcess.ID,
				"command":  ev.BackgroundProcess.Command,
				"reason":   ev.BackgroundProcess.Reason,
				"exitCode": ev.BackgroundProcess.ExitCode,
				"success":  !ev.IsError,
				"output":   ev.Result,
			}
		}
	case agent.EventUserMessageDisplay:
		method = "agent/user_message"
		params = map[string]any{"turn": ev.Turn, "content": ev.Result}
	case agent.EventGenericSystemSignal:
		method = "agent/system_signal"
		params = map[string]any{"content": "System: " + ev.Result}
	case agent.EventQueueChanged:
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		method = "agent/queue_changed"
		params = map[string]any{"items": queue, "version": ev.QueueVersion}
	case agent.EventUsage:
		method = "agent/usage"
		params = r.tokenUsageForEvent(ev)
	case agent.EventTurnStart:
		method = "agent/turn_start"
		params = map[string]any{"turn": ev.Turn}
	case agent.EventTurnEnd:
		r.sendNotification(Notification{
			JSONRPC: "2.0",
			Method:  "agent/turn_end",
			Params:  map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled},
		})
		if ev.RefreshSession {
			r.pushSessionChangedForEvent(ev)
		}
		return
	case agent.EventError:
		method = "agent/error"
		params = map[string]any{"message": ev.Error, "turn": ev.Turn}
	case agent.EventPermissionRequest:
		method = "agent/permission_request"
		params = map[string]any{
			"id":                 ev.PermReq.ID,
			"sessionId":          ev.PermReq.SessionID,
			"projectId":          ev.PermReq.ProjectID,
			"tool":               ev.PermReq.ToolName,
			"arg":                ev.PermReq.Arg,
			"resolvedArg":        ev.PermReq.ResolvedArg,
			"canAllowAll":        ev.PermReq.CanAllowAll,
			"canSaveProject":     !ev.PermReq.DisableProjectSave,
			"batchIndex":         ev.PermReq.BatchIndex,
			"batchTotal":         ev.PermReq.BatchTotal,
			"batchFiles":         ev.PermReq.BatchFiles,
			"batchResolvedFiles": ev.PermReq.BatchResolvedFiles,
		}
	case agent.EventCompactionStart:
		method = "agent/compaction_start"
	case agent.EventCompactionEnd:
		r.sendNotification(Notification{
			JSONRPC: "2.0",
			Method:  "agent/compaction_end",
		})
		if ev.RefreshSession {
			r.pushSessionChangedForEvent(ev)
		}
		return
	case agent.EventWarning:
		method = "agent/warnings"
		params = warningSnapshot(ev.Warnings)
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

func (r *Runner) sv() *agent.SessionView {
	r.viewOnce.Do(func() { r.view = agent.NewSessionView(r.agent) })
	return r.view
}

func (r *Runner) setCurrentSessionID(id string) {
	r.sv().SetCurrent(id)
}

func (r *Runner) currentSession() (string, error) {
	return r.sv().CurrentOrErr()
}

func (r *Runner) paramsSessionID(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit, nil
	}
	return r.currentSession()
}

func (r *Runner) acceptsSessionEvent(sessionID string) bool {
	return r.sv().AcceptsSessionEvent(sessionID)
}

func (r *Runner) liveCurrentSessionID() string {
	return r.sv().LiveCurrent()
}

func (r *Runner) currentSessionSummary() agent.SessionSummary {
	return r.sv().CurrentSummary()
}

func (r *Runner) tokenUsageForEvent(ev agent.Event) agent.TokenReport {
	if ev.SessionID != "" {
		if report, err := r.agent.TokenUsageForSession(ev.SessionID); err == nil {
			return report
		}
	}
	if current, err := r.currentSession(); err == nil {
		if report, err := r.agent.TokenUsageForSession(current); err == nil {
			return report
		}
	}
	return agent.TokenReport{}
}

func (r *Runner) handleSessionNew(req Request) {
	id, err := r.agent.NewSession("", "primary")
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.setCurrentSessionID(id)
	r.pushSessionChangedForSession(id)
	r.respond(req.ID, r.currentSessionSummary())
}

func (r *Runner) handleSessionPrompt(ctx context.Context, req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if strings.TrimSpace(params.SessionID) != "" {
		if _, err := r.agent.SessionSummaryForSession(sessionID); err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		r.setCurrentSessionID(sessionID)
	}
	res, err := r.agent.SubmitToSession(ctx, sessionID, params.Content)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{
		"started": res.Started,
		"turn":    res.Turn,
		"queue":   res.Queue,
		"version": res.Version,
	})
}

func (r *Runner) handleSessionCancel(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.CancelSession(sessionID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleQueueList(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	q, err := r.agent.QueueSnapshotForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, q)
}

func (r *Runner) handleSessionList(req Request) {
	var params struct {
		State string `json:"state"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
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

func (r *Runner) handleSessionMessages(req Request) {
	var params struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			r.respondError(req.ID, -32602, "invalid params")
			return
		}
	}
	id := strings.TrimSpace(params.SessionID)
	if id == "" {
		id = strings.TrimSpace(params.ID)
	}
	if id == "" {
		var err error
		id, err = r.currentSession()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		if _, err := r.agent.SessionSummaryForSession(id); err != nil {
			r.setCurrentSessionID("")
			r.respondError(req.ID, -32000, err.Error())
			return
		}
	}
	msgs, err := r.agent.SessionMessagesFor(id)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, msgs)
}

func (r *Runner) handleSessionSwitch(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	summary, err := r.agent.OpenSession(params.ID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.setCurrentSessionID(summary.ID)
	r.pushSessionChangedForSession(summary.ID)
	r.respond(req.ID, summary)
}

func (r *Runner) handleSessionArchive(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SessionArchive(params.ID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	wasCurrent := r.sv().RemovedCurrent(params.ID)
	if wasCurrent {
		r.pushSessionChangedForSession(strings.TrimSpace(params.ID))
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
	if err := r.agent.SessionDelete(params.ID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	wasCurrent := r.sv().RemovedCurrent(params.ID)
	if wasCurrent {
		r.pushSessionChangedForSession(strings.TrimSpace(params.ID))
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleSessionFork(req Request) {
	r.handleTurnAction(req, agent.TurnActionFork)
}

func (r *Runner) handleRevertCode(req Request) {
	r.handleTurnAction(req, agent.TurnActionRevertCode)
}

func (r *Runner) handleRevertHistory(req Request) {
	r.handleTurnAction(req, agent.TurnActionRevertHistory)
}

func (r *Runner) handleTurnAction(req Request, action string) {
	var params turnActionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if action == agent.TurnActionRevertCode {
		params.AlsoRevertCode = false
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	result, err := r.agent.ApplyTurnActionForSession(sessionID, params.Turn, action, params.AlsoRevertCode)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if result.SessionChanged {
		if result.Session.ID != "" {
			r.setCurrentSessionID(result.Session.ID)
			r.pushSessionChangedForSession(result.Session.ID)
		} else {
			r.pushSessionChangedForSession(sessionID)
		}
	}
	r.respond(req.ID, result)
}

func (r *Runner) handleModelCurrent(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	model, err := r.agent.CurrentModelForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, model)
}

func (r *Runner) handleModelSwitch(req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		Ref       string `json:"ref"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.SwitchModelForSession(sessionID, params.Ref); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	model, err := r.agent.CurrentModelForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, model)
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
		SessionID string `json:"session_id"`
		ProjectID string `json:"project_id"`
		Tool      string `json:"tool"`
		Arg       string `json:"arg"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	var (
		suggestions []agent.PermissionSuggestion
		err         error
	)
	if strings.TrimSpace(params.ProjectID) != "" {
		suggestions, err = r.agent.PermissionSuggestForProject(params.ProjectID, params.Tool, params.Arg)
	} else {
		sessionID, sessionErr := r.paramsSessionID(params.SessionID)
		if sessionErr != nil {
			r.respondError(req.ID, -32000, sessionErr.Error())
			return
		}
		suggestions, err = r.agent.PermissionSuggestForSession(sessionID, params.Tool, params.Arg)
	}
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, suggestions)
}

func (r *Runner) handlePermissionSave(req Request) {
	var params struct {
		SessionID string   `json:"session_id"`
		ID        string   `json:"id"`
		Patterns  []string `json:"patterns"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.SaveProjectPermissionForSession(sessionID, params.ID, params.Patterns); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handlePermissionRespond(req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
		Allow     *bool  `json:"allow"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if params.Action == "" {
		if params.Allow == nil {
			r.respondError(req.ID, -32602, "missing permission action")
			return
		}
		if *params.Allow {
			params.Action = "allow"
		} else {
			params.Action = "deny"
		}
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.RespondPermissionActionForSession(sessionID, params.ID, params.Action); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleCompact(ctx context.Context, req Request) {
	var params struct {
		SessionID string `json:"session_id"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			r.respondError(req.ID, -32602, "invalid params")
			return
		}
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		sessionID = r.liveCurrentSessionID()
		if sessionID == "" {
			r.respondError(req.ID, -32000, "no current session")
			return
		}
	}
	if err := r.agent.CompactNowForSession(ctx, sessionID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleTokenUsage(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	report, err := r.agent.TokenUsageForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, report)
}

func warningSnapshot(warnings []agent.PromptWarning) []agent.PromptWarning {
	if warnings == nil {
		return []agent.PromptWarning{}
	}
	return warnings
}

func (r *Runner) pushSessionChanged() {
	current, err := r.currentSession()
	if err != nil {
		r.pushSessionChangedForSession("")
		return
	}
	r.pushSessionChangedForSession(current)
}

func (r *Runner) pushSessionChangedForEvent(ev agent.Event) {
	if strings.TrimSpace(ev.SessionID) != "" {
		r.pushSessionChangedForSession(ev.SessionID)
		return
	}
	r.pushSessionChanged()
}

func (r *Runner) pushSessionChangedForSession(sessionID string) {
	payload := r.sv().SessionChangedPayload(sessionID)
	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  "agent/session_changed",
		Params:  payload,
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
	_, _ = r.out.Write(data)
}

func (r *Runner) sendNotification(n Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = r.out.Write(data)
}
