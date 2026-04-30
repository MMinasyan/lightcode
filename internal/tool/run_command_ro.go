package tool

import (
	"context"
	"fmt"
	"strings"
)

var readOnlyCommands = []string{
	"ls", "cat", "grep", "rg", "find", "head", "tail", "wc",
	"file", "stat", "which", "pwd", "echo", "tree",
	"git log", "git diff", "git show", "git status",
	"git blame", "git branch", "git rev-parse", "git tag",
}

type ReadOnlyRunCommand struct {
	inner RunCommand
}

func NewReadOnlyRunCommand() *ReadOnlyRunCommand {
	return &ReadOnlyRunCommand{}
}

func (*ReadOnlyRunCommand) Name() string { return "run_command" }
func (*ReadOnlyRunCommand) Description() string {
	return "Execute a read-only shell command and return its output. Only non-destructive commands are allowed (ls, cat, grep, find, git log, git diff, etc.)."
}
func (r *ReadOnlyRunCommand) ParametersSchema() map[string]any {
	return r.inner.ParametersSchema()
}

func (r *ReadOnlyRunCommand) Execute(ctx context.Context, params map[string]any) (string, error) {
	command, _ := params["command"].(string)
	if !isReadOnlyCommand(command) {
		return "", fmt.Errorf("command not permitted in read-only mode: %s", command)
	}
	return r.inner.Execute(ctx, params)
}

func isReadOnlyCommand(command string) bool {
	command = strings.TrimSpace(command)
	if strings.ContainsAny(command, ">") {
		return false
	}
	for _, allowed := range readOnlyCommands {
		if command == allowed || strings.HasPrefix(command, allowed+" ") || strings.HasPrefix(command, allowed+"\t") {
			return true
		}
	}
	for _, sep := range []string{"&&", "||", ";", "|"} {
		if strings.Contains(command, sep) {
			parts := strings.Split(command, sep)
			for _, part := range parts {
				if !isReadOnlyCommand(strings.TrimSpace(part)) {
					return false
				}
			}
			return true
		}
	}
	return false
}
