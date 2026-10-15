package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/acpclient"
	"github.com/MMinasyan/lightcode/internal/cliclient"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/desktopclient"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Diagnostics only: no config or env-file loading happens before
	// dispatch.
	level := slog.LevelWarn
	if os.Getenv("LIGHTCODE_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Strict dispatch: a known command runs, anything else errors. Only a
	// bare invocation or the desktop command reaches the GUI path.
	launchGUI, code := dispatch(os.Args)
	if !launchGUI {
		os.Exit(code)
	}

	if err := runWails(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
		os.Exit(1)
	}
}

// workspace resolves the invocation directory once for every client entry.
func workspace() (string, error) {
	return filepath.Abs(".")
}

func runCLI() error {
	dir, err := workspace()
	if err != nil {
		return err
	}
	return cliclient.New(cliclient.Options{Startup: clienthost.Options{}, Workspace: dir}).Run(context.Background(), os.Stdin, os.Stdout)
}

func runACP() error {
	dir, err := workspace()
	if err != nil {
		return err
	}
	err = acpclient.New(acpclient.Options{Startup: clienthost.Options{}, Workspace: dir}).Run(context.Background(), os.Stdin, os.Stdout)
	// A signal shutdown is the bridge's successful exit, matching the
	// retained terminal-signal contract of the ACP entry.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runWails() error {
	dir, err := workspace()
	if err != nil {
		return err
	}
	host := desktopclient.New(clienthost.Options{}, dir)
	return desktopclient.Run(host, assets)
}
