//go:build windows

package melta

import (
	"os/exec"
	"syscall"
)

func executableProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func killExecutableProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
