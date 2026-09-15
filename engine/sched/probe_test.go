package sched

import (
	"testing"
	"time"
)

// F3：ProbeBest 必须覆盖 New 候选。
//
// 现状 bug（2026-09-15 定位）：ProbeBest 只探 StateActive——last_good
// 恢复（main.go）与 rules 种子入池（rulesglue.go）后的「验证」调用
// 全是空转：候选未验证直接进轮换，首个用户请求单发死 IP。
func TestProbeBestProbesNew(t *testing.T) {
	sc, _ := newTestSched()
	probes := 0
	sc.Dial = func(addr string, timeout time.Duration) error {
		probes++
		return nil // 全活
	}
	sc.AddCandidates("github.com", []string{"10.0.0.1:443", "10.0.0.2:443"})

	n := sc.ProbeBest("github.com", 5)
	if n == 0 {
		t.Fatal("New-only 池 ProbeBest 探测数应 >0（现为空转 bug）")
	}
	if probes != n {
		t.Fatalf("探测拨号 %d 次，报告 %d", probes, n)
	}
	for _, ip := range sc.Snapshot("github.com") {
		if ip.State != StateActive {
			t.Fatalf("%s 应被探测为 Active，得 %v", ip.Addr, ip.State)
		}
	}
}

// topN 截断在含 New 候选时同样生效（探测成本控制）。
func TestProbeBestTopNWithNew(t *testing.T) {
	sc, _ := newTestSched()
	sc.Dial = func(addr string, timeout time.Duration) error { return nil }
	sc.AddCandidates("github.com", []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443"})
	if n := sc.ProbeBest("github.com", 2); n != 2 {
		t.Fatalf("topN=2 应只探 2 个，得 %d", n)
	}
}
