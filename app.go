package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/permission"
)

// App is the Wails-bound struct that bridges the Go backend to the
// frontend. All exported methods are callable from JavaScript.
type App struct {
	ctx context.Context
	svc *agent.Agent
}

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
				"success":   !ev.IsError,
				"output":    ev.Result,
			})
		case agent.EventSubagentStart:
			wailsRuntime.EventsEmit(a.ctx, "subagent_session_start", map[string]any{
				"sessionId":      ev.SubagentSessionID,
				"taskToolCallId": ev.ToolCallID,
				"taskIndex":      ev.TaskIndex,
			})
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
			"id":      ev.ToolCallID,
			"success": !ev.IsError,
			"output":  ev.Result,
		})
	case agent.EventUsage:
		wailsRuntime.EventsEmit(a.ctx, "usage", a.svc.TokenUsage())
	case agent.EventTurnStart:
		wailsRuntime.EventsEmit(a.ctx, "status", map[string]any{"state": "streaming"})
	case agent.EventTurnEnd:
		wailsRuntime.EventsEmit(a.ctx, "turn_end", map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled})
		wailsRuntime.EventsEmit(a.ctx, "status", map[string]any{"state": "idle"})
	case agent.EventError:
		wailsRuntime.EventsEmit(a.ctx, "error", map[string]any{"message": ev.Error})
	case agent.EventPermissionRequest:
		wailsRuntime.EventsEmit(a.ctx, "permission_request", map[string]any{
			"id":   ev.PermReq.ID,
			"tool": ev.PermReq.ToolName,
			"args": ev.PermReq.Arg,
		})
	case agent.EventCompactionStart:
		wailsRuntime.EventsEmit(a.ctx, "compaction_start", nil)
	case agent.EventCompactionEnd:
		wailsRuntime.EventsEmit(a.ctx, "compaction_end", nil)
		a.emitSessionChanged()
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

// SendPrompt sends a user message and starts the agentic loop.
func (a *App) SendPrompt(content string) (int, error) {
	return a.svc.SendPrompt(a.ctx, content)
}

// AppendUserMessage adds a user message as a complete turn without running the model.
func (a *App) AppendUserMessage(content string) (int, error) {
	return a.svc.AppendUserMessage(content)
}

// SwitchModel changes the active provider and model.
func (a *App) SwitchModel(providerName, model string) error {
	return a.svc.SwitchModel(providerName, model)
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

// RespondPermission answers a pending permission prompt.
func (a *App) RespondPermission(id string, action string) error {
	return a.svc.RespondPermission(id, action == "allow")
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

// ModelList returns all configured providers and their models.
func (a *App) ModelList() ([]agent.ProviderModels, error) {
	return a.svc.ModelList(), nil
}

// CurrentModel returns the active provider and model.
func (a *App) CurrentModel() agent.ModelInfo {
	return a.svc.CurrentModel()
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

	_ = a.svc.Cancel()
	for i := 0; i < 200; i++ {
		if !a.svc.Busy() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Close session via store (need direct access for this Wails-specific flow).
	if a.svc.Store().Active() {
		a.svc.Store().Close()
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
