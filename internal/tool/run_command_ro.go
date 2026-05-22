package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/shellparse"
)

var simpleReadOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "grep": true, "head": true,
	"tail": true, "wc": true, "stat": true, "which": true,
	"pwd": true, "echo": true, "printf": true,
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
	"-mtime": true, "-mmin": true,
}

// dangerousReadOnlyFlags is the chokepoint for read-only safety filtering.
// Escaped spellings (--pre\=, --out\put, --ext-\diff, etc.) are unescaped by
// shellparse argv normalization before any per-command code runs, so a single
// shared denial list catches every variant. Per-command locality would add
// parser complexity without coverage gain.
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
	if background, _ := params["background"].(bool); background {
		return "", fmt.Errorf("background commands are not permitted in read-only mode")
	}
	safeCommand, err := readOnlyCommand(command)
	if err != nil {
		return "", fmt.Errorf("command not permitted in read-only mode: %s", command)
	}
	return r.inner.Execute(ctx, map[string]any{"command": safeCommand})
}

func isReadOnlyCommand(command string) bool {
	_, err := readOnlyCommand(command)
	return err == nil
}

func readOnlyCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("empty command")
	}
	segments, err := shellparse.Parse(command)
	if err != nil || len(segments) == 0 {
		return "", fmt.Errorf("parse command: %w", err)
	}
	parts := make([]string, 0, len(segments)*2)
	for i, segment := range segments {
		if segment.UnsafeExpansion || len(segment.Redirections) > 0 {
			return "", fmt.Errorf("unsupported shell syntax")
		}
		argv, ok := readOnlyArgv(segment.Argv)
		if !ok {
			return "", fmt.Errorf("unsafe command")
		}
		parts = append(parts, quoteShellArgv(argv))
		if segment.Separator == "" {
			continue
		}
		if i == len(segments)-1 || !isSafeReadOnlySeparator(segment.Separator) {
			return "", fmt.Errorf("unsafe shell separator")
		}
		parts = append(parts, segment.Separator)
	}
	return strings.Join(parts, " "), nil
}

func readOnlyArgv(fields []string) ([]string, bool) {
	if len(fields) == 0 {
		return nil, false
	}
	if hasDangerousReadOnlyFlag(fields) {
		return nil, false
	}
	switch fields[0] {
	case "git":
		if hasGitConfigFlag(fields[1:]) {
			return nil, false
		}
		if !isReadOnlyGit(fields[1:]) {
			return nil, false
		}
		return rewriteReadOnlyGit(fields), true
	case "rg":
		if !isReadOnlyRG(fields[1:]) {
			return nil, false
		}
		return fields, true
	case "find":
		if !isReadOnlyFind(fields[1:]) {
			return nil, false
		}
		return fields, true
	default:
		if !simpleReadOnlyCommands[fields[0]] {
			return nil, false
		}
		return fields, true
	}
}

func isReadOnlySimpleCommand(fields []string) bool {
	_, ok := readOnlyArgv(fields)
	return ok
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

func hasGitConfigFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-c" || strings.HasPrefix(arg, "-c") || arg == "--config-env" || strings.HasPrefix(arg, "--config-env=") {
			return true
		}
	}
	return false
}

func rewriteReadOnlyGit(fields []string) []string {
	if len(fields) < 2 || fields[0] != "git" {
		return fields
	}
	args := fields[1:]
	rewritten := []string{
		"git",
		"--no-pager",
		"--no-optional-locks",
		"-c", "core.fsmonitor=false",
		"-c", "diff.external=",
		"-c", "core.pager=cat",
		args[0],
	}
	if gitMaterializesContent(args[0]) {
		rewritten = append(rewritten, "--no-ext-diff", "--no-textconv")
	}
	rewritten = append(rewritten, args[1:]...)
	return rewritten
}

func gitMaterializesContent(subcommand string) bool {
	switch subcommand {
	case "diff", "log", "show", "blame":
		return true
	default:
		return false
	}
}

func isSafeReadOnlySeparator(separator string) bool {
	switch separator {
	case "|", "&&", "||", ";":
		return true
	default:
		return false
	}
}

func quoteShellArgv(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, quoteShellArg(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteShellArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if isShellSafeArg(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func isShellSafeArg(arg string) bool {
	for _, r := range arg {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '_', '-', '.', '/', ':', ',', '=', '+', '%', '@':
			continue
		default:
			return false
		}
	}
	return true
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

func isReadOnlyRG(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return true
		}
		if arg == "--search-zip" || strings.HasPrefix(arg, "--search-zip=") {
			return false
		}
		if strings.HasPrefix(arg, "--") {
			continue
		}
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, "z") {
			return false
		}
	}
	return true
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
