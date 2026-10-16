package tool

import (
	"testing"
)

func TestReadOnlyRunCommandAllowsWhitelistedCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "exact", command: "pwd"},
		{name: "with args", command: "echo allowed"},
		{name: "tab args", command: "echo\tallowed"},
		{name: "pipeline", command: "echo beta | grep beta"},
		{name: "grep count", command: "grep -c package literal-grep-count-pattern.txt"},
		{name: "quoted pipe", command: "grep -E 'package|func' run_command_ro_test.go"},
		{name: "quoted semicolon", command: "printf 'a;b\\n'"},
		{name: "cat", command: "cat run_command_ro_test.go"},
		{name: "git read only", command: "git status --short"},
		{name: "git log", command: "git log --oneline -5"},
		{name: "git branch list", command: "git branch"},
		{name: "git branch list option", command: "git branch --list"},
		{name: "git branch list pattern", command: "git branch --list '*'"},
		{name: "literal single quoted substitution", command: "echo '$(whoami)'"},
		{name: "literal single quoted backticks", command: "grep '`code`' literal-backtick-pattern.txt"},
		{name: "git tag list", command: "git tag"},
		{name: "git tag list pattern", command: "git tag -l 'v*'"},
		{name: "find name", command: "find . -name '*.go'"},
		{name: "literal redirection character", command: "echo '>'"},
		{name: "double quoted regex backslash", command: `printf '%s\n' "foo\.bar"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadOnlyCommand(tt.command); err != nil {
				t.Fatalf("ReadOnlyCommand(%q) error = %v, want accepted", tt.command, err)
			}
		})
	}
}

func TestReadOnlyRunCommandRejectsUnsafeCommands(t *testing.T) {
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
		"find . -newer /etc/shadow",
		"find . -exec rm {} +",
		"find . -execdir rm {} +",
		"git diff -c diff.external=/tmp/lightcode-helper",
		"git log --config-env=core.pager=HOME -1",
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
			if _, err := ReadOnlyCommand(command); err == nil {
				t.Fatalf("ReadOnlyCommand(%q) accepted, want read-only rejection", command)
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
		{command: "find . -newer /etc/shadow", want: false},
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
		{command: "git diff -c diff.external=/tmp/lightcode-helper", want: false},
		{command: "git log --config-env=core.pager=HOME -1", want: false},
		{command: "git diff -- -c", want: true},
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

func TestReadOnlyRunCommandRejectsAmbiguousGitConfigFlags(t *testing.T) {
	for _, command := range []string{
		"git diff -c diff.external=/tmp/lightcode-helper",
		"git diff -cfoo.bar=/tmp/lightcode-helper",
		"git log --config-env=core.pager=HOME -1",
		"git show --config-env core.pager=HOME HEAD",
	} {
		t.Run(command, func(t *testing.T) {
			if isReadOnlyCommand(command) {
				t.Fatalf("isReadOnlyCommand(%q) = true, want false", command)
			}
		})
	}
	if !isReadOnlyCommand("git diff -- -c") {
		t.Fatal("isReadOnlyCommand(\"git diff -- -c\") = false, want true for literal path after --")
	}
}

func TestPR11Closure_GitSubcommandLocalDashCDeliberatelyBlocked(t *testing.T) {
	for _, command := range []string{
		"git diff -c HEAD",
		"git log -c HEAD",
		"git show -c HEAD",
	} {
		t.Run(command, func(t *testing.T) {
			if isReadOnlyCommand(command) {
				t.Fatalf("isReadOnlyCommand(%q) = true, want false (subcommand-local -c is deliberately blocked)", command)
			}
		})
	}
	if !isReadOnlyCommand("git diff -- -c") {
		t.Fatal("isReadOnlyCommand(\"git diff -- -c\") = false, want true (literal -- escape must still work)")
	}
}

func TestPR11Closure_ReadOnlyRejectsHeredocAndHereString(t *testing.T) {
	for _, command := range []string{
		"cat <<EOF",
		"cat <<<input",
		`grep pattern <<<"$var"`,
	} {
		t.Run(command, func(t *testing.T) {
			if isReadOnlyCommand(command) {
				t.Fatalf("isReadOnlyCommand(%q) = true, want false (heredoc/here-string redirection must be rejected)", command)
			}
		})
	}
}
