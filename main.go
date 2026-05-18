package main

import (
	"context"
	"embed"
	"flag"
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
	// Subcommand dispatch — serve and acp run in the foreground.
	if len(os.Args) >= 2 {
		var err error
		switch os.Args[1] {
		case "serve":
			err = runServe(os.Args[2:])
		case "acp":
			err = runACP()
		case "cli":
			err = runCLI()
		}
		if os.Args[1] == "serve" || os.Args[1] == "acp" || os.Args[1] == "cli" {
			if err != nil {
				fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	// Wails GUI path — detach from the terminal.
	if os.Getenv("LIGHTCODE_DETACHED") != "1" {
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

	if err := runWails(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: %v\n", err)
		os.Exit(1)
	}
}

// buildAgent performs shared setup (dotenv, logging, config) and
// constructs the Agent that all adapters share.
func buildAgent() (*agent.Agent, error) {
	if err := config.LoadDotEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: .env: %v\n", err)
	}

	level := slog.LevelWarn
	if os.Getenv("LIGHTCODE_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfgPath := os.Getenv("LIGHTCODE_CONFIG")
	if cfgPath == "" {
		p, perr := config.ConfigPath()
		if perr != nil {
			return nil, fmt.Errorf("resolve config path: %w", perr)
		}
		cfgPath = p
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
		ProjectRoot: projectRoot,
		Home:        home,
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

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "listen port (0 = OS-assigned)")
	_ = fs.Parse(args)

	svc, err := buildAgent()
	if err != nil {
		return err
	}

	home, _ := os.UserHomeDir()
	proj, err := svc.Projects().Ensure()
	if err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}

	srv := server.New(svc, server.Config{Port: *port})
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
