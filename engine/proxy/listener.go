package proxy

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// NewListener 在 addr（形如 "127.0.0.1:9801"，仅支持 IPv4 回环）
// 上创建 TCP listener。
//
// 为什么不走 net.Listen：Go 标准库固定 listen(fd, somaxconn)，
// 不暴露 backlog 参数，Linux 上还被 /proc/sys/net/core/somaxconn
// （常见 128）二次 clamp——M0 千并发实测中 1000 瞬时连接风暴的
// 尾延迟根因即在此（SYN 重传退避）。自建 socket 可显式指定
// backlog（调研：wanglong.cv/articles/tcp-listen-backlog-linux/）。
//
// TCP_NODELAY 无需手设：Go net 包对 TCP 连接默认开启（dial 与
// accept 两侧均是）。
func NewListener(addr string, backlog int) (net.Listener, error) {
	if backlog <= 0 {
		backlog = 4096
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("地址格式: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("仅支持 IPv4 字面量地址，得到 %q（M1 代理只监听本地回环）", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("端口非法: %q", portStr)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := setReuseAddr(fd); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	var sa [4]byte
	copy(sa[:], ip.To4())
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
