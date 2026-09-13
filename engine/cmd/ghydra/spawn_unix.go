//go:build !windows

package main

import "syscall"

// detachAttr setsid 脱离终端会话（孤儿进程由 init 收养）。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// killServe Unix：SIGTERM 优雅退出（serve hook 会 flush 持久化）。
func killServe(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
}

// killServeHard Unix：SIGKILL 强杀（Stop() 优雅超时后的兜底——慢死的
// serve 会让重启的新 serve 撞端口迁移，见 selfupdateglue.go Stop 注释）。
func killServeHard(pid int) {
	syscall.Kill(pid, syscall.SIGKILL)
}
