//go:build !windows

package rules

import "os"

// syncDir 目录 fsync：保证 rename 的目录项持久化（崩溃后新文件名可见）。
// Windows 无此语义（NTFS 元数据日志保证），由 syncdir_windows.go 空实现。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
