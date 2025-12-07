//go:build windows

package app

import "syscall"

// setTCPNoDelay 在 Windows 上设置 TCP_NODELAY
func setTCPNoDelay(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}
