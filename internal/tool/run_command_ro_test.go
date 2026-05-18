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
		{name: "cat", command: "cat run_command_ro_test.go", want: "package tool"},
		{name: "git read only", command: "git status --short", want: ""},
		{name: "git log", command: "git log --oneline -5", want: ""},
		{name: "git branch list", command: "git branch", want: ""},
		{name: "git tag list", command: "git tag", want: ""},
		{name: "find name", command: "find . -name '*.go'", want: ".go"},
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
		"echo $(touch /tmp/x)",
		"echo `touch /tmp/x`",
		"echo allowed\nrm -rf file",
		"echo allowed\rrm -rf file",
		"echo allowed && rm -rf file",
		"cat missing.txt || rm -rf file",
		"find . -delete",
		"find . -exec rm {} +",
		"find . -execdir rm {} +",
		"git branch -d old",
		"git branch -D main",
		"git tag -d v1",
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
		{command: "rg package .", want: true},
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
