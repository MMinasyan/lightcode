package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestReadOnlyRunCommandRejectsBackgroundParamBeforeProcessManager(t *testing.T) {
	procMgr := &recordingProcessManager{id: "proc-1"}
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), procMgr))

	_, err := tool.Execute(context.Background(), map[string]any{"command": "pwd", "background": true})
	if err == nil || !strings.Contains(err.Error(), "background") || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Execute background error = %v, want read-only background rejection", err)
	}
	if procMgr.command != "" {
		t.Fatalf("background command started as %q, want no process start", procMgr.command)
	}
}

func TestReadOnlyRunCommandAllowsWhitelistedCommands(t *testing.T) {
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil))
	literalFile := "literal-backtick-pattern.txt"
	if err := os.WriteFile(literalFile, []byte("`code`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(literalFile)

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "exact", command: "pwd", want: ""},
		{name: "with args", command: "echo allowed", want: "allowed"},
		{name: "tab args", command: "echo\tallowed", want: "allowed"},
		{name: "pipeline", command: "echo beta | grep beta", want: "beta"},
		{name: "quoted pipe", command: "grep -E 'package|func' run_command_ro_test.go", want: "package"},
		{name: "quoted semicolon", command: "printf 'a;b\\n'", want: "a;b"},
		{name: "cat", command: "cat run_command_ro_test.go", want: "package tool"},
		{name: "git read only", command: "git status --short", want: ""},
		{name: "git log", command: "git log --oneline -5", want: ""},
		{name: "git branch list", command: "git branch", want: ""},
		{name: "git branch list option", command: "git branch --list", want: ""},
		{name: "git branch list pattern", command: "git branch --list '*'", want: ""},
		{name: "literal single quoted substitution", command: "echo '$(whoami)'", want: "$(whoami)"},
		{name: "literal single quoted backticks", command: "grep '`code`' " + literalFile, want: "`code`"},
		{name: "git tag list", command: "git tag", want: ""},
		{name: "git tag list pattern", command: "git tag -l 'v*'", want: ""},
		{name: "find name", command: "find . -name '*.go'", want: ".go"},
		{name: "literal redirection character", command: "echo '>'", want: ">"},
		{name: "double quoted regex backslash", command: `printf '%s\n' "foo\.bar"`, want: `foo\.bar`},
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

func TestReadOnlyRunCommandIgnoresModelSuppliedTimeout(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "log.txt")
	if err := os.WriteFile(target, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 1}, tempDir, nil))

	start := time.Now()
	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "tail -f " + quoteShellArg(target),
		"timeout": float64(30),
	})
	if err == nil {
		t.Fatal("Execute succeeded, want configured timeout")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(exitErr.Output, "timeout") {
		t.Fatalf("Execute error = %v, want configured timeout ExitError", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Execute elapsed %s, model-supplied timeout was not ignored", elapsed)
	}
}

func TestReadOnlyRunCommandRejectsUnsafeCommands(t *testing.T) {
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil))
	tests := []string{
		"",
		"rm -rf file",
		"echo unsafe > file.txt",
		"cat <(echo ok)",
		"echo $(touch /tmp/x)",
		"echo `touch /tmp/x`",
		"echo allowed\nrm -rf file",
		"echo allowed\rrm -rf file",
		"echo ok & rm -rf file",
		"&& pwd",
		"echo ok && && pwd",
		"echo allowed && rm -rf file",
		`echo \" && touch /tmp/lightcode-owned \"`,
		"cat missing.txt || rm -rf file",
		"find . -delete",
		"find . '-delete'",
		"find . -exec rm {} +",
		"find . -execdir rm {} +",
		"git diff --output=/tmp/lightcode-out",
		`git diff --out\put=/tmp/lightcode-out`,
		"git log --output /tmp/lightcode-out -1",
		`git log --out\put /tmp/lightcode-out -1`,
		"git show --output=/tmp/lightcode-out HEAD",
		"git diff --ext-diff",
		`git diff --ext-\diff`,
		"git show --textconv HEAD:README.md",
		`git show --text\conv HEAD:README.md`,
		"git branch -d old",
		"git branch -D main",
		"git branch -m old new",
		"git branch --move old new",
		"git tag -d v1",
		"git tag v1",
		"git tag -f v1",
		"git tag --delete v1",
		"tree -o /tmp/lightcode-ro-tree",
		"file README.md",
		"file -z README.md",
		"file --uncompress README.md",
		"file -Z README.md",
		"file --uncompress-noreport README.md",
		"file -S README.md",
		"file --no-sandbox README.md",
		"rg --pre=touch package .",
		`rg --pre\=touch package .`,
		"rg --pre touch package .",
		"rg --pre-glob=*.go package .",
		`rg --pre-\glob=*.go package .`,
		"rg -z package .",
		"rg --search-zip package .",
		`rg --search-\zip package .`,
		"rg -zS package .",
		"git diff --out*",
		"echo *",
		"echo ?",
		"echo [abc]",
		"echo {a,b}",
		"echo $HOME",
		"echo ~/file",
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
		{command: "&& pwd", want: false},
		{command: "echo ok && && pwd", want: false},
		{command: "echo allowed && rm -rf file", want: false},
		{command: "echo \"a && b\"", want: true},
		{command: `echo \" && touch /tmp/lightcode-owned \"`, want: false},
		{command: "echo 'a & b'", want: true},
		{command: "grep -E 'foo|bar' file", want: true},
		{command: "printf 'a;b\\n'", want: true},
		{command: "git log --oneline | head -1", want: true},
		{command: "git log --oneline | xargs rm", want: false},
		{command: "git branch --show-current", want: true},
		{command: "git branch --format '%(refname:short)'", want: true},
		{command: "git branch --list 'feature*'", want: true},
		{command: "git branch feature", want: false},
		{command: "git tag -l 'v*'", want: true},
		{command: "git tag v1", want: false},
		{command: "find . -maxdepth 2 -type f -name '*.go'", want: true},
		{command: "find . '-delete'", want: false},
		{command: "rg package .", want: true},
		{command: "rg -- --pre run_command_ro_test.go", want: true},
		{command: "rg -- --search-zip run_command_ro_test.go", want: true},
		{command: "rg --pre=touch package .", want: false},
		{command: `rg --pre\=touch package .`, want: false},
		{command: "rg -z package .", want: false},
		{command: "rg --search-zip package .", want: false},
		{command: `rg --search-\zip package .`, want: false},
		{command: "rg -zS package .", want: false},
		{command: "git log --output=/tmp/lightcode-out -1", want: false},
		{command: `git log --out\put /tmp/lightcode-out -1`, want: false},
		{command: "tree -o /tmp/lightcode-ro-tree", want: false},
		{command: " echo trimmed ", want: true},
		{command: "echo unsafe > file", want: false},
		{command: "echo '$(whoami)'", want: true},
		{command: "grep '`code`' README.md", want: true},
		{command: "echo '>'", want: true},
		{command: "echo *", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := isReadOnlyCommand(tt.command); got != tt.want {
				t.Fatalf("isReadOnlyCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestReadOnlyRunCommandSuppressesGitContentHelpers(t *testing.T) {
	repo := t.TempDir()
	runGitForReadOnlyTest(t, repo, "init")
	runGitForReadOnlyTest(t, repo, "config", "user.email", "test@example.invalid")
	runGitForReadOnlyTest(t, repo, "config", "user.name", "Test User")

	marker := filepath.Join(repo, "helper-ran")
	textconv := filepath.Join(repo, "textconv.sh")
	external := filepath.Join(repo, "external.sh")
	writeExecutableForReadOnlyTest(t, textconv, fmt.Sprintf("#!/bin/sh\nprintf textconv >> %q\ncat \"$1\"\n", marker))
	writeExecutableForReadOnlyTest(t, external, fmt.Sprintf("#!/bin/sh\nprintf external >> %q\nexit 0\n", marker))

	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.secret diff=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForReadOnlyTest(t, repo, "add", ".")
	runGitForReadOnlyTest(t, repo, "commit", "-m", "initial")
	runGitForReadOnlyTest(t, repo, "config", "diff.secret.textconv", textconv)
	runGitForReadOnlyTest(t, repo, "config", "diff.external", external)
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForReadOnlyTest(t, repo, "add", "file.secret")
	runGitForReadOnlyTest(t, repo, "commit", "-m", "second")
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	tool := NewReadOnlyRunCommand(NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, repo, nil))
	for _, command := range []string{
		"git diff -- file.secret",
		"git log -p -1 -- file.secret",
		"git show HEAD -- file.secret",
		"git show HEAD:file.secret",
		"git blame -- file.secret",
	} {
		t.Run(command, func(t *testing.T) {
			_ = os.Remove(marker)
			if _, err := tool.Execute(context.Background(), map[string]any{"command": command}); err != nil {
				t.Fatalf("Execute(%q) error = %v", command, err)
			}
			if data, err := os.ReadFile(marker); err == nil {
				t.Fatalf("Execute(%q) ran configured helper: %q", command, data)
			} else if !os.IsNotExist(err) {
				t.Fatalf("marker read error = %v", err)
			}
		})
	}
}

func runGitForReadOnlyTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeExecutableForReadOnlyTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
