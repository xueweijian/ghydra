// get.go —— ghydra get 下载器 CLI（M2-W2，方案 D4）。
//
// 用法：ghydra get <url> [-o 输出文件] [--cdn 前缀] [--db 路径] [--json]
// 择路：A（IP 择优直连）起步，TTFB/前 1MB 吞吐判慢 → B（CDN 前缀）
// Range 续传；B 失败/缓存错配 → 回 A。报告进 doctor_log。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xueweijian/ghydra/engine/bootstrap"
	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/rules"

	"github.com/xueweijian/ghydra/engine/store"
)

// newBFetcherProd 生产 B 通道接线：DoH 自举解析（W3，消除系统 DNS
// 单点——真网实证 resolv.conf 被魔法写坏后 B 通道死），系统 DNS 兜底。
func newBFetcherProd(prefix string) *get.BFetcher {
	b := get.NewBFetcher(prefix)
	if b != nil {
		res := bootstrap.New()
		b.DNS = &get.BDNS{ResolveHost: res.ResolveHost}
	}
	return b
}

// reorderGetArgs 允许 flag 出现在 URL 前后（get URL -o x ≡ get -o x URL）。
// Go flag 包遇到首个非 flag 参数即停止解析，这里按参数类型重排后再解析。
func reorderGetArgs(args []string) []string {
	boolFlags := map[string]bool{"-json": true, "--json": true, "-stats": true, "--stats": true}
	var flags, pos []string
	expectVal := ""
	for _, a := range args {
		if expectVal != "" { // 上一个 flag 的取值
			flags = append(flags, a)
			expectVal = ""
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && !boolFlags[a] {
				expectVal = a // 字符串型 flag：下一参是其值
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func getCmd(args []string) {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	out := fs.String("o", "", "输出文件（默认 URL 末段）")
	cdn := fs.String("cdn", "https://gh-proxy.com/", "B 通道 CDN 前缀（空 = 禁用切道）")
	dbPath := fs.String("db", defaultDBPath(), "调度持久化与指标库（空 = 不落盘）")
	asJSON := fs.Bool("json", false, "输出 JSON 报告")
	stats := fs.Bool("stats", false, "打印历史下载速率统计后退出")
	if err := fs.Parse(reorderGetArgs(args)); err != nil {
		os.Exit(2)
	}
	// --stats：只查库，无需 URL
	if *stats {
		printDownloadStats(*dbPath)
		return
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: ghydra get <url> [-o 文件] [--cdn 前缀] [--json] [--stats]")
		os.Exit(2)
	}
	rawURL := fs.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// A 通道：调度器择优（失败降级系统直连——下载器依然可用）
	var pick func(host string) (string, bool)
	m := rules.New(rules.DefaultDomains)
	if sel, stop, err := startScheduler(m, *dbPath); err == nil {
		defer stop()
		pick = func(host string) (string, bool) { return sel.Sched.Pick(host) }
	} else {
		log.Printf("调度器未启用（%v），A 通道走系统直连", err)
	}
	a := get.NewAFetcher(pick)

	// B 通道：CDN 前缀反代（--cdn 空 = A-only）
	var b get.Fetcher
	if *cdn != "" {
		b = newBFetcherProd(*cdn)
	}

	dst := *out
	if dst == "" {
		dst = filepath.Base(rawURL)
		if dst == "" || dst == "/" || dst == "." {
			fmt.Fprintln(os.Stderr, "无法从 URL 推断文件名，请用 -o 指定")
			os.Exit(2)
		}
	}
	if _, err := os.Stat(dst); err == nil {
		fmt.Fprintf(os.Stderr, "输出文件已存在：%s（暂不支持断点续传已有文件）\n", dst)
		os.Exit(2)
	}

	d := get.New(get.DefaultConfig(), a, b)
	start := time.Now()
	res, err := d.Get(ctx, rawURL, dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
	}
	dur := time.Since(start)

	// 指标落库（尽力而为，失败不影响下载）
	runID := fmt.Sprintf("get-%d", start.UnixMilli())
	if *dbPath != "" {
		if st, err := store.Open(*dbPath); err == nil {
			st.AppendDownload(runID, res)
			st.Close()
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
	} else {
		printGetReport(res, dst, dur)
	}
	if err != nil {
		os.Exit(1)
	}
}

func printGetReport(res get.Result, dst string, wall time.Duration) {
	fmt.Printf("目标: %s\n", res.URL)
	fmt.Printf("输出: %s (%.2f MB)\n", dst, float64(res.GotBytes)/(1<<20))
	if res.TotalBytes > 0 {
		fmt.Printf("声明总量: %s\n", humanBytes(res.TotalBytes))
	} else {
		fmt.Println("声明总量: 未知")
	}
	if res.ETag != "" {
		fmt.Printf("ETag: %s\n", res.ETag)
	}
	fmt.Println("通道轨迹:")
	for i, seg := range res.Segments {
		fmt.Printf("  %d. [%s] 从 %s 起 %s（TTFB %.0fms, %.2f MB/s）",
			i+1, seg.Channel, humanOff(seg.StartOff), humanBytes(seg.Bytes), seg.TTFBMS, seg.RateBPS/(1<<20))
		if seg.WhyOut != "" {
			fmt.Printf(" ← %s", seg.WhyOut)
		}
		fmt.Println()
	}
	fmt.Printf("最终通道: %s | 端到端 %.2f MB/s | 耗时 %.1fs\n",
		res.FinalCh, res.RateBPS/(1<<20), wall.Seconds())
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.2fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func humanOff(o int64) string {
	if o == 0 {
		return "0"
	}
	return humanBytes(o)
}

func printDownloadStats(dbPath string) {
	if dbPath == "" {
		fmt.Fprintln(os.Stderr, "--db 为空，无法查询")
		os.Exit(2)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()
	stats, err := st.DownloadStats(time.Now().AddDate(0, 0, -7))
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
		os.Exit(1)
	}
	if len(stats) == 0 {
		fmt.Println("近 7 天无下载记录（ghydra get 会自动记录）")
		return
	}
	fmt.Println("近 7 天下载速率（Release 验收口径：中位 ≥ 2MB/s）:")
	for _, s := range stats {
		fmt.Printf("  通道 %s: %d 段 | 中位 %.2f MB/s | 峰值 %.2f MB/s | 累计 %.1f MB | 失败 %d/%d 段\n",
			s.Channel, s.Runs, s.MedianBPS/(1<<20), s.MaxBPS/(1<<20), s.TotalMB, s.Failures, s.Attempts)
	}
}

// servePlainHTTP 明文代理路径的 A/B 改写（W2 交付：http:// 请求
// URL 对代理可见，直接按通道决策改写转发）。
//
// 路由：channel.Router（D1/D3）——命中加速域名时，Closed→A 转发，
// Open→B CDN 改写；结果回灌 Router 流量窗口。
func servePlainHTTP(router *channel.Router, m *rules.Matcher, pick func(string) (string, bool), cdnFn func() string) http.Handler {
	a := get.NewAFetcher(pick)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 绝对形态（http://host/path）才是代理请求；其余走本地端点
		if r.URL.Host == "" {
			http.NotFound(w, r)
			return
		}
		host := r.URL.Hostname()
		if !m.Match(host) {
			// 非加速域名：普通正向代理转发（系统网络）
			proxyPassthrough(w, r)
			return
		}
		route := router.Route(channel.Flow{Kind: channel.KindPlainHTTP, Host: host})
		ch := route.Channel
		var b get.Fetcher // B 运行时构造（cdnFn 支持热更；空 = 回落 A）
		if ch == channel.CDN {
			if p := cdnFn(); p != "" {
				b = newBFetcherProd(p)
			} else {
				ch = channel.Direct
			}
		}

		target := *r.URL
		var fetch get.Fetcher
		var fetchURL string
		if ch == channel.CDN {
			fetch = b
			fetchURL = target.String() // BFetcher 自行拼接前缀
		} else {
			fetch = a
			fetchURL = target.String()
		}
		resp, err := fetch.Get(r.Context(), fetchURL, -1)
		if err != nil {
			http.Error(w, "ghydra 上游失败: "+err.Error(), http.StatusBadGateway)
			router.Report(channel.Flow{Kind: channel.KindPlainHTTP, Host: host}, ch, false)
			return
		}
		defer resp.Body.Close()
		router.Report(channel.Flow{Kind: channel.KindPlainHTTP, Host: host}, ch, resp.StatusCode < 400)
		// 透传响应
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flushCopy(w, resp.Body)
	})
}

func flushCopy(w http.ResponseWriter, body io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func proxyPassthrough(w http.ResponseWriter, r *http.Request) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}
