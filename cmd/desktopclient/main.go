// Command desktopclient is the explicitly built development preview host for
// the target desktop client: the native bridge components over an explicit
// assets directory, without Runtime composition. It is not part of the
// shipped command registry.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/desktopclient"
)

// defaultAssetsDir is the built target frontend relative to this preview
// host's project directory: the same entry, build and asset layout the
// default build emits and the shipped binary embeds.
const defaultAssetsDir = "../../frontend/dist"

// parseArgs parses the preview host's fixed argument surface: only
// --assets-dir and --runtime-executable exist, and every positional or
// unknown argument is an error before Wails opens. In particular, a
// contender invocation of this preview's nonexistent runtime command is a
// positional argument and exits, never recursively opening a window.
func parseArgs(args []string, output io.Writer) (assetsDir, runtimeExecutable string, err error) {
	flags := flag.NewFlagSet("desktopclient", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&assetsDir, "assets-dir", defaultAssetsDir, "directory of built frontend assets")
	flags.StringVar(&runtimeExecutable, "runtime-executable", "", "override the runtime launcher executable")
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if flags.NArg() != 0 {
		return "", "", fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	return assetsDir, runtimeExecutable, nil
}

func run(args []string) int {
	assetsDir, runtimeExecutable, err := parseArgs(args, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktopclient: %v\n", err)
		return 2
	}
	opts, err := clienthost.DefaultOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktopclient: %v\n", err)
		return 1
	}
	if runtimeExecutable != "" {
		opts.Executable = runtimeExecutable
	}
	workspace, err := filepath.Abs(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktopclient: %v\n", err)
		return 1
	}
	host := desktopclient.New(opts, workspace)
	if err := desktopclient.Run(host, os.DirFS(assetsDir)); err != nil {
		fmt.Fprintf(os.Stderr, "desktopclient: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:]))
}
