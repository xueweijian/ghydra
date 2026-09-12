// Package sched 实现 GHydra 的 IP 调度器（M1-W2）：
// 五态状态机管生死（可用性），EWMA 评分管排序（速度），
// 粘性管体验（会话稳定），熔断优先于粘性。
//
// 探测分层（设计定稿）：被动为主——真实连接的成败就是探测信号
// （dev-sidecar v2.2 按需探测：池空时轮转分配候选 IP 给并发连接
// 当试金石）；主动为辅——仅启动 last_good 验证与后台 RTT 刷新。
package sched

import (
	"sort"
	"sync"
	"time"
)

// Config 调度参数（默认值见 DefaultConfig）。
type Config struct {
	Cooldown     time.Duration // 单次失败熔断时长
	Quarantine   time.Duration // 连续失败的隔离时长
	CooldownMax  int           // 连续 Cooldown 轮数达到即隔离
	Sticky       time.Duration // 同域粘性窗口
	ProbeTimeout time.Duration // 按需/主动探测的超时（失败样本 RTT 记此值）
	EWMAAlpha    float64
	IdleAfter    time.Duration // 域名无流量多久后停后台刷新
}

// DefaultConfig 默认参数（PRD + dev-sidecar 实证）。
func DefaultConfig() Config {
	return Config{
		Cooldown:     7 * time.Second,
		Quarantine:   5 * time.Minute,
		CooldownMax:  3,
		Sticky:       60 * time.Second,
		ProbeTimeout: 5 * time.Second,
		EWMAAlpha:    0.3,
		IdleAfter:    time.Hour,
	}
}

// ipState 单个候选 IP 的全部调度状态。
type ipState struct {
	addr string
	rtt  rttStats

	failRate ewma // 失败率 EWMA（失败=1 成功=0）
	samples  int  // 总样本数（含失败）

	state           State
	probing         bool // 被某连接持有当试金石（防重复分配）
	cooldownUntil   time.Time
	quarantineUntil time.Time
	cooldownCount   int // 连续失败轮数（成功清零）
}

func (ip *ipState) scoreOf(cfg Config) float64 {
	return score(ip.rtt.mean.val(), ip.failRate.val(), ip.rtt.cv.val(), ip.samples)
}

// domainState 单域名的调度池。
type domainState struct {
	host        string
	ips         []*ipState
	sticky      *ipState
	stickyUntil time.Time
	probeIdx    int       // 轮转索引（并发自动分散）
	lastAccess  time.Time // 最近 Pick 时间（IdleAfter 节能用）
}

func (d *domainState) find(addr string) *ipState {
	for _, ip := range d.ips {
		if ip.addr == addr {
			return ip
		}
	}
	return nil
}

// Scheduler 并发安全的 IP 调度器。零值不可用，用 New。
type Scheduler struct {
	cfg Config

	mu      sync.Mutex
	domains map[string]*domainState

	// 可注入依赖（生产由 cmd 层接线；测试注入 fake）。
	Dial func(addr string, timeout time.Duration) error // 主动 TCP 探测（兼容测试/通用候选）
	// DialHost 可选：按 host 的 SNI 做完整 TLS 预筛。HTTPS 加速生产路径
	// 使用它，避免“TCP 通但 ClientHello 后沉默”的死 IP 被标成 Active。
	DialHost func(host, addr string, timeout time.Duration) error
	Resolve  func(domain string) ([]string, error) // 池枯竭时 DNS/DoH 补充
	Logf     func(format string, args ...any)      // 调试日志

	// OnActive 在任意 IP 变为/维持 Active 且拿到新 RTT 样本时回调
	// （供 store 持久化 last_good）。回调在调度器锁内，必须非阻塞。
	OnActive func(host, addr string, scoreVal, rttMS float64)

	// now 可注入假时钟（测试）；nil = time.Now。
	now func() time.Time

	resolving map[string]bool // 池枯竭触发的异步补充去重
}

// New 创建调度器。dial/resolve 可后置赋值（nil 时主动探测跳过、
// 池枯竭不补充——测试可完全离线）。
func New(cfg Config) *Scheduler {
	if cfg.EWMAAlpha <= 0 || cfg.EWMAAlpha > 1 {
		cfg.EWMAAlpha = 0.3
	}
	if cfg.CooldownMax <= 0 {
		cfg.CooldownMax = 3
	}
	return &Scheduler{
		cfg:       cfg,
		domains:   make(map[string]*domainState),
		resolving: make(map[string]bool),
	}
}

func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// AddCandidates 向域名池补充候选 IP（bootstrap 喂数 / DoH 兜底 /
// last_good 恢复）。已存在的忽略。返回实际新增数。
func (s *Scheduler) AddCandidates(host string, ips []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.getOrCreateLocked(host)
	added := 0
	for _, addr := range ips {
		if addr == "" || d.find(addr) != nil {
			continue
		}
		d.ips = append(d.ips, &ipState{addr: addr, state: StateNew})
		added++
	}
	return added
}

func (s *Scheduler) getOrCreateLocked(host string) *domainState {
	d, ok := s.domains[host]
	if !ok {
		d = &domainState{host: host}
		s.domains[host] = d
	}
	return d
}

// Pick 为到 host 的新连接选出上游地址。
//
// 优先级：粘性 Active → 全局最优 Active → 轮转候选（本连接当试金石）
// → 池枯竭（触发异步 Resolve 后返回 false，调用方降级直连域名）。
func (s *Scheduler) Pick(host string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	d := s.getOrCreateLocked(host)
	d.lastAccess = now

	// ① 粘性：熔断会同时清 sticky（见 recordFail），故只需查窗口。
	if d.sticky != nil && now.Before(d.stickyUntil) && d.sticky.state == StateActive {
		return d.sticky.addr, true
	}
	d.sticky = nil

	// ② Active 池取 score 最优。
	if best := s.bestActiveLocked(d); best != nil {
		d.sticky, d.stickyUntil = best, now.Add(s.cfg.Sticky)
		return best.addr, true
	}

	// ③ 轮转候选（New → Cooldown/Quarantine 到期 → Probing 空闲复用）。
	if pick := s.rotateLocked(d, now); pick != nil {
		pick.state, pick.probing = StateProbing, true
		return pick.addr, true
	}

	// ④ 池枯竭：异步补充候选，本次降级（调用方直连域名走系统 DNS）。
	go s.resolveAsync(host)
	return "", false
}

func (s *Scheduler) bestActiveLocked(d *domainState) *ipState {
	var best *ipState
	for _, ip := range d.ips {
		if ip.state != StateActive {
			continue
		}
		if best == nil || ip.scoreOf(s.cfg) < best.scoreOf(s.cfg) {
			best = ip
		}
	}
	return best
}

// rotateLocked 按优先级轮转挑选可分配候选。轮转索引保证并发连接
// 拿到不同 IP（分散探测）；新鲜度优先：New > 到期冷却 > 空闲 Probing。
// 全部在探测中时允许复用（洪峰下总比降级 DNS 好，dev-sidecar 第二轮语义）。
func (s *Scheduler) rotateLocked(d *domainState, now time.Time) *ipState {
	var pool []*ipState
	for _, ip := range d.ips {
		if ip.rotatable(now) {
			pool = append(pool, ip)
		}
	}
	if len(pool) == 0 { // 第二轮：忙的 Probing 也放行（复用）
		for _, ip := range d.ips {
			if ip.state == StateProbing {
				pool = append(pool, ip)
			}
		}
	}
	if len(pool) == 0 {
		return nil
	}
	sort.SliceStable(pool, func(i, j int) bool { // New 优先，其次到期冷却，最后复用
		rank := func(ip *ipState) int {
			switch ip.state {
			case StateNew:
				return 0
			case StateProbing:
				return 2
			default: // Cooldown/Quarantine 到期
				return 1
			}
		}
		return rank(pool[i]) < rank(pool[j])
	})
	d.probeIdx %= len(pool)
	pick := pool[d.probeIdx]
	d.probeIdx++
	return pick
}

// Report 上报一次连接结果（被动探测信号；dial 成败或主动探测结果）。
// rtt 为连接耗时（失败时传 0，内部记 ProbeTimeout）。
func (s *Scheduler) Report(host, addr string, rtt time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.getOrCreateLocked(host)
	ip := d.find(addr)
	if ip == nil {
		return // 未入池 IP（降级直连域名的连接等）——不追踪
	}
	if ok {
		s.recordSuccessLocked(d, ip, rtt)
	} else {
		s.recordFailLocked(d, ip)
	}
}

func (s *Scheduler) recordSuccessLocked(d *domainState, ip *ipState, rtt time.Duration) {
	ms := float64(rtt.Microseconds()) / 1000
	ip.rtt.observe(ms)
	ip.failRate.add(0)
	ip.samples++
	ip.cooldownCount = 0
	ip.probing = false
	if ip.state == StateProbing || ip.state == StateCooldown || ip.state == StateQuarantine {
		s.logf("[sched] %s 复活/入池: %s (rtt=%.0fms)", d.host, ip.addr, ip.rtt.mean.val())
	}
	ip.state = StateActive
	if s.OnActive != nil {
		s.OnActive(d.host, ip.addr, ip.scoreOf(s.cfg), ip.rtt.mean.val())
	}
	// 粘性续期：粘中它或尚无粘性 → 绑定它
	if d.sticky == nil || d.sticky == ip {
		d.sticky, d.stickyUntil = ip, s.clock().Add(s.cfg.Sticky)
	}
}

func (s *Scheduler) recordFailLocked(d *domainState, ip *ipState) {
	ip.rtt.observe(float64(s.cfg.ProbeTimeout.Milliseconds()))
	ip.failRate.add(1)
	ip.samples++
	ip.probing = false
	ip.cooldownCount++
	now := s.clock()

	// 熔断优先于粘性：立即失效（M1-Plan R3）
	if d.sticky == ip {
		d.sticky = nil
	}

	switch {
	case ip.state == StateQuarantine: // 隔离期重试仍失败：续隔离
		ip.quarantineUntil = now.Add(s.cfg.Quarantine)
	default:
		if ip.cooldownCount >= s.cfg.CooldownMax {
			ip.state = StateQuarantine
			ip.quarantineUntil = now.Add(s.cfg.Quarantine)
			s.logf("[sched] %s 隔离 %s（连续 %d 轮失败）",
				d.host, ip.addr, ip.cooldownCount)
		} else {
			ip.state = StateCooldown
			ip.cooldownUntil = now.Add(s.cfg.Cooldown)
		}
	}
}

// IPInfo 对外可见的池成员快照。
type IPInfo struct {
	Addr          string
	State         State
	Score         float64
	RTTMS         float64 // EWMA RTT（ms）
	FailRate      float64
	Samples       int
	CooldownCount int
}

// Snapshot 返回域名池快照（按 score 升序；测试与 status 观察口）。
func (s *Scheduler) Snapshot(host string) []IPInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.domains[host]
	if !ok {
		return nil
	}
	out := make([]IPInfo, 0, len(d.ips))
	for _, ip := range d.ips {
		out = append(out, IPInfo{
			Addr: ip.addr, State: ip.state, Score: ip.scoreOf(s.cfg),
			RTTMS: ip.rtt.mean.val(), FailRate: ip.failRate.val(),
			Samples: ip.samples, CooldownCount: ip.cooldownCount,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out
}

// StickyIP 返回当前粘性 IP（status 观察口；无则空）。
func (s *Scheduler) StickyIP(host string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.domains[host]
	if !ok || d.sticky == nil || !s.clock().Before(d.stickyUntil) {
		return ""
	}
	return d.sticky.addr
}

// Hosts 返回有池的域名列表（status 观察口）。
func (s *Scheduler) Hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.domains))
	for h := range s.domains {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// resolveAsync 池枯竭触发的候选补充（去重，单飞）。
func (s *Scheduler) resolveAsync(host string) {
	if s.Resolve == nil {
		return
	}
	s.mu.Lock()
	if s.resolving[host] {
		s.mu.Unlock()
		return
	}
	s.resolving[host] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.resolving, host)
		s.mu.Unlock()
	}()

	ips, err := s.Resolve(host)
	if err != nil {
		s.logf("[sched] 池枯竭补充失败 %s: %v", host, err)
		return
	}
	if n := s.AddCandidates(host, ips); n > 0 {
		s.logf("[sched] 池枯竭补充 %s: +%d 候选", host, n)
		if s.Dial != nil || s.DialHost != nil {
			go s.Preflight(host) // 预筛后活 IP 才进用户连接路径
		}
	}
}
