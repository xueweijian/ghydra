package rules

import (
	"context"
	"testing"
	"time"
)

// SetSeenMax（phase 3）：持久化恢复入口。只增不减；恢复后 A3 回滚
// 防线跨磁盘损坏存活（R9 的 Provider 层语义）。
func TestProviderSetSeenMax(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)

	if p.SeenMax() != 1 {
		t.Fatalf("initial seenMax=%d, want 1 (embedded)", p.SeenMax())
	}
	p.SetSeenMax(15)
	if p.SeenMax() != 15 {
		t.Fatalf("seenMax=%d, want 15", p.SeenMax())
	}
	p.SetSeenMax(10) // 回退尝试：静默忽略
	if p.SeenMax() != 15 {
		t.Fatalf("seenMax regressed to %d, want 15", p.SeenMax())
	}

	// 恢复 seenMax=15 后：喂旧合法版 v12（签名合法、从未落盘）→ rollback 拒
	gen, exp := testTimes()
	data := mustRulesJSON(t, 12, nil, gen, exp)
	sig := s.signFile(data, "v12")
	if _, err := p.Apply(data, sig); err == nil {
		t.Fatal("Apply v12 after SetSeenMax(15) should reject")
	} else if !errorsIs(err, ErrRollback) {
		t.Fatalf("err = %v, want rollback", err)
	}
	// 快照不变（拒绝即无副作用）
	if p.Snapshot().Version != 1 {
		t.Fatalf("version = %d, snapshot must stay frozen", p.Snapshot().Version)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- refresher 测试基建（R1–R6） ---

// fakeStateStore StateStore 内存实现。
type fakeStateStore struct {
	seenMax int64
	hasSeen bool
	seenLog []int64 // SetSeenMax 调用序（断言推进时机）
	st      RefreshState
	stSet   int
}

func (f *fakeStateStore) SeenMax() (int64, bool, error) { return f.seenMax, f.hasSeen, nil }
func (f *fakeStateStore) SetSeenMax(v int64) error {
	f.seenMax = v
	f.hasSeen = true
	f.seenLog = append(f.seenLog, v)
	return nil
}
func (f *fakeStateStore) LoadRefreshState() (RefreshState, bool, error) {
	return f.st, f.stSet > 0, nil
}
func (f *fakeStateStore) SaveRefreshState(st RefreshState) error {
	f.st = st
	f.stSet++
	return nil
}

// fakeClock 可控时钟。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeTimer 记录 after 请求；前 immediate 次立即触发，之后挂起
// （防测试循环失控；由 Stop 收尾）。
type fakeTimer struct {
	immediate int
	delays    []time.Duration
}

func (f *fakeTimer) after(d time.Duration) <-chan time.Time {
	f.delays = append(f.delays, d)
	if len(f.delays) <= f.immediate {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		close(ch)
		return ch
	}
	return make(chan time.Time) // 永不触发
}

// refTestHarness 一套可注入的 refresher 测试装置。
type refTestHarness struct {
	r        *Refresher
	prov     *Provider
	signer   *testSigner
	st       *fakeStateStore
	clk      *fakeClock
	tm       *fakeTimer
	calls    []string // fetch 调用序（"A:json"/"B:sig"/...）
	fetchAFn func(url string) ([]byte, error)
	fetchBFn func(url string) ([]byte, error)
}

// newHarness 建可写 provider（测试密钥）+ refresher（fetchA/fetchB 待测试赋值）。
func newHarness(t *testing.T, cfg RefresherConfig) *refTestHarness {
	t.Helper()
	dir := t.TempDir()
	s := newTestSigner()
	prov, _ := newTestProvider(t, dir, s)
	st := &fakeStateStore{}
	clk := &fakeClock{now: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
	tm := &fakeTimer{immediate: 2}
	h := &refTestHarness{
		prov: prov, signer: s, st: st, clk: clk, tm: tm,
		fetchAFn: func(url string) ([]byte, error) { return nil, errNoFetch },
		fetchBFn: func(url string) ([]byte, error) { return nil, errNoFetch },
	}
	if cfg.Clock == nil {
		cfg.Clock = clk.Now
	}
	r := NewRefresher(prov, cfg,
		func(ctx context.Context, url string) ([]byte, error) {
			h.calls = append(h.calls, "A:"+suffix(url))
			return h.fetchAFn(url)
		},
		func(ctx context.Context, url string) ([]byte, error) {
			h.calls = append(h.calls, "B:"+suffix(url))
			return h.fetchBFn(url)
		},
		"https://raw.test/rules/current.json",
		"https://b.test/https://raw.test/rules/current.json",
		st)
	r.afterFn = tm.after
	h.r = r
	return h
}

// serve 返回一个恒定伺服 data 的 fetch 函数（json 与 sig 同体测试用）。
func serve(data []byte) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return data, nil }
}

type errNoFetchT struct{}

func (errNoFetchT) Error() string { return "no fetch configured" }

var errNoFetch = errNoFetchT{}
