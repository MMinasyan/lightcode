package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// Request is the structured payload sent to the frontend when the gate
// needs to ask the user for permission.
type Request struct {
	ID          string   `json:"id"`
	ToolName    string   `json:"tool"`
	Arg         string   `json:"args"`
	CanAllowAll bool     `json:"can_allow_all,omitempty"`
	BatchIndex  int      `json:"batch_index,omitempty"`
	BatchTotal  int      `json:"batch_total,omitempty"`
	BatchFiles  []string `json:"batch_files,omitempty"`
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
	pending map[string]chan ResponseAction

	// OnRequest is called when a new permission request is registered.
	OnRequest func(req Request)
}

// NewGate returns a Gate that calls onRequest for each new permission request.
func NewGate(onRequest func(req Request)) *Gate {
	return &Gate{
		pending:   make(map[string]chan ResponseAction),
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
	g.pending[id] = ch
	g.mu.Unlock()

	if g.OnRequest != nil {
		g.OnRequest(req)
	}

	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return ResponseDeny
	}
}

// Respond delivers an answer to the pending request with the given id.
func (g *Gate) Respond(id string, allow bool) error {
	if allow {
		return g.RespondAction(id, string(ResponseAllow))
	}
	return g.RespondAction(id, string(ResponseDeny))
}

// RespondAction delivers an action to the pending request with the given id.
func (g *Gate) RespondAction(id string, action string) error {
	response := ResponseAction(action)
	switch response {
	case ResponseAllow, ResponseDeny, ResponseAllowAll:
	default:
		return fmt.Errorf("invalid permission response action %q", action)
	}

	g.mu.Lock()
	ch, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
	}
	g.mu.Unlock()

	if !ok {
		return fmt.Errorf("no pending permission request with id %q", id)
	}
	ch <- response
	return nil
}

func newID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
