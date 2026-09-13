//go:build !windows

package fsx

import "os"

// RenameAtomic posix：rename(2) 同文件系统原子，直通。
func RenameAtomic(tmp, dst string) error {
	return os.Rename(tmp, dst)
}

func readFileRetry(path string) ([]byte, error) {
	return os.ReadFile(path)
}
