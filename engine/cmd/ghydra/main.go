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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xueweijian/ghydra/engine/bootstrap"
	"path/filepath"

	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
	"github.com/xueweijian/ghydra/engine/sched"
	"github.com/xueweijian/ghydra/engine/sni"
	"github.com/xueweijian/ghydra/engine/store"
	"github.com/xueweijian/ghydra/engine/sysproxy"
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
	case "doctor":
		doctorCmd(os.Args[2:])
	case "poc":
		pocCmd(os.Args[2:])
	case "loadtest":
		loadtestCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "on":
		onCmd(os.Args[2:])
	case "off":
		offCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
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
  ghydra on [--port N] [--mode pac|proxy]             接管系统代理 + 后台拉起 serve
  ghydra off                                          恢复系统代理 + 停止 serve
  ghydra serve [--listen ADDR] [--scheduler on|off]   前台运行（调试用）
  ghydra status                                      last_good 持久化观察口
  ghydra doctor [--mode direct|proxy|both]          六场景探针+直连对照+分类报告
  ghydra bench [--mode bootstrap|direct|proxy]     自举链或六域名存活报告
  ghydra poc [--listen ADDR] [--rewrite-sni N]      裸 SNI 转发器（调试工具）
  ghydra loadtest [--mode sni|connect] [--concurrency N] [--rounds M]  并发压测
`)
}

// statusCmd 打印 SQLite last_good（调度器池状态的持久化投影）。
// 实时池快照看 serve 日志（[sched] 前缀）；W4 doctor 会给出完整视图。
func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	_ = fs.Parse(args)

	ensureReconcile(*dbPath) // 任何命令入口都对账（崩溃残留恢复）

	if *dbPath == "" {
		fmt.Println("未配置数据库路径（HOME 不可用）")
		return
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()
	good, err := st.LastGood()
	if err != nil {
		log.Fatalf("读取失败: %v", err)
	}
	if len(good) == 0 {
		fmt.Println("暂无 last_good 记录（serve 运行并产生流量后生成）")
		return
	}
	fmt.Printf("%-40s %-22s %8s %9s  %s\n", "DOMAIN", "IP", "SCORE", "RTT_MS", "UPDATED")
	for host, e := range good {
		fmt.Printf("%-40s %-22s %8.3f %9.0f  %s\n",
			host, e.IP, e.Score, e.RTTMS, e.Updated.Format("01-02 15:04"))
	}
}

// serveCmd 启动 CONNECT 代理（M1 数据面）。
//
// W2 起 --scheduler=on（默认）：命中加速域名的连接由 IP 调度器择优
// （EWMA + 五态状态机 + 粘性 + 熔断），候选来自自举链 meta/DoH、
// SQLite last_good 恢复与池枯竭 DoH 补充；真实流量成败即探测信号。
// --scheduler=off 回退 W1 行为（直连域名，系统 DNS）——A/B 对照与
// 故障逃生通道。
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "本地监听地址（仅 IPv4 回环）")
	backlog := fs.Int("backlog", 4096, "listen backlog")
	timeout := fs.Duration("dial-timeout", proxy.DefaultDialTimeout, "上游连接超时")
	schedOn := fs.Bool("scheduler", true, "IP 调度器（off = W1 直连行为，对照/逃生）")
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径（空 = 不持久化）")
	doctorInterval := fs.Duration("doctor-interval", 0, "自动 doctor 周期（0 = 关闭；ghydra on 默认 1h）")
	doctorRepo := fs.String("doctor-repo", probe.DefaultConfig().Repo, "自动 doctor 使用的仓库 owner/name")
	managed := fs.Bool("managed", false, "由 ghydra on 拉起（退出时恢复系统代理）")
	_ = fs.Parse(args)

	// on 已在拉起前完成残留对账并写入快照；managed 子进程启动
	// 期间快照存在但 serve.json 还没落盘，不能把当前接管误判成
	// 上一代 kill -9 残留（W3 启动竞态）。
	if !*managed {
		ensureReconcile(*dbPath)
	}

	m := rules.New(rules.DefaultDomains)

	var sel proxy.UpstreamSelector = proxy.SelectorFunc(func(host string) (string, bool) {
		if m.Match(host) {
			return net.JoinHostPort(host, "443"), true
		}
		return "", false
	})

	var (
		reportEvent func(proxy.Event)
		sc          *sched.Scheduler
	)
	if *schedOn {
		sel2, shutdown, err := startScheduler(m, *dbPath)
		if err != nil {
			log.Fatalf("调度器启动失败: %v", err)
		}
		sel, reportEvent, sc = sel2, sel2.ReportEvent, sel2.Sched
		defer shutdown()
	}

	var conns atomic.Int64

	srv := &proxy.Server{
		Selector:    sel,
		DialTimeout: *timeout,
		OnEvent: func(e proxy.Event) {
			n := conns.Add(1)
			status := "OK"
			if e.DialErr != nil {
				status = e.DialErr.Error()
			} else if e.CopyErr != "" {
				status = "copy:" + e.CopyErr
			}
			log.Printf("[conn#%d] %s -> %s accel=%t dial=%.1fms rx=%dB tx=%dB %s",
				n, e.Host, e.Target, e.Accel, e.DialMS, e.Rx, e.Tx, status)
			if reportEvent != nil {
				reportEvent(e) // 调度器信号（非阻塞；池外目标自动忽略）
			}
		},
	}

	// 同端口托管 PAC / status（R5：主端口 http 分流，AutoConfigURL 直指）
	actualAddr := *listen
	mux := http.NewServeMux()
	mux.HandleFunc("/pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		fmt.Fprint(w, m.PAC(fmt.Sprintf("127.0.0.1:%d", portOf(actualAddr))))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"listen":    actualAddr,
			"scheduler": *schedOn,
			"conns":     conns.Load(),
			"uptime_s":  time.Since(startTime).Seconds(),
		}
		if sc != nil {
			pools := map[string]any{}
			for _, host := range sc.Hosts() {
				pools[host] = map[string]any{
					"sticky": sc.StickyIP(host),
					"ips":    sc.Snapshot(host),
				}
			}
			out["pools"] = pools
		}
		writeJSON(w, out)
	})
	srv.HTTPHandler = mux

	// 端口冲突迁移（W3：9801 被占 → +1..+8）
	var ln net.Listener
	var err error
	for i := 0; i < 9; i++ {
		ln, err = proxy.NewListener(actualAddr, *backlog)
		if err == nil {
			break
		}
		if p := portOf(actualAddr); p > 0 {
			actualAddr = fmt.Sprintf("127.0.0.1:%d", p+1)
		} else {
			break
		}
	}
	if err != nil {
		log.Fatalf("监听失败（含迁移尝试）: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("收到退出信号，正在关闭…")
		if *managed {
			restoreSnapshot(*dbPath) // on 拉起的 serve：退出时恢复系统代理
		}
		ln.Close()
	}()

	var stopDoctor func()
	if *doctorInterval > 0 && *dbPath != "" {
		stopDoctor = startDoctorLoop(*doctorInterval, *dbPath, *doctorRepo, "http://"+ln.Addr().String())
		defer stopDoctor()
	}

	log.Printf("ghydra serve 已启动: %s | 加速域名 %d 条 | scheduler=%t | backlog %d",
		ln.Addr().String(), len(m.Domains()), *schedOn, *backlog)
	log.Printf("PAC: http://%s/pac | 系统代理指向 %s（或 ghydra on 一键接管）",
		ln.Addr().String(), ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("serve 退出: %v", err)
	}
	if *managed {
		restoreSnapshot(*dbPath) // 正常退出路径也恢复
	}
	log.Printf("已退出，共服务 %d 条连接", conns.Load())
}

var startTime = time.Now()

// restoreSnapshot 恢复接管前的系统代理（managed serve 退出 hook）。
func restoreSnapshot(dbPath string) {
	if dbPath == "" {
		return
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer st.Close()
	ps, pac, ok, err := st.LoadSnapshot()
	if err != nil || !ok {
		return
	}
	if err := sysproxy.Apply(sysproxy.Setting{ProxyServer: ps, PACURL: pac}); err != nil {
		log.Printf("[restore] 恢复系统代理失败: %v（原值 server=%q pac=%q）", err, ps, pac)
		return
	}
	st.DeleteSnapshot()
	removeDaemonState()
	log.Printf("[restore] 系统代理已恢复")
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(p)
	return n
}

// startScheduler 装配 M1-W2 调度器：候选喂数（自举链 + last_good
// 恢复）→ 主动探测（拨号函数）→ 池枯竭 DoH 兜底 → 后台刷新 →
// 事件管道。返回 shutdown 必须在退出时调用（flush 持久化）。
func startScheduler(m *rules.Matcher, dbPath string) (*sched.Selector, func(), error) {
	cfg := sched.DefaultConfig()
	sc := sched.New(cfg)
	sc.Logf = func(f string, a ...any) { log.Printf(f, a...) }

	// 持久化（可选：dbPath 空 = 纯内存）
	var st *store.Store
	if dbPath != "" {
		s, err := store.Open(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite: %w", err)
		}
		st = s
		sc.OnActive = func(host, addr string, score, rtt float64) {
			st.UpdateLastGood(host, addr, score, rtt) // 非阻塞
		}
	}

	// 主动探测拨号：保留 TCP-only 作为通用回退；HTTPS 候选的
	// Preflight/refresh 使用下面的 DialHost 做带 SNI 的 TLS 握手，
	// 避免 TCP 通但 ClientHello 后沉默的死 IP 被放入 Active。
	sc.Dial = func(addr string, timeout time.Duration) error {
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	}
	sc.DialHost = func(host, addr string, timeout time.Duration) error {
		var d net.Dialer
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		t := tls.Client(c, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host})
		if err := t.Handshake(); err != nil {
			return err
		}
		return nil
	}

	// 池枯竭兜底：DoH 解析该域名（bootstrap 的 per-host 能力）
	// 调度器池内地址统一为 host:443，避免裸 IP 在代理数据面报
	// "missing port in address"。

	resolver := bootstrap.New()
	sc.Resolve = func(host string) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		ips, err := resolver.ResolveHost(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.JoinHostPort(ip, "443"))
		}
		return out, nil
	}

	// ① 候选喂数：DoH 解析（就近可达 IP，实测质量最高）+ 自举链
	// （meta 网段 + last-good 缓存）。meta 老段在本网络可能整段不可达
	// ——Preflight 预筛保证用户连接不背死 IP 的 dial 成本。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t0 := time.Now()

		// 1a. DoH 解析 github.com（223 系就近结果，立即可用）
		var dohIPs []string
		if ips, err := resolver.ResolveHost(ctx, "github.com"); err == nil {
			for _, ip := range ips {
				dohIPs = append(dohIPs, net.JoinHostPort(ip, "443"))
			}
		}
		// 1b. 自举链（meta 网段 + 缓存 + 种子）
		res := resolver.Resolve(ctx)
		metaIPs := make([]string, 0, len(res.IPs))
		for _, ip := range res.IPs {
			metaIPs = append(metaIPs, net.JoinHostPort(ip, "443"))
		}
		if n := sc.AddCandidates("github.com", append(dohIPs, metaIPs...)); n > 0 {
			log.Printf("[sched] 候选入池 github.com: +%d（DoH %d + %s %d，%.1fs）",
				n, len(dohIPs), res.Source, len(metaIPs), time.Since(t0).Seconds())
		}
		// 1c. 并行预筛：死 IP 直接熔断，不进用户连接路径
		if n := sc.Preflight("github.com"); n > 0 {
			log.Printf("[sched] Preflight 预筛 github.com: %d 候选完成", n)
		}
	}()

	// ② last_good 恢复（经主动验证后放行）
	if st != nil {
		if good, err := st.LastGood(); err == nil {
			for host, e := range good {
				if !m.Match(host) {
					continue // 规则外域名不恢复（防规则变更残留）
				}
				n := sc.AddCandidates(host, []string{e.IP})
				if n > 0 {
					go sc.ProbeBest(host, 1)
					log.Printf("[sched] last_good 恢复 %s -> %s (score=%.3f rtt=%.0fms)",
						host, e.IP, e.Score, e.RTTMS)
				}
			}
		}
	}

	// ③ 后台刷新（活跃域名 Active top5，30s 一轮）
	stop := make(chan struct{})
	sc.StartRefreshLoop(30*time.Second, 5, stop)

	shutdown := func() {
		close(stop)
		if st != nil {
			st.Close() // flush 在途写
		}
	}
	return sched.NewSelector(sc, m), shutdown, nil
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra", "ghydra.db")
}

func benchCmd(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "输出 JSON（真机验收报告格式）")
	mode := fs.String("mode", "bootstrap", "模式: bootstrap|direct|proxy")
	proxyAddr := fs.String("proxy", "http://127.0.0.1:9801", "proxy 模式的 HTTP CONNECT 地址")
	repo := fs.String("repo", "xueweijian/ghydra", "六域名探针使用的仓库 owner/name")
	timeout := fs.Duration("timeout", 30*time.Second, "总超时")
	_ = fs.Parse(args)

	if *mode == "direct" || *mode == "proxy" {
		cfg := probe.DefaultConfig()
		cfg.Timeout, cfg.ProxyURL, cfg.Repo = *timeout, *proxyAddr, *repo
		runner := probe.New(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), *timeout+3*time.Second)
		defer cancel()
		pmode := probe.ModeDirect
		if *mode == "proxy" {
			pmode = probe.ModeProxy
		}
		checks := runner.RunGitHubDomains(ctx, pmode)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"mode": pmode, "checks": checks})
			return
		}
		passed := 0
		for _, c := range checks {
			if c.OK {
				passed++
			}
			fmt.Printf("%-10s %-4s %-15s status=%d ttfb=%.1fms class=%s %s\n", c.Name, map[bool]string{true: "OK", false: "FAIL"}[c.OK], c.Mode, c.Status, c.TTFBMS, c.Class, c.Error)
		}
		fmt.Printf("bench %s: %d/%d GitHub 域名可用\n", pmode, passed, len(checks))
		return
	}
	if *mode != "bootstrap" {
		log.Fatalf("无效 --mode %q（bootstrap|direct|proxy）", *mode)
	}

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
