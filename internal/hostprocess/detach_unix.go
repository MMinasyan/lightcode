//go:build !windows

// Package hostprocess provides process attributes for hosts that detach child
// processes from the controlling terminal.
package hostprocess

import "syscall"

// DetachAttr returns the process attributes that detach a child process from
// the controlling terminal.
func DetachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
