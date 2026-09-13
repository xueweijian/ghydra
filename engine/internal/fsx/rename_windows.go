//go:build windows

package fsx

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// RenameAtomic windows：MoveFileEx(REPLACE_EXISTING) 撞并发句柄时按
// 版本/时序报 SHARING_VIOLATION / LOCK_VIOLATION / ACCESS_DENIED
// （rules 包 CI 实测三种都出现，2026-09-13）。指数退避总窗口 ~6.4s——
// W4 加宽：CI windows-2022 实证 Defender 实时扫描新构建 exe 会持锁
// 数秒（rules 时代的 ~800ms 不够自更新场景）。
func RenameAtomic(tmp, dst string) error {
	var err error
	for _, backoff := range []time.Duration{
		0, 10 * time.Millisecond, 25 * time.Millisecond,
		50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond,
		1600 * time.Millisecond, 3200 * time.Millisecond,
	} {
		time.Sleep(backoff)
		err = os.Rename(tmp, dst)
		if err == nil {
			return nil
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			return err
		}
		if errno != windows.ERROR_SHARING_VIOLATION && errno != windows.ERROR_LOCK_VIOLATION && errno != windows.ERROR_ACCESS_DENIED {
			return err
		}
	}
	return err
}

func readFileRetry(path string) ([]byte, error) {
	var b []byte
	var err error
	for _, backoff := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		time.Sleep(backoff)
		b, err = os.ReadFile(path)
		if err == nil {
			return b, nil
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			return nil, err
		}
		if errno != windows.ERROR_SHARING_VIOLATION && errno != windows.ERROR_LOCK_VIOLATION {
			return nil, err
		}
	}
	return nil, err
}
