// Package rules 提供加速域名匹配与默认域名清单。
//
// M1 规则引擎骨架（PRD F3）：域名清单与 GitHub meta API 的 domains
// 字段同构（"github.com" / "*.github.com"），运行时可由 bootstrap 的
// meta 结果热替换；ed25519 远程签名规则属 M9，不在本包范围。
package rules

import "strings"

// DefaultDomains 是发版时冻结的默认加速域名清单（六场景全集）。
// 运行时由 bootstrap 从 meta API 刷新，失败时以本清单兜底。
// 注意：不含 github.io（Pages 为用户自建站，IP 语义不同，M1 不加速）。
var DefaultDomains = []string{
	"github.com", "*.github.com",
	"githubusercontent.com", "*.githubusercontent.com",
	"githubassets.com", "*.githubassets.com",
}

// Matcher 匹配加速域名。零值不可用，必须经 New 构造；构造后只读，并发安全。
type Matcher struct {
	exact  map[string]struct{}
	suffix map[string]struct{} // key 形如 ".github.com"
	doms   []string
}

// New 按域名清单构造匹配器。元素形如 "github.com"（精确）或
// "*.github.com"（匹配任意深度子域）。空白与非法元素被忽略。
func New(domains []string) *Matcher {
	m := &Matcher{
		exact:  make(map[string]struct{}, len(domains)),
		suffix: make(map[string]struct{}, len(domains)),
		doms:   make([]string, 0, len(domains)),
	}
	for _, d := range domains {
		d = Normalize(d)
		if d == "" {
			continue
		}
		m.doms = append(m.doms, d)
		if rest, ok := strings.CutPrefix(d, "*"); ok {
			// rest 形如 ".github.com"；要求至少一个子域标签
			if len(rest) > 1 {
				m.suffix[rest] = struct{}{}
			}
			continue
		}
		m.exact[d] = struct{}{}
	}
	return m
}

// Match 判断 host（可含端口、大小写不敏感）是否命中加速规则。
// "*.github.com" 匹配任意深度子域（api.github.com、a.b.github.com），
// 但不匹配裸域 github.com（与 meta API 清单语义一致，裸域单列）。
func (m *Matcher) Match(host string) bool {
	h := Normalize(host)
	if h == "" {
		return false
	}
	if _, ok := m.exact[h]; ok {
		return true
	}
	for i := 0; i < len(h); i++ {
		if h[i] == '.' {
			if _, ok := m.suffix[h[i:]]; ok {
				return true
			}
		}
	}
	return false
}

// Domains 返回当前清单快照（构造顺序），用于 PAC 生成与诊断输出。
func (m *Matcher) Domains() []string {
	out := make([]string, len(m.doms))
	copy(out, m.doms)
	return out
}

// Normalize 规范化主机名：去空白、转小写、剥端口（authority 形式
// host:port 与 IPv6 [::1]:port）、剥 FQDN 尾点。
func Normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndexByte(host, ']'); i >= 0 {
		host = host[:i+1] // IPv6 字面量，保留括号形式（加速清单中不会出现）
	} else if strings.Count(host, ":") == 1 {
		host = host[:strings.IndexByte(host, ':')] // 唯一冒号 = host:port
	}
	host = strings.TrimSuffix(host, ".")
	return host
}
