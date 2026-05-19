package permission

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/pathutil"
)

// Rules is the shape of both global permissions (in config.json) and
// local permissions (in projects/<id>/permissions.json).
type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	Ask   []string `json:"ask,omitempty"`
}

// Decision is the outcome of evaluating rules against a tool call.
type Decision int

const (
	DecisionAsk   Decision = iota // no rule matched — default
	DecisionAllow                 // an allow rule matched
	DecisionDeny                  // a deny rule matched
)

// parsedRule is a decomposed Tool(pattern) string.
type parsedRule struct {
	tool    string
	pattern string
}

// parseRule splits "tool_name(pattern)" into its parts.
func parseRule(s string) (parsedRule, error) {
	idx := strings.IndexByte(s, '(')
	if idx < 1 || !strings.HasSuffix(s, ")") {
		return parsedRule{}, fmt.Errorf("invalid rule %q: expected Tool(pattern)", s)
	}
	return parsedRule{
		tool:    s[:idx],
		pattern: s[idx+1 : len(s)-1],
	}, nil
}

// resolvePath expands the path prefix convention to an absolute glob pattern.
//
//	/foo    → <projectRoot>/foo   (project-relative)
//	~/foo   → <home>/foo          (home-relative)
//	//foo   → /foo                (absolute)
//	foo     → <cwd>/foo           (cwd-relative)
func resolvePath(pattern, projectRoot, home, cwd string) string {
	return newEvalContext(projectRoot, home, cwd).resolvePath(pattern)
}

type evalContext struct {
	projectRoot string
	home        string
	cwd         string
}

func newEvalContext(projectRoot, home, cwd string) evalContext {
	return evalContext{
		projectRoot: canonicalBase(projectRoot),
		home:        canonicalBase(home),
		cwd:         canonicalBase(cwd),
	}
}

func (ctx evalContext) resolvePath(pattern string) string {
	switch {
	case strings.HasPrefix(pattern, "//"):
		return pattern[1:] // "//etc/passwd" → "/etc/passwd"
	case strings.HasPrefix(pattern, "~/"):
		return filepath.Join(ctx.home, pattern[2:])
	case strings.HasPrefix(pattern, "/"):
		return filepath.Join(ctx.projectRoot, pattern[1:])
	default:
		return filepath.Join(ctx.cwd, pattern)
	}
}

func canonicalBase(path string) string {
	if path == "" {
		return path
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	resolved, _, err := pathutil.ResolveAbsPath(absPath)
	if err != nil {
		return filepath.Clean(absPath)
	}
	return resolved
}

// matchGlob matches path against pattern, supporting ** for zero or more
// directory segments, * for a single-segment wildcard, and ? for a single
// character. Both pattern and path are split by filepath.Separator.
func matchGlob(pattern, path string) bool {
	patParts := splitPath(pattern)
	pathParts := splitPath(path)
	return matchParts(patParts, pathParts)
}

func splitPath(p string) []string {
	p = filepath.Clean(p)
	return strings.Split(p, string(filepath.Separator))
}

func matchParts(pat, path []string) bool {
	for len(pat) > 0 && len(path) > 0 {
		if pat[0] == "**" {
			// ** matches zero or more segments.
			pat = pat[1:]
			// Skip consecutive ** entries.
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true // trailing ** matches everything
			}
			// Try matching the rest of the pattern against every
			// suffix of path.
			for i := 0; i <= len(path); i++ {
				if matchParts(pat, path[i:]) {
					return true
				}
			}
			return false
		}
		// Single segment match (supports * and ? within segment).
		ok, _ := filepath.Match(pat[0], path[0])
		if !ok {
			return false
		}
		pat = pat[1:]
		path = path[1:]
	}
	// Handle trailing ** in pattern.
	for len(pat) > 0 && pat[0] == "**" {
		pat = pat[1:]
	}
	return len(pat) == 0 && len(path) == 0
}

// matchCommand matches shell command rules. Exact rules are literal strings;
// rules containing * use * as a command wildcard. Backslashes and other
// shell characters remain literal command text, not filepath glob escapes.
func matchCommand(pattern, command string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == command
	}
	return matchCommandWildcard(pattern, command)
}

func matchCommandWildcard(pattern, command string) bool {
	p, c := 0, 0
	star := -1
	matchedAtStar := 0

	for c < len(command) {
		if p < len(pattern) && pattern[p] == '*' {
			star = p
			matchedAtStar = c
			p++
			continue
		}
		if p < len(pattern) && pattern[p] == command[c] {
			p++
			c++
			continue
		}
		if star >= 0 {
			p = star + 1
			matchedAtStar++
			c = matchedAtStar
			continue
		}
		return false
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// DecomposeCommand splits a shell command on &&, ||, &, ;, |, LF, and CR
// respecting single and double quotes. Returns an error if $( or backticks are
// found outside single quotes (command substitution cannot be safely
// pattern-matched).
func DecomposeCommand(cmd string) ([]string, error) {
	var parts []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// Track quote state. Outside single quotes, a backslash-escaped quote
		// is literal shell text and must not hide following operators.
		if r == '\'' && !inDouble && (inSingle || !isEscaped(runes, i)) {
			inSingle = !inSingle
			current.WriteRune(r)
			continue
		}
		if r == '"' && !inSingle && !isEscaped(runes, i) {
			inDouble = !inDouble
			current.WriteRune(r)
			continue
		}

		// Reject command substitution outside single quotes.
		if !inSingle {
			if r == '`' {
				return nil, fmt.Errorf("backtick command substitution not allowed")
			}
			if r == '$' && i+1 < len(runes) && runes[i+1] == '(' {
				return nil, fmt.Errorf("$() command substitution not allowed")
			}
		}

		// Outside quotes, check for operators.
		if !inSingle && !inDouble {
			// && or ||
			if i+1 < len(runes) && ((r == '&' && runes[i+1] == '&') || (r == '|' && runes[i+1] == '|')) {
				s := strings.TrimSpace(current.String())
				if s != "" {
					parts = append(parts, s)
				}
				current.Reset()
				i++ // skip second char of operator
				continue
			}
			if r == '&' && isAmpersandCommandSeparator(runes, i) {
				s := strings.TrimSpace(current.String())
				if s != "" {
					parts = append(parts, s)
				}
				current.Reset()
				continue
			}
			// ;, |, LF, or CR
			if r == ';' || r == '|' || r == '\n' || r == '\r' {
				s := strings.TrimSpace(current.String())
				if s != "" {
					parts = append(parts, s)
				}
				current.Reset()
				continue
			}
		}

		current.WriteRune(r)
	}

	s := strings.TrimSpace(current.String())
	if s != "" {
		parts = append(parts, s)
	}
	return parts, nil
}

func isAmpersandCommandSeparator(runes []rune, i int) bool {
	if i+1 < len(runes) && runes[i+1] == '>' {
		return false
	}
	if i > 0 && (runes[i-1] == '>' || runes[i-1] == '<') {
		return false
	}
	return true
}

func isEscaped(runes []rune, i int) bool {
	backslashes := 0
	for j := i - 1; j >= 0 && runes[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// ruleMatches checks whether a single parsed rule matches the given tool call.
func ruleMatches(r parsedRule, toolName, arg string, ctx evalContext) bool {
	if r.tool != toolName {
		return false
	}
	if toolName == "run_command" {
		return matchCommand(r.pattern, arg)
	}
	absPattern := ctx.resolvePath(r.pattern)
	if isFiletool(toolName) {
		absPattern = pathutil.ResolvePathPattern(absPattern)
		arg = pathutil.ResolvePathPattern(arg)
	}
	return matchGlob(absPattern, arg)
}

// evaluateRules checks all rules in one bucket (allow/deny/ask) for a match.
func evaluateRules(rules []string, toolName, arg string, ctx evalContext) bool {
	for _, s := range rules {
		r, err := parseRule(s)
		if err != nil {
			continue
		}
		if ruleMatches(r, toolName, arg, ctx) {
			return true
		}
	}
	return false
}

// Evaluate checks a tool call against one level of rules.
// Precedence: deny > ask > allow. No match returns DecisionAsk.
//
// For run_command, the command is decomposed first. Each subcommand is
// evaluated independently: any deny → deny, any ask → ask, all allow → allow.
func Evaluate(rules Rules, toolName, arg, projectRoot, home, cwd string) Decision {
	ctx := newEvalContext(projectRoot, home, cwd)
	return evaluate(rules, toolName, arg, ctx)
}

func evaluate(rules Rules, toolName, arg string, ctx evalContext) Decision {
	if toolName == "run_command" {
		return evaluateCommand(rules, arg, ctx)
	}
	return evaluateSingle(rules, toolName, arg, ctx)
}

func evaluateSingle(rules Rules, toolName, arg string, ctx evalContext) Decision {
	sensitiveArg := arg
	if isFiletool(toolName) {
		sensitiveArg = pathutil.ResolvePathPattern(arg)
	}
	if evaluateRules(rules.Deny, toolName, arg, ctx) {
		return DecisionDeny
	}
	if evaluateRules(rules.Ask, toolName, arg, ctx) {
		return DecisionAsk
	}
	if isFiletool(toolName) && isSensitivePath(sensitiveArg) {
		return DecisionAsk
	}
	if evaluateRules(rules.Allow, toolName, arg, ctx) {
		return DecisionAllow
	}
	return DecisionAsk
}

func isFiletool(toolName string) bool {
	return toolName == "read_file" || toolName == "write_file" || toolName == "edit_file"
}

var sensitiveNames = map[string]bool{
	".env":       true,
	".netrc":     true,
	".npmrc":     true,
	".pypirc":    true,
	"id_rsa":     true,
	"id_ed25519": true,
	"id_ecdsa":   true,
	"id_dsa":     true,
}

var sensitiveGlobs = []string{
	".env.*",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"*.keystore",
	"*.jks",
	"id_rsa.*",
	"id_ed25519.*",
	"id_ecdsa.*",
	"id_dsa.*",
	"credentials*.json",
	"credentials*.yaml",
	"credentials*.yml",
}

func isSensitivePath(path string) bool {
	base := filepath.Base(path)
	if sensitiveNames[base] {
		return true
	}
	for _, g := range sensitiveGlobs {
		if matched, _ := filepath.Match(g, base); matched {
			return true
		}
	}
	return false
}

func evaluateCommand(rules Rules, command string, ctx evalContext) Decision {
	subs, err := DecomposeCommand(command)
	if err != nil {
		return DecisionAsk // substitution detected → always ask
	}
	if len(subs) == 0 {
		return DecisionAsk
	}

	worst := DecisionAllow
	for _, sub := range subs {
		d := evaluateSingle(rules, "run_command", sub, ctx)
		if d == DecisionDeny {
			return DecisionDeny
		}
		if d == DecisionAsk {
			worst = DecisionAsk
		}
	}
	return worst
}

// Check evaluates local rules first, then global. Local overrides global:
// if any rule matches in local, that decision is final. If no local rule
// matches, global is checked. If neither matches, the default is DecisionAsk.
func Check(local, global Rules, toolName, arg, projectRoot, home, cwd string) Decision {
	ctx := newEvalContext(projectRoot, home, cwd)
	d := evaluate(local, toolName, arg, ctx)
	if d != DecisionAsk {
		return d
	}
	// Local had no matching rule (returned default ask). Check if any local
	// rule explicitly matched as ask vs. simply no-match.
	if hasExplicitMatch(local, toolName, arg, ctx) {
		return DecisionAsk
	}
	return evaluate(global, toolName, arg, ctx)
}

// hasExplicitMatch returns true if any rule in the set (allow, deny, or ask)
// matches the tool call. This distinguishes "explicitly asked" from "no match".
func hasExplicitMatch(rules Rules, toolName, arg string, ctx evalContext) bool {
	return evaluateRules(rules.Allow, toolName, arg, ctx) ||
		evaluateRules(rules.Deny, toolName, arg, ctx) ||
		evaluateRules(rules.Ask, toolName, arg, ctx)
}
