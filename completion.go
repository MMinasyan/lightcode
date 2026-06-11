package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// The completion generators iterate the registry, so dispatch, help, and
// completion stay one source of truth: every command name, summary, and
// flag below comes from the commands slice. The scripts embed only registry
// strings (compile-time ASCII constants); they are escaped anyway.

// registeredFlagNames enumerates a command's flag names via VisitAll. Help
// is not among them: the flag package implements it via ErrHelp, not a
// registered flag, so the generators inject -h/--help themselves.
func registeredFlagNames(cmd *command) []string {
	var names []string
	cmd.flags().VisitAll(func(f *flag.Flag) {
		names = append(names, f.Name)
	})
	return names
}

// flagWords renders flag names in the single- and double-dash forms the
// flag package accepts, plus the injected -h/--help.
func flagWords(cmd *command) []string {
	var words []string
	for _, name := range registeredFlagNames(cmd) {
		words = append(words, "-"+name, "--"+name)
	}
	return append(words, "-h", "--help")
}

func commandNames() []string {
	names := make([]string, 0, len(commands))
	for i := range commands {
		names = append(names, commands[i].name)
	}
	return names
}

// completionWordsFor returns what completes after a command word: its
// flags, plus the static positional arm for completion's own closed shell
// enum — the only positional that gets value completion. Everything else
// (models providers, upgrade tags, help's command argument) is deliberately
// not completed.
func completionWordsFor(cmd *command) []string {
	var words []string
	if cmd.name == "completion" {
		words = append(words, "bash", "zsh", "fish")
	}
	return append(words, flagWords(cmd)...)
}

func escapeSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

func writeBashCompletion(w io.Writer) {
	top := append(commandNames(), "-h", "--help", "-v", "--version")
	fmt.Fprintln(w, "# bash completion for lightcode")
	fmt.Fprintln(w, `# install: eval "$(lightcode completion bash)"`)
	fmt.Fprintln(w, "_lightcode_completions() {")
	fmt.Fprintln(w, `    local cur="${COMP_WORDS[COMP_CWORD]}"`)
	fmt.Fprintln(w, `    if [ "$COMP_CWORD" -eq 1 ]; then`)
	fmt.Fprintf(w, "        COMPREPLY=($(compgen -W '%s' -- \"$cur\"))\n", escapeSingleQuoted(strings.Join(top, " ")))
	fmt.Fprintln(w, "        return")
	fmt.Fprintln(w, "    fi")
	fmt.Fprintln(w, `    case "${COMP_WORDS[1]}" in`)
	for i := range commands {
		cmd := &commands[i]
		fmt.Fprintf(w, "        %s)\n", cmd.name)
		fmt.Fprintf(w, "            COMPREPLY=($(compgen -W '%s' -- \"$cur\"))\n", escapeSingleQuoted(strings.Join(completionWordsFor(cmd), " ")))
		fmt.Fprintln(w, "            ;;")
	}
	fmt.Fprintln(w, "    esac")
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w, "complete -F _lightcode_completions lightcode")
}

func writeZshCompletion(w io.Writer) {
	fmt.Fprintln(w, "#compdef lightcode")
	fmt.Fprintln(w, `# install: eval "$(lightcode completion zsh)"`)
	fmt.Fprintln(w, "_lightcode() {")
	fmt.Fprintln(w, "    local -a _lightcode_commands")
	fmt.Fprintln(w, "    _lightcode_commands=(")
	for i := range commands {
		cmd := &commands[i]
		fmt.Fprintf(w, "        '%s:%s'\n", cmd.name, escapeSingleQuoted(cmd.summary))
	}
	fmt.Fprintln(w, "        '-h:show help'")
	fmt.Fprintln(w, "        '--help:show help'")
	fmt.Fprintln(w, "        '-v:print the build version'")
	fmt.Fprintln(w, "        '--version:print the build version'")
	fmt.Fprintln(w, "    )")
	fmt.Fprintln(w, "    if (( CURRENT == 2 )); then")
	fmt.Fprintln(w, "        _describe 'command' _lightcode_commands")
	fmt.Fprintln(w, "        return")
	fmt.Fprintln(w, "    fi")
	fmt.Fprintln(w, `    case "${words[2]}" in`)
	for i := range commands {
		cmd := &commands[i]
		fmt.Fprintf(w, "        %s)\n", cmd.name)
		fmt.Fprintf(w, "            compadd -- %s\n", escapeSingleQuoted(strings.Join(completionWordsFor(cmd), " ")))
		fmt.Fprintln(w, "            ;;")
	}
	fmt.Fprintln(w, "    esac")
	fmt.Fprintln(w, "}")
	// compdef registers under eval too; the #compdef header alone is only
	// honored when the file is loaded from fpath.
	fmt.Fprintln(w, "compdef _lightcode lightcode")
}

func writeFishCompletion(w io.Writer) {
	fmt.Fprintln(w, "# fish completion for lightcode")
	fmt.Fprintln(w, "# install: lightcode completion fish | source")
	fmt.Fprintln(w, "complete -c lightcode -f")
	for i := range commands {
		cmd := &commands[i]
		fmt.Fprintf(w, "complete -c lightcode -n __fish_use_subcommand -a %s -d '%s'\n", cmd.name, escapeSingleQuoted(cmd.summary))
	}
	fmt.Fprintln(w, "complete -c lightcode -n __fish_use_subcommand -s h -l help -d 'show help'")
	fmt.Fprintln(w, "complete -c lightcode -n __fish_use_subcommand -s v -l version -d 'print the build version'")
	for i := range commands {
		cmd := &commands[i]
		cond := fmt.Sprintf("__fish_seen_subcommand_from %s", cmd.name)
		if cmd.name == "completion" {
			fmt.Fprintf(w, "complete -c lightcode -n '%s' -a 'bash zsh fish'\n", cond)
		}
		for _, name := range registeredFlagNames(cmd) {
			if len(name) == 1 {
				fmt.Fprintf(w, "complete -c lightcode -n '%s' -s %s\n", cond, name)
			} else {
				fmt.Fprintf(w, "complete -c lightcode -n '%s' -o %s -l %s\n", cond, name, name)
			}
		}
		fmt.Fprintf(w, "complete -c lightcode -n '%s' -s h -l help -d 'show help'\n", cond)
	}
}
