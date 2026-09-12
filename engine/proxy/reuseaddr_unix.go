//go:build !windows

package proxy

import "syscall"

// setReuseAddr 允许 TIME_WAIT 状态下快速重绑本地端口（服务重启场景）。
func setReuseAddr(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
