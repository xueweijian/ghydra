package channel

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock 可手动推进的时钟。
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock               { return &fakeClock{t: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func download() Flow { return Flow{Kind: KindDownload, Host: "github.com"} }
func connect() Flow  { return Flow{Kind: KindCONNECT, Host: "github.com"} }
func git() Flow      { return Flow{Kind: KindGit, Host: "github.com"} }

// --- 路由基础语义（D1） ---

func TestRouteClosedAuto(t *testing.T) {
	r := New(DefaultConfig(), newFakeClock().Now)
	for _, f := range []Flow{download(), connect(), git(), {Kind: KindPlainHTTP}} {
		if got := r.Route(f); got.Channel != Direct || got.Bypassed {
			t.Fatalf("closed 态 %v 应走 A: %+v", f.Kind, got)
		}
	}
}

func TestRouteConnectAlwaysA(t *testing.T) {
	// CONNECT 即使 A Open / override=B 也只能走 A（B 无 CONNECT 语义）
	r := New(DefaultConfig(), newFakeClock().Now)
	r.NotifyDoctor(VerdictSourceFault)
	if got := r.Route(connect()); got.Channel != Direct {
		t.Fatalf("A open 后 CONNECT 仍应走 A: %+v", got)
	}
	r.SetOverride(OverrideForceB)
	if got := r.Route(connect()); got.Channel != Direct || !got.Bypassed {
		t.Fatalf("override=B 时 CONNECT 应走 A 且标记 Bypassed: %+v", got)
	}
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("override=B 时下载应走 B: %+v", got)
	}
}

func TestRouteOverrideForceA(t *testing.T) {
	r := New(DefaultConfig(), newFakeClock().Now)
	r.NotifyDoctor(VerdictSourceFault)
	r.SetOverride(OverrideForceA)
	got := r.Route(download())
	if got.Channel != Direct || !got.Bypassed {
		t.Fatalf("override=A 应强制直连且标记 Bypassed: %+v", got)
	}
}

// --- 状态机全转移（D3） ---

func TestCircuitTripByTraffic(t *testing.T) {
	cfg := DefaultConfig() // 12 窗口、≥8 样本、50% 阈值
	r := New(cfg, newFakeClock().Now)
	// 8 样本 5 失败 = 62.5% ≥ 50% → 熔断
	for i := 0; i < 8; i++ {
		r.Report(download(), Direct, i%8 < 5) // 0-4 ok=true（5 个），5-7 false
	}
	// 注意：i%8<5 → i=0..4 → 5 次 true；i=5,6,7 → 3 次 false。
	// 失败率 3/8=37.5% < 50% —— 不应熔断。再补失败样本：
	r.Report(download(), Direct, false)
	r.Report(download(), Direct, false) // 5/10 = 50% ≥ 阈值
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("失败率过半应熔断切 B: %+v", got)
	}
}

func TestCircuitNoTripBelowMinSamples(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSamples = 8
	r := New(cfg, newFakeClock().Now)
	for i := 0; i < 7; i++ {
		r.Report(download(), Direct, false) // 7 连败但样本不足
	}
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("样本不足 MinSamples 不应熔断: %+v", got)
	}
	r.Report(download(), Direct, false) // 第 8 败 → 100% ≥ 50% → 熔断
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("达到 MinSamples 且失败率 100%% 应熔断: %+v", got)
	}
}

func TestCircuitDoctorTripAndRecover(t *testing.T) {
	clk := newFakeClock()
	cfg := DefaultConfig()
	r := New(cfg, clk.Now)

	// 源头 403 → 立即 Open
	r.NotifyDoctor(VerdictSourceFault)
	if s := r.Snapshot(); s.State != StateOpen || s.TripWhy == "" {
		t.Fatalf("应为 Open 且带原因: %+v", s)
	}
	// 流量走 B，doctor Unclear 不动状态
	r.NotifyDoctor(VerdictUnclear)
	if s := r.Snapshot(); s.State != StateOpen {
		t.Fatalf("Unclear 不应改变状态: %+v", s)
	}
	// 冷却到期 → Route 触发推进 HalfOpen；流量仍 B
	clk.Advance(cfg.OpenCooldown + time.Second)
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("HalfOpen 流量仍应走 B: %+v", got)
	}
	if s := r.Snapshot(); s.State != StateHalfOpen {
		t.Fatalf("冷却到期应 HalfOpen: %+v", s)
	}
	// doctor 探活成功 → Closed，流量回 A
	r.NotifyDoctor(VerdictHealthy)
	if s := r.Snapshot(); s.State != StateClosed {
		t.Fatalf("探活成功应 Closed: %+v", s)
	}
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("恢复后应回 A: %+v", got)
	}
}

func TestCircuitHalfOpenFailureBackoff(t *testing.T) {
	clk := newFakeClock()
	cfg := DefaultConfig()
	r := New(cfg, clk.Now)

	r.NotifyDoctor(VerdictSourceFault)
	clk.Advance(cfg.OpenCooldown + time.Second)
	r.Route(download()) // 推进到 HalfOpen

	// 探活失败 → backoff×2 再回 Open
	r.NotifyDoctor(VerdictNetworkFault)
	s := r.Snapshot()
	if s.State != StateOpen || s.BackoffMult != 1 {
		t.Fatalf("探活失败应 backoff=1 再 Open: %+v", s)
	}
	// 冷却时间翻倍：第一个冷却期内不应回 HalfOpen
	clk.Advance(cfg.OpenCooldown + time.Second)
	if s := r.Snapshot(); s.State != StateOpen {
		t.Fatalf("翻倍冷却期内应保持 Open: %+v", s)
	}
	clk.Advance(cfg.OpenCooldown)
	if s := r.Snapshot(); s.State != StateHalfOpen {
		t.Fatalf("翻倍冷却期满应 HalfOpen: %+v", s)
	}
	// 探活成功 → 全部复位
	r.NotifyDoctor(VerdictHealthy)
	if s := r.Snapshot(); s.State != StateClosed || s.BackoffMult != 0 {
		t.Fatalf("恢复应复位 backoff: %+v", s)
	}
}

func TestOpenNotResetByRepeatFault(t *testing.T) {
	clk := newFakeClock()
	cfg := DefaultConfig()
	r := New(cfg, clk.Now)
	r.NotifyDoctor(VerdictSourceFault)
	first := r.Snapshot().OpenUntil
	clk.Advance(10 * time.Second)
	r.NotifyDoctor(VerdictSourceFault) // 已 Open：不重置冷却
	if got := r.Snapshot().OpenUntil; !got.Equal(first) {
		t.Fatalf("重复故障不应重置冷却: %v → %v", first, got)
	}
}

func TestReportBSkipped(t *testing.T) {
	// B 的成败不进 A 窗口（B 故障 ≠ A 故障）
	r := New(DefaultConfig(), newFakeClock().Now)
	for i := 0; i < 20; i++ {
		r.Report(download(), CDN, false)
	}
	if s := r.Snapshot(); s.Samples != 0 {
		t.Fatalf("B 结果不应计入 A 窗口: %+v", s)
	}
}

// --- 故障注入场景：退出标准① 的 CI 化 ---
// 注入源头 403（doctor 判定）→ 新请求 30s 内自动走 B（实际是即时）。
// PRD 验收口径：注入源头 403 后 30s 内新请求自动降级。

func TestFaultInjection403FailsOverWithin30s(t *testing.T) {
	r := New(DefaultConfig(), time.Now) // 真实时钟测墙钟
	if got := r.Route(git()); got.Channel != Direct {
		t.Fatalf("注入前 git 应走 A: %+v", got)
	}
	// 模拟 doctor 探到源头 403（真实链路：probe 分类 http_4xx(403)
	// + 直连对照组同败 + B 探针活 → VerdictSourceFault）
	r.NotifyDoctor(VerdictSourceFault)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.Route(git()); got.Channel == CDN {
			return // 30s 内降级 ✓
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("注入源头 403 后 30s 内未降级到 B（退出标准①失败）")
}

// 流量路径的降级（无 doctor 信号、纯流量失败触发）。
func TestFaultInjectionTrafficFailover(t *testing.T) {
	cfg := DefaultConfig()
	r := New(cfg, time.Now)
	// 模拟用户在下载：A 上连续失败（源头 403 表现为 HTTP 层失败）
	start := time.Now()
	for i := 0; i < cfg.MinSamples; i++ {
		r.Report(download(), Direct, false)
	}
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("连续失败应熔断: %+v", got)
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("流量降级耗时 %v 超出 30s MTTR", el)
	}
}

// --- 并发安全 ---

func TestRouterConcurrent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSamples = 4
	r := New(cfg, time.Now)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.Route(download())
				r.Report(download(), Direct, g%2 == 0)
				if i%50 == 0 {
					r.NotifyDoctor(Verdict(1 + i%4))
					_ = r.Snapshot()
				}
			}
		}(g)
	}
	wg.Wait()
	// 无死锁无 panic 即通过（-race 由 CI 三平台矩阵保证）
}

func TestKindCanB(t *testing.T) {
	if KindCONNECT.CanB() {
		t.Fatal("CONNECT 不能走 B")
	}
	for _, k := range []Kind{KindGit, KindDownload, KindPlainHTTP} {
		if !k.CanB() {
			t.Fatalf("%v 应可走 B", k)
		}
	}
}

func TestStateString(t *testing.T) {
	for _, c := range []struct {
		s    State
		want string
	}{{StateClosed, "closed"}, {StateOpen, "open"}, {StateHalfOpen, "half-open"}} {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d) = %q want %q", c.s, got, c.want)
		}
	}
}

func ExampleRouter_Route() {
	r := New(DefaultConfig(), nil)
	r.NotifyDoctor(VerdictSourceFault)
	fmt.Println(r.Route(Flow{Kind: KindDownload}).Channel)
	// Output: B
}
