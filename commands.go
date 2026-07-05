package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/doctor"
	"github.com/MMinasyan/lightcode/internal/selfupdate"
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

// errSilent marks a failure whose message the handler already printed in
// its own pinned format (the taxonomy's unprefixed exceptions, like the
// user-decline status "cancelled"); the dispatcher exits 1 without the
// "lightcode:" line.
var errSilent = errors.New("silent failure")

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

// The command flag targets are bound by their FlagSets; the *Var calls
// reset them to defaults on every flags() call, so state never leaks
// between dispatches.
var (
	servePort       int
	versionJSON     bool
	doctorJSON      bool
	upgradeCheck    bool
	upgradeJSON     bool
	uninstallPurge  bool
	uninstallYes    bool
	uninstallDryRun bool
	modelsAll       bool
	modelsJSON      bool
	configPathOnly  bool
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
			name:    "stop",
			summary: "stop the local owner process",
			flags:   func() *flag.FlagSet { return newFlagSet("stop") },
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runStop()
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
			name:    "doctor",
			summary: "diagnose the install, config, and providers",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("doctor")
				fs.BoolVar(&doctorJSON, "json", false, "machine-readable output")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runDoctor(doctorJSON)
			},
		},
		{
			name:    "upgrade",
			summary: "update the installed binary from the release channel",
			args:    "[version]",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("upgrade")
				fs.BoolVar(&upgradeCheck, "check", false, "compare against the latest release without installing")
				fs.BoolVar(&upgradeJSON, "json", false, "machine-readable --check output")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				return runUpgrade(fs.Args())
			},
		},
		{
			name:    "uninstall",
			summary: "remove the installed binary (and optionally all data)",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("uninstall")
				fs.BoolVar(&uninstallPurge, "purge", false, "also remove ~/.lightcode (API keys, sessions, snapshots)")
				fs.BoolVar(&uninstallYes, "yes", false, "skip the confirmation prompt")
				fs.BoolVar(&uninstallDryRun, "dry-run", false, "list what would be removed and exit")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runUninstall(uninstallPurge, uninstallYes, uninstallDryRun)
			},
		},
		{
			name:    "completion",
			summary: "print a shell completion script",
			args:    "[bash|zsh|fish]",
			flags:   func() *flag.FlagSet { return newFlagSet("completion") },
			run: func(fs *flag.FlagSet, args []string) error {
				shell := "bash"
				switch fs.NArg() {
				case 0:
				case 1:
					shell = fs.Arg(0)
				default:
					return usageErrorf("unexpected argument %q", fs.Arg(1))
				}
				switch shell {
				case "bash":
					writeBashCompletion(outW)
				case "zsh":
					writeZshCompletion(outW)
				case "fish":
					writeFishCompletion(outW)
				default:
					return usageErrorf("unsupported shell %q (supported: bash, zsh, fish)", shell)
				}
				return nil
			},
		},
		{
			name:    "models",
			summary: "list available models",
			args:    "[provider]",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("models")
				fs.BoolVar(&modelsAll, "all", false, "include hidden and incomplete models")
				fs.BoolVar(&modelsJSON, "json", false, "machine-readable output")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				// The flag package stops parsing at the first positional;
				// fold trailing flags back so `models <provider> --all`
				// works like `models --all <provider>`.
				var positionals []string
				for _, a := range fs.Args() {
					switch a {
					case "--all", "-all":
						modelsAll = true
					case "--json", "-json":
						modelsJSON = true
					default:
						positionals = append(positionals, a)
					}
				}
				provider := ""
				switch len(positionals) {
				case 0:
				case 1:
					provider = positionals[0]
				default:
					return usageErrorf("unexpected argument %q", positionals[1])
				}
				return runModels(provider, modelsAll, modelsJSON)
			},
		},
		{
			name:    "config",
			summary: "print the config file",
			flags: func() *flag.FlagSet {
				fs := newFlagSet("config")
				fs.BoolVar(&configPathOnly, "path", false, "print the resolved config path instead")
				return fs
			},
			run: func(fs *flag.FlagSet, args []string) error {
				if err := checkNoArgs(fs); err != nil {
					return err
				}
				return runConfig(configPathOnly)
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

// runUpgrade implements the upgrade flow. raw is everything after the
// parsed flags; the flag package stops parsing at the first positional, so
// check/json given after a version argument arrive here and are folded back
// into their flags.
func runUpgrade(raw []string) error {
	var positionals []string
	for _, a := range raw {
		switch a {
		case "--check", "-check":
			upgradeCheck = true
		case "--json", "-json":
			upgradeJSON = true
		default:
			positionals = append(positionals, a)
		}
	}
	if upgradeJSON && !upgradeCheck {
		return usageErrorf("--json requires --check")
	}

	if upgradeCheck {
		return runUpgradeCheck(positionals)
	}

	if len(positionals) > 1 {
		return usageErrorf("unexpected argument %q", positionals[1])
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("no prebuilt release for %s/%s; build from source (see README)", runtime.GOOS, runtime.GOARCH)
	}

	base := os.Getenv("LIGHTCODE_RELEASE_BASE")
	if base != "" && selfupdate.IgnoreReleaseBaseAsRoot() {
		fmt.Fprintln(errW, "note: LIGHTCODE_RELEASE_BASE ignored when running as root")
		base = ""
	}

	explicit := ""
	if len(positionals) == 1 {
		explicit = positionals[0]
	}
	if !version.IsRelease(version.Version) && explicit == "" {
		return fmt.Errorf("this is a development build (%s); pass an explicit version to upgrade anyway: lightcode upgrade %s", version.String(), selfupdate.MinInstallable)
	}

	var target string
	switch {
	case explicit != "":
		if !version.IsRelease(explicit) {
			return usageErrorf("invalid version %q (expected vMAJOR.MINOR.PATCH)", explicit)
		}
		target = explicit
	case base != "":
		return usageErrorf("LIGHTCODE_RELEASE_BASE is set; pass an explicit version")
	default:
		tag, err := selfupdate.LatestTag()
		if err != nil {
			return err
		}
		target = tag
	}

	// The floor gate applies to the target in all cases, the equal-version
	// early exit included.
	floorOK := version.Compare(target, selfupdate.MinInstallable) >= 0
	if version.IsRelease(version.Version) {
		switch version.Compare(version.Version, target) {
		case 0:
			if floorOK {
				fmt.Fprintf(errW, "already up to date (%s)\n", target)
				return nil
			}
		case 1:
			if explicit == "" {
				// Downgrade is an explicit-target operation; an implicit
				// latest below the current release is nothing to do.
				fmt.Fprintf(errW, "already up to date (%s)\n", version.Version)
				return nil
			}
			fmt.Fprintf(errW, "downgrading %s -> %s\n", version.Version, target)
		}
	} else {
		// Comparison is undefined for a non-semver current; the explicit
		// target (required above) installs unconditionally.
		fmt.Fprintf(errW, "installing %s over dev build\n", target)
	}
	if !floorOK {
		return fmt.Errorf("%s predates the upgrade command; install it with the installer instead: curl -fsSL https://github.com/MMinasyan/lightcode/releases/latest/download/install.sh | LIGHTCODE_VERSION=%s sh", target, target)
	}

	targetPath, targetDir, err := selfupdate.ResolveExecutable()
	if err != nil {
		return err
	}
	if !selfupdate.DirWritable(targetDir) {
		return fmt.Errorf("binary is at %s (not writable); re-run as: sudo lightcode upgrade", targetPath)
	}

	assetURL, sumsURL := selfupdate.AssetURLs(base, target)
	tarball, err := selfupdate.Download(assetURL)
	if err != nil {
		return err
	}
	sums, err := selfupdate.Download(sumsURL)
	if err != nil {
		return err
	}
	if err := selfupdate.VerifySHA256(tarball, sums, selfupdate.AssetName); err != nil {
		return err
	}
	binary, err := selfupdate.ExtractBinary(tarball)
	if err != nil {
		return err
	}
	staged, err := selfupdate.Stage(targetDir, binary)
	if err != nil {
		return err
	}
	if err := selfupdate.SmokeCheck(staged, target); err != nil {
		// #nosec G703 -- staged comes from os.CreateTemp in the binary's own resolved directory, not external input.
		_ = os.Remove(staged)
		return fmt.Errorf("%v; install with the installer instead: curl -fsSL https://github.com/MMinasyan/lightcode/releases/latest/download/install.sh | LIGHTCODE_VERSION=%s sh", err, target)
	}
	if err := selfupdate.Replace(staged, targetPath); err != nil {
		return err
	}
	fmt.Fprintf(errW, "upgraded %s -> %s; takes effect the next time lightcode starts - including the GUI's own relaunch when switching projects.\n", version.String(), target)
	return nil
}

// runUpgradeCheck is a pure read: it branches before every installation
// gate, so it works on dev builds and against a pre-surface channel.
func runUpgradeCheck(positionals []string) error {
	if len(positionals) > 0 {
		return usageErrorf("--check takes no version argument")
	}
	if os.Getenv("LIGHTCODE_RELEASE_BASE") != "" {
		return usageErrorf("LIGHTCODE_RELEASE_BASE is set; --check is meaningless against a directory base")
	}
	latest, err := selfupdate.LatestTag()
	if err != nil {
		return err
	}
	result := selfupdate.Check(version.Version, version.String(), latest)
	if upgradeJSON {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Fprintln(outW, string(b))
		return nil
	}
	switch result.Status {
	case selfupdate.StatusBelowFloor:
		fmt.Fprintf(outW, "current: %s, latest: %s (not selfupdate-capable; use the installer)\n", result.Current, result.Latest)
	case selfupdate.StatusDevBuild:
		fmt.Fprintf(outW, "current: %s, latest: %s\n", result.Current, result.Latest)
	case selfupdate.StatusUpdateAvailable:
		fmt.Fprintf(outW, "current: %s, latest: %s (update available; run: lightcode upgrade)\n", result.Current, result.Latest)
	default:
		fmt.Fprintf(outW, "current: %s, latest: %s (up to date)\n", result.Current, result.Latest)
	}
	return nil
}

// runUninstall removes the installed binary and, with purge, the data
// directory. Data validation and removal always precede binary removal —
// a failed purge after a successful binary unlink would leave no command
// to retry with.
func runUninstall(purge, yes, dryRun bool) error {
	binPath, binDir, err := selfupdate.ResolveExecutable()
	if err != nil {
		return err
	}
	// The home dir is resolved only where the data half needs it: the
	// binary-only flow (sudo's half of the privileged two-step) must work
	// without $HOME, and the root purge refusal precedes any home lookup.
	resolveDataDir := func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, ".lightcode"), nil
	}

	// A pure read: it works in a pipe without --yes and against an
	// unwritable install.
	if dryRun {
		fmt.Fprintf(outW, "would remove %s\n", binPath)
		if purge {
			dataDir, err := resolveDataDir()
			if err != nil {
				return err
			}
			fmt.Fprintf(outW, "would remove %s (all data: API keys, sessions, snapshots)\n", dataDir)
		}
		return nil
	}

	dataDir := ""
	if purge {
		if selfupdate.Geteuid() == 0 {
			return fmt.Errorf("refusing to purge user data as root; run 'lightcode uninstall --purge' as the owning user first, then remove the binary with sudo")
		}
		dataDir, err = resolveDataDir()
		if err != nil {
			return err
		}
		state, err := selfupdate.ClassifyDataDir(dataDir)
		if err != nil {
			return err
		}
		switch state {
		case selfupdate.DataDirAbsent:
			fmt.Fprintf(errW, "no data directory at %s - nothing to purge\n", dataDir)
			// Continue exactly as a plain no-purge uninstall, so the
			// privileged partial-flow message (which claims data was
			// removed) can never fire when nothing was purged.
			purge = false
		case selfupdate.DataDirSymlink:
			return fmt.Errorf("~/.lightcode is a symlink; refusing to purge through it")
		case selfupdate.DataDirNotDir:
			return fmt.Errorf("~/.lightcode is not a directory; refusing to purge")
		case selfupdate.DataDirNotOwned:
			return fmt.Errorf("~/.lightcode is not owned by the current user; refusing to purge")
		}
	}

	// Without a purge nothing useful can happen against an unwritable
	// install, and the refusal must be deterministic when both this
	// preflight and the no-TTY refusal would fire.
	if !purge && !selfupdate.DirWritable(binDir) {
		return fmt.Errorf("binary is at %s (not writable); re-run as: sudo lightcode uninstall", binPath)
	}

	if !yes {
		if !selfupdate.IsTerminal() {
			return fmt.Errorf("uninstall is interactive; pass --yes to confirm non-interactively")
		}
		if purge {
			fmt.Fprintf(errW, "Remove %s and ALL data in %s (API keys, sessions, snapshots)? [y/N] ", binPath, dataDir)
		} else {
			fmt.Fprintf(errW, "Remove %s? [y/N] ", binPath)
		}
		answer, _ := bufio.NewReader(selfupdate.PromptInput).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			fmt.Fprintln(errW, "cancelled")
			return errSilent
		}
	}

	if purge {
		if err := selfupdate.PurgeData(dataDir); err != nil {
			return err
		}
		fmt.Fprintf(errW, "removed %s\n", dataDir)
	}

	selfupdate.SweepStaged(binDir)
	if err := os.Remove(binPath); err != nil {
		if purge {
			// The privileged two-step: data is gone, the root-owned
			// binary needs sudo. The requested operation is incomplete.
			return fmt.Errorf("data removed; binary still installed at %s - run: sudo lightcode uninstall --yes", binPath)
		}
		return err
	}
	fmt.Fprintf(errW, "removed %s\n", binPath)
	return nil
}

// runModels lists models through the same agent bootstrap as every
// cli/GUI start (it may refresh a stale discovery cache and auto-create
// the config templates on first run — doctor and config are the
// zero-write commands). The handler filters Incomplete entries from the
// default view itself: ModelList() resolves refs via LookupOrIncomplete
// and filters hidden only.
func runModels(provider string, all, asJSON bool) error {
	svc, err := buildAgent()
	if err != nil {
		return err
	}

	var rows []agent.ModelListEntry
	if all {
		rows = svc.AllModelList()
	} else {
		for _, row := range svc.ModelList() {
			if row.Incomplete {
				continue
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ref < rows[j].Ref })

	hint := ""
	if provider != "" {
		// Validate against ProviderList: the filtered rows cannot
		// distinguish an unknown provider from a known-but-disconnected
		// one.
		var match *agent.ProviderStatus
		var known []string
		for _, p := range svc.ProviderList() {
			known = append(known, p.ID)
			if p.ID == provider {
				m := p
				match = &m
			}
		}
		if match == nil {
			return usageErrorf("unknown provider %q (known: %s)", provider, strings.Join(known, ", "))
		}
		filtered := rows[:0]
		for _, row := range rows {
			if row.Provider == provider {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
		switch {
		case !match.Connected:
			rows = nil
			hint = fmt.Sprintf("provider %q is not connected - connect it in Settings first", provider)
		case len(rows) == 0:
			hint = fmt.Sprintf("no visible models for %q - rerun with --all", provider)
		}
	} else if len(rows) == 0 {
		anyConnected := false
		for _, p := range svc.ProviderList() {
			if p.Connected {
				anyConnected = true
				break
			}
		}
		if anyConnected {
			hint = "no visible models - rerun with --all"
		} else {
			hint = "no models available - connect a provider in Settings first"
		}
	}

	if hint != "" {
		fmt.Fprintln(errW, hint)
	}
	if asJSON {
		// Always valid JSON on stdout: an empty result is [], never
		// empty bytes.
		if rows == nil {
			rows = []agent.ModelListEntry{}
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		fmt.Fprintln(outW, string(b))
		return nil
	}
	for _, row := range rows {
		var markers []string
		if row.Default {
			markers = append(markers, "default")
		}
		if row.Hidden {
			markers = append(markers, "hidden")
		}
		if row.Incomplete {
			markers = append(markers, "incomplete")
		}
		marker := "-"
		if len(markers) > 0 {
			marker = strings.Join(markers, ",")
		}
		fmt.Fprintf(outW, "%s\t%d\t%d\t%s\n", row.Ref, row.ContextWindow, row.MaxOutputTokens, marker)
	}
	return nil
}

// runConfig prints the raw config bytes (cat semantics — pipes stay
// intact) or, with --path, the resolved path. It never auto-creates the
// file.
func runConfig(pathOnly bool) error {
	path, err := config.ResolvePath()
	if err != nil {
		return err
	}
	if pathOnly {
		// The path IS the stdout answer; the env note below does not
		// fire here.
		fmt.Fprintln(outW, path)
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("config not created yet (run lightcode once to initialize): %s", path)
	}
	if err != nil {
		return err
	}
	if os.Getenv("LIGHTCODE_CONFIG") != "" {
		fmt.Fprintf(errW, "using config at %s\n", path)
	}
	_, err = outW.Write(data)
	return err
}

// runDoctor renders the diagnostics report on outW. Any FAIL makes the
// command exit 1; warnings alone exit 0.
func runDoctor(asJSON bool) error {
	home, homeErr := os.UserHomeDir()
	cfgPath, cfgPathErr := config.ResolvePath()
	workDir, _ := os.Getwd()
	binPath, binErr := os.Executable()
	if binErr != nil {
		binPath = fmt.Sprintf("unknown (%v)", binErr)
	}
	report, noProviders := doctor.Run(doctor.Params{
		Home:          home,
		HomeErr:       homeErr,
		ConfigPath:    cfgPath,
		ConfigPathErr: cfgPathErr,
		WorkDir:       workDir,
		BinaryPath:    binPath,
		Version:       version.String(),
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		LookupEnv:     os.LookupEnv,
	})

	if asJSON {
		b, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(outW, string(b))
	} else {
		for _, c := range report.Checks {
			fmt.Fprintf(outW, "%s %s/%s: %s\n", c.Status, c.Group, c.Name, c.Detail)
		}
		fmt.Fprintf(outW, "doctor: %d ok, %d warnings, %d failures\n", report.OK, report.Warnings, report.Failures)
		if noProviders && report.Failures == 0 {
			fmt.Fprintln(outW, "no providers connected yet - this is expected on a new install")
		}
	}
	if report.Failures > 0 {
		return fmt.Errorf("doctor found %d failures", report.Failures)
	}
	return nil
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
	case errors.Is(err, errSilent):
		return false, 1
	case exitCodeOf(err) >= 0:
		return false, exitCodeOf(err)
	case errors.Is(err, errUsage):
		fmt.Fprintf(errW, "lightcode: %v\n", err)
		renderCommandUsage(errW, cmd)
		return false, 2
	default:
		fmt.Fprintf(errW, "lightcode: %v\n", err)
		return false, 1
	}
}

func exitCodeOf(err error) int {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
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
