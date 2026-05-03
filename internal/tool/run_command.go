package tool

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

type ExitError struct {
	Output   string
	ExitCode int
}

func (e *ExitError) Error() string { return e.Output }

// RunCommand implements the run_command tool. Permission checking is
// handled by the PermWrapped wrapper, not by this struct.
type RunCommand struct{}

func (*RunCommand) Name() string { return "run_command" }

func (*RunCommand) Description() string {
	return "Execute a shell command and return its combined stdout and stderr output."
}

func (*RunCommand) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
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

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.SysProcAttr = childProcAttr()
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", &ExitError{
				Output:   fmt.Sprintf("%s\nexit status: %d", string(out), exitErr.ExitCode()),
				ExitCode: exitErr.ExitCode(),
			}
		}
		return "", fmt.Errorf("run_command: %w", err)
	}
	return string(out), nil
}
