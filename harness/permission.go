package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// PermissionFileRead, PermissionFileWrite, PermissionCommandRun,
// PermissionWorkspaceInspect, PermissionSleep, PermissionChildLaunch and
// PermissionProcessOwn are the published semantic permission names of the
// fixed automatic policy. They are exact strings; plugins may define new
// namespaced names but cannot contribute rules to the built-in policy, and
// these built-in names are not a closed plugin namespace.
//
// The access values stay private: access strings and policy parsing are the
// Harness's own.
const (
	PermissionFileRead         = "file.read"
	PermissionFileWrite        = "file.write"
	PermissionCommandRun       = "command.run"
	PermissionWorkspaceInspect = "workspace.inspect"
	PermissionSleep            = "sleep"
	PermissionChildLaunch      = "child.launch"
	PermissionProcessOwn       = "process.own"

	permissionAccessAllow = "allow"
	permissionAccessDeny  = "deny"
)

// PermissionRequest is one declared semantic permission/target pair of a
// prepared call. Target is one canonical string produced by the tool that
// understands it; target-independent operations use the fixed target "*".
type PermissionRequest struct {
	Permission string
	Target     string
}

// PermissionDecision is the closed decision of one evaluated prepared
// action: allow or deny. Any other value is outside the closed set and
// settles as denial.
type PermissionDecision string

const (
	PermissionAllow PermissionDecision = "allow"
	PermissionDeny  PermissionDecision = "deny"
)

// PreparedAction is one prepared call handed to a PermissionEvaluator: the
// actual prepared canonical Workspace root and write root, plus the complete
// ordered permission/target pairs as an evaluator-owned copy.
type PreparedAction struct {
	CanonicalWorkspace string
	CanonicalWriteDir  string
	Permissions        []PermissionRequest
}

// PermissionEvaluator decides one PreparedAction at the Harness permission
// boundary. Allow runs the call; every other outcome — deny, a decision
// outside the closed set, or an error — settles the ordinary permission
// denial without running the effect. Constraint checks and target
// preparation stay inside the Harness and are never delegated.
type PermissionEvaluator interface {
	Evaluate(context.Context, PreparedAction) (PermissionDecision, error)
}

// permissionRule is one compiled rule of a parsed policy level. The glob is
// matched against the complete canonical target.
type permissionRule struct {
	permission string
	glob       *regexp.Regexp
	allow      bool
}

// permissionLevel is one parsed user policy source: an ordered rule list plus
// an optional binary default. Rules are authoritative; the default applies
// only when no rule of the level matches.
type permissionLevel struct {
	rules        []permissionRule
	hasDefault   bool
	defaultAllow bool
}

// PermissionPolicy is the resolved automatic permission policy: the parsed
// global and Workspace levels, evaluated against the fixed built-in policy.
// It has no public mutation and no plugin contribution point. The zero
// PermissionPolicy is the built-in policy; it is the included
// PermissionEvaluator, and a nil evaluator selects it.
type PermissionPolicy struct {
	global    permissionLevel
	workspace permissionLevel
}

// ResolvePermissionPolicy parses the global and Workspace permission
// configuration blocks into one complete usable policy. It never returns an
// error and never reads files: a nil or empty block is an absent level, a
// null block is malformed, and a malformed supplied block discards both user
// levels for this resolution, leaving the built-in policy.
func ResolvePermissionPolicy(global, workspace json.RawMessage) PermissionPolicy {
	globalLevel, globalOK := parsePermissionLevel(global)
	workspaceLevel, workspaceOK := parsePermissionLevel(workspace)
	if !globalOK || !workspaceOK {
		return PermissionPolicy{}
	}
	return PermissionPolicy{global: globalLevel, workspace: workspaceLevel}
}

// evaluate decides one declared permission/target pair through the single
// fallback chain: the last matching Workspace rule, then the Workspace
// default if present, then the last matching global rule, then the global
// default if present, then the built-in rules in their fixed order, then the
// built-in default deny. canonicalRoot is the call's prepared canonical
// Workspace root: evaluation input for the built-in file and inspect rules,
// never policy state.
func (p PermissionPolicy) evaluate(canonicalRoot string, req PermissionRequest) bool {
	if allow, decided := p.workspace.decide(req); decided {
		return allow
	}
	if allow, decided := p.global.decide(req); decided {
		return allow
	}
	return builtinDecide(canonicalRoot, req)
}

// callAllowed reports whether a prepared call may run: every one of its
// declared permission/target pairs must allow.
func (p PermissionPolicy) callAllowed(canonicalRoot string, requests []PermissionRequest) bool {
	for _, req := range requests {
		if !p.evaluate(canonicalRoot, req) {
			return false
		}
	}
	return true
}

// Evaluate adapts the resolved policy to the PermissionEvaluator contract:
// the prepared action is allowed exactly when every one of its declared
// permission/target pairs allows. It never errors.
func (p PermissionPolicy) Evaluate(_ context.Context, action PreparedAction) (PermissionDecision, error) {
	if p.callAllowed(action.CanonicalWorkspace, action.Permissions) {
		return PermissionAllow, nil
	}
	return PermissionDeny, nil
}

// decide returns the level's answer for one request and whether the level
// decides it at all.
func (l permissionLevel) decide(req PermissionRequest) (allow bool, decided bool) {
	for _, rule := range l.rules {
		if rule.permission == req.Permission && rule.glob.MatchString(req.Target) {
			allow, decided = rule.allow, true
		}
	}
	if decided {
		return allow, true
	}
	if l.hasDefault {
		return l.defaultAllow, true
	}
	return false, false
}

// builtinRule is one fixed built-in rule. File basename rules match the glob
// against filepath.Base of the canonical target; every user rule matches the
// complete canonical target instead.
type builtinRule struct {
	permission string
	matches    func(canonicalRoot, target string) bool
	allow      bool
}

// sensitiveBasenamePatterns deny these Workspace file basenames for both
// file permissions. They are basename globs only: no root-prefixed sensitive
// globs exist, so a sensitive-looking ancestor directory never denies the
// files beneath it.
var sensitiveBasenamePatterns = []string{
	".env", ".env.*", ".netrc", ".npmrc", ".pypirc",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.keystore", "*.jks",
	"id_rsa", "id_rsa.*", "id_ed25519", "id_ed25519.*",
	"id_ecdsa", "id_ecdsa.*", "id_dsa", "id_dsa.*",
	"credentials*.json", "credentials*.yaml", "credentials*.yml",
}

// builtinRules is the fixed built-in policy in evaluation order: in-Workspace
// file allows first, sensitive-basename denials after them so they win under
// last-match, then the target permissions. The final built-in default is
// deny. Compiled once at package initialization.
var builtinRules = buildBuiltinRules()

func buildBuiltinRules() []builtinRule {
	rules := []builtinRule{
		{permission: PermissionFileRead, matches: insideWorkspace, allow: true},
		{permission: PermissionFileWrite, matches: insideWorkspace, allow: true},
	}
	for _, pattern := range sensitiveBasenamePatterns {
		deny := denySensitiveBasename(pattern)
		rules = append(rules,
			builtinRule{permission: PermissionFileRead, matches: deny, allow: false},
			builtinRule{permission: PermissionFileWrite, matches: deny, allow: false},
		)
	}
	return append(rules,
		builtinRule{permission: PermissionCommandRun, matches: anyTarget, allow: true},
		builtinRule{permission: PermissionWorkspaceInspect, matches: equalsWorkspaceRoot, allow: true},
		builtinRule{permission: PermissionSleep, matches: equalsFixedTarget, allow: true},
		builtinRule{permission: PermissionChildLaunch, matches: anyTarget, allow: true},
		builtinRule{permission: PermissionProcessOwn, matches: anyTarget, allow: true},
	)
}

func denySensitiveBasename(pattern string) func(canonicalRoot, target string) bool {
	glob := mustCompileGlob(pattern)
	return func(_, target string) bool { return glob.MatchString(filepath.Base(target)) }
}

func insideWorkspace(canonicalRoot, target string) bool {
	return containsPath(canonicalRoot, target)
}

func equalsWorkspaceRoot(canonicalRoot, target string) bool {
	return target == canonicalRoot
}

func anyTarget(_, _ string) bool { return true }

// equalsFixedTarget matches only the exact fixed target "*" that
// target-independent operations declare; other sleep targets fall through to
// the built-in default deny.
func equalsFixedTarget(_, target string) bool { return target == "*" }

// containsPath reports whether target is at or under canonicalRoot, derived
// with filepath.Rel.
func containsPath(canonicalRoot, target string) bool {
	rel, err := filepath.Rel(canonicalRoot, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// builtinDecide applies the built-in rules with last-match semantics; no
// match means the built-in default deny.
func builtinDecide(canonicalRoot string, req PermissionRequest) bool {
	for i := len(builtinRules) - 1; i >= 0; i-- {
		rule := builtinRules[i]
		if rule.permission == req.Permission && rule.matches(canonicalRoot, req.Target) {
			return rule.allow
		}
	}
	return false
}

// compileGlob compiles one permission target glob into an anchored, dot-all
// regexp: '*' matches zero or more runes including separators and newlines,
// '?' matches exactly one rune, and a backslash quotes the rune that
// follows. A trailing backslash is invalid. Consecutive stars carry ordinary
// star meaning; every other character, including brackets and braces, is
// literal.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	runes := []rune(pattern)
	var expr strings.Builder
	expr.WriteString(`(?s)\A`)
	var literal []rune
	flush := func() {
		expr.WriteString(regexp.QuoteMeta(string(literal)))
		literal = literal[:0]
	}
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			flush()
			expr.WriteString(`.*`)
		case '?':
			flush()
			expr.WriteString(`.`)
		case '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("glob pattern %q ends with a trailing backslash", pattern)
			}
			i++
			literal = append(literal, runes[i])
		default:
			literal = append(literal, runes[i])
		}
	}
	flush()
	expr.WriteString(`\z`)
	return regexp.Compile(expr.String())
}

// mustCompileGlob compiles a fixed built-in pattern; failure is a programming
// error in built-in data, not user input.
func mustCompileGlob(pattern string) *regexp.Regexp {
	glob, err := compileGlob(pattern)
	if err != nil {
		panic("harness: invalid built-in permission glob: " + err.Error())
	}
	return glob
}

// parsePermissionLevel parses one supplied block: exactly one JSON object
// with only the optional "default" and optional "rules" members. Duplicate
// members resolve by encoding/json's ordinary last-value decoding. It reports
// whether the block is well-formed.
func parsePermissionLevel(raw json.RawMessage) (permissionLevel, bool) {
	if len(raw) == 0 {
		return permissionLevel{}, true
	}
	fields, err := decodePayloadObject(raw)
	if err != nil {
		return permissionLevel{}, false
	}
	var level permissionLevel
	for name, value := range fields {
		switch name {
		case "default":
			var access string
			if err := json.Unmarshal(value, &access); err != nil {
				return permissionLevel{}, false
			}
			switch access {
			case permissionAccessAllow:
				level.hasDefault, level.defaultAllow = true, true
			case permissionAccessDeny:
				level.hasDefault = true
			default:
				return permissionLevel{}, false
			}
		case "rules":
			rules, ok := parsePermissionRules(value)
			if !ok {
				return permissionLevel{}, false
			}
			level.rules = rules
		default:
			return permissionLevel{}, false
		}
	}
	return level, true
}

// parsePermissionRules parses a present rules member: it must be a JSON
// array (null is malformed, not absence), whose elements are rule objects
// with exactly the required nonempty string fields "permission", "target",
// and "access".
func parsePermissionRules(raw json.RawMessage) ([]permissionRule, bool) {
	var encoded []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil || encoded == nil {
		return nil, false
	}
	var rules []permissionRule
	for _, fields := range encoded {
		rule, ok := parsePermissionRule(fields)
		if !ok {
			return nil, false
		}
		rules = append(rules, rule)
	}
	return rules, true
}

func parsePermissionRule(fields map[string]json.RawMessage) (permissionRule, bool) {
	if len(fields) != 3 {
		return permissionRule{}, false
	}
	permission, ok := requiredRuleString(fields, "permission")
	if !ok {
		return permissionRule{}, false
	}
	target, ok := requiredRuleString(fields, "target")
	if !ok {
		return permissionRule{}, false
	}
	access, ok := requiredRuleString(fields, "access")
	if !ok {
		return permissionRule{}, false
	}
	if access != permissionAccessAllow && access != permissionAccessDeny {
		return permissionRule{}, false
	}
	glob, err := compileGlob(target)
	if err != nil {
		return permissionRule{}, false
	}
	return permissionRule{permission: permission, glob: glob, allow: access == permissionAccessAllow}, true
}

func requiredRuleString(fields map[string]json.RawMessage, name string) (string, bool) {
	value, present := fields[name]
	if !present {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}
