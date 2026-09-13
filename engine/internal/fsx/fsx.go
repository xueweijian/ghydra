// Package fsx 文件系统原子操作（W4）：rules 包 W2 时期验证过的模式
// （tmp+fsync+rename、Windows 锁退避）在此泛化，供 selfupdate 等消费。
// rules 包暂不迁移（CI 已锁定的成熟代码不动，v1.0 后统一去重）。
package fsx

import (
	"fmt"
	"os"
)

// WriteFileAtomic tmp+fsync+rename 单文件原子落盘。
// 崩溃只留 .tmp 残留，不破坏旧内容。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("fsx: open %s: %w", tmp, err)
	}
	// umask 会让 create perm 失真，显式 chmod 保证跨环境确定
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("fsx: chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("fsx: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsx: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fsx: close %s: %w", tmp, err)
	}
	if err := RenameAtomic(tmp, path); err != nil {
		return fmt.Errorf("fsx: rename %s: %w", tmp, err)
	}
	return nil
}

// ReadFileRetry 读侧退避（Windows rename 瞬间的 sharing violation；
// posix 直通零开销）。
func ReadFileRetry(path string) ([]byte, error) {
	return readFileRetry(path)
}
