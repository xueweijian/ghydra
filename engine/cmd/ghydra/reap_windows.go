//go:build windows

package main

// reapUnix Windows 无僵尸语义（tasklist 判定不受影响），空实现。
func reapUnix(pid int) {}
