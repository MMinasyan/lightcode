package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

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

func EraseBlock(n int) string {
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

// RenderUserMsg renders a user message with horizontal borders.
func RenderUserMsg(content string, width int) string {
	if width < 20 {
		width = 20
	}
	lines := WrapLine(content, width-4)
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

// RenderAssistantMsg renders assistant text with basic markdown→ANSI.
func RenderAssistantMsg(content string, width int) string {
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
		wrapped := WrapLine(rendered, inner)
		for _, wl := range wrapped {
			b.WriteString(indent)
			b.WriteString(wl)
			b.WriteString(nl)
		}
	}
	return b.String()
}

// RenderToolCall renders a tool call header line.
func RenderToolCall(name, args string, metadata map[string]any) string {
	argDisplay := FormatToolArgs(name, args)
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
	case "apply_patch":
		files, ok := editpreview.FilesFromMetadata(metadata)
		if !ok {
			return toolChangeCounts{}, false
		}
		counts := applyPatchFilesChangeCounts(files)
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

func applyPatchFilesChangeCounts(files []editpreview.FileEntry) toolChangeCounts {
	var counts toolChangeCounts
	for _, f := range files {
		for _, hunk := range f.Preview.Hunks {
			for _, row := range hunk.Rows {
				switch row.Kind {
				case editpreview.KindAdd:
					counts.added++
				case editpreview.KindRemove:
					counts.removed++
				}
			}
		}
	}
	return counts
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

// RenderToolResult renders a tool call result.
func RenderToolResult(name, args, result string, success bool, expanded bool, width int, metadata map[string]any) string {
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
			b.WriteString(RenderEditPreview(preview, indent, inner, expanded))
			return b.String()
		}
		return ""
	}

	// apply_patch success output is per-file; render one diff block
	// per file under the outer per-file shape.
	if name == "apply_patch" {
		if files, ok := editpreview.FilesFromMetadata(metadata); ok {
			b.WriteString(renderApplyPatchFiles(files, indent, inner, expanded))
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

func RenderEditPreview(preview *editpreview.Preview, indent string, inner int, expanded bool) string {
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
			b.WriteString(Truncate(row.Text, codeWidth))
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

// renderApplyPatchFiles renders one diff block per entry in the
// edit_preview_files list (apply_patch). Each block has the op tag
// (A/M/D) and the path on its first line, followed by the per-file
// preview's hunks. Per-file expand / collapse is per-block; the outer
// shape mirrors the GUI's outer per-file grouping.
func renderApplyPatchFiles(files []editpreview.FileEntry, indent string, inner int, expanded bool) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	for i, file := range files {
		if i > 0 {
			b.WriteString(nl)
		}
		op := file.Op
		if op == "" {
			op = "M"
		}
		b.WriteString(indent)
		b.WriteString(colorCyan)
		b.WriteString(op)
		b.WriteString(colorReset)
		b.WriteString(" ")
		b.WriteString(file.Path)
		b.WriteString(nl)
		if len(file.Preview.Hunks) == 0 {
			continue
		}
		b.WriteString(RenderEditPreview(&file.Preview, indent, inner, expanded))
	}
	return b.String()
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
		truncated := Truncate(line, inner)
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

// RenderSystemMsg renders a system message (dim italic).
func RenderSystemMsg(content string) string {
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

// RenderErrorMsg renders an error message.
func RenderErrorMsg(content string) string {
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

// RenderWarningMsg renders a warning message.
func RenderWarningMsg(content string) string {
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

func FormatToolArgs(name, args string) string {
	switch name {
	case "read_file", "write_file", "edit_file":
		return FormatHeaderArg(extractJSONString(args, "path"), 80)
	case "run_command":
		command := FormatMultilineHeaderArg(extractJSONString(args, "command"))
		if command == "" {
			return "$"
		}
		return "$ " + command
	case "process":
		action := FormatHeaderArg(extractJSONString(args, "action"), 80)
		id := FormatHeaderArg(extractJSONString(args, "id"), 80)
		if action != "" && id != "" {
			return action + " " + id
		}
		if action == "list" {
			return "list"
		}
		return FormatHeaderArg(args, 80)
	case "sleep":
		seconds := extractJSONNumber(args, "seconds")
		if seconds != "" {
			return seconds + "s"
		}
		return FormatHeaderArg(args, 80)
	case "task":
		prompt := extractJSONString(args, "prompt")
		if prompt != "" {
			return FormatHeaderArg(prompt, 80)
		}
		return FormatHeaderArg(args, 80)
	case "diagnostics":
		return ""
	case "workspace_symbol":
		q := extractJSONString(args, "query")
		if q != "" {
			return FormatHeaderArg(q, 80)
		}
		return FormatHeaderArg(args, 80)
	default:
		return FormatHeaderArg(args, 80)
	}
}

func FormatHeaderArg(s string, maxW int) string {
	return Truncate(strings.Join(strings.Fields(s), " "), maxW)
}

func FormatMultilineHeaderArg(s string) string {
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

func Truncate(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	w := VisibleWidth(s)
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

func WrapLine(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}

	var lines []string
	current := ""
	currentW := 0

	words := splitKeepAnsi(text)
	for _, word := range words {
		wordW := VisibleWidth(word)
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

func VisibleWidth(s string) int {
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
