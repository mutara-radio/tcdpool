//go:build linux

package nostar

import "syscall"

// sysProcAttr makes the kernel stop tcd when tcdpool dies, so no tcd is
// left behind.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
