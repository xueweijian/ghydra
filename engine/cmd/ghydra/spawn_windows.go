//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

// killServeHard Windows：taskkill /F（同 Unix 语义，Stop 超时兜底）。
func killServeHard(pid int) {
	exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// procAlive Windows：tasklist 过滤 PID（卸载器 off --wait 轮询用；
// 找到进程名 = 活着。taskkill /F 后进程表移除有延迟，轮询是唯一可靠法）。
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}
