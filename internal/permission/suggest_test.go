package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuggestRunCommandBash(t *testing.T) {
	suggestions := Suggest("run_command", "bash -c 'echo hello'", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions for bash command")
	}

	// First suggestion should be the exact command
	if suggestions[0].Rule != "run_command(bash -c 'echo hello')" {
		t.Fatalf("first suggestion = %q", suggestions[0].Rule)
	}
	if got := Evaluate(Rules{Allow: []string{suggestions[0].Rule}}, "run_command", "bash -c 'echo hello'", "/project", "/home/user", "/project"); got != DecisionAllow {
		t.Fatalf("Evaluate with first suggestion = %d, want DecisionAllow", got)
	}

	// Should have progressive wildcards
	lastIdx := len(suggestions) - 1
	if suggestions[lastIdx].Rule != "run_command(bash *)" {
		t.Fatalf("last suggestion = %q, want run_command(bash *)", suggestions[lastIdx].Rule)
	}
}

func TestSuggestRunCommandSimple(t *testing.T) {
	suggestions := Suggest("run_command", "npm run build", "/project")
	if len(suggestions) < 2 {
		t.Fatalf("expected at least 2 suggestions, got %d", len(suggestions))
	}

	// Exact match
	if suggestions[0].Rule != "run_command(npm run build)" {
		t.Fatalf("first = %q", suggestions[0].Rule)
	}
	if suggestions[0].Label != "npm run build" {
		t.Fatalf("first label = %q", suggestions[0].Label)
	}

	// Progressive: "npm run *"
	if suggestions[1].Rule != "run_command(npm run *)" {
		t.Fatalf("second = %q", suggestions[1].Rule)
	}

	// Progressive: "npm *"
	if suggestions[2].Rule != "run_command(npm *)" {
		t.Fatalf("third = %q", suggestions[2].Rule)
	}
}

func TestSuggestRunCommandSingleWord(t *testing.T) {
	suggestions := Suggest("run_command", "ls", "/project")
	if len(suggestions) != 1 {
		t.Fatalf("expected 1 suggestion for single-word command, got %d", len(suggestions))
	}
	if suggestions[0].Rule != "run_command(ls)" {
		t.Fatalf("suggestion = %q", suggestions[0].Rule)
	}
}

func TestSuggestEditFile(t *testing.T) {
	suggestions := Suggest("edit_file", "/project/src/main.go", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions for edit_file")
	}

	// First: exact file
	if suggestions[0].Rule != "edit_file(/src/main.go)" {
		t.Fatalf("first = %q", suggestions[0].Rule)
	}

	// Should have wildcard for directory
	found := false
	for _, s := range suggestions {
		if s.Rule == "edit_file(/src/*)" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no /src/* suggestion found")
	}

	// Should end with **
	lastIdx := len(suggestions) - 1
	if suggestions[lastIdx].Rule != "edit_file(/**)" {
		t.Fatalf("last = %q, want edit_file(/**)", suggestions[lastIdx].Rule)
	}
}

func TestSuggestWriteFile(t *testing.T) {
	suggestions := Suggest("write_file", "/project/src/pkg/util.go", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions for write_file")
	}

	// Exact match
	if suggestions[0].Rule != "write_file(/src/pkg/util.go)" {
		t.Fatalf("first = %q", suggestions[0].Rule)
	}

	// Should have src/pkg/* and src/pkg/**
	found := false
	for _, s := range suggestions {
		if s.Rule == "write_file(/src/pkg/*)" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no write_file(/src/pkg/*) suggestion")
	}
}

func TestSuggestReadFile(t *testing.T) {
	suggestions := Suggest("read_file", "/project/README.md", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions for read_file")
	}

	if suggestions[0].Rule != "read_file(/README.md)" {
		t.Fatalf("first = %q", suggestions[0].Rule)
	}
}

func TestSuggestCanonicalProjectRootForResolvedTarget(t *testing.T) {
	tmp := t.TempDir()
	realRoot := filepath.Join(tmp, "real-project")
	linkRoot := filepath.Join(tmp, "link-project")
	target := filepath.Join(realRoot, "src", "file.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}

	suggestions := Suggest("read_file", target, linkRoot)
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions")
	}
	if suggestions[0].Rule != "read_file(/src/file.go)" {
		t.Fatalf("first = %q, want read_file(/src/file.go)", suggestions[0].Rule)
	}
	found := false
	for _, suggestion := range suggestions {
		if suggestion.Rule == "read_file(/src/**)" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("suggestions = %+v, want project-relative wildcard", suggestions)
	}
}

func TestSuggestVisibleProjectRootStillWorks(t *testing.T) {
	tmp := t.TempDir()
	realRoot := filepath.Join(tmp, "real-project")
	linkRoot := filepath.Join(tmp, "link-project")
	if err := os.MkdirAll(filepath.Join(realRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	visiblePath := filepath.Join(linkRoot, "src", "file.go")

	suggestions := Suggest("write_file", visiblePath, linkRoot)
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions")
	}
	if suggestions[0].Rule != "write_file(/src/file.go)" {
		t.Fatalf("first = %q, want write_file(/src/file.go)", suggestions[0].Rule)
	}
}

func TestSuggestHomeRelativePath(t *testing.T) {
	// Use a path under the actual home directory (os.UserHomeDir())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	// Use a file that is under home but not under a project root
	absPath := filepath.Join(home, "myfile.go")
	suggestions := Suggest("write_file", absPath, "/other/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions")
	}

	// Should use ~/ prefix since the file is under home but not under projectRoot
	if suggestions[0].Rule != "write_file(~/myfile.go)" {
		t.Fatalf("first = %q, want write_file(~/myfile.go)", suggestions[0].Rule)
	}
}

func TestSuggestAbsoluteExternalPath(t *testing.T) {
	suggestions := Suggest("write_file", "/etc/nginx/nginx.conf", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions")
	}

	// filePrefix returns "//" for absolute paths outside project and home,
	// and rel is the full absolute path. The rule becomes "//" + "/etc/..." = "///etc/..."
	// resolvePath("///etc/...", ...) strips the first "/" from "//" → "/etc/..."
	if suggestions[0].Rule != "write_file(///etc/nginx/nginx.conf)" {
		t.Fatalf("first = %q, want write_file(///etc/nginx/nginx.conf)", suggestions[0].Rule)
	}
}

func TestSuggestStabilityIdenticalInputs(t *testing.T) {
	inputs := []struct {
		toolName string
		arg      string
		root     string
	}{
		{"write_file", "/project/src/main.go", "/project"},
		{"run_command", "npm run build", "/project"},
		{"edit_file", "/project/src/pkg/util.go", "/project"},
		{"read_file", "/project/README.md", "/project"},
	}

	for _, input := range inputs {
		s1 := Suggest(input.toolName, input.arg, input.root)
		s2 := Suggest(input.toolName, input.arg, input.root)

		if len(s1) != len(s2) {
			t.Fatalf("Suggest(%q,%q,%q) len mismatch: %d vs %d", input.toolName, input.arg, input.root, len(s1), len(s2))
		}
		for i := range s1 {
			if s1[i].Rule != s2[i].Rule || s1[i].Label != s2[i].Label {
				t.Fatalf("Suggest(%q,%q) not stable at index %d: %+v vs %+v",
					input.toolName, input.arg, i, s1[i], s2[i])
			}
		}
	}
}

func TestSuggestRunCommandNoDuplicates(t *testing.T) {
	suggestions := Suggest("run_command", "npm run test", "/project")
	seen := make(map[string]bool)
	for _, s := range suggestions {
		if seen[s.Rule] {
			t.Fatalf("duplicate suggestion: %q", s.Rule)
		}
		seen[s.Rule] = true
	}
}

func TestSuggestForSubcommandsSimple(t *testing.T) {
	groups := SuggestForSubcommands("ls -la")
	if len(groups) != 1 {
		t.Fatalf("expected 1 group for single command, got %d", len(groups))
	}
	if len(groups[0]) == 0 {
		t.Fatal("expected suggestions in the group")
	}
}

func TestSuggestForSubcommandsCompound(t *testing.T) {
	groups := SuggestForSubcommands("npm run build && npm run test")
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups for compound command, got %d", len(groups))
	}
	if len(groups[0]) == 0 || len(groups[1]) == 0 {
		t.Fatal("both groups should have suggestions")
	}
}

func TestSuggestForSubcommandsWithDeny(t *testing.T) {
	groups := SuggestForSubcommands("npm run build && rm -rf /tmp/junk")
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	// First group: npm run build
	if groups[0][0].Rule != "run_command(npm run build)" {
		t.Fatalf("first group first = %q", groups[0][0].Rule)
	}
	// Second group: rm -rf /tmp/junk
	if groups[1][0].Rule != "run_command(rm -rf /tmp/junk)" {
		t.Fatalf("second group first = %q", groups[1][0].Rule)
	}
}

func TestSuggestRunCommandEmptyCommand(t *testing.T) {
	suggestions := Suggest("run_command", "", "/project")
	if suggestions != nil {
		t.Fatalf("expected nil for empty command, got %v", suggestions)
	}
}

func TestSuggestRunCommandWhitespaceOnly(t *testing.T) {
	suggestions := Suggest("run_command", "   ", "/project")
	if suggestions != nil {
		t.Fatalf("expected nil for whitespace command, got %v", suggestions)
	}
}

func TestSuggestFileWithSubdirectory(t *testing.T) {
	suggestions := Suggest("write_file", "/project/src/internal/pkg/helper.go", "/project")
	if len(suggestions) == 0 {
		t.Fatal("Suggest returned no suggestions")
	}

	// Should have intermediate directory wildcards
	foundRules := make(map[string]bool)
	for _, s := range suggestions {
		foundRules[s.Rule] = true
	}

	expected := []string{
		"write_file(/src/internal/pkg/helper.go)",
		"write_file(/src/internal/pkg/*)",
		"write_file(/src/internal/pkg/**)",
		"write_file(/src/internal/*)",
		"write_file(/src/internal/**)",
		"write_file(/src/*)",
		"write_file(/src/**)",
		"write_file(/**)",
	}
	for _, e := range expected {
		if !foundRules[e] {
			t.Fatalf("missing suggestion: %q", e)
		}
	}
}

// TestPR11Closure_ProcessSuggestRoundTrip asserts that every suggestion
// emitted for a `process` permission prompt, when saved as an allow rule,
// round-trips through the matcher to DecisionAllow for the original arg.
// The prior implementation routed `process` through the file-suggestion
// path, producing path-style rules the direct-glob process matcher could
// not match — breaking the "Allow for project" UX entirely for the
// process tool.
func TestPR11Closure_ProcessSuggestRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		wantHas []string
	}{
		{
			name:    "id_arg_has_exact_and_wildcard",
			arg:     "process:abc123",
			wantHas: []string{"process(process:abc123)", "process(process:*)"},
		},
		{
			name:    "bare_arg_has_literal",
			arg:     "process",
			wantHas: []string{"process(process)"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			suggestions := Suggest("process", tc.arg, "/project")
			if len(suggestions) == 0 {
				t.Fatalf("Suggest(process, %q) returned no suggestions", tc.arg)
			}

			rules := make(map[string]bool)
			for _, s := range suggestions {
				rules[s.Rule] = true
			}
			for _, want := range tc.wantHas {
				if !rules[want] {
					t.Fatalf("Suggest(process, %q) missing %q; got %v", tc.arg, want, suggestions)
				}
			}

			// Each suggestion must round-trip: saving it as an Allow rule
			// must produce DecisionAllow for the original arg.
			for _, s := range suggestions {
				d := Evaluate(Rules{Allow: []string{s.Rule}}, "process", tc.arg,
					"/project", "/home/user", "/project")
				if d != DecisionAllow {
					t.Fatalf("Evaluate with saved suggestion %q against arg %q = %d, want DecisionAllow",
						s.Rule, tc.arg, d)
				}
			}
		})
	}
}
