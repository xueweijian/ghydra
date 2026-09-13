//go:build !windows

package rules

import "os"

// readFileRetry unix：posix 语义下并发读写无 sharing violation，直通。
func readFileRetry(path string) ([]byte, error) {
	return os.ReadFile(path)
}
