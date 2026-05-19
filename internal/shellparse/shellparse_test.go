package shellparse

import "testing"

func TestParseNormalizesShellArgv(t *testing.T) {
	segments, err := Parse(`git diff --out\put=/tmp/x && rg --pre\=cmd pattern .`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	if got := segments[0].Argv[2]; got != "--output=/tmp/x" {
		t.Fatalf("git arg = %q, want normalized --output=/tmp/x", got)
	}
	if got := segments[1].Argv[1]; got != "--pre=cmd" {
		t.Fatalf("rg arg = %q, want normalized --pre=cmd", got)
	}
}

func TestParseAllowsLiteralSingleQuotedSubstitution(t *testing.T) {
	segments, err := Parse("echo '$(whoami)'")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || len(segments[0].Argv) != 2 || segments[0].Argv[1] != "$(whoami)" {
		t.Fatalf("segments = %#v, want literal substitution arg", segments)
	}
}

func TestParseRejectsExecutableSubstitution(t *testing.T) {
	for _, command := range []string{
		"echo $(whoami)",
		"echo `whoami`",
		`echo "$(whoami)"`,
		"echo \"`whoami`\"",
	} {
		t.Run(command, func(t *testing.T) {
			if _, err := Parse(command); err == nil {
				t.Fatal("Parse returned nil error, want substitution rejection")
			}
		})
	}
}

func TestParseRedirections(t *testing.T) {
	tests := []struct {
		command string
		safe    bool
	}{
		{"echo ok > file", false},
		{"echo ok >> file", false},
		{"echo ok 1>file", false},
		{"echo ok 2>err", false},
		{"echo ok >&file", false},
		{"echo ok &> file", false},
		{"cat < file", false},
		{"cat <<EOF", false},
		{"echo ok 2>&1", true},
		{"echo ok >&2", true},
		{"echo ok 2>&-", true},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			segments, err := Parse(tt.command)
			if err != nil {
				t.Fatal(err)
			}
			if len(segments) != 1 || len(segments[0].Redirections) != 1 {
				t.Fatalf("segments = %#v, want one redirection", segments)
			}
			if got := segments[0].Redirections[0].SafeFDDup; got != tt.safe {
				t.Fatalf("SafeFDDup = %v, want %v", got, tt.safe)
			}
		})
	}
}

func TestParseQuotedMetacharactersAreLiteral(t *testing.T) {
	segments, err := Parse(`grep -E 'foo|bar' file && echo '>'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	if len(segments[0].Redirections) != 0 || len(segments[1].Redirections) != 0 {
		t.Fatalf("segments = %#v, want no redirections", segments)
	}
	if segments[0].Argv[2] != "foo|bar" || segments[1].Argv[1] != ">" {
		t.Fatalf("argv = %#v %#v, want quoted literals", segments[0].Argv, segments[1].Argv)
	}
}

func TestParseMarksUnsafeExpansions(t *testing.T) {
	for _, command := range []string{
		"echo *",
		"echo ?",
		"echo [abc]",
		"echo {a,b}",
		"echo $HOME",
		"echo ~/file",
	} {
		t.Run(command, func(t *testing.T) {
			segments, err := Parse(command)
			if err != nil {
				t.Fatal(err)
			}
			if len(segments) != 1 || !segments[0].UnsafeExpansion {
				t.Fatalf("segments = %#v, want unsafe expansion", segments)
			}
		})
	}
}
