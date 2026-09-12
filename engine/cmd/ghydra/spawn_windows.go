//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// detachAttr 脱离控制台的进程属性（DETACHED_PROCESS |
// CREATE_NEW_PROCESS_GROUP）：ghydra on 退出后 serve 存活。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x8 | 0x200}
}

// killServe Windows：taskkill 树杀（off 已先恢复代理，强杀无副作用）。
func killServe(pid int) {
	exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
