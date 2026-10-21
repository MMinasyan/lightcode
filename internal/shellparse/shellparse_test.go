package shellparse

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

func TestParseNormalizesShellArgv(t *testing.T) {
	segments, err := Parse(`git diff --out\put=/tmp/x && rg --pre\=cmd pattern .`)
	testassert.NoError(t, err)
	testassert.Equal(t, len(segments), 2)
	testassert.Equal(t, segments[0].Argv[2], "--output=/tmp/x")
	testassert.Equal(t, segments[1].Argv[1], "--pre=cmd")
}

func TestParseNormalizesShellLineContinuation(t *testing.T) {
	segments, err := Parse("r\\\nm -rf x")
	testassert.NoError(t, err)
	testassert.Require(t, len(segments) == 1 && segments[0].Text == "rm -rf x", "segments = %#v, want shell-effective rm segment", segments)

	segments, err = Parse("r\\\r\nm -rf x")
	testassert.NoError(t, err)
	testassert.Require(t, len(segments) == 1 && segments[0].Text == "rm -rf x", "CRLF segments = %#v, want shell-effective rm segment", segments)

	segments, err = Parse("echo \"a\\\nb\"")
	testassert.NoError(t, err)
	testassert.Require(t, len(segments) == 1 && len(segments[0].Argv) == 2 && segments[0].Argv[1] == "ab", "double-quoted line continuation segments = %#v, want ab arg", segments)
}

func TestParseDoubleQuotedBackslashSemantics(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{`echo "foo\.bar"`, `foo\.bar`},
		{`echo "\$HOME"`, "$HOME"},
		{`echo "\\path"`, `\path`},
		{`echo "\"quoted\""`, `"quoted"`},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			segments, err := Parse(tt.command)
			testassert.NoError(t, err)
			testassert.Require(t, len(segments) == 1 && len(segments[0].Argv) == 2 && segments[0].Argv[1] == tt.want, "segments = %#v, want second argv %q", segments, tt.want)
		})
	}
}

func TestParseAllowsLiteralSingleQuotedSubstitution(t *testing.T) {
	segments, err := Parse("echo '$(whoami)'")
	testassert.NoError(t, err)
	testassert.Require(t, len(segments) == 1 && len(segments[0].Argv) == 2 && segments[0].Argv[1] == "$(whoami)", "segments = %#v, want literal substitution arg", segments)
}

func TestParseRejectsExecutableSubstitution(t *testing.T) {
	for _, command := range []string{
		"echo $(whoami)",
		"echo `whoami`",
		`echo "$(whoami)"`,
		"echo \"`whoami`\"",
	} {
		t.Run(command, func(t *testing.T) {
			{
				_, err := Parse(command)
				testassert.Require(t, err != nil, "Parse returned nil error, want substitution rejection")
			}
		})
	}
}

func TestParseRedirections(t *testing.T) {
	for _, command := range []string{
		"echo ok > file",
		"echo ok >> file",
		"echo ok 1>file",
		"echo ok 2>err",
		"echo ok >&file",
		"echo ok &> file",
		"cat < file",
		"cat <<EOF",
		"echo ok 2>&1",
		"echo ok >&2",
		"echo ok 2>&-",
	} {
		t.Run(command, func(t *testing.T) {
			segments, err := Parse(command)
			testassert.NoError(t, err)
			testassert.Require(t, len(segments) == 1 && segments[0].HasRedirection, "segments = %#v, want one segment with HasRedirection=true", segments)
		})
	}
}

func TestParseQuotedMetacharactersAreLiteral(t *testing.T) {
	segments, err := Parse(`grep -E 'foo|bar' file && echo '>'`)
	testassert.NoError(t, err)
	testassert.Require(t, len(segments) == 2 && !segments[0].HasRedirection && !segments[1].HasRedirection, "got %v, want %v; segments = %#v, want no redirections", len(segments), 2, segments)
	testassert.Require(t, segments[0].Argv[2] == "foo|bar" && segments[1].Argv[1] == ">", "argv = %#v %#v, want quoted literals", segments[0].Argv, segments[1].Argv)
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
			testassert.NoError(t, err)
			testassert.Require(t, len(segments) == 1 && segments[0].UnsafeExpansion, "segments = %#v, want unsafe expansion", segments)
		})
	}
}

func TestPR11Closure_RedirectionTargetMarksOrRejectsSubstitution(t *testing.T) {
	t.Run("substitution_rejects", func(t *testing.T) {
		for _, command := range []string{
			"echo ok >$(id)",
			"echo ok > `id`",
		} {
			{
				_, err := Parse(command)
				testassert.Require(t, err != nil, "Parse(%q) returned nil error; want non-nil (redirection-target substitution must reject)", command)
			}
		}
	})

	t.Run("expansion_marks_unsafe", func(t *testing.T) {
		for _, command := range []string{
			"echo ok >$VAR",
			"echo ok >*",
			"echo ok >?",
			"echo ok >[abc]",
			"echo ok >~/file",
		} {
			segments, err := Parse(command)
			testassert.Require(t, err == nil, "Parse(%q) = error %v; want nil error + UnsafeExpansion=true", command, err)
			testassert.Require(t, len(segments) == 1 && segments[0].UnsafeExpansion, "Parse(%q) segments = %#v; want one segment with UnsafeExpansion=true", command, segments)
		}
	})

	t.Run("single_quoted_literal_is_safe", func(t *testing.T) {
		for _, command := range []string{
			"echo ok >'$VAR'",
			"echo ok >'$(id)'",
			"echo ok >'`id`'",
			"echo ok >'*'",
			"echo ok >'~'",
		} {
			segments, err := Parse(command)
			testassert.Require(t, err == nil, "Parse(%q) = error %v; want nil error (single-quoted target is literal)", command, err)
			testassert.Require(t, len(segments) == 1 && !segments[0].UnsafeExpansion && segments[0].HasRedirection, "Parse(%q) segments = %#v; want one segment with UnsafeExpansion=false; Parse(%q) segments = %#v; want HasRedirection=true", command, segments, command, segments)
		}
	})

	t.Run("double_quoted_substitution_rejected", func(t *testing.T) {
		for _, command := range []string{
			`echo ok >"$(id)"`,
			"echo ok >\"`id`\"",
		} {
			{
				_, err := Parse(command)
				testassert.Require(t, err != nil, "Parse(%q) returned nil error; want non-nil (double-quoted substitution must reject)", command)
			}
		}
	})

	t.Run("double_quoted_dollar_marks_unsafe", func(t *testing.T) {
		for _, command := range []string{
			`echo ok >"$VAR"`,
			`echo ok >"text $VAR"`,
		} {
			segments, err := Parse(command)
			testassert.Require(t, err == nil, "Parse(%q) = error %v; want nil error + UnsafeExpansion=true", command, err)
			testassert.Require(t, len(segments) == 1 && segments[0].UnsafeExpansion, "Parse(%q) segments = %#v; want one segment with UnsafeExpansion=true", command, segments)
		}
		// Glob chars inside double-quotes are literal — not unsafe.
		segments, err := Parse(`echo ok >"foo*"`)
		testassert.NoError(t, err)
		testassert.Require(t, len(segments) == 1 && !segments[0].UnsafeExpansion, `Parse "echo ok >\"foo*\"" segments = %#v; want UnsafeExpansion=false (literal inside double-quote)`, segments)
	})
}

func TestPR11Closure_PipeAmpersandIsSingleOperator(t *testing.T) {
	segments, err := Parse("echo a |& cat")
	testassert.NoError(t, err)
	testassert.Equal(t, len(segments), 2)
	testassert.Equal(t, segments[0].Separator, "|&")
}
