//go:build windows

// elevate_windows.go —— UAC runas 壳（v1.0.3 PR3）。
// ShellExecuteW verb="runas"：系统弹 UAC，同意则以管理员启动新进程，
// 拒绝返回 SE_ERR_ACCESSDENIED（<=32 均 fail）。风格对齐 sysproxy/windows.go
// 的 LazyDLL 模式；不引 x/sys 新面（syscall 足够）。
package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	shell32          = syscall.NewLazyDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteW")
)

// runasRelaunch 弹 UAC 以管理员重跑 exe args；错误 = 用户拒绝/系统拒绝。
func runasRelaunch(exe, args string) error {
	verb, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	params, err := syscall.UTF16PtrFromString(args)
	if err != nil {
		return err
	}
	dir, err := syscall.UTF16PtrFromString(filepath.Dir(exe))
	if err != nil {
		return err
	}
	const swShownormal = 1
	rc, _, _ := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(dir)),
		swShownormal,
	)
	if rc <= 32 {
		return fmt.Errorf("ShellExecute runas code %d（5=用户取消）", rc)
	}
	return nil
}
