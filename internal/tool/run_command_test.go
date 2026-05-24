package tool

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestRunCommandNonZeroExitLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   2,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	output, err := tool.Execute(context.Background(), map[string]any{"command": "printf 'abcdefg\\n1234567\\n' && exit 7"})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if output == "abcdefg\n1234567\n" {
		t.Fatalf("output = %q, want bounded output", output)
	}
	if !strings.Contains(exitErr.Output, "Error: Exit code 7\n") {
		t.Fatalf("ExitError.Output = %q, missing exit header", exitErr.Output)
	}
	if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
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

func TestRunCommandTimeoutLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   5,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	output, err := tool.Execute(context.Background(), map[string]any{
		"command": "printf 'abcdefg\\n1234567\\n'; sleep 5",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if output == "abcdefg\n1234567\n" {
		t.Fatalf("output = %q, want bounded output", output)
	}
	if !strings.Contains(exitErr.Output, "Error: Exit code -1 (timeout)\n") {
		t.Fatalf("ExitError.Output = %q, missing timeout header", exitErr.Output)
	}
	if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
	}
}

func TestRunCommandCancellationOutput(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)
		errCh := make(chan error, 1)
		go func() {
			_, err := tool.Execute(ctx, map[string]any{"command": "sleep 5"})
			errCh <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		var exitErr *ExitError
		select {
		case err := <-errCh:
			if !errors.As(err, &exitErr) {
				t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled command did not return")
		}
		if exitErr.Output != "command cancelled" {
			t.Fatalf("ExitError.Output = %q, want exact cancellation marker", exitErr.Output)
		}
	})

	t.Run("non-empty", func(t *testing.T) {
		home := t.TempDir()
		ready := home + "/ready"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tool := NewRunCommand(config.ToolsConfig{
			CommandTimeout:   5,
			MaxOutputBytes:   12,
			ReadLineMaxChars: 5,
		}, home, nil)
		errCh := make(chan error, 1)
		outCh := make(chan string, 1)
		command := "printf 'abcdefg\\n1234567\\n'; touch " + ready + "; sleep 5"
		go func() {
			output, err := tool.Execute(ctx, map[string]any{"command": command})
			outCh <- output
			errCh <- err
		}()
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(ready); err != nil {
			t.Fatalf("command did not reach ready marker: %v", err)
		}
		cancel()
		var exitErr *ExitError
		select {
		case err := <-errCh:
			if !errors.As(err, &exitErr) {
				t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled command did not return")
		}
		output := <-outCh
		if !strings.HasPrefix(exitErr.Output, "command cancelled\n") {
			t.Fatalf("ExitError.Output = %q, want cancellation header plus output", exitErr.Output)
		}
		if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
			t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
		}
		if output == "abcdefg\n1234567\n" {
			t.Fatalf("output = %q, want bounded output", output)
		}
	})
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

func TestRunCommandTruncatesManyLinesAndSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout: 2,
		MaxOutputBytes: 100,
	}, home, nil)

	var b strings.Builder
	for i := 1; i <= 25; i++ {
		b.WriteString("line ")
		b.WriteString(string(rune('A' + i - 1)))
		b.WriteByte('\n')
	}
	fullOutput := b.String()

	result, err := tool.Execute(context.Background(), map[string]any{"command": "cat <<'EOF'\n" + fullOutput + "EOF"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "line A") || !strings.Contains(result, "line Y") {
		t.Fatalf("Execute result = %q, want first and last lines", result)
	}
	spillPath := extractSpillPath(t, result)
	if !strings.HasPrefix(spillPath, home+"/.lightcode/cmd_output_") {
		t.Fatalf("spill path = %q, want command spill path under home %q", spillPath, home)
	}
	data, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", spillPath, err)
	}
	if string(data) != fullOutput {
		t.Fatalf("spill file content = %q, want full output %q", string(data), fullOutput)
	}
}

func TestRunCommandTimeoutSendsTERMBeforeKILL(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)

	output, err := tool.Execute(context.Background(), map[string]any{
		"command": "trap 'echo got-term; exit 0' TERM; sleep 10",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if !strings.Contains(output, "got-term") || !strings.Contains(exitErr.Output, "got-term") {
		t.Fatalf("output=%q exitErr.Output=%q, want SIGTERM trap output before timeout error", output, exitErr.Output)
	}
}

func TestRunCommandTerminateProcessUsesExistingWaitChannel(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 10")
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error = %v", err)
	}
	done := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(waitDone)
	}()

	tool := NewRunCommand(config.ToolsConfig{}, t.TempDir(), nil)
	tool.terminateProcess(cmd, waitDone)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatal("terminateProcess did not wait for the existing cmd.Wait channel")
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

func extractSpillPath(t *testing.T, result string) string {
	t.Helper()
	marker := "saved to: "
	idx := strings.LastIndex(result, marker)
	if idx < 0 {
		t.Fatalf("result = %q, missing spill marker", result)
	}
	path := result[idx+len(marker):]
	if end := strings.IndexAny(path, "]\n"); end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}
