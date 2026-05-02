package cli

import (
	"fmt"
	"os"
	"syscall"
)

func (c *CLI) relaunchIn(dir string) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lightcode binary: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("chdir: %w", err)
	}
	return syscall.Exec(bin, []string{bin, "cli"}, os.Environ())
}
