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
	token   string
	httpSrv *http.Server
	srvCtx  context.Context

	permMu     sync.Mutex
	permTimers map[string]*time.Timer
}

// New constructs a Server.
func New(a *agent.Agent, cfg Config) *Server {
	if cfg.PermissionTimeout == 0 {
		cfg.PermissionTimeout = 60 * time.Second
	}
	return &Server{
		agent:      a,
		cfg:        cfg,
		hub:        newSSEHub(),
		permTimers: make(map[string]*time.Timer),
	}
}

// Serve starts the HTTP server and blocks until ctx is cancelled or a
// signal is received. It writes a lockfile on startup and removes it
// on shutdown.
func (s *Server) Serve(ctx context.Context, home, projectID string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Check for existing daemon.
	if lf, err := Read(home, projectID); err == nil {
		if !IsStale(lf) {
			return fmt.Errorf("daemon already running for this project (pid %d, port %d)", lf.PID, lf.Port)
		}
		_ = Remove(home, projectID)
	}

	// Generate auth token.
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	s.token = hex.EncodeToString(tokenBytes[:])

	// Bind listener.
	addr := fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Write lockfile.
	lf := LockFile{Port: port, Token: s.token, PID: os.Getpid()}
	if err := Write(home, projectID, lf); err != nil {
		_ = ln.Close()
		return fmt.Errorf("write lockfile: %w", err)
	}
	defer Remove(home, projectID)

	s.srvCtx = ctx

	// Wire event handler.
	s.agent.SetEventHandler(s.handleEvent)
	s.agent.Init(ctx)

	// Build routes.
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Shutdown goroutine. Uses context.Background for the Shutdown timeout
	// because the parent ctx has already been cancelled by the time we get
	// here; deriving from it would give Shutdown zero time to drain.
	// #nosec G118 -- parent ctx is already cancelled; Shutdown needs its own 5s budget.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
	}()

	slog.Info("lightcode serve", "port", port, "pid", os.Getpid())
	fmt.Fprintf(os.Stderr, "lightcode: serving on 127.0.0.1:%d (token in %s)\n", port, Path(home, projectID))

	if err := s.httpSrv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// SSE endpoint (no auth — token is in the query string for EventSource compatibility).
	mux.HandleFunc("GET /v1/events", s.handleSSE)

	// All other routes require Bearer auth.
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
		name = "queue_changed"
		data = map[string]any{"items": ev.Queue, "version": ev.QueueVersion}
	case agent.EventUsage:
		name = "usage"
		data = s.agent.TokenUsage()
	case agent.EventTurnStart:
		name = "turn_start"
		data = map[string]any{"turn": ev.Turn}
	case agent.EventTurnEnd:
		name = "turn_end"
		data = map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled}
	case agent.EventError:
		name = "error"
		data = map[string]any{"message": ev.Error, "turn": ev.Turn}
	case agent.EventPermissionRequest:
		name = "permission_request"
		data = map[string]any{
			"id":          ev.PermReq.ID,
			"tool":        ev.PermReq.ToolName,
			"arg":         ev.PermReq.Arg,
			"resolvedArg": ev.PermReq.ResolvedArg,
			"canAllowAll": ev.PermReq.CanAllowAll,
			"batchIndex":  ev.PermReq.BatchIndex,
			"batchTotal":  ev.PermReq.BatchTotal,
			"batchFiles":  ev.PermReq.BatchFiles,
		}
		s.startPermissionTimer(ev.PermReq.ID)
	case agent.EventCompactionStart:
		name = "compaction_start"
	case agent.EventCompactionEnd:
		s.hub.broadcast("compaction_end", nil)
		s.broadcastSessionChanged()
		return
	case agent.EventWarning:
		name = "warnings"
		data = warningSnapshot(ev.Warnings)
	default:
		return
	}

	s.hub.broadcast(name, data)
}

func (s *Server) startPermissionTimer(id string) {
	s.permMu.Lock()
	defer s.permMu.Unlock()
	timer := time.AfterFunc(s.cfg.PermissionTimeout, func() {
		slog.Warn("permission timeout, auto-denying", "id", id)
		_ = s.agent.RespondPermission(id, false)
		s.permMu.Lock()
		delete(s.permTimers, id)
		s.permMu.Unlock()
	})
	s.permTimers[id] = timer
}

func (s *Server) cancelPermissionTimer(id string) {
	s.permMu.Lock()
	defer s.permMu.Unlock()
	if timer, ok := s.permTimers[id]; ok {
		timer.Stop()
		delete(s.permTimers, id)
	}
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

	ch, unsub := s.hub.subscribe()
	defer unsub()

	// On disconnect, auto-deny any pending permissions.
	defer func() {
		s.permMu.Lock()
		ids := make([]string, 0, len(s.permTimers))
		for id := range s.permTimers {
			ids = append(ids, id)
		}
		s.permMu.Unlock()
		for _, id := range ids {
			s.cancelPermissionTimer(id)
			_ = s.agent.RespondPermission(id, false)
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

// broadcastSessionChanged pushes a session_changed event to all SSE clients.
func (s *Server) broadcastSessionChanged() {
	s.hub.broadcast("session_changed", map[string]any{
		"session":  s.agent.SessionCurrent(),
		"messages": s.agent.SessionMessages(),
		"tokens":   s.agent.TokenUsage(),
	})
}

// --- Route handlers ---

type turnActionRequest struct {
	Turn           int  `json:"turn"`
	AlsoRevertCode bool `json:"alsoRevertCode"`
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	// srvCtx (not the request ctx) so the started/queued turn outlives the HTTP
	// response.
	res, err := s.agent.Submit(s.srvCtx, body.Content)
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
	jsonResp(w, http.StatusOK, s.agent.QueueSnapshot())
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	_ = s.agent.Cancel()
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/permission/")
	if id == "" {
		jsonError(w, "missing permission id", http.StatusBadRequest)
		return
	}
	var body struct {
		Allow  *bool  `json:"allow"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	s.cancelPermissionTimer(id)
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
	if err := s.agent.RespondPermissionAction(id, body.Action); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePermissionSuggest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tool string `json:"tool"`
		Arg  string `json:"arg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	jsonResp(w, http.StatusOK, s.agent.PermissionSuggest(body.Tool, body.Arg))
}

func (s *Server) handlePermissionSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string   `json:"id"`
		Patterns []string `json:"patterns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	s.cancelPermissionTimer(body.ID)
	if err := s.agent.SaveProjectPermission(body.ID, body.Patterns); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionCurrent(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.SessionCurrent())
}

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.SessionNew(); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	s.broadcastSessionChanged()
	jsonResp(w, http.StatusOK, s.agent.SessionCurrent())
}

func (s *Server) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := s.agent.SessionSwitch(body.ID); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.broadcastSessionChanged()
	jsonResp(w, http.StatusOK, s.agent.SessionCurrent())
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
	closedCurrent, err := s.agent.SessionArchive(body.ID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if closedCurrent {
		s.broadcastSessionChanged()
	}
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
	closedCurrent, err := s.agent.SessionDelete(body.ID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if closedCurrent {
		s.broadcastSessionChanged()
	}
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.SessionMessages())
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
	result, err := s.agent.ApplyTurnAction(body.Turn, action, body.AlsoRevertCode)
	if err != nil {
		jsonError(w, err.Error(), actionErrorStatus)
		return
	}
	if result.SessionChanged {
		s.broadcastSessionChanged()
	}
	jsonResp(w, http.StatusOK, result)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.SnapshotList()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, list)
}

func (s *Server) handleModelCurrent(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.CurrentModel())
}

func (s *Server) handleModelSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := s.agent.SwitchModel(body.Ref); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResp(w, http.StatusOK, s.agent.CurrentModel())
}

func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.ModelList())
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agent.TokenUsage())
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
	if err := s.agent.CompactNow(r.Context()); err != nil {
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

// --- SSE hub ---

type sseClient struct {
	mu     sync.Mutex
	ch     chan []byte
	closed bool
}

type sseHub struct {
	mu      sync.Mutex
	clients []*sseClient
}

func newSSEHub() *sseHub {
	return &sseHub{}
}

func (h *sseHub) subscribe() (<-chan []byte, func()) {
	client := &sseClient{ch: make(chan []byte, 64)}
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
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
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
