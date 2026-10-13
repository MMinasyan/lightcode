package acpclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/protocol"
)

// connection is one client-owned Runtime connection generation: its
// authenticated generated client, the generated Health it described, its one
// event subscription, and the context whose cancellation retires the
// generation's calls and stream together.
type connection struct {
	client *client.Client
	health protocol.Health
	ctx    context.Context
	cancel context.CancelFunc
	events <-chan protocol.Event
	errs   <-chan error
}

// establish returns the current connection or creates one through the common
// startup owner. Startup discovery and its mandatory probe run first; the
// candidate's event stream is opened next, and its already-reported
// establishment failure is observed synchronously before anything else, so a
// failed stream is never installed, announced, or read from. The descriptive
// Health read runs on the candidate context and one final gate repeats the
// check immediately before installation.
func (r *Runner) establish(ctx context.Context) (*connection, error) {
	r.connectMu.Lock()
	defer r.connectMu.Unlock()
	if conn := r.currentConnection(); conn != nil {
		return conn, nil
	}
	c, err := clienthost.Connect(ctx, r.opts.Startup)
	if err != nil {
		return nil, err
	}
	connCtx, cancel := context.WithCancel(ctx)
	conn := &connection{client: c, ctx: connCtx, cancel: cancel}
	conn.events, conn.errs = c.Events(connCtx)
	if err := streamEstablished(conn); err != nil {
		cancel()
		return nil, err
	}
	health, err := readHealth(connCtx, c)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := streamEstablished(conn); err != nil {
		cancel()
		return nil, err
	}
	conn.health = health
	r.mu.Lock()
	recovered := r.initialized
	r.conn = conn
	r.initialized = true
	r.mu.Unlock()
	r.pumps.Add(1)
	go r.pump(conn)
	r.publishConnection(conn, recovered)
	return conn, nil
}

// publishConnection is the one publication of a successful installation:
// every new connection announces itself, and an installation that recovers a
// previously established bridge records the repair and re-snapshots the
// retained selection. An installation that never had a connection publishes
// only the announcement.
func (r *Runner) publishConnection(conn *connection, recovered bool) {
	connected, _ := json.Marshal(conn.health)
	if !r.notify(conn, "runtime/connected", connected, nil) {
		return
	}
	if !recovered {
		return
	}
	if !r.notify(conn, "runtime/resync_required", nil, nil) {
		return
	}
	selected := r.selectionSnapshot()
	if selected.id == "" {
		return
	}
	raw, err := r.hydrate(conn, selected.id)
	if err != nil {
		return
	}
	r.notify(conn, "session/snapshot", raw, &selected)
}

// streamEstablished reports one candidate stream's already-reported terminal
// failure: client.Events queues an initial transport or non-200 failure
// before it returns, so a synchronous observation decides without waiting.
func streamEstablished(conn *connection) error {
	select {
	case err, ok := <-conn.errs:
		if !ok {
			return nil
		}
		return err
	default:
		return nil
	}
}

// readHealth reads the authenticated connection's generated Health through
// the shared invocation table.
func readHealth(ctx context.Context, c *client.Client) (protocol.Health, error) {
	raw, err := clientops.Call(ctx, c, "GetHealth", nil)
	if err != nil {
		return protocol.Health{}, err
	}
	var health protocol.Health
	if err := json.Unmarshal(raw, &health); err != nil {
		return protocol.Health{}, fmt.Errorf("runtime health: %v", err)
	}
	return health, nil
}

// hydrate reads one explicit Session's generated hydration through the
// shared invocation table on the generation that serves it, under that
// generation's context: retirement cancels the HTTP resources and the caller
// receives the real transport error under its original request ID.
func (r *Runner) hydrate(conn *connection, sessionID string) (json.RawMessage, error) {
	params, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
	}{SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	return clientops.Call(conn.ctx, conn.client, "GetSessionHydration", params)
}

func (r *Runner) currentConnection() *connection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn
}

// requireConnection returns the established connection or the unavailable
// transport error every recognized Runtime and Session method reports before
// a successful initialize.
func (r *Runner) requireConnection() (*connection, *rpcError) {
	r.mu.Lock()
	conn := r.conn
	initialized := r.initialized
	r.mu.Unlock()
	if !initialized || conn == nil {
		return nil, &rpcError{Code: codeTransport, Message: "runtime connection is not established"}
	}
	return conn, nil
}

// retire ends one generation: it is cleared before its calls and stream are
// canceled, so no later request can address it.
func (r *Runner) retire(conn *connection) {
	r.mu.Lock()
	if r.conn == conn {
		r.conn = nil
	}
	r.mu.Unlock()
	conn.cancel()
}

// pump relays one connection generation's events as runtime/event
// notifications. A terminal stream failure or a full progress queue ends the
// generation and makes one repair attempt — but only while the generation's
// own context is still live: an intentional retirement is not continuity
// loss and never repairs itself.
func (r *Runner) pump(conn *connection) {
	defer r.pumps.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-conn.ctx.Done():
			return
		case event, ok := <-conn.events:
			if !ok {
				if err, ok := <-conn.errs; ok && err != nil {
					r.repair(conn)
				}
				return
			}
			if !r.enqueueProgress(conn, event) {
				r.repair(conn)
				return
			}
		case err, ok := <-conn.errs:
			if !ok {
				return
			}
			if err != nil {
				r.repair(conn)
			}
			return
		}
	}
}

// repair is the one entry for continuity loss: an intentional retirement is
// not loss, so a generation whose own context is already canceled never
// repairs; otherwise it retires the lost generation and re-establishes
// through the one installation path, which decides and publishes the recovery
// itself. A failed attempt leaves the bridge unconnected with the
// established-once obligation intact and launches no further repair from the
// never-installed candidate.
func (r *Runner) repair(conn *connection) {
	if conn.ctx.Err() != nil {
		return
	}
	r.retire(conn)
	if r.ctx.Err() != nil {
		return
	}
	_, _ = r.establish(r.ctx)
}
