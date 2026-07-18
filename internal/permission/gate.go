package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrUnknownRequest = errors.New("no pending permission request")
var ErrSessionMismatch = errors.New("permission request belongs to another session")

// Request is the structured payload sent to the frontend when the gate
// needs to ask the user for permission.
type Request struct {
	ID                 string   `json:"id"`
	SessionID          string   `json:"session_id,omitempty"`
	ProjectID          string   `json:"project_id,omitempty"`
	ToolName           string   `json:"tool"`
	Arg                string   `json:"args"`
	ResolvedArg        string   `json:"resolved_arg,omitempty"`
	CanAllowAll        bool     `json:"can_allow_all,omitempty"`
	DisableProjectSave bool     `json:"disable_project_save,omitempty"`
	BatchIndex         int      `json:"batch_index,omitempty"`
	BatchTotal         int      `json:"batch_total,omitempty"`
	BatchFiles         []string `json:"batch_files,omitempty"`
	BatchResolvedFiles []string `json:"batch_resolved_files,omitempty"`
}

// ResponseAction is the user's answer to a permission prompt.
type ResponseAction string

const (
	ResponseAllow    ResponseAction = "allow"
	ResponseDeny     ResponseAction = "deny"
	ResponseAllowAll ResponseAction = "allow_all"
)

// Gate bridges synchronous permission checks to an async request/response
// round-trip through the Wails frontend.
type Gate struct {
	mu      sync.Mutex
	pending map[string]pendingRequest

	// OnRequest is called when a new permission request is registered.
	OnRequest func(ctx context.Context, req Request)
}

type pendingRequest struct {
	ch  chan ResponseAction
	req Request
}

// NewGate returns a Gate that calls onRequest for each new permission request.
func NewGate(onRequest func(ctx context.Context, req Request)) *Gate {
	return &Gate{
		pending:   make(map[string]pendingRequest),
		OnRequest: onRequest,
	}
}

// Ask registers a pending request and blocks until the user responds or
// ctx is cancelled. Returns true for allow, false for deny.
func (g *Gate) Ask(ctx context.Context, toolName, arg string) bool {
	action := g.AskRequest(ctx, Request{ToolName: toolName, Arg: arg})
	return action == ResponseAllow || action == ResponseAllowAll
}

// AskRequest registers a pending structured request and blocks until the
// user responds or ctx is cancelled.
func (g *Gate) AskRequest(ctx context.Context, req Request) ResponseAction {
	if ctx.Err() != nil {
		return ResponseDeny
	}

	id := newID()
	ch := make(chan ResponseAction, 1)
	req.ID = id

	g.mu.Lock()
	g.pending[id] = pendingRequest{ch: ch, req: req}
	g.mu.Unlock()

	if g.OnRequest != nil {
		g.OnRequest(ctx, req)
	}

	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		select {
		case result := <-ch:
			return result
		default:
		}
		g.mu.Lock()
		select {
		case result := <-ch:
			delete(g.pending, id)
			g.mu.Unlock()
			return result
		default:
		}
		if current, ok := g.pending[id]; ok && current.ch == ch {
			delete(g.pending, id)
		}
		g.mu.Unlock()
		return ResponseDeny
	}
}

// CancelAll resolves every pending request as denied and clears the pending set.
func (g *Gate) CancelAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, ch := range g.pending {
		select {
		case ch.ch <- ResponseDeny:
		default:
		}
		delete(g.pending, id)
	}
}

// PendingForSession returns a snapshot of the pending requests owned by
// sessionID, keyed by request id. The order is unspecified: consumers key by
// Request.ID.
func (g *Gate) PendingForSession(sessionID string) []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Request
	for _, p := range g.pending {
		if p.req.SessionID == sessionID {
			req := p.req
			// Detach the batch slices so the snapshot shares no backing array
			// with the gate's live request.
			req.BatchFiles = append([]string(nil), p.req.BatchFiles...)
			req.BatchResolvedFiles = append([]string(nil), p.req.BatchResolvedFiles...)
			out = append(out, req)
		}
	}
	return out
}

// CancelSession resolves pending requests owned by sessionID as denied.
func (g *Gate) CancelSession(sessionID string) int {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cancelled := 0
	for id, pending := range g.pending {
		if pending.req.SessionID != sessionID {
			continue
		}
		select {
		case pending.ch <- ResponseDeny:
		default:
		}
		delete(g.pending, id)
		cancelled++
	}
	return cancelled
}

// Respond delivers an answer to the pending request with the given id.
func (g *Gate) Respond(id string, allow bool) error {
	if allow {
		return g.RespondAction(id, string(ResponseAllow))
	}
	return g.RespondAction(id, string(ResponseDeny))
}

// RespondForSession delivers an answer after validating request ownership.
func (g *Gate) RespondForSession(sessionID, id string, allow bool) error {
	if allow {
		return g.RespondActionForSession(sessionID, id, string(ResponseAllow))
	}
	return g.RespondActionForSession(sessionID, id, string(ResponseDeny))
}

// RespondAction delivers an action to the pending request with the given id.
func (g *Gate) RespondAction(id string, action string) error {
	return g.respondAction("", id, action, false)
}

// RespondActionForSession delivers an action after validating request ownership.
func (g *Gate) RespondActionForSession(sessionID, id string, action string) error {
	return g.respondAction(sessionID, id, action, true)
}

func (g *Gate) respondAction(sessionID, id string, action string, requireSession bool) error {
	response := ResponseAction(action)
	switch response {
	case ResponseAllow, ResponseDeny, ResponseAllowAll:
	default:
		return fmt.Errorf("invalid permission response action %q", action)
	}

	g.mu.Lock()
	pending, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrUnknownRequest, id)
	}
	if requireSession && !requestMatchesSession(pending.req, sessionID) {
		g.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrSessionMismatch, id)
	}
	if response == ResponseAllowAll && !pending.req.CanAllowAll {
		response = ResponseAllow
	}
	pending.ch <- response
	delete(g.pending, id)
	g.mu.Unlock()
	return nil
}

// ProjectSaveRequest returns the pending request after validating that
// project-level permission persistence is allowed.
func (g *Gate) ProjectSaveRequest(sessionID, id string, requireSession bool) (Request, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.pending[id]
	if !ok {
		return Request{}, fmt.Errorf("%w: id %q", ErrUnknownRequest, id)
	}
	if requireSession && !requestMatchesSession(pending.req, sessionID) {
		return Request{}, fmt.Errorf("%w: id %q", ErrSessionMismatch, id)
	}
	if pending.req.DisableProjectSave {
		return Request{}, errors.New("project permission save is disabled for this request")
	}
	return pending.req, nil
}

func requestMatchesSession(req Request, sessionID string) bool {
	if req.SessionID == "" {
		return true
	}
	return strings.TrimSpace(sessionID) == req.SessionID
}

func newID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
