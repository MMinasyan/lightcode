package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/shellparse"
)

var simpleReadOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "grep": true, "rg": true, "head": true,
	"tail": true, "wc": true, "file": true, "stat": true, "which": true,
	"pwd": true, "echo": true, "printf": true, "tree": true,
}

var readOnlyGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"blame": true, "rev-parse": true,
}

var readOnlyFindFlags = map[string]bool{
	"-print": true, "-print0": true, "-a": true, "-and": true, "-o": true, "-or": true,
	"!": true, "(": true, ")": true,
}

var readOnlyFindValueFlags = map[string]bool{
	"-name": true, "-iname": true, "-type": true, "-maxdepth": true, "-mindepth": true,
	"-path": true, "-ipath": true, "-regex": true, "-iregex": true, "-size": true,
	"-mtime": true, "-mmin": true, "-newer": true,
}

var dangerousReadOnlyFlags = []string{
	"--output",
	"--pre",
	"--pre-glob",
	"--ext-diff",
	"--textconv",
}

// ReadOnlyRunCommand wraps RunCommand and restricts commands to a
// whitelist of non-destructive operations (ls, cat, grep, git log, etc.).
type ReadOnlyRunCommand struct {
	inner *RunCommand
}

// NewReadOnlyRunCommand creates a read-only command tool.
func NewReadOnlyRunCommand(inner *RunCommand) *ReadOnlyRunCommand {
	return &ReadOnlyRunCommand{inner: inner}
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
	if command == "" {
		return false
	}
	segments, err := shellparse.Parse(command)
	if err != nil || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if segment.UnsafeExpansion || len(segment.Redirections) > 0 {
			return false
		}
		if !isReadOnlySimpleCommand(segment.Argv) {
			return false
		}
	}
	return true
}

func isReadOnlySimpleCommand(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	if hasDangerousReadOnlyFlag(fields) {
		return false
	}
	switch fields[0] {
	case "git":
		return isReadOnlyGit(fields[1:])
	case "find":
		return isReadOnlyFind(fields[1:])
	default:
		return simpleReadOnlyCommands[fields[0]]
	}
}

func hasDangerousReadOnlyFlag(fields []string) bool {
	for _, arg := range fields {
		if arg == "--" {
			return false
		}
		for _, flag := range dangerousReadOnlyFlags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
	}
	return false
}

func isReadOnlyGit(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	if readOnlyGitSubcommands[sub] {
		return true
	}
	switch sub {
	case "branch":
		return isReadOnlyGitBranch(args[1:])
	case "tag":
		return isReadOnlyGitTag(args[1:])
	default:
		return false
	}
}

func isReadOnlyGitBranch(args []string) bool {
	if len(args) == 0 {
		return true
	}
	listMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "--sort=") || strings.HasPrefix(arg, "--color=") {
			continue
		}
		switch arg {
		case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose",
			"--show-current", "--no-color", "--color",
			"--column", "--no-column", "--contains", "--no-contains",
			"--merged", "--no-merged", "--points-at", "--format", "--sort":
			if gitBranchFlagNeedsValue(arg) {
				i++
				if i >= len(args) {
					return false
				}
			}
		case "--list":
			listMode = true
		default:
			if listMode && !strings.HasPrefix(arg, "-") {
				continue
			}
			return false
		}
	}
	return true
}

func gitBranchFlagNeedsValue(flag string) bool {
	switch flag {
	case "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort":
		return true
	default:
		return false
	}
}

func isReadOnlyGitTag(args []string) bool {
	if len(args) == 0 {
		return true
	}
	hasList := false
	for _, arg := range args {
		switch {
		case arg == "-l" || arg == "--list":
			hasList = true
		case strings.HasPrefix(arg, "-"):
			return false
		}
	}
	return hasList
}

func isReadOnlyFind(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if readOnlyFindFlags[arg] {
			continue
		}
		if readOnlyFindValueFlags[arg] {
			i++
			if i >= len(args) {
				return false
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}
