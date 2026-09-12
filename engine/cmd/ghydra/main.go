// ghydra CLI — M0 PoC：SNI 转发器 + 自举链基准 + 并发压测。
//
// 用法:
//
//	ghydra bench [--json]                    四级自举链探测，输出轨迹报告
//	ghydra poc [--listen L] [--rewrite-sni N] [--upstream HOST:PORT]  本地 SNI 转发器
//	ghydra loadtest [--concurrency N] [--rounds M]  千并发全链路压测
//
// M0 验证目标见 docs/GHydra-PRD.md §8 M0。M1 起将替换为 cobra 子命令结构。
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xueweijian/ghydra/engine/bootstrap"
	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
	"github.com/xueweijian/ghydra/engine/sni"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bench":
		benchCmd(os.Args[2:])
	case "poc":
		pocCmd(os.Args[2:])
	case "loadtest":
		loadtestCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ghydra (M1 dev)

用法:
  ghydra serve [--listen ADDR] [--backlog N]     CONNECT 代理服务（通道 A 数据面）
  ghydra bench [--json]                          四级自举链探测报告
  ghydra poc [--listen ADDR] [--rewrite-sni N]   裸 SNI 转发器（调试工具）
  ghydra loadtest [--concurrency N] [--rounds M] 并发压测（内置假上游 + 转发器 + 客户端）
`)
}

// serveCmd 启动 CONNECT 代理（M1 W1 内核）。
//
// W1 阶段：命中加速域名的连接直连域名本身（系统 DNS）——内核与规则
// 已就位，加速效果待 W2 IP 调度器接入 Select 后生效。
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "本地监听地址（仅 IPv4 回环）")
	backlog := fs.Int("backlog", 4096, "listen backlog")
	timeout := fs.Duration("dial-timeout", proxy.DefaultDialTimeout, "上游连接超时")
	_ = fs.Parse(args)

	m := rules.New(rules.DefaultDomains)
	var conns atomic.Int64
	srv := &proxy.Server{
		Selector: proxy.SelectorFunc(func(host string) (string, bool) {
			if m.Match(host) {
				// W1：暂无调度器，直连域名本身；W2 起返回择优 IP:443
				return net.JoinHostPort(host, "443"), true
			}
			return "", false
		}),
		DialTimeout: *timeout,
		OnEvent: func(e proxy.Event) {
			n := conns.Add(1)
			status := "OK"
			if e.DialErr != nil {
				status = e.DialErr.Error()
			}
			log.Printf("[conn#%d] %s -> %s accel=%t dial=%.1fms rx=%dB tx=%dB %s",
				n, e.Host, e.Target, e.Accel, e.DialMS, e.Rx, e.Tx, status)
		},
	}

	ln, err := proxy.NewListener(*listen, *backlog)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("收到退出信号，正在关闭…")
		ln.Close()
	}()

	log.Printf("ghydra serve 已启动: %s | 加速域名 %d 条 | backlog %d | dial-timeout %s",
		ln.Addr().String(), len(m.Domains()), *backlog, *timeout)
	log.Printf("将系统代理指向 %s 即可使用（一键接管 = W3 ghydra on）", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("serve 退出: %v", err)
	}
	log.Printf("已退出，共服务 %d 条连接", conns.Load())
}

func benchCmd(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "输出 JSON（真机验收报告格式）")
	timeout := fs.Duration("timeout", 30*time.Second, "总超时")
	_ = fs.Parse(args)

	r := bootstrap.New()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res := r.Resolve(ctx)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			log.Fatal(err)
		}
		return
	}
	fmt.Printf("四级自举链报告  总耗时 %.1fms\n", res.ElapsedMS)
	fmt.Printf("来源: %s  候选 IP: %d 个\n", res.Source, len(res.IPs))
	for _, s := range res.Steps {
		status := "FAIL"
		if s.OK {
			status = "OK"
		}
		fmt.Printf("  L%d %-45s %-4s %6.1fms  err=%s\n", s.Level, s.Name, status, s.ElapsedMS, s.Err)
	}
	for _, ip := range res.IPs {
		fmt.Printf("    %s\n", ip)
	}
}

func pocCmd(args []string) {
	fs := flag.NewFlagSet("poc", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "本地监听地址")
	rewrite := fs.String("rewrite-sni", "", "实验工具：改写 ClientHello 的 SNI（TLS1.2/1.3 下会 bad record mac，仅协议研究用）")
	upstream := fs.String("upstream", "", "可选：覆盖上游地址（HOST:PORT），默认按 SNI 域名拨 443")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second, "上游连接超时")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	log.Printf("GHydra PoC 转发器已启动: %s (rewrite-sni=%q upstream=%q)", *listen, *rewrite, *upstream)
	forwardServer(ln, *rewrite, *upstream, *dialTimeout)
}

// forwardServer 是 poc 与 loadtest 共用的转发 accept 循环。
func forwardServer(ln net.Listener, rewrite, upstream string, dialTimeout time.Duration) {
	var conns int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		id := atomic.AddInt64(&conns, 1)
		go func(c net.Conn) {
			defer c.Close()
			relay(c, rewrite, upstream, dialTimeout, id)
		}(conn)
	}
}

func relay(conn net.Conn, rewrite, upstream string, dialTimeout time.Duration, id int64) {
	start := time.Now()
	br := bufio.NewReader(conn)
	ch, err := sni.ReadClientHello(br)
	if err != nil {
		log.Printf("#%d ClientHello 解析失败: %v", id, err)
		return
	}
	host := ch.ServerName
	out := ch.Record
	if rewrite != "" {
		out, err = ch.RewriteSNI(rewrite)
		if err != nil {
			log.Printf("#%d SNI 改写失败: %v", id, err)
			return
		}
		host = rewrite
	}
	if host == "" {
		log.Printf("#%d 无 SNI，拒绝转发", id)
		return
	}
	addr := net.JoinHostPort(host, "443")
	if upstream != "" {
		addr = upstream
	}
	upstreamConn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		log.Printf("#%d %s 上游连接失败: %v", id, addr, err)
		return
	}
	defer upstreamConn.Close()
	if _, err := upstreamConn.Write(out); err != nil {
		log.Printf("#%d %s 写入上游失败: %v", id, addr, err)
		return
	}
	log.Printf("#%d SNI=%s -> %s (改写=%t)", id, ch.ServerName, upstreamConn.RemoteAddr(), rewrite != "")
	go func() {
		io.Copy(upstreamConn, br)
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			upstreamConn.Close()
		}
	}()
	n, _ := io.Copy(conn, upstreamConn)
	log.Printf("#%d 完成 %s 下行 %dB 耗时 %s", id, ch.ServerName, n, time.Since(start).Round(time.Millisecond))
}

// loadtestCmd 端到端并发压测：内置假上游 TLS 服务 + 转发器 + N×M 客户端。
// 验收口径（PRD 非功能需求）：1000 并发连接、成功率 100%、无 goroutine 泄漏。
func loadtestCmd(args []string) {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	concurrency := fs.Int("concurrency", 1000, "并发 worker 数")
	rounds := fs.Int("rounds", 3, "每 worker 建拆连接轮数")
	deadline := fs.Duration("timeout", 15*time.Second, "单连接超时")
	mode := fs.String("mode", "sni", "压测模式: sni(M0 裸SNI转发) | connect(M1 CONNECT内核+自建backlog)")
	backlog := fs.Int("backlog", 4096, "connect 模式 listen backlog")
	_ = fs.Parse(args)

	// ① 假上游（自签 github.com 证书的 HTTPS 服务）
	cert := genSelfSignedCert("github.com")
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	upSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "OK")
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	defer upSrv.Close()
	go upSrv.ServeTLS(upLn, "", "")

	// ② 中继：sni = M0 裸转发路径；connect = M1 CONNECT 内核（自建 socket backlog）
	var fwdAddr string
	switch *mode {
	case "connect":
		sel := proxy.SelectorFunc(func(string) (string, bool) { return upLn.Addr().String(), true })
		srv := &proxy.Server{
			Selector:    sel,
			DialTimeout: 5 * time.Second,
			OnEvent:     func(proxy.Event) {},
		}
		fwdLn, err := proxy.NewListener("127.0.0.1:0", *backlog)
		if err != nil {
			log.Fatal(err)
		}
		fwdAddr = fwdLn.Addr().String()
		go srv.Serve(fwdLn)
	case "sni":
		fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		fwdAddr = fwdLn.Addr().String()
		go forwardServer(fwdLn, "", upLn.Addr().String(), 5*time.Second)
	default:
		log.Fatalf("未知模式 %q（sni|connect）", *mode)
	}
	time.Sleep(200 * time.Millisecond)

	// ③ 客户端压测
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientCfg := &tls.Config{ServerName: "github.com", RootCAs: pool}

	baseGoroutines := runtime.NumGoroutine()
	var peakGoroutines int64
	var okCount, failCount int64
	errKinds := sync.Map{}
	latencies := make([]time.Duration, 0, *concurrency**rounds)
	var mu sync.Mutex

	log.SetOutput(io.Discard) // 压测期间静默 relay 日志
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < *rounds; i++ {
				if n := runtime.NumGoroutine(); int64(n) > atomic.LoadInt64(&peakGoroutines) {
					atomic.StoreInt64(&peakGoroutines, int64(n))
				}
				t0 := time.Now()
				var err error
				if *mode == "connect" {
					err = connectRound(fwdAddr, clientCfg, *deadline)
				} else {
					err = oneRound(fwdAddr, clientCfg, *deadline)
				}
				lat := time.Since(t0)
				mu.Lock()
				latencies = append(latencies, lat)
				mu.Unlock()
				if err != nil {
					atomic.AddInt64(&failCount, 1)
					kind := errKind(err)
					errKinds.Store(kind, true)
					continue
				}
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	log.SetOutput(os.Stderr)

	// ④ 泄漏检测：连接全部关闭后 goroutine 应回落
	time.Sleep(2 * time.Second)
	finalGoroutines := runtime.NumGoroutine()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}
	total := *concurrency * *rounds
	var kinds []string
	errKinds.Range(func(k, _ any) bool {
		kinds = append(kinds, k.(string))
		return true
	})

	fmt.Printf("千并发压测报告\n")
	fmt.Printf("  规模: %d 并发 × %d 轮 = %d 连接（建拆） 耗时 %s\n", *concurrency, *rounds, total, elapsed.Round(time.Millisecond))
	fmt.Printf("  成功率: %d/%d (%.2f%%)\n", okCount, total, float64(okCount)/float64(total)*100)
	if len(latencies) > 0 {
		fmt.Printf("  延迟: p50 %s  p95 %s  p99 %s  max %s\n", pct(0.50).Round(time.Microsecond), pct(0.95).Round(time.Microsecond), pct(0.99).Round(time.Microsecond), latencies[len(latencies)-1].Round(time.Microsecond))
	}
	fmt.Printf("  吞吐: %.0f conn/s\n", float64(total)/elapsed.Seconds())
	if failCount > 0 {
		fmt.Printf("  失败: %d  种类: %v\n", failCount, kinds)
	}
	fmt.Printf("  goroutine: 基线 %d → 峰值 %d → 回落 %d %s\n", baseGoroutines, peakGoroutines, finalGoroutines, leakVerdict(baseGoroutines, finalGoroutines))
	fmt.Printf("  内存: HeapAlloc %.1fMB / HeapSys %.1fMB\n", float64(ms.HeapAlloc)/1e6, float64(ms.HeapSys)/1e6)
}

// connectRound 完成一次 CONNECT 隧道全流程（M1 内核路径）。
func connectRound(addr string, cfg *tls.Config, timeout time.Duration) error {
	raw, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(timeout))
	fmt.Fprint(raw, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		return fmt.Errorf("tunnel: %q", strings.TrimSpace(line))
	}
	c := tls.Client(raw, cfg)
	defer c.Close()
	if err := c.Handshake(); err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n")
	body, err := io.ReadAll(c)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(body) == 0 || !containsStatus200(body) {
		return fmt.Errorf("bad response (%d bytes)", len(body))
	}
	return nil
}

// oneRound 完成一次：建连 → TLS 握手 → 请求 → 读完整响应 → 关闭。
func oneRound(addr string, cfg *tls.Config, timeout time.Duration) error {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	req := "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	body, err := io.ReadAll(c)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(body) == 0 || !containsStatus200(body) {
		return fmt.Errorf("bad response (%d bytes)", len(body))
	}
	return nil
}

func containsStatus200(head []byte) bool {
	return len(head) >= 12 && string(head[:12]) == "HTTP/1.1 200"
}

func errKind(err error) string {
	s := err.Error()
	for _, prefix := range []string{"dial/tls", "write", "read"} {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return prefix
		}
	}
	return "other"
}

func leakVerdict(base, final int) string {
	if final <= base+8 {
		return "✓ 无泄漏"
	}
	return "✗ 疑似泄漏"
}

func genSelfSignedCert(dnsName string) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	c := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	c.Leaf, _ = x509.ParseCertificate(der)
	return c
}
