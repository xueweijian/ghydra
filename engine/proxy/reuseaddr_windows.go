//go:build windows

package proxy

import "syscall"

// setReuseAddr 在 Windows 上不设 SO_REUSEADDR：其语义与 Unix 不同
// （允许强占他人端口），本地代理不冒这个险。快速重绑场景由
// SO_EXCLUSIVEADDRUSE 的缺省行为兜底。
func setReuseAddr(fd syscall.Handle) error {
	return nil
}
