package tool

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/cmdoutput"
	"github.com/MMinasyan/lightcode/internal/config"
)

// ProcessManager is the interface for background process management.
// The concrete implementation lives in internal/process/ and is wired in
// by the agent. May be nil if background process support is not loaded.
type ProcessManager interface {
	Start(command string, timeoutSec int) (id string, err error)
}

type activeProcessLister interface {
	ActiveIDs() []string
}

// RunCommand implements the run_command tool.
type RunCommand struct {
	cfg           config.ToolsConfig
	homeDir       string
	workspaceRoot string
	procMgr       ProcessManager
}

// NewRunCommand creates a RunCommand tool.
func NewRunCommand(cfg config.ToolsConfig, homeDir string, procMgr ProcessManager) *RunCommand {
	return NewRunCommandAtRoot(cfg, homeDir, "", procMgr)
}

func NewRunCommandAtRoot(cfg config.ToolsConfig, homeDir, workspaceRoot string, procMgr ProcessManager) *RunCommand {
	return &RunCommand{
		cfg:           cfg,
		homeDir:       homeDir,
		workspaceRoot: workspaceRoot,
		procMgr:       procMgr,
	}
}

func (r *RunCommand) SetToolsConfig(cfg config.ToolsConfig) { r.cfg = cfg }

func (*RunCommand) Name() string { return "run_command" }

func (*RunCommand) Description() string {
	return `Executes a shell command and returns combined stdout and stderr.
- Each call starts a fresh shell in the project root. Environment variables, aliases, and working directory do not persist between calls. Use "cd /path && command" if you need a different working directory.
- Foreground commands use the default timeout. Background commands run until they exit, are killed, or reach an explicit timeout parameter.
- For commands that may run for a long time, keep producing output, wait on external state, and are not needed before your next step, set background=true. It returns immediately with a process ID. You will be notified when it finishes. To read output while it is still running, use sleep to wait, then process to read the output. To kill it, use process. Do not use background=true for commands that will probably finish in a few seconds.
- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use <EDIT FILE OR WRITE FILE>.`
}

func (*RunCommand) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds for this command. Overrides the default.",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "If true, run the command in the background and return immediately with a process ID.",
			},
		},
		"required": []string{"command"},
	}
}

func (r *RunCommand) Execute(ctx context.Context, params map[string]any) (string, error) {
	command, _ := params["command"].(string)
	if command == "" {
		return "", fmt.Errorf("run_command: command is required")
	}

	background, _ := params["background"].(bool)
	timeoutSec := 0
	if v, ok := params["timeout"].(float64); ok {
		timeoutSec = int(v)
	}

	if background {
		if timeoutSec < 1 {
			timeoutSec = 0
		}
		return r.runBackground(ctx, command, timeoutSec)
	}
	if timeoutSec < 1 {
		timeoutSec = r.cfg.CommandTimeout
	}
	return r.runForeground(ctx, command, timeoutSec)
}

func (r *RunCommand) runBackground(ctx context.Context, command string, timeoutSec int) (string, error) {
	if r.procMgr == nil {
		return "", fmt.Errorf("run_command: background processes not available")
	}
	id, err := r.procMgr.Start(command, timeoutSec)
	if err != nil {
		return "", fmt.Errorf("run_command: background start: %w", err)
	}
	active := []string{id}
	if lister, ok := r.procMgr.(activeProcessLister); ok {
		active = lister.ActiveIDs()
	}
	return fmt.Sprintf("Command running in the background with ID: `%s`. You will be notified when it finishes. If your next steps do not depend on its output, continue with them; otherwise use sleep to wait and process to read the output.\nRunning in the background: `%s`.", id, strings.Join(active, ", ")), nil
}

func (r *RunCommand) runForeground(ctx context.Context, command string, timeoutSec int) (string, error) {
	cmdCtx := ctx
	var cancel context.CancelFunc
	if timeoutSec > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	cmd := exec.Command("sh", "-c", command)
	if r.workspaceRoot != "" {
		cmd.Dir = r.workspaceRoot
	}
	cmd.SysProcAttr = childProcAttr()

	capture := cmdoutput.NewCapture(cmdoutput.Options{
		HomeDir:      r.homeDir,
		SpillPrefix:  "cmd_output_",
		MaxBytes:     r.cfg.MaxOutputBytes,
		MaxLineChars: r.cfg.ReadLineMaxChars,
	})
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	err := cmd.Start()
	if err != nil {
		return "", fmt.Errorf("run_command: start: %w", err)
	}
	defer capture.Close()

	done := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(waitDone)
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		// User cancelled — send SIGTERM, wait 500ms, then SIGKILL.
		r.terminateProcess(cmd, waitDone)
		<-done
		body := capture.Format()
		output := "command cancelled"
		if body != "" {
			output += "\n" + body
		}
		return body, &ExitError{
			Output:   output,
			ExitCode: -1,
		}
	case <-cmdCtx.Done():
		// Timeout.
		r.terminateProcess(cmd, waitDone)
		<-done
		body := capture.Format()
		return body, &ExitError{
			Output:   fmt.Sprintf("Error: Exit code -1 (timeout)\n%s", body),
			ExitCode: -1,
		}
	}

	output := capture.Format()

	if cmdCtx.Err() == context.DeadlineExceeded {
		return output, &ExitError{
			Output:   fmt.Sprintf("Error: Exit code -1 (timeout)\n%s", output),
			ExitCode: -1,
		}
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			return output, &ExitError{
				Output:   fmt.Sprintf("Error: Exit code %d\n%s", code, output),
				ExitCode: code,
			}
		}
		return output, fmt.Errorf("run_command: %w", waitErr)
	}

	if output == "" {
		return "(No output)", nil
	}
	return output, nil
}

func (r *RunCommand) terminateProcess(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	// Send SIGTERM to process group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	// Wait 500ms grace period.
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
		// SIGKILL if still running.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
