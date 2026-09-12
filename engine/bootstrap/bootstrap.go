// Package bootstrap 实现四级自举链（见 docs/GHydra-PRD.md §6.4）：
//
//	① GitHub meta API → ② DoH(dns.alidns.com JSON) → ③ 本地 last-good 缓存
//	→ ④ 二进制内冻结种子（种子 IP 带 Host 头直连 meta，闭环回 ①）
//
// 任一级成功即返回，并把结果刷新到缓存。全程不依赖系统 DNS（②之后）。
package bootstrap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Step 记录自举链中一级的尝试结果。
type Step struct {
	Level     int      `json:"level"`
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Err       string   `json:"err,omitempty"`
	ElapsedMS float64  `json:"elapsed_ms"`
	IPs       []string `json:"ips,omitempty"`
}

// Result 是一次自举解析的完整轨迹（bench 报告的数据源）。
type Result struct {
	Steps     []Step   `json:"steps"`
	IPs       []string `json:"ips"`
	Source    string   `json:"source"`
	Domains   []string `json:"domains,omitempty"`
	ElapsedMS float64  `json:"elapsed_ms"`
}

// Seeds 是编译期嵌入的冻结种子清单（发版时快照）。
type Seeds struct {
	Version    int      `json:"version"`
	CapturedAt string   `json:"captured_at"`
	Source     string   `json:"source"`
	Domains    []string `json:"domains"`
	SeedIPs    []string `json:"seed_ips"`
}

// Resolver 按四级链解析 GitHub 候选 IP。零值不可用，请用 New()。
type Resolver struct {
	HTTP         *http.Client
	MetaURL      string
	DoHEndpoints []string
	DoHName      string
	CachePath    string
	Seeds        *Seeds
	PerNet       int
	SeedPort     int
	SeedParallel int
	SeedTimeout  time.Duration
	TLSRoots     *x509.CertPool
}

// directClient 不走任何代理（含环境变量代理）——自举探测必须反映
// 直连环境的真实可达性，用户的代理配置不应污染四级链的判断。
var directClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{},
}

// New 返回默认配置的解析器。
func New() *Resolver {
	seeds, err := DefaultSeeds()
	if err != nil {
		seeds = &Seeds{}
	}
	home, _ := os.UserHomeDir()
	return &Resolver{
		HTTP:         directClient,
		MetaURL:      "https://api.github.com/meta",
		DoHEndpoints: []string{"https://dns.alidns.com/resolve", "https://doh.pub/resolve"},
		DoHName:      "github.com",
		CachePath:    filepath.Join(home, ".ghydra", "cache.json"),
		Seeds:        seeds,
		PerNet:       4,
		SeedPort:     443,
		SeedParallel: 8,
		SeedTimeout:  5 * time.Second,
	}
}

type metaDomains struct {
	Website []string `json:"website"`
}

type metaResp struct {
	Web     []string    `json:"web"`
	Git     []string    `json:"git"`
	Domains metaDomains `json:"domains"`
}

type dohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	Data string `json:"data"`
}

type dohResp struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

type cacheFile struct {
	SavedAt time.Time `json:"saved_at"`
	IPs     []string  `json:"ips"`
	Domains []string  `json:"domains"`
}

// Resolve 依次执行四级自举链，任一级成功即返回。
func (r *Resolver) Resolve(ctx context.Context) *Result {
	start := time.Now()
	res := &Result{}

	// ① meta API（正常 DNS）
	t0 := time.Now()
	if ips, doms, err := r.fetchMetaWith(ctx, r.HTTP); err == nil && len(ips) > 0 {
		r.finish(res, 1, "meta", ips, doms, t0)
		res.ElapsedMS = ms(start)
		return res
	} else if err != nil {
		res.Steps = append(res.Steps, Step{Level: 1, Name: "meta", Err: err.Error(), ElapsedMS: ms(t0)})
	} else {
		res.Steps = append(res.Steps, Step{Level: 1, Name: "meta", Err: "empty ip list", ElapsedMS: ms(t0)})
	}

	// ② DoH（多端点轮询）
	for _, ep := range r.DoHEndpoints {
		t0 = time.Now()
		ips, err := r.fetchDoH(ctx, ep)
		if err == nil && len(ips) > 0 {
			r.finish(res, 2, "doh:"+ep, ips, r.seedDomains(), t0)
			res.ElapsedMS = ms(start)
			return res
		}
		msg := "empty answer"
		if err != nil {
			msg = err.Error()
		}
		res.Steps = append(res.Steps, Step{Level: 2, Name: "doh:" + ep, Err: msg, ElapsedMS: ms(t0)})
	}

	// ③ last-good 缓存
	t0 = time.Now()
	if ips, doms, err := r.loadCache(); err == nil && len(ips) > 0 {
		res.Steps = append(res.Steps, Step{Level: 3, Name: "cache", OK: true, ElapsedMS: ms(t0), IPs: ips})
		res.IPs, res.Domains, res.Source = ips, doms, "cache"
		res.ElapsedMS = ms(start)
		return res
	} else if err != nil {
		res.Steps = append(res.Steps, Step{Level: 3, Name: "cache", Err: err.Error(), ElapsedMS: ms(t0)})
	} else {
		res.Steps = append(res.Steps, Step{Level: 3, Name: "cache", Err: "empty", ElapsedMS: ms(t0)})
	}

	// ④ 冻结种子直连 meta（带 Host 头，绕开 DNS）
	t0 = time.Now()
	if ips, doms, err := r.resolveViaSeeds(ctx); err == nil && len(ips) > 0 {
		r.finish(res, 4, "seed-direct", ips, doms, t0)
		res.ElapsedMS = ms(start)
		return res
	} else if err != nil {
		res.Steps = append(res.Steps, Step{Level: 4, Name: "seed-direct", Err: err.Error(), ElapsedMS: ms(t0)})
	} else {
		res.Steps = append(res.Steps, Step{Level: 4, Name: "seed-direct", Err: "empty", ElapsedMS: ms(t0)})
	}

	res.ElapsedMS = ms(start)
	return res
}

func (r *Resolver) finish(res *Result, level int, name string, ips, doms []string, t0 time.Time) {
	res.Steps = append(res.Steps, Step{Level: level, Name: name, OK: true, ElapsedMS: ms(t0), IPs: ips})
	res.IPs, res.Domains, res.Source = ips, doms, name
	_ = r.saveCache(ips, doms)
}

func ms(t0 time.Time) float64 {
	return float64(time.Since(t0).Microseconds()) / 1000.0
}

// fetchMetaWith 请求 meta API 并提取候选 IP 与域名清单。
func (r *Resolver) fetchMetaWith(ctx context.Context, client *http.Client) ([]string, []string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.MetaURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "GHydra-Bootstrap/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("meta api status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	var m metaResp
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("meta json: %w", err)
	}
	cidrs := append(append([]string{}, m.Web...), m.Git...)
	ips := extractCIDRIPs(cidrs, r.perNet())
	return ips, m.Domains.Website, nil
}

// dohDialMap 把 DoH 端点域名映射到已知服务 IP，使 L2 在系统 DNS
// 完全不可用时仍可工作（自举链每一级都不得依赖上一级的能力）。
// 阿里 / 腾讯的公共 DoH 服务 IP 官方长期稳定。
var dohDialMap = map[string]string{
	"dns.alidns.com": "223.5.5.5",
	"doh.pub":        "119.29.29.29",
}

// resolveDoHAddr 将 addr 中的 DoH 域名替换为已知 IP，其余原样返回。
func resolveDoHAddr(addr string, dialMap map[string]string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip, ok := dialMap[host]; ok {
		return net.JoinHostPort(ip, port)
	}
	return addr
}

// dohClient 的 Transport 在 TCP 层拨服务 IP，但 URL/Host/SNI/证书
// 校验仍按端点域名进行，语义与正常访问完全一致。
var dohClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, resolveDoHAddr(addr, dohDialMap))
		},
	},
}

// fetchDoH 请求一个 DoH JSON 端点并提取 A 记录。
func (r *Resolver) fetchDoH(ctx context.Context, endpoint string) ([]string, error) {
	u := endpoint + "?name=" + url.QueryEscape(r.DoHName) + "&type=A"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := dohClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if err != nil {
		return nil, err
	}
	var d dohResp
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("doh json: %w", err)
	}
	if d.Status != 0 {
		return nil, fmt.Errorf("doh dns status %d", d.Status)
	}
	var ips []string
	for _, a := range d.Answer {
		if a.Type == 1 && net.ParseIP(a.Data) != nil && net.ParseIP(a.Data).To4() != nil {
			ips = append(ips, a.Data)
		}
	}
	return ips, nil
}

// resolveViaSeeds 并发尝试种子 IP 直连 meta API，任一成功即返回。
func (r *Resolver) resolveViaSeeds(ctx context.Context) ([]string, []string, error) {
	if r.Seeds == nil || len(r.Seeds.SeedIPs) == 0 {
		return nil, nil, errors.New("bootstrap: no seed ips")
	}
	type attempt struct {
		ips     []string
		domains []string
	}
	sem := make(chan struct{}, r.seedParallel())
	results := make(chan attempt, len(r.Seeds.SeedIPs))
	errs := make(chan error, len(r.Seeds.SeedIPs))
	for _, ip := range r.Seeds.SeedIPs {
		go func(ip string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			ips, doms, err := r.trySeedIP(ctx, ip)
			if err != nil {
				errs <- err
				return
			}
			results <- attempt{ips: ips, domains: doms}
		}(ip)
	}
	for i := 0; i < len(r.Seeds.SeedIPs); i++ {
		select {
		case a := <-results:
			return a.ips, a.domains, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-errs:
			// 继续等待其余种子
		}
	}
	return nil, nil, errors.New("bootstrap: all seed ips failed")
}

// trySeedIP 用 TLS 直连种子 IP（SNI/Host 均为 meta 域名，绕开系统 DNS）。
func (r *Resolver) trySeedIP(ctx context.Context, ip string) ([]string, []string, error) {
	u, err := url.Parse(r.MetaURL)
	if err != nil {
		return nil, nil, err
	}
	seedPort := strconv.Itoa(r.seedPort())
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: r.seedTimeout()},
		Config:    &tls.Config{ServerName: u.Hostname(), RootCAs: r.TLSRoots},
	}
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, seedPort))
		},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	return r.fetchMetaWith(ctx, client)
}

func (r *Resolver) saveCache(ips, doms []string) error {
	if r.CachePath == "" {
		return nil
	}
	c := cacheFile{SavedAt: time.Now(), IPs: ips, Domains: doms}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.CachePath), 0o755); err != nil {
		return err
	}
	tmp := r.CachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.CachePath)
}

func (r *Resolver) loadCache() ([]string, []string, error) {
	if r.CachePath == "" {
		return nil, nil, errors.New("no cache path")
	}
	data, err := os.ReadFile(r.CachePath)
	if err != nil {
		return nil, nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil, err
	}
	return c.IPs, c.Domains, nil
}

func (r *Resolver) seedDomains() []string {
	if r.Seeds != nil {
		return r.Seeds.Domains
	}
	return nil
}

func (r *Resolver) perNet() int {
	if r.PerNet > 0 {
		return r.PerNet
	}
	return 4
}

func (r *Resolver) seedParallel() int {
	if r.SeedParallel > 0 {
		return r.SeedParallel
	}
	return 8
}

func (r *Resolver) seedPort() int {
	if r.SeedPort > 0 {
		return r.SeedPort
	}
	return 443
}

func (r *Resolver) seedTimeout() time.Duration {
	if r.SeedTimeout > 0 {
		return r.SeedTimeout
	}
	return 5 * time.Second
}

// extractCIDRIPs 从 IPv4 CIDR 段中提取代表 IP（每段前 perNet 个 + 段尾 1 个）。
func extractCIDRIPs(cidrs []string, perNet int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ip net.IP) {
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	inc := func(ip net.IP) bool {
		for i := len(ip) - 1; i >= 0; i-- {
			ip[i]++
			if ip[i] != 0 {
				return true
			}
		}
		return false
	}
	for _, c := range cidrs {
		ip, ipnet, err := net.ParseCIDR(c)
		if err != nil || ip.To4() == nil || ipnet == nil {
			continue
		}
		base := ip.To4()
		cur := make(net.IP, len(base))
		copy(cur, base)
		for i := 0; i < perNet; i++ {
			if !inc(cur) || !ipnet.Contains(cur) {
				break
			}
			add(cur.To4())
		}
		// 段尾地址 = 网络地址 + 段大小 - 1
		ones, _ := ipnet.Mask.Size()
		hostBits := uint(32 - ones)
		last := make(net.IP, len(base))
		copy(last, base)
		if hostBits > 0 {
			carry := uint32(1)<<hostBits - 1
			for i := 3; i >= 0; i-- {
				v := uint32(last[i]) + carry&0xFF
				last[i] = byte(v & 0xFF)
				carry = (carry >> 8) + (v >> 8)
			}
		}
		if !last.Equal(base) && ipnet.Contains(last) {
			add(last.To4())
		}
	}
	return out
}
