//go:build windows

package rules

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// renameAtomic windows：MoveFileEx(REPLACE_EXISTING) 在目标文件正被其他
// 句柄打开时按版本/时序报两种错——ERROR_SHARING_VIOLATION 或
// ERROR_ACCESS_DENIED（CI 实测两种都出现，2026-09-13）。防御 = 指数退避
// 重试（总窗口 ~800ms，读侧 os.ReadFile 持句柄为毫秒级，必释放）；
// 持续失败最终上抛最后一次错误。
func renameAtomic(tmp, dst string) error {
	var err error
	for _, backoff := range []time.Duration{
		0, 10 * time.Millisecond, 25 * time.Millisecond,
		50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
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
