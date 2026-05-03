package cli

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/MMinasyan/lightcode/internal/agent"
)

const nl = "\r\n"

const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorItalic = "\033[3m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorInv    = "\033[7m"
)

func eraseBlock(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\r\x1b[2K")
	for i := 0; i < n-1; i++ {
		b.WriteString("\x1b[1A\r\x1b[2K")
	}
	return b.String()
}

// renderUserMsg renders a user message with horizontal borders.
func renderUserMsg(content string, width int) string {
	if width < 20 {
		width = 20
	}
	lines := wrapLine(content, width-4)
	collapsed := false
	if len(lines) > 5 {
		lines = lines[:5]
		collapsed = true
	}

	var b strings.Builder
	b.WriteString(colorDim)
	b.WriteString(" ")
	b.WriteString(strings.Repeat("─", width-1))
	b.WriteString(colorReset)
	b.WriteString(nl)

	for _, line := range lines {
		b.WriteString("   ")
		b.WriteString(line)
		b.WriteString(nl)
	}

	if collapsed {
		b.WriteString(colorDim)
		b.WriteString("   (more)")
		b.WriteString(colorReset)
		b.WriteString(nl)
	}

	b.WriteString(colorDim)
	b.WriteString(" ")
	b.WriteString(strings.Repeat("─", width-1))
	b.WriteString(colorReset)
	b.WriteString(nl)

	return b.String()
}

// renderQueuedMsg renders a queued user message in dim/gray.
func renderQueuedMsg(content string, width int) string {
	if width < 20 {
		width = 20
	}
	lines := wrapLine(content, width-4)
	if len(lines) > 3 {
		lines = lines[:3]
	}

	var b strings.Builder
	b.WriteString(colorGray)
	b.WriteString(" ┌")
	b.WriteString(strings.Repeat("─", width-2))
	b.WriteString(nl)

	for _, line := range lines {
		b.WriteString("   ")
		b.WriteString(line)
		b.WriteString(nl)
	}

	b.WriteString(" └")
	b.WriteString(strings.Repeat("─", width-2))
	b.WriteString(colorReset)
	b.WriteString(nl)

	return b.String()
}

// renderAssistantMsg renders assistant text with basic markdown→ANSI.
func renderAssistantMsg(content string, width int) string {
	if width < 20 {
		width = 20
	}
	indent := "  "
	inner := width - 2
	lines := strings.Split(content, "\n")

	var b strings.Builder
	prevBlank := false
	for _, line := range lines {
		isBlank := strings.TrimSpace(line) == ""
		if isBlank && prevBlank {
			continue
		}
		prevBlank = isBlank
		rendered := renderMarkdownLine(line)
		wrapped := wrapLine(rendered, inner)
		for _, wl := range wrapped {
			b.WriteString(indent)
			b.WriteString(wl)
			b.WriteString(nl)
		}
	}
	return b.String()
}

// renderToolCall renders a tool call header line.
func renderToolCall(name, args string) string {
	argDisplay := formatToolArgs(name, args)
	var b strings.Builder
	b.WriteString(colorCyan)
	b.WriteString("  ⟩ ")
	b.WriteString(name)
	if argDisplay != "" {
		b.WriteString("  ")
		b.WriteString(argDisplay)
	}
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

// renderToolResult renders a tool call result.
func renderToolResult(result string, success bool, expanded bool, width int) string {
	if width < 20 {
		width = 20
	}
	indent := "     "
	inner := width - 5

	var b strings.Builder
	if !success {
		b.WriteString(colorRed)
		truncated := truncate(result, inner)
		b.WriteString(indent)
		b.WriteString(truncated)
		b.WriteString(colorReset)
		b.WriteString(nl)
		return b.String()
	}

	lines := strings.Split(result, "\n")
	if !expanded && len(lines) > 3 {
		lines = lines[:3]
	}

	color := colorDim
	b.WriteString(color)
	for i, line := range lines {
		truncated := truncate(line, inner)
		b.WriteString(indent)
		b.WriteString(truncated)
		if i < len(lines)-1 {
			b.WriteString(nl)
		}
	}
	b.WriteString(colorReset)
	b.WriteString(nl)

	return b.String()
}

// renderSubagentMsg renders a subagent event line.
func renderSubagentMsg(sessionID, content string) string {
	var b strings.Builder
	b.WriteString(colorDim)
	b.WriteString("[subagent:")
	b.WriteString(sessionID)
	b.WriteString("] ")
	b.WriteString(content)
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

// renderSystemMsg renders a system message (dim italic).
func renderSystemMsg(content string) string {
	var b strings.Builder
	b.WriteString(colorDim)
	b.WriteString(colorItalic)
	b.WriteString("  ")
	b.WriteString(content)
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

// renderErrorMsg renders an error message.
func renderErrorMsg(content string) string {
	var b strings.Builder
	b.WriteString(colorRed)
	b.WriteString("  ✕ ")
	b.WriteString(content)
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

// renderWarningMsg renders a warning message.
func renderWarningMsg(content string) string {
	var b strings.Builder
	b.WriteString(colorYellow)
	b.WriteString("  ⚠ ")
	b.WriteString(content)
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

func renderMarkdownLine(line string) string {
	line = replaceInline(line, "**", colorBold, colorReset)
	line = replaceInline(line, "`", colorDim, colorReset)
	return line
}

func replaceInline(s, marker, open, close string) string {
	for {
		start := strings.Index(s, marker)
		if start == -1 {
			return s
		}
		end := strings.Index(s[start+len(marker):], marker)
		if end == -1 {
			return s
		}
		end += start + len(marker)
		inner := s[start+len(marker) : end]
		s = s[:start] + open + inner + close + s[end+len(marker):]
	}
}

func formatToolArgs(name, args string) string {
	switch name {
	case "read_file", "write_file", "edit_file":
		return extractJSONString(args, "path")
	case "run_command":
		return extractJSONString(args, "command")
	case "task":
		prompt := extractJSONString(args, "prompt")
		if prompt != "" {
			return truncate(prompt, 80)
		}
		return truncate(args, 80)
	default:
		return truncate(args, 80)
	}
}

func extractJSONString(jsonStr, key string) string {
	search := `"` + key + `"`
	idx := strings.Index(jsonStr, search)
	if idx == -1 {
		return ""
	}
	rest := jsonStr[idx+len(search):]
	rest = strings.TrimLeft(rest, " \t\n\r:")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(rest[i+1])
			}
			i++
			continue
		}
		if rest[i] == '"' {
			return b.String()
		}
		b.WriteByte(rest[i])
	}
	return b.String()
}

type displayEntry struct {
	typ     string
	content string
	turn    int
	name    string
	args    string
	done    bool
	success bool
	result  string
}

func buildDisplayMsgs(msgs []agent.DisplayMessage) []displayEntry {
	out := make([]displayEntry, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, displayEntry{
			typ:     m.Type,
			content: m.Content,
			turn:    m.Turn,
			name:    m.Name,
			args:    m.Args,
			done:    m.Done,
			success: m.Success,
			result:  m.Result,
		})
	}
	return out
}

func fmtTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1_000_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
}

func truncate(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	w := visibleWidth(s)
	if w <= maxW {
		return s
	}
	if maxW <= 3 {
		return s[:maxW]
	}
	result := []rune{}
	currentW := 0
	for _, r := range s {
		rw := runeWidth(r)
		if currentW+rw > maxW-3 {
			break
		}
		result = append(result, r)
		currentW += rw
	}
	return string(result) + "..."
}

func wrapLine(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}

	var lines []string
	current := ""
	currentW := 0

	words := splitKeepAnsi(text)
	for _, word := range words {
		wordW := visibleWidth(word)
		if currentW+wordW > width && currentW > 0 {
			lines = append(lines, current)
			current = ""
			currentW = 0
		}
		current += word
		currentW += wordW
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

func splitKeepAnsi(text string) []string {
	words := strings.Split(text, " ")
	result := make([]string, 0, len(words)*2-1)
	for i, w := range words {
		if i > 0 {
			result = append(result, " ")
		}
		result = append(result, w)
	}
	return result
}

func stripAnsi(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func visibleWidth(s string) int {
	stripped := stripAnsi(s)
	w := 0
	for _, r := range stripped {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	if r == '\t' {
		return 4
	}
	if unicode.IsControl(r) {
		return 0
	}
	if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
		return 2
	}
	return 1
}
