package proxy

import (
	"fmt"
	"net"
	"strconv"
)

// NewListener 在 addr（形如 "127.0.0.1:9801"，仅支持 IPv4 字面量，
// M1 代理只监听本地回环）上创建 TCP listener。
//
// 为什么 Unix 不走 net.Listen：Go 标准库固定 listen(fd, somaxconn)，
// 不暴露 backlog 参数，Linux 上还被 /proc/sys/net/core/somaxconn
// （常见 128）二次 clamp——M0 千并发实测中 1000 瞬时连接风暴的
// 尾延迟根因即在此（SYN 重传退避）。Unix 自建 socket 显式指定
// backlog；Windows 回退 net.Listen：Go 后端已传 SOMAXCONN 且 NT
// 内核动态管理 accept 队列，无 Linux 式 clamp 问题（且 Windows 的
// net.FileListener 不支持任意 fd，见 golang/go#26072）。
//
// TCP_NODELAY 无需手设：Go net 包对 TCP 连接默认开启。
func NewListener(addr string, backlog int) (net.Listener, error) {
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
	_ = port
	return newListenerOS(addr, backlog)
}
