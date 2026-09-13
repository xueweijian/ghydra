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
// 句柄打开时返回 ERROR_SHARING_VIOLATION（读侧 os.ReadFile 的毫秒级窗口
// 即可触发，Linux 无此语义）。防御 = 指数退避重试：窗口瞬时，重试必过；
// 持续失败（文件被长期占用）最终返回最后一次错误，由调用方上抛。
func renameAtomic(tmp, dst string) error {
	var err error
	for _, backoff := range []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond} {
		time.Sleep(backoff)
		err = os.Rename(tmp, dst)
		if err == nil {
			return nil
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			return err
		}
		if errno != windows.ERROR_SHARING_VIOLATION && errno != windows.ERROR_LOCK_VIOLATION {
			return err
		}
	}
	return err
}
