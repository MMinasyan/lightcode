package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	broad := ruleJSON(PermissionFileRead, "/w/*", permissionAccessDeny)
	narrow := ruleJSON(PermissionFileRead, "/w/sub/*", permissionAccessAllow)
	countered := ruleJSON(PermissionFileRead, "/w/sub/a.txt", permissionAccessDeny)
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
			PermissionRequest{PermissionFileRead, "/w/a.txt"}, true},
		{"absent levels reach built-in outside-root deny", "", "", "/w",
			PermissionRequest{PermissionFileRead, "/etc/passwd"}, false},
		{"last matching workspace rule decides over earlier siblings", "", levelJSON(rulesJSON(broad, narrow)), "/w",
			PermissionRequest{PermissionFileRead, "/w/sub/a.txt"}, true},
		{"last matching workspace rule decides under a later counter-exception", "",
			levelJSON(rulesJSON(broad, narrow, countered)), "/w",
			PermissionRequest{PermissionFileRead, "/w/sub/a.txt"}, false},
		{"last matching global rule decides without any workspace level", levelJSON(rulesJSON(broad, narrow)), "", "/w",
			PermissionRequest{PermissionFileRead, "/w/sub/a.txt"}, true},
		{"workspace rule overrides an opposite global rule",
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/*", permissionAccessAllow))),
			levelJSON(rulesJSON(broad)), "/w",
			PermissionRequest{PermissionFileRead, "/w/a.txt"}, false},
		{"workspace match denies even when the workspace default allows", "",
			levelJSON(`"default":"allow"`, rulesJSON(broad)), "/w",
			PermissionRequest{PermissionFileRead, "/w/a.txt"}, false},
		{"workspace default applies when no workspace rule matches", "",
			levelJSON(`"default":"deny"`, rulesJSON(ruleJSON(PermissionCommandRun, "*", permissionAccessAllow))), "/w",
			PermissionRequest{PermissionFileRead, "/w/a.txt"}, false},
		{"a matching workspace rule outranks a denying workspace default and a denying global",
			levelJSON(`"default":"deny"`), levelJSON(`"default":"deny"`, rulesJSON(narrow)), "/w",
			PermissionRequest{PermissionFileRead, "/w/sub/a.txt"}, true},
		{"workspace default decides without consulting global rules",
			levelJSON(`"default":"deny"`), levelJSON(`"default":"allow"`), "/w",
			PermissionRequest{PermissionFileRead, "/elsewhere/x"}, true},
		{"global default applies when the workspace level is absent",
			levelJSON(`"default":"allow"`), "", "/w",
			PermissionRequest{PermissionFileRead, "/elsewhere/x"}, true},
		{"global default applies when the workspace declares neither rules nor default",
			levelJSON(`"default":"allow"`), "{}", "/w",
			PermissionRequest{PermissionFileRead, "/elsewhere/x"}, true},
		{"built-ins apply when user levels provide no default",
			levelJSON(rulesJSON(ruleJSON("acme.op", "irrelevant", permissionAccessDeny))), "", "/w",
			PermissionRequest{PermissionFileRead, "/w/a.txt"}, true},
		{"unknown permission namespace reaches the built-in default deny", "", "", "/w",
			PermissionRequest{"acme.unknown", "*"}, false},
		{"unknown permission namespace is denied even under an absent-default global",
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/*", permissionAccessAllow))), "", "/w",
			PermissionRequest{"acme.unknown", "*"}, false},
		{"outside-root file target denied by the global default",
			levelJSON(`"default":"deny"`), "", "/w",
			PermissionRequest{PermissionFileRead, "/etc/passwd"}, false},
		{"workspace rule explicitly allows an outside-root file target", "",
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/etc/*", permissionAccessAllow))), "/w",
			PermissionRequest{PermissionFileRead, "/etc/passwd"}, true},
		{"global rule explicitly allows an outside-root file target",
			levelJSON(rulesJSON(ruleJSON(PermissionFileWrite, "/srv/*", permissionAccessAllow))), "", "/w",
			PermissionRequest{PermissionFileWrite, "/srv/data/db"}, true},
		{"workspace rule overrides the built-in sensitive-file denial", "",
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/.env", permissionAccessAllow))), "/w",
			PermissionRequest{PermissionFileRead, "/w/.env"}, true},
		{"global rule overrides the built-in sensitive-file denial",
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/.env", permissionAccessAllow))), "", "/w",
			PermissionRequest{PermissionFileRead, "/w/.env"}, true},
		{"zero policy is the built-in policy", "", "", "/w",
			PermissionRequest{PermissionCommandRun, "echo hi"}, true},
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
			for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
				if !policy.evaluate("/w", PermissionRequest{permission, path}) {
					t.Errorf("built-in %s of %s denied, want allow", permission, path)
				}
			}
		})
	}
	outside := []string{"/etc/passwd", "/w.txt", "/other/sub/a", "relative/a.txt", "/w/../secrets/key"}
	for _, path := range outside {
		t.Run("outside-workspace file denied "+path, func(t *testing.T) {
			for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
				if policy.evaluate("/w", PermissionRequest{permission, path}) {
					t.Errorf("built-in %s of %s allowed, want deny", permission, path)
				}
			}
		})
	}
	t.Run("workspace root itself is inside", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{PermissionFileRead, "/w"}) {
			t.Error("built-in file.read of the root denied, want allow")
		}
	})
	t.Run("plan-asserted sensitive versus sensitive-looking pairs", func(t *testing.T) {
		type pair struct {
			allow string
			deny  string
		}
		for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
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
			for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
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
			for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
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
			for _, permission := range []string{PermissionFileRead, PermissionFileWrite} {
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
			if !policy.evaluate("/w", PermissionRequest{PermissionFileRead, "/w/" + base}) {
				t.Errorf("built-in file.read of %s denied, want allow", base)
			}
		}
	})
	t.Run("root path metacharacters stay literal path data", func(t *testing.T) {
		root := "/w/[x]"
		if !policy.evaluate(root, PermissionRequest{PermissionFileRead, root + "/a.txt"}) {
			t.Error("file under a metacharacter root denied, want allow")
		}
		if !policy.evaluate(root, PermissionRequest{PermissionWorkspaceInspect, root}) {
			t.Error("inspect of the metacharacter root denied, want allow")
		}
		if policy.evaluate(root, PermissionRequest{PermissionWorkspaceInspect, "/w/y"}) {
			t.Error("inspect of a different root allowed, want deny")
		}
	})
	t.Run("command run allows any canonical command target", func(t *testing.T) {
		for _, target := range []string{"*", "git push origin main", "rm -rf /tmp/x", "echo\nls"} {
			if !policy.evaluate("/w", PermissionRequest{PermissionCommandRun, target}) {
				t.Errorf("built-in command.run of %q denied, want allow", target)
			}
		}
	})
	t.Run("workspace inspect allows only the canonical root", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{PermissionWorkspaceInspect, "/w"}) {
			t.Error("built-in inspect of the root denied, want allow")
		}
		for _, target := range []string{"/w/keys", "/etc", "*", ""} {
			if policy.evaluate("/w", PermissionRequest{PermissionWorkspaceInspect, target}) {
				t.Errorf("built-in inspect of %q allowed, want deny", target)
			}
		}
	})
	t.Run("sleep allows the fixed target only", func(t *testing.T) {
		if !policy.evaluate("/w", PermissionRequest{PermissionSleep, "*"}) {
			t.Error("built-in sleep of the fixed target denied, want allow")
		}
		for _, target := range []string{"1", ""} {
			if policy.evaluate("/w", PermissionRequest{PermissionSleep, target}) {
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

func TestBuiltInAllowsChildLaunchAndProcessOwn(t *testing.T) {
	builtIn := ResolvePermissionPolicy(nil, nil)
	concrete := []struct {
		permission string
		target     string
	}{
		{PermissionChildLaunch, "0123456789abcdef0123456789abcdef"},
		{PermissionProcessOwn, "a1b2c3d4"},
	}
	for _, req := range concrete {
		t.Run("built-in "+req.permission+" of "+req.target, func(t *testing.T) {
			if !builtIn.evaluate("/w", PermissionRequest{req.permission, req.target}) {
				t.Errorf("built-in %s of %q denied, want allow", req.permission, req.target)
			}
		})
	}
	denied := ResolvePermissionPolicy(nil, json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(PermissionChildLaunch, "*", permissionAccessDeny),
		ruleJSON(PermissionProcessOwn, "*", permissionAccessDeny),
	))))
	for _, req := range concrete {
		t.Run("user deny decides "+req.permission+" of "+req.target, func(t *testing.T) {
			if denied.evaluate("/w", PermissionRequest{req.permission, req.target}) {
				t.Errorf("user deny did not decide %s of %q: allowed, want deny", req.permission, req.target)
			}
		})
	}
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
			ruleJSON(PermissionFileRead, "*", permissionAccessAllow),
			ruleJSON(PermissionFileRead, "/w/docs/*", permissionAccessDeny),
		))))
		if policy.evaluate("/w", PermissionRequest{PermissionFileRead, "/w/docs/a.txt"}) {
			t.Error("the complete-target deny rule did not match its canonical target")
		}
		if !policy.evaluate("/w", PermissionRequest{PermissionFileRead, "/etc/docs/a.txt"}) {
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
	validGlobal := levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/elsewhere/*", permissionAccessAllow)))
	validWorkspace := levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/docs/*", permissionAccessDeny)))
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
			levelJSON(rulesJSON(ruleJSON(PermissionFileRead, "/w/*", "permit"))), validGlobal},
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
		ruleJSON(PermissionFileRead, "/elsewhere/*", permissionAccessAllow),
	)))
	workspaceDeny := json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(PermissionFileRead, "/w/private/*", permissionAccessDeny),
	)))
	if !ResolvePermissionPolicy(outsideAllow, workspaceDeny).evaluate("/w",
		PermissionRequest{PermissionFileRead, "/elsewhere/x"}) {
		t.Error("valid global level lost when a valid workspace block is present")
	}
	if ResolvePermissionPolicy(outsideAllow, workspaceDeny).evaluate("/w",
		PermissionRequest{PermissionFileRead, "/w/private/x"}) {
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
			if got := policy.evaluate("/w", PermissionRequest{PermissionFileRead, "/elsewhere/x"}); got != want {
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
		PermissionRequest{PermissionFileRead, "/elsewhere/x"}) {
		t.Error("last duplicate default did not win")
	}
	lastRule := json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(PermissionFileRead, "/w/private/*", permissionAccessDeny),
		ruleJSON(PermissionFileRead, "/w/private/*", permissionAccessAllow),
	)))
	if !ResolvePermissionPolicy(nil, lastRule).evaluate("/w",
		PermissionRequest{PermissionFileRead, "/w/private/x"}) {
		t.Error("last duplicate rule did not win")
	}
	// Whitespace before the top-level value is ordinary JSON, not trailing
	// garbage.
	padded := json.RawMessage(" \n\t" + levelJSON(`"default":"allow"`))
	if !ResolvePermissionPolicy(padded, nil).evaluate("/w",
		PermissionRequest{PermissionSleep, "1"}) {
		t.Error("whitespace-padded block did not parse as a valid level")
	}
}

func TestPermissionPolicyCallAllowedAllPairs(t *testing.T) {
	policy := ResolvePermissionPolicy(nil, json.RawMessage(levelJSON(rulesJSON(
		ruleJSON(PermissionFileWrite, "/w/secret", permissionAccessDeny),
	))))
	tests := []struct {
		name     string
		requests []PermissionRequest
		want     bool
	}{
		{"every pair allows", []PermissionRequest{
			{PermissionFileWrite, "/w/a.txt"},
			{PermissionFileRead, "/w/b.txt"},
			{PermissionCommandRun, "git status"},
		}, true},
		{"one denied pair denies the whole call", []PermissionRequest{
			{PermissionFileWrite, "/w/a.txt"},
			{PermissionFileWrite, "/w/secret"},
			{PermissionFileRead, "/w/b.txt"},
		}, false},
		{"one unmatched outside-root pair denies the whole call", []PermissionRequest{
			{PermissionFileWrite, "/w/a.txt"},
			{PermissionFileWrite, "/etc/passwd"},
		}, false},
		{"a lone denied pair denies", []PermissionRequest{
			{PermissionFileWrite, "/w/secret"},
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
	if _, decided := level.decide(PermissionRequest{PermissionFileRead, "/w/a"}); decided {
		t.Error("empty level without a default decided a request")
	}
	level.defaultAllow = true
	level.hasDefault = true
	if allow, decided := level.decide(PermissionRequest{PermissionFileRead, "/w/a"}); !decided || !allow {
		t.Errorf("level default did not decide: allow=%v decided=%v", allow, decided)
	}
	glob := mustCompileGlob("/w/*")
	level.rules = []permissionRule{
		{permission: PermissionFileRead, glob: glob, allow: false},
		{permission: PermissionFileWrite, glob: glob, allow: true},
		{permission: PermissionFileRead, glob: glob, allow: true},
	}
	if allow, decided := level.decide(PermissionRequest{PermissionFileRead, "/w/a"}); !decided || !allow {
		t.Errorf("last matching rule did not decide over the level default: allow=%v decided=%v", allow, decided)
	}
	if allow, decided := level.decide(PermissionRequest{PermissionCommandRun, "x"}); !decided || !allow {
		t.Errorf("level default did not decide an unmatched permission: allow=%v decided=%v", allow, decided)
	}
	if allow, decided := level.decide(PermissionRequest{PermissionFileRead, "/other/a"}); !decided || !allow {
		t.Errorf("level default did not decide an unmatched target: allow=%v decided=%v", allow, decided)
	}
}

// TestPermissionNamesKeepPublishedLiterals pins the exported policy
// vocabulary to its exact published strings: these names cross the plugin
// boundary as literals, so the constants may never drift from them.
func TestPermissionNamesKeepPublishedLiterals(t *testing.T) {
	want := map[string]string{
		PermissionFileRead:         "file.read",
		PermissionFileWrite:        "file.write",
		PermissionCommandRun:       "command.run",
		PermissionWorkspaceInspect: "workspace.inspect",
		PermissionSleep:            "sleep",
		PermissionChildLaunch:      "child.launch",
		PermissionProcessOwn:       "process.own",
	}
	if len(want) != 7 { // distinct literals: a duplicated value would collapse the map.
		t.Fatalf("published permission vocabulary carries %d distinct literals, want 7", len(want))
	}
	for name, literal := range want {
		if name != literal {
			t.Errorf("permission constant %q = %q, want the published literal", name, literal)
		}
	}
	if PermissionDeniedContent != "Permission denied." {
		t.Errorf("PermissionDeniedContent = %q, want the published denial content", PermissionDeniedContent)
	}
}

// readmePermissionsDocument extracts the one fenced JSON document in README.md
// that carries a permissions member: the published Permissions example.
func readmePermissionsDocument(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```json" {
			continue
		}
		var block strings.Builder
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			block.WriteString(lines[i])
			block.WriteByte('\n')
		}
		if strings.Contains(block.String(), `"permissions"`) {
			return []byte(block.String())
		}
	}
	t.Fatal("README.md has no fenced permissions JSON example")
	return nil
}

// TestReadmePermissionsExampleAppliesItsRules guards the published example
// with the parser that actually consumes it: the fenced document must parse
// through the private level parser — no silent built-in fallback — and its
// git allow, generated-file deny and default-deny rules must decide exactly
// as written. The built-in policy would answer the deny and default rows the
// opposite way, so those rows fail on any fallback; the non-git command row
// pins that the allow is scoped to the git target, and the built-in policy
// would allow that command too. The generated-file deny is additionally
// observed with the example's default cleared, so that row cannot be
// satisfied by the default alone.
func TestReadmePermissionsExampleAppliesItsRules(t *testing.T) {
	block := readmePermissionsDocument(t)
	var doc struct {
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal(block, &doc); err != nil {
		t.Fatalf("README permissions example is not a JSON document: %v", err)
	}
	if len(doc.Permissions) == 0 {
		t.Fatal("README permissions example carries no permissions member")
	}
	policy := ResolvePermissionPolicy(doc.Permissions, nil)
	if isBuiltInOnly(policy) {
		t.Fatal("README permissions example fell back to the built-in policy instead of parsing")
	}
	for _, tc := range []struct {
		name string
		req  PermissionRequest
		want bool
	}{
		{"git command allow", PermissionRequest{PermissionCommandRun, "git status"}, true},
		{"non-git command falls to the example default deny", PermissionRequest{PermissionCommandRun, "echo hello"}, false},
		{"generated file write deny", PermissionRequest{PermissionFileWrite, "/w/generated/x.go"}, false},
		{"unmatched read falls to the example default deny", PermissionRequest{PermissionFileRead, "/w/plain.txt"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.evaluate("/w", tc.req); got != tc.want {
				t.Errorf("README example %s = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	// The extracted generated-file deny must decide on its own: the built-in
	// policy allows a plain workspace write, so clearing the example default
	// on a test-local copy leaves that allow as the fallback and only the
	// extracted rule can deny the target. The full-policy row above stays
	// masked by the default by design; this observation is not.
	generatedWrite := PermissionRequest{PermissionFileWrite, "/w/generated/x.go"}
	if !(PermissionPolicy{}).evaluate("/w", generatedWrite) {
		t.Fatal("built-in control: a plain workspace write must be allowed without any user level")
	}
	ruleOnly := policy
	ruleOnly.global.hasDefault = false
	if ruleOnly.evaluate("/w", generatedWrite) {
		t.Error("extracted generated-file deny rule did not deny its target once the example default was cleared")
	}
}
