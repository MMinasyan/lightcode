package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

const nl = "\r\n"

const collapsedPreviewLines = 5
const collapsedEditPreviewLines = collapsedPreviewLines * 2

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
	if len(lines) > collapsedPreviewLines {
		lines = lines[:collapsedPreviewLines]
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
func renderToolCall(name, args string, metadata map[string]any) string {
	argDisplay := formatToolArgs(name, args)
	summary, hasSummary := toolChangeSummary(name, args, metadata)
	var b strings.Builder
	b.WriteString(colorCyan)
	b.WriteString("  ▸ ")
	b.WriteString(name)
	if argDisplay != "" {
		argLines := strings.Split(argDisplay, "\n")
		b.WriteString("  ")
		b.WriteString(argLines[0])
		for _, line := range argLines[1:] {
			b.WriteString(nl)
			b.WriteString("     ")
			b.WriteString(line)
		}
	}
	if hasSummary {
		b.WriteString("  ")
		writeToolChangeSummary(&b, summary)
	}
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

func renderBackgroundProcessCall(bg *agent.BackgroundProcessDisplay, success bool) string {
	var b strings.Builder
	if success {
		b.WriteString(colorCyan)
	} else {
		b.WriteString(colorRed)
	}
	b.WriteString("  ▸ background_process")
	if bg != nil {
		if bg.ID != "" {
			b.WriteString("  ")
			b.WriteString(formatHeaderArg(bg.ID, 80))
		}
		reason := bg.Reason
		if reason == "" {
			reason = "completed"
		}
		b.WriteString("  ")
		b.WriteString(reason)
		b.WriteString(fmt.Sprintf(" exit %d", bg.ExitCode))
		command := formatMultilineHeaderArg(bg.Command)
		if command != "" {
			lines := strings.Split(command, "\n")
			b.WriteString(nl)
			b.WriteString("     $ ")
			b.WriteString(lines[0])
			for _, line := range lines[1:] {
				b.WriteString(nl)
				b.WriteString("       ")
				b.WriteString(line)
			}
		}
	}
	b.WriteString(colorReset)
	b.WriteString(nl)
	return b.String()
}

type toolChangeCounts struct {
	added   int
	removed int
}

func toolChangeSummary(name, args string, metadata map[string]any) (toolChangeCounts, bool) {
	switch name {
	case "edit_file":
		preview, ok := editpreview.FromMetadata(metadata)
		if !ok {
			return toolChangeCounts{}, false
		}
		counts := editPreviewChangeCounts(preview)
		return counts, counts.added > 0 || counts.removed > 0
	case "write_file":
		lines := countContentLines(extractWriteContent(args))
		if lines <= 0 {
			return toolChangeCounts{}, false
		}
		return toolChangeCounts{added: lines}, true
	default:
		return toolChangeCounts{}, false
	}
}

func writeToolChangeSummary(b *strings.Builder, counts toolChangeCounts) {
	if counts.added > 0 {
		b.WriteString(colorGreen)
		b.WriteString(fmt.Sprintf("+%d", counts.added))
		b.WriteString(colorCyan)
	}
	if counts.removed > 0 {
		if counts.added > 0 {
			b.WriteString(" ")
		}
		b.WriteString(colorRed)
		b.WriteString(fmt.Sprintf("-%d", counts.removed))
		b.WriteString(colorCyan)
	}
}

// renderToolResult renders a tool call result.
func renderToolResult(name, args, result string, success bool, expanded bool, width int, metadata map[string]any) string {
	if width < 20 {
		width = 20
	}
	indent := "     "
	inner := width - 5

	var b strings.Builder

	if !success {
		return renderOutputPreview(result, indent, inner, colorRed, expanded)
	}

	if name == "read_file" {
		return ""
	}

	// edit_file success output is user-facing metadata, not the model-facing summary.
	if name == "edit_file" {
		if preview, ok := editpreview.FromMetadata(metadata); ok {
			b.WriteString(renderEditPreview(preview, indent, inner, expanded))
			return b.String()
		}
		return ""
	}

	display := result
	if name == "write_file" {
		display = extractWriteContent(args)
	} else {
		display = strings.TrimRight(display, "\n")
	}

	return renderOutputPreview(display, indent, inner, colorDim, expanded)
}

func renderEditPreview(preview *editpreview.Preview, indent string, inner int, expanded bool) string {
	if preview == nil || len(preview.Hunks) == 0 {
		return ""
	}
	width := 1
	for _, hunk := range preview.Hunks {
		for _, row := range hunk.Rows {
			line := editpreview.DisplayLine(row)
			if line <= 0 {
				continue
			}
			if n := len(fmt.Sprintf("%d", line)); n > width {
				width = n
			}
		}
	}

	var b strings.Builder
	rowsWritten := 0
	totalRows := editPreviewRowCount(preview)
	limit := totalRows
	collapsed := !expanded && totalRows > collapsedEditPreviewLines
	if collapsed {
		limit = collapsedEditPreviewLines
	}

	for hunkIdx, hunk := range preview.Hunks {
		if rowsWritten >= limit {
			break
		}
		if len(hunk.Rows) == 0 {
			continue
		}
		if expanded && hunkIdx > 0 && rowsWritten > 0 {
			b.WriteString(nl)
		}
		for _, row := range hunk.Rows {
			if rowsWritten >= limit {
				break
			}
			line := editpreview.DisplayLine(row)
			marker := editpreview.Marker(row)
			gutterColor := colorDim
			if row.Kind == editpreview.KindRemove {
				gutterColor = colorRed
			} else if row.Kind == editpreview.KindAdd {
				gutterColor = colorGreen
			}

			gutter := fmt.Sprintf("%*d %s", width, line, marker)
			codeWidth := inner - len(gutter)
			if codeWidth < 1 {
				codeWidth = 1
			}

			b.WriteString(indent)
			b.WriteString(gutterColor)
			b.WriteString(gutter)
			b.WriteString(colorReset)
			b.WriteString(colorDim)
			b.WriteString(truncate(row.Text, codeWidth))
			b.WriteString(colorReset)
			b.WriteString(nl)
			rowsWritten++
		}
	}
	if collapsed {
		b.WriteString(colorDim)
		b.WriteString(indent)
		b.WriteString(moreLinesLabel(totalRows - limit))
		b.WriteString(colorReset)
		b.WriteString(nl)
	}
	return b.String()
}

func editPreviewRowCount(preview *editpreview.Preview) int {
	if preview == nil {
		return 0
	}
	total := 0
	for _, hunk := range preview.Hunks {
		total += len(hunk.Rows)
	}
	return total
}

func editPreviewChangeCounts(preview *editpreview.Preview) toolChangeCounts {
	var counts toolChangeCounts
	if preview == nil {
		return counts
	}
	for _, hunk := range preview.Hunks {
		for _, row := range hunk.Rows {
			switch row.Kind {
			case editpreview.KindAdd:
				counts.added++
			case editpreview.KindRemove:
				counts.removed++
			}
		}
	}
	return counts
}

func renderOutputPreview(display, indent string, inner int, color string, expanded bool) string {
	if display == "" {
		return ""
	}
	allLines := strings.Split(display, "\n")
	lines := allLines
	collapsed := !expanded && len(allLines) > collapsedPreviewLines
	if collapsed {
		lines = lines[:collapsedPreviewLines]
	}

	var b strings.Builder
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
	if collapsed {
		b.WriteString(nl)
		b.WriteString(colorDim)
		b.WriteString(indent)
		b.WriteString(moreLinesLabel(len(allLines) - len(lines)))
		b.WriteString(colorReset)
	}
	b.WriteString(nl)

	return b.String()
}

func moreLinesLabel(n int) string {
	if n == 1 {
		return "(1 more line)"
	}
	return fmt.Sprintf("(%d more lines)", n)
}

func extractWriteContent(args string) string {
	var params map[string]any
	if json.Unmarshal([]byte(args), &params) != nil {
		return ""
	}
	content, _ := params["content"].(string)
	return content
}

func countContentLines(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
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
	content = strings.TrimSpace(content)
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
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			b.WriteString("  ✕ ")
		} else {
			b.WriteString("    ")
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString(nl)
		}
	}
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

func permissionActions(req *agent.PermissionRequest) []menuItem {
	items := []menuItem{}
	if req != nil && req.CanAllowAll {
		items = append(items, menuItem{label: "Allow all", detail: "remaining staged calls", selectable: true, extra: "allow_all"})
	}
	items = append(items,
		menuItem{label: "Allow", detail: "once", selectable: true, extra: "allow"},
		menuItem{label: "Deny", detail: "this request", selectable: true, extra: "deny"},
		menuItem{label: "Allow for project", detail: "save rule", selectable: true, extra: "project"},
	)
	return items
}

func permissionActionCount(req *agent.PermissionRequest) int {
	return len(permissionActions(req))
}

func permissionMenuItems(req *agent.PermissionRequest) []menuItem {
	label := req.Arg
	if req.CanAllowAll && req.BatchTotal > 0 {
		label = fmt.Sprintf("%s  (%d/%d)", req.Arg, req.BatchIndex, req.BatchTotal)
	}
	items := []menuItem{
		{label: req.ToolName, selectable: false},
		{label: label, selectable: false},
	}
	if req.ResolvedArg != "" && req.ResolvedArg != req.Arg {
		items = append(items, menuItem{label: "resolves to " + req.ResolvedArg, selectable: false})
	}
	if req.CanAllowAll && len(req.BatchFiles) > 0 {
		items = append(items, menuItem{label: "Staged files:", selectable: false})
		current := req.Arg
		for _, file := range req.BatchFiles {
			prefix := "  "
			if file == current {
				prefix = "> "
			}
			items = append(items, menuItem{label: prefix + file, selectable: false})
		}
	}
	return append(items, permissionActions(req)...)
}

func renderPermissionPrompt(req *agent.PermissionRequest, selected int, width int) string {
	items := permissionMenuItems(req)
	return renderMenu("Permission Required", items, selected+permissionPromptHeaderRows(req), width, defaultMenuFooter)
}

func permissionPromptRows(req ...*agent.PermissionRequest) int {
	var r *agent.PermissionRequest
	if len(req) > 0 {
		r = req[0]
	}
	return menuRowsForItemCount(permissionPromptHeaderRows(r) + permissionActionCount(r))
}

func permissionPromptHeaderRows(req *agent.PermissionRequest) int {
	if req == nil {
		return 2
	}
	rows := 2
	if req.ResolvedArg != "" && req.ResolvedArg != req.Arg {
		rows++
	}
	if req.CanAllowAll && len(req.BatchFiles) > 0 {
		rows += 1 + len(req.BatchFiles)
	}
	return rows
}

func renderPermissionSuggestions(req *agent.PermissionRequest, suggestions []agent.PermissionSuggestion, selected int, width int) string {
	items := []menuItem{
		{label: req.ToolName, selectable: false},
		{label: req.Arg, selectable: false},
	}
	offset := 2
	if req.ResolvedArg != "" && req.ResolvedArg != req.Arg {
		items = append(items, menuItem{label: "resolves to " + req.ResolvedArg, selectable: false})
		offset++
	}
	for _, s := range suggestions {
		items = append(items, menuItem{label: s.Label, selectable: true})
	}
	return renderMenu("Allow for project", items, selected+offset, width, defaultMenuFooter)
}

func permissionSuggestionRows(suggestionCount int) int {
	return menuRowsForItemCount(2 + suggestionCount)
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
		return formatHeaderArg(extractJSONString(args, "path"), 80)
	case "run_command":
		command := formatMultilineHeaderArg(extractJSONString(args, "command"))
		if command == "" {
			return "$"
		}
		return "$ " + command
	case "execute_pending":
		act := extractJSONString(args, "action")
		if act == "discard" {
			return "discard"
		}
		return "apply"
	case "process":
		action := formatHeaderArg(extractJSONString(args, "action"), 80)
		id := formatHeaderArg(extractJSONString(args, "id"), 80)
		if action != "" && id != "" {
			return action + " " + id
		}
		if action == "list" {
			return "list"
		}
		return formatHeaderArg(args, 80)
	case "sleep":
		seconds := extractJSONNumber(args, "seconds")
		if seconds != "" {
			return seconds + "s"
		}
		return formatHeaderArg(args, 80)
	case "task":
		prompt := extractJSONString(args, "prompt")
		if prompt != "" {
			return formatHeaderArg(prompt, 80)
		}
		return formatHeaderArg(args, 80)
	case "save_memory":
		t := extractJSONString(args, "title")
		if t != "" {
			return formatHeaderArg(t, 80)
		}
		return formatHeaderArg(args, 80)
	case "search_memory", "search_history":
		q := extractJSONString(args, "query")
		if q != "" {
			return formatHeaderArg(q, 80)
		}
		return formatHeaderArg(args, 80)
	case "diagnostics":
		return ""
	case "workspace_symbol":
		q := extractJSONString(args, "query")
		if q != "" {
			return formatHeaderArg(q, 80)
		}
		return formatHeaderArg(args, 80)
	default:
		return formatHeaderArg(args, 80)
	}
}

func formatHeaderArg(s string, maxW int) string {
	return truncate(strings.Join(strings.Fields(s), " "), maxW)
}

func formatMultilineHeaderArg(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	trimmed := strings.TrimRight(s, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= 1 {
		return s
	}
	return lines[0] + "\n" + moreLinesLabel(len(lines)-1)
}

func extractJSONString(jsonStr, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(jsonStr), &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func extractJSONNumber(jsonStr, key string) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(jsonStr), &raw) != nil {
		return ""
	}
	v, ok := raw[key]
	if !ok {
		return ""
	}
	v = bytes.TrimSpace(v)
	if len(v) == 0 || v[0] == '"' {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var n json.Number
	if dec.Decode(&n) != nil {
		return ""
	}
	return n.String()
}

type displayEntry struct {
	typ      string
	id       string
	content  string
	turn     int
	name     string
	args     string
	done     bool
	success  bool
	result   string
	metadata map[string]any
	bg       *agent.BackgroundProcessDisplay
}

func buildDisplayMsgs(msgs []agent.DisplayMessage) []displayEntry {
	out := make([]displayEntry, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, displayEntry{
			typ:      m.Type,
			id:       m.ID,
			content:  m.Content,
			turn:     m.Turn,
			name:     m.Name,
			args:     m.Args,
			done:     m.Done,
			success:  m.Success,
			result:   m.Result,
			metadata: m.Metadata,
			bg:       m.BackgroundProcess,
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
		var result []rune
		currentW := 0
		for _, r := range s {
			rw := runeWidth(r)
			if currentW+rw > maxW {
				break
			}
			result = append(result, r)
			currentW += rw
		}
		return string(result)
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
