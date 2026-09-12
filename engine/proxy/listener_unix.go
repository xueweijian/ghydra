//go:build !windows

package proxy

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// newListenerOS Unix 路径：自建 socket 精控 backlog。
func newListenerOS(addr string, backlog int) (net.Listener, error) {
	if backlog <= 0 {
		backlog = 4096
	}
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr) // 范围已在公共 NewListener 校验

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	var sa [4]byte
	copy(sa[:], net.ParseIP(host).To4())
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: sa}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", addr, err)
	}
	if err := syscall.Listen(fd, backlog); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen backlog=%d: %w", backlog, err)
	}

	file := os.NewFile(uintptr(fd), addr)
	ln, err := net.FileListener(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("FileListener: %w", err)
	}
	// FileListener 内部已 dup fd；关闭原始句柄防泄漏。
	file.Close()
	return ln, nil
}
