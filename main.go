package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/MMinasyan/lightcode/internal/acp"
	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/cli"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/server"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Strict dispatch: a known command runs, anything else errors. Only a
	// bare invocation or the desktop command reaches the GUI path.
	launchGUI, code := dispatch(os.Args)
	if !launchGUI {
		os.Exit(code)
	}

	// Wails GUI path — detach from the terminal.
	if shouldDetach() {
		detachAndExit()
	}

	if err := runWails(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
		os.Exit(1)
	}
}

func shouldDetach() bool {
	return os.Getenv("LIGHTCODE_DETACHED") != "1"
}

// detachAndExit re-launches the binary detached from the terminal and
// exits; the child re-enters main with no args and LIGHTCODE_DETACHED=1.
func detachAndExit() {
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(bin)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = append(os.Environ(), "LIGHTCODE_DETACHED=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// buildAgent performs shared setup (dotenv, logging, config) and
// constructs the Agent that all adapters share.
func buildAgent() (*agent.Agent, error) {
	managedEnv, err := config.LoadDotEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: .env: %v\n", err)
	}

	level := slog.LevelWarn
	if os.Getenv("LIGHTCODE_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfgPath, err := config.ResolvePath()
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	projectRoot, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}

	return agent.New(agent.Config{
		Cfg:         cfg,
		ConfigPath:  cfgPath,
		ProjectRoot: projectRoot,
		Home:        home,
		Env:         managedEnv,
	})
}

func runCLI() error {
	svc, err := buildAgent()
	if err != nil {
		return err
	}
	return cli.New(svc).Run(context.Background())
}

func runACP() error {
	svc, err := buildAgent()
	if err != nil {
		return err
	}
	return acp.New(svc).Run(context.Background())
}

func runServe(port int) error {
	svc, err := buildAgent()
	if err != nil {
		return err
	}

	home, _ := os.UserHomeDir()
	proj, err := svc.Projects().Ensure()
	if err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}

	srv := server.New(svc, server.Config{Port: port})
	return srv.Serve(context.Background(), home, proj.ID)
}

func runWails() error {
	svc, err := buildAgent()
	if err != nil {
		return err
	}

	app := &App{svc: svc}

	return wails.Run(&options.App{
		Title:  "Lightcode — " + svc.ProjectName(),
		Width:  900,
		Height: 700,
		Linux: &linux.Options{
			WebviewGpuPolicy: linux.WebviewGpuPolicyAlways,
		},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		Bind:      []interface{}{app},
	})
}
