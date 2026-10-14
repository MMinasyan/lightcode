package cliclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/clientview"
	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

type readChain struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	stamp  stamp
}
type sessionReader struct {
	id                  string
	hydration           *protocol.Hydration
	items               []protocol.ConversationItem
	cursor              *string
	floor               *protocol.SessionRevision
	assignmentVersion   uint64
	dirty, pendingOlder bool
	chain               *readChain
}

func call(s stamp, operation, id string, body, query any, result any) (json.RawMessage, error) {
	if s.conn == nil || s.ctx == nil {
		return nil, errors.New("Runtime is not connected")
	}
	envelope := make(map[string]any)
	if id != "" {
		envelope["session_id"] = id
	}
	if body != nil {
		envelope["body"] = body
	}
	if query != nil {
		envelope["query"] = query
	}
	params, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	raw, err := clientops.Call(s.ctx, s.conn, operation, params)
	if err == nil && result != nil {
		// UseNumber keeps opaque numeric lexemes exact while typed fields keep
		// their generated type checks; unused opaque fields cannot reject a
		// valid response after the shared invocation succeeds.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(result)
	}
	return raw, err
}

func (c *CLI) newReaderLocked(id string) *sessionReader { return &sessionReader{id: id, dirty: true} }
func (c *CLI) resetReadersLocked() {
	for _, r := range []*sessionReader{c.root, c.child} {
		if r != nil && r.chain != nil {
			r.chain.cancel()
		}
	}
	c.root = nil
	c.child = nil
	c.forkBoundary = nil
	c.forkRewind = false
	c.clearProgressLocked()
}
func (c *CLI) restampRootLocked() { c.resumeReaderLocked(c.root) }
func (c *CLI) readerCurrentLocked(r *sessionReader, chain *readChain) bool {
	return c.currentLocked(chain.stamp) && (c.root == r || c.child == r) && r.chain == chain
}
func (c *CLI) kickReaderLocked(r *sessionReader) {
	if r == nil || c.conn == nil || r.chain != nil || (!r.dirty && !r.pendingOlder) {
		return
	}
	s := c.stampLocked()
	ctx, cancel := context.WithCancel(s.ctx)
	s.ctx = ctx
	chain := &readChain{ctx: ctx, cancel: cancel, done: make(chan struct{}), stamp: s}
	r.chain = chain
	c.taskLocked(func() { defer cancel(); defer close(chain.done); c.readSession(r, chain) })
}

func (c *CLI) refreshReadersLocked() {
	for _, r := range []*sessionReader{c.root, c.child} {
		if r != nil {
			r.dirty = true
			c.kickReaderLocked(r)
		}
	}
}

var errContinuity = errors.New("invalid conversation continuity")
var errStalePage = errors.New("stale conversation page")

func pageFloor(next protocol.SessionRevision, floor *protocol.SessionRevision, instance string) error {
	if next.InstanceId != instance {
		return errContinuity
	}
	if _, err := clientview.CompareSessionRevision(next, next); err != nil {
		return fmt.Errorf("%w: %v", errContinuity, err)
	}
	if floor != nil {
		order, err := clientview.CompareSessionRevision(next, *floor)
		if err != nil {
			return fmt.Errorf("%w: %v", errContinuity, err)
		}
		if order < 0 {
			return errStalePage
		}
	}
	return nil
}

func (c *CLI) readSession(r *sessionReader, chain *readChain) {
	for {
		c.mu.Lock()
		if !c.readerCurrentLocked(r, chain) {
			c.mu.Unlock()
			return
		}
		if !r.dirty && !r.pendingOlder {
			r.chain = nil
			c.notifyLocked()
			c.mu.Unlock()
			return
		}
		older := !r.dirty && r.pendingOlder
		if older {
			r.pendingOlder = false
		} else {
			r.dirty = false
		}
		current, items, cursor, floor := r.hydration, r.items, r.cursor, r.floor
		var config *protocol.ConfigurationRevision
		var warnings *protocol.WarningsRevision
		if c.configuration != nil {
			copy := c.configuration.ConfigurationRevision
			config = &copy
		}
		if c.warnings != nil {
			copy := c.warnings.WarningsRevision
			warnings = &copy
		}
		c.mu.Unlock()
		var next protocol.Hydration
		var nextItems []protocol.ConversationItem
		var nextCursor *string
		var nextFloor protocol.SessionRevision
		var err error
		if older {
			if cursor == nil {
				continue
			}
			var page protocol.HistoryPage
			_, err = call(chain.stamp, "GetSessionHistory", r.id, nil, protocol.GetSessionHistoryParams{Cursor: cursor}, &page)
			if err == nil {
				err = pageFloor(page.SessionRevision, floor, chain.stamp.conn.InstanceID())
			}
			if err == nil {
				nextItems, err = clientview.MergeConversation(page.Items, items)
				if err != nil {
					err = fmt.Errorf("%w: %v", errContinuity, err)
				}
				nextCursor = page.OlderCursor
				nextFloor = page.SessionRevision
			}
		} else {
			_, err = call(chain.stamp, "GetSessionHydration", r.id, nil, nil, &next)
			if err == nil && next.Session.SessionId != r.id {
				err = errContinuity
			}
			if err == nil {
				var accepted bool
				accepted, err = clientview.AcceptHydration(current, next, chain.stamp.conn.InstanceID(), config, warnings)
				if err != nil {
					err = fmt.Errorf("%w: %v", errContinuity, err)
				} else if !accepted {
					err = errStalePage
				}
			}
			if err == nil {
				err = pageFloor(next.SessionRevision, floor, chain.stamp.conn.InstanceID())
			}
			if err == nil {
				nextItems, nextCursor, nextFloor, err = c.bridge(chain.stamp, r.id, items, cursor, next)
			}
		}
		c.mu.Lock()
		if !c.readerCurrentLocked(r, chain) {
			c.mu.Unlock()
			return
		}
		if err == nil && !older && r == c.root && next.Session.ParentSessionId != nil {
			c.navigationLocked()
			c.resetReadersLocked()
			c.rootID = ""
			c.childID = ""
			fresh := c.stampLocked()
			c.mu.Unlock()
			c.report(fresh, terminal.RenderErrorMsg("Children are inspection-only; select a root Session."), false)
			return
		}
		if err == nil && !older {
			config = nil
			warnings = nil
			if c.configuration != nil {
				config = &c.configuration.ConfigurationRevision
			}
			if c.warnings != nil {
				warnings = &c.warnings.WarningsRevision
			}
			accepted, checkErr := clientview.AcceptHydration(r.hydration, next, chain.stamp.conn.InstanceID(), config, warnings)
			if checkErr != nil {
				err = errContinuity
			} else if !accepted {
				err = errStalePage
			}
		}
		if err != nil {
			var syntax *json.SyntaxError
			var shape *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &shape) || errors.Is(err, clientops.ErrInvalidResponse) {
				err = fmt.Errorf("%w: %v", errContinuity, err)
			}
			if errors.Is(err, errStalePage) {
				if older {
					r.pendingOlder = true
				}
				r.dirty = true
				c.mu.Unlock()
				continue
			}
			r.chain = nil
			c.mu.Unlock()
			if errors.Is(err, errContinuity) {
				c.repair(chain.stamp.conn)
			} else {
				c.report(chain.stamp, terminal.RenderErrorMsg(err.Error()), false)
			}
			return
		}
		r.items, r.cursor, r.floor = nextItems, nextCursor, &nextFloor
		r.assignmentVersion++
		if !older {
			r.hydration = &next
			if c.displayedLocked() == r {
				c.clearProgressLocked()
			}
			c.reconcileAttemptsLocked(r)
		}
		c.notifyLocked()
		c.mu.Unlock()
	}
}

func (c *CLI) bridge(s stamp, id string, cached []protocol.ConversationItem, oldest *string, next protocol.Hydration) ([]protocol.ConversationItem, *string, protocol.SessionRevision, error) {
	segment := next.Conversation.Items
	cursor := next.Conversation.OlderCursor
	floor := next.SessionRevision
	missing := make(map[string]bool)
	if len(cached) != 0 {
		anchor, err := clientview.ItemID(cached[len(cached)-1])
		if err != nil {
			return nil, nil, floor, errContinuity
		}
		missing[anchor] = true
	}
	for _, item := range cached {
		kind, _ := item.Discriminator()
		if kind != "assistant" {
			continue
		}
		a, err := item.AsAssistantItem()
		if err != nil {
			return nil, nil, floor, errContinuity
		}
		for _, call := range a.ToolCalls {
			if call.Status == nil {
				missing[a.ItemId] = true
			}
		}
	}
	visit := func(items []protocol.ConversationItem) error {
		for _, item := range items {
			id, err := clientview.ItemID(item)
			if err != nil {
				return err
			}
			delete(missing, id)
		}
		return nil
	}
	if err := visit(segment); err != nil {
		return nil, nil, floor, errContinuity
	}
	for len(missing) != 0 {
		if cursor == nil {
			return nil, nil, floor, errContinuity
		}
		var page protocol.HistoryPage
		if _, err := call(s, "GetSessionHistory", id, nil, protocol.GetSessionHistoryParams{Cursor: cursor}, &page); err != nil {
			return nil, nil, floor, err
		}
		if err := pageFloor(page.SessionRevision, &floor, s.conn.InstanceID()); err != nil {
			return nil, nil, floor, err
		}
		merged, err := clientview.MergeConversation(page.Items, segment)
		if err != nil {
			return nil, nil, floor, errContinuity
		}
		segment = merged
		cursor = page.OlderCursor
		floor = page.SessionRevision
		if err := visit(page.Items); err != nil {
			return nil, nil, floor, errContinuity
		}
	}
	merged, err := clientview.MergeConversation(cached, segment)
	if err != nil {
		return nil, nil, floor, errContinuity
	}
	// The old cursor continues addressing the oldest cached edge unless the
	// walk actually fetched a prefix preceding that edge.
	if len(cached) != 0 && len(merged) != 0 {
		oldID, _ := clientview.ItemID(cached[0])
		firstID, _ := clientview.ItemID(merged[0])
		if oldID == firstID {
			cursor = oldest
		}
	}
	return merged, cursor, floor, nil
}

func (c *CLI) kickGlobalsLocked() {
	if c.closed || c.conn == nil || !c.globalsDirty || c.globalsBusy {
		return
	}
	c.globalsBusy = true
	c.taskLocked(c.readGlobals)
}
func (c *CLI) readGlobals() {
	for {
		c.mu.Lock()
		if c.closed || c.conn == nil || !c.globalsDirty {
			c.globalsBusy = false
			c.mu.Unlock()
			return
		}
		c.globalsDirty = false
		s := c.stampLocked()
		c.mu.Unlock()
		var config protocol.ConfigurationView
		_, configErr := call(s, "GetConfiguration", "", nil, nil, &config)
		var warnings protocol.WarningsSnapshot
		_, warningErr := call(s, "GetWarnings", "", nil, nil, &warnings)
		c.mu.Lock()
		if !c.connectionCurrentLocked(s) {
			c.globalsDirty = true
			c.mu.Unlock()
			continue
		}
		invalid := false
		if configErr == nil {
			floor := protocol.ConfigurationRevision{InstanceId: s.conn.InstanceID(), Generation: "0"}
			if c.configuration != nil {
				floor = c.configuration.ConfigurationRevision
			}
			order, err := clientview.CompareConfigurationRevision(config.ConfigurationRevision, floor)
			if err != nil {
				configErr = err
				invalid = true
			} else if order >= 0 {
				c.configuration = &config
			} else {
				c.globalsDirty = true
			}
		}
		if warningErr == nil {
			floor := protocol.WarningsRevision{InstanceId: s.conn.InstanceID(), Revision: "0"}
			if c.warnings != nil {
				floor = c.warnings.WarningsRevision
			}
			order, err := clientview.CompareWarningsRevision(warnings.WarningsRevision, floor)
			if err != nil {
				warningErr = err
				invalid = true
			} else if order >= 0 {
				c.warnings = &warnings
			} else {
				c.globalsDirty = true
			}
		}
		var syntax *json.SyntaxError
		var shape *json.UnmarshalTypeError
		invalid = invalid || errors.As(configErr, &syntax) || errors.As(configErr, &shape) || errors.As(warningErr, &syntax) || errors.As(warningErr, &shape) || errors.Is(configErr, clientops.ErrInvalidResponse) || errors.Is(warningErr, clientops.ErrInvalidResponse)
		if invalid {
			c.globalsBusy = false
			c.mu.Unlock()
			c.repair(s.conn)
			return
		}
		c.notifyLocked()
		c.mu.Unlock()
		// Runtime-global outcomes stay valid on their serving connection alone:
		// a local Session navigation neither drops nor re-reads them.
		if configErr != nil {
			c.reportConnection(s, terminal.RenderErrorMsg(configErr.Error()))
		}
		if warningErr != nil {
			c.reportConnection(s, terminal.RenderErrorMsg(warningErr.Error()))
		}
	}
}
