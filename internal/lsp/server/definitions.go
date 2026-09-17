package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Definition struct {
	Name       string
	Command    string
	Args       []string
	Extensions []string
	Markers    []string
	LanguageID string
	// Install installs the server binary into its cache directory. The
	// context is the owner's lifetime: cancellation kills the install and
	// returns the cancellation error.
	Install func(ctx context.Context, cacheDir string) error
}

var definitions = []Definition{
	{
		Name:       "gopls",
		Command:    "gopls",
		Args:       []string{"serve"},
		Extensions: []string{".go"},
		Markers:    []string{"go.mod"},
		LanguageID: "go",
		Install: func(ctx context.Context, cacheDir string) error {
			cmd := exec.CommandContext(ctx, "go", "install", "golang.org/x/tools/gopls@latest")
			cmd.Env = append(os.Environ(), "GOBIN="+cacheDir)
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(ctx, cmd, 5*time.Minute)
		},
	},
	{
		Name:       "pyright",
		Command:    "pyright-langserver",
		Args:       []string{"--stdio"},
		Extensions: []string{".py"},
		Markers:    []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", ".python-version"},
		LanguageID: "python",
		Install: func(ctx context.Context, cacheDir string) error {
			cmd := exec.CommandContext(ctx, "npm", "install", "--prefix", cacheDir, "pyright")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(ctx, cmd, 5*time.Minute)
		},
	},
	{
		Name:       "typescript-language-server",
		Command:    "typescript-language-server",
		Args:       []string{"--stdio"},
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		Markers:    []string{"tsconfig.json", "jsconfig.json", "package.json"},
		LanguageID: "typescript",
		Install: func(ctx context.Context, cacheDir string) error {
			cmd := exec.CommandContext(ctx, "npm", "install", "--prefix", cacheDir, "typescript", "typescript-language-server")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(ctx, cmd, 5*time.Minute)
		},
	},
	{
		Name:       "rust-analyzer",
		Command:    "rust-analyzer",
		Args:       []string{},
		Extensions: []string{".rs"},
		Markers:    []string{"Cargo.toml"},
		LanguageID: "rust",
		Install: func(ctx context.Context, cacheDir string) error {
			cmd := exec.CommandContext(ctx, "rustup", "component", "add", "rust-analyzer")
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := runWithTimeout(ctx, cmd, 5*time.Minute); err != nil {
				return err
			}
			which := exec.CommandContext(ctx, "rustup", "which", "rust-analyzer")
			out, err := which.Output()
			if err != nil {
				return fmt.Errorf("locate rust-analyzer: %w", err)
			}
			src := strings.TrimSpace(string(out))
			dst := filepath.Join(cacheDir, "rust-analyzer")
			_ = os.MkdirAll(cacheDir, 0755)
			_ = os.Remove(dst)
			return os.Symlink(src, dst)
		},
	},
	{
		Name:       "clangd",
		Command:    "clangd",
		Args:       []string{"--log=error"},
		Extensions: []string{".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".hxx"},
		Markers:    []string{"CMakeLists.txt", "compile_commands.json", "Makefile", ".clang-format"},
		LanguageID: "c",
		Install:    nil,
	},
	{
		Name:       "csharp-ls",
		Command:    "csharp-ls",
		Args:       []string{},
		Extensions: []string{".cs"},
		Markers:    []string{".sln", ".csproj"},
		LanguageID: "csharp",
		Install: func(ctx context.Context, cacheDir string) error {
			cmd := exec.CommandContext(ctx, "dotnet", "tool", "install", "--tool-path", cacheDir, "csharp-ls")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(ctx, cmd, 5*time.Minute)
		},
	},
}

func All() []Definition {
	return definitions
}

func ForExtension(ext string) *Definition {
	for i := range definitions {
		for _, e := range definitions[i].Extensions {
			if e == ext {
				return &definitions[i]
			}
		}
	}
	return nil
}

func DetectFromProject(root string) []*Definition {
	var found []*Definition
	for i := range definitions {
		for _, marker := range definitions[i].Markers {
			if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
				found = append(found, &definitions[i])
				break
			}
		}
	}
	return found
}

func CacheDir(home, name string) string {
	return filepath.Join(home, ".cache", "lightcode", "lsp", name)
}

func ResolveBinary(home string, def *Definition) string {
	cacheDir := CacheDir(home, def.Name)

	direct := filepath.Join(cacheDir, def.Command)
	if _, err := os.Stat(direct); err == nil {
		return direct
	}

	npmBin := filepath.Join(cacheDir, "node_modules", ".bin", def.Command)
	if _, err := os.Stat(npmBin); err == nil {
		return npmBin
	}

	if p, err := exec.LookPath(def.Command); err == nil {
		return p
	}

	return ""
}

// runWithTimeout runs cmd under a five-minute-class wall-clock timeout
// derived from ctx: the timeout still bounds the install when the owner
// outlives it, and cancellation of the parent kills the process, reaps it,
// and returns the cancellation error.
func runWithTimeout(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
}
