// Package probe 提供 M1-W4 doctor 探针。
//
// 设计原则：
//   - direct 与 proxy 使用完全独立的、明确禁用环境代理的 Transport；
//   - HTTP 探针只读，不执行 clone/push 写操作；clone/push 用 Git Smart HTTP
//     的 info/refs GET 做 dry-run；
//   - 每个请求保留 DNS/connect/TLS/TTFB 阶段计时，错误交给 classify.go；
//   - SSH 只做 TCP 连通性探测（22 与 ssh.github.com:443），不发送认证数据。
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Mode 是探针出站路径。
type Mode string

const (
	ModeDirect Mode = "direct"
	ModeProxy  Mode = "proxy"
	// ModeCDN 经 B 通道 CDN 前缀探测（W3：M2-R1 对策——trip 前要知道
	// B 活不活，别往死通道里切）。请求 URL 改写为 cdnPrefix+原URL。
	ModeCDN Mode = "cdn"
)

// Class 是 doctor 的基础根因类别。
type Class string

const (
	ClassOK               Class = "ok"
	ClassDNS              Class = "dns_pollution"
	ClassTCPBlock         Class = "tcp_block"
	ClassTLSReset         Class = "tls_reset"
	ClassTimeout          Class = "timeout"
	ClassHTTP4xx          Class = "http_4xx"
	ClassHTTP5xx          Class = "http_5xx"
	ClassProxyUnreachable Class = "proxy_unreachable"
	ClassUnknown          Class = "unknown"
)

// Config 是探针运行参数。
type Config struct {
	Timeout    time.Duration
	ProxyURL   string // 例如 http://127.0.0.1:9801；ModeDirect 忽略
	CDNPrefix  string // B 通道 CDN 前缀（gh-proxy 协议）；ModeCDN 必填
	Repo       string // owner/name；clone/push dry-run 使用
	ReleaseURL string
	BodyLimit  int64
	UserAgent  string
	// TLSConfig 可选，仅测试/企业受控环境注入；生产默认使用系统根。
	TLSConfig *tls.Config
}

// DefaultConfig 返回生产默认值。所有超时都由调用方可控，避免 doctor
// 在网络黑洞上无限挂起。
func DefaultConfig() Config {
	return Config{
		Timeout:    12 * time.Second,
		Repo:       "xueweijian/ghydra",
		ReleaseURL: "https://github.com/cli/cli/releases/latest",
		BodyLimit:  512 * 1024,
		UserAgent:  "GHydra-Doctor/0.1",
	}
}

// Check 是一个 HTTP 或 TCP 子探针的结果。
type Check struct {
	Scenario string `json:"scenario"`
	Name     string `json:"name"`
	Mode     Mode   `json:"mode"`
	Target   string `json:"target"`
	OK       bool   `json:"ok"`
	// Reachable 表示已经收到对端 HTTP 响应；即使是 401/403/404，
	// 也能证明 DNS/TCP/TLS/HTTP 链路已走通。
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status,omitempty"`
	Class     Class  `json:"class"`
	Error     string `json:"error,omitempty"`

	DurationMS float64 `json:"duration_ms"`
	DNSMS      float64 `json:"dns_ms,omitempty"`
	ConnectMS  float64 `json:"connect_ms,omitempty"`
	TLSMS      float64 `json:"tls_ms,omitempty"`
	TTFBMS     float64 `json:"ttfb_ms,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
	RateBPS    float64 `json:"rate_bps,omitempty"`
}

// ScenarioReport 是六个用户场景之一。SSH 场景包含两个 TCP 子检查。
type ScenarioReport struct {
	Scenario string  `json:"scenario"`
	OK       bool    `json:"ok"`
	Checks   []Check `json:"checks"`
}

// Report 是一次 direct 或 proxy 探针运行结果。
type Report struct {
	Mode          Mode             `json:"mode"`
	ProxyURL      string           `json:"proxy_url,omitempty"`
	StartedAt     time.Time        `json:"started_at"`
	DurationMS    float64          `json:"duration_ms"`
	Total         int              `json:"total"` // 含 SSH 观测场景
	Passed        int              `json:"passed"`
	CoveredTotal  int              `json:"covered_total"` // 五场景可用率分母
	CoveredPassed int              `json:"covered_passed"`
	Scenarios     []ScenarioReport `json:"scenarios"`
}

// Endpoint 描述一个只读 HTTP 端点。AcceptStatus 为空时接受 2xx/3xx。
type Endpoint struct {
	Scenario     string
	Name         string
	URL          string
	BodyLimit    int64
	AcceptStatus func(int) bool
}

// Runner 执行 doctor 探针。
type Runner struct {
	cfg Config
}

// New 创建 Runner，并修正不安全/无意义的零值。
func New(cfg Config) *Runner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultConfig().Timeout
	}
	if cfg.Repo == "" {
		cfg.Repo = DefaultConfig().Repo
	}
	if cfg.ReleaseURL == "" {
		cfg.ReleaseURL = DefaultConfig().ReleaseURL
	}
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = DefaultConfig().BodyLimit
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultConfig().UserAgent
	}
	return &Runner{cfg: cfg}
}

// Config 返回只读配置副本，供 CLI 展示/测试。
func (r *Runner) Config() Config { return r.cfg }

// Run 执行六场景：网页、登录、clone dry-run、push dry-run、Release、SSH。
func (r *Runner) Run(ctx context.Context, mode Mode) Report {
	started := time.Now()
	rep := Report{Mode: mode, ProxyURL: r.cfg.ProxyURL, StartedAt: started}

	// 七个子探针相互独立，并发启动：黑洞网络不能让 doctor 串行
	// 等待；每个 HTTP/TCP 检查都能获得完整的 cfg.Timeout 预算。
	// ModeCDN 只跑 HTTP 五场景（B 通道是 HTTP 反代，SSH 不适用）。
	isCDN := mode == ModeCDN
	endpoints := r.httpEndpoints()
	httpChecks := make([]Check, len(endpoints))
	sshChecks := make([]Check, 2)
	sshTargets := [2]string{"github.com:22", "ssh.github.com:443"}
	sshNames := [2]string{"ssh-22", "ssh-443"}
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep Endpoint) {
			defer wg.Done()
			httpChecks[i] = r.runHTTP(ctx, mode, ep)
		}(i, ep)
	}
	if !isCDN {
		for i := range sshChecks {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sshChecks[i] = r.runTCP(ctx, mode, "ssh", sshNames[i], sshTargets[i])
			}(i)
		}
	}
	wg.Wait()
	for i, ep := range endpoints {
		c := httpChecks[i]
		rep.Scenarios = append(rep.Scenarios, ScenarioReport{Scenario: ep.Scenario, OK: c.OK, Checks: []Check{c}})
	}
	if !isCDN {
		sshOK := sshChecks[0].OK && sshChecks[1].OK
		rep.Scenarios = append(rep.Scenarios, ScenarioReport{Scenario: "ssh", OK: sshOK, Checks: sshChecks})
	}
	rep.DurationMS = elapsedMS(started)
	for _, s := range rep.Scenarios {
		rep.Total++
		if s.OK {
			rep.Passed++
		}
		if s.Scenario != "ssh" {
			rep.CoveredTotal++
			if s.OK {
				rep.CoveredPassed++
			}
		}
	}
	return rep
}

// RunEndpoints 探测自定义端点，供单测与 bench 使用。
func (r *Runner) RunEndpoints(ctx context.Context, mode Mode, endpoints []Endpoint) []Check {
	out := make([]Check, 0, len(endpoints))
	for _, ep := range endpoints {
		out = append(out, r.runHTTP(ctx, mode, ep))
	}
	return out
}

// GitHubEndpoints 是 M0 六域名真网存活探针。objects 根没有稳定的
// 公开资源路径，因此按“收到任意 1xx–4xx 响应即主机可达”计 Reachable。
func GitHubEndpoints(repo string) []Endpoint {
	if repo == "" {
		repo = DefaultConfig().Repo
	}
	repo = strings.Trim(repo, "/")
	return []Endpoint{
		{Scenario: "github-domains", Name: "github", URL: "https://github.com/"},
		{Scenario: "github-domains", Name: "api", URL: "https://api.github.com/"},
		{Scenario: "github-domains", Name: "codeload", URL: "https://codeload.github.com/" + repo + "/zip/refs/heads/main", BodyLimit: 64 * 1024},
		{Scenario: "github-domains", Name: "avatars", URL: "https://avatars.githubusercontent.com/u/9919?s=64", BodyLimit: 64 * 1024},
		{Scenario: "github-domains", Name: "objects", URL: "https://objects.githubusercontent.com/", AcceptStatus: func(code int) bool { return code >= 100 && code < 500 }, BodyLimit: 4 * 1024},
		{Scenario: "github-domains", Name: "raw", URL: "https://raw.githubusercontent.com/" + repo + "/main/README.md", BodyLimit: 64 * 1024},
	}
}

// RunGitHubDomains 探测六个 GitHub 主机，供 bench --mode direct|proxy。
func (r *Runner) RunGitHubDomains(ctx context.Context, mode Mode) []Check {
	return r.RunEndpoints(ctx, mode, GitHubEndpoints(r.cfg.Repo))
}

func (r *Runner) httpEndpoints() []Endpoint {
	repo := strings.Trim(r.cfg.Repo, "/")
	cloneURL := "https://github.com/" + repo + ".git/info/refs?service=git-upload-pack"
	pushURL := "https://github.com/" + repo + ".git/info/refs?service=git-receive-pack"
	return []Endpoint{
		{Scenario: "web", Name: "web", URL: "https://github.com/"},
		{Scenario: "login", Name: "login", URL: "https://github.com/login"},
		{Scenario: "clone", Name: "clone-dry-run", URL: cloneURL, BodyLimit: 64 * 1024},
		// GET info/refs 不会修改仓库；401/403 说明已经到达 GitHub
		// 的鉴权层，故作为 transport 可用但业务未授权处理。
		{Scenario: "push", Name: "push-dry-run", URL: pushURL, BodyLimit: 64 * 1024, AcceptStatus: func(code int) bool {
			return (code >= 200 && code < 400) || code == http.StatusUnauthorized || code == http.StatusForbidden
		}},
		{Scenario: "release", Name: "release-ttfb-rate", URL: r.cfg.ReleaseURL, BodyLimit: r.cfg.BodyLimit},
	}
}

func (r *Runner) client(mode Mode) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if r.cfg.TLSConfig != nil {
		tlsConfig = r.cfg.TLSConfig.Clone()
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // 明确禁止 HTTP(S)_PROXY 污染 direct 组
		DialContext:           (&net.Dialer{Timeout: r.cfg.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   r.cfg.Timeout,
		ResponseHeaderTimeout: r.cfg.Timeout,
		IdleConnTimeout:       10 * time.Second,
	}
	if mode == ModeCDN {
		if r.cfg.CDNPrefix == "" {
			return nil, fmt.Errorf("未提供 CDN 前缀（ModeCDN 必填）")
		}
		// CDN 侧客户端与 direct 同构（不走代理）；URL 改写在 runHTTP。
	}
	if mode == ModeProxy {
		if r.cfg.ProxyURL == "" {
			return nil, fmt.Errorf("未提供 HTTP 代理地址")
		}
		u, err := url.Parse(r.cfg.ProxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("代理地址无效: %q", r.cfg.ProxyURL)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: r.cfg.Timeout}, nil
}

type traceTimes struct {
	mu                  sync.Mutex
	dnsStart, dnsDone   time.Time
	connStart, connDone time.Time
	tlsStart, tlsDone   time.Time
	firstByte           time.Time
	stage               string
}

func (t *traceTimes) setStage(stage string) {
	t.mu.Lock()
	t.stage = stage
	t.mu.Unlock()
}
func (t *traceTimes) getStage() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stage
}

func (r *Runner) runHTTP(ctx context.Context, mode Mode, ep Endpoint) Check {
	started := time.Now()
	c := Check{Scenario: ep.Scenario, Name: ep.Name, Mode: mode, Target: ep.URL, Class: ClassUnknown}
	client, err := r.client(mode)
	if err != nil {
		c.Error = err.Error()
		c.Class = ClassProxyUnreachable
		c.DurationMS = elapsedMS(started)
		return c
	}

	limit := ep.BodyLimit
	if limit <= 0 {
		limit = r.cfg.BodyLimit
	}
	trace := &traceTimes{}
	ct := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			trace.mu.Lock()
			trace.dnsStart = time.Now()
			trace.stage = "dns"
			trace.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) { trace.mu.Lock(); trace.dnsDone = time.Now(); trace.mu.Unlock() },
		ConnectStart: func(_, _ string) {
			trace.mu.Lock()
			trace.connStart = time.Now()
			trace.stage = "connect"
			trace.mu.Unlock()
		},
		ConnectDone: func(_, _ string, e error) {
			trace.mu.Lock()
			trace.connDone = time.Now()
			if e != nil {
				trace.stage = "connect"
			}
			trace.mu.Unlock()
		},
		TLSHandshakeStart:    func() { trace.mu.Lock(); trace.tlsStart = time.Now(); trace.stage = "tls"; trace.mu.Unlock() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { trace.mu.Lock(); trace.tlsDone = time.Now(); trace.mu.Unlock() },
		GotFirstResponseByte: func() { trace.mu.Lock(); trace.firstByte = time.Now(); trace.stage = "response"; trace.mu.Unlock() },
	}
	requestCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, ct), r.cfg.Timeout)
	defer cancel()
	reqURL := ep.URL
	if mode == ModeCDN {
		// gh-proxy 协议：完整目标 URL 拼接在 CDN 前缀后
		reqURL = r.cfg.CDNPrefix + ep.URL
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		c.Error = err.Error()
		c.Class = ClassUnknown
		c.DurationMS = elapsedMS(started)
		return c
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		c.Error = err.Error()
		c.Class = classify(err, 0, trace.getStage(), mode)
		c.DurationMS = elapsedMS(started)
		r.fillTrace(&c, trace, started)
		return c
	}
	defer resp.Body.Close()
	c.Status = resp.StatusCode
	c.Reachable = true
	bodyStart := time.Now()
	if !trace.firstByte.IsZero() {
		bodyStart = trace.firstByte
	}
	c.Bytes, err = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
	if err != nil {
		c.Error = err.Error()
		c.Class = classify(err, c.Status, trace.getStage(), mode)
	} else {
		accepted := ep.AcceptStatus == nil && resp.StatusCode >= 200 && resp.StatusCode < 400
		if ep.AcceptStatus != nil {
			accepted = ep.AcceptStatus(resp.StatusCode)
		}
		c.OK = accepted
		if accepted {
			c.Class = ClassOK
		} else {
			c.Error = resp.Status
			c.Class = classify(nil, resp.StatusCode, trace.getStage(), mode)
		}
	}
	if c.Bytes > 0 {
		readMS := time.Since(bodyStart).Seconds()
		if readMS > 0 {
			c.RateBPS = float64(c.Bytes) / readMS
		}
	}
	c.DurationMS = elapsedMS(started)
	r.fillTrace(&c, trace, started)
	return c
}

func (r *Runner) fillTrace(c *Check, t *traceTimes, started time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.dnsStart.IsZero() && !t.dnsDone.IsZero() {
		c.DNSMS = t.dnsDone.Sub(t.dnsStart).Seconds() * 1000
	}
	if !t.connStart.IsZero() && !t.connDone.IsZero() {
		c.ConnectMS = t.connDone.Sub(t.connStart).Seconds() * 1000
	}
	if !t.tlsStart.IsZero() && !t.tlsDone.IsZero() {
		c.TLSMS = t.tlsDone.Sub(t.tlsStart).Seconds() * 1000
	}
	if !t.firstByte.IsZero() {
		c.TTFBMS = t.firstByte.Sub(started).Seconds() * 1000
	}
}

func (r *Runner) runTCP(ctx context.Context, mode Mode, scenario, name, target string) Check {
	started := time.Now()
	c := Check{Scenario: scenario, Name: name, Mode: mode, Target: target, Class: ClassUnknown}
	var conn net.Conn
	var err error
	if mode == ModeProxy {
		conn, err = dialHTTPConnect(ctx, r.cfg.ProxyURL, target, r.cfg.Timeout, r.cfg.UserAgent)
	} else {
		d := net.Dialer{Timeout: r.cfg.Timeout, KeepAlive: 30 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", target)
	}
	if err != nil {
		c.Error = err.Error()
		s := strings.ToLower(err.Error())
		if mode == ModeProxy && (strings.Contains(s, "proxy dial") || strings.Contains(s, "proxy: 未提供") || strings.Contains(s, "proxy: 地址无效") || strings.Contains(s, "proxyconnect")) {
			c.Class = ClassProxyUnreachable
		} else {
			// 代理返回的 CONNECT 502/拒绝是代理可达但上游失败。
			// 没有更细的响应阶段时归 tcp_block，避免误报代理自身死亡。
			c.Class = ClassTCPBlock
		}
		c.DurationMS = elapsedMS(started)
		return c
	}
	c.OK, c.Reachable, c.Class = true, true, ClassOK
	c.DurationMS = elapsedMS(started)
	conn.Close()
	return c
}

func dialHTTPConnect(ctx context.Context, rawProxy, target string, timeout time.Duration, userAgent string) (net.Conn, error) {
	if rawProxy == "" {
		return nil, fmt.Errorf("proxy: 未提供 HTTP 代理地址")
	}
	u, err := url.Parse(rawProxy)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("proxy: 地址无效 %q", rawProxy)
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy dial %s: %w", u.Host, err)
	}
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	if userAgent == "" {
		userAgent = "GHydra-Doctor/0.1"
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nProxy-Connection: keep-alive\r\n\r\n", target, target, userAgent); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy write CONNECT: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: target}})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy read CONNECT: %w", err)
	}
	if resp.Body != nil {
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", target, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func elapsedMS(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

// classify 将“能分则分”的低级信号映射为稳定枚举。更精细的 M11
// 分类器不在 M1 扩张。
func classify(err error, status int, stage string, mode Mode) Class {
	if err == nil {
		switch {
		case status >= 500:
			return ClassHTTP5xx
		case status >= 400:
			return ClassHTTP4xx
		default:
			return ClassUnknown
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ClassTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || strings.Contains(strings.ToLower(err.Error()), "no such host") || strings.Contains(strings.ToLower(err.Error()), "temporary failure in name resolution") {
		return ClassDNS
	}
	s := strings.ToLower(err.Error())
	if mode == ModeProxy && (strings.Contains(s, "proxy dial") || strings.Contains(s, "proxy: ") || strings.Contains(s, "proxyconnect")) {
		return ClassProxyUnreachable
	}
	if stage == "tls" || strings.Contains(s, "tls") || strings.Contains(s, "bad record mac") || strings.Contains(s, "remote error") {
		return ClassTLSReset
	}
	if stage == "connect" || strings.Contains(s, "connection reset") || strings.Contains(s, "connection refused") || strings.Contains(s, "network is unreachable") || strings.Contains(s, "no route to host") {
		return ClassTCPBlock
	}
	if strings.Contains(s, "eof") || strings.Contains(s, "broken pipe") {
		return ClassTLSReset
	}
	return ClassUnknown
}

// Classify 暴露纯函数供单元测试与 CLI 以外的调用者复用。
func Classify(err error, status int, stage string, mode Mode) Class {
	return classify(err, status, stage, mode)
}
