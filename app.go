package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/version"
)

// App is the Wails-bound struct that bridges the Go backend to the
// frontend. All exported methods are callable from JavaScript.
type App struct {
	ctx context.Context
	svc *agent.Agent
}

type ModelCompletion = agent.ModelCompletion

// startup is called by Wails after the window is created.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.svc.SetEventHandler(a.handleEvent)
	a.svc.Init(ctx)
}

func (a *App) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		switch ev.Kind {
		case agent.EventTextDelta:
			wailsRuntime.EventsEmit(a.ctx, "subagent_token", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"content":   ev.Result,
			})
		case agent.EventToolCallStart:
			wailsRuntime.EventsEmit(a.ctx, "subagent_tool_start", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
			})
		case agent.EventToolCallEnd:
			wailsRuntime.EventsEmit(a.ctx, "subagent_tool_result", map[string]any{
				"sessionId": ev.SubagentSessionID,
				"id":        ev.ToolCallID,
				"name":      ev.ToolName,
				"args":      ev.Args,
				"success":   !ev.IsError,
				"output":    ev.Result,
				"metadata":  ev.Metadata,
			})
		case agent.EventSubagentStart:
			wailsRuntime.EventsEmit(a.ctx, "subagent_session_start", map[string]any{
				"sessionId":      ev.SubagentSessionID,
				"taskToolCallId": ev.ToolCallID,
				"taskIndex":      ev.TaskIndex,
			})
		case agent.EventBackgroundProcessComplete:
			if ev.BackgroundProcess != nil {
				wailsRuntime.EventsEmit(a.ctx, "subagent_background_process_complete", map[string]any{
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
		wailsRuntime.EventsEmit(a.ctx, "token", map[string]any{
			"content": ev.Result,
		})
	case agent.EventToolCallStart:
		wailsRuntime.EventsEmit(a.ctx, "tool_start", map[string]any{
			"id":   ev.ToolCallID,
			"name": ev.ToolName,
			"args": ev.Args,
		})
	case agent.EventToolCallEnd:
		wailsRuntime.EventsEmit(a.ctx, "tool_result", map[string]any{
			"id":       ev.ToolCallID,
			"name":     ev.ToolName,
			"args":     ev.Args,
			"success":  !ev.IsError,
			"output":   ev.Result,
			"metadata": ev.Metadata,
		})
	case agent.EventBackgroundProcessComplete:
		if ev.BackgroundProcess != nil {
			wailsRuntime.EventsEmit(a.ctx, "background_process_complete", map[string]any{
				"id":       ev.BackgroundProcess.ID,
				"command":  ev.BackgroundProcess.Command,
				"reason":   ev.BackgroundProcess.Reason,
				"exitCode": ev.BackgroundProcess.ExitCode,
				"success":  !ev.IsError,
				"output":   ev.Result,
			})
		}
	case agent.EventUserMessageDisplay:
		wailsRuntime.EventsEmit(a.ctx, "user_message", map[string]any{
			"turn":    ev.Turn,
			"content": ev.Result,
		})
	case agent.EventGenericSystemSignal:
		wailsRuntime.EventsEmit(a.ctx, "system_signal", map[string]any{
			"content": "System: " + ev.Result,
		})
	case agent.EventQueueChanged:
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		wailsRuntime.EventsEmit(a.ctx, "queue_changed", map[string]any{
			"items":   queue,
			"version": ev.QueueVersion,
		})
	case agent.EventUsage:
		wailsRuntime.EventsEmit(a.ctx, "usage", a.svc.TokenUsage())
	case agent.EventTurnStart:
		wailsRuntime.EventsEmit(a.ctx, "turn_start", map[string]any{"turn": ev.Turn})
		wailsRuntime.EventsEmit(a.ctx, "status", map[string]any{"state": "streaming"})
	case agent.EventTurnEnd:
		wailsRuntime.EventsEmit(a.ctx, "turn_end", map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled})
		wailsRuntime.EventsEmit(a.ctx, "status", map[string]any{"state": "idle"})
		if ev.RefreshSession {
			a.emitSessionChanged()
		}
	case agent.EventError:
		wailsRuntime.EventsEmit(a.ctx, "error", map[string]any{"message": ev.Error})
	case agent.EventPermissionRequest:
		wailsRuntime.EventsEmit(a.ctx, "permission_request", map[string]any{
			"id":                 ev.PermReq.ID,
			"tool":               ev.PermReq.ToolName,
			"args":               ev.PermReq.Arg,
			"resolvedArg":        ev.PermReq.ResolvedArg,
			"canAllowAll":        ev.PermReq.CanAllowAll,
			"batchIndex":         ev.PermReq.BatchIndex,
			"batchTotal":         ev.PermReq.BatchTotal,
			"batchFiles":         ev.PermReq.BatchFiles,
			"batchResolvedFiles": ev.PermReq.BatchResolvedFiles,
		})
	case agent.EventCompactionStart:
		wailsRuntime.EventsEmit(a.ctx, "compaction_start", nil)
	case agent.EventCompactionEnd:
		wailsRuntime.EventsEmit(a.ctx, "compaction_end", nil)
		if ev.RefreshSession {
			a.emitSessionChanged()
		}
	case agent.EventWarning:
		wailsRuntime.EventsEmit(a.ctx, "warnings", ev.Warnings)
	}
}

// emitSessionChanged tells the frontend to replace its message list.
func (a *App) emitSessionChanged() {
	wailsRuntime.EventsEmit(a.ctx, "session_changed", map[string]any{
		"session":  a.svc.SessionCurrent(),
		"messages": a.svc.SessionMessages(),
		"tokens":   a.svc.TokenUsage(),
	})
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
	return a.svc.Submit(a.ctx, content)
}

// QueueSnapshot returns the backend's versioned input-queue snapshot for
// frontend hydration (register the queue_changed listener before calling).
func (a *App) QueueSnapshot() agent.QueueState {
	return a.svc.QueueSnapshot()
}

// SwitchModel changes the active model by provider-prefixed catalog ref.
func (a *App) SwitchModel(ref string) error {
	return a.svc.SwitchModel(ref)
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
func (a *App) RevertCode(turn int) error {
	return a.svc.RevertCode(turn)
}

// RevertHistory truncates conversation after turn N.
func (a *App) RevertHistory(turn int) error {
	if err := a.svc.RevertHistory(turn); err != nil {
		return err
	}
	a.emitSessionChanged()
	return nil
}

// ForkSession creates a new session branched from turn N.
func (a *App) ForkSession(turn int) error {
	if err := a.svc.ForkSession(turn); err != nil {
		return err
	}
	a.emitSessionChanged()
	return nil
}

// ApplyTurnAction applies a user-message revert/fork action.
func (a *App) ApplyTurnAction(turn int, action string, alsoRevertCode bool) (agent.TurnActionResult, error) {
	result, err := a.svc.ApplyTurnAction(turn, action, alsoRevertCode)
	if err != nil {
		return result, err
	}
	if result.SessionChanged {
		a.emitSessionChanged()
	}
	return result, nil
}

// RespondPermission answers a pending permission prompt.
func (a *App) RespondPermission(id string, action string) error {
	return a.svc.RespondPermissionAction(id, action)
}

// PermissionSuggest returns pattern suggestions for the "Allow for project" UI.
func (a *App) PermissionSuggest(toolName, arg string) []permission.Suggestion {
	return a.svc.PermissionSuggest(toolName, arg)
}

// SaveProjectPermission appends patterns to project permissions and allows the request.
func (a *App) SaveProjectPermission(id string, patterns []string) error {
	return a.svc.SaveProjectPermission(id, patterns)
}

// CompactNow triggers manual context compaction.
func (a *App) CompactNow() error {
	if err := a.svc.CompactNow(a.ctx); err != nil {
		return err
	}
	return nil
}

// Cancel aborts the current agentic loop iteration.
func (a *App) Cancel() error {
	return a.svc.Cancel()
}

// SnapshotList returns the timeline of all snapshots in the session.
func (a *App) SnapshotList() ([]agent.Snapshot, error) {
	return a.svc.SnapshotList()
}

// ModelList returns all visible catalog models.
func (a *App) ModelList() ([]agent.ModelListEntry, error) {
	return a.svc.ModelList(), nil
}

// CurrentModel returns the active provider and model.
func (a *App) CurrentModel() agent.ModelInfo {
	return a.svc.CurrentModel()
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

// SetDefaultModel writes the persisted default model to config.
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

// ProjectName returns the basename of the project directory.
func (a *App) ProjectName() string {
	return a.svc.ProjectName()
}

// ReadFileContent loads a file's contents for the in-app viewer.
func (a *App) ReadFileContent(path string) (string, error) {
	return a.svc.ReadFileContent(path)
}

// TokenUsage returns the current cumulative token usage for the session.
func (a *App) TokenUsage() agent.TokenReport {
	return a.svc.TokenUsage()
}

// SessionCurrent returns the active session.
func (a *App) SessionCurrent() agent.SessionSummary {
	return a.svc.SessionCurrent()
}

// SessionList returns sessions filtered by state.
func (a *App) SessionList(state string) ([]agent.SessionSummary, error) {
	return a.svc.SessionList(state)
}

// SessionSwitch switches to another session.
func (a *App) SessionSwitch(id string) error {
	if err := a.svc.SessionSwitch(id); err != nil {
		return err
	}
	a.emitSessionChanged()
	return nil
}

// SessionArchive archives a session.
func (a *App) SessionArchive(id string) error {
	closedCurrent, err := a.svc.SessionArchive(id)
	if err != nil {
		return err
	}
	if closedCurrent {
		a.emitSessionChanged()
	}
	return nil
}

// SessionDelete removes a session from disk.
func (a *App) SessionDelete(id string) error {
	closedCurrent, err := a.svc.SessionDelete(id)
	if err != nil {
		return err
	}
	if closedCurrent {
		a.emitSessionChanged()
	}
	return nil
}

// SessionNew starts a fresh session.
func (a *App) SessionNew() error {
	if err := a.svc.SessionNew(); err != nil {
		return err
	}
	a.emitSessionChanged()
	return nil
}

// SessionMessages returns persisted history for the current session.
func (a *App) SessionMessages() []agent.DisplayMessage {
	return a.svc.SessionMessages()
}

// SessionMessagesFor returns persisted history for a session without switching.
func (a *App) SessionMessagesFor(id string) ([]agent.DisplayMessage, error) {
	return a.svc.SessionMessagesFor(id)
}

// ProjectList returns every known project sorted by last activity.
func (a *App) ProjectList() ([]agent.ProjectSummary, error) {
	return a.svc.ProjectList()
}

// ProjectCurrent returns the project record for the current cwd.
func (a *App) ProjectCurrent() agent.ProjectSummary {
	return a.svc.ProjectCurrent()
}

// ProjectSwitch spawns a detached child in the target directory and quits.
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
	if abs == a.svc.ProjectRoot() {
		return nil
	}

	if err := a.svc.CloseForProjectSwitch(); err != nil {
		return fmt.Errorf("close current session: %w", err)
	}

	if err := a.relaunchIn(abs); err != nil {
		return err
	}
	wailsRuntime.Quit(a.ctx)
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

func (a *App) relaunchIn(dir string) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lightcode binary: %w", err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachAttr()
	return cmd.Start()
}
