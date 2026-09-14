//go:build !windows

package main

import "syscall"

// reapUnix 僵尸收尸（测试用：真实卸载场景 off 与 serve 无父子关系，
// init 负责收尸；Waitpid 不存在错误直接忽略）。
func reapUnix(pid int) {
	syscall.Wait4(pid, nil, syscall.WNOHANG, nil) //nolint:errcheck
}
