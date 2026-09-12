package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/internal/testutil"
)

// TestNewListenerBasic 验证自建 listener 的基本收发与 port=0 分配。
func TestNewListenerBasic(t *testing.T) {
	ln, err := NewListener("127.0.0.1:0", 128)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if addr == "" || addr == "127.0.0.1:0" {
		t.Fatalf("port=0 时应返回内核分配的实际地址，得到 %q", addr)
	}

	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	// 无 accept 方也无妨：仅验证连接建立成功（TCP 层）
}

// TestNewListenerRejectsBadAddr 验证地址校验。
func TestNewListenerRejectsBadAddr(t *testing.T) {
	for _, bad := range []string{"localhost:1", "127.0.0.1", "::1:1", "127.0.0.1:99999", "127.0.0.1:x", ""} {
		if _, err := NewListener(bad, 128); err == nil {
			t.Errorf("NewListener(%q) 应报错", bad)
		}
	}
}

// TestListenerConcurrentAccept backlog 生效冒烟：128 backlog 下
// 100 并发瞬时连接全部建立成功（M0 风暴尾延迟回归的对照）。
func TestListenerConcurrentAccept(t *testing.T) {
	ln, err := NewListener("127.0.0.1:0", 128)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(c, c) // echo
				c.Close()
			}(c)
		}
	}()

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
			if err != nil {
				t.Errorf("conn %d: %v", i, err)
				return
			}
			defer c.Close()
			msg := fmt.Sprintf("hello-%d", i)
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(c, msg); err != nil {
				t.Errorf("conn %d write: %v", i, err)
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(c, buf); err != nil {
				t.Errorf("conn %d read: %v", i, err)
				return
			}
			if string(buf) != msg {
				t.Errorf("conn %d echo = %q, want %q", i, buf, msg)
			}
		}(i)
	}
	wg.Wait()
}

// TestListenerReuseAddr TIME_WAIT 快速重绑（unix 语义；Windows 不设
// SO_REUSEADDR，跳过）。
func TestListenerReuseAddr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 不设 SO_REUSEADDR")
	}
	const port = 19876
	ln1, err := NewListener(fmt.Sprintf("127.0.0.1:%d", port), 16)
	if err != nil {
		t.Fatal(err)
	}
	// 建一条连接制造 TIME_WAIT 后关闭 listener
	c, err := net.DialTimeout("tcp", ln1.Addr().String(), time.Second)
	if err == nil {
		c.Close()
	}
	ln1.Close()
	ln2, err := NewListener(fmt.Sprintf("127.0.0.1:%d", port), 16)
	if err != nil {
		t.Fatalf("立即重绑同端口失败（SO_REUSEADDR 未生效?）: %v", err)
	}
	ln2.Close()
}

// --- tunnel ---

// startProxy 起一个 Server 并返回其地址与事件通道。
func startProxy(t *testing.T, sel UpstreamSelector) (addr string, events chan Event, stop func()) {
	t.Helper()
	ln, err := NewListener("127.0.0.1:0", 64)
	if err != nil {
		t.Fatal(err)
	}
	events = make(chan Event, 16)
	srv := &Server{Selector: sel, OnEvent: func(e Event) { events <- e }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ln)
	}()
	return ln.Addr().String(), events, func() {
		ln.Close()
		<-done
	}
}

// dialAndConnect 建立到代理的 CONNECT 隧道，返回裸连接（已读过 200）。
func dialAndConnect(t *testing.T, proxyAddr, authority string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连代理: %v", err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读隧道响应: %v", err)
	}
	if len(line) < 12 || line[:12] != "HTTP/1.1 200" {
		c.Close()
		t.Fatalf("隧道响应 = %q, want 200", line)
	}
	// 吃掉剩余头
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	// 注意：br 里可能缓冲了后续数据（本测试客户端不发，返回裸 conn）
	return c
}

// TestTunnelAccelerated 加速路径：selector 命中 → 假上游 → TLS+HTTP 全通。
func TestTunnelAccelerated(t *testing.T) {
	up, stopUp, err := startFakeTLS(t, "github.com")
	if err != nil {
		t.Fatal(err)
	}
	defer stopUp()

	sel := SelectorFunc(func(host string) (string, bool) {
		if host == "github.com" {
			return up.Addr, true
		}
		return "", false
	})
	proxyAddr, events, stop := startProxy(t, sel)
	defer stop()

	c := dialAndConnect(t, proxyAddr, "github.com:443")
	defer c.Close()
	// 在隧道上完成 TLS + HTTP
	tlsC := tlsClient(t, c, up, "github.com")
	defer tlsC.Close()
	fmt.Fprint(tlsC, "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(tlsC)
	if !contains(body, "200 OK") {
		t.Fatalf("上游响应异常: %q", body)
	}
	c.Close()

	select {
	case ev := <-events:
		if !ev.Accel || ev.Host != "github.com" || ev.Target != up.Addr {
			t.Errorf("事件字段异常: %+v", ev)
		}
		if ev.DialErr != nil {
			t.Errorf("事件应记录成功拨号: %+v", ev)
		}
		// DialMS 可为 0：windows 计时器精度 ~15ms，本地 dial 快于一个 tick
		if ev.Rx == 0 || ev.Tx == 0 {
			t.Errorf("事件应记录双向字节: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到连接事件")
	}
}

// TestTunnelPassthrough 放行路径：未命中规则 → 直连 authority 本身。
func TestTunnelPassthrough(t *testing.T) {
	up, stopUp, err := startFakeTLS(t, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer stopUp()

	sel := SelectorFunc(func(string) (string, bool) { return "", false })
	proxyAddr, events, stop := startProxy(t, sel)
	defer stop()

	c := dialAndConnect(t, proxyAddr, up.Addr) // authority = 假上游地址
	defer c.Close()
	tlsC := tlsClient(t, c, up, "example.test")
	defer tlsC.Close()
	fmt.Fprint(tlsC, "GET / HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(tlsC)
	if !contains(body, "200 OK") {
		t.Fatalf("上游响应异常: %q", body)
	}
	c.Close()

	select {
	case ev := <-events:
		if ev.Accel {
			t.Errorf("未命中规则的事件不应标记加速: %+v", ev)
		}
		if ev.Target != up.Addr {
			t.Errorf("放行应直连 authority: %q want %q", ev.Target, up.Addr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到连接事件")
	}
}

// TestTunnelRejectsNonConnect 非 CONNECT 请求返回 400。
func TestTunnelRejectsNonConnect(t *testing.T) {
	proxyAddr, _, stop := startProxy(t, nil)
	defer stop()
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !contains([]byte(line), "400") {
		t.Fatalf("应拒绝非 CONNECT: %q", line)
	}
}

// TestTunnelDialFailure 上游不可达：客户端 502 + 事件带 DialErr。
func TestTunnelDialFailure(t *testing.T) {
	sel := SelectorFunc(func(string) (string, bool) { return "127.0.0.1:1", true }) // 未监听端口
	proxyAddr, events, stop := startProxy(t, sel)
	defer stop()

	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !contains([]byte(line), "502") {
		t.Fatalf("上游失败应回 502: %q", line)
	}

	select {
	case ev := <-events:
		if ev.DialErr == nil {
			t.Errorf("事件应带 DialErr: %+v", ev)
		}
		if !ev.Accel {
			t.Errorf("命中规则的事件应标记加速: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到失败事件")
	}
}

// TestTunnelEarlyData 客户端不等 200 就发数据（激进 TLS 客户端行为）：
// 代理必须在回写 200 后把已缓冲的数据透传给上游。
func TestTunnelEarlyData(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(c, c)
				c.Close()
			}(c)
		}
	}()

	sel := SelectorFunc(func(string) (string, bool) { return echoLn.Addr().String(), true })
	proxyAddr, _, stop := startProxy(t, sel)
	defer stop()

	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	// CONNECT 与载荷一并发出——不等 200
	fmt.Fprintf(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\nEARLYDATA")
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !contains([]byte(line), "200") {
		t.Fatalf("隧道响应 = %q", line)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	echoed := make([]byte, len("EARLYDATA"))
	if _, err := io.ReadFull(br, echoed); err != nil {
		t.Fatalf("读回显: %v", err)
	}
	if string(echoed) != "EARLYDATA" {
		t.Fatalf("预发数据应被透传, got %q", echoed)
	}
}

// --- helpers ---

func startFakeTLS(t *testing.T, dnsName string) (*testutil.TLSServer, func(), error) {
	t.Helper()
	return testutil.StartTLSServer(dnsName, nil)
}

func tlsClient(t *testing.T, raw net.Conn, srv *testutil.TLSServer, serverName string) net.Conn {
	t.Helper()
	c := tls.Client(raw, srv.ClientTLSConfig(serverName))
	if err := c.Handshake(); err != nil {
		t.Fatalf("TLS 握手: %v", err)
	}
	return c
}

func contains(b []byte, sub string) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
