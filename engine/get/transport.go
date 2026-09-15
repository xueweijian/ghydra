// transport.go —— A/B 两条下载路径的 Fetcher 实现。
package get

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AFetcher 通道 A：IP 择优直连。
//
// PickAddr 由调度器提供（sched.Pick → "ip:443"）；命中则拨 IP、
// TLS SNI 保持原域名（M0 验证的裸转发存活路径），证书按域名校验。
// 未命中/调度器未启 → 系统解析直连（兜底）。
type AFetcher struct {
	PickAddr    func(host string) (addr string, ok bool) // 可为 nil
	DialTimeout time.Duration
	// InsecureTLS 跳过证书校验（仅测试/自签内网场景；生产禁用）
	InsecureTLS bool
	// OnAddr 诊断回调（每次拨号的实际目标；测试/doctor 用）
	OnAddr func(host, addr string, picked bool)
	// OnDialResult 拨号/TLS 结果回报（F3：下载路径失败回灌调度池——
	// 拨号超时与 SNI 阻断 RST 都让死 IP 出局）。仅 picked 候选触发。
	OnDialResult func(host, addr string, dialMS float64, ok bool)
}

// NewAFetcher 构造（零值超时用默认）。
func NewAFetcher(pick func(host string) (string, bool)) *AFetcher {
	return &AFetcher{PickAddr: pick, DialTimeout: 10 * time.Second}
}

// Get 实现 Fetcher。
func (a *AFetcher) Get(ctx context.Context, rawURL string, from int64) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}

	tr := &http.Transport{
		// 禁连接池复用：每次 Get 都走调度器择优（IP 可能变化），
		// 且 Range 续传响应各异
		DisableKeepAlives: true,
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			// addr 是 http.Transport 想拨的 host:port；换调度器择优 IP
			target := net.JoinHostPort(host, port)
			picked := false
			if a.PickAddr != nil {
				if ipAddr, ok := a.PickAddr(host); ok && ipAddr != "" {
					target = ipAddr
					picked = true
				}
			}
			if a.OnAddr != nil {
				a.OnAddr(host, target, picked)
			}
			report := func(ms float64, ok bool) {
				if a.OnDialResult != nil && picked {
					a.OnDialResult(host, target, ms, ok)
				}
			}
			t0 := time.Now()
			d := &net.Dialer{Timeout: a.dialTimeout()}
			c, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				report(sinceMS(t0), false)
				return nil, err
			}
			if port == "80" || u.Scheme == "http" {
				report(sinceMS(t0), true)
				return c, nil // 明文：不做 TLS
			}
			tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: a.InsecureTLS})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				report(sinceMS(t0), false) // TCP 通但 TLS 死（SNI 阻断）：同罪
				return nil, err
			}
			report(sinceMS(t0), true)
			return tc, nil
		},
	}
	cl := &http.Client{Transport: tr}
	return doRanged(ctx, cl, rawURL, from)
}

func (a *AFetcher) dialTimeout() time.Duration {
	if a.DialTimeout <= 0 {
		return 10 * time.Second
	}
	return a.DialTimeout
}

func sinceMS(t0 time.Time) float64 {
	return float64(time.Since(t0).Microseconds()) / 1000
}

// BFetcher 通道 B：CDN 前缀反代（gh-proxy 协议）。
//
// 目标 URL 原样拼接在 Prefix 后：Prefix="https://gh-proxy.com/" +
// "https://github.com/o/r/releases/download/v1/x.zip"。
//
// 解析策略（W3）：DNS（*BDNS，DoH 主力）优先——系统 DNS 是单点故障
// （真网实证：resolv.conf 被写成 1.1.1.1 后 B 通道直接死）；
// DNS 为 nil 时纯系统解析（兼容旧行为，测试兜底）。
type BFetcher struct {
	Prefix string // 必须以 / 或 # 结尾（gh-proxy 兼容两种分隔）
	Client *http.Client
	DNS    *BDNS
	// InsecureTLS 跳过证书校验（仅测试/自签内网场景；生产禁用）
	InsecureTLS bool
	// OnAddr 诊断回调（每次拨号的实际目标与是否 DoH 路径）
	OnAddr func(host, addr string, doh bool)
}

// NewBFetcher 构造。prefix 空返回 nil（A-only 模式由调用方处理）。
func NewBFetcher(prefix string) *BFetcher {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") && !strings.HasSuffix(prefix, "#") {
		prefix += "/"
	}
	return &BFetcher{Prefix: prefix}
}

// Get 实现 Fetcher。
func (b *BFetcher) Get(ctx context.Context, rawURL string, from int64) (*http.Response, error) {
	cl := b.Client
	if cl == nil {
		if b.DNS != nil {
			d := &bDialer{DNS: b.DNS, InsecureTLS: b.InsecureTLS, OnAddr: b.OnAddr}
			cl = d.client()
		} else {
			cl = &http.Client{Timeout: 120 * time.Second}
		}
	}
	return doRanged(ctx, cl, b.Prefix+rawURL, from)
}

// doRanged 发 GET（from≥0 时带 Range 头）。
func doRanged(ctx context.Context, cl *http.Client, url string, from int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if from >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	req.Header.Set("User-Agent", "ghydra/0.5")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		resp.Body.Close()
		return nil, fmt.Errorf("Range %d 不可满足（文件比预期短？）", from)
	}
	return resp, nil
}

// ParseRangeStart 从 Content-Range 取起始偏移（"bytes 100-999/5000" → 100）。
func ParseRangeStart(cr string) int64 {
	i := strings.IndexByte(cr, ' ')
	if i < 0 {
		return -1
	}
	rest := cr[i+1:]
	j := strings.IndexByte(rest, '-')
	if j < 0 {
		return -1
	}
	n, err := strconv.ParseInt(rest[:j], 10, 64)
	if err != nil {
		return -1
	}
	return n
}
