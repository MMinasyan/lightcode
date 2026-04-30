package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
)

const (
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiReset  = "\033[0m"
)

func colorRed(s string) string    { return ansiRed + s + ansiReset }
func colorYellow(s string) string { return ansiYellow + s + ansiReset }
func colorCyan(s string) string   { return ansiCyan + s + ansiReset }
func colorGreen(s string) string  { return ansiGreen + s + ansiReset }
func bold(s string) string        { return ansiBold + s + ansiReset }
func dim(s string) string         { return ansiDim + s + ansiReset }

const maxToolOutputLen = 2000

func renderToolStart(toolName, args string) string {
	return fmt.Sprintf("  %s %s(%s)", colorCyan("▶"), bold(toolName), dim(args))
}

func renderToolEnd(isError bool, result string) string {
	out := result
	if len(out) > maxToolOutputLen {
		out = out[:maxToolOutputLen] + "... (truncated)"
	}
	if isError {
		return fmt.Sprintf("  %s %s", colorRed("✗"), out)
	}
	return fmt.Sprintf("  %s %s", colorGreen("✓"), out)
}

func subagentPrefix(ev agent.Event) string {
	return fmt.Sprintf("[subagent:task%d:%s]", ev.TaskIndex+1, shortID(ev.SubagentSessionID))
}

func renderSubagentToolStart(ev agent.Event) string {
	return fmt.Sprintf("%s %s %s(%s)", subagentPrefix(ev), colorCyan("▶"), bold(ev.ToolName), dim(ev.Args))
}

func renderSubagentToolEnd(ev agent.Event) string {
	out := ev.Result
	if len(out) > maxToolOutputLen {
		out = out[:maxToolOutputLen] + "... (truncated)"
	}
	if ev.IsError {
		return fmt.Sprintf("%s %s %s", subagentPrefix(ev), colorRed("✗"), out)
	}
	return fmt.Sprintf("%s %s %s", subagentPrefix(ev), colorGreen("✓"), out)
}

func renderPermissionPrompt(toolName, arg string) string {
	return fmt.Sprintf(
		"\n%s\n  Tool: %s\n  Arg:  %s\nallow? [y/n/p]: ",
		colorYellow("Permission required:"),
		bold(toolName),
		arg,
	)
}

func renderSuggestions(suggestions []agent.PermissionSuggestion) string {
	var b strings.Builder
	b.WriteString("  Patterns to allow:\n")
	for i, s := range suggestions {
		fmt.Fprintf(&b, "    %d. %s\n", i+1, s.Label)
	}
	b.WriteString("Select patterns (e.g. 1,3 or Enter to cancel): ")
	return b.String()
}

func renderTurnUsage(u turnUsage) string {
	prefix := ""
	if u.estimated {
		prefix = "~"
	}
	total := u.cache + u.input + u.output
	return fmt.Sprintf("Tokens: cache=%s%s input=%s%s output=%s%s total=%s%s",
		prefix, formatInt(u.cache),
		prefix, formatInt(u.input),
		prefix, formatInt(u.output),
		prefix, formatInt(total),
	)
}

func renderSessionLine(s agent.SessionSummary, isCurrent bool) string {
	ts := time.Unix(s.LastActivity, 0).Format("2006-01-02 15:04")
	cur := ""
	if isCurrent {
		cur = "  (current)"
	}
	return fmt.Sprintf("  %s  %s%s", shortID(s.ID), ts, cur)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func formatInt(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	offset := len(s) % 3
	if offset > 0 {
		b.WriteString(s[:offset])
	}
	for i := offset; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func parsePatternSelection(input string, max int) ([]int, error) {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' '
	})
	var indices []int
	seen := make(map[int]bool)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > max {
			return nil, fmt.Errorf("invalid number %q (valid: 1-%d)", p, max)
		}
		if !seen[n] {
			indices = append(indices, n-1)
			seen[n] = true
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("no valid numbers given")
	}
	return indices, nil
}
