package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/MMinasyan/lightcode/internal/version"
)

// outW and errW are the dispatcher's streams: a command's answer goes to
// outW, everything else (errors, usage, hints) to errW. Tests swap them.
var (
	outW io.Writer = os.Stdout
	errW io.Writer = os.Stderr
)

// errUsage marks a post-parse usage violation; the dispatcher prints the
// message plus the command's usage line and exits 2.
var errUsage = errors.New("usage")

type usageError struct{ msg string }

func (e *usageError) Error() string        { return e.msg }
func (e *usageError) Is(target error) bool { return target == errUsage }

func usageErrorf(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// errLaunchGUI signals the dispatcher that the command resolves to the
// desktop app; main owns the actual detach/launch.
var errLaunchGUI = errors.New("launch gui")

// command is one registry entry. The registry is the single source for
// dispatch, help, and completion.
type command struct {
	name    string
	summary string               // one line, used by help + completion
	args    string               // positional hint for usage lines, e.g. "[command]"
	flags   func() *flag.FlagSet // completion enumerates via VisitAll
	run     func(fs *flag.FlagSet, args []string) error
}

func newFlagSet(name string) *flag.FlagSet {
	// ContinueOnError with discarded output and a no-op Usage: the flag
	// package prints help and errors to a single stream, which cannot
	// satisfy the help-to-stdout / errors-to-stderr split, so the
	// dispatcher does all printing.
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func checkNoArgs(fs *flag.FlagSet) error {
	if fs.NArg() > 0 {
		return usageErrorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// servePort and versionJSON are bound by their FlagSets; the *Var calls
// reset them to defaults on every flags() call, so state never leaks
// between dispatches.
var (
	servePort   int
	versionJSON bool
)

// commands is filled in init: the help entry's closure walks the registry
// itself, which would otherwise be an initialization cycle.
var commands []command

func init() {
	commands = []command{
		{
			name:    "desktop",
			summary: "launch the desktop app",
			flags:   func() *flag.FlagSet { return newFlagSet("desktop") },
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return errLaunchGUI
			},
		},
		{
			name:    "cli",
			summary: "run the interactive terminal session",
			flags:   func() *flag.FlagSet { return newFlagSet("cli") },
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runCLI()
			},
		},
		{
			name:    "serve",
			summary: "run the local HTTP daemon",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("serve")
				fs.IntVar(&servePort, "port", 0, "listen port (0 = OS-assigned)")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runServe(servePort)
			},
		},
		{
			name:    "acp",
			summary: "run as an ACP agent over stdio",
			flags:   func() *flag.FlagSet { return newFlagSet("acp") },
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runACP()
			},
		},
		{
			name:    "version",
			summary: "print the build version",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("version")
				fs.BoolVar(&versionJSON, "json", false, "machine-readable output")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				if versionJSON {
					b, err := json.Marshal(version.Current())
					if err != nil {
						return err
					}
					fmt.Fprintln(outW, string(b))
					return nil
				}
				fmt.Fprintln(outW, version.Line())
				return nil
			},
		},
		{
			name:    "help",
			summary: "show help for a command",
			args:    "[command]",
			flags:   func() *flag.FlagSet { return newFlagSet("help") },
			run: func(fs *flag.FlagSet, args []string) error {
				switch fs.NArg() {
				case 0:
					renderTopHelp(outW)
					return nil
				case 1:
					cmd := findCommand(fs.Arg(0))
					if cmd == nil {
						return usageErrorf("unknown command %q", fs.Arg(0))
					}
					renderCommandHelp(outW, cmd)
					return nil
				default:
					return usageErrorf("unexpected argument %q", fs.Arg(1))
				}
			},
		},
	}
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// dispatch routes argv. launchGUI reports that the invocation resolves to
// the desktop app (bare invocation or the desktop command); otherwise code
// is the process exit code. It never spawns or execs anything itself, so
// it is testable without side effects.
func dispatch(argv []string) (launchGUI bool, code int) {
	if len(argv) < 2 {
		return true, 0
	}
	name, rest := argv[1], argv[2:]
	switch name {
	case "-h", "--help":
		name = "help"
	case "-v", "--version":
		name = "version"
	}
	cmd := findCommand(name)
	if cmd == nil {
		fmt.Fprintf(errW, "lightcode: unknown command %q\n", argv[1])
		fmt.Fprintln(errW, "Run 'lightcode help' for usage.")
		return false, 2
	}
	fs := cmd.flags()
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			renderCommandHelp(outW, cmd)
			return false, 0
		}
		fmt.Fprintf(errW, "lightcode: %v\n", err)
		renderCommandUsage(errW, cmd)
		return false, 2
	}
	err := cmd.run(fs, fs.Args())
	switch {
	case err == nil:
		return false, 0
	case errors.Is(err, errLaunchGUI):
		return true, 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(errW, "lightcode: %v\n", err)
		renderCommandUsage(errW, cmd)
		return false, 2
	default:
		fmt.Fprintf(errW, "lightcode: %v\n", err)
		return false, 1
	}
}

func usageLine(cmd *command) string {
	s := "Usage: lightcode " + cmd.name
	if cmd.args != "" {
		s += " " + cmd.args
	}
	if hasFlags(cmd) {
		s += " [flags]"
	}
	return s
}

func hasFlags(cmd *command) bool {
	n := 0
	cmd.flags().VisitAll(func(*flag.Flag) { n++ })
	return n > 0
}

func renderCommandUsage(w io.Writer, cmd *command) {
	fmt.Fprintln(w, usageLine(cmd))
}

func renderCommandHelp(w io.Writer, cmd *command) {
	fmt.Fprintln(w, usageLine(cmd))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+cmd.summary)
	if hasFlags(cmd) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		fs := cmd.flags()
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
}

func renderTopHelp(w io.Writer) {
	fmt.Fprintln(w, "Lightcode - coding agent")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: lightcode [command]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for i := range commands {
		cmd := &commands[i]
		summary := cmd.summary
		if cmd.name == "desktop" {
			summary += " (default)"
		}
		fmt.Fprintf(w, "  %-10s %s\n", cmd.name, summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -h, --help      show help")
	fmt.Fprintln(w, "  -v, --version   print the build version")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Use 'lightcode help <command>' or 'lightcode <command> -h' for command help.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Exit codes: 0 success, 1 failure, 2 usage error.")
}
