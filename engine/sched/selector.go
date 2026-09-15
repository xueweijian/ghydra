package sched

import (
	"net"
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
)

// Selector 把调度器适配成 proxy.UpstreamSelector（W1 预留的接口）。
//
// 命中加速规则 → 调度器择优；池枯竭 → 降级直连域名（系统 DNS，
// W1 行为兜底）。降级连接 Target 为域名（事件日志可辨识）。
type Selector struct {
	Sched *Scheduler
	Rules *rules.Matcher
}

// NewSelector 构造（scheduler 与 matcher 均由调用方装配）。
func NewSelector(sc *Scheduler, m *rules.Matcher) *Selector {
	return &Selector{Sched: sc, Rules: m}
}

// Select 实现 proxy.UpstreamSelector。
func (sel *Selector) Select(host string) (string, bool) {
	if !sel.accelHost(host) {
		return "", false // 未命中加速域名/SSH 例外：放行（系统 DNS 直连）
	}
	if addr, ok := sel.Sched.Pick(host); ok {
		return addr, true
	}
	// 池枯竭降级：仍走加速路径标记，但目标回退域名本身。
	return net.JoinHostPort(host, "443"), true
}

// accelHost 判定 host 是否走加速路径：规则命中且非 SSH 例外。
// Select 与竞速三方法共用同一口径。
func (sel *Selector) accelHost(host string) bool {
	// ssh.github.com:443 是 SSH 协议，不是 HTTPS；M1 只探测 SSH，
	// 不把它改道到普通 GitHub HTTPS IP。通道 B/M2 再处理 SSH over 443。
	if strings.EqualFold(strings.TrimSuffix(host, "."), "ssh.github.com") {
		return false
	}
	return sel != nil && sel.Sched != nil && sel.Rules != nil && sel.Rules.Match(host)
}

// 编译期断言：Selector 具备 proxy 的竞速能力（F3）。
var _ proxy.RacingSelector = (*Selector)(nil)

// ValidatedPick 实现 proxy.RacingSelector 热路径：只出已验证上游。
func (sel *Selector) ValidatedPick(host string) (string, bool) {
	if !sel.accelHost(host) {
		return "", false
	}
	return sel.Sched.PickValidated(host)
}

// RacingPick 实现 proxy.RacingSelector 竞速候选。
func (sel *Selector) RacingPick(host string, n int, exclude map[string]bool) []string {
	if !sel.accelHost(host) {
		return nil
	}
	return sel.Sched.PickN(host, n, exclude)
}

// RacingReportFail 上报候选真实失败（进 Cooldown，下轮排除）。
func (sel *Selector) RacingReportFail(host, addr string) {
	sel.Sched.Report(host, addr, 0, false)
}

// RacingReportWin 上报候选成功（拨号 ms 喂 EWMA；竞速胜者/迟到胜者）。
func (sel *Selector) RacingReportWin(host, addr string, dialMS float64) {
	sel.Sched.Report(host, addr, time.Duration(dialMS*float64(time.Millisecond)), true)
}

// RacingRelease 归还未实际拨号的候选（取消不惩罚）。
func (sel *Selector) RacingRelease(host string, addrs []string) {
	sel.Sched.ReleaseCandidates(host, addrs)
}

// ReportEvent 把 proxy.Event 翻译成调度信号（OnEvent 回调里调用）。
//
// 信号分档：
//   - DialErr ≠ nil     → 硬失败（TCP 层死，立即熔断）
//   - 疑似握手死        → 硬失败（dial 成功但极短时间内双向流极小且带
//     转发错误——TCP 通而 TLS 死的 GFW 特征，设计 §1.3 盲区兜底）
//   - 其余              → 成功（客户端主动断开是正常浏览行为，不归罪 IP）
func (sel *Selector) ReportEvent(ev proxy.Event) {
	if sel == nil || sel.Sched == nil || !ev.Accel {
		return
	}
	// Target 为池内 IP:443；降级直连的连接 Target=域名:443，不在池内。
	addr := ev.Target
	if _, ok := sel.Sched.lookup(ev.Host, addr); !ok {
		return
	}
	rtt := time.Duration(ev.DialMS * float64(time.Millisecond))
	if ev.DialErr != nil || looksLikeHandshakeDead(ev) {
		sel.Sched.Report(ev.Host, addr, 0, false)
		return
	}
	sel.Sched.Report(ev.Host, addr, rtt, true)
}

// HandshakeDead 对外暴露握手死判定（daemon 的 CONNECT 事件进通道
// 流量窗口时复用同一特征口径）。
func HandshakeDead(ev proxy.Event) bool { return looksLikeHandshakeDead(ev) }

// looksLikeHandshakeDead：dial 成功但上游零回包（客户端发出去的
// ClientHello 石沉大海）+ 带转发错误——TCP 通而 TLS 死的干扰特征。
// 实测特征（2026-09-12 移动网络）：tx=517 rx=0，客户端超时断开后
// 上行报 splice reset。rx==0 是核心信号：任何正常服务至少回一个字节。
func looksLikeHandshakeDead(ev proxy.Event) bool {
	return ev.CopyErr != "" && ev.Rx == 0 && ev.Tx > 0
}

// lookup 判断 addr 是否在 host 池内（ReportEvent 过滤降级连接）。
func (s *Scheduler) lookup(host, addr string) (*ipState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.domains[host]
	if !ok {
		return nil, false
	}
	ip := d.find(addr)
	return ip, ip != nil
}
