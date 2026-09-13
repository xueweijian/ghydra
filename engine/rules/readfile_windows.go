//go:build windows

package rules

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// readFileRetry windows：读者侧同样会撞 transient sharing violation——
// 写侧 rename 覆盖目标的瞬间，读者的 CreateFile 会拿到
// ERROR_SHARING_VIOLATION（实测于 CI：TestAtomicConcurrentReadWrite，
// 2026-09-13）。防御 = 指数退避重试，与写侧 renameAtomic 对称。
func readFileRetry(path string) ([]byte, error) {
	var b []byte
	var err error
	for _, backoff := range []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond} {
		time.Sleep(backoff)
		b, err = os.ReadFile(path)
		if err == nil {
			return b, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, err // 不存在是终态，立即返回
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			return nil, err
		}
		if errno != windows.ERROR_SHARING_VIOLATION && errno != windows.ERROR_LOCK_VIOLATION {
			return nil, err
		}
	}
	return b, err
}
