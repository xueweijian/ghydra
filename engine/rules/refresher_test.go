package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- M3-W2 phase 3 测试计划 §8（R1–R6）：先于实现冻结 ---

// waitFor 条件等待（测试同步原语；超时 fatal）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("waitFor timeout")
}

// suffix 测试用 URL 分类标记：.minisig 结尾 → "sig"，否则 "json"。
func suffix(url string) string {
	if strings.HasSuffix(url, ".minisig") {
		return "sig"
	}
	return "json"
}

// rulesJSON 构造可签名 rules json（不依赖 testing；表驱动场景用）。
func rulesJSON(version int64) []byte {
	gen, exp := testTimes()
	b, err := json.Marshal(RulesFile{
		SchemaVersion: 1,
		Version:       version,
		GeneratedAt:   gen.UTC(),
		ExpiresAt:     exp.UTC(),
		Domains:       []string{"github.com", "*.github.com"},
		CDNEndpoints:  []string{"https://gh.1ciyuan.cn"},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// signPair 用测试密钥产出一对 (json, minisig)。
func signPair(s *testSigner, version int64, mutate func([]byte) []byte) ([]byte, []byte) {
	data := rulesJSON(version)
	if mutate != nil {
		data = mutate(data)
	}
	return data, s.signFile(data, "test v")
}

// servePair 生成按 url 后缀伺服 data/sig 的 fetch 函数。
func servePair(data, sig []byte) func(string) ([]byte, error) {
	return func(url string) ([]byte, error) {
		if strings.HasSuffix(url, ".minisig") {
			return sig, nil
		}
		return data, nil
	}
}

// R1 调度时序：30s 首拉 → 6h 周期；成功推进 seen_max；Stop 收口。
func TestRefresherSchedule(t *testing.T) {
	h := newHarness(t, RefresherConfig{
		FirstDelay: 30 * time.Second,
		Interval:   6 * time.Hour,
	})
	data, sig := signPair(h.signer, 2, nil)
	h.fetchAFn = servePair(data, sig)

	h.r.Start()
	waitFor(t, func() bool { return h.r.State().Attempts >= 2 })
	h.r.Stop()

	if len(h.tm.delays) < 2 {
		t.Fatalf("after called %d times, want >=2", len(h.tm.delays))
	}
	if h.tm.delays[0] != 30*time.Second {
		t.Fatalf("first delay = %v, want 30s", h.tm.delays[0])
	}
	if h.tm.delays[1] != 6*time.Hour {
		t.Fatalf("second delay = %v, want 6h", h.tm.delays[1])
	}
	if got := h.st.seenMax; got != 2 {
		t.Fatalf("seen_max = %d, want 2 (applied v2)", got)
	}
	if h.r.SnapshotVersion() != 2 {
		t.Fatalf("snapshot version = %d, want 2", h.r.SnapshotVersion())
	}

	// Stop 后不再拉取
	n := len(h.calls)
	time.Sleep(30 * time.Millisecond)
	if len(h.calls) != n {
		t.Fatalf("fetches after stop: %d → %d", n, len(h.calls))
	}
}

// R2 退避状态机：1h→4h→24h 封顶；成功清零；持久化保存。
func TestRefresherBackoff(t *testing.T) {
	h := newHarness(t, RefresherConfig{
		BackoffSteps: []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour},
	})

	// 连续失败：fetch 全死
	for i, want := range []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		res, err := h.r.TriggerSync()
		if err != nil || res != ResFetchErr {
			t.Fatalf("run %d: res=%q err=%v", i, res, err)
		}
		if got := h.r.nextDelay(h.clk.now); got != want {
			t.Fatalf("nextDelay after %d failures = %v, want %v", i+1, got, want)
		}
	}
	st := h.r.State()
	if st.ConsecutiveFailures != 4 {
		t.Fatalf("failures = %d, want 4", st.ConsecutiveFailures)
	}
	wantBackoff := h.clk.now.Add(24 * time.Hour)
	if !st.BackoffUntil.Equal(wantBackoff) {
		t.Fatalf("backoff_until = %v, want %v", st.BackoffUntil, wantBackoff)
	}
	if h.st.stSet == 0 || !h.st.st.BackoffUntil.Equal(wantBackoff) {
		t.Fatal("backoff not persisted to state store")
	}
	if st.Rejects != 0 {
		t.Fatalf("rejects = %d, want 0 (fetch_err 是失败不是拒绝)", st.Rejects)
	}

	// 成功清零
	data, sig := signPair(h.signer, 2, nil)
	h.fetchAFn = servePair(data, sig)
	res, err := h.r.TriggerSync()
	if err != nil || res != ResOK {
		t.Fatalf("recovery: res=%q err=%v", res, err)
	}
	st = h.r.State()
	if st.ConsecutiveFailures != 0 || st.Successes != 1 {
		t.Fatalf("after success: failures=%d successes=%d", st.ConsecutiveFailures, st.Successes)
	}
	if !st.BackoffUntil.IsZero() {
		t.Fatalf("backoff should clear, got %v", st.BackoffUntil)
	}
	if !st.LastOKAt.Equal(h.clk.now) {
		t.Fatalf("last_ok_at = %v, want %v", st.LastOKAt, h.clk.now)
	}
}

// R2b 退避跨重启：恢复的 BackoffUntil 推迟首拉（而非 30s firstDelay）。
func TestRefresherBackoffRestore(t *testing.T) {
	h := newHarness(t, RefresherConfig{FirstDelay: 30 * time.Second})
	h.st.st = RefreshState{
		BackoffUntil:        h.clk.now.Add(2 * time.Hour),
		ConsecutiveFailures: 3,
		LastResult:          ResFetchErr,
	}
	h.st.stSet = 1

	data, sig := signPair(h.signer, 2, nil)
	h.fetchAFn = servePair(data, sig)
	h.r.Start()
	waitFor(t, func() bool { return h.r.State().Attempts >= 1 })
	h.r.Stop()

	if h.tm.delays[0] != 2*time.Hour {
		t.Fatalf("restored first delay = %v, want 2h (backoff wins)", h.tm.delays[0])
	}
}

// R2c 退避期内手动触发放行（用户意图至上）。
func TestRefresherManualBeatsBackoff(t *testing.T) {
	h := newHarness(t, RefresherConfig{})
	// 预置退避中
	h.st.st = RefreshState{BackoffUntil: h.clk.now.Add(2 * time.Hour), ConsecutiveFailures: 1}
	h.st.stSet = 1

	data, sig := signPair(h.signer, 2, nil)
	h.fetchAFn = servePair(data, sig)
	res, err := h.r.TriggerSync()
	if err != nil || res != ResOK {
		t.Fatalf("manual during backoff: res=%q err=%v", res, err)
	}
}

// R3 单飞：进行中再触发 → ErrBusy；完成后恢复。
func TestRefresherSingleFlight(t *testing.T) {
	h := newHarness(t, RefresherConfig{})
	data, sig := signPair(h.signer, 2, nil)
	block := make(chan struct{})
	h.fetchAFn = func(url string) ([]byte, error) {
		<-block
		if strings.HasSuffix(url, ".minisig") {
			return sig, nil
		}
		return data, nil
	}

	go h.r.Trigger()
	waitFor(t, func() bool { return h.r.InFlight() })

	if _, err := h.r.Trigger(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second trigger err = %v, want ErrBusy", err)
	}
	close(block)
	waitFor(t, func() bool { return !h.r.InFlight() && h.r.State().Successes == 1 })

	if _, err := h.r.Trigger(); err != nil {
		t.Fatalf("trigger after completion: %v", err)
	}
	h.r.Stop()
}

// R4 结果分类 + R6 尺寸防线：七类逐个退出（表驱动）。
func TestRefresherResultClasses(t *testing.T) {
	cases := []struct {
		name    string
		version int64
		mutate  func([]byte) []byte
		fetch   func(h *refTestHarness, url string) ([]byte, error)
		wantRes string
		wantRej bool // 计入 rejects
	}{
		{
			name: "ok", version: 2,
			wantRes: ResOK,
		},
		{
			name: "fetch_err", version: 2,
			fetch:   func(h *refTestHarness, url string) ([]byte, error) { return nil, errNoFetch },
			wantRes: ResFetchErr,
		},
		{
			name: "size_json", version: 2,
			fetch: func(h *refTestHarness, url string) ([]byte, error) {
				if strings.HasSuffix(url, ".minisig") {
					return []byte("sig"), nil
				}
				return make([]byte, 1<<20+1), nil // > 1MiB
			},
			wantRes: ResSizeExceeded, wantRej: true,
		},
		{
			name: "sig_rejected", version: 2,
			fetch: func(h *refTestHarness, url string) ([]byte, error) {
				if strings.HasSuffix(url, ".minisig") {
					return []byte("corrupted sig"), nil
				}
				return rulesJSON(2), nil
			},
			wantRes: ResSigRejected, wantRej: true,
		},
		{
			name: "rollback", version: 1, // embedded/disk seenMax=1，v1 等值拒
			wantRes: ResRollback, wantRej: true,
		},
		{
			name: "fast_forward", version: 999_999_999,
			wantRes: ResFastForward, wantRej: true,
		},
		{
			name: "schema_rejected", version: 2,
			mutate: func(b []byte) []byte {
				// 未知字段 → DisallowUnknownFields 拒（签名对内容仍合法）
				return append(b[:len(b)-1], []byte(",\"evil\":1}")...)
			},
			wantRes: ResSchemaRejected, wantRej: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, RefresherConfig{})
			data, sig := signPair(h.signer, tc.version, tc.mutate)
			if tc.fetch != nil {
				h.fetchAFn = func(url string) ([]byte, error) { return tc.fetch(h, url) }
				h.fetchBFn = func(url string) ([]byte, error) { return nil, errNoFetch }
			} else {
				h.fetchAFn = servePair(data, sig)
			}

			res, err := h.r.TriggerSync()
			if res != tc.wantRes {
				t.Fatalf("res = %q (err=%v), want %q", res, err, tc.wantRes)
			}
			st := h.r.State()
			if tc.wantRes == ResOK {
				if st.Successes != 1 || len(h.st.seenLog) != 1 {
					t.Fatalf("ok path: successes=%d seenLog=%v", st.Successes, h.st.seenLog)
				}
			} else if tc.wantRej && st.Rejects != 1 {
				t.Fatalf("rejects = %d, want 1", st.Rejects)
			}
			// 快照冻结语义：非 ok 一律不变（embedded v1）
			if tc.wantRes != ResOK && h.r.SnapshotVersion() != 1 {
				t.Fatalf("snapshot version = %d, want frozen 1", h.r.SnapshotVersion())
			}
			if st.LastResult != tc.wantRes {
				t.Fatalf("state.last_result = %q, want %q", st.LastResult, tc.wantRes)
			}
			if st.LastDetail == "" && tc.wantRes != ResOK {
				t.Fatal("rejection must carry detail")
			}
		})
	}
}

// R5 双源编排：A 成功不碰 B；A 死 B 兜底；双死 fetch_err。
func TestRefresherSourceOrder(t *testing.T) {
	t.Run("A first", func(t *testing.T) {
		h := newHarness(t, RefresherConfig{})
		data, sig := signPair(h.signer, 2, nil)
		h.fetchAFn = servePair(data, sig)
		h.fetchBFn = func(url string) ([]byte, error) {
			t.Fatal("B must not be called when A succeeds")
			return nil, nil
		}
		if res, _ := h.r.TriggerSync(); res != ResOK {
			t.Fatalf("res = %q", res)
		}
	})
	t.Run("A dead B serves", func(t *testing.T) {
		h := newHarness(t, RefresherConfig{})
		d2, sg2 := signPair(h.signer, 2, nil)
		h.fetchAFn = func(string) ([]byte, error) { return nil, errNoFetch }
		h.fetchBFn = servePair(d2, sg2)
		if res, _ := h.r.TriggerSync(); res != ResOK {
			t.Fatalf("res = %q", res)
		}
		sawB := false
		for _, c := range h.calls {
			if strings.HasPrefix(c, "B:") {
				sawB = true
			}
		}
		if !sawB {
			t.Fatalf("calls = %v, want B fallback", h.calls)
		}
	})
	t.Run("both dead", func(t *testing.T) {
		h := newHarness(t, RefresherConfig{})
		res, _ := h.r.TriggerSync()
		if res != ResFetchErr {
			t.Fatalf("res=%q", res)
		}
	})
}

// R4 补充：sig 超上限同走 size_exceeded（64KiB 上限）。
func TestRefresherSigSize(t *testing.T) {
	h := newHarness(t, RefresherConfig{})
	data, _ := signPair(h.signer, 2, nil)
	bigSig := make([]byte, maxSigBytes+1)
	h.fetchAFn = func(url string) ([]byte, error) {
		if strings.HasSuffix(url, ".minisig") {
			return bigSig, nil
		}
		return data, nil
	}
	if res, _ := h.r.TriggerSync(); res != ResSizeExceeded {
		t.Fatalf("res = %q, want size_exceeded", res)
	}
}

// OnApply 回调：Apply 成功路径 fire 一次，携带新快照。
func TestRefresherOnApply(t *testing.T) {
	h := newHarness(t, RefresherConfig{})
	data, sig := signPair(h.signer, 2, nil)
	h.fetchAFn = servePair(data, sig)

	var mu sync.Mutex
	var got []int64
	h.r.OnApply(func(s *Snapshot) {
		mu.Lock()
		got = append(got, s.Version)
		mu.Unlock()
	})
	h.r.TriggerSync()
	h.r.TriggerSync() // v3 再来一发？——seenMax=2，v2 等值拒，不 fire
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("OnApply fired %v, want [2]", got)
	}
}
