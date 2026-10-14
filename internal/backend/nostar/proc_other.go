//go:build !linux

package nostar

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
