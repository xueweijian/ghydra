package sched

import (
	"testing"
	"time"
)

// F3 竞速拨号的调度器侧契约（TDD 先红）。
//
// 背景（2026-09-15 GUI 第二轮实证）：CONNECT 单发派一个上游、失败
// 即 502 不换 IP；冷启动期死 IP（meta 老段/SNI 阻断）占比高，用户侧
// 失败率 88% 打熔断。修复面：热路径只用已验证候选（PickValidated），
// 未验证候选交给竞速（PickN 批量取 + ReleaseCandidates 归还未拨者）。

func TestPickValidatedOnlyVerified(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"1.1.1.1:443", "2.2.2.2:443"})

	// 全 New：热路径不得返回未验证候选（单发死 IP 的根源）
	if addr, ok := sc.PickValidated("github.com"); ok {
		t.Fatalf("全 New 池 PickValidated 应 false（交给竞速），得 %q", addr)
	}

	// 2.2.2.2 验证成功 → Active → 热路径返回它
	sc.Report("github.com", "2.2.2.2:443", 80*time.Millisecond, true)
	addr, ok := sc.PickValidated("github.com")
	if !ok || addr != "2.2.2.2:443" {
		t.Fatalf("PickValidated = %q,%v; want 2.2.2.2:443", addr, ok)
	}
	// 粘性窗口内重复取仍为它
	if addr2, _ := sc.PickValidated("github.com"); addr2 != "2.2.2.2:443" {
		t.Fatalf("粘性应保持 2.2.2.2:443，得 %q", addr2)
	}
}

func TestPickNExcludesAndPrefersActive(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443", "10.0.0.4:443"})
	sc.Report("github.com", "10.0.0.2:443", 50*time.Millisecond, true)  // Active 快
	sc.Report("github.com", "10.0.0.1:443", 200*time.Millisecond, true) // Active 慢

	got := sc.PickN("github.com", 3, map[string]bool{"10.0.0.4:443": true})
	want := []string{"10.0.0.2:443", "10.0.0.1:443", "10.0.0.3:443"} // Active(score 升序) + New 补位；exclude 跳过
	if len(got) != len(want) {
		t.Fatalf("PickN = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PickN = %v, want %v", got, want)
		}
	}
	// 未验证补位候选应标记 Probing（防并发重复分配）
	for _, ip := range sc.Snapshot("github.com") {
		if ip.Addr == "10.0.0.3:443" && ip.State != StateProbing {
			t.Fatalf("竞速补位候选应 Probing，得 %v", ip.State)
		}
	}
	// Active 候选保持 Active（不因竞速丢择优资格）
	if p, _ := sc.PickValidated("github.com"); p != "10.0.0.2:443" {
		t.Fatalf("Active 不应被竞速降级，PickValidated = %q", p)
	}
}

func TestPickNExhaustedEmpty(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"10.0.0.1:443"})
	got := sc.PickN("github.com", 3, map[string]bool{"10.0.0.1:443": true})
	if len(got) != 0 {
		t.Fatalf("全排除后 PickN 应空，得 %v", got)
	}
	// Resolve 为 nil（未注入）：枯竭触发异步补充应静默无害
	if got2 := sc.PickN("github.com", 3, nil); len(got2) != 1 {
		t.Fatalf("无排除时应取回候选，得 %v", got2)
	}
}

func TestReleaseCandidates(t *testing.T) {
	sc, _ := newTestSched()
	sc.AddCandidates("github.com", []string{"10.0.0.1:443", "10.0.0.2:443"})
	got := sc.PickN("github.com", 2, nil)
	if len(got) != 2 {
		t.Fatalf("PickN = %v, want 2 个", got)
	}
	// 归还第二个（竞速取消/未起跑的候选）
	sc.ReleaseCandidates("github.com", []string{got[1]})
	// 释放者可再次被取（回 New）；未释放者仍占位（exclude 模拟）
	again := sc.PickN("github.com", 2, map[string]bool{got[0]: true})
	if len(again) != 1 || again[0] != got[1] {
		t.Fatalf("释放后应可复取 %q，得 %v", got[1], again)
	}
}

func TestStatsColdWarm(t *testing.T) {
	sc, c := newTestSched()
	sc.AddCandidates("github.com", []string{"10.0.0.1:443"})
	c.Advance(1200 * time.Millisecond)
	sc.Report("github.com", "10.0.0.1:443", 100*time.Millisecond, true)

	st, ok := sc.Stats("github.com")
	if !ok {
		t.Fatal("Stats 应有值")
	}
	if st.ColdFirstMS < 1000 || st.ColdFirstMS > 1500 {
		t.Fatalf("cold_first ≈ 1200ms，得 %.0fms", st.ColdFirstMS)
	}
	if st.WarmP50MS > 150 || st.WarmSamples != 1 {
		t.Fatalf("warm_p50 ≈ 100ms/1 样本，得 %.0fms/%d", st.WarmP50MS, st.WarmSamples)
	}

	// 无池域名
	if _, ok := sc.Stats("nosuch.example"); ok {
		t.Fatal("无池域名 Stats 应 false")
	}
}
