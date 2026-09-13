//go:build !windows

package rules

import "os"

// renameAtomic unix：rename 天然允许覆盖任意可写文件，无需重试。
func renameAtomic(tmp, dst string) error {
	return os.Rename(tmp, dst)
}
