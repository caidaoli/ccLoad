//go:build !windows

package app

import "syscall"

// setTCPNoDelay 在 Unix 系统上设置 TCP_NODELAY
func setTCPNoDelay(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}
