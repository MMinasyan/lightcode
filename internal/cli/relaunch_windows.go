package cli

import (
	"fmt"
	"os"
	"os/exec"
)

func (c *CLI) relaunchIn(dir string) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lightcode binary: %w", err)
	}
	cmd := exec.Command(bin, "cli")
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = detachAttr()
	return cmd.Start()
}
