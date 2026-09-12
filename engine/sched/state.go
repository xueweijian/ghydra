package sched

import "time"

// State 是候选 IP 的生命周期状态（五态，PRD §5 核心机制）。
//
//	New ──轮转分配/主动探测──► Probing ──成功──► Active
//	 │                            │失败              │连接失败
//	 │                            ▼                  ▼
//	 └────（Cooldown 7s 到期重入轮转）◄── Cooldown ──┘
//	                            连续 3 轮 Cooldown ──► Quarantine(5min)
//	                            Quarantine 到期/池枯竭 ──► New（计数清零）
type State uint8

const (
	StateNew        State = iota // 未探测（候选入池的初始态）
	StateProbing                 // 被轮转分配给真实连接当试金石（dev-sidecar v2.2 按需探测）
	StateActive                  // 验证可用，参与择优
	StateCooldown                // 熔断退避（7s），期间不被轮转
	StateQuarantine              // 隔离（5min），仅到期或池枯竭时释放
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "New"
	case StateProbing:
		return "Probing"
	case StateActive:
		return "Active"
	case StateCooldown:
		return "Cooldown"
	case StateQuarantine:
		return "Quarantine"
	default:
		return "Unknown"
	}
}

// rotatable 该 IP 当前是否可被按需探测轮转分配。
// New 直接可分；Probing 未在探测中的可复用（并发洪峰时总比降级 DNS 好，
// dev-sidecar 第二轮语义）；Cooldown/Quarantine 到期视为复活。
func (ip *ipState) rotatable(now time.Time) bool {
	switch ip.state {
	case StateNew:
		return true
	case StateProbing:
		return !ip.probing
	case StateCooldown:
		return !ip.probing && !now.Before(ip.cooldownUntil)
	case StateQuarantine:
		return !ip.probing && !now.Before(ip.quarantineUntil)
	default:
		return false
	}
}
