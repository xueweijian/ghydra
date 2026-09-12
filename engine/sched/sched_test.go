package sched

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
)

// fakeClock 可手动推进的时钟（状态机时间语义测试）。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1700000000, 0)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestSched() (*Scheduler, *fakeClock) {
	sc := New(DefaultConfig())
	c := newFakeClock()
	sc.now = c.Now
	return sc, c
}

// --- EWMA 与评分 ---

func TestEWMAScoreColdStart(t *testing.T) {
	// 冷启动无样本：中庸 0.5
	if got := score(0, 0, 0, 0); got != 0.5 {
		t.Fatalf("冷启动 score = %v, want 0.5", got)
	}
}

func TestEWMAScoreWeights(t *testing.T) {
	// 权重锁定：可用性主导、速度微调（设计 §3.3 行为契约）
	fastGood := score(150, 0, 0.1, 10)   // 150ms 零失败
	slowGood := score(300, 0, 0.1, 10)   // 300ms 零失败
	fastFail := score(150, 0.2, 0.1, 10) // 150ms 两成失败

	if fastGood >= slowGood {
		t.Errorf("同可用性下 RTT 应影响排序: %v vs %v", fastGood, slowGood)
	}
	if fastFail <= slowGood {
		t.Errorf("失败率惩罚应远大于 RTT 差异: fail=%v slowGood=%v", fastFail, slowGood)
	}
	// 定量：失败率 0.2 的惩罚（0.1）应 ≥ 2× RTT 差 150ms 的惩罚（0.045）
	if (fastFail - fastGood) < 2*(slowGood-fastGood) {
		t.Errorf("权重比例失调: Δfail=%v Δrtt=%v", fastFail-fastGood, slowGood-fastGood)
	}
}

func TestEWMAConvergence(t *testing.T) {
	var e ewma
	e.alpha = 0.3
	e.add(0)
	for i := 0; i < 20; i++ {
		e.add(100)
	}
	if e.val() < 92 || e.val() > 100 {
		t.Fatalf("20 个 100 样本后应收敛至 ~100: %v", e.val())
	}
}

func TestFailurePullsRTT(t *testing.T) {
	// 失败样本 RTT 记 ProbeTimeout：双通道拉低评分
	cfg := DefaultConfig()
	var r rttStats
	r.mean.alpha, r.cv.alpha = cfg.EWMAAlpha, cfg.EWMAAlpha
	for i := 0; i < 5; i++ {
		r.observe(150)
	}
	before := score(r.mean.val(), 0, r.cv.val(), 5)
	r.observe(float64(cfg.ProbeTimeout.Milliseconds()))
	after := score(r.mean.val(), 0.3, r.cv.val(), 6)
	if after <= before {
		t.Fatalf("失败样本应拉低评分: %v -> %v", before, after)
	}
}

// --- 状态机 ---

func TestStateMachineHappyPath(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})

	addr, ok := sc.Pick("github.com")
	if !ok || addr != "1.1.1.1:443" {
		t.Fatalf("Pick = %q %v, want 1.1.1.1:443 true", addr, ok)
	}
	snap := sc.Snapshot("github.com")
	if snap[0].State != StateProbing {
		t.Fatalf("轮转分配后应 Probing: %v", snap[0].State)
	}

	sc.Report("github.com", addr, 150*time.Millisecond, true)
	snap = sc.Snapshot("github.com")
	if snap[0].State != StateActive || snap[0].RTTMS <= 0 {
		t.Fatalf("成功上报后应 Active 且有 RTT: %+v", snap[0])
	}
}

func TestStateMachineCooldownThenRecover(t *testing.T) {
	sc, clk := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443"})

	a1, _ := sc.Pick("github.com")
	sc.Report("github.com", a1, 0, false) // 试金石失败

	snap := sc.Snapshot("github.com")
	for _, ip := range snap {
		if ip.Addr == a1 && ip.State != StateCooldown {
			t.Fatalf("失败后应 Cooldown: %+v", ip)
		}
	}

	// Cooldown 期内不被轮转：Pick 应给另一个
	a2, _ := sc.Pick("github.com")
	if a2 == a1 {
		t.Fatalf("Cooldown 期内不应再分到 %s", a1)
	}

	// 7s 到期：重新可轮转
	clk.Advance(8 * time.Second)
	a3, _ := sc.Pick("github.com")
	if a3 != a1 {
		t.Fatalf("Cooldown 到期应重新轮转到 %s, got %s", a1, a3)
	}
	// 成功复活
	sc.Report("github.com", a3, 120*time.Millisecond, true)
	snap = sc.Snapshot("github.com")
	for _, ip := range snap {
		if ip.Addr == a3 && ip.State != StateActive {
			t.Fatalf("Cooldown 后成功应复活 Active: %+v", ip)
		}
	}
}

func TestStateMachineQuarantine(t *testing.T) {
	sc, clk := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443", "3.3.3.3:443", "4.4.4.4:443"})
	bad := "1.1.1.1:443"

	// 连续 3 轮失败 → Quarantine。
	// 「轮」= Cooldown 到期后重试又失败：到期重试不经轮转直接 Report
	// （轮转语义由 TestStateMachineCooldownThenRecover 单独锁定）。
	for round := 0; round < 3; round++ {
		clk.Advance(8 * time.Second)           // 过 Cooldown，bad 重回可轮转
		sc.Report("github.com", bad, 0, false) // 到期重试仍失败
	}
	snap := sc.Snapshot("github.com")
	for _, ip := range snap {
		if ip.Addr == bad && ip.State != StateQuarantine {
			t.Fatalf("3 轮失败后应 Quarantine: %+v", ip)
		}
	}

	// 隔离期 5min 内不被轮转
	clk.Advance(4 * time.Minute)
	for i := 0; i < 10; i++ {
		if p, _ := sc.Pick("github.com"); p == bad {
			t.Fatalf("隔离期内不应分到 %s", bad)
		}
	}
	// 5min 到期释放回轮转，成功则计数清零复活
	clk.Advance(2 * time.Minute)
	p, _ := sc.Pick("github.com")
	sc.Report("github.com", p, 90*time.Millisecond, true)
	snap = sc.Snapshot("github.com")
	for _, ip := range snap {
		if ip.State == StateQuarantine {
			t.Fatalf("Quarantine 到期成功后应全部复活: %+v", snap)
		}
	}
}

// --- 粘性 ---

func TestStickyWindow(t *testing.T) {
	sc, clk := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443"})

	a1, _ := sc.Pick("github.com")
	sc.Report("github.com", a1, 100*time.Millisecond, true) // 入 Active+粘性

	for i := 0; i < 5; i++ {
		if p, _ := sc.Pick("github.com"); p != a1 {
			t.Fatalf("粘性窗口内应稳定 %s, got %s", a1, p)
		}
	}

	// 61s 后粘性过期，重新按 score 择优（另一 IP 更优的场景）
	clk.Advance(70 * time.Second)
	sc.Report("github.com", "2.2.2.2:443", 10*time.Millisecond, true) // 先把它拉入 Active
	// 2.2.2.2 rtt 10ms 远优于 1.1.1.1 的 100ms
	p, _ := sc.Pick("github.com")
	if p != "2.2.2.2:443" {
		t.Fatalf("粘性过期后应择优: got %s", p)
	}
}

func TestStickyBrokenByFailure(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443"})

	a1, _ := sc.Pick("github.com")
	sc.Report("github.com", a1, 100*time.Millisecond, true)
	if sc.StickyIP("github.com") != a1 {
		t.Fatal("应有粘性")
	}

	// 熔断优先于粘性（R3）：失败立即打破
	sc.Report("github.com", a1, 0, false)
	if sc.StickyIP("github.com") == a1 {
		t.Fatal("失败后粘性应立即失效")
	}
	p, _ := sc.Pick("github.com")
	if p == a1 {
		t.Fatalf("熔断后不应继续用 %s", a1)
	}
}

// --- 轮转分散 ---

func TestRotateSpreads(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{
		"1.1.1.1:443", "2.2.2.2:443", "3.3.3.3:443", "4.4.4.4:443", "5.5.5.5:443",
	})
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		p, ok := sc.Pick("github.com")
		if !ok {
			t.Fatal("有候选时 Pick 不应枯竭")
		}
		if seen[p] {
			t.Fatalf("轮转应分散: %s 重复", p)
		}
		seen[p] = true // 连接未结束（probing 持有），下一个应不同
	}
	if len(seen) != 5 {
		t.Fatalf("5 连接应拿到 5 个不同 IP: %v", seen)
	}
}

func TestRotateReuseWhenAllProbing(t *testing.T) {
	// 全部在探测中：第二轮允许复用（dev-sidecar 语义）
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})
	p1, _ := sc.Pick("github.com")
	p2, ok := sc.Pick("github.com")
	if !ok || p2 != p1 {
		t.Fatalf("全忙时应复用: %q %v", p2, ok)
	}
}

// --- 池枯竭与降级 ---

func TestPoolExhaustionFallsBack(t *testing.T) {
	sc, _ := newTestSched()
	// 空池 + 无 Resolve：Pick 返回 false（selector 降级直连域名）
	if _, ok := sc.Pick("github.com"); ok {
		t.Fatal("空池应返回 false 让调用方降级")
	}
}

func TestPoolExhaustionResolves(t *testing.T) {
	sc, _ := newTestSched()
	resolved := make(chan string, 1)
	sc.Resolve = func(host string) ([]string, error) {
		resolved <- host
		return []string{"6.6.6.6:443"}, nil
	}
	sc.Pick("github.com") // 触发异步补充
	select {
	case h := <-resolved:
		if h != "github.com" {
			t.Fatalf("resolve host = %q", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("池枯竭应触发 Resolve")
	}
	// 补充后可 Pick 到
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := sc.Pick("github.com"); ok && p == "6.6.6.6:443" {
			return
		}
	}
	t.Fatal("补充后应能 Pick 到新候选")
}

// --- 主动探测 ---

func TestPreflightKillsDeadCandidates(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443", "3.3.3.3:443"})
	dead := map[string]bool{"1.1.1.1:443": true, "3.3.3.3:443": true}
	sc.Dial = func(addr string, timeout time.Duration) error {
		if dead[addr] {
			return errDial()
		}
		return nil
	}
	if n := sc.Preflight("github.com"); n != 3 {
		t.Fatalf("Preflight = %d, want 3", n)
	}
	// 死的直接进 Cooldown（不吃用户连接的轮转配额），活的 Active
	snap := sc.Snapshot("github.com")
	states := map[string]State{}
	for _, ip := range snap {
		states[ip.Addr] = ip.State
	}
	if states["1.1.1.1:443"] != StateCooldown || states["3.3.3.3:443"] != StateCooldown {
		t.Fatalf("死候选应熔断: %v", states)
	}
	if states["2.2.2.2:443"] != StateActive {
		t.Fatalf("活候选应 Active: %v", states)
	}
	// 用户连接立即拿到活的
	p, _ := sc.Pick("github.com")
	if p != "2.2.2.2:443" {
		t.Fatalf("Pick = %s, want 2.2.2.2:443", p)
	}
}

func TestProbeBest(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})
	p, _ := sc.Pick("github.com")
	sc.Report("github.com", p, 100*time.Millisecond, true)

	probes := 0
	sc.Dial = func(addr string, timeout time.Duration) error {
		probes++
		return nil
	}
	if n := sc.ProbeBest("github.com", 5); n != 1 {
		t.Fatalf("ProbeBest = %d, want 1", n)
	}
	if probes != 1 {
		t.Fatalf("拨号次数 = %d", probes)
	}
	// 探测成功也是样本（EWMA 混新样本）
	snap := sc.Snapshot("github.com")
	if snap[0].Samples < 2 {
		t.Fatalf("主动探测应计入样本: %+v", snap[0])
	}
}

// --- Selector 适配（事件翻译 + 握手死启发式） ---

func TestSelectorSSH443IsPassThrough(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("ssh.github.com", []string{"1.2.3.4:443"})
	sel := NewSelector(sc, rules.New(rules.DefaultDomains))
	if addr, accel := sel.Select("ssh.github.com"); accel || addr != "" {
		t.Fatalf("SSH 443 在 M1 应放行而非 HTTPS 加速: %q %v", addr, accel)
	}
}

func TestSelectorEventTranslation(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})
	m := rules.New(rules.DefaultDomains)
	sel := NewSelector(sc, m)

	// 规则命中 + 池有候选
	addr, accel := sel.Select("github.com")
	if !accel || addr != "1.1.1.1:443" {
		t.Fatalf("Select = %q %v", addr, accel)
	}
	// 非加速域名放行
	if _, accel := sel.Select("example.test"); accel {
		t.Fatal("非加速域名应放行")
	}

	// dial 成功 + 正常流量 → 成功信号
	sel.ReportEvent(proxy.Event{Host: "github.com", Target: addr, Accel: true, DialMS: 120, Rx: 5000, Tx: 800})
	if s := sc.Snapshot("github.com"); s[0].State != StateActive {
		t.Fatalf("正常事件应置 Active: %+v", s[0])
	}

	// dial 失败 → 熔断
	sel.ReportEvent(proxy.Event{Host: "github.com", Target: addr, Accel: true, DialErr: errDial()})
	if s := sc.Snapshot("github.com"); s[0].State != StateCooldown {
		t.Fatalf("DialErr 应熔断: %+v", s[0])
	}
}

func TestSelectorHandshakeDeadHeuristic(t *testing.T) {
	// TCP 通但 TLS 死：dial ok + 存活 <2s + 流量 <512B + copy 错误
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})
	sel := NewSelector(sc, rules.New(rules.DefaultDomains))

	p, _ := sc.Pick("github.com")
	sc.Report("github.com", p, 100*time.Millisecond, true) // 先 Active

	sel.ReportEvent(proxy.Event{
		Host: "github.com", Target: p, Accel: true, DialMS: 50,
		Rx: 0, Tx: 517, CopyErr: "splice: connection reset by peer", Duration: 300,
	})
	if s := sc.Snapshot("github.com"); s[0].State != StateCooldown {
		t.Fatalf("握手死特征应熔断: %+v", s[0])
	}

}

func TestSelectorNormalBrowsingNotPunished(t *testing.T) {
	// 客户端看完页面主动断（copy reset + 流量大）→ 不惩罚
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443"})
	sel := NewSelector(sc, rules.New(rules.DefaultDomains))
	p, _ := sc.Pick("github.com")
	sc.Report("github.com", p, 100*time.Millisecond, true)

	sel.ReportEvent(proxy.Event{
		Host: "github.com", Target: p, Accel: true, DialMS: 50,
		Rx: 580000, Tx: 12000, CopyErr: "connection reset by peer", Duration: 8000,
	})
	if s := sc.Snapshot("github.com"); s[0].State != StateActive {
		t.Fatalf("正常浏览断开不应惩罚: %+v", s[0])
	}
}

type dialErr struct{}

func (dialErr) Error() string { return "dial tcp: connection refused" }
func errDial() error          { return dialErr{} }

// --- 并发安全（go test -race，CI Linux 矩阵）---

func TestPreflightUsesDialHost(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443"})
	var hostCalls, tcpCalls int
	sc.Dial = func(string, time.Duration) error {
		tcpCalls++
		return nil
	}
	sc.DialHost = func(host, addr string, _ time.Duration) error {
		if host != "github.com" || addr == "" {
			t.Fatalf("DialHost 参数异常: %q %q", host, addr)
		}
		hostCalls++
		return nil
	}
	if got := sc.Preflight("github.com"); got != 2 {
		t.Fatalf("Preflight = %d", got)
	}
	if hostCalls != 2 || tcpCalls != 0 {
		t.Fatalf("应优先使用 DialHost: host=%d tcp=%d", hostCalls, tcpCalls)
	}
}

func TestSchedulerConcurrent(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443", "3.3.3.3:443"})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p, ok := sc.Pick("github.com")
				if ok {
					if j%3 == 0 {
						sc.Report("github.com", p, 80*time.Millisecond, false)
					} else {
						sc.Report("github.com", p, 80*time.Millisecond, true)
					}
				}
			}
			sc.Snapshot("github.com")
		}(i)
	}
	wg.Wait()
}

var _ net.Addr // 保持 net import（Event 用到 proxy 包）
