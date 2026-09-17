package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// isBuiltInOnly reports whether a resolved policy carries no user-level state
// and therefore evaluates exactly as the built-in policy.
func isBuiltInOnly(p PermissionPolicy) bool {
	return reflect.DeepEqual(p, PermissionPolicy{})
}

// ruleJSON renders one permission rule object for policy fixtures.
func ruleJSON(permission, target, access string) string {
	b, err := json.Marshal(map[string]string{
		"permission": permission,
		"target":     target,
		"access":     access,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// levelJSON renders a permission block with the given members.
func levelJSON(members ...string) string {
	return "{" + strings.Join(members, ",") + "}"
}

func rulesJSON(rules ...string) string {
	return `"rules":[` + strings.Join(rules, ",") + `]`
}

func TestResolvePermissionPolicyFallbackChain(t *testing.T) {
	broad := ruleJSON(permissionFileRead, "/w/*", permissionAccessDeny)
	narrow := ruleJSON(permissionFileRead, "/w/sub/*", permissionAccessAllow)
	countered := ruleJSON(permissionFileRead, "/w/sub/a.txt", permissionAccessDeny)
	policy := func(global, workspace string) PermissionPolicy {
		t.Helper()
		var g, w json.RawMessage
		if global != "" {
			g = json.RawMessage(global)
		}
		if workspace != "" {
			w = json.RawMessage(workspace)
		}
		return ResolvePermissionPolicy(g, w)
	}
	tests := []struct {
		name    string
		global  string
		workspe string
		root    string
		req     PermissionRequest
		want    bool
	}{
		{"absent levels reach built-in in-root allow", "", "", "/w",
			PermissionRequest{permissionFileRead, "/w/a.txt"}, true},
		{"absent levels reach built-in outside-root deny", "", "", "/w",
			PermissionRequest{permissionFileRead, "/etc/passwd"}, false},
		{"last matching workspace rule decides over earlier siblings", "", levelJSON(rulesJSON(broad, narrow)), "/w",
			PermissionRequest{permissionFileRead, "/w/sub/a.txt"}, true},
		{"last matching workspace rule decides under a later counter-exception", "",
			levelJSON(rulesJSON(broad, narrow, countered)), "/w",
			PermissionRequest{permissionFileRead, "/w/sub/a.txt"}, false},
		{"last matching global rule decides without any workspace level", levelJSON(rulesJSON(broad, narrow)), "", "/w",
			PermissionRequest{permissionFileRead, "/w/sub/a.txt"}, true},
		{"workspace rule overrides an opposite global rule",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/*", permissionAccessAllow))),
			levelJSON(rulesJSON(broad)), "/w",
			PermissionRequest{permissionFileRead, "/w/a.txt"}, false},
		{"workspace match denies even when the workspace default allows", "",
			levelJSON(`"default":"allow"`, rulesJSON(broad)), "/w",
			PermissionRequest{permissionFileRead, "/w/a.txt"}, false},
		{"workspace default applies when no workspace rule matches", "",
			levelJSON(`"default":"deny"`, rulesJSON(ruleJSON(permissionCommandRun, "*", permissionAccessAllow))), "/w",
			PermissionRequest{permissionFileRead, "/w/a.txt"}, false},
		{"a matching workspace rule outranks a denying workspace default and a denying global",
			levelJSON(`"default":"deny"`), levelJSON(`"default":"deny"`, rulesJSON(narrow)), "/w",
			PermissionRequest{permissionFileRead, "/w/sub/a.txt"}, true},
		{"workspace default decides without consulting global rules",
			levelJSON(`"default":"deny"`), levelJSON(`"default":"allow"`), "/w",
			PermissionRequest{permissionFileRead, "/elsewhere/x"}, true},
		{"global default applies when the workspace level is absent",
			levelJSON(`"default":"allow"`), "", "/w",
			PermissionRequest{permissionFileRead, "/elsewhere/x"}, true},
		{"global default applies when the workspace declares neither rules nor default",
			levelJSON(`"default":"allow"`), "{}", "/w",
			PermissionRequest{permissionFileRead, "/elsewhere/x"}, true},
		{"built-ins apply when user levels provide no default",
			levelJSON(rulesJSON(ruleJSON("acme.op", "irrelevant", permissionAccessDeny))), "", "/w",
			PermissionRequest{permissionFileRead, "/w/a.txt"}, true},
		{"unknown permission namespace reaches the built-in default deny", "", "", "/w",
			PermissionRequest{"acme.unknown", "*"}, false},
		{"unknown permission namespace is denied even under an absent-default global",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/*", permissionAccessAllow))), "", "/w",
			PermissionRequest{"acme.unknown", "*"}, false},
		{"outside-root file target denied by the global default",
			levelJSON(`"default":"deny"`), "", "/w",
			PermissionRequest{permissionFileRead, "/etc/passwd"}, false},
		{"workspace rule explicitly allows an outside-root file target", "",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/etc/*", permissionAccessAllow))), "/w",
			PermissionRequest{permissionFileRead, "/etc/passwd"}, true},
		{"global rule explicitly allows an outside-root file target",
			levelJSON(rulesJSON(ruleJSON(permissionFileWrite, "/srv/*", permissionAccessAllow))), "", "/w",
			PermissionRequest{permissionFileWrite, "/srv/data/db"}, true},
		{"workspace rule overrides the built-in sensitive-file denial", "",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/.env", permissionAccessAllow))), "/w",
			PermissionRequest{permissionFileRead, "/w/.env"}, true},
		{"global rule overrides the built-in sensitive-file denial",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/.env", permissionAccessAllow))), "", "/w",
			PermissionRequest{permissionFileRead, "/w/.env"}, true},
		{"zero policy is the built-in policy", "", "", "/w",
			PermissionRequest{permissionCommandRun, "echo hi"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy(tc.global, tc.workspe).evaluate(tc.root, tc.req); got != tc.want {
				t.Errorf("evaluate(%q, %+v) = %v, want %v", tc.root, tc.req, got, tc.want)
			}
		})
	}
}

func TestResolvePermissionPolicyBuiltins(t *testing.T) {
	policy := ResolvePermissionPolicy(nil, nil)
	inside := []string{
		"/w/a.txt", "/w/sub/b.go", "/w/Makefile", "/w/README.md", "/w/credentials.json.example",
	}
	for _, path := range inside {
		t.Run("in-workspace file allowed "+path, func(t *testing.T) {
			for _, permission := range []string{permissionFileRead, permissionFileWrite} {
				if !policy.evaluate("/w", PermissionRequest{permission, path}) {
					t.Errorf("built-in %s of %s denied, want allow", permission, path)
				}
			}
		})
	}
	outside := []string{"/etc/passwd", "/w.txt", "/other/sub/a", "relative/a.txt", "/w/../secrets/key"}
	for _, path := range outside {
		t.Run("outside-workspace file denied "+path, func(t *testing.T) {
			for _, permission := range []string{permissionFileRead, permissionFileWrite} {
				if policy.evaluate("/w", PermissionRequest{permission, path}) {
					t.Errorf("built-in %s of %s allowed, want deny", permission, path)
				}
			}
		})
	}
	t.Run("workspace root itself is inside", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{permissionFileRead, "/w"}) {
			t.Error("built-in file.read of the root denied, want allow")
		}
	})
	t.Run("plan-asserted sensitive versus sensitive-looking pairs", func(t *testing.T) {
		type pair struct {
			allow string
			deny  string
		}
		for _, permission := range []string{permissionFileRead, permissionFileWrite} {
			for _, p := range []pair{
				{"/w/.env.cache/README.md", "/w/sub/.env.cache"},
				{"/w/credentials-cache/data.json", "/w/sub/credentials-cache.json"},
			} {
				if !policy.evaluate("/w", PermissionRequest{permission, p.allow}) {
					t.Errorf("built-in %s of %s denied, want allow", permission, p.allow)
				}
				if policy.evaluate("/w", PermissionRequest{permission, p.deny}) {
					t.Errorf("built-in %s of %s allowed, want deny", permission, p.deny)
				}
			}
		}
	})
	t.Run("every sensitive basename denies under both file permissions", func(t *testing.T) {
		denied := []string{
			".env", ".env.local", ".env.cache",
			".netrc", ".npmrc", ".pypirc",
			"server.pem", "tls.key", "cert.p12", "cert.pfx", "app.keystore", "app.jks",
			"id_rsa", "id_rsa.pub", "id_ed25519", "id_ed25519.pub", "id_ecdsa", "id_ecdsa.pub", "id_dsa", "id_dsa.pub",
			"credentials.json", "credentials.yaml", "credentials.yml",
			"credentialsx.json", "credentials-db.yaml", "credentials_cache.yml",
			"id_dsa.pem",
		}
		for _, base := range denied {
			for _, permission := range []string{permissionFileRead, permissionFileWrite} {
				for _, dir := range []string{"/w/", "/w/sub/"} {
					target := dir + base
					if !policy.evaluate("/w", PermissionRequest{permission, target}) {
						continue
					}
					t.Errorf("built-in %s of %s allowed, want deny", permission, target)
				}
			}
		}
	})
	t.Run("nearest forbidden siblings of every sensitive basename allow", func(t *testing.T) {
		allowed := []string{
			"env", ".envi", ".environment", "myenv", ".envrc", "my.env", "x.env",
			"netrc", "npmrc", "pypirc", "environment",
			"pem", "key", "key.pemx", "p12", "pfx", "keystore", "jks",
			"id_rsaa", "id_rsa_pub", "id_ed25519x", "id_ecdsa_",
			"MyCredentials.JSON", "credentials", "mycredentials.json", "credentials.jsonx",
		}
		for _, base := range allowed {
			for _, permission := range []string{permissionFileRead, permissionFileWrite} {
				for _, dir := range []string{"/w/", "/w/sub/"} {
					target := dir + base
					if policy.evaluate("/w", PermissionRequest{permission, target}) {
						continue
					}
					t.Errorf("built-in %s of %s denied, want allow", permission, target)
				}
			}
		}
	})
	t.Run("sensitive-looking ancestor directory never denies its contents", func(t *testing.T) {
		for _, base := range sensitiveBasenamePatterns {
			dir := strings.ReplaceAll(base, "*", "x")
			for _, permission := range []string{permissionFileRead, permissionFileWrite} {
				target := "/w/" + dir + "/f"
				if !policy.evaluate("/w", PermissionRequest{permission, target}) {
					t.Errorf("built-in %s of %s denied, want allow", permission, target)
				}
				nested := "/w/" + dir + "/sub/" + base
				if policy.evaluate("/w", PermissionRequest{permission, nested}) {
					t.Errorf("built-in %s of %s allowed, want deny", permission, nested)
				}
			}
		}
	})
	t.Run("literal glob characters in a real path are plain data", func(t *testing.T) {
		for _, base := range []string{"*", "?", "[x]", "a?b", "x[1].txt"} {
			if !policy.evaluate("/w", PermissionRequest{permissionFileRead, "/w/" + base}) {
				t.Errorf("built-in file.read of %s denied, want allow", base)
			}
		}
	})
	t.Run("root path metacharacters stay literal path data", func(t *testing.T) {
		root := "/w/[x]"
		if !policy.evaluate(root, PermissionRequest{permissionFileRead, root + "/a.txt"}) {
			t.Error("file under a metacharacter root denied, want allow")
		}
		if !policy.evaluate(root, PermissionRequest{permissionWorkspaceInspect, root}) {
			t.Error("inspect of the metacharacter root denied, want allow")
		}
		if policy.evaluate(root, PermissionRequest{permissionWorkspaceInspect, "/w/y"}) {
			t.Error("inspect of a different root allowed, want deny")
		}
	})
	t.Run("command run allows any canonical command target", func(t *testing.T) {
		for _, target := range []string{"*", "git push origin main", "rm -rf /tmp/x", "echo\nls"} {
			if !policy.evaluate("/w", PermissionRequest{permissionCommandRun, target}) {
				t.Errorf("built-in command.run of %q denied, want allow", target)
			}
		}
	})
	t.Run("workspace inspect allows only the canonical root", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{permissionWorkspaceInspect, "/w"}) {
			t.Error("built-in inspect of the root denied, want allow")
		}
		for _, target := range []string{"/w/keys", "/etc", "*", ""} {
			if policy.evaluate("/w", PermissionRequest{permissionWorkspaceInspect, target}) {
				t.Errorf("built-in inspect of %q allowed, want deny", target)
			}
		}
	})
	t.Run("sleep allows the fixed target only", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{permissionSleep, "*"}) {
			t.Error("built-in sleep of the fixed target denied, want allow")
		}
		for _, target := range []string{"1", ""} {
			if policy.evaluate("/w", PermissionRequest{permissionSleep, target}) {
				t.Errorf("built-in sleep of %q allowed, want deny", target)
			}
		}
	})
	t.Run("uncovered future permission namespaces deny", func(t *testing.T) {
		for _, permission := range []string{
			"process.list", "process.read", "process.stop", "agent.start", "owned.process.list",
		} {
			if policy.evaluate("/w", PermissionRequest{permission, "*"}) {
				t.Errorf("built-in %s allowed, want deny", permission)
			}
		}
	})
}

func TestPermissionGlobMatcher(t *testing.T) {
	globs := []struct {
		name    string
		pattern string
		target  string
		want    bool
	}{
		{"star matches across separators", "/w/*", "/w/a/b/c.txt", true},
		{"star matches across newlines", "/w/*", "/w/a\nb", true},
		{"star matches the empty string", "/w/*", "/w/", true},
		{"consecutive stars carry ordinary star meaning", "/w/**/?.txt", "/w/a/b/c.txt", true},
		{"question mark matches exactly one newline rune", "a?c", "a\nc", true},
		{"question mark matches exactly one rune", "a?c", "ac", false},
		{"question mark matches one unicode rune", "ü?er", "über", true},
		{"question mark does not match two runes", "a?c", "a\n\nc", false},
		{"star matches a multi-rune segment", "/w/*/x", "/w/ä/\n/y", false},
		{"star matches unicode and newlines mid-pattern", "/w/*/x", "/w/ä\nb/x", true},
		{"backslash quotes the star", `\\d\*\x`, `\d*x`, true},
		{"backslash quotes the star against a real target", `\\d\*\x`, `\d?x`, false},
		{"quoted literal is not a wildcard", `\a\*c`, "abc", false},
		{"brackets are literal", "/w/[x]", "/w/x", false},
		{"brackets match literally", "/w/[x]", "/w/[x]", true},
		{"braces are literal", "/w/{a,b}", "/w/a", false},
		{"brace literal matches literally", "/w/{a}", "/w/{a}", true},
		{"pattern anchors to the full target", "/w/a", "/w/ab", false},
		{"pattern anchors at the start too", "/w/a", "x/w/a", false},
		{"no case folding", "/w/A.txt", "/w/a.txt", false},
		{"star alone matches everything", "*", "", true},
	}
	for _, tc := range globs {
		t.Run(tc.name, func(t *testing.T) {
			glob, err := compileGlob(tc.pattern)
			if err != nil {
				t.Fatalf("compileGlob(%q): %v", tc.pattern, err)
			}
			if got := glob.MatchString(tc.target); got != tc.want {
				t.Errorf("glob %q match %q = %v, want %v", tc.pattern, tc.target, got, tc.want)
			}
		})
	}
	t.Run("rule matching uses the complete canonical target", func(t *testing.T) {
		policy := ResolvePermissionPolicy(nil, json.RawMessage(levelJSON(rulesJSON(
			ruleJSON(permissionFileRead, "*", permissionAccessAllow),
			ruleJSON(permissionFileRead, "/w/docs/*", permissionAccessDeny),
		))))
		if policy.evaluate("/w", PermissionRequest{permissionFileRead, "/w/docs/a.txt"}) {
			t.Error("the complete-target deny rule did not match its canonical target")
		}
		if !policy.evaluate("/w", PermissionRequest{permissionFileRead, "/etc/docs/a.txt"}) {
			t.Error("the complete-target deny rule matched outside its pattern")
		}
	})
}

func TestCompileGlobRejectsTrailingBackslash(t *testing.T) {
	if _, err := compileGlob(`/w/sub\`); err == nil {
		t.Fatal("compileGlob accepted a trailing backslash")
	}
	if _, err := compileGlob(`\`); err == nil {
		t.Fatal("compileGlob accepted a lone backslash")
	}
}

func TestResolvePermissionPolicyMalformedDiscardsBothLevels(t *testing.T) {
	validGlobal := levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/elsewhere/*", permissionAccessAllow)))
	validWorkspace := levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/docs/*", permissionAccessDeny)))
	tests := []struct {
		name  string
		bad   string
		valid string
	}{
		{"null block is malformed not absence", `null`, validGlobal},
		{"array block is malformed", `[]`, validGlobal},
		{"string block is malformed", `"default"`, validGlobal},
		{"unknown top-level member is malformed", levelJSON(`"permission":"file.read"`), validWorkspace},
		{"default with wrong type is malformed", levelJSON(`"default":true`), validWorkspace},
		{"default null is malformed", levelJSON(`"default":null`), validWorkspace},
		{"default with unknown access is malformed", levelJSON(`"default":"ask"`), validWorkspace},
		{"rules null is malformed not absence", levelJSON(`"rules":null`), validGlobal},
		{"rules with wrong type is malformed", levelJSON(`"rules":"*"`), validGlobal},
		{"rules object is malformed", levelJSON(`"rules":{}`), validGlobal},
		{"rule missing a required field is malformed",
			levelJSON(rulesJSON(`{"permission":"file.read","target":"/w/*"}`)), validGlobal},
		{"rule with unknown field is malformed",
			levelJSON(rulesJSON(`{"permission":"file.read","target":"/w/*","access":"allow","note":"x"}`)), validGlobal},
		{"rule with wrong-typed field is malformed",
			levelJSON(rulesJSON(`{"permission":7,"target":"/w/*","access":"allow"}`)), validGlobal},
		{"rule with empty permission is malformed",
			levelJSON(rulesJSON(`{"permission":"","target":"/w/*","access":"allow"}`)), validGlobal},
		{"rule with empty target is malformed",
			levelJSON(rulesJSON(`{"permission":"file.read","target":"","access":"allow"}`)), validGlobal},
		{"rule with unknown access is malformed",
			levelJSON(rulesJSON(ruleJSON(permissionFileRead, "/w/*", "permit"))), validGlobal},
		{"rule with invalid glob is malformed",
			levelJSON(rulesJSON(`{"permission":"file.read","target":"/w/sub\\","access":"allow"}`)), validGlobal},
		{"rules array member that is not an object is malformed",
			levelJSON(`"rules":["x"]`), validGlobal},
		{"rules array member null is malformed",
			levelJSON(`"rules":[null]`), validGlobal},
		{"trailing json after the block is malformed", levelJSON(`"default":"deny"`) + `{"default":"allow"}`, validGlobal},
		{"trailing garbage after the rules array is malformed",
			`{"rules":[]}` + "junk", validWorkspace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			malformed := json.RawMessage(tc.bad)
			valid := json.RawMessage(tc.valid)
			// The malformed block discards the valid level on both sides
			// for this resolution, leaving the exact built-in policy.
			for _, policy := range []PermissionPolicy{
				ResolvePermissionPolicy(malformed, valid),
				ResolvePermissionPolicy(valid, malformed),
			} {
				if !isBuiltInOnly(policy) {
					t.Errorf("malformed %s did not discard both levels: %+v", tc.name, policy)
				}
			}
		})
	}
}

func TestResolvePermissionPolicyValidShapes(t *testing.T) {
	if got := ResolvePermissionPolicy(nil, nil); !isBuiltInOnly(got) {
		t.Errorf("absent levels resolved to %+v, want the zero built-in policy", got)
	}
	outsideAllow := json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(permissionFileRead, "/elsewhere/*", permissionAccessAllow),
	)))
	workspaceDeny := json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(permissionFileRead, "/w/private/*", permissionAccessDeny),
	)))
	if !ResolvePermissionPolicy(outsideAllow, workspaceDeny).evaluate("/w",
		PermissionRequest{permissionFileRead, "/elsewhere/x"}) {
		t.Error("valid global level lost when a valid workspace block is present")
	}
	if ResolvePermissionPolicy(outsideAllow, workspaceDeny).evaluate("/w",
		PermissionRequest{permissionFileRead, "/w/private/x"}) {
		t.Error("valid workspace level lost when a valid global block is present")
	}
	// A malformed Workspace block affects only resolutions carrying it: the
	// same global level still decides for a valid Workspace sibling.
	for _, tc := range []struct {
		name      string
		workspace json.RawMessage
	}{
		{"malformed workspace block", json.RawMessage(`"not-an-object"`)},
		{"valid workspace sibling", json.RawMessage("{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := ResolvePermissionPolicy(outsideAllow, tc.workspace)
			want := tc.name != "malformed workspace block"
			if got := policy.evaluate("/w", PermissionRequest{permissionFileRead, "/elsewhere/x"}); got != want {
				t.Errorf("global decision under %s = %v, want %v", tc.name, got, want)
			}
		})
	}
	emptyRules := json.RawMessage(levelJSON(rulesJSON()))
	if got := ResolvePermissionPolicy(emptyRules, nil); !isBuiltInOnly(got) {
		t.Errorf("empty rules list resolved to %+v, want the zero built-in policy", got)
	}
	// Duplicate members resolve by encoding/json's ordinary last-value
	// decoding, not a second duplicate-key parser.
	duplicates := json.RawMessage(`{"default":"deny","default":"allow","rules":[],"rules":[]}`)
	if !ResolvePermissionPolicy(nil, duplicates).evaluate("/w",
		PermissionRequest{permissionFileRead, "/elsewhere/x"}) {
		t.Error("last duplicate default did not win")
	}
	lastRule := json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(permissionFileRead, "/w/private/*", permissionAccessDeny),
		ruleJSON(permissionFileRead, "/w/private/*", permissionAccessAllow),
	)))
	if !ResolvePermissionPolicy(nil, lastRule).evaluate("/w",
		PermissionRequest{permissionFileRead, "/w/private/x"}) {
		t.Error("last duplicate rule did not win")
	}
	// Whitespace before the top-level value is ordinary JSON, not trailing
	// garbage.
	padded := json.RawMessage(" \n\t" + levelJSON(`"default":"allow"`))
	if !ResolvePermissionPolicy(padded, nil).evaluate("/w",
		PermissionRequest{permissionSleep, "1"}) {
		t.Error("whitespace-padded block did not parse as a valid level")
	}
}

func TestPermissionPolicyCallAllowedAllPairs(t *testing.T) {
	policy := ResolvePermissionPolicy(nil, json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(permissionFileWrite, "/w/secret", permissionAccessDeny),
	))))
	tests := []struct {
		name     string
		requests []PermissionRequest
		want     bool
	}{
		{"every pair allows", []PermissionRequest{
			{permissionFileWrite, "/w/a.txt"},
			{permissionFileRead, "/w/b.txt"},
			{permissionCommandRun, "git status"},
		}, true},
		{"one denied pair denies the whole call", []PermissionRequest{
			{permissionFileWrite, "/w/a.txt"},
			{permissionFileWrite, "/w/secret"},
			{permissionFileRead, "/w/b.txt"},
		}, false},
		{"one unmatched outside-root pair denies the whole call", []PermissionRequest{
			{permissionFileWrite, "/w/a.txt"},
			{permissionFileWrite, "/etc/passwd"},
		}, false},
		{"a lone denied pair denies", []PermissionRequest{
			{permissionFileWrite, "/w/secret"},
		}, false},
		{"no declared pairs declare nothing to deny", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.callAllowed("/w", tc.requests); got != tc.want {
				t.Errorf("callAllowed(%+v) = %v, want %v", tc.requests, got, tc.want)
			}
		})
	}
}

func TestPermissionLevelDecideSemantics(t *testing.T) {
	level := permissionLevel{}
	if _, decided := level.decide(PermissionRequest{permissionFileRead, "/w/a"}); decided {
		t.Error("empty level without a default decided a request")
	}
	level.defaultAllow = true
	level.hasDefault = true
	if allow, decided := level.decide(PermissionRequest{permissionFileRead, "/w/a"}); !decided || !allow {
		t.Errorf("level default did not decide: allow=%v decided=%v", allow, decided)
	}
	glob := mustCompileGlob("/w/*")
	level.rules = []permissionRule{
		{permission: permissionFileRead, glob: glob, allow: false},
		{permission: permissionFileWrite, glob: glob, allow: true},
		{permission: permissionFileRead, glob: glob, allow: true},
	}
	if allow, decided := level.decide(PermissionRequest{permissionFileRead, "/w/a"}); !decided || !allow {
		t.Errorf("last matching rule did not decide over the level default: allow=%v decided=%v", allow, decided)
	}
	if allow, decided := level.decide(PermissionRequest{permissionCommandRun, "x"}); !decided || !allow {
		t.Errorf("level default did not decide an unmatched permission: allow=%v decided=%v", allow, decided)
	}
	if allow, decided := level.decide(PermissionRequest{permissionFileRead, "/other/a"}); !decided || !allow {
		t.Errorf("level default did not decide an unmatched target: allow=%v decided=%v", allow, decided)
	}
}
