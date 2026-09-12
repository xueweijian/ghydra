// Package fault 提供故障注入假上游（M2-W1）。
//
// 用途：W2 下载器故障切换测试、W4 故障演练脚本、退出标准①的
// 端到端 CI 化。每个构造器返回可用地址（host:port），Close 释放。
//
// 注入面与真实故障的对应（M0/W2 真网实证的特征）：
//   - 403：       源头拒绝（2025-04 型事件）——HTTP 层到达但被拒
//   - RST：       TCP 重置——accept 后立刻 RST 关闭（SO_LINGER=0）
//   - TLS 死：    握手黑洞——读走 ClientHello 后沉默（tx>0 rx==0 特征）
//   - 慢响应：    吞吐不足——头之后按配速滴流（吞吐启发切道的触发器）
//   - 黑洞：      连接建立但无响应（DNS 污染解析出的不可达地址同特征）
package fault

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Server 故障注入服务器统一接口。
type Server interface {
	Addr() string    // host:port（客户端目标地址）
	Request() string // 预期请求形态说明（测试注释/日志用）
	Close() error
}

// --- 403 源头拒绝 ---

type http403 struct{ ln net.Listener }

// New403 所有请求返回 403（带 GitHub 风格 body 以假乱真）。
func New403() (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "GitHub.com")
		w.WriteHeader(403)
		fmt.Fprint(w, "Request forbidden by administrative rules")
	})}
	go s.Serve(ln)
	return &http403{ln}, nil
}

func (h *http403) Addr() string    { return h.ln.Addr().String() }
func (h *http403) Request() string { return "HTTP 任意方法 → 403" }
func (h *http403) Close() error    { return h.ln.Close() }

// --- RST 重置 ---

type rst struct{ ln net.Listener }

// NewRST accept 后立即发 RST（连接被重置特征）。
func NewRST() (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0) // Close 发 RST 而非 FIN
			}
			c.Close()
		}
	}()
	return &rst{ln}, nil
}

func (r *rst) Addr() string    { return r.ln.Addr().String() }
func (r *rst) Request() string { return "TCP accept → 立即 RST" }
func (r *rst) Close() error    { return r.ln.Close() }

// --- TLS 握手黑洞 ---

type tlsDead struct{ ln net.Listener }

// NewTLSDead 完成 TCP accept、读走 ClientHello，然后沉默不响应
// （GFW TLS 干扰实测特征：客户端 tx=ClientHello 字节、rx=0）。
func NewTLSDead() (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				c.SetReadDeadline(time.Now().Add(30 * time.Second))
				c.Read(buf) // 读走 ClientHello（或任意首包）后挂死
				// 刻意不响应、不关闭：连接保持到客户端超时
				time.Sleep(5 * time.Minute)
				c.Close()
			}(c)
		}
	}()
	return &tlsDead{ln}, nil
}

func (d *tlsDead) Addr() string    { return d.ln.Addr().String() }
func (d *tlsDead) Request() string { return "TLS ClientHello → 沉默（握手黑洞）" }
func (d *tlsDead) Close() error    { return d.ln.Close() }

// --- 慢响应（吞吐不足） ---

type slow struct {
	ln net.Listener
	s  *http.Server
}

// NewSlow 响应头立即发，body 按每秒 rateB 字节滴流（模拟国际链路
// 拥塞/限速——吞吐启发式「前 1MB < 200KB/s」的触发器）。
func NewSlow(total int64, rateBps int) (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		chunk := make([]byte, rateBps/10) // 100ms 一个滴流片
		if chunk == nil {
			chunk = make([]byte, 1)
		}
		sent := int64(0)
		for sent < total {
			n := int64(len(chunk))
			if sent+n > total {
				n = total - sent
			}
			w.Write(chunk[:n])
			if flusher != nil {
				flusher.Flush()
			}
			sent += n
			time.Sleep(100 * time.Millisecond)
		}
	})}
	go s.Serve(ln)
	return &slow{ln, s}, nil
}

func (s *slow) Addr() string    { return s.ln.Addr().String() }
func (s *slow) Request() string { return "HTTP 200 + 按 rate 滴流 body" }
func (s *slow) Close() error    { return s.ln.Close() }

// --- 黑洞（无响应） ---

type blackhole struct{ ln net.Listener }

// NewBlackhole accept 队列满载但不 accept、不响应（DNS 污染解析出的
// 不可达地址、防火墙丢包特征：TCP 可能握手成功但应用层永远无响应）。
func NewBlackhole() (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// 不进入 Accept 循环：backlog 满后新连接被拒；已入队的连接无响应
	return &blackhole{ln}, nil
}

func (b *blackhole) Addr() string    { return b.ln.Addr().String() }
func (b *blackhole) Request() string { return "TCP backlog 挂起不响应" }
func (b *blackhole) Close() error    { return b.ln.Close() }

// --- 正常对照（健康上游，演练脚本用） ---

type healthy struct {
	ln net.Listener
}

// NewHealthy 200 OK + 小 body（对照组：证明注入的是「故障」而非
// 测试环境本身坏了）。
func NewHealthy() (Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	return &healthy{ln}, nil
}

func (h *healthy) Addr() string    { return h.ln.Addr().String() }
func (h *healthy) Request() string { return "HTTP 200 ok（对照）" }
func (h *healthy) Close() error    { return h.ln.Close() }

// TDead 为 TLS 探测提供「真 TLS 上游」的对照（正常完成握手）。
func TLSDialOK(addr string, timeout time.Duration) error {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	c.Close()
	return nil
}
