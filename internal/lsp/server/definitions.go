package server

import (
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
	Install    func(cacheDir string) error
}

var definitions = []Definition{
	{
		Name:       "gopls",
		Command:    "gopls",
		Args:       []string{"serve"},
		Extensions: []string{".go"},
		Markers:    []string{"go.mod"},
		LanguageID: "go",
		Install: func(cacheDir string) error {
			cmd := exec.Command("go", "install", "golang.org/x/tools/gopls@latest")
			cmd.Env = append(os.Environ(), "GOBIN="+cacheDir)
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(cmd, 5*time.Minute)
		},
	},
	{
		Name:       "pyright",
		Command:    "pyright-langserver",
		Args:       []string{"--stdio"},
		Extensions: []string{".py"},
		Markers:    []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", ".python-version"},
		LanguageID: "python",
		Install: func(cacheDir string) error {
			cmd := exec.Command("npm", "install", "--prefix", cacheDir, "pyright")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(cmd, 5*time.Minute)
		},
	},
	{
		Name:       "typescript-language-server",
		Command:    "typescript-language-server",
		Args:       []string{"--stdio"},
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		Markers:    []string{"tsconfig.json", "jsconfig.json", "package.json"},
		LanguageID: "typescript",
		Install: func(cacheDir string) error {
			cmd := exec.Command("npm", "install", "--prefix", cacheDir, "typescript", "typescript-language-server")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(cmd, 5*time.Minute)
		},
	},
	{
		Name:       "rust-analyzer",
		Command:    "rust-analyzer",
		Args:       []string{},
		Extensions: []string{".rs"},
		Markers:    []string{"Cargo.toml"},
		LanguageID: "rust",
		Install: func(cacheDir string) error {
			cmd := exec.Command("rustup", "component", "add", "rust-analyzer")
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := runWithTimeout(cmd, 5*time.Minute); err != nil {
				return err
			}
			which := exec.Command("rustup", "which", "rust-analyzer")
			out, err := which.Output()
			if err != nil {
				return fmt.Errorf("locate rust-analyzer: %w", err)
			}
			src := strings.TrimSpace(string(out))
			dst := filepath.Join(cacheDir, "rust-analyzer")
			os.MkdirAll(cacheDir, 0755)
			os.Remove(dst)
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
		Install: func(cacheDir string) error {
			cmd := exec.Command("dotnet", "tool", "install", "--tool-path", cacheDir, "csharp-ls")
			cmd.Stdout = nil
			cmd.Stderr = nil
			return runWithTimeout(cmd, 5*time.Minute)
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

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		cmd.Process.Kill()
		return fmt.Errorf("install timed out after %v", timeout)
	}
}
