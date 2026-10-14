//go:build !linux

package nostar

import (
	"net"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func quickAck(*net.TCPConn) {}
