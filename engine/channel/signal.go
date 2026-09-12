// signal.go —— doctor 双列报告 → 通道判定（M2-W1，方案 D2/D6）。
//
// 对照组语义（D6「探针带直连对照组」在通道层的应用）：
//   - proxy 列（经 ghydra/通道 A）健康          → A 恢复钥匙（HalfOpen→Closed）
//   - proxy 死 + direct 对照活                  → ghydra 自身的锅 → Unclear（不切道，报警修自己）
//   - 403 特征（HTTP 层到达但被拒）             → 源头级（2025-04 型）→ SourceFault，B 出口海外不受影响
//   - TCP/TLS/DNS/超时特征多场景齐死            → 网络级阻断 → NetworkFault
//   - 5xx 为主（对照也死）                      → GitHub 宕机，B 同死 → Unclear
package channel

import (
	"github.com/xueweijian/ghydra/engine/probe"
)

// minFailedScenarios 单场景失败可能是抖动；≥2 个 covered 场景失败才定性。
const minFailedScenarios = 2

// VerdictFromReports 蒸馏一次 doctor 运行的双列报告。
// proxy 为经 ghydra（通道 A 路径）的探针；direct 为直连对照组，可为 nil。
func VerdictFromReports(proxyRep, directRep *probe.Report) Verdict {
	if proxyRep == nil || proxyRep.CoveredTotal == 0 {
		return VerdictNone // 无数据
	}
	if CoveredHealthy(proxyRep) {
		return VerdictHealthy
	}

	failed := failedCovered(proxyRep)
	if len(failed) < minFailedScenarios {
		return VerdictUnclear // 单场景失败不定性
	}

	// 对照组活 = 网络到 GitHub 是通的，锅在 ghydra 本地 → 不切道
	if directRep != nil && directRep.CoveredTotal > 0 && CoveredHealthy(directRep) {
		return VerdictUnclear
	}

	has403, netFails, srvFails := 0, 0, 0
	for _, c := range failed {
		switch {
		case c.Status == 403:
			has403++
		case c.Class == probe.ClassTCPBlock || c.Class == probe.ClassTLSReset ||
			c.Class == probe.ClassTimeout || c.Class == probe.ClassDNS:
			netFails++
		case c.Class == probe.ClassHTTP5xx:
			srvFails++
		}
	}
	switch {
	case has403 > 0:
		return VerdictSourceFault
	case netFails >= minFailedScenarios:
		return VerdictNetworkFault
	case srvFails >= minFailedScenarios:
		return VerdictUnclear // GitHub 宕机：B 出口同样到不了服务，切道无益
	default:
		return VerdictUnclear // 证据不足
	}
}

// CoveredHealthy 覆盖场景全过（导出：doctor_loop 判定 B 列健康用）。
func CoveredHealthy(r *probe.Report) bool {
	return r.CoveredPassed == r.CoveredTotal && r.CoveredTotal > 0
}

// failedCovered 收集失败场景的代表性 Check（每场景取第一个失败子检查）。
func failedCovered(r *probe.Report) []probe.Check {
	var out []probe.Check
	for _, sc := range r.Scenarios {
		if sc.OK {
			continue
		}
		for _, c := range sc.Checks {
			if !c.OK {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
