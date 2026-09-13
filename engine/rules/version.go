package rules

import "fmt"

// 版本判定（M3-W2 设计 §3）：纯函数，表驱动测试锁定全矩阵。
//
// 防线语义：
//   - 回滚投毒（A3）：candidate ≤ seenMax 一律拒绝——seenMax 是本地见过的
//     最大合法版本（含内嵌地板），历史合法文件重放无法复活
//   - 快进 DoS（A4）：candidate > MaxVersion 一律拒绝——攻击者把版本抬到
//     天文数字以永久锁死后续真更新的路径被封死，且判定原因可观测（doctor）

// Decision 是版本判定结果。
type Decision int

const (
	// Accept 通过：可进入后续（schema 已过）落盘与热生效流程。
	Accept Decision = iota
	// RejectRollback 回滚投毒：candidate ≤ seenMax。
	RejectRollback
	// RejectFastForward 快进 DoS：candidate > MaxVersion。
	RejectFastForward
)

// DecideVersion 判定远程候选版本。embedded 是内嵌地板版本（首次启动时
// seenMax 的初值，保证首拉必须严格新于出厂规则，无特例分支）。
func DecideVersion(seenMax, candidate int64) (Decision, string) {
	switch {
	case candidate > MaxVersion:
		return RejectFastForward, fmt.Sprintf("version %d exceeds hard cap %d (fast-forward DoS guard)", candidate, MaxVersion)
	case candidate <= seenMax:
		return RejectRollback, fmt.Sprintf("version %d <= seen max %d (rollback guard)", candidate, seenMax)
	default:
		return Accept, fmt.Sprintf("version %d > seen max %d", candidate, seenMax)
	}
}
