package permission

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/shellparse"
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
	segments, err := shellparse.Parse(cmd)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment.Text != "" {
			parts = append(parts, segment.Text)
		}
	}
	return parts, nil
}

// ruleMatches checks whether a single parsed rule matches the given tool call.
func ruleMatches(r parsedRule, toolName, arg string, ctx evalContext) bool {
	if r.tool != toolName {
		return false
	}
	if toolName == "run_command" {
		return matchCommand(r.pattern, arg)
	}
	if toolName == "process" {
		return matchGlob(r.pattern, arg)
	}
	absPattern := ctx.resolvePath(r.pattern)
	if IsFileTool(toolName) {
		absPattern = pathutil.ResolvePathPattern(absPattern)
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

// hasMalformedRule reports whether the bucket contains any rule that fails
// to parse. Used by the deny/ask buckets to fail closed: a malformed rule
// in those buckets forces the bucket to "match" so it cannot be skipped
// past a broad allow. Allow callers do not consult this — malformed allow
// rules stay skipped so they cannot silently authorize.
func hasMalformedRule(rules []string) bool {
	for _, s := range rules {
		if _, err := parseRule(s); err != nil {
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
	if IsFileTool(toolName) {
		sensitiveArg = pathutil.ResolvePathPattern(arg)
		arg = sensitiveArg
	}
	if hasMalformedRule(rules.Deny) || evaluateRules(rules.Deny, toolName, arg, ctx) {
		return DecisionDeny
	}
	if hasMalformedRule(rules.Ask) || evaluateRules(rules.Ask, toolName, arg, ctx) {
		return DecisionAsk
	}
	if IsFileTool(toolName) && IsSensitivePath(sensitiveArg) {
		return DecisionAsk
	}
	if evaluateRules(rules.Allow, toolName, arg, ctx) {
		return DecisionAllow
	}
	return DecisionAsk
}

func IsFileTool(toolName string) bool {
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

func IsSensitivePath(path string) bool {
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
	analysis := analyzeCommand(command)
	return evaluateCommandAnalysis(rules, analysis, ctx)
}

type commandAnalysis struct {
	segments []commandSegmentAnalysis
}

type commandSegmentAnalysis struct {
	variants      []string
	allowVariants []string
	children      []commandSegmentAnalysis
	unsupported   bool
}

func evaluateCommandAnalysis(rules Rules, analysis commandAnalysis, ctx evalContext) Decision {
	if len(analysis.segments) == 0 {
		return DecisionAsk
	}
	if hasMalformedRule(rules.Deny) || commandRulesMatch(rules.Deny, analysis, ctx) {
		return DecisionDeny
	}
	if hasMalformedRule(rules.Ask) || commandRulesMatch(rules.Ask, analysis, ctx) {
		return DecisionAsk
	}
	if commandAnalysisUnsupported(analysis) {
		return DecisionAsk
	}
	if commandAnalysisAllowed(rules.Allow, analysis, ctx) {
		return DecisionAllow
	}
	return DecisionAsk
}

func analyzeCommand(command string) commandAnalysis {
	segments, err := shellparse.Parse(command)
	if err != nil {
		return commandAnalysis{
			segments: []commandSegmentAnalysis{{
				variants:    commandVariants(command),
				unsupported: true,
			}},
		}
	}
	analysis := commandAnalysis{
		segments: make([]commandSegmentAnalysis, 0, len(segments)),
	}
	// Bare `&` (segment backgrounding) flags the segment plus the one that
	// follows as unsupported so allow rules cannot authorize them. Inner
	// deny/ask rules still match through commandRulesMatch, preserving
	// precedence. `&&` and `|&` are different separators and unaffected.
	prevBackground := false
	for _, segment := range segments {
		seg := analyzeCommandSegment(segment)
		if prevBackground {
			seg.unsupported = true
		}
		if segment.Separator == "&" {
			seg.unsupported = true
			prevBackground = true
		} else {
			prevBackground = false
		}
		analysis.segments = append(analysis.segments, seg)
	}
	return analysis
}

func analyzeCommandSegment(segment shellparse.Segment) commandSegmentAnalysis {
	surfaceVariants := commandVariants(segment.Text, segment.Normalized)
	analysis := commandSegmentAnalysis{
		variants:      surfaceVariants,
		allowVariants: surfaceVariants,
	}
	if segment.UnsafeExpansion || hasUnsafeRedirection(segment) {
		analysis.unsupported = true
	}
	if len(segment.Argv) == 0 {
		return analysis
	}
	effective := analyzeArgvCommand(segment.Argv)
	analysis.variants = mergeCommandVariants(analysis.variants, effective.variants...)
	analysis.allowVariants = mergeCommandVariants(analysis.allowVariants, effective.allowVariants...)
	analysis.children = append(analysis.children, effective.children...)
	if effective.unsupported {
		analysis.unsupported = true
	}
	return analysis
}

func analyzeArgvCommand(argv []string) commandSegmentAnalysis {
	var analysis commandSegmentAnalysis
	if len(argv) == 0 {
		return analysis
	}
	if stripped := stripLeadingAssignments(argv); len(stripped) != len(argv) {
		if len(stripped) == 0 {
			return analysis
		}
		analysis.variants = append(analysis.variants, normalizedArgv(stripped))
		analysis.mergeInspection(analyzeArgvCommand(stripped))
		return analysis
	}

	name := argv[0]
	if isUnsupportedShellCommand(name) || hasUnsupportedShellCommandSpelling(name) {
		analysis.unsupported = true
		return analysis
	}
	switch name {
	case "command":
		if len(argv) < 2 {
			return analysis
		}
		next := argv[1:]
		if next[0] == "--" {
			next = next[1:]
		} else if strings.HasPrefix(next[0], "-") {
			analysis.unsupported = true
			return analysis
		}
		if len(next) == 0 {
			return analysis
		}
		analysis.variants = append(analysis.variants, normalizedArgv(next))
		analysis.allowVariants = append(analysis.allowVariants, normalizedArgv(next))
		analysis.merge(analyzeArgvCommand(next))
	case "env":
		next, ok := unwrapEnvArgv(argv[1:])
		if !ok {
			analysis.unsupported = true
			return analysis
		}
		if len(next) == 0 {
			return analysis
		}
		analysis.variants = append(analysis.variants, normalizedArgv(next))
		analysis.mergeInspection(analyzeArgvCommand(next))
	case "sh", "bash", "dash", "zsh":
		script, ok := shellScriptArg(argv)
		if !ok {
			analysis.unsupported = true
			return analysis
		}
		child := analyzeCommand(script)
		analysis.children = append(analysis.children, child.segments...)
	default:
		if isShellReservedWord(name) {
			analysis.unsupported = true
		}
	}
	return analysis
}

func (analysis *commandSegmentAnalysis) merge(other commandSegmentAnalysis) {
	analysis.variants = mergeCommandVariants(analysis.variants, other.variants...)
	analysis.allowVariants = mergeCommandVariants(analysis.allowVariants, other.allowVariants...)
	analysis.children = append(analysis.children, other.children...)
	if other.unsupported {
		analysis.unsupported = true
	}
}

func (analysis *commandSegmentAnalysis) mergeInspection(other commandSegmentAnalysis) {
	analysis.variants = mergeCommandVariants(analysis.variants, other.variants...)
	analysis.children = append(analysis.children, other.children...)
	if other.unsupported {
		analysis.unsupported = true
	}
}

func stripLeadingAssignments(argv []string) []string {
	for len(argv) > 0 && isShellAssignment(argv[0]) {
		argv = argv[1:]
	}
	return argv
}

func unwrapEnvArgv(argv []string) ([]string, bool) {
	for len(argv) > 0 {
		arg := argv[0]
		if strings.HasPrefix(arg, "-") {
			return nil, false
		}
		if !isShellAssignment(arg) {
			break
		}
		argv = argv[1:]
	}
	return argv, true
}

func shellScriptArg(argv []string) (string, bool) {
	if len(argv) < 3 || argv[1] != "-c" {
		return "", false
	}
	return argv[2], true
}

func isShellAssignment(arg string) bool {
	idx := strings.IndexByte(arg, '=')
	if idx <= 0 {
		return false
	}
	for i, r := range arg[:idx] {
		if i == 0 {
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func isUnsupportedShellCommand(name string) bool {
	switch name {
	case "eval", "exec", "time", ".", "source", "trap", "alias", "builtin",
		"busybox", "nohup", "nice", "timeout", "setsid", "chroot", "unshare",
		"sudo", "doas":
		return true
	default:
		return false
	}
}

func isShellReservedWord(name string) bool {
	switch name {
	case "!", "{", "}", "if", "then", "else", "elif", "fi", "for", "select",
		"while", "until", "do", "done", "case", "esac", "in", "function":
		return true
	default:
		return false
	}
}

func hasUnsupportedShellCommandSpelling(name string) bool {
	return strings.HasPrefix(name, "(") || name == ")" || strings.HasSuffix(name, ")")
}

func normalizedArgv(argv []string) string {
	return strings.Join(argv, " ")
}

func commandVariants(values ...string) []string {
	var variants []string
	return mergeCommandVariants(variants, values...)
}

func mergeCommandVariants(existing []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen := false
		for _, current := range existing {
			if current == value {
				seen = true
				break
			}
		}
		if !seen {
			existing = append(existing, value)
		}
	}
	return existing
}

func commandRulesMatch(rules []string, analysis commandAnalysis, ctx evalContext) bool {
	for _, segment := range analysis.segments {
		if commandSegmentRulesMatch(rules, segment, ctx) {
			return true
		}
	}
	return false
}

func commandSegmentRulesMatch(rules []string, segment commandSegmentAnalysis, ctx evalContext) bool {
	if commandVariantsMatchRules(rules, segment.variants, ctx) {
		return true
	}
	for _, child := range segment.children {
		if commandSegmentRulesMatch(rules, child, ctx) {
			return true
		}
	}
	return false
}

func commandVariantsMatchRules(rules []string, variants []string, ctx evalContext) bool {
	for _, variant := range variants {
		if evaluateRules(rules, "run_command", variant, ctx) {
			return true
		}
	}
	return false
}

func commandAnalysisUnsupported(analysis commandAnalysis) bool {
	for _, segment := range analysis.segments {
		if commandSegmentUnsupported(segment) {
			return true
		}
	}
	return false
}

func commandSegmentUnsupported(segment commandSegmentAnalysis) bool {
	if segment.unsupported {
		return true
	}
	for _, child := range segment.children {
		if commandSegmentUnsupported(child) {
			return true
		}
	}
	return false
}

func commandAnalysisAllowed(rules []string, analysis commandAnalysis, ctx evalContext) bool {
	for _, segment := range analysis.segments {
		if !commandSegmentAllowed(rules, segment, ctx) {
			return false
		}
	}
	return true
}

func commandSegmentAllowed(rules []string, segment commandSegmentAnalysis, ctx evalContext) bool {
	if segment.unsupported {
		return false
	}
	hasExactMatch, hasWildcardMatch := false, false
	for _, s := range rules {
		r, err := parseRule(s)
		if err != nil || r.tool != "run_command" {
			continue
		}
		for _, v := range segment.allowVariants {
			if !matchCommand(r.pattern, v) {
				continue
			}
			if wrapperScriptHasWildcard(r.pattern) {
				hasWildcardMatch = true
			} else {
				hasExactMatch = true
			}
			break
		}
		if hasExactMatch {
			break
		}
	}
	if !hasExactMatch && !hasWildcardMatch {
		return false
	}
	if hasExactMatch {
		return true
	}
	for _, child := range segment.children {
		if !commandSegmentAllowed(rules, child, ctx) {
			return false
		}
	}
	return true
}

// wrapperScriptHasWildcard reports whether pattern is a shell-wrapper rule
// of the form `sh|bash|dash|zsh -c <body>` where <body> contains a literal
// `*`. Such wildcard wrappers must additionally require every parsed child
// segment to be independently allowed; exact-wrapper rules (no `*` in the
// script body) keep the round-trip behavior of allowing on outer match.
func wrapperScriptHasWildcard(pattern string) bool {
	segments, err := shellparse.Parse(pattern)
	if err != nil || len(segments) == 0 {
		return false
	}
	argv := segments[0].Argv
	if len(argv) < 3 {
		return false
	}
	switch argv[0] {
	case "sh", "bash", "dash", "zsh":
	default:
		return false
	}
	if argv[1] != "-c" {
		return false
	}
	return strings.ContainsRune(argv[2], '*')
}

func hasUnsafeRedirection(segment shellparse.Segment) bool {
	for _, redirect := range segment.Redirections {
		if !redirect.SafeFDDup {
			return true
		}
	}
	return false
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
// A malformed deny/ask rule in the local set also counts as an explicit match
// so the fail-closed semantics do not silently fall through to the global set.
func hasExplicitMatch(rules Rules, toolName, arg string, ctx evalContext) bool {
	if hasMalformedRule(rules.Deny) || hasMalformedRule(rules.Ask) {
		return true
	}
	if toolName == "run_command" {
		analysis := analyzeCommand(arg)
		return commandRulesMatch(rules.Allow, analysis, ctx) ||
			commandRulesMatch(rules.Deny, analysis, ctx) ||
			commandRulesMatch(rules.Ask, analysis, ctx)
	}
	if IsFileTool(toolName) {
		arg = pathutil.ResolvePathPattern(arg)
	}
	return evaluateRules(rules.Allow, toolName, arg, ctx) ||
		evaluateRules(rules.Deny, toolName, arg, ctx) ||
		evaluateRules(rules.Ask, toolName, arg, ctx)
}
