package channel

import (
	"testing"

	"github.com/xueweijian/ghydra/engine/probe"
)

// mkReport 构造双列测试报告。scenarios 形如 (ok, status, class) 三元组。
func mkReport(coveredTotal, coveredPassed int, scs ...[3]any) *probe.Report {
	r := &probe.Report{
		Mode:          probe.ModeProxy,
		CoveredTotal:  coveredTotal,
		CoveredPassed: coveredPassed,
	}
	for i, s := range scs {
		ok := s[0].(bool)
		sc := probe.ScenarioReport{
			Scenario: "s" + string(rune('0'+i)),
			OK:       ok,
		}
		if !ok {
			sc.Checks = []probe.Check{{
				OK: false, Status: s[1].(int), Class: probe.Class(s[2].(string)),
			}}
		} else {
			sc.Checks = []probe.Check{{OK: true, Class: probe.ClassOK}}
		}
		r.Scenarios = append(r.Scenarios, sc)
	}
	return r
}

func TestVerdictHealthy(t *testing.T) {
	rep := mkReport(5, 5, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"})
	if v := VerdictFromReports(rep, nil); v != VerdictHealthy {
		t.Fatalf("全过应 Healthy: %v", v)
	}
}

func TestVerdictNoData(t *testing.T) {
	if v := VerdictFromReports(nil, nil); v != VerdictNone {
		t.Fatalf("无数据应 None: %v", v)
	}
	if v := VerdictFromReports(&probe.Report{}, nil); v != VerdictNone {
		t.Fatalf("CoveredTotal=0 应 None: %v", v)
	}
}

func TestVerdictSingleFailureInsufficient(t *testing.T) {
	// 5 场景挂 1 个（哪怕 403）→ 不定性（抖动容错）
	rep := mkReport(5, 4,
		[3]any{true, 0, "ok"},
		[3]any{false, 403, "http_4xx"},
		[3]any{true, 0, "ok"},
		[3]any{true, 0, "ok"},
		[3]any{true, 0, "ok"},
	)
	if v := VerdictFromReports(rep, nil); v != VerdictUnclear {
		t.Fatalf("单场景失败应 Unclear: %v", v)
	}
}

func TestVerdictSource403(t *testing.T) {
	// 2025-04 型：多场景 403，直连对照同样死（同因）
	rep := mkReport(5, 2,
		[3]any{true, 0, "ok"},
		[3]any{false, 403, "http_4xx"},
		[3]any{false, 403, "http_4xx"},
		[3]any{false, 403, "http_4xx"},
		[3]any{true, 0, "ok"},
	)
	direct := mkReport(5, 1,
		[3]any{false, 403, "http_4xx"},
		[3]any{false, 403, "http_4xx"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	if v := VerdictFromReports(rep, direct); v != VerdictSourceFault {
		t.Fatalf("多场景 403 应 SourceFault: %v", v)
	}
}

func TestVerdictControlAliveIsOurBug(t *testing.T) {
	// proxy 全死（TCP 特征）但 direct 对照全活 → ghydra 自身问题，不切道
	rep := mkReport(5, 1,
		[3]any{false, 0, "tcp_block"},
		[3]any{false, 0, "tls_reset"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	direct := mkReport(5, 5, [3]any{true, 0, "ok"})
	if v := VerdictFromReports(rep, direct); v != VerdictUnclear {
		t.Fatalf("对照活应 Unclear（我们的锅）: %v", v)
	}
}

func TestVerdictNetworkFault(t *testing.T) {
	// proxy 与 direct 同死（TCP/TLS/超时）→ 网络级阻断 → 切 B
	rep := mkReport(5, 1,
		[3]any{false, 0, "tcp_block"},
		[3]any{false, 0, "timeout"},
		[3]any{false, 0, "tls_reset"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	direct := mkReport(5, 0,
		[3]any{false, 0, "tcp_block"},
		[3]any{false, 0, "tcp_block"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	if v := VerdictFromReports(rep, direct); v != VerdictNetworkFault {
		t.Fatalf("双侧网络特征应 NetworkFault: %v", v)
	}
	// direct 为 nil（没跑对照）：网络特征也成立
	if v := VerdictFromReports(rep, nil); v != VerdictNetworkFault {
		t.Fatalf("无对照时网络特征应 NetworkFault: %v", v)
	}
}

func TestVerdictSourceDown5xx(t *testing.T) {
	// GitHub 宕机：双侧 5xx → B 同死，不切
	rep := mkReport(5, 1,
		[3]any{false, 503, "http_5xx"},
		[3]any{false, 500, "http_5xx"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	direct := mkReport(5, 0,
		[3]any{false, 503, "http_5xx"},
		[3]any{false, 503, "http_5xx"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	if v := VerdictFromReports(rep, direct); v != VerdictUnclear {
		t.Fatalf("双侧 5xx 应 Unclear（源头宕机）: %v", v)
	}
}

// 端到端：报告 → 判定 → 决策器状态变化 → 路由切换（退出标准①链路）。
func TestSignalToRouteEndToEnd(t *testing.T) {
	r := New(DefaultConfig(), nil)
	if got := r.Route(download()); got.Channel != Direct {
		t.Fatalf("初始应走 A: %+v", got)
	}
	rep := mkReport(5, 1,
		[3]any{false, 403, "http_4xx"},
		[3]any{false, 403, "http_4xx"},
		[3]any{false, 403, "http_4xx"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	direct := mkReport(5, 0,
		[3]any{false, 403, "http_4xx"}, [3]any{false, 403, "http_4xx"},
		[3]any{true, 0, "ok"}, [3]any{true, 0, "ok"}, [3]any{true, 0, "ok"},
	)
	r.NotifyDoctor(VerdictFromReports(rep, direct))
	if got := r.Route(download()); got.Channel != CDN {
		t.Fatalf("源头 403 判定后应切 B: %+v", got)
	}
	// 恢复：doctor 探针全绿
	healthy := mkReport(5, 5, [3]any{true, 0, "ok"})
	r.NotifyDoctor(VerdictHealthy) // 半开下 Healthy 才复位；先到 HalfOpen
	if s := r.Snapshot(); s.State == StateOpen {
		// Open 冷却未到，流量仍 B——符合 D3（doctor 驱动恢复）
		if got := r.Route(download()); got.Channel != CDN {
			t.Fatalf("Open 冷却期内流量应仍走 B: %+v", got)
		}
	}
	_ = healthy // 完整恢复时序由 TestCircuitDoctorTripAndRecover 覆盖
}
