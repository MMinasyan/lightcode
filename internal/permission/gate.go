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
	ID       string `json:"id"`
	ToolName string `json:"tool"`
	Arg      string `json:"args"`
}

// Gate bridges synchronous permission checks to an async request/response
// round-trip through the Wails frontend.
type Gate struct {
	mu      sync.Mutex
	pending map[string]chan bool

	// OnRequest is called when a new permission request is registered.
	OnRequest func(req Request)
}

// NewGate returns a Gate that calls onRequest for each new permission request.
func NewGate(onRequest func(req Request)) *Gate {
	return &Gate{
		pending:   make(map[string]chan bool),
		OnRequest: onRequest,
	}
}

// Ask registers a pending request and blocks until the user responds or
// ctx is cancelled. Returns true for allow, false for deny.
func (g *Gate) Ask(ctx context.Context, toolName, arg string) bool {
	id := newID()
	ch := make(chan bool, 1)

	g.mu.Lock()
	g.pending[id] = ch
	g.mu.Unlock()

	if g.OnRequest != nil {
		g.OnRequest(Request{ID: id, ToolName: toolName, Arg: arg})
	}

	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return false
	}
}

// Respond delivers an answer to the pending request with the given id.
func (g *Gate) Respond(id string, allow bool) error {
	g.mu.Lock()
	ch, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
	}
	g.mu.Unlock()

	if !ok {
		return fmt.Errorf("no pending permission request with id %q", id)
	}
	ch <- allow
	return nil
}

func newID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
