//go:build windows

package hostprocess

import "syscall"

// DetachAttr returns the process attributes that detach a child process from
// the controlling terminal.
func DetachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
}
