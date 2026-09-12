// bdns.go —— B 通道 DoH 自举解析（W3：消除系统 DNS 单点故障）。
//
// 真网实证（2026-09-13）：魔法开关会把系统 resolv.conf 写成 1.1.1.1，
// 直连态下 B 通道 DNS 解析直接死（lookup gh-proxy.com timeout）。
// B 通道必须自带解析：bootstrap.DoH（alidns/doh.pub IP 直连）优先，
// 系统 DNS 降级为兜底而非主力。
package get

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// BDNS B 通道域名解析器：DoH 主力 + 内存 TTL 缓存 + 系统 DNS 兜底。
type BDNS struct {
	// ResolveHost DoH 解析（bootstrap.Resolver.ResolveHost 提供）。
	// 返回 IP 列表或 "ip:port" 形态（后者供测试注入随机端口）。
	ResolveHost func(ctx context.Context, host string) ([]string, error)
	// PositiveTTL 正结果缓存时长（默认 5m；DoH TTL 细粒度化留待有实测诉求再做）。
	PositiveTTL time.Duration
	// NegativeTTL 负结果缓存时长（默认 30s，防 DoH 抖动雪崩）。
	NegativeTTL time.Duration

	mu  sync.Mutex
	pos map[string]bnsEntry
	neg map[string]time.Time
}

type bnsEntry struct {
	addrs  []string
	expire time.Time
}

func (b *BDNS) ttl() (pos, neg time.Duration) {
	pos, neg = b.PositiveTTL, b.NegativeTTL
	if pos <= 0 {
		pos = 5 * time.Minute
	}
	if neg <= 0 {
		neg = 30 * time.Second
	}
	return
}

// lookup 缓存感知的解析；DoH 失败返回错误（调用方走系统兜底）。
func (b *BDNS) lookup(ctx context.Context, host string) ([]string, error) {
	if b == nil || b.ResolveHost == nil {
		return nil, contextCancelled(ctx)
	}
	pos, neg := b.ttl()
	now := time.Now()

	b.mu.Lock()
	if b.pos == nil {
		b.pos = map[string]bnsEntry{}
		b.neg = map[string]time.Time{}
	}
	if e, ok := b.pos[host]; ok && now.Before(e.expire) {
		addrs := e.addrs
		b.mu.Unlock()
		return addrs, nil
	}
	if t, ok := b.neg[host]; ok && now.Before(t) {
		b.mu.Unlock()
		return nil, errNegativeCached
	}
	b.mu.Unlock()

	addrs, err := b.ResolveHost(ctx, host)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil || len(addrs) == 0 {
		b.neg[host] = now.Add(neg)
		delete(b.pos, host)
		if err == nil {
			err = errNoAddrs
		}
		return nil, err
	}
	b.pos[host] = bnsEntry{addrs: addrs, expire: now.Add(pos)}
	delete(b.neg, host)
	return addrs, nil
}

// drop 驱逐缓存（拨号全败后调用：DoH 结果 ≠ 可达）。
func (b *BDNS) drop(host string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pos, host)
}

type bdnsErr string

func (e bdnsErr) Error() string { return string(e) }

const (
	errNegativeCached = bdnsErr("DoH 近期失败（负缓存内）")
	errNoAddrs        = bdnsErr("DoH 无可用地址")
)

func contextCancelled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errNoAddrs
	}
}

// joinDialAddr 解析结果 → 拨号目标。ResolveHost 返回 "ip:port" 时原样用
// （测试注入 httptest 随机端口），纯 IP 时拼 addr 的端口。
func joinDialAddr(addrFromResolver, port string) string {
	if _, _, err := net.SplitHostPort(addrFromResolver); err == nil {
		return addrFromResolver
	}
	return net.JoinHostPort(addrFromResolver, port)
}

// bDialer 产出 B 通道 http.Client：DoH 拨 IP + SNI/证书按 CDN 域名语义
// （同 M0 dohDialMap 手法：换的只是 TCP 层目标）。
type bDialer struct {
	DNS         *BDNS
	InsecureTLS bool
	OnAddr      func(host, addr string, doh bool)
}

func (d *bDialer) client() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			// 头必须到（TTFB 上限）；body 流不限时（大文件透传，ctx 可取消）
			ResponseHeaderTimeout: 30 * time.Second,
			DialTLSContext:        d.dialTLS,
		},
	}
}

func (d *bDialer) dialTLS(ctx context.Context, _, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	plain := port == "80"

	// 主力：DoH 解析 → 逐 IP 拨号
	if addrs, err := d.DNS.lookup(ctx, host); err == nil {
		for _, a := range addrs {
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", joinDialAddr(a, port))
			if err != nil {
				continue
			}
			if plain {
				d.note(host, c.RemoteAddr().String(), true)
				return c, nil
			}
			tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: d.InsecureTLS})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				continue
			}
			d.note(host, c.RemoteAddr().String(), true)
			return tc, nil
		}
		// DoH 给的全拨不通：驱逐，后续走系统兜底/重解析
		d.DNS.drop(host)
	}

	// 兜底：系统解析直拨（旧行为）
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if plain {
		d.note(host, c.RemoteAddr().String(), false)
		return c, nil
	}
	tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: d.InsecureTLS})
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	d.note(host, c.RemoteAddr().String(), false)
	return tc, nil
}

func (d *bDialer) note(host, addr string, doh bool) {
	if d.OnAddr != nil {
		d.OnAddr(host, addr, doh)
	}
}
