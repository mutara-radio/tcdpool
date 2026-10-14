//go:build linux

package nostar

import (
	"net"
	"syscall"
)

// sysProcAttr makes the kernel stop tcd when tcdpool dies, so no tcd is
// left behind.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

const tcpQuickAck = 12 // TCP_QUICKACK, linux/tcp.h

// quickAck asks the kernel to send ACKs at once, not delayed. An
// unpatched tcd sends with Nagle's algorithm on: a reply then waits for
// the ACK of the reply before it, and a delayed ACK adds about one
// frame (20 ms) to each reply. The kernel can clear the flag, so it is
// set again before each read. Errors are ignored: it is only a speed-up.
func quickAck(c *net.TCPConn) {
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpQuickAck, 1) })
}
