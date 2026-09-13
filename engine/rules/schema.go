package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// rules.json v1 严格解析（M3-W2 设计 §2.1 / 威胁模型 A8 语义投毒、A9 降级）。
//
// 防线三层：
//  1. 结构：DisallowUnknownFields + 重复键检测 + 尾随数据检测
//     （重复键可让人眼 review 的合法 JSON 在运行时语义反转，是语义投毒的隐形通道）
//  2. 值域：版本上限（A4 快进 DoS）、时间窗（A6 冻结告警线）
//  3. 内容上限：域名/CDN/种子的数量与形态白名单——规则是远程可写攻击面，
//     宁可拒收合法但罕见的内容，也不放过构造内容

// SchemaVersionV1 当前唯一支持的 schema 版本。出现 v2 时老客户端必须拒绝
// （向前安全：宁可冻结在旧规则，不可硬吃未知格式）。
const SchemaVersionV1 = 1

// 上限常量（设计 §1 A4/A8）。
const (
	MaxVersion      = 1_000_000           // 快进 DoS 硬上限
	MaxDomains      = 64                  // 加速域名上限
	MaxCDNEndpoints = 4                   // B 通道端点上限
	MaxSeedDomains  = 8                   // 种子 IP 表域名数上限
	MaxSeedIPs      = 32                  // 单域种子 IP 上限
	MaxFileBytes    = 1 << 20             // 1 MiB 拉取/文件尺寸上限（A5 无尽数据）
	MaxValidity     = 45 * 24 * time.Hour // expires_at 距 generated_at 上限
)

var (
	// ErrSchema 结构或值域非法（错误链含具体原因）。
	ErrSchema = errors.New("rules: schema violation")
	// ErrTooLarge 内容超过 MaxFileBytes（A5）。
	ErrTooLarge = errors.New("rules: file too large")
	// ErrVersionCap 版本超过 MaxVersion 硬上限（A4 快进 DoS）。sentinel
	// 供 Apply 区分归因：同样拒收，但 doctor 必须看到 fast-forward 攻击
	// 类型而非笼统 schema violation。
	ErrVersionCap = fmt.Errorf("%w: version exceeds hard cap", ErrSchema)

	domainRe = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])*)+$`)
)

// 保留名拒收：加速清单不允许 loopback/内网/链路本地语义域，
// 防止把本机或内网服务流量导进代理路径。
var reservedSuffixes = []string{
	".localhost", ".local", ".internal", ".home.arpa", ".lan", ".home", ".corp", ".invalid",
}

// RulesFile rules.json v1。
type RulesFile struct {
	SchemaVersion int                 `json:"schema_version"`
	Version       int64               `json:"version"`
	GeneratedAt   time.Time           `json:"generated_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
	Domains       []string            `json:"domains"`
	CDNEndpoints  []string            `json:"cdn_endpoints"`
	SeedIPs       map[string][]string `json:"seed_ips"`
}

// ParseRules 严格解析并校验 rules.json 全文。
func ParseRules(data []byte) (*RulesFile, error) {
	if len(data) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rf RulesFile
	if err := dec.Decode(&rf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	// 重复键检测：DisallowUnknownFields 抓不到 {"version":1,"version":2}。
	if err := dupKeys(data); err != nil {
		return nil, err
	}
	// 尾随数据：一个文件只允许一个 JSON 值。
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after JSON object", ErrSchema)
	}
	if err := rf.Validate(); err != nil {
		return nil, err
	}
	return &rf, nil
}

// Validate 值域与内容白名单校验（可独立调用：provider 加载磁盘文件时复验）。
func (r *RulesFile) Validate() error {
	if r.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("%w: schema_version %d unsupported", ErrSchema, r.SchemaVersion)
	}
	if r.Version < 1 {
		return fmt.Errorf("%w: version %d < 1", ErrSchema, r.Version)
	}
	if r.Version > MaxVersion {
		return fmt.Errorf("%w: %d > %d", ErrVersionCap, r.Version, MaxVersion)
	}
	if r.GeneratedAt.IsZero() || r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: missing timestamps", ErrSchema)
	}
	if r.ExpiresAt.Before(r.GeneratedAt) {
		return fmt.Errorf("%w: expires_at before generated_at", ErrSchema)
	}
	if r.ExpiresAt.Sub(r.GeneratedAt) > MaxValidity {
		return fmt.Errorf("%w: validity exceeds %s", ErrSchema, MaxValidity)
	}
	if len(r.Domains) == 0 || len(r.Domains) > MaxDomains {
		return fmt.Errorf("%w: %d domains", ErrSchema, len(r.Domains))
	}
	for _, d := range r.Domains {
		if err := checkDomain(d); err != nil {
			return err
		}
	}
	if len(r.CDNEndpoints) > MaxCDNEndpoints {
		return fmt.Errorf("%w: %d cdn endpoints", ErrSchema, len(r.CDNEndpoints))
	}
	for _, c := range r.CDNEndpoints {
		if err := checkCDN(c); err != nil {
			return err
		}
	}
	if len(r.SeedIPs) > MaxSeedDomains {
		return fmt.Errorf("%w: %d seed domains", ErrSchema, len(r.SeedIPs))
	}
	for dom, ips := range r.SeedIPs {
		if err := checkDomain(dom); err != nil {
			return fmt.Errorf("seed domain: %w", err)
		}
		if len(ips) == 0 || len(ips) > MaxSeedIPs {
			return fmt.Errorf("%w: %q has %d seed ips", ErrSchema, dom, len(ips))
		}
		for _, s := range ips {
			ip, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("%w: seed ip %q: %v", ErrSchema, s, err)
			}
			if !ip.Is4() && !ip.Is4In6() && !ip.Is6() || isPrivateIP(ip) {
				return fmt.Errorf("%w: seed ip %q not public unicast", ErrSchema, s)
			}
		}
	}
	return nil
}

func isPrivateIP(ip netip.Addr) bool {
	// netip.Addr 没有分类辅助，用 net.IP 复用标准库判定。
	n := net.IP(ip.AsSlice())
	return n.IsLoopback() || n.IsPrivate() || n.IsLinkLocalUnicast() ||
		n.IsLinkLocalMulticast() || n.IsMulticast() || n.IsUnspecified()
}

func checkDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" || len(d) > 253 {
		return fmt.Errorf("%w: domain %q length", ErrSchema, d)
	}
	ld := strings.ToLower(d)
	if !domainRe.MatchString(ld) {
		return fmt.Errorf("%w: domain %q malformed", ErrSchema, d)
	}
	for _, suf := range reservedSuffixes {
		if strings.HasSuffix(ld, suf) {
			return fmt.Errorf("%w: domain %q reserved (%s)", ErrSchema, d, suf)
		}
	}
	return nil
}

func checkCDN(c string) error {
	if len(c) > 200 || strings.ContainsAny(c, " \t\r\n") {
		return fmt.Errorf("%w: cdn %q", ErrSchema, c)
	}
	rest, ok := strings.CutPrefix(strings.ToLower(c), "https://")
	if !ok || rest == "" {
		return fmt.Errorf("%w: cdn %q not https", ErrSchema, c)
	}
	host := rest
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if h, _, ok := strings.Cut(host, ":"); ok { // :port
		host = h
	}
	return checkDomain(strings.TrimPrefix(host, "www."))
}

// dupKeys 走 token 流检测对象内重复键（任意嵌套层）。
func dupKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func(dec *json.Decoder, inObj bool) error
	walk = func(dec *json.Decoder, inObj bool) error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch tv := t.(type) {
		case json.Delim:
			switch tv {
			case '{':
				seen := map[string]struct{}{}
				for dec.More() {
					kt, err := dec.Token()
					if err != nil {
						return err
					}
					key := kt.(string)
					if _, dup := seen[key]; dup {
						return fmt.Errorf("%w: duplicate key %q", ErrSchema, key)
					}
					seen[key] = struct{}{}
					if err := walk(dec, false); err != nil {
						return err
					}
				}
				_, err = dec.Token() // closing '}'
				return err
			case '[':
				for dec.More() {
					if err := walk(dec, false); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			}
			return nil
		default:
			_ = inObj
			return nil // 标量值：消费掉即可
		}
	}
	if err := walk(dec, false); err != nil {
		return fmt.Errorf("%w: %v", ErrSchema, err)
	}
	return nil
}
