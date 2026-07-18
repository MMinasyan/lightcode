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

// deliveryFrame is one immutable item the delivery FIFO carries. Today every
// frame is a legacy named event with its payload; the sole drainer emits it.
type deliveryFrame struct {
	name    string
	payload any
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

	scope           *agent.AdapterScope
	viewOnce        sync.Once
	view            *agent.SessionView
	adapterAttached bool

	// Delivery spine: every event and navigation payload is appended as a frame;
	// the single drainer is the only goroutine that emits to the frontend, so
	// delivery order is the drainer's write order. emitFn is the one emission
	// choke point (overridable in tests).
	emitFn         func(name string, payload any)
	deliveryMu     sync.Mutex
	deliveryFrames []deliveryFrame
	deliveryWake   chan struct{}
	deliveryClosed bool
	deliveryDone   chan struct{}
	deliveryOnce   sync.Once
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
	// The owner's host-context teardown (LSP shutdown) keys on the init context, so
	// initialize under a cancelable child that shutdown cancels. Delivery keeps using
	// the original ctx, which stays valid until the drainer is closed.
	hostCtx, cancel := context.WithCancel(ctx)
	a.hostCancel = cancel
	a.startDelivery()
	a.svc.SetEventHandler(a.handleEvent)
	a.svc.Init(hostCtx)
	a.scope = agent.NewAdapterScope(a.svc, a.svc.ProjectRoot())
	if lifecycle, ok := a.svc.(interface{ AttachAdapter(context.Context) error }); ok {
		a.adapterAttached = lifecycle.AttachAdapter(hostCtx) == nil
	}
	sessionID := ""
	if sessions, err := a.scope.SessionList("active"); err == nil && len(sessions) > 0 {
		if summary, err := a.svc.OpenSession(sessions[0].ID); err == nil {
			sessionID = summary.ID
		}
	}
	if sessionID == "" {
		if id, err := a.scope.NewSession("primary"); err == nil {
			sessionID = id
		}
	}
	a.navMu.Lock()
	a.setCurrentSessionID(sessionID)
	a.navMu.Unlock()
	a.started = true
}

func (a *App) shutdown(ctx context.Context) {
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
	if a.adapterAttached {
		if lifecycle, ok := a.svc.(interface{ DetachAdapter(context.Context) error }); ok {
			_ = lifecycle.DetachAdapter(ctx)
		}
	}
	if !started {
		return
	}
	// Join the owner's turns and workers first: ShutdownOwner drains in-flight turns
	// while the internal event drainer is still alive, then stops the workers. Only
	// then cancel the host context, whose sole watcher is the LSP teardown goroutine
	// (ShutdownOwner never cancels it, since it keys on the init context).
	if a.agent != nil {
		a.agent.ShutdownOwner()
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
	a.deliveryWake = make(chan struct{}, 1)
	a.deliveryDone = make(chan struct{})
	go a.runDeliveryDrainer()
}

// emitFrame appends one frame and wakes the drainer. It never blocks or emits,
// so it is safe to call from an event callback that runs under an owner lock.
func (a *App) emitFrame(name string, payload any) {
	a.deliveryMu.Lock()
	if a.deliveryClosed {
		a.deliveryMu.Unlock()
		return
	}
	a.deliveryFrames = append(a.deliveryFrames, deliveryFrame{name: name, payload: payload})
	a.deliveryMu.Unlock()
	select {
	case a.deliveryWake <- struct{}{}:
	default:
	}
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
			// frontend after shutdown has proceeded.
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
		a.deliveryMu.Unlock()
		a.emitFn(frame.name, frame.payload)
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

func (a *App) sv() *agent.SessionView {
	a.viewOnce.Do(func() { a.view = agent.NewSessionView(a.svc) })
	return a.view
}

func (a *App) handleEvent(ev agent.Event) {
	if !a.acceptsEvent(ev) {
		return
	}
	if ev.SubagentSessionID != "" {
		switch ev.Kind {
		case agent.EventTextDelta:
			a.emitFrame("subagent_token", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"content":   ev.Result,
			})
		case agent.EventToolCallStart:
			a.emitFrame("subagent_tool_start", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
			})
		case agent.EventToolCallEnd:
			a.emitFrame("subagent_tool_result", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
				"success":   !ev.IsError,
				"output":    ev.Result,
				"metadata":  ev.Metadata,
			})
		case agent.EventSubagentStart:
			a.emitFrame("subagent_session_start", map[string]any{
				"sessionId":      ev.SubagentSessionID,
				"taskToolCallId": ev.ToolCallID,
				"taskIndex":      ev.TaskIndex,
			})
		case agent.EventBackgroundProcessComplete:
			if ev.BackgroundProcess != nil {
				a.emitFrame("subagent_background_process_complete", map[string]any{
					"sessionId": ev.SubagentSessionID,
					"id":        ev.BackgroundProcess.ID,
					"command":   ev.BackgroundProcess.Command,
					"reason":    ev.BackgroundProcess.Reason,
					"exitCode":  ev.BackgroundProcess.ExitCode,
					"success":   !ev.IsError,
					"output":    ev.Result,
				})
			}
		}
		return
	}
	switch ev.Kind {
	case agent.EventTextDelta:
		a.emitFrame("token", map[string]any{
			"seq":     ev.Seq,
			"content": ev.Result,
		})
	case agent.EventToolCallStart:
		a.emitFrame("tool_start", map[string]any{
			"seq":  ev.Seq,
			"id":   ev.ToolCallID,
			"name": ev.ToolName,
			"args": ev.Args,
		})
	case agent.EventToolCallEnd:
		a.emitFrame("tool_result", map[string]any{
			"id":       ev.ToolCallID,
			"name":     ev.ToolName,
			"args":     ev.Args,
			"success":  !ev.IsError,
			"output":   ev.Result,
			"metadata": ev.Metadata,
		})
	case agent.EventBackgroundProcessComplete:
		if ev.BackgroundProcess != nil {
			a.emitFrame("background_process_complete", map[string]any{
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
		a.emitFrame("user_message", map[string]any{
			"seq":     ev.Seq,
			"turn":    ev.Turn,
			"content": ev.Result,
		})
	case agent.EventGenericSystemSignal:
		a.emitFrame("system_signal", map[string]any{
			"seq":     ev.Seq,
			"content": "System: " + ev.Result,
		})
	case agent.EventQueueChanged:
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		a.emitFrame("queue_changed", map[string]any{
			"items":   queue,
			"version": ev.QueueVersion,
		})
	case agent.EventUsage:
		a.emitFrame("usage", a.tokenUsage())
	case agent.EventTurnStart:
		a.emitFrame("turn_start", map[string]any{"turn": ev.Turn})
		a.emitFrame("status", map[string]any{"state": "streaming"})
	case agent.EventTurnEnd:
		a.emitFrame("turn_end", map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled})
		a.emitFrame("status", map[string]any{"state": "idle"})
		if ev.RefreshSession {
			a.emitSessionChangedForEvent(ev)
		}
	case agent.EventError:
		a.emitFrame("error", map[string]any{"seq": ev.Seq, "message": ev.Error})
	case agent.EventPermissionRequest:
		a.emitFrame("permission_request", map[string]any{
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
	case agent.EventCompactionStart:
		a.emitFrame("compaction_start", nil)
	case agent.EventCompactionEnd:
		a.emitFrame("compaction_end", nil)
		if ev.RefreshSession {
			a.emitSessionChangedForEvent(ev)
		}
	case agent.EventWarning:
		a.emitFrame("warnings", ev.Warnings)
	}
}

// emitSessionChanged tells the frontend to replace its message list.
func (a *App) emitSessionChanged() {
	a.emitSessionChangedForSession(a.currentSessionID())
}

func (a *App) emitSessionChangedForEvent(ev agent.Event) {
	if strings.TrimSpace(ev.SessionID) != "" {
		a.emitSessionChangedForSession(ev.SessionID)
		return
	}
	a.emitSessionChanged()
}

func (a *App) emitSessionChangedForSession(sessionID string) {
	if a.ctx == nil {
		return
	}
	payload := a.sv().SessionChangedPayload(sessionID)
	a.emitFrame("session_changed", payload)
}

// emitNavigationBoundary captures the destination session's complete state and
// appends it as the navigation boundary the frontend applies as its whole
// replacement view. An empty or unresolved id yields the zero state, which the
// frontend applies as a detach. The caller holds navMu, so navMu -> the owner's
// HydrateSession locks stays in the documented order.
func (a *App) emitNavigationBoundary(sessionID string) {
	if a.ctx == nil {
		return
	}
	emit := func(state agent.HydrationState) { a.emitFrame("navigation", state) }
	if a.agent == nil {
		emit(agent.HydrationState{})
		return
	}
	// Append the boundary while the capture locks are held so no event delivered
	// after the capture can be enqueued before it.
	a.agent.HydrateSessionWithBoundary(sessionID, emit)
}

// turnActionBoundary is the ordered frame a fork, history revert, or code revert
// appends through the delivery FIFO: the destination session's complete state (nil
// when the action changed no session) plus any files a code revert kept unchanged.
// The ordered consumer applies the state and the skip notice together, so no live
// frame interleaves between them or clobbers the notice.
type turnActionBoundary struct {
	State        *agent.HydrationState    `json:"state"`
	SkippedFiles []snapshot.SkippedRevert `json:"skippedFiles"`
}

// emitTurnActionBoundary appends the destination's complete state and code-revert
// skip notice as one ordered frame. It captures atomically like the navigation
// boundary, so a live frame delivered after the capture is enqueued after it and
// the skip notice cannot be clobbered by an out-of-band apply.
func (a *App) emitTurnActionBoundary(sessionID string, skipped []snapshot.SkippedRevert) {
	if a.ctx == nil {
		return
	}
	emit := func(state agent.HydrationState) {
		a.emitFrame("turn_action", turnActionBoundary{State: &state, SkippedFiles: skipped})
	}
	if a.agent == nil {
		emit(agent.HydrationState{})
		return
	}
	a.agent.HydrateSessionWithBoundary(sessionID, emit)
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

func (a *App) setCurrentSessionID(id string) {
	a.sv().SetCurrent(id)
}

func (a *App) currentSessionID() string {
	return a.sv().Current()
}

func (a *App) currentSession() (string, error) {
	return a.sv().CurrentOrErr()
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

func (a *App) acceptsSessionEvent(sessionID string) bool {
	return a.sv().AcceptsSessionEvent(sessionID)
}

func (a *App) acceptsEvent(ev agent.Event) bool {
	return a.sv().AcceptsEvent(ev)
}

func (a *App) acceptsSubagentEvent(ev agent.Event) bool {
	return a.sv().AcceptsSubagentEvent(ev)
}

func (a *App) acceptsSubagentEventForCurrent(current string, ev agent.Event) bool {
	return a.sv().AcceptsSubagentEventForCurrent(current, ev)
}

func (a *App) liveCurrentSessionID() string {
	return a.sv().LiveCurrent()
}

func (a *App) resolveSessionID(id string) (string, error) {
	return a.sv().Resolve(id)
}

func (a *App) currentSessionSummary() agent.SessionSummary {
	return a.sv().CurrentSummary()
}

func (a *App) tokenUsage() agent.TokenReport {
	return a.sv().TokenUsage()
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
	if _, err := a.svc.SessionSummaryForSession(id); err != nil {
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
	return a.svc.SubmitToSession(a.ctx, sessionID, content)
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
	return a.svc.SwitchModelForSession(sessionID, ref)
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

// RevertCode restores files to their state at turn N.
func (a *App) RevertCode(turn int) (snapshot.RevertResult, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return snapshot.RevertResult{}, err
	}
	return a.svc.RevertCodeForSession(sessionID, turn)
}

// RevertHistory truncates conversation after turn N.
func (a *App) RevertHistory(turn int) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	if err := a.svc.RevertHistoryForSession(sessionID, turn); err != nil {
		return err
	}
	a.emitSessionChangedForSession(sessionID)
	return nil
}

// ForkSession creates a new session branched from turn N.
func (a *App) ForkSession(turn int) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return err
	}
	result, err := a.svc.ApplyTurnActionForSession(sessionID, turn, agent.TurnActionFork, false)
	if err != nil {
		return err
	}
	if result.Session.ID != "" {
		a.setCurrentSessionID(result.Session.ID)
	}
	a.emitSessionChangedForSession(result.Session.ID)
	return nil
}

// ApplyTurnAction applies a user-message revert/fork action.
func (a *App) ApplyTurnAction(turn int, action string, alsoRevertCode bool) (agent.TurnActionResult, error) {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	sessionID, err := a.boundedSessionIDLocked()
	if err != nil {
		return agent.TurnActionResult{}, err
	}
	result, err := a.svc.ApplyTurnActionForSession(sessionID, turn, action, alsoRevertCode)
	if err != nil {
		return result, err
	}
	if result.SessionChanged && result.Session.ID != "" {
		// A fork navigates to the new branched session; a history revert stays on
		// the reverted one. Commit routing, then append the destination's complete
		// state and any code-revert skip notice as one ordered boundary, so the
		// state and notice apply together and no live frame interleaves between them.
		a.setCurrentSessionID(result.Session.ID)
		a.emitTurnActionBoundary(result.Session.ID, result.SkippedFiles)
	} else {
		// A code-only revert changes no session; deliver its skip notice through the
		// same ordered FIFO so a queued refresh cannot clobber it.
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
	return a.svc.RespondPermissionActionForSession(sessionID, id, action)
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
	return a.svc.SaveProjectPermissionForSession(sessionID, id, patterns)
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
	sessionID := a.liveCurrentSessionID()
	a.navMu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("no current session")
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
	sessionID := a.liveCurrentSessionID()
	a.navMu.Unlock()
	if sessionID == "" {
		return nil, fmt.Errorf("no current session")
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
	return a.scope.ProjectName()
}

// ReadFileContent loads a file's contents for the in-app viewer.
func (a *App) ReadFileContent(path string) (string, error) {
	return a.scope.ReadFileContent(path)
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
	return a.scope.SessionList(state)
}

// SessionSwitch switches to another session.
func (a *App) SessionSwitch(id string) error {
	a.navMu.Lock()
	defer a.navMu.Unlock()
	if a.navClosed {
		return errAdapterClosed
	}
	summary, err := a.svc.OpenSession(id)
	if err != nil {
		return err
	}
	a.setCurrentSessionID(summary.ID)
	a.emitNavigationBoundary(summary.ID)
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
	if a.sv().RemovedCurrent(id) {
		a.emitNavigationBoundary(strings.TrimSpace(id))
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
	if a.sv().RemovedCurrent(id) {
		a.emitNavigationBoundary(strings.TrimSpace(id))
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
	id, err := a.scope.NewSession("primary")
	if err != nil {
		return err
	}
	a.setCurrentSessionID(id)
	a.emitNavigationBoundary(id)
	return nil
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
	return a.scope.ProjectCurrent()
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
	if abs == a.scope.ProjectRoot() {
		return nil
	}
	summary, err := a.scope.OpenOrCreateSession(abs)
	if err != nil {
		return err
	}
	// Commit the project path and the session id together under navMu so a
	// following current-target call never sees one without the other.
	a.scope.SetProjectPath(abs)
	a.setCurrentSessionID(summary.ID)
	a.emitNavigationBoundary(summary.ID)
	if a.ctx != nil {
		wailsRuntime.WindowSetTitle(a.ctx, "Lightcode — "+a.scope.ProjectName())
	}
	return nil
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
