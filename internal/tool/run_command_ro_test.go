package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestReadOnlyRunCommandAllowsWhitelistedCommands(t *testing.T) {
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil))
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "exact", command: "pwd", want: ""},
		{name: "with args", command: "echo allowed", want: "allowed"},
		{name: "tab args", command: "echo\tallowed", want: "allowed"},
		{name: "pipeline", command: "echo beta | grep beta", want: "beta"},
		{name: "git read only", command: "git status --short", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), map[string]any{"command": tt.command})
			if err != nil {
				t.Fatalf("Execute(%q) error = %v", tt.command, err)
			}
			if tt.want != "" && !strings.Contains(result, tt.want) {
				t.Fatalf("Execute(%q) result = %q, want containing %q", tt.command, result, tt.want)
			}
		})
	}
}

func TestReadOnlyRunCommandRejectsUnsafeCommands(t *testing.T) {
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil))
	tests := []string{
		"",
		"rm -rf file",
		"echo unsafe > file.txt",
		"echo allowed && rm -rf file",
		"cat missing.txt || rm -rf file",
		"lsx",
	}

	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), map[string]any{"command": command})
			if err == nil || !strings.Contains(err.Error(), "command not permitted in read-only mode") {
				t.Fatalf("Execute(%q) error = %v, want read-only rejection", command, err)
			}
		})
	}
}

func TestIsReadOnlyCommandClassifiesChainsBeforePrefixAllow(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{command: "echo allowed && pwd", want: true},
		{command: "echo allowed && rm -rf file", want: false},
		{command: "git log --oneline | head -1", want: true},
		{command: "git log --oneline | xargs rm", want: false},
		{command: " echo trimmed ", want: true},
		{command: "echo unsafe > file", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := isReadOnlyCommand(tt.command); got != tt.want {
				t.Fatalf("isReadOnlyCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
