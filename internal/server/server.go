package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
)

// Config controls daemon behavior.
type Config struct {
	Port              int
	PermissionTimeout time.Duration
}

// Server is the HTTP+SSE daemon.
type Server struct {
	agent   *agent.Agent
	cfg     Config
	hub     *sseHub
	adapter *sseHub
	token   string
	httpSrv *http.Server
	srvCtx  context.Context

	permMu            sync.Mutex
	permTimers        map[string]*time.Timer
	permTimerSessions map[string]string
}

// New constructs a Server.
func New(a *agent.Agent, cfg Config) *Server {
	if cfg.PermissionTimeout == 0 {
		cfg.PermissionTimeout = 60 * time.Second
	}
	return &Server{
		agent:             a,
		cfg:               cfg,
		hub:               newSSEHub(),
		adapter:           newSSEHub(),
		permTimers:        make(map[string]*time.Timer),
		permTimerSessions: make(map[string]string),
	}
}

// Start starts the HTTP server in the background. It writes the owner
// lockfile on startup and removes it on shutdown.
func (s *Server) Start(ctx context.Context, home string) (LockFile, <-chan error, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)

	if lf, err := Read(home); err == nil {
		if !IsStale(lf) {
			stop()
			return LockFile{}, nil, fmt.Errorf("owner already running (pid %d, port %d)", lf.PID, lf.Port)
		}
		_ = Remove(home)
	} else if !os.IsNotExist(err) {
		if removeErr := Remove(home); removeErr != nil {
			stop()
			return LockFile{}, nil, fmt.Errorf("remove unreadable owner lock: read %v: remove %w", err, removeErr)
		}
	}

	// Generate auth token.
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		stop()
		return LockFile{}, nil, fmt.Errorf("generate token: %w", err)
	}
	s.token = hex.EncodeToString(tokenBytes[:])

	// Bind listener.
	addr := fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		stop()
		return LockFile{}, nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Write lockfile.
	lf := LockFile{Port: port, Token: s.token, PID: os.Getpid()}
	if err := Write(home, lf); err != nil {
		_ = ln.Close()
		stop()
		return LockFile{}, nil, fmt.Errorf("write lockfile: %w", err)
	}

	s.srvCtx = ctx

	// Wire event handler.
	s.agent.SubscribeEvents(s.handleEvent)
	s.agent.Init(ctx)

	// Build routes.
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Shutdown goroutine. Uses context.Background for the Shutdown timeout
	// because the parent ctx has already been cancelled by the time we get
	// here; deriving from it would give Shutdown zero time to drain.
	// #nosec G118 -- parent ctx is already cancelled; Shutdown needs its own 5s budget.
	done := make(chan error, 1)
	go func() {
		defer stop()
		defer Remove(home)
		err := s.httpSrv.Serve(ln)
		if err == http.ErrServerClosed {
			err = nil
		}
		done <- err
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
	}()

	slog.Info("lightcode serve", "port", port, "pid", os.Getpid())
	fmt.Fprintf(os.Stderr, "lightcode: serving on 127.0.0.1:%d (token in %s)\n", port, Path(home))

	return lf, done, nil
}

// Serve starts the HTTP server and blocks until ctx is cancelled or a
// signal is received.
func (s *Server) Serve(ctx context.Context, home string) error {
	_, done, err := s.Start(ctx, home)
	if err != nil {
		return err
	}
	return <-done
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// SSE endpoint (no auth — token is in the query string for EventSource compatibility).
	mux.HandleFunc("GET /v1/events", s.handleSSE)
	mux.HandleFunc("GET /v1/adapter/events", s.handleAdapterSSE)

	// All other routes require Bearer auth.
	mux.HandleFunc("POST /v1/adapter/rpc", s.auth(s.handleAdapterRPC))
	mux.HandleFunc("POST /v1/prompt", s.auth(s.handlePrompt))

	mux.HandleFunc("POST /v1/cancel", s.auth(s.handleCancel))
	mux.HandleFunc("POST /v1/permission/suggest", s.auth(s.handlePermissionSuggest))
	mux.HandleFunc("POST /v1/permission/save", s.auth(s.handlePermissionSave))
	mux.HandleFunc("POST /v1/permission/", s.auth(s.handlePermission))
	mux.HandleFunc("GET /v1/session", s.auth(s.handleSessionCurrent))
	mux.HandleFunc("POST /v1/session/new", s.auth(s.handleSessionNew))
	mux.HandleFunc("POST /v1/session/switch", s.auth(s.handleSessionSwitch))
	mux.HandleFunc("GET /v1/session/list", s.auth(s.handleSessionList))
	mux.HandleFunc("POST /v1/session/archive", s.auth(s.handleSessionArchive))
	mux.HandleFunc("POST /v1/session/delete", s.auth(s.handleSessionDelete))
	mux.HandleFunc("GET /v1/session/messages", s.auth(s.handleSessionMessages))
	mux.HandleFunc("GET /v1/queue", s.auth(s.handleQueue))
	mux.HandleFunc("POST /v1/session/fork", s.auth(s.handleSessionFork))
	mux.HandleFunc("POST /v1/revert/code", s.auth(s.handleRevertCode))
	mux.HandleFunc("POST /v1/revert/history", s.auth(s.handleRevertHistory))
	mux.HandleFunc("GET /v1/snapshots", s.auth(s.handleSnapshots))
	mux.HandleFunc("GET /v1/model", s.auth(s.handleModelCurrent))
	mux.HandleFunc("POST /v1/model/switch", s.auth(s.handleModelSwitch))
	mux.HandleFunc("GET /v1/model/list", s.auth(s.handleModelList))
	mux.HandleFunc("GET /v1/tokens", s.auth(s.handleTokens))
	mux.HandleFunc("GET /v1/project", s.auth(s.handleProjectCurrent))
	mux.HandleFunc("GET /v1/project/list", s.auth(s.handleProjectList))
	mux.HandleFunc("GET /v1/file", s.auth(s.handleFile))
	mux.HandleFunc("POST /v1/compact", s.auth(s.handleCompact))
	mux.HandleFunc("GET /v1/warnings", s.auth(s.handleWarnings))
}

// --- Auth middleware ---

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || strings.TrimPrefix(h, "Bearer ") != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- Event handler ---

func (s *Server) handleEvent(ev agent.Event) {
	if s.adapter != nil {
		s.adapter.broadcast("agent_event", ev)
	}

	if ev.SubagentSessionID != "" {
		return
	}

	var name string
	var data any

	switch ev.Kind {
	case agent.EventTextDelta:
		name = "message_chunk"
		data = map[string]any{"content": ev.Result}
	case agent.EventToolCallStart:
		name = "tool_start"
		data = map[string]any{"id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args}
	case agent.EventToolCallEnd:
		name = "tool_result"
		data = map[string]any{"id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args, "success": !ev.IsError, "output": ev.Result, "metadata": ev.Metadata}
	case agent.EventBackgroundProcessComplete:
		name = "background_process_complete"
		if ev.BackgroundProcess != nil {
			data = map[string]any{
				"id":       ev.BackgroundProcess.ID,
				"command":  ev.BackgroundProcess.Command,
				"reason":   ev.BackgroundProcess.Reason,
				"exitCode": ev.BackgroundProcess.ExitCode,
				"success":  !ev.IsError,
				"output":   ev.Result,
			}
		}
	case agent.EventUserMessageDisplay:
		name = "user_message"
		data = map[string]any{"turn": ev.Turn, "content": ev.Result}
	case agent.EventGenericSystemSignal:
		name = "system_signal"
		data = map[string]any{"content": "System: " + ev.Result}
	case agent.EventQueueChanged:
		// Best-effort broadcast (like all hub events); clients reconcile the
		// queue via the versioned GET /v1/queue on connect/reconnect.
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		name = "queue_changed"
		data = map[string]any{"items": queue, "version": ev.QueueVersion}
	case agent.EventUsage:
		name = "usage"
		data = s.tokenUsageForEvent(ev)
	case agent.EventTurnStart:
		name = "turn_start"
		data = map[string]any{"turn": ev.Turn}
	case agent.EventTurnEnd:
		s.hub.broadcastForSession(ev.SessionID, "turn_end", map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled})
		if ev.RefreshSession {
			s.broadcastSessionChangedForSession(ev.SessionID)
		}
		return
	case agent.EventError:
		name = "error"
		data = map[string]any{"message": ev.Error, "turn": ev.Turn}
	case agent.EventPermissionRequest:
		name = "permission_request"
		data = map[string]any{
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
		s.startPermissionTimer(ev.PermReq)
	case agent.EventCompactionStart:
		name = "compaction_start"
	case agent.EventCompactionEnd:
		s.hub.broadcastForSession(ev.SessionID, "compaction_end", nil)
		if ev.RefreshSession {
			s.broadcastSessionChangedForSession(ev.SessionID)
		}
		return
	case agent.EventWarning:
		name = "warnings"
		data = warningSnapshot(ev.Warnings)
	default:
		return
	}

	s.hub.broadcastForSession(ev.SessionID, name, data)
}

func (s *Server) tokenUsageForEvent(ev agent.Event) agent.TokenReport {
	if ev.SessionID != "" {
		if report, err := s.agent.TokenUsageForSession(ev.SessionID); err == nil {
			return report
		}
	}
	return s.agent.TokenUsage()
}

func (s *Server) startPermissionTimer(req *agent.PermissionRequest) {
	if req == nil {
		return
	}
	id := req.ID
	sessionID := req.SessionID
	s.permMu.Lock()
	defer s.permMu.Unlock()
	timer := time.AfterFunc(s.cfg.PermissionTimeout, func() {
		slog.Warn("permission timeout, auto-denying", "id", id)
		if sessionID != "" {
			_ = s.agent.RespondPermissionForSession(sessionID, id, false)
		} else {
			_ = s.agent.RespondPermission(id, false)
		}
		s.permMu.Lock()
		delete(s.permTimers, id)
		delete(s.permTimerSessions, id)
		s.permMu.Unlock()
	})
	if s.permTimerSessions == nil {
		s.permTimerSessions = make(map[string]string)
	}
	s.permTimers[id] = timer
	s.permTimerSessions[id] = sessionID
}

func (s *Server) cancelPermissionTimer(id string) {
	s.permMu.Lock()
	defer s.permMu.Unlock()
	if timer, ok := s.permTimers[id]; ok {
		timer.Stop()
		delete(s.permTimers, id)
	}
	delete(s.permTimerSessions, id)
}

// --- SSE handler ---

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	// Auth via query param for EventSource compatibility.
	if r.URL.Query().Get("token") != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	ch, unsub := s.hub.subscribe(sessionID)
	defer unsub()

	// On disconnect, auto-deny pending permissions only for the subscribed session.
	defer func() {
		if sessionID == "" {
			return
		}
		s.permMu.Lock()
		ids := make([]string, 0)
		for id := range s.permTimers {
			if s.permTimerSessions[id] == sessionID {
				ids = append(ids, id)
			}
		}
		s.permMu.Unlock()
		for _, id := range ids {
			s.cancelPermissionTimer(id)
			_ = s.agent.RespondPermissionForSession(sessionID, id, false)
		}
	}()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// broadcastSessionChangedForSession pushes a session_changed event to subscribers of one session.
func (s *Server) broadcastSessionChangedForSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.broadcastSessionChanged(sessionID, sessionID)
}

func (s *Server) broadcastSessionChanged(routeSessionID, payloadSessionID string) {
	routeSessionID = strings.TrimSpace(routeSessionID)
	if routeSessionID == "" {
		return
	}
	payloadSessionID = strings.TrimSpace(payloadSessionID)
	payload := agent.SessionPayload{}
	if payloadSessionID != "" {
		var err error
		payload, err = s.agent.SessionPayloadForSession(payloadSessionID)
		if err != nil {
			payload = agent.SessionPayload{}
		}
	}
	s.hub.broadcastForSession(routeSessionID, "session_changed", payload)
}

// --- Route handlers ---

type turnActionRequest struct {
	SessionID      string `json:"session_id"`
	Turn           int    `json:"turn"`
	AlsoRevertCode bool   `json:"alsoRevertCode"`
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	sessionID, ok := requireSessionID(w, body.SessionID)
	if !ok {
		return
	}
	// srvCtx (not the request ctx) so the started/queued turn outlives the HTTP
	// response.
	res, err := s.agent.SubmitToSession(s.srvCtx, sessionID, body.Content)
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResp(w, http.StatusAccepted, map[string]any{
		"started": res.Started,
		"turn":    res.Turn,
		"queue":   res.Queue,
		"version": res.Version,
	})
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	q, err := s.agent.QueueSnapshotForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, q)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	if err := s.agent.CancelSession(sessionID); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/permission/")
	if id == "" {
		jsonError(w, "missing permission id", http.StatusBadRequest)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Allow     *bool  `json:"allow"`
		Action    string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	sessionID, ok := requireSessionID(w, body.SessionID)
	if !ok {
		return
	}
	if body.Action == "" {
		if body.Allow == nil {
			jsonError(w, "missing permission action", http.StatusBadRequest)
			return
		}
		if *body.Allow {
			body.Action = "allow"
		} else {
			body.Action = "deny"
		}
	}
	if err := s.agent.RespondPermissionActionForSession(sessionID, id, body.Action); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.cancelPermissionTimer(id)
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePermissionSuggest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		ProjectID string `json:"project_id"`
		Tool      string `json:"tool"`
		Arg       string `json:"arg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	sessionID := strings.TrimSpace(body.SessionID)
	projectID := strings.TrimSpace(body.ProjectID)
	var (
		suggestions []agent.PermissionSuggestion
		err         error
	)
	switch {
	case sessionID != "":
		suggestions, err = s.agent.PermissionSuggestForSession(sessionID, body.Tool, body.Arg)
	case projectID != "":
		suggestions, err = s.agent.PermissionSuggestForProject(projectID, body.Tool, body.Arg)
	default:
		jsonError(w, "session_id or project_id is required", http.StatusBadRequest)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, suggestions)
}

func (s *Server) handlePermissionSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string   `json:"session_id"`
		ID        string   `json:"id"`
		Patterns  []string `json:"patterns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	sessionID, ok := requireSessionID(w, body.SessionID)
	if !ok {
		return
	}
	if err := s.agent.SaveProjectPermissionForSession(sessionID, body.ID, body.Patterns); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.cancelPermissionTimer(body.ID)
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionCurrent(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	summary, err := s.agent.SessionSummaryForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, summary)
}

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	id, err := s.agent.NewSession("", "primary")
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	s.broadcastSessionChangedForSession(id)
	summary, err := s.agent.SessionSummaryForSession(id)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, summary)
}

func (s *Server) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	summary, err := s.agent.OpenSession(body.ID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.broadcastSessionChangedForSession(summary.ID)
	jsonResp(w, http.StatusOK, summary)
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "active"
	}
	list, err := s.agent.SessionList(state)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResp(w, http.StatusOK, list)
}

func (s *Server) handleSessionArchive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	_, err := s.agent.SessionArchive(body.ID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.broadcastSessionChangedForSession(body.ID)
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	_, err := s.agent.SessionDelete(body.ID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.broadcastSessionChangedForSession(body.ID)
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if id == "" {
		id = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if id == "" {
		jsonError(w, "session_id is required", http.StatusBadRequest)
		return
	}
	msgs, err := s.agent.SessionMessagesFor(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, msgs)
}

func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	s.handleTurnAction(w, r, agent.TurnActionFork, http.StatusInternalServerError)
}

func (s *Server) handleRevertCode(w http.ResponseWriter, r *http.Request) {
	s.handleTurnAction(w, r, agent.TurnActionRevertCode, http.StatusConflict)
}

func (s *Server) handleRevertHistory(w http.ResponseWriter, r *http.Request) {
	s.handleTurnAction(w, r, agent.TurnActionRevertHistory, http.StatusConflict)
}

func (s *Server) handleTurnAction(w http.ResponseWriter, r *http.Request, action string, actionErrorStatus int) {
	var body turnActionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if action == agent.TurnActionRevertCode {
		body.AlsoRevertCode = false
	}
	sessionID, ok := requireSessionID(w, body.SessionID)
	if !ok {
		return
	}
	result, err := s.agent.ApplyTurnActionForSession(sessionID, body.Turn, action, body.AlsoRevertCode)
	if err != nil {
		jsonError(w, err.Error(), actionErrorStatus)
		return
	}
	if result.SessionChanged {
		if result.Session.ID != "" {
			s.broadcastSessionChanged(sessionID, result.Session.ID)
		} else {
			s.broadcastSessionChangedForSession(sessionID)
		}
	}
	jsonResp(w, http.StatusOK, result)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	list, err := s.agent.SnapshotListForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, list)
}

func (s *Server) handleModelCurrent(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	model, err := s.agent.CurrentModelForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, model)
}

func (s *Server) handleModelSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Ref       string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	sessionID, ok := requireSessionID(w, body.SessionID)
	if !ok {
		return
	}
	if err := s.agent.SwitchModelForSession(sessionID, body.Ref); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	model, err := s.agent.CurrentModelForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, model)
}

func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.ModelList())
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	report, err := s.agent.TokenUsageForSession(sessionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, report)
}

func (s *Server) handleProjectCurrent(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.ProjectCurrent())
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.ProjectList()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, list)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	content, err := s.agent.ReadFileContent(path)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"content": content})
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := requireQuerySessionID(w, r)
	if !ok {
		return
	}
	if err := s.agent.CompactNowForSession(r.Context(), sessionID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWarnings(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, warningSnapshot(s.agent.CurrentWarnings()))
}

func warningSnapshot(warnings []agent.PromptWarning) []agent.PromptWarning {
	if warnings == nil {
		return []agent.PromptWarning{}
	}
	return warnings
}

func requireQuerySessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requireSessionID(w, r.URL.Query().Get("session_id"))
}

func requireSessionID(w http.ResponseWriter, id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		jsonError(w, "session_id is required", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

// --- SSE hub ---

type sseClient struct {
	mu        sync.Mutex
	ch        chan []byte
	sessionID string
	closed    bool
}

type sseHub struct {
	mu      sync.Mutex
	clients []*sseClient
}

func newSSEHub() *sseHub {
	return &sseHub{}
}

func (h *sseHub) subscribe(sessionID string) (<-chan []byte, func()) {
	client := &sseClient{ch: make(chan []byte, 64), sessionID: strings.TrimSpace(sessionID)}
	h.mu.Lock()
	h.clients = append(h.clients, client)
	h.mu.Unlock()
	return client.ch, func() {
		h.mu.Lock()
		for i, c := range h.clients {
			if c == client {
				h.clients = append(h.clients[:i], h.clients[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		client.mu.Lock()
		if !client.closed {
			client.closed = true
			close(client.ch)
		}
		client.mu.Unlock()
	}
}

func (h *sseHub) broadcast(eventName string, data any) {
	h.broadcastForSession("", eventName, data)
}

func (h *sseHub) broadcastForSession(sessionID string, eventName string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	msg := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", eventName, payload)
	h.mu.Lock()
	clients := append([]*sseClient(nil), h.clients...)
	h.mu.Unlock()
	for _, client := range clients {
		client.mu.Lock()
		if client.closed {
			client.mu.Unlock()
			continue
		}
		if sessionID != "" && client.sessionID != sessionID {
			client.mu.Unlock()
			continue
		}
		select {
		case client.ch <- msg:
		default:
		}
		client.mu.Unlock()
	}
}

// --- JSON helpers ---

func jsonResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
