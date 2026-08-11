package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/version"
)

// deliveryJoinTimeout bounds how long close waits for the delivery drainer to
// finish. A drainer blocked inside a framework emit is abandoned after it; it
// holds no lock or state and process exit owns the blocked call.
var deliveryJoinTimeout = 5 * time.Second

// errAdapterClosed is returned by a current-target or navigation call that
// acquires navMu after close has won it.
var errAdapterClosed = errors.New("adapter is closed")

// frameKind selects how the sole drainer filters a frame against presentation
// current before emitting it.
type frameKind uint8

const (
	// frameNormal is delivered while its sessionID is presentation-current; an
	// empty sessionID is a global frame the drainer always delivers.
	frameNormal frameKind = iota
	// frameSubagent is delivered for a child registered under the presented root.
	frameSubagent
	// frameAdvance is a navigation/turn-action boundary: always delivered, and it
	// adopts sessionID as the new presentation current (empty for a detach).
	frameAdvance
)

// deliveryFrame is one immutable item the delivery FIFO carries: a named event
// with its payload, plus an optional native window title a navigation boundary
// carries so the drainer applies the title in the same ordered step as the
// boundary. kind and the session/subagent tags let the sole drainer filter it
// against presentation current at drain time, so the event callback that appends
// it never queries the owner. The sole drainer emits it.
type deliveryFrame struct {
	name      string
	payload   any
	title     string
	kind      frameKind
	sessionID string // frameNormal tag / frameAdvance destination
	parent    string // frameSubagent parent (root) session
	child     string // frameSubagent child session
}

// App is the Wails-bound struct that bridges the Go backend to the
// frontend. All exported methods are callable from JavaScript.
type App struct {
	ctx context.Context
	svc agent.AdapterService
	// agent is the concrete local owner this adapter constructs, initializes, and
	// shuts down. It also backs the concrete-only complete-state hydration surface,
	// which is intentionally not part of AdapterService.
	agent *agent.Agent

	// lifecycleMu serializes startup against shutdown. Startup initializes the owner
	// under it and records started; shutdown takes it, records closed, and tears down
	// only an owner startup actually initialized. This makes an early close that races
	// an asynchronous startup safe: shutdown before startup short-circuits both.
	lifecycleMu sync.Mutex
	started     bool
	closed      bool
	hostCancel  context.CancelFunc

	// navMu is the operation gate owning routing-current (session id + project
	// path). Ordinary current-target calls hold it across owner entry; a
	// cancellable/long call captures its id under it and releases before invoking
	// the owner; navigation holds it across the routing commit. Lock order is
	// lifecycleMu (App startup/shutdown) -> navMu -> the owner's own locks.
	// navClosed, set under navMu on close, makes a call waiting on navMu reject.
	navMu     sync.Mutex
	navClosed bool

	// routeMu guards the adapter's routing-current session id — a leaf below navMu.
	// Navigation and current-target operations take navMu then routeMu. Routing
	// current is where operations route; presentation current (owned by the drainer)
	// is what the frontend shows, and the two diverge while a boundary is in flight.
	// routeReadOnly sits beside it: the id of the routing-current session that was
	// opened read-only because another process holds its claim. The two states are
	// explicit: the routing setter clears it on every commit, and only the read-only
	// open path sets it, after committing the id it names — so a successful live
	// commit of the same session can never leave it stale.
	routeMu       sync.Mutex
	routeCurrent  string
	routeReadOnly string

	// routeProjectPath is the adapter's routing-current project path, owned by
	// navMu: navigation commits it and every project-scoped read captures it under
	// navMu. Nothing reads it off the operation gate, so it needs no leaf lock.
	routeProjectPath string

	// Delivery spine: every event and navigation payload is appended as a frame;
	// the single drainer is the only goroutine that emits to the frontend, so
	// delivery order is the drainer's write order. emitFn is the one emission
	// choke point (overridable in tests). The queue is unbounded by design: a
	// drainer blocked inside one framework emit grows it without limit rather
	// than block a producer appending under an owner lock, so appending never
	// waits for queue capacity or for the sink to accept the frame — only for
	// the queue mutex, behind another producer or the drainer's critical
	// section.
	emitFn         func(name string, payload any)
	titleFn        func(title string)
	deliveryMu     sync.Mutex
	deliveryFrames []deliveryFrame
	deliveryWake   chan struct{}
	deliveryClosed bool
	deliveryDone   chan struct{}
	deliveryOnce   sync.Once

	// presented is presentation current: the session id the drainer is currently
	// showing. It advances only when the drainer consumes a navigation/turn-action
	// boundary, so a stalled sink keeps presenting the prior session while routing
	// current has already moved. presentedChildren holds the subagent children
	// registered under it. Both are owned by the drainer and read or written only
	// under deliveryMu (the startup seed included).
	presented         string
	presentedChildren map[string]struct{}
}

type ModelCompletion = agent.ModelCompletion

// startup is called by Wails after the window is created. Wails runs it
// asynchronously, so it holds lifecycleMu across owner initialization: a close
// that races it either waits here or, if it already ran, short-circuits.
func (a *App) startup(ctx context.Context) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.closed {
		return
	}
	a.ctx = ctx
	// The host context only triggers the joined owner shutdown (the Init
	// watcher); the project LSP teardown runs on the owner context inside that
	// join. Initialize under a cancelable child so shutdown can release the
	// watcher. Delivery keeps using the original ctx, which stays valid until
	// the drainer is closed.
	hostCtx, cancel := context.WithCancel(ctx)
	a.hostCancel = cancel
	// Hold navMu across the whole startup so it is a readiness barrier. Wails loads
	// the frontend concurrently with this asynchronous startup, so every routing
	// operation waits here until routing is fully set up: it can neither read a
	// half-initialized route nor commit a project switch that startup would then
	// overwrite. Owner initialization takes no adapter lock, so this cannot deadlock.
	a.navMu.Lock()
	defer a.navMu.Unlock()
	a.startDelivery()
	a.svc.SetEventHandler(a.handleEvent)
	// Init resumes the most recent acquirable session and returns its id, so
	// the adapter adopts exactly the session that was resumed; a new session
	// is created only when nothing was resumed.
	sessionID := a.svc.Init(hostCtx)
	a.routeProjectPath = a.svc.ProjectRoot()
	if sessionID == "" {
		if id, err := a.svc.NewSessionForProjectPath(a.routeProjectPath, "primary"); err == nil {
			sessionID = id
		}
	}
	a.setCurrentSessionID(sessionID)
	a.seedPresented(sessionID)
	a.started = true
}

func (a *App) shutdown(_ context.Context) {
	a.lifecycleMu.Lock()
	a.closed = true
	started := a.started
	hostCancel := a.hostCancel
	// Win navMu so a current-target/navigation call waiting on it rejects instead
	// of racing teardown.
	a.navMu.Lock()
	a.navClosed = true
	a.navMu.Unlock()
	a.lifecycleMu.Unlock()

	a.closeDelivery()
	if !started {
		return
	}
	// Join the owner's turns and workers first: ShutdownOwner drains in-flight turns
	// while the internal event drainer is still alive, then stops the workers, tears
	// down the LSP services on the owner context inside the join, and detaches the
	// session stores when every turn finished. Only then cancel the host context,
	// whose sole watcher is the shutdown trigger goroutine.
	if a.agent != nil {
		// The Wails shutdown hook has no return channel: the stderr diagnostic
		// inside ShutdownOwner is this host's only available signal.
		_ = a.agent.ShutdownOwner()
	}
	if hostCancel != nil {
		hostCancel()
	}
}

// HydrateSession returns a session's complete live state for the frontend to
// apply as one snapshot before replaying subsequent live events. It reaches the
// concrete owner directly because complete-state hydration is not part of the
// shared AdapterService.
func (a *App) HydrateSession(sessionID string) (agent.HydrationState, error) {
	if a.agent == nil {
		return agent.HydrationState{}, fmt.Errorf("hydration is unavailable")
	}
	return a.agent.HydrateSession(sessionID)
}

// startDelivery installs the emission choke point and starts the sole drainer
// before any event callback is connected. emitFn defaults to the framework emit
// and is left untouched when a test set it first.
func (a *App) startDelivery() {
	if a.emitFn == nil {
		a.emitFn = func(name string, payload any) {
			wailsRuntime.EventsEmit(a.ctx, name, payload)
		}
	}
	if a.titleFn == nil {
		a.titleFn = func(title string) {
			wailsRuntime.WindowSetTitle(a.ctx, title)
		}
	}
	a.deliveryWake = make(chan struct{}, 1)
	a.deliveryDone = make(chan struct{})
	go a.runDeliveryDrainer()
}

// enqueueFrame appends one frame and wakes the drainer. It never blocks, emits,
// or queries the owner, so it is safe to call from an event callback that runs
// under an owner lock.
func (a *App) enqueueFrame(f deliveryFrame) {
	a.deliveryMu.Lock()
	if a.deliveryClosed {
		a.deliveryMu.Unlock()
		return
	}
	a.deliveryFrames = append(a.deliveryFrames, f)
	a.deliveryMu.Unlock()
	select {
	case a.deliveryWake <- struct{}{}:
	default:
	}
}

// emitFrame appends a global frame the drainer always delivers.
func (a *App) emitFrame(name string, payload any) {
	a.enqueueFrame(deliveryFrame{name: name, payload: payload})
}

// emitSessionFrame appends a session-tagged frame the drainer delivers only while
// that session is presentation-current. An empty session id is a global frame.
func (a *App) emitSessionFrame(sessionID, name string, payload any) {
	a.enqueueFrame(deliveryFrame{name: name, payload: payload, sessionID: strings.TrimSpace(sessionID)})
}

// emitSubagentFrame appends a subagent frame the drainer delivers only for a child
// registered under the presentation-current root.
func (a *App) emitSubagentFrame(ev agent.Event, name string, payload any) {
	a.enqueueFrame(deliveryFrame{
		name:    name,
		payload: payload,
		kind:    frameSubagent,
		parent:  strings.TrimSpace(ev.ParentSessionID),
		child:   strings.TrimSpace(ev.SubagentSessionID),
	})
}

// enqueueBoundary appends a navigation/turn-action boundary: the drainer delivers
// it and then adopts sessionID as presentation current (empty for a detach). A
// native window title, if any, rides the same ordered step.
func (a *App) enqueueBoundary(name string, payload any, title, sessionID string) {
	a.enqueueFrame(deliveryFrame{
		name:      name,
		payload:   payload,
		title:     title,
		kind:      frameAdvance,
		sessionID: strings.TrimSpace(sessionID),
	})
}

// seedPresented sets presentation current at startup, before the frontend's
// hydration pull, so live frames for the initially-current session reach it. It
// takes deliveryMu because the drainer is already running.
func (a *App) seedPresented(id string) {
	a.deliveryMu.Lock()
	a.presented = strings.TrimSpace(id)
	a.presentedChildren = nil
	a.deliveryMu.Unlock()
}

// runDeliveryDrainer is the only goroutine that emits to the frontend. It drains
// frames in FIFO order and exits once closed and drained.
func (a *App) runDeliveryDrainer() {
	defer close(a.deliveryDone)
	for {
		a.deliveryMu.Lock()
		if a.deliveryClosed {
			// Once closed, stop emitting and drop pending frames — including any
			// queued behind an abandoned blocked emit, which must not reach the
			// frontend after shutdown has proceeded. The drop is deliberate,
			// unlike the protocol host's close, which drains its backlog because
			// its client process is still reading the pipe: the Wails framework
			// stops its main loop and releases the window and webview before
			// invoking the shutdown hook (verified in Wails v2.12.0 on all three
			// platforms), so an emit from inside that hook is never dispatched —
			// on Linux the idle source never fires and the queued entry leaks.
			a.deliveryMu.Unlock()
			return
		}
		if len(a.deliveryFrames) == 0 {
			a.deliveryMu.Unlock()
			<-a.deliveryWake
			continue
		}
		frame := a.deliveryFrames[0]
		a.deliveryFrames = a.deliveryFrames[1:]
		deliver := a.presentAcceptsLocked(frame)
		a.deliveryMu.Unlock()
		if !deliver {
			continue
		}
		a.emitFn(frame.name, frame.payload)
		if frame.title != "" {
			// Recheck close before applying the title: if emitFn blocked and close
			// abandoned the drainer meanwhile, the window is going away and the title
			// must not change after shutdown has proceeded.
			a.deliveryMu.Lock()
			closed := a.deliveryClosed
			a.deliveryMu.Unlock()
			if !closed {
				a.titleFn(frame.title)
			}
		}
	}
}

// presentAcceptsLocked decides whether a frame reaches the frontend and adopts a
// boundary's destination as presentation current. It runs under deliveryMu on the
// sole drainer, so presented and its subagent child set are drainer-owned and
// never queried from the owner: a session-tagged frame reaches the frontend only
// while its session is presentation-current, a boundary always passes and advances
// presented (empty for a detach), and a subagent frame passes through the child
// set registered under the presented root.
func (a *App) presentAcceptsLocked(f deliveryFrame) bool {
	switch f.kind {
	case frameAdvance:
		a.presented = f.sessionID
		a.presentedChildren = nil
		return true
	case frameSubagent:
		if a.presented == "" {
			return false
		}
		if f.parent == a.presented {
			if a.presentedChildren == nil {
				a.presentedChildren = make(map[string]struct{})
			}
			a.presentedChildren[f.child] = struct{}{}
			return true
		}
		_, ok := a.presentedChildren[f.child]
		return ok
	default:
		return f.sessionID == "" || f.sessionID == a.presented
	}
}

// closeDelivery rejects further frames and joins the drainer, abandoning it if
// it is blocked inside one framework emit.
func (a *App) closeDelivery() {
	a.deliveryOnce.Do(func() {
		a.deliveryMu.Lock()
		a.deliveryClosed = true
		a.deliveryMu.Unlock()
		select {
		case a.deliveryWake <- struct{}{}:
		default:
		}
		if a.deliveryDone == nil {
			return
		}
		select {
		case <-a.deliveryDone:
		case <-time.After(deliveryJoinTimeout):
		}
	})
}

func (a *App) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		emit := func(name string, payload any) { a.emitSubagentFrame(ev, name, payload) }
		switch ev.Kind {
		case agent.EventTextDelta:
			emit("subagent_token", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"seq":       ev.Seq,
				"content":   ev.Result,
			})
		case agent.EventToolCallStart:
			emit("subagent_tool_start", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"seq":       ev.Seq,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
			})
		case agent.EventToolCallEnd:
			// Not sequence-gated: a tool result updates its row in place without
			// advancing the sequence, so gating it would drop every result whose
			// start already advanced the high-water. Delivered id-keyed and
			// applied idempotently, exactly as the root branch handles it.
			emit("subagent_tool_result", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
				"success":   !ev.IsError,
				"output":    ev.Result,
				"metadata":  ev.Metadata,
			})
		case agent.EventSubagentStart:
			emit("subagent_session_start", map[string]any{
				"sessionId":      ev.SubagentSessionID,
				"taskToolCallId": ev.ToolCallID,
				"taskIndex":      ev.TaskIndex,
			})
		case agent.EventBackgroundProcessComplete:
			if ev.BackgroundProcess != nil {
				emit("subagent_background_process_complete", map[string]any{
					"sessionId": ev.SubagentSessionID,
					"seq":       ev.Seq,
					"id":        ev.BackgroundProcess.ID,
					"command":   ev.BackgroundProcess.Command,
					"reason":    ev.BackgroundProcess.Reason,
					"exitCode":  ev.BackgroundProcess.ExitCode,
					"success":   !ev.IsError,
					"output":    ev.Result,
				})
			}
		case agent.EventUserMessageDisplay:
			emit("subagent_user_message", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"seq":       ev.Seq,
				"turn":      ev.Turn,
				"content":   ev.Result,
			})
		case agent.EventGenericSystemSignal:
			emit("subagent_system_signal", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"seq":       ev.Seq,
				"content":   "System: " + ev.Result,
			})
		}
		return
	}
	// Every frame this callback appends carries the event's session tag; the sole
	// drainer, not this callback, decides delivery against presentation current, so
	// the callback never queries the owner for liveness.
	emit := func(name string, payload any) { a.emitSessionFrame(ev.SessionID, name, payload) }
	switch ev.Kind {
	case agent.EventTextDelta:
		emit("token", map[string]any{
			"seq":     ev.Seq,
			"content": ev.Result,
		})
	case agent.EventToolCallStart:
		emit("tool_start", map[string]any{
			"seq":  ev.Seq,
			"id":   ev.ToolCallID,
			"name": ev.ToolName,
			"args": ev.Args,
		})
	case agent.EventToolCallEnd:
		emit("tool_result", map[string]any{
			"id":       ev.ToolCallID,
			"name":     ev.ToolName,
			"args":     ev.Args,
			"success":  !ev.IsError,
			"output":   ev.Result,
			"metadata": ev.Metadata,
		})
	case agent.EventBackgroundProcessComplete:
		if ev.BackgroundProcess != nil {
			emit("background_process_complete", map[string]any{
				"seq":      ev.Seq,
				"id":       ev.BackgroundProcess.ID,
				"command":  ev.BackgroundProcess.Command,
				"reason":   ev.BackgroundProcess.Reason,
				"exitCode": ev.BackgroundProcess.ExitCode,
				"success":  !ev.IsError,
				"output":   ev.Result,
			})
		}
	case agent.EventUserMessageDisplay:
		emit("user_message", map[string]any{
			"seq":     ev.Seq,
			"turn":    ev.Turn,
			"content": ev.Result,
		})
	case agent.EventGenericSystemSignal:
		emit("system_signal", map[string]any{
			"seq":     ev.Seq,
			"content": "System: " + ev.Result,
		})
	case agent.EventQueueChanged:
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		emit("queue_changed", map[string]any{
			"items":   queue,
			"version": ev.QueueVersion,
		})
	case agent.EventUsage:
		// Apply the event's absolute cumulative report as a replacement rather than
		// querying the owner, so this callback never re-enters the owner while it is
		// emitting under tokensMu.
		report := agent.TokenReport{}
		if ev.CumulativeTokens != nil {
			report = *ev.CumulativeTokens
		}
		emit("usage", report)
	case agent.EventTurnStart:
		emit("turn_start", map[string]any{"turn": ev.Turn})
		emit("status", map[string]any{"state": "streaming"})
	case agent.EventTurnEnd:
		emit("turn_end", map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled})
		emit("status", map[string]any{"state": "idle"})
	case agent.EventError:
		// A frame carries a sequence only when the event actually has one: a
		// sessionless error is emitted directly, never sequenced, and a
		// zero-stamped seq would gate it as already shown against every
		// snapshot high-water, dropping the error.
		errorFrame := map[string]any{"message": ev.Error}
		if ev.Seq != 0 {
			errorFrame["seq"] = ev.Seq
		}
		emit("error", errorFrame)
	case agent.EventPermissionRequest:
		emit("permission_request", map[string]any{
			"id":                 ev.PermReq.ID,
			"sessionId":          ev.PermReq.SessionID,
			"projectId":          ev.PermReq.ProjectID,
			"tool":               ev.PermReq.ToolName,
			"args":               ev.PermReq.Arg,
			"resolvedArg":        ev.PermReq.ResolvedArg,
			"canAllowAll":        ev.PermReq.CanAllowAll,
			"canSaveProject":     !ev.PermReq.DisableProjectSave,
			"batchIndex":         ev.PermReq.BatchIndex,
			"batchTotal":         ev.PermReq.BatchTotal,
			"batchFiles":         ev.PermReq.BatchFiles,
			"batchResolvedFiles": ev.PermReq.BatchResolvedFiles,
		})
	case agent.EventPermissionResolved:
		emit("permission_resolved", map[string]any{
			"id":        ev.PermReq.ID,
			"sessionId": ev.PermReq.SessionID,
		})
	case agent.EventCompactionStart:
		emit("compaction_start", nil)
	case agent.EventCompactionEnd:
		emit("compaction_end", nil)
	case agent.EventSessionRewrite:
		a.emitResyncBoundary(ev.SessionID, ev.RewritePayload)
	case agent.EventWarning:
		emit("warnings", ev.Warnings)
	}
}

// turnActionBoundary is the ordered frame a fork, history revert, or code revert
// appends through the delivery FIFO: the destination session's complete state (nil
// when the action changed no session), any files a code revert kept unchanged, and
// the warning a fork carries when its best-effort code revert failed. The ordered
// consumer applies the state, the skip notice, and the warning in that order, so no
// live frame interleaves between them or clobbers either notice.
type turnActionBoundary struct {
	State        *agent.HydrationState    `json:"state"`
	SkippedFiles []snapshot.SkippedRevert `json:"skippedFiles"`
	Warning      string                   `json:"warning,omitempty"`
}

// emitTurnActionNotice appends a code revert's skip notice as an ordered notice-only
// frame (no state change), so a refresh already queued ahead of it applies first and
// cannot clobber the notice appended after.
func (a *App) emitTurnActionNotice(skipped []snapshot.SkippedRevert) {
	if a.ctx == nil || len(skipped) == 0 {
		return
	}
	a.emitFrame("turn_action", turnActionBoundary{SkippedFiles: skipped})
}

// emitResyncBoundary re-syncs a compacted session's transcript and tokens through
// one ordered frame. The payload is built by the producer and carried on the
// event, so this callback only enqueues it and never re-enters the owner. The
// frontend applies the compaction-affected transcript and tokens, leaving
// activity and queue that compaction did not change to the live stream.
func (a *App) emitResyncBoundary(sessionID string, payload *agent.SessionPayload) {
	if a.ctx == nil {
		return
	}
	p := agent.SessionPayload{}
	if payload != nil {
		p = *payload
	}
	a.emitSessionFrame(sessionID, "resync", p)
}

func (a *App) setCurrentSessionID(id string) {
	id = strings.TrimSpace(id)
	a.routeMu.Lock()
	// Every routing commit clears the read-only marker unconditionally: only
	// the read-only open path sets it, after committing the id it names, so a
	// successful live commit of the same session can never leave it stale.
	a.routeReadOnly = ""
	a.routeCurrent = id
	a.routeMu.Unlock()
}

// markRouteReadOnly records that the routing current was opened read-only
// because another process holds the session's claim. The routing setter clears
// the marker on every commit, so it must be set after committing the id it
// names.
func (a *App) markRouteReadOnly(id string) {
	id = strings.TrimSpace(id)
	a.routeMu.Lock()
	a.routeReadOnly = id
	a.routeMu.Unlock()
}

// routeReadOnlyNames reports whether the given id is the read-only-marked
// routing current.
func (a *App) routeReadOnlyNames(id string) bool {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	return a.routeReadOnly != "" && a.routeReadOnly == id
}

// clearRouteIfCurrent clears the routing current only when it still equals id, so
// a stale validation cannot clear a session another goroutine has since selected.
func (a *App) clearRouteIfCurrent(id string) {
	id = strings.TrimSpace(id)
	a.routeMu.Lock()
	if a.routeCurrent == id {
		a.routeCurrent = ""
	}
	a.routeMu.Unlock()
}

func (a *App) currentSessionID() string {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	return a.routeCurrent
}

func (a *App) currentSession() (string, error) {
	id := a.currentSessionID()
	if id == "" {
		return "", fmt.Errorf("no current session")
	}
	return id, nil
}

// boundedSessionIDLocked returns the routing-current session id for an ordinary
// current-target call. The caller holds navMu across the owner call so the id it
// targets cannot change under it; a close that won navMu first rejects here.
func (a *App) boundedSessionIDLocked() (string, error) {
	if a.navClosed {
		return "", errAdapterClosed
	}
	return a.currentSession()
}

func (a *App) liveCurrentSessionID() string {
	id := a.currentSessionID()
	if id == "" {
		return ""
	}
	if a.routeReadOnlyNames(id) {
		// The routing current was opened read-only because another process
		// drives it; it stays routed, just not live here.
		return ""
	}
	if _, err := a.svc.SessionSummaryForSession(id); err != nil {
		a.clearRouteIfCurrent(id)
		return ""
	}
	return id
}

// liveCurrentSessionOrErr is liveCurrentSessionID for a caller that must
// distinguish why no live session is current: the read-only-marked session is
// current, so another process drives it, or there is no current session at all.
func (a *App) liveCurrentSessionOrErr() (string, error) {
	id := a.liveCurrentSessionID()
	if id != "" {
		return id, nil
	}
	current := a.currentSessionID()
	if a.routeReadOnlyNames(current) {
		return "", agent.SessionContendedError(current)
	}
	return "", fmt.Errorf("no current session")
}

func (a *App) resolveSessionID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		return id, nil
	}
	return a.currentSession()
}

func (a *App) currentSessionSummary() agent.SessionSummary {
	id := a.currentSessionID()
	if id == "" {
		return agent.SessionSummary{}
	}
	s, err := a.svc.SessionSummaryForSessionOrPersisted(id)
	if err != nil {
		a.setCurrentSessionID("")
		return agent.SessionSummary{}
	}
	return s
}

func (a *App) tokenUsage() agent.TokenReport {
	id := a.currentSessionID()
	if id == "" {
		return agent.TokenReport{}
	}
	report, err := a.svc.TokenUsageForSession(id)
	if err != nil {
		return agent.TokenReport{}
	}
	return report
}

// removedCurrent clears the routing current when the removed id was it, and
// reports whether the adapter should refresh its view.
func (a *App) removedCurrent(id string) bool {
	id = strings.TrimSpace(id)
	wasCurrent := a.currentSessionID() == id
	if wasCurrent {
		a.setCurrentSessionID("")
	}
	return wasCurrent
}

func (a *App) sessionMessages() []agent.DisplayMessage {
	a.navMu.Lock()
	if a.navClosed {
		a.navMu.Unlock()
		return nil
	}
	id := a.currentSessionID()
	if id == "" {
		a.navMu.Unlock()
		return nil
	}
	if _, err := a.svc.SessionSummaryForSessionOrPersisted(id); err != nil {
		a.setCurrentSessionID("")
		a.navMu.Unlock()
		return nil
	}
	a.navMu.Unlock()
	msgs, err := a.svc.SessionMessagesFor(id)
	if err != nil {
		return nil
	}
	return msgs
}

// CurrentWarnings returns the backend-owned warning snapshot.
func (a *App) CurrentWarnings() []agent.PromptWarning {
	return a.svc.CurrentWarnings()
}

// AppVersion returns the binary's build identity for the About row.
func (a *App) AppVersion() string {
	return version.String()
}

// Submit is the single entry point for user input: it starts a turn when idle
// or queues the input in the backend, returning whether it started and a
// versioned queue snapshot.
func (a *App) Submit(content string) (agent.SubmitResult, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return agent.SubmitResult{}, err
	}
	result, err := a.svc.SubmitToSession(a.ctx, sessionID, content)
	if err != nil && a.routeReadOnlyNames(sessionID) {
		// The session is read-only: another process drives it, so the owner
		// refuses it as unknown. Say what is actually wrong.
		return agent.SubmitResult{}, agent.SessionContendedError(sessionID)
	}
	return result, err
}

// QueueSnapshot returns the backend's versioned input-queue snapshot for
// frontend hydration (register the queue_changed listener before calling).
func (a *App) QueueSnapshot() agent.QueueState {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return agent.QueueState{}
	}
	q, err := a.svc.QueueSnapshotForSession(sessionID)
	if err != nil {
		return agent.QueueState{}
	}
	return q
}

// SwitchModel changes the active model by provider-prefixed catalog ref.
func (a *App) SwitchModel(ref string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	if a.agent == nil {
		return a.svc.SwitchModelForSession(sessionID, ref)
	}
	// The owner returns the committed model from the same mutation; append it as an
	// ordered item tagged with the root it switched, so the selector updates only
	// while that root is presentation-current — never out of band or on another root.
	model, err := a.agent.SwitchModelForSessionInfo(sessionID, ref)
	if err != nil {
		return err
	}
	a.emitFrame("model", modelItem{RootID: sessionID, Model: model})
	return nil
}

// modelItem is the ordered frame a root-model switch appends: the committed model
// info tagged with the root it switched.
type modelItem struct {
	RootID string          `json:"rootId"`
	Model  agent.ModelInfo `json:"model"`
}

// Reload reloads config and catalog state for future turns.
func (a *App) Reload() error {
	return a.svc.Reload()
}

// CompleteModelEntry writes missing model metadata and reloads the catalog.
func (a *App) CompleteModelEntry(ref string, completion ModelCompletion) error {
	return a.svc.CompleteModelEntry(ref, completion)
}

// ProviderList returns provider setup status for Settings.
func (a *App) ProviderList() []agent.ProviderStatus {
	return a.svc.ProviderList()
}

// ConnectProvider connects an existing provider with an optional API key.
func (a *App) ConnectProvider(providerID string, apiKey string) error {
	return a.svc.ConnectProvider(providerID, apiKey)
}

// DiscoverCustomProvider runs one-shot discovery for an unsaved custom provider.
func (a *App) DiscoverCustomProvider(req agent.CustomProviderRequest) ([]agent.DiscoveryModelCandidate, error) {
	return a.svc.DiscoverCustomProvider(req)
}

// AddCustomProvider persists a custom provider and selected models.
func (a *App) AddCustomProvider(req agent.CustomProviderRequest) error {
	return a.svc.AddCustomProvider(req)
}

// DisconnectProvider removes a Lightcode-managed provider key.
func (a *App) DisconnectProvider(providerID string) error {
	return a.svc.DisconnectProvider(providerID)
}

// RemoveProvider deletes a custom provider entry.
func (a *App) RemoveProvider(providerID string) error {
	return a.svc.RemoveProvider(providerID)
}

// GenerateAPIKeyEnvName returns a unique env var name for a provider id.
func (a *App) GenerateAPIKeyEnvName(providerID string) string {
	return a.svc.GenerateAPIKeyEnvName(providerID)
}

// GetProviderConfig returns the merged effective config of a provider for editing.
func (a *App) GetProviderConfig(providerID string) (agent.ProviderConfigView, error) {
	return a.svc.GetProviderConfig(providerID)
}

// DiscoverableModels returns the provider's discovered-but-not-included models.
func (a *App) DiscoverableModels(providerID string) ([]agent.DiscoveryModelCandidate, error) {
	return a.svc.DiscoverableModels(providerID)
}

// SetProviderConfig edits an existing provider's transport/provider-level config.
func (a *App) SetProviderConfig(providerID string, cfg agent.ProviderConfigInput) error {
	return a.svc.SetProviderConfig(providerID, cfg)
}

// ResetProviderField reverts one provider config field to the bundled default.
func (a *App) ResetProviderField(providerID string, field string) error {
	return a.svc.ResetProviderField(providerID, field)
}

// SaveModel adds or edits one model's config fields under a provider.
func (a *App) SaveModel(providerID string, modelID string, cfg agent.ModelConfigInput) error {
	return a.svc.SaveModel(providerID, modelID, cfg)
}

// DeleteModel removes a user-added model from config.
func (a *App) DeleteModel(providerID string, modelID string) error {
	return a.svc.DeleteModel(providerID, modelID)
}

// ResetModelField reverts one model config field to the bundled/discovery default.
func (a *App) ResetModelField(providerID string, modelID string, field string) error {
	return a.svc.ResetModelField(providerID, modelID, field)
}

// RevertCode restores files through the direct code-revert convention: N is
// the first restored turn, so the store target is N-1, matching the shared
// revert_code turn action.
func (a *App) RevertCode(turn int) (snapshot.RevertResult, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return snapshot.RevertResult{}, err
	}
	return a.svc.RevertCodeForSession(sessionID, turn)
}

// turnActionBoundaryEmit is the owner's in-commit callback for a session-changing
// revert/fork: it commits routing current, then appends the destination's complete
// state, any code-revert skip notice, and a fork's failed-code-revert warning as one
// ordered boundary, so state and notices apply together and no live frame
// interleaves between them.
func (a *App) turnActionBoundaryEmit() func(agent.HydrationState, []snapshot.SkippedRevert, string) {
	return func(state agent.HydrationState, skipped []snapshot.SkippedRevert, warning string) {
		a.setCurrentSessionID(state.Session.ID)
		a.enqueueBoundary("turn_action", turnActionBoundary{State: &state, SkippedFiles: skipped, Warning: warning}, "", state.Session.ID)
	}
}

// applyTurnActionWithOwnedBoundary runs a turn action through the owner's
// in-commit boundary callback and records synchronously whether the callback
// emitted. A postcommit partial error — a reconciled history revert whose walk
// failed after the boundary published the survivors — then resolves the method
// as success: the ordered turn_action frame owns the error through its warning,
// and a second, unowned direct error would duplicate it in the frontend. A
// precommit error emits no frame and still rejects/returns; the typed return
// error is kept for the ACP/CLI disposition.
func (a *App) applyTurnActionWithOwnedBoundary(sessionID string, turn int, action string, alsoRevertCode bool) (agent.TurnActionResult, error) {
	var emitted bool
	result, err := a.svc.ApplyTurnActionForSessionWithBoundary(sessionID, turn, action, alsoRevertCode, func(state agent.HydrationState, skipped []snapshot.SkippedRevert, warning string) {
		emitted = true
		a.setCurrentSessionID(state.Session.ID)
		a.enqueueBoundary("turn_action", turnActionBoundary{State: &state, SkippedFiles: skipped, Warning: warning}, "", state.Session.ID)
	})
	if err != nil && emitted {
		return result, nil
	}
	return result, err
}

// RevertHistory truncates conversation above the given turn. It is the bound
// alias of the turn-action route: the given turn is the first one removed, so
// turns up to turn-1 survive, matching ApplyTurnAction's revert_history.
func (a *App) RevertHistory(turn int) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	_, err = a.applyTurnActionWithOwnedBoundary(sessionID, turn, agent.TurnActionRevertHistory, false)
	return err
}

// ForkSession creates a new session branched from turn N.
func (a *App) ForkSession(turn int) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	_, err = a.svc.ApplyTurnActionForSessionWithBoundary(sessionID, turn, agent.TurnActionFork, false, a.turnActionBoundaryEmit())
	return err
}

// ApplyTurnAction applies a user-message revert/fork action.
func (a *App) ApplyTurnAction(turn int, action string, alsoRevertCode bool) (agent.TurnActionResult, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return agent.TurnActionResult{}, err
	}
	result, err := a.applyTurnActionWithOwnedBoundary(sessionID, turn, action, alsoRevertCode)
	if err != nil {
		return result, err
	}
	if !result.SessionChanged {
		// A code-only revert changes no session, so the owner emits no boundary;
		// deliver its skip notice through the same ordered FIFO so a queued refresh
		// cannot clobber it.
		a.emitTurnActionNotice(result.SkippedFiles)
	}
	return result, nil
}

// RespondPermission answers a pending permission prompt.
func (a *App) RespondPermission(sessionID string, id string, action string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	sessionID, err := a.resolveSessionID(sessionID)
	if err != nil {
		return err
	}
	err = a.svc.RespondPermissionActionForSession(sessionID, id, action)
	if errors.Is(err, permission.ErrUnknownRequest) {
		// The bound methods are reachable only by the bundled frontend, which
		// never constructs an id — every id it answers came from a request event
		// or a gate-captured snapshot. An unknown-request outcome can therefore
		// only mean the prompt was resolved underneath the user. Benign.
		return nil
	}
	return err
}

// PermissionSuggest returns pattern suggestions for the "Allow for project" UI.
func (a *App) PermissionSuggest(sessionID string, projectID string, toolName string, arg string) []permission.Suggestion {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return nil
	}
	var (
		suggestions []permission.Suggestion
		err         error
	)
	if strings.TrimSpace(projectID) != "" {
		suggestions, err = a.svc.PermissionSuggestForProject(projectID, toolName, arg)
	} else {
		sessionID, err = a.resolveSessionID(sessionID)
		if err == nil {
			suggestions, err = a.svc.PermissionSuggestForSession(sessionID, toolName, arg)
		}
	}
	if err != nil {
		return nil
	}
	return suggestions
}

// SaveProjectPermission appends patterns to project permissions and allows the request.
func (a *App) SaveProjectPermission(sessionID string, id string, patterns []string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	sessionID, err := a.resolveSessionID(sessionID)
	if err != nil {
		return err
	}
	err = a.svc.SaveProjectPermissionForSession(sessionID, id, patterns)
	if errors.Is(err, permission.ErrUnknownRequest) {
		// Same soundness as RespondPermission: the id can only have come from a
		// prompt this host was given, so an unknown-request outcome means it was
		// resolved underneath the user. Benign.
		return nil
	}
	return err
}

// CompactNow triggers manual context compaction.
func (a *App) CompactNow() error {
	a.navMu.Lock()
	if a.navClosed {
		a.navMu.Unlock()
		return errAdapterClosed
	}
	// Capture the id under navMu, then release it before the long owner call so it
	// stays free for Cancel/Close.
	sessionID, err := a.liveCurrentSessionOrErr()
	a.navMu.Unlock()
	if err != nil {
		return err
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.svc.CompactNowForSession(ctx, sessionID); err != nil {
		return err
	}
	return nil
}

// Cancel aborts the current agentic loop iteration.
func (a *App) Cancel() error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	return a.svc.CancelSession(sessionID)
}

// SnapshotList returns the timeline of all snapshots in the session.
func (a *App) SnapshotList() ([]agent.Snapshot, error) {
	a.navMu.Lock()
	if a.navClosed {
		a.navMu.Unlock()
		return nil, errAdapterClosed
	}
	sessionID, err := a.liveCurrentSessionOrErr()
	a.navMu.Unlock()
	if err != nil {
		return nil, err
	}
	return a.svc.SnapshotListForSession(sessionID)
}

// ModelList returns all visible catalog models.
func (a *App) ModelList() ([]agent.ModelListEntry, error) {
	return a.svc.ModelList(), nil
}

// CurrentModel returns the active provider and model.
func (a *App) CurrentModel() agent.ModelInfo {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return agent.ModelInfo{}
	}
	model, err := a.svc.CurrentModelForSession(sessionID)
	if err != nil {
		return agent.ModelInfo{}
	}
	return model
}

// AllModelList returns every catalog model including hidden ones.
func (a *App) AllModelList() ([]agent.ModelListEntry, error) {
	return a.svc.AllModelList(), nil
}

// SetModelHidden writes the hidden flag for a model to config and reloads.
func (a *App) SetModelHidden(ref string, hidden bool) error {
	return a.svc.SetModelHidden(ref, hidden)
}

// SetProviderHidden writes the hidden flag for a provider to config and reloads.
func (a *App) SetProviderHidden(providerID string, hidden bool) error {
	return a.svc.SetProviderHidden(providerID, hidden)
}

// SetDefaultModel writes the persisted primary model to agent type config.
func (a *App) SetDefaultModel(ref string) error {
	return a.svc.SetDefaultModel(ref)
}

// GetRuntimeConfig returns GUI-editable runtime settings.
func (a *App) GetRuntimeConfig() agent.RuntimeConfigSettings {
	return a.svc.GetRuntimeConfig()
}

// SetRuntimeConfig writes GUI-editable runtime settings to config.
func (a *App) SetRuntimeConfig(settings agent.RuntimeConfigSettings) error {
	return a.svc.SetRuntimeConfig(settings)
}

// ProjectName returns the basename of the adapter-local project directory.
func (a *App) ProjectName() string {
	return filepath.Base(a.routeProjectPathCaptured())
}

// ReadFileContent loads a file's contents for the in-app viewer.
func (a *App) ReadFileContent(path string) (string, error) {
	root, err := a.routeProjectPathBounded()
	if err != nil {
		return "", err
	}
	return a.svc.ReadFileContentForProjectPath(root, path)
}

// routeProjectPathCaptured reads the routing-current project path under navMu and
// releases it, so the value is a consistent snapshot linearized against a project
// switch without holding the gate across the read that follows.
func (a *App) routeProjectPathCaptured() string {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	return a.routeProjectPath
}

// routeProjectPathBounded is routeProjectPathCaptured for a route that must reject
// after the adapter is closed.
func (a *App) routeProjectPathBounded() (string, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return "", errAdapterClosed
	}
	return a.routeProjectPath, nil
}

// setRouteProjectPathLocked commits the routing-current project path. The caller
// holds navMu. The owner always resolves a selectable session against its
// project, so the boundary summary carries a project path; the commit is
// unconditional.
func (a *App) setRouteProjectPathLocked(path string) {
	a.routeProjectPath = path
}

// openOrCreateSession opens the most recent active session for a project path, or
// creates one. The caller holds navMu across it as part of a navigation.
func (a *App) openOrCreateSession(projectPath string, emit func(agent.HydrationState)) (agent.SessionSummary, error) {
	sessions, err := a.svc.SessionListForProjectPath(projectPath, "active")
	if err != nil {
		return agent.SessionSummary{}, err
	}
	// Scan newest-first and skip a candidate another process is driving, so a
	// contended session does not fail the whole navigation. Any other open
	// failure surfaces: this is user-initiated, so a corrupt session must not
	// be skipped silently.
	for _, s := range sessions {
		summary, err := a.svc.OpenSessionWithBoundary(s.ID, emit)
		if err == nil {
			return summary, nil
		}
		if !errors.Is(err, snapshot.ErrSessionContended) {
			return agent.SessionSummary{}, err
		}
	}
	id, err := a.svc.NewSessionForProjectPathWithBoundary(projectPath, "primary", emit)
	if err != nil {
		return agent.SessionSummary{}, err
	}
	return agent.SessionSummary{ID: id}, nil
}

// TokenUsage returns the current cumulative token usage for the session.
func (a *App) TokenUsage() agent.TokenReport {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return agent.TokenReport{}
	}
	return a.tokenUsage()
}

// SessionCurrent returns the active session.
func (a *App) SessionCurrent() agent.SessionSummary {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return agent.SessionSummary{}
	}
	return a.currentSessionSummary()
}

// SessionList returns sessions filtered by state.
func (a *App) SessionList(state string) ([]agent.SessionSummary, error) {
	root, err := a.routeProjectPathBounded()
	if err != nil {
		return nil, err
	}
	return a.svc.SessionListForProjectPath(root, state)
}

// SessionSwitch switches to another session.
func (a *App) SessionSwitch(id string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	// The owner publishes the destination boundary in-commit; the callback commits
	// adapter routing current — including the destination project, since the id may
	// resolve in another project — before appending the boundary, both while this
	// goroutine holds navMu, so a failed switch leaves routing and presentation
	// unchanged. The native window title is computed from the destination's project
	// path, exactly as a project switch does, so a cross-project switch moves the
	// window title with the boundary.
	_, err := a.svc.OpenSessionWithBoundary(id, func(state agent.HydrationState) {
		a.setRouteProjectPathLocked(state.Session.ProjectPath)
		a.setCurrentSessionID(state.Session.ID)
		a.enqueueBoundary("navigation", state, "Lightcode — "+filepath.Base(state.Session.ProjectPath), state.Session.ID)
	})
	if err != nil {
		if !errors.Is(err, snapshot.ErrSessionContended) {
			return err
		}
		// The user named a session another process is driving: the open
		// succeeds as a read-only one. Present its durable transcript through
		// the same navigation frame, committing routing current only once the
		// view is available, so a failed presentation leaves routing unchanged.
		state, herr := a.HydrateSession(id)
		if herr != nil {
			return herr
		}
		a.setRouteProjectPathLocked(state.Session.ProjectPath)
		a.setCurrentSessionID(state.Session.ID)
		a.markRouteReadOnly(state.Session.ID)
		a.enqueueBoundary("navigation", state, "Lightcode — "+filepath.Base(state.Session.ProjectPath), state.Session.ID)
		return nil
	}
	return nil
}

// SessionArchive archives a session.
func (a *App) SessionArchive(id string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	// Durable archive then the deterministic detach, together under navMu so a
	// concurrent switch to the same session cannot interleave between them.
	if err := a.svc.SessionArchive(id); err != nil {
		return err
	}
	// Removing the current session performs no complete-state capture of the removed session: current
	// removal appends a deterministic no-session boundary the frontend applies as a
	// detach; non-current removal leaves selection/presentation unchanged.
	if a.removedCurrent(id) {
		a.enqueueBoundary("navigation", agent.HydrationState{}, "", "")
	}
	return nil
}

// SessionDelete removes a session from disk.
func (a *App) SessionDelete(id string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	if err := a.svc.SessionDelete(id); err != nil {
		return err
	}
	// Current removal: deterministic no-session boundary, no capture.
	if a.removedCurrent(id) {
		a.enqueueBoundary("navigation", agent.HydrationState{}, "", "")
	}
	return nil
}

// SessionNew starts a fresh session in the adapter-local project.
func (a *App) SessionNew() error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	_, err := a.svc.NewSessionForProjectPathWithBoundary(a.routeProjectPath, "primary", func(state agent.HydrationState) {
		a.setCurrentSessionID(state.Session.ID)
		a.enqueueBoundary("navigation", state, "", state.Session.ID)
	})
	return err
}

// SessionMessages returns persisted history for the current session.
func (a *App) SessionMessages() []agent.DisplayMessage {
	return a.sessionMessages()
}

// SessionMessagesFor returns persisted history for a session without switching.
func (a *App) SessionMessagesFor(id string) ([]agent.DisplayMessage, error) {
	return a.svc.SessionMessagesFor(id)
}

// ProjectList returns every known project sorted by last activity.
func (a *App) ProjectList() ([]agent.ProjectSummary, error) {
	return a.svc.ProjectList()
}

// ProjectCurrent returns the project record for the adapter-local project.
func (a *App) ProjectCurrent() agent.ProjectSummary {
	out, _ := a.svc.ProjectCurrentForPath(a.routeProjectPathCaptured())
	return out
}

// ProjectSwitch navigates to a different project in-place over the existing
// owner connection — no process spawn, no detach, no lease gap.
func (a *App) ProjectSwitch(targetPath string) error {
	if targetPath == "" {
		return fmt.Errorf("empty target path")
	}
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}

	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	if abs == a.routeProjectPath {
		return nil
	}
	// Commit the project path, session id, title, and boundary together in-commit under
	// navMu so a following current-target call never sees one without the other. The
	// native title rides the boundary, so it changes only when the boundary is consumed.
	title := "Lightcode — " + filepath.Base(abs)
	_, err = a.openOrCreateSession(abs, func(state agent.HydrationState) {
		a.setRouteProjectPathLocked(abs)
		a.setCurrentSessionID(state.Session.ID)
		a.enqueueBoundary("navigation", state, title, state.Session.ID)
	})
	return err
}

// ProjectPickAndSwitch opens a native directory picker.
func (a *App) ProjectPickAndSwitch() error {
	selected, err := wailsRuntime.OpenDirectoryDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "Open project",
	})
	if err != nil {
		return err
	}
	if selected == "" {
		return nil
	}
	return a.ProjectSwitch(selected)
}
