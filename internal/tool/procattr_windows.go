package tool

import "syscall"

func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
