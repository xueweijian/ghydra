package channel

import (
	"testing"
	"time"
)

// --- W3：B 健康信号 + trip 前置检查 ---

// B 探针明确失败 → NetworkFault 不再盲切（W1 真实缺陷的修复验收）。
func TestNotifyDoctorSuppressedWhenBDead(t *testing.T) {
	fc := newFakeClock()
	r := New(DefaultConfig(), fc.Now)
	r.NotifyB(false)
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("B 死时不应切道: %+v", got)
	}
	s := r.Snapshot()
	if s.BSuppressed != 1 {
		t.Errorf("BSuppressed = %d, 想 1", s.BSuppressed)
	}
	if s.BOK == nil || *s.BOK {
		t.Errorf("BOK 应为 false")
	}
	// 多次抑制累计
	r.NotifyDoctor(VerdictNetworkFault)
	if r.Snapshot().BSuppressed != 2 {
		t.Errorf("抑制应累计")
	}
}

// B 活 → 正常切道（抑制只在 B 确定死时生效）。
func TestNotifyDoctorTripsWhenBAlive(t *testing.T) {
	r := New(DefaultConfig(), newFakeClock().Now)
	r.NotifyB(true)
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("B 活时应切道: %+v", got)
	}
}

// B 未知（从未探测）→ 保持 W1 行为（可切）。
func TestNotifyDoctorTripsWhenBUnknown(t *testing.T) {
	r := New(DefaultConfig(), newFakeClock().Now)
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("B 未知时应保持 W1 行为: %+v", got)
	}
	if r.Snapshot().BSuppressed != 0 {
		t.Errorf("未知不产生抑制记录")
	}
}

// B 死信号 TTL 过期 → 回退未知（可切），避免陈旧信号永久卡死切道。
func TestNotifyDoctorSuppressionTTLExpiry(t *testing.T) {
	fc := newFakeClock()
	r := New(DefaultConfig(), fc.Now)
	r.NotifyB(false)
	fc.Advance(6 * time.Minute) // 默认 TTL 5m
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("TTL 过期后应恢复可切: %+v", got)
	}
}

// B 探针恢复活 → 解除抑制（下一轮判定照常）。
func TestNotifyBRecovery(t *testing.T) {
	r := New(DefaultConfig(), newFakeClock().Now)
	r.NotifyB(false)
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("B 死时不应切道")
	}
	r.NotifyB(true)
	r.NotifyDoctor(VerdictNetworkFault)
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("B 恢复后应可切道: %+v", got)
	}
}

// 流量窗口熔断路径同样受 B 健康约束。
func TestWindowTripSuppressedWhenBDead(t *testing.T) {
	fc := newFakeClock()
	cfg := DefaultConfig()
	cfg.MinSamples = 3
	r := New(cfg, fc.Now)
	r.NotifyB(false)
	for i := 0; i < 3; i++ {
		r.Route(download())
		r.Report(download(), Direct, false)
	}
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("流量窗口熔断也应被 B 死抑制: %+v", got)
	}
	if r.Snapshot().BSuppressed != 1 {
		t.Errorf("窗口抑制也应计数, got %d", r.Snapshot().BSuppressed)
	}
}
