// Package proxy 实现 GHydra 的 HTTP CONNECT 代理内核（通道 A 数据面）。
//
// M1 架构（GHydra-M1-Plan.md §2.1）：系统代理/PAC 的标准协议是
// HTTP CONNECT——浏览器与 git 原生支持。流程：客户端 CONNECT
// host:443 → 规则引擎判定 → 命中则由调度器给出择优 IP 直连，
// 未命中则原样直连域名（零打扰放行）→ 回写 200 → TLS 流原样
// 双向转发（不解密，ClientHello 的 SNI 由客户端自带）。
//
// M0 的裸 SNI 转发器保留在 cmd/ghydra poc 作为调试工具。
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultDialTimeout 是上游连接超时（dev-sidecar v2.2 实证参数：
// 15-21s → 7s，熔断感知足够快、正常握手足够宽）。
const DefaultDialTimeout = 7 * time.Second

// UpstreamSelector 决定目标域名的转发上游。
// 返回 ok=false 表示不加速：代理将按系统 DNS 直连域名本身。
type UpstreamSelector interface {
	Select(host string) (addr string, ok bool)
}

// SelectorFunc 函数适配器。
type SelectorFunc func(host string) (addr string, ok bool)

func (f SelectorFunc) Select(host string) (string, bool) { return f(host) }

// Event 是一次隧道连接的完整生命周期事件（连接结束或失败时上报一次）。
// 回调必须非阻塞（只做入队），它在数据面路径上被同步调用。
type Event struct {
	Host      string    // 目标域名（剥端口）
	Target    string    // 实际拨号地址
	Accel     bool      // 是否命中加速规则
	DialErr   error     // 上游连接失败原因（nil = 成功）
	DialMS    float64   // 上游连接耗时
	Rx, Tx    int64     // 下行 / 上行字节总数
	StartedAt time.Time // 隧道发起时刻
}

// Server 是 CONNECT 代理服务。构造后调用 Serve 开始接受连接。
type Server struct {
	Selector    UpstreamSelector
	OnEvent     func(Event)                      // 可选；事件回调（非阻塞约定见 Event）
	DialTimeout time.Duration                    // 零值 = DefaultDialTimeout
	Logf        func(format string, args ...any) // 可选调试日志

	bufPool sync.Pool // []byte 32KB，双向转发复用（M0 千并发教训：buffer 池而非连接池）
}

// Serve 循环接受连接，每连接一个 goroutine 处理。listener 关闭时返回。
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err // net.ErrClosed 由调用方按需区分
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 8192)
	req, err := http.ReadRequest(br)
	if err != nil {
		return // 畸形请求：直接断开（客户端拿 EOF 自会重试或报错）
	}
	if req.Method != http.MethodConnect {
		writePlain(conn, http.StatusBadRequest, "ghydra: 仅支持 CONNECT 隧道（GitHub 流量全 HTTPS）")
		return
	}
	authority := req.URL.Host // net/http 对 CONNECT 的 authority-form 已归一化
	if authority == "" {
		authority = req.Host
	}
	if authority == "" {
		writePlain(conn, http.StatusBadRequest, "ghydra: CONNECT 缺少目标")
		return
	}
	if _, _, err := net.SplitHostPort(authority); err != nil {
		authority = net.JoinHostPort(authority, "443")
	}
	hostOnly, _, _ := net.SplitHostPort(authority)

	target, accel := authority, false
	if s.Selector != nil {
		if up, ok := s.Selector.Select(hostOnly); ok && up != "" {
			target, accel = up, true
		}
	}
	ev := Event{Host: hostOnly, Target: target, Accel: accel, StartedAt: time.Now()}

	t0 := time.Now()
	up, err := net.DialTimeout("tcp", target, s.dialTimeout())
	ev.DialMS = msSince(t0)
	if err != nil {
		ev.DialErr = err
		s.emit(ev)
		writePlain(conn, http.StatusBadGateway, "ghydra: 上游连接失败: "+err.Error())
		return
	}
	defer up.Close()

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		ev.DialErr = err
		s.emit(ev)
		return
	}

	// 双向转发。br 可能已缓冲客户端在等 200 期间预发的数据
	// （TLS 客户端激进时不等 200 就发 ClientHello）——从 br 起拷贝即覆盖。
	txBuf := s.getBuf()
	rxBuf := s.getBuf()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ev.Tx, _ = io.CopyBuffer(up, br, txBuf)
		s.putBuf(txBuf)
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite() // 半关闭：让对端感知 EOF
		}
	}()
	ev.Rx, _ = io.CopyBuffer(conn, up, rxBuf)
	s.putBuf(rxBuf)
	// 等上行 goroutine 退出后再 emit，保证 Rx/Tx 完整。
	// 上行退出依赖 up 读到 EOF/FIN——先关 down 侧不阻塞 up 读；客户端关连接后 br 返回 EOF。
	wg.Wait()
	s.emit(ev)
}

func (s *Server) dialTimeout() time.Duration {
	if s.DialTimeout > 0 {
		return s.DialTimeout
	}
	return DefaultDialTimeout
}

func (s *Server) emit(ev Event) {
	if s.OnEvent != nil {
		s.OnEvent(ev)
	}
}

func (s *Server) getBuf() []byte {
	if b, _ := s.bufPool.Get().([]byte); b != nil {
		return b
	}
	return make([]byte, 32*1024)
}

func (s *Server) putBuf(b []byte) {
	s.bufPool.Put(b[:cap(b)]) // 归还时保留完整容量
}

func writePlain(conn net.Conn, code int, msg string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
}

func msSince(t0 time.Time) float64 {
	return float64(time.Since(t0).Microseconds()) / 1000
}
