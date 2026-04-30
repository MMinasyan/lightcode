package cli

import (
	"fmt"
	"strconv"
	"strings"
)

func handleCommand(c *CLI, input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/help":
		cmdHelp(c)
	case "/model":
		cmdModel(c, args)
	case "/session":
		cmdSession(c, args)
	case "/project":
		cmdProject(c, args)
	case "/revert":
		cmdRevert(c, args)
	case "/fork":
		cmdFork(c, args)
	case "/tokens":
		cmdTokens(c)
	case "/compact":
		cmdCompact(c)
	case "/snapshot":
		cmdSnapshot(c)
	case "/view":
		cmdView(c, args)
	case "/clear":
		cmdClear(c)
	case "/exit":
		cmdExit(c)
	default:
		c.writeln(colorRed(fmt.Sprintf("Unknown command: %s. Type /help for list.", cmd)))
	}
}

func checkBusy(c *CLI, opName string) bool {
	if c.svc.Busy() {
		c.writeln(colorYellow(fmt.Sprintf(
			"Cannot %s while a turn is running. Wait or press Ctrl+C to cancel.",
			opName,
		)))
		return true
	}
	return false
}

func cmdHelp(c *CLI) {
	c.writeln(`Commands:
  /help                         Show this help
  /model                        List models
  /model <provider> <model>     Switch model
  /session                      List active sessions
  /session archived             List archived sessions
  /session new                  Start fresh session
  /session switch <id>          Switch session
  /session archive <id>         Archive session
  /session delete <id>          Delete session
  /project                      List projects
  /project switch <path>        Switch project (re-launches CLI)
  /revert code <turn>           Restore files to turn N
  /revert history <turn>        Truncate history to turns 1..N-1
  /fork <turn>                  Fork session from turn N
  /tokens                       Show token usage
  /compact                      Trigger manual compaction
  /snapshot                     Show snapshot timeline
  /view <path>                  View file contents
  /clear                        Clear terminal
  /exit                         Exit CLI

Keyboard:
  Ctrl+C  (idle)       Exit
  Ctrl+C  (streaming)  Cancel current turn
  Ctrl+C  (permission) Deny permission
  Ctrl+D               Exit`)
}

func cmdModel(c *CLI, args []string) {
	if len(args) == 0 {
		models := c.svc.ModelList()
		current := c.svc.CurrentModel()
		c.writef("  Current: %s/%s\n", current.Provider, current.Model)
		c.writeln("  Available:")
		n := 0
		for _, pm := range models {
			for _, m := range pm.Models {
				n++
				marker := ""
				if pm.Provider == current.Provider && m == current.Model {
					marker = " (current)"
				}
				c.writef("    %d. %s/%s%s\n", n, pm.Provider, m, marker)
			}
		}
		c.writeln("  Use: /model <provider> <model>")
		return
	}
	if len(args) < 2 {
		c.writeln("Usage: /model <provider> <model>")
		return
	}
	if checkBusy(c, "switch model") {
		return
	}
	provider, model := args[0], args[1]
	if err := c.svc.SwitchModel(provider, model); err != nil {
		c.writeError(err.Error())
		return
	}
	c.writef("Switched to %s/%s\n", provider, model)
}

func cmdSession(c *CLI, args []string) {
	if len(args) == 0 {
		cmdSessionList(c, "active")
		return
	}
	switch args[0] {
	case "archived":
		cmdSessionList(c, "archived")
	case "new":
		if checkBusy(c, "start new session") {
			return
		}
		if err := c.svc.SessionNew(); err != nil {
			c.writeError(err.Error())
			return
		}
		c.emitSessionChanged()
		c.writeln("New session started.")
	case "switch":
		if len(args) < 2 {
			c.writeln("Usage: /session switch <id>")
			cmdSessionList(c, "active")
			return
		}
		id := args[1]
		if isArchivedSession(c, id) {
			c.writeln("Reactivating archived session...")
		}
		if err := c.svc.SessionSwitch(id); err != nil {
			c.writeError(err.Error())
			return
		}
		c.emitSessionChanged()
		sess := c.svc.SessionCurrent()
		model := c.svc.CurrentModel()
		c.writef("Switched to session %s (model: %s/%s)\n", shortID(sess.ID), model.Provider, model.Model)
	case "archive":
		if len(args) < 2 {
			c.writeln("Usage: /session archive <id>")
			cmdSessionList(c, "active")
			return
		}
		closedCurrent, err := c.svc.SessionArchive(args[1])
		if err != nil {
			c.writeError(err.Error())
			return
		}
		if closedCurrent {
			c.emitSessionChanged()
		}
		c.writeln("Session archived.")
	case "delete":
		if len(args) < 2 {
			c.writeln("Usage: /session delete <id>")
			cmdSessionList(c, "active")
			return
		}
		closedCurrent, err := c.svc.SessionDelete(args[1])
		if err != nil {
			c.writeError(err.Error())
			return
		}
		if closedCurrent {
			c.emitSessionChanged()
		}
		c.writeln("Session deleted.")
	default:
		c.writef("Unknown session command: %s\n", args[0])
		c.writeln("Commands: new, switch, archive, delete, archived")
	}
}

func cmdSessionList(c *CLI, state string) {
	sessions, err := c.svc.SessionList(state)
	if err != nil {
		c.writeError(err.Error())
		return
	}
	current := c.svc.SessionCurrent()
	if state == "active" {
		c.writeln("  Active sessions:")
	} else {
		c.writeln("  Archived sessions:")
	}
	if len(sessions) == 0 {
		c.writeln("    (none)")
	}
	for _, s := range sessions {
		c.writeln(renderSessionLine(s, s.ID == current.ID))
	}
	if state == "active" {
		c.writeln("  Commands: /session new, /session switch <id>, /session archive <id>, /session delete <id>")
	}
}

func isArchivedSession(c *CLI, id string) bool {
	sessions, err := c.svc.SessionList("archived")
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.ID == id || strings.HasPrefix(s.ID, id) {
			return true
		}
	}
	return false
}

func cmdProject(c *CLI, args []string) {
	if len(args) == 0 {
		projects, err := c.svc.ProjectList()
		if err != nil {
			c.writeError(err.Error())
			return
		}
		current := c.svc.ProjectCurrent()
		c.writeln("  Projects:")
		for _, p := range projects {
			cur := ""
			if p.ID == current.ID {
				cur = "  (current)"
			}
			c.writef("    %s  %s%s\n", p.Name, p.Path, cur)
		}
		return
	}
	if args[0] == "switch" {
		if len(args) < 2 {
			c.writeln("Usage: /project switch <path>")
			return
		}
		path := strings.Join(args[1:], " ")
		if err := c.projectSwitch(path); err != nil {
			c.writeError(err.Error())
		}
		return
	}
	c.writef("Unknown project command: %s\n", args[0])
	c.writeln("Commands: switch")
}

func cmdRevert(c *CLI, args []string) {
	if len(args) < 2 {
		c.writeln("Usage: /revert code <turn> | /revert history <turn>")
		return
	}
	subCmd := args[0]
	turn, err := strconv.Atoi(args[1])
	if err != nil || turn < 1 {
		c.writef("Invalid turn number: %s\n", args[1])
		return
	}
	if checkBusy(c, "revert") {
		return
	}

	switch subCmd {
	case "code":
		if err := c.svc.RevertCode(turn); err != nil {
			c.writeError(err.Error())
			return
		}
		c.writef("Files reverted to turn %d.\n", turn)

	case "history":
		if err := c.svc.RevertHistory(turn); err != nil {
			c.writeError(err.Error())
			return
		}
		c.writef("History truncated to turns 1..%d.\n", turn-1)
		c.emitSessionChanged()

	default:
		c.writef("Unknown revert subcommand: %s. Use 'code' or 'history'.\n", subCmd)
	}
}

func cmdFork(c *CLI, args []string) {
	if len(args) < 1 {
		c.writeln("Usage: /fork <turn>")
		return
	}
	turn, err := strconv.Atoi(args[0])
	if err != nil || turn < 1 {
		c.writef("Invalid turn number: %s\n", args[0])
		return
	}
	if checkBusy(c, "fork session") {
		return
	}
	if err := c.svc.ForkSession(turn); err != nil {
		c.writeError(err.Error())
		return
	}
	c.emitSessionChanged()
	sess := c.svc.SessionCurrent()
	c.writef("Forked session from turn %d. New session: %s\n", turn, shortID(sess.ID))
}

func cmdTokens(c *CLI) {
	t := c.svc.TokenUsage()
	total := t.Total
	c.writef("  Cache: %s | Input: %s | Output: %s | Total: %s\n",
		formatInt(total.Cache),
		formatInt(total.Input),
		formatInt(total.Output),
		formatInt(total.Cache+total.Input+total.Output),
	)
	if t.ContextUsed > 0 && t.ContextWindow > 0 {
		pct := float64(t.ContextUsed) / float64(t.ContextWindow) * 100
		c.writef("  Context: %s / %s (%.1f%%)\n",
			formatInt(t.ContextUsed),
			formatInt(t.ContextWindow),
			pct,
		)
	}
}

func cmdCompact(c *CLI) {
	if checkBusy(c, "compact") {
		return
	}
	c.compactionManual = true
	if err := c.svc.CompactNow(c.ctx); err != nil {
		c.compactionManual = false
		c.writeError(err.Error())
	}
}

func cmdSnapshot(c *CLI) {
	snapshots, err := c.svc.SnapshotList()
	if err != nil {
		c.writeError(err.Error())
		return
	}
	if len(snapshots) == 0 {
		c.writeln("  No snapshots.")
		return
	}
	c.writeln("  Snapshots:")
	for _, s := range snapshots {
		paths := make([]string, len(s.Files))
		for i, f := range s.Files {
			paths[i] = f.Path
		}
		c.writef("    Turn %d: %s\n", s.Turn, strings.Join(paths, ", "))
	}
}

func cmdView(c *CLI, args []string) {
	if len(args) < 1 {
		c.writeln("Usage: /view <path>")
		return
	}
	path := strings.Join(args, " ")
	content, err := c.svc.ReadFileContent(path)
	if err != nil {
		c.writeError(fmt.Sprintf("read file: %v", err))
		return
	}
	lines := strings.Split(content, "\n")
	c.writef("  File: %s (%d lines)\n", path, len(lines))
	for i, line := range lines {
		c.writef("  %4d  %s\n", i+1, line)
	}
}

func cmdClear(c *CLI) {
	c.writeRaw("\033[2J\033[H")
}

func cmdExit(c *CLI) {
	c.writeln("Exiting.")
	c.cancel()
}
