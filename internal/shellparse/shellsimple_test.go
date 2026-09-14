package shellparse

import "testing"

// TestParseSimpleGrammar pins the restricted simple-command grammar: every
// conclusive row decomposes into per-segment source-faithful Text targets,
// and every row outside the grammar falls back with no partial segment list.
// The fallback's canonical target text (leading-blank removal and the
// complete-original branch) is the plugin's rule and is pinned there.
func TestParseSimpleGrammar(t *testing.T) {
	cases := []struct {
		name    string
		command string
		text    []string // conclusive expectation; nil means fallback
	}{
		// Single simple commands.
		{name: "single", command: "echo hi", text: []string{"echo hi"}},
		{name: "leading blanks", command: "  \t echo hi", text: []string{"echo hi"}},
		{name: "trailing blanks", command: "echo hi  ", text: []string{"echo hi"}},
		{name: "internal blanks preserved", command: "echo   a   b", text: []string{"echo   a   b"}},
		{name: "empty argv word", command: "echo ''", text: []string{"echo ''"}},

		// Joins.
		{name: "semicolon join", command: "echo one; echo two", text: []string{"echo one", "echo two"}},
		{name: "and join", command: "echo one && echo two", text: []string{"echo one", "echo two"}},
		{name: "or join", command: "echo one || echo two", text: []string{"echo one", "echo two"}},
		{name: "pipe join", command: "echo one | cat", text: []string{"echo one", "cat"}},
		{name: "pipe_amp join", command: "echo one |& cat", text: []string{"echo one", "cat"}},
		{name: "newline join", command: "echo one\necho two", text: []string{"echo one", "echo two"}},
		{name: "mixed joins", command: "echo a; echo b && echo c || echo d | cat", text: []string{"echo a", "echo b", "echo c", "echo d", "cat"}},

		// Blank lines and final separators.
		{name: "blank lines ignored", command: "\n\necho a\n\necho b\n\n", text: []string{"echo a", "echo b"}},
		{name: "newline after pending operator", command: "echo a &&\necho b", text: []string{"echo a", "echo b"}},
		{name: "newline after pipe operator", command: "echo a |\necho b", text: []string{"echo a", "echo b"}},
		{name: "final semicolon", command: "echo a;", text: []string{"echo a"}},
		{name: "final semicolon and newline", command: "echo a;\n", text: []string{"echo a"}},
		{name: "final newline", command: "echo a\n", text: []string{"echo a"}},

		// Comments: only unquoted word-initial # falls back.
		{name: "word-initial comment", command: "echo a #b", text: nil},
		{name: "comment-only", command: "# just a comment", text: nil},
		{name: "comment after separator", command: "echo a; #b", text: nil},
		{name: "literal hash mid-word", command: "echo a#b", text: []string{"echo a#b"}},
		{name: "quoted hash", command: "echo '#b'", text: []string{"echo '#b'"}},
		{name: "escaped hash", command: "echo \\#b", text: []string{"echo \\#b"}},

		// Reserved words versus quoted or argument-position spellings.
		{name: "reserved if", command: "if", text: nil},
		{name: "reserved time prefix", command: "time ls", text: nil},
		{name: "reserved negation", command: "! true", text: nil},
		{name: "reserved in for-loop shape", command: "for i in 1 2 3; do echo $i; done", text: nil},
		{name: "reserved word as argument", command: "echo if then done", text: []string{"echo if then done"}},
		{name: "quoted reserved command name", command: `"if"`, text: []string{`"if"`}},
		{name: "escaped reserved command name", command: `\\if`, text: []string{`\\if`}},
		{name: "quoted command with reserved spelling", command: `'while' loop`, text: []string{`'while' loop`}},

		// Assignment prefixes.
		{name: "assignment then command", command: "FOO=bar ls", text: nil},
		{name: "bare assignment", command: "FOO=bar", text: nil},
		{name: "assignment quoted value", command: "FOO=bar'baz' ls", text: nil},
		{name: "assignment as argument", command: "echo FOO=bar", text: []string{"echo FOO=bar"}},
		{name: "quoted name assignment shape", command: `"FOO"=bar ls`, text: []string{`"FOO"=bar ls`}},
		{name: "equals-first command word", command: "=bar", text: []string{"=bar"}},

		// Source-token boundaries: quoted and escaped trailing whitespace.
		{name: "quoted trailing space", command: "echo 'a '", text: []string{"echo 'a '"}},
		{name: "escaped trailing space", command: "echo a\\ ", text: []string{"echo a\\ "}},
		{name: "quoted internal newline", command: "echo 'a\nb'", text: []string{"echo 'a\nb'"}},

		// Backslash-LF continuation versus carriage returns.
		{name: "backslash-LF continuation", command: "echo a\\\nb", text: []string{"echo ab"}},
		{name: "backslash-LF between words", command: "echo \\\n hi", text: []string{"echo  hi"}},
		{name: "backslash-CR", command: "echo a\\\rb", text: nil},
		{name: "backslash-CRLF", command: "echo a\\\r\nb", text: nil},
		{name: "carriage return", command: "echo a\rb", text: nil},
		{name: "unicode whitespace", command: "echo a\u00a0b", text: nil},

		// Dangling operators and empty commands.
		{name: "dangling and", command: "echo a &&", text: nil},
		{name: "dangling or", command: "echo a ||", text: nil},
		{name: "dangling pipe", command: "echo a |", text: nil},
		{name: "dangling pipe_amp", command: "echo a |&", text: nil},
		{name: "dangling after newline", command: "echo a &&\n", text: nil},
		{name: "empty semicolon command", command: "echo a;;echo b", text: nil},
		{name: "leading semicolon", command: ";echo a", text: nil},
		{name: "semicolon blank semicolon", command: "echo a; ; echo b", text: nil},
		{name: "newline then and", command: "echo a\n&& echo b", text: nil},
		{name: "bare ampersand", command: "echo a & echo b", text: nil},
		{name: "ampersand redirect", command: "echo a &> b", text: nil},

		// Redirections, heredocs, groups, functions, expansions.
		{name: "stdout redirect", command: "echo a > b", text: nil},
		{name: "append redirect", command: "echo a >> b", text: nil},
		{name: "stdin redirect", command: "cat < f", text: nil},
		{name: "fd redirect", command: "echo a 2>&1", text: nil},
		{name: "heredoc", command: "cat <<EOF\nbody\nEOF", text: nil},
		{name: "here-string", command: "cat <<< x", text: nil},
		{name: "function definition", command: "f() { :; }", text: nil},
		{name: "subshell group", command: "(echo a)", text: nil},
		{name: "brace group", command: "{ echo a; }", text: nil},
		{name: "command substitution", command: "echo $(x)", text: nil},
		{name: "backtick substitution", command: "echo `x`", text: nil},
		{name: "variable expansion", command: "echo $HOME", text: nil},
		{name: "double-quoted expansion", command: `echo "$HOME"`, text: nil},
		{name: "glob", command: "echo *.go", text: nil},
		{name: "brace expansion", command: "echo {a,b}", text: nil},
		{name: "subshell after join", command: "echo a; (echo b)", text: nil},

		// The canonical fallback case: a comment swallows the tail, so the
		// only target is the complete text — never an invented rm segment.
		{name: "comment swallows separator", command: "printf ok # ; rm -rf scratch", text: nil},

		// Blank and all-whitespace inputs contain no command.
		{name: "empty", command: "", text: nil},
		{name: "blank", command: "   ", text: nil},
		{name: "newlines only", command: "\n\n", text: nil},

		// Malformed inputs.
		{name: "unterminated single quote", command: "echo 'a", text: nil},
		{name: "unterminated double quote", command: `echo "a`, text: nil},
		{name: "trailing backslash", command: "echo a\\", text: nil},
	}

	for _, tc := range cases {
		segments, ok := ParseSimple(tc.command)
		if ok != (tc.text != nil) {
			t.Errorf("%s: ParseSimple(%q) ok=%v, want %v", tc.name, tc.command, ok, tc.text != nil)
			continue
		}
		if !ok {
			if segments != nil {
				t.Errorf("%s: fallback returned a partial segment list %q", tc.name, segmentTexts(segments))
			}
			continue
		}
		if len(segments) != len(tc.text) {
			t.Errorf("%s: ParseSimple(%q) = %q, want %q", tc.name, tc.command, segmentTexts(segments), tc.text)
			continue
		}
		for i := range segments {
			if segments[i].Text != tc.text[i] {
				t.Errorf("%s: ParseSimple(%q)[%d].Text = %q, want %q", tc.name, tc.command, i, segments[i].Text, tc.text[i])
			}
			if segments[i].UnsafeExpansion || len(segments[i].Redirections) > 0 {
				t.Errorf("%s: ParseSimple(%q)[%d] carries grammar markers outside the simple-command shape", tc.name, tc.command, i)
			}
		}
	}
}

func segmentTexts(segments []Segment) []string {
	texts := make([]string, 0, len(segments))
	for _, s := range segments {
		texts = append(texts, s.Text)
	}
	return texts
}
