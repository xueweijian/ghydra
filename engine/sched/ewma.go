package sched

import "math"

// ewma 指数加权移动平均。value += alpha·(x−value)。
// alpha=0.3 时约 7 个样本收敛 92%——够快的遗忘、够稳的平滑。
type ewma struct {
	alpha float64
	value float64
	set   bool
}

func (e *ewma) add(x float64) float64 {
	if !e.set {
		e.value, e.set = x, true
		return x
	}
	e.value += e.alpha * (x - e.value)
	return e.value
}

func (e *ewma) val() float64 { return e.value }

// rttStats 维护 RTT 均值与抖动（变异系数）的 EWMA。
// 抖动用「单样本相对偏差 |x−mean|/mean」的 EWMA——比 Welford 方差
// 便宜，且 score 只用于排序，不需要统计严谨。
type rttStats struct {
	mean ewma // RTT 均值（ms）
	cv   ewma // 变异系数（无量纲）
}

func (r *rttStats) observe(ms float64) {
	if ms < 0 {
		ms = 0
	}
	m := r.mean.add(ms)
	if m > 1 { // 均值太小（<1ms，本机假上游）时 CV 无意义，跳过
		dev := math.Abs(ms-m) / m
		if dev > 1 {
			dev = 1 // clamp：单样本极端偏差不爆表
		}
		r.cv.add(dev)
	}
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 || math.IsNaN(x) {
		return 1
	}
	return x
}

// score PRD 公式：0.3·RTT + 0.5·失败率 + 0.2·抖动（越低越好）。
// 权重即价值观：可用性主导（失败率一动就是 0.5 的量级），
// 速度微调（150ms 差距折 0.045）。冷启动无样本返回中庸 0.5，
// 不奖励不惩罚，靠轮转快速积累真实数据。
//
// 失败样本的 RTT 记为探测超时值（见 recordFail）——失败既惩罚
// 可用性也抬高 RTT 项，双通道拉低评分。
func score(rttMS, failRate, cv float64, samples int) float64 {
	if samples == 0 {
		return 0.5
	}
	return 0.3*clamp01(rttMS/1000) + 0.5*clamp01(failRate) + 0.2*clamp01(cv)
}
