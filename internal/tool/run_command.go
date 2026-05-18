package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

// ProcessManager is the interface for background process management.
// The concrete implementation lives in internal/process/ and is wired in
// by the agent. May be nil if background process support is not loaded.
type ProcessManager interface {
	Start(command string, timeoutSec int) (id string, err error)
}

// ExitError carries the combined output and exit code from a failed command.
type ExitError struct {
	Output   string
	ExitCode int
}

func (e *ExitError) Error() string { return e.Output }

// RunCommand implements the run_command tool.
type RunCommand struct {
	cfg     config.ToolsConfig
	homeDir string
	procMgr ProcessManager
}

// NewRunCommand creates a RunCommand tool.
func NewRunCommand(cfg config.ToolsConfig, homeDir string, procMgr ProcessManager) *RunCommand {
	return &RunCommand{
		cfg:     cfg,
		homeDir: homeDir,
		procMgr: procMgr,
	}
}

func (*RunCommand) Name() string { return "run_command" }

func (*RunCommand) Description() string {
	return `Executes a shell command and returns combined stdout and stderr.
- Each call starts a fresh shell in the project root. Environment variables, aliases, and working directory do not persist between calls. Use "cd /path && command" if you need a different working directory.
- Default timeout is 120 seconds. Use the timeout parameter to override for commands that need more time.
- For long-running commands like dev servers, file watchers, or test suites that run continuously, set background=true. The command returns immediately with a process ID. Use the sleep tool to wait, then the process tool to read output or kill the process.
- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file.`
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
	timeoutSec := r.cfg.CommandTimeout
	if v, ok := params["timeout"].(float64); ok {
		timeoutSec = int(v)
	}
	if timeoutSec < 1 {
		timeoutSec = r.cfg.CommandTimeout
	}

	if background {
		return r.runBackground(ctx, command, timeoutSec)
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
	return fmt.Sprintf("Command running in background with ID: %s", id), nil
}

func (r *RunCommand) runForeground(ctx context.Context, command string, timeoutSec int) (string, error) {
	cmdCtx := ctx
	var cancel context.CancelFunc
	if timeoutSec > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.SysProcAttr = childProcAttr()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Start()
	if err != nil {
		return "", fmt.Errorf("run_command: start: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		// User cancelled — send SIGTERM, wait 500ms, then SIGKILL.
		r.terminateProcess(cmd)
		<-done
		return string(stdout.Bytes()) + string(stderr.Bytes()), &ExitError{
			Output:   "command cancelled",
			ExitCode: -1,
		}
	case <-cmdCtx.Done():
		// Timeout.
		r.terminateProcess(cmd)
		<-done
		output := string(stdout.Bytes()) + string(stderr.Bytes())
		return output, &ExitError{
			Output:   fmt.Sprintf("Error: Exit code -1 (timeout)\n%s", output),
			ExitCode: -1,
		}
	}

	output := string(stdout.Bytes()) + string(stderr.Bytes())

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

	// Apply output truncation.
	maxBytes := r.cfg.MaxOutputBytes
	if maxBytes <= 0 {
		return output, nil
	}

	if len(output) <= maxBytes {
		return output, nil
	}

	// Truncate: keep first 10 lines + last 10 lines, save full to file.
	return r.truncateOutput(output, maxBytes), nil
}

func (r *RunCommand) truncateOutput(output string, maxBytes int) string {
	lines := strings.Split(output, "\n")
	totalLines := len(lines)
	// Remove trailing empty line from split.
	if totalLines > 0 && lines[totalLines-1] == "" {
		lines = lines[:totalLines-1]
		totalLines--
	}

	if totalLines <= 20 {
		spillPath := r.spillFile()
		_ = os.MkdirAll(filepath.Dir(spillPath), 0o700)
		_ = os.WriteFile(spillPath, []byte(output), 0o600)
		return perLineTruncate(output, r.cfg.ReadLineMaxChars) + fmt.Sprintf("\n[Output truncated. Full output (%d bytes) saved to: %s]", len(output), spillPath)
	}

	firstLines := lines[:10]
	lastLines := lines[totalLines-10:]

	var buf bytes.Buffer
	for _, l := range firstLines {
		buf.WriteString(truncateLine(l, r.cfg.ReadLineMaxChars))
		buf.WriteByte('\n')
	}
	buf.WriteString(fmt.Sprintf("[Output truncated. Full output (%d bytes) saved to: %s]\n", len(output), r.spillAndSave(output)))
	for _, l := range lastLines {
		buf.WriteString(truncateLine(l, r.cfg.ReadLineMaxChars))
		buf.WriteByte('\n')
	}
	result := buf.String()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}

func (r *RunCommand) spillAndSave(output string) string {
	spillPath := r.spillFile()
	_ = os.MkdirAll(filepath.Dir(spillPath), 0o700)
	_ = os.WriteFile(spillPath, []byte(output), 0o600)
	return spillPath
}

func (r *RunCommand) spillFile() string {
	ts := time.Now().UnixNano()
	return filepath.Join(r.homeDir, ".lightcode", fmt.Sprintf("cmd_output_%d_%x.txt", ts, ts%65536))
}

func (r *RunCommand) terminateProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Send SIGTERM to process group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	// Wait 500ms grace period.
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
		// SIGKILL if still running.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func truncateLine(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + fmt.Sprintf("... [truncated %d chars]", len(runes))
}

func perLineTruncate(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(truncateLine(l, maxChars))
		buf.WriteByte('\n')
	}
	result := buf.String()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}
