package permission

import (
	"os"
	"path/filepath"
	"strings"
)

// Suggestion is a pattern choice shown in the "Allow for project" UI.
type Suggestion struct {
	Rule  string `json:"rule"`  // full rule string, e.g. "run_command(npm run *)"
	Label string `json:"label"` // human-readable label for the UI
}

// Suggest returns pattern suggestions of escalating generality for a
// tool call. For run_command, compound commands are decomposed and
// suggestions are returned per unmatched subcommand.
func Suggest(toolName, arg, projectRoot string) []Suggestion {
	if toolName == "run_command" {
		return suggestCommand(arg)
	}
	return suggestFile(toolName, arg, projectRoot)
}

// SuggestForSubcommands returns suggestions grouped by subcommand.
// Each inner slice is the suggestions for one subcommand.
func SuggestForSubcommands(command string) [][]Suggestion {
	subs, err := DecomposeCommand(command)
	if err != nil || len(subs) == 0 {
		return [][]Suggestion{suggestCommand(command)}
	}
	if len(subs) == 1 {
		return [][]Suggestion{suggestCommand(subs[0])}
	}
	var out [][]Suggestion
	for _, sub := range subs {
		out = append(out, suggestCommand(sub))
	}
	return out
}

func suggestCommand(command string) []Suggestion {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return nil
	}

	var suggestions []Suggestion

	// Exact command.
	suggestions = append(suggestions, Suggestion{
		Rule:  "run_command(" + command + ")",
		Label: command,
	})

	// Progressive wildcards: replace last N tokens with *.
	for i := len(parts) - 1; i >= 1; i-- {
		pattern := strings.Join(parts[:i], " ") + " *"
		label := pattern
		if len(suggestions) > 0 && suggestions[len(suggestions)-1].Label == label {
			continue
		}
		suggestions = append(suggestions, Suggestion{
			Rule:  "run_command(" + pattern + ")",
			Label: label,
		})
	}

	return suggestions
}

func suggestFile(toolName, absPath, projectRoot string) []Suggestion {
	prefix, rel := filePrefix(absPath, projectRoot)

	var suggestions []Suggestion

	// Exact file.
	suggestions = append(suggestions, Suggestion{
		Rule:  toolName + "(" + prefix + rel + ")",
		Label: prefix + rel,
	})

	// Progressive directory wildcards.
	dir := filepath.Dir(rel)
	for dir != "." && dir != "/" && dir != "" {
		pattern := prefix + dir + "/*"
		suggestions = append(suggestions, Suggestion{
			Rule:  toolName + "(" + pattern + ")",
			Label: pattern,
		})

		patternRecursive := prefix + dir + "/**"
		suggestions = append(suggestions, Suggestion{
			Rule:  toolName + "(" + patternRecursive + ")",
			Label: patternRecursive,
		})

		dir = filepath.Dir(dir)
	}

	// Root-level wildcard.
	rootPattern := prefix + "**"
	if len(suggestions) == 0 || suggestions[len(suggestions)-1].Label != rootPattern {
		suggestions = append(suggestions, Suggestion{
			Rule:  toolName + "(" + rootPattern + ")",
			Label: rootPattern,
		})
	}

	return suggestions
}

// filePrefix returns the appropriate prefix (/, ~/, //) and the relative
// path for use in rule patterns.
func filePrefix(absPath, projectRoot string) (prefix, rel string) {
	if projectRoot != "" {
		if r, err := filepath.Rel(projectRoot, absPath); err == nil && !strings.HasPrefix(r, "..") {
			return "/", r
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if r, err := filepath.Rel(home, absPath); err == nil && !strings.HasPrefix(r, "..") {
			return "~/", r
		}
	}
	return "//", absPath
}
