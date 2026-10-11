package cli

import (
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/terminal"
)

const nl = "\r\n"

// collapsedPreviewLines bounds the preview of a queued message.
const collapsedPreviewLines = 5

const (
	colorReset = "\033[0m"
	colorDim   = "\033[2m"
	colorCyan  = "\033[36m"
	colorRed   = "\033[31m"
	colorGray  = "\033[90m"
)

// renderQueuedMsg renders a queued user message in dim/gray.
func renderQueuedMsg(content string, width int) string {
	if width < 20 {
		width = 20
	}
	lines := terminal.WrapLine(content, width-4)
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
			b.WriteString(terminal.FormatHeaderArg(bg.ID, 80))
		}
		reason := bg.Reason
		if reason == "" {
			reason = "completed"
		}
		b.WriteString("  ")
		b.WriteString(reason)
		b.WriteString(fmt.Sprintf(" exit %d", bg.ExitCode))
		command := terminal.FormatMultilineHeaderArg(bg.Command)
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

func permissionActions(req *agent.PermissionRequest) []terminal.MenuItem {
	items := []terminal.MenuItem{}
	items = append(items,
		terminal.MenuItem{Label: "Allow", Detail: "once", Selectable: true, Extra: "allow"},
		terminal.MenuItem{Label: "Deny", Detail: "this request", Selectable: true, Extra: "deny"},
	)
	if req == nil || !req.DisableProjectSave {
		items = append(items, terminal.MenuItem{Label: "Allow for project", Detail: "save rule", Selectable: true, Extra: "project"})
	}
	return items
}

func permissionActionCount(req *agent.PermissionRequest) int {
	return len(permissionActions(req))
}

func permissionMenuItems(req *agent.PermissionRequest) []terminal.MenuItem {
	label := req.Arg
	items := []terminal.MenuItem{
		{Label: req.ToolName, Selectable: false},
		{Label: label, Selectable: false},
	}
	if req.ResolvedArg != "" && req.ResolvedArg != req.Arg {
		items = append(items, terminal.MenuItem{Label: "resolves to " + req.ResolvedArg, Selectable: false})
	}
	if len(req.BatchFiles) > 0 {
		title := "Affected files:"
		items = append(items, terminal.MenuItem{Label: title, Selectable: false})
		current := req.Arg
		for i, file := range req.BatchFiles {
			prefix := "  "
			if file == current {
				prefix = "> "
			}
			if i < len(req.BatchResolvedFiles) && req.BatchResolvedFiles[i] != "" && req.BatchResolvedFiles[i] != file {
				file += " -> " + req.BatchResolvedFiles[i]
			}
			items = append(items, terminal.MenuItem{Label: prefix + file, Selectable: false})
		}
	}
	return append(items, permissionActions(req)...)
}

func renderPermissionPrompt(req *agent.PermissionRequest, selected int, width int) string {
	items := permissionMenuItems(req)
	return terminal.RenderMenu("Permission Required", items, selected+permissionPromptHeaderRows(req), width, "")
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
	if len(req.BatchFiles) > 0 {
		rows += 1 + len(req.BatchFiles)
	}
	return rows
}

func renderPermissionSuggestions(req *agent.PermissionRequest, suggestions []agent.PermissionSuggestion, selected int, width int) string {
	items := []terminal.MenuItem{
		{Label: req.ToolName, Selectable: false},
		{Label: req.Arg, Selectable: false},
	}
	offset := 2
	if req.ResolvedArg != "" && req.ResolvedArg != req.Arg {
		items = append(items, terminal.MenuItem{Label: "resolves to " + req.ResolvedArg, Selectable: false})
		offset++
	}
	for _, s := range suggestions {
		items = append(items, terminal.MenuItem{Label: s.Label, Selectable: true})
	}
	return terminal.RenderMenu("Allow for project", items, selected+offset, width, "")
}

func permissionSuggestionRows(suggestionCount int) int {
	return menuRowsForItemCount(2 + suggestionCount)
}

// menuRowsForItemCount is the rendered row count of a menu with itemCount
// items plus its title and footer rows.
func menuRowsForItemCount(itemCount int) int {
	return itemCount + 2
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
