package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestRunCommandRequiresCommand(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 1}, t.TempDir(), nil)

	for _, params := range []map[string]any{{}, {"command": ""}} {
		_, err := tool.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "command is required") {
			t.Fatalf("Execute(%v) error = %v, want command-required error", params, err)
		}
	}
}

func TestRunCommandExecutesShellCommandWithInheritedEnv(t *testing.T) {
	t.Setenv("LIGHTCODE_RUN_COMMAND_TEST", "env-value")
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "printf '%s' \"$LIGHTCODE_RUN_COMMAND_TEST\"",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "env-value" {
		t.Fatalf("Execute result = %q, want inherited env output", result)
	}
}

func TestRunCommandEachCallStartsFreshShell(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	first, err := tool.Execute(context.Background(), map[string]any{"command": "cd / && pwd"})
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	second, err := tool.Execute(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if strings.TrimSpace(first) != "/" {
		t.Fatalf("first pwd = %q, want /", first)
	}
	if strings.TrimSpace(second) == "/" {
		t.Fatalf("second pwd = %q, want fresh shell not affected by prior cd", second)
	}
}

func TestRunCommandReturnsNoOutputForSuccessfulSilentCommand(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	result, err := tool.Execute(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "(No output)" {
		t.Fatalf("Execute result = %q, want no-output marker", result)
	}
}

func TestRunCommandNonZeroExitReturnsExitErrorWithOutput(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	output, err := tool.Execute(context.Background(), map[string]any{"command": "printf fail && exit 7"})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if exitErr.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", exitErr.ExitCode)
	}
	if output != "fail" || !strings.Contains(exitErr.Output, "Error: Exit code 7\nfail") {
		t.Fatalf("output=%q exitErr.Output=%q, want failed command output", output, exitErr.Output)
	}
}

func TestRunCommandTimeoutUsesOverrideAndReturnsExitError(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)
	start := time.Now()

	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "sleep 2",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if exitErr.ExitCode != -1 || !strings.Contains(exitErr.Output, "timeout") {
		t.Fatalf("ExitError = %+v, want timeout with exit code -1", exitErr)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout elapsed %s, want command killed close to override", elapsed)
	}
}

func TestRunCommandBackgroundDelegatesToProcessManager(t *testing.T) {
	procMgr := &recordingProcessManager{id: "proc-1"}
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), procMgr)

	result, err := tool.Execute(context.Background(), map[string]any{
		"command":    "sleep 10",
		"background": true,
		"timeout":    float64(4),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Command running in background with ID: proc-1" {
		t.Fatalf("Execute result = %q, want background id", result)
	}
	if procMgr.command != "sleep 10" || procMgr.timeoutSec != 4 {
		t.Fatalf("ProcessManager call = (%q, %d), want command and timeout override", procMgr.command, procMgr.timeoutSec)
	}
}

func TestRunCommandBackgroundWithoutProcessManagerErrors(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), nil)

	_, err := tool.Execute(context.Background(), map[string]any{
		"command":    "sleep 10",
		"background": true,
	})
	if err == nil || !strings.Contains(err.Error(), "background processes not available") {
		t.Fatalf("Execute error = %v, want background unavailable error", err)
	}
}

func TestRunCommandTruncatesLargeOutputAndSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   2,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	result, err := tool.Execute(context.Background(), map[string]any{"command": "printf 'abcdefg\\n1234567\\n'"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "abcde... [truncated 7 chars]") {
		t.Fatalf("Execute result = %q, want per-line truncation", result)
	}
	if !strings.Contains(result, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("Execute result = %q, want spill path under home", result)
	}
}

type recordingProcessManager struct {
	id         string
	err        error
	command    string
	timeoutSec int
}

func (m *recordingProcessManager) Start(command string, timeoutSec int) (string, error) {
	m.command = command
	m.timeoutSec = timeoutSec
	return m.id, m.err
}
