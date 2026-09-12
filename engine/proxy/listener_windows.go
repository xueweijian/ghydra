//go:build windows

package proxy

import "net"

// newListenerOS Windows 路径：回退 net.Listen。
//
// Go 的 Windows 后端 listen() 传 SOMAXCONN，NT 内核对 backlog 动态
// 管理（无 Linux somaxconn=128 的 clamp 问题）；且 net.FileListener
// 在 Windows 不支持包装任意 socket fd（golang/go#26072），自建
// socket 路径行不通。backlog 参数被忽略（保持调用方签名一致）。
//
// Windows 也不设 SO_REUSEADDR（其语义允许强占端口，本地代理不冒
// 这个险）；TIME_WAIT 快速重绑场景由 SO_EXCLUSIVEADDRUSE 缺省兜底。
func newListenerOS(addr string, backlog int) (net.Listener, error) {
	return net.Listen("tcp4", addr)
}
