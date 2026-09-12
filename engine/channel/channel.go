// Package channel 实现双通道路由决策（M2-W1，方案 D1/D3）。
//
// D1 按流量形态分流：CONNECT（浏览器/API TLS 隧道）是通道 A 专属——
// URL 前缀反代（gh-proxy 协议）要求目标 URL 出现在请求路径里，
// CONNECT 内不可见，SNI 改写已被 M0 密码学证伪。git/下载/明文 HTTP
// 可走 B。
//
// D3 两级熔断分层：sched 管 IP 级（通道 A 内部）；本包管通道级——
// A 整体 Closed/Open/HalfOpen 三态。恢复只由 doctor 探针驱动
// （HalfOpen 下流量仍走 B，不拿用户流量试探，避免体验损耗与抖动）。
package channel

import (
	"fmt"
	"sync"
	"time"
)

// Name 通道标识。
type Name string

const (
	Direct Name = "A" // 直连择优（M1 引擎）
	CDN    Name = "B" // CDN 前缀反代（gh-proxy 协议）
)

// Kind 流量形态（决定 B 通道能否承接）。
type Kind uint8

const (
	KindCONNECT   Kind = iota // 浏览器/API TLS 隧道（CONNECT）——A 专属
	KindGit                   // clone/fetch/push（insteadOf 层）
	KindDownload              // Release/归档/raw 下载
	KindPlainHTTP             // serve 明文 http 代理路径
)

// CanB 该形态能否走通道 B。
func (k Kind) CanB() bool { return k != KindCONNECT }

// Flow 一次路由请求的输入。
type Flow struct {
	Kind Kind
	Host string // 目标域名（M2 全局通道状态；按域细分留 M4 遥测后）
}

// Route 路由决策输出。
type Route struct {
	Channel  Name
	Bypassed bool   // override 强制覆盖了熔断状态（诊断用）
	Reason   string // 人话原因（status/日志）
}

// Override 用户逃生通道（ghydra channel set）。
type Override uint8

const (
	OverrideAuto Override = iota
	OverrideForceA
	OverrideForceB
)

// State 通道 A 的通道级熔断状态。
type State uint8

const (
	StateClosed   State = iota // A 正常承载
	StateOpen                  // A 隔离：新流量（可走 B 的）切 B
	StateHalfOpen              // 探活复验中：流量仍走 B，等 doctor 结论
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	default:
		return "half-open"
	}
}

// Verdict doctor 对通道 A 的判定信号（probe.Report 蒸馏而来，
// 直接 vs 代理对照组区分「我们的锅 vs GitHub 的锅」）。
type Verdict uint8

const (
	VerdictNone        Verdict = iota
	VerdictHealthy             // A 探针恢复（HalfOpen → Closed）
	VerdictSourceFault         // 源头级故障：GitHub 侧 403/全场景死——A/B 同因，
	// 但 B 出口在海外不受影响（2025-04 型事件特征）
	VerdictNetworkFault // 网络级故障：TCP/TLS 阻断特征——A 死 B 活
	VerdictUnclear      // 对照组也死（GitHub 自身故障）——不动通道状态
)

// Config 决策参数（零值可用，见 DefaultConfig）。
type Config struct {
	WindowSize    int           // 失败率滑动窗口样本数
	FailThreshold float64       // 窗口失败率触发阈值
	MinSamples    int           // 统计触发的最小样本数
	OpenCooldown  time.Duration // Open → HalfOpen 冷却
	BackoffMax    time.Duration // HalfOpen 探活失败的退避上限（翻倍增长）
	BHealthTTL    time.Duration // B 探针结果的信任时长（过期回退未知=可切）
}

// DefaultConfig 实证参数：窗口 12 样本、≥8 样本失败率过半即熔断
// （dev-sidecar v2.2 一次失败即切的激进度在 A 通道会造成误切——
// 单 IP 故障由 sched IP 级熔断兜住，通道级只接系统性故障）。
func DefaultConfig() Config {
	return Config{
		WindowSize:    12,
		FailThreshold: 0.5,
		MinSamples:    8,
		OpenCooldown:  60 * time.Second,
		BackoffMax:    10 * time.Minute,
		BHealthTTL:    5 * time.Minute,
	}
}

// Router 通道决策器。并发安全。
type Router struct {
	cfg Config
	now func() time.Time // 时钟注入（测试）

	mu        sync.Mutex
	override  Override
	st        State
	openUntil time.Time // Open 状态的冷却截止
	backoff   int       // HalfOpen 失败退避指数（OpenCooldown << backoff）
	tripAt    time.Time
	tripWhy   string
	win       []bool // 环形窗口
	winN      int    // 已填充样本数（< len 时不算失败率）
	winHead   int

	// B 通道健康信号（W3：trip 前置检查——别往死通道里切）。
	// bOK=nil 表示未知（从未探过）：不抑制熔断（保持 W1 行为）。
	bOK         *bool
	bAt         time.Time
	bSuppressed int    // 因 B 死而抑制切道的次数
	bSuppWhy    string // 最近一次抑制的原始判定
	bSuppAt     time.Time
}

// New 创建决策器。now 为 nil 用真实时钟。
func New(cfg Config, now func() time.Time) *Router {
	if cfg.WindowSize <= 0 {
		cfg = DefaultConfig()
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = cfg.WindowSize / 2
	}
	if now == nil {
		now = time.Now
	}
	return &Router{cfg: cfg, now: now, st: StateClosed, win: make([]bool, cfg.WindowSize)}
}

// Route 为一次流量选道。
func (r *Router) Route(f Flow) Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stateLocked() // 先推进状态机（Open 到期 → HalfOpen）

	switch r.override {
	case OverrideForceA:
		return Route{Channel: Direct, Bypassed: st != StateClosed,
			Reason: "override=A（用户强制直连）"}
	case OverrideForceB:
		if !f.Kind.CanB() {
			// CONNECT 交给 B = 全断（B 没有 CONNECT 语义），宁可走 A
			return Route{Channel: Direct, Bypassed: true,
				Reason: "override=B，但 CONNECT 无法走 B（D1），仍走 A"}
		}
		return Route{Channel: CDN, Bypassed: st == StateClosed, Reason: "override=B（用户强制 CDN）"}
	}

	if !f.Kind.CanB() {
		why := "CONNECT 为 A 专属形态（D1）"
		if st != StateClosed {
			why += "；A 故障中（" + r.tripWhy + "），CONNECT 无兜底——等待 doctor 结论"
		}
		return Route{Channel: Direct, Reason: why}
	}
	switch st {
	case StateClosed:
		return Route{Channel: Direct, Reason: "A 正常"}
	default: // Open / HalfOpen：流量一律 B，恢复由 doctor 驱动（D3）
		return Route{Channel: CDN, Reason: "A " + st.String() + "（" + r.tripWhy + "）→ B 承接"}
	}
}

// Report 上报一次真实流量结果（通道 A 的成败进失败率窗口；
// B 的结果不参与 A 状态——B 自身的重试/换端点在下载器层处理）。
func (r *Router) Report(f Flow, ch Name, ok bool) {
	if ch != Direct {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.win[r.winHead] = ok
	r.winHead = (r.winHead + 1) % len(r.win)
	if r.winN < len(r.win) {
		r.winN++
	}
	if r.st == StateClosed && r.winN >= r.cfg.MinSamples && r.failRateLocked() >= r.cfg.FailThreshold {
		if r.bKnownDeadLocked() {
			// B 死：切过去也是死（W3 trip 前置检查，与 doctor 路径一致）
			r.bSuppressed++
			r.bSuppWhy = "流量失败率 " + pctLocked(r.failRateLocked())
			r.bSuppAt = r.now()
			r.resetWindowLocked()
			return
		}
		r.tripLocked("流量失败率 " + pctLocked(r.failRateLocked()))
	}
}

// NotifyDoctor 接入 doctor 判定。
func (r *Router) NotifyDoctor(v Verdict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stateLocked()
	switch v {
	case VerdictSourceFault, VerdictNetworkFault:
		if r.bKnownDeadLocked() {
			// B 探针近期失败：切过去也是死——保持 A，记录抑制
			// （真实缺陷修复：W1 会盲切进死 B）。
			r.bSuppressed++
			r.bSuppWhy = verdictWhy(v)
			r.bSuppAt = r.now()
			return
		}
		if st == StateHalfOpen {
			// 探活失败：退避翻倍再回 Open（M2-R4 防抖）
			r.backoff++
		}
		if st != StateOpen { // 已 Open 不重置冷却计时
			r.tripLocked(verdictWhy(v))
		}
	case VerdictHealthy:
		if st == StateHalfOpen {
			r.st = StateClosed
			r.resetWindowLocked()
			r.backoff = 0
		}
		// VerdictUnclear / VerdictNone：不动状态（对照组也死 = 不是我们的锅）
	}
}

// NotifyB 接入 B 通道探针结果（doctor cdn 列）。
func (r *Router) NotifyB(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := ok
	r.bOK = &v
	r.bAt = r.now()
}

// bKnownDeadLocked 近期探针明确失败（未知/过期 = 不抑制）。
func (r *Router) bKnownDeadLocked() bool {
	if r.bOK == nil || *r.bOK {
		return false
	}
	ttl := r.cfg.BHealthTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return r.now().Sub(r.bAt) < ttl
}

// SetOverride 用户逃生通道。
func (r *Router) SetOverride(o Override) {
	r.mu.Lock()
	r.override = o
	r.mu.Unlock()
}

// Snapshot 状态诊断（ghydra status / doctor 用）。
type Snapshot struct {
	State       State
	Override    Override
	FailRate    float64 // 窗口失败率（样本不足为 0）
	Samples     int
	TripAt      time.Time
	TripWhy     string
	OpenUntil   time.Time
	BackoffMult int

	// B 通道健康（W3）
	BOK         *bool     // nil = 未知（未探测）
	BAt         time.Time // 最近 B 探针时间
	BSuppressed int       // 因 B 死而抑制切道次数
	BSuppWhy    string    // 最近一次抑制的原始判定
	BSuppAt     time.Time // 最近一次抑制时间
}

func (r *Router) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		State:       r.stateLocked(),
		Override:    r.override,
		Samples:     r.winN,
		TripAt:      r.tripAt,
		TripWhy:     r.tripWhy,
		OpenUntil:   r.openUntil,
		BackoffMult: r.backoff,
		BOK:         r.bOK,
		BAt:         r.bAt,
		BSuppressed: r.bSuppressed,
		BSuppWhy:    r.bSuppWhy,
		BSuppAt:     r.bSuppAt,
	}
	if r.winN >= r.cfg.MinSamples {
		s.FailRate = r.failRateLocked()
	}
	return s
}

// --- 内部（持锁） ---

// stateLocked 推进并返回当前状态（Open 冷却到期 → HalfOpen）。
func (r *Router) stateLocked() State {
	if r.st == StateOpen && !r.now().Before(r.openUntil) {
		r.st = StateHalfOpen
	}
	return r.st
}

func (r *Router) tripLocked(why string) {
	r.st = StateOpen
	cd := r.cfg.OpenCooldown << uint(r.backoff) // 退避：60s,120s,…上限 BackoffMax
	if cd > r.cfg.BackoffMax || cd <= 0 {
		cd = r.cfg.BackoffMax
	}
	r.openUntil = r.now().Add(cd)
	r.tripAt = r.now()
	r.tripWhy = why
	r.resetWindowLocked() // 熔断后清窗口，恢复期重新积累样本
}

func (r *Router) failRateLocked() float64 {
	if r.winN == 0 {
		return 0
	}
	fails := 0
	for i := 0; i < r.winN; i++ {
		if !r.win[i] {
			fails++
		}
	}
	return float64(fails) / float64(r.winN)
}

func (r *Router) resetWindowLocked() {
	r.winN, r.winHead = 0, 0
}

func verdictWhy(v Verdict) string {
	if v == VerdictSourceFault {
		return "doctor：源头级故障（403 特征，A/B 同因，B 出口不受影响）"
	}
	return "doctor：网络级阻断（TCP/TLS 特征）"
}

func pctLocked(f float64) string {
	return fmt.Sprintf("%.0f%%", f*100)
}
