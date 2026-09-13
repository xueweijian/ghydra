//go:build windows

package rules

// syncDir Windows 空实现：NTFS 不支持目录句柄 FlushFileBuffers，
// 文件级 fsync + rename 已满足崩溃安全目标（W2 设计 §4）。
func syncDir(dir string) error { return nil }
