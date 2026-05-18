package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluateLiteralMatch(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/main.go)"},
	}

	d := Evaluate(rules, "write_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}
}

func TestEvaluateNoMatchDefaultsToAsk(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/main.go)"},
	}

	d := Evaluate(rules, "write_file", "/project/other.go", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate = %d, want DecisionAsk", d)
	}
}

func TestEvaluateGlobMatch(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/**/*.go)"},
	}

	d := Evaluate(rules, "write_file", "/project/src/pkg/util.go", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}

	// Outside the glob scope
	d = Evaluate(rules, "write_file", "/project/test/helper.go", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate outside scope = %d, want DecisionAsk", d)
	}
}

func TestEvaluateDenyPrecedenceOverAllow(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/**)"},
		Deny:  []string{"write_file(/src/secret/**)"},
	}

	// Should be denied — deny takes precedence over allow
	d := Evaluate(rules, "write_file", "/project/src/secret/keys.go", "/project", "/home/user", "/project")
	if d != DecisionDeny {
		t.Fatalf("Evaluate = %d, want DecisionDeny", d)
	}

	// Should be allowed — not in the deny scope
	d = Evaluate(rules, "write_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}
}

func TestEvaluateDenyPrecedenceOverAsk(t *testing.T) {
	rules := Rules{
		Ask:  []string{"write_file(/src/**)"},
		Deny: []string{"write_file(/src/secret/**)"},
	}

	d := Evaluate(rules, "write_file", "/project/src/secret/keys.go", "/project", "/home/user", "/project")
	if d != DecisionDeny {
		t.Fatalf("Evaluate = %d, want DecisionDeny (deny beats ask)", d)
	}
}

func TestEvaluateAskRules(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/**)"},
		Ask:   []string{"write_file(/tmp/**)"},
	}

	// Ask rule should win over allow rule for /tmp files
	d := Evaluate(rules, "write_file", "/project/tmp/build.go", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate = %d, want DecisionAsk", d)
	}
}

func TestEvaluateRunCommandExactMatch(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(npm run build)"},
	}

	d := Evaluate(rules, "run_command", "npm run build", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}

	// Different command
	d = Evaluate(rules, "run_command", "npm run test", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate different cmd = %d, want DecisionAsk", d)
	}
}

func TestEvaluateRunCommandWildcard(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(npm run *)"},
	}

	d := Evaluate(rules, "run_command", "npm run build", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}

	d = Evaluate(rules, "run_command", "npm run test", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}

	d = Evaluate(rules, "run_command", "rm -rf /", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate = %d, want DecisionAsk for unrelated command", d)
	}
}

func TestEvaluateRunCommandDenyBeatAllow(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(npm *)"},
		Deny:  []string{"run_command(npm run test)"},
	}

	d := Evaluate(rules, "run_command", "npm run test", "/project", "/home/user", "/project")
	if d != DecisionDeny {
		t.Fatalf("Evaluate = %d, want DecisionDeny", d)
	}

	d = Evaluate(rules, "run_command", "npm run build", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate = %d, want DecisionAllow", d)
	}
}

func TestEvaluateCompoundCommand(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(npm run *)"},
		Deny:  []string{"run_command(rm *)"},
	}

	// Mixed: one allow, one deny → deny wins
	d := Evaluate(rules, "run_command", "npm run build && rm -rf /tmp/junk", "/project", "/home/user", "/project")
	if d != DecisionDeny {
		t.Fatalf("Evaluate compound = %d, want DecisionDeny", d)
	}

	// All allowed
	d = Evaluate(rules, "run_command", "npm run build && npm run test", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Evaluate compound all-allow = %d, want DecisionAllow", d)
	}
}

func TestEvaluateCommandWithSubstitutionReturnsAsk(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(*)"},
	}

	// $() substitution should force ask
	d := Evaluate(rules, "run_command", "echo $(cat secret.txt)", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate substitution = %d, want DecisionAsk", d)
	}

	// backtick substitution should force ask
	d = Evaluate(rules, "run_command", "echo `cat secret.txt`", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate backtick = %d, want DecisionAsk", d)
	}
}

func TestEvaluateEmptyRulesDefaultsToAsk(t *testing.T) {
	d := Evaluate(Rules{}, "write_file", "/any/path", "/project", "/home", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate empty = %d, want DecisionAsk", d)
	}
}

func TestEvaluateDifferentToolNamesDontMatch(t *testing.T) {
	rules := Rules{
		Allow: []string{"write_file(/src/**)"},
	}

	d := Evaluate(rules, "read_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Evaluate = %d, want DecisionAsk (different tool)", d)
	}
}

func TestCheckLocalOverridesGlobal(t *testing.T) {
	local := Rules{
		Deny: []string{"write_file(/src/**)"},
	}
	global := Rules{
		Allow: []string{"write_file(/src/**)"},
	}

	// Local deny should win
	d := Check(local, global, "write_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionDeny {
		t.Fatalf("Check = %d, want DecisionDeny", d)
	}
}

func TestCheckFallsThroughToGlobal(t *testing.T) {
	local := Rules{} // no matching rules
	global := Rules{
		Allow: []string{"write_file(/src/**)"},
	}

	d := Check(local, global, "write_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionAllow {
		t.Fatalf("Check = %d, want DecisionAllow", d)
	}
}

func TestCheckLocalExplicitAskPreventsGlobal(t *testing.T) {
	local := Rules{
		Ask: []string{"write_file(/src/**)"},
	}
	global := Rules{
		Allow: []string{"write_file(/src/**)"},
	}

	// Local explicitly asks → should not fall through to global allow
	d := Check(local, global, "write_file", "/project/src/main.go", "/project", "/home/user", "/project")
	if d != DecisionAsk {
		t.Fatalf("Check = %d, want DecisionAsk", d)
	}
}

func TestCheckCanonicalProjectRootLocalDenyOverridesGlobalAllow(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realRoot, "src", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	local := Rules{Deny: []string{"read_file(/src/secret.txt)"}}
	global := Rules{Allow: []string{"read_file(/src/**)"}}

	d := Check(local, global, "read_file", path, linkRoot, linkRoot, linkRoot)
	if d != DecisionDeny {
		t.Fatalf("Check = %d, want DecisionDeny", d)
	}
}

func TestCheckCanonicalProjectRootExplicitAskBlocksGlobalAllow(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkRoot, "src", "file.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	local := Rules{Ask: []string{"read_file(/src/file.txt)"}}
	global := Rules{Allow: []string{"read_file(/src/**)"}}

	d := Check(local, global, "read_file", path, linkRoot, linkRoot, linkRoot)
	if d != DecisionAsk {
		t.Fatalf("Check = %d, want DecisionAsk", d)
	}
}

func TestEvaluateProjectRuleDoesNotAllowSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	allowedDir := filepath.Join(root, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(allowedDir, "link")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(linkDir, "new.txt")

	d := Evaluate(Rules{
		Allow: []string{"write_file(/allowed/**)"},
	}, "write_file", requested, root, root, root)
	if d != DecisionAsk {
		t.Fatalf("Evaluate = %d, want DecisionAsk", d)
	}
}

func TestCheckBothEmptyDefaultsToAsk(t *testing.T) {
	d := Check(Rules{}, Rules{}, "write_file", "/any", "/project", "/home", "/project")
	if d != DecisionAsk {
		t.Fatalf("Check = %d, want DecisionAsk", d)
	}
}

func TestParseRuleValid(t *testing.T) {
	r, err := parseRule("write_file(/src/**)")
	if err != nil {
		t.Fatal(err)
	}
	if r.tool != "write_file" || r.pattern != "/src/**" {
		t.Fatalf("parseRule = %+v", r)
	}
}

func TestParseRuleInvalid(t *testing.T) {
	_, err := parseRule("no-parens")
	if err == nil {
		t.Fatal("parseRule should fail for rule without parens")
	}

	_, err = parseRule("()")
	if err == nil {
		t.Fatal("parseRule should fail for empty tool name")
	}

	_, err = parseRule("tool(")
	if err == nil {
		t.Fatal("parseRule should fail for unclosed paren")
	}
}

func TestDecomposeCommandSimple(t *testing.T) {
	parts, err := DecomposeCommand("ls -la")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "ls -la" {
		t.Fatalf("DecomposeCommand = %v", parts)
	}
}

func TestDecomposeCommandCompound(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		{"a && b", []string{"a", "b"}},
		{"a || b", []string{"a", "b"}},
		{"a ; b", []string{"a", "b"}},
		{"a | b", []string{"a", "b"}},
		{"a && b || c ; d | e", []string{"a", "b", "c", "d", "e"}},
	}
	for _, tt := range tests {
		got, err := DecomposeCommand(tt.cmd)
		if err != nil {
			t.Fatalf("DecomposeCommand(%q): %v", tt.cmd, err)
		}
		if len(got) != len(tt.want) {
			t.Fatalf("DecomposeCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("DecomposeCommand(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.want[i])
			}
		}
	}
}

func TestDecomposeCommandRejectsSubstitution(t *testing.T) {
	_, err := DecomposeCommand("echo $(whoami)")
	if err == nil {
		t.Fatal("DecomposeCommand should reject $()")
	}

	_, err = DecomposeCommand("echo `whoami`")
	if err == nil {
		t.Fatal("DecomposeCommand should reject backticks")
	}
}

func TestDecomposeCommandQuotedSemicolon(t *testing.T) {
	parts, err := DecomposeCommand(`echo "hello; world"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("quoted semicolon should not split, got %v", parts)
	}
}

func TestDecomposeCommandSingleQuotedSubstitution(t *testing.T) {
	// Command substitution inside single quotes is allowed (it's literal)
	parts, err := DecomposeCommand(`echo '$(whoami)'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "echo '$(whoami)'" {
		t.Fatalf("got %v", parts)
	}
}

func TestMatchGlobSimple(t *testing.T) {
	// matchGlob compares resolved absolute patterns against absolute paths.
	// After resolvePath, "/src/main.go" (project-relative) becomes "/project/src/main.go".
	if !matchGlob("/project/src/main.go", "/project/src/main.go") {
		t.Fatal("exact glob should match")
	}
	if matchGlob("/project/src/*.txt", "/project/src/main.go") {
		t.Fatal("glob should not match .go vs .txt")
	}
	if !matchGlob("/project/src/*.go", "/project/src/main.go") {
		t.Fatal("glob should match *.go vs main.go")
	}
}

func TestMatchGlobDoubleStar(t *testing.T) {
	if !matchGlob("/project/src/**", "/project/src/deep/nested/file.go") {
		t.Fatal("** should match deep paths")
	}
	if !matchGlob("/project/src/**", "/project/src/file.go") {
		t.Fatal("** should match single level")
	}
}

func TestMatchGlobQuestionMark(t *testing.T) {
	if !matchGlob("/project/src/f?le.go", "/project/src/file.go") {
		t.Fatal("? should match single char")
	}
	if matchGlob("/project/src/f?le.go", "/project/src/foole.go") {
		t.Fatal("? should not match two chars")
	}
}

func TestMatchCommandExact(t *testing.T) {
	if !matchCommand("npm run build", "npm run build") {
		t.Fatal("exact command should match")
	}
	if matchCommand("npm run build", "npm run test") {
		t.Fatal("different commands should not match")
	}
}

func TestMatchCommandWildcard(t *testing.T) {
	if !matchCommand("npm run *", "npm run build") {
		t.Fatal("wildcard should match")
	}
	if !matchCommand("npm run *", "npm run test") {
		t.Fatal("wildcard should match")
	}
	if matchCommand("npm run *", "yarn run build") {
		t.Fatal("wildcard should not match different prefix")
	}
	if !matchCommand("*", "anything") {
		t.Fatal("* should match anything")
	}
}

func TestResolvePathAbsolute(t *testing.T) {
	got := resolvePath("//etc/passwd", "/proj", "/home", "/cwd")
	if got != "/etc/passwd" {
		t.Fatalf("resolvePath(//etc/passwd) = %q, want /etc/passwd", got)
	}
}

func TestResolvePathHomeRelative(t *testing.T) {
	got := resolvePath("~/file.go", "/proj", "/home/user", "/cwd")
	if got != "/home/user/file.go" {
		t.Fatalf("resolvePath(~/file.go) = %q", got)
	}
}

func TestResolvePathProjectRelative(t *testing.T) {
	got := resolvePath("/src/main.go", "/proj", "/home", "/cwd")
	if got != "/proj/src/main.go" {
		t.Fatalf("resolvePath(/src/main.go) = %q", got)
	}
}

func TestResolvePathCwdRelative(t *testing.T) {
	got := resolvePath("src/main.go", "/proj", "/home", "/cwd")
	if got != "/cwd/src/main.go" {
		t.Fatalf("resolvePath(src/main.go) = %q", got)
	}
}

func TestIsSensitivePath(t *testing.T) {
	sensitive := []string{
		"/home/user/.env",
		"/home/user/.netrc",
		"/home/user/.ssh/id_rsa",
		"/home/user/cert.pem",
		"/home/user/key.key",
		"/home/user/credentials.json",
		"/home/user/credentials.yaml",
	}
	for _, p := range sensitive {
		if !isSensitivePath(p) {
			t.Fatalf("isSensitivePath(%q) = false, want true", p)
		}
	}

	normal := []string{
		"/home/user/README.md",
		"/home/user/main.go",
		"/home/user/.gitignore",
	}
	for _, p := range normal {
		if isSensitivePath(p) {
			t.Fatalf("isSensitivePath(%q) = true, want false", p)
		}
	}
}
