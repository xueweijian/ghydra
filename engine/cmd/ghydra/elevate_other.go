//go:build !windows

// elevate_other.go —— 非 Windows 无 UAC 语义（v1.0.3 PR3）：
// 不可写由 decideElevate 的错误路径直接给出 sudo 指引（runas 恒失败，
// 决策表把它折叠进「需要管理员权限」人话文案）。
package main

import "errors"

func runasRelaunch(exe, args string) error {
	return errors.New("此平台不支持 UAC 提权重跑（请用 sudo 运行）")
}
