package supervisor

// restart_test.go —— F12：用户显式重启意图（托盘菜单）的一次性语义。
//
// 契约：Restart() 置一次性 intent；下一观察轮死亡分支按保活语义拉起
// 继任；intent 消费即清零——不破坏 off 保护（无 intent 的死亡绝不复活，
// TestStepOffGuardNoResurrection 保持不变语义）。

import (
	"testing"
	"time"
)

func TestRestartExplicitIntent(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())

	// 前提：运行期死亡 + 无 pending = off 保护（不复活）
	if st := l.Step(false); st.Phase != "stopped" || spawns() != 0 {
		t.Fatalf("off 保护前提: %+v spawns=%d", st, spawns())
	}

	// 用户显式请求重启 → 下一轮拉起继任
	l.Restart()
	first := true
	l.probeFn = func() probeResult {
		if first {
			first = false
			return probeResult{}
		}
		return probeResult{alive: true, version: "1.0.2"}
	}
	st := l.Step(false)
	if spawns() != 1 {
		t.Fatalf("显式重启应拉起继任，spawns=%d", spawns())
	}
	if st.Phase != "running" || st.Version != "1.0.2" {
		t.Fatalf("重启后应 running@1.0.2: %+v", st)
	}

	// intent 一次性：再次死亡回归 off 保护（无 pending 绝不复活）
	l.probeFn = func() probeResult { return probeResult{} }
	if st2 := l.Step(false); st2.Phase != "stopped" || spawns() != 1 {
		t.Fatalf("intent 应一次性消费: %+v spawns=%d", st2, spawns())
	}
}

// intent 与 pending 并存时拉起一次即可（不双 spawn）。
func TestRestartWithPendingSingleSpawn(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())
	l.pendingFn = func() string { return "2.0.0" }
	l.Restart()
	first := true
	l.probeFn = func() probeResult {
		if first {
			first = false
			return probeResult{}
		}
		return probeResult{alive: true, version: "2.0.0"}
	}
	if st := l.Step(false); spawns() != 1 || st.Phase != "updated" {
		t.Fatalf("intent+pending 应单次拉起继任: %+v spawns=%d", st, spawns())
	}
}

// daemon 存活时的 Restart：置 intent（kill 失败与否不阻塞请求），
// 后续死亡仍按 intent 拉起。
func TestRestartWhileAliveDefersToDeath(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())
	l.probeFn = aliveProbe("1.0.1")
	if st := l.Step(false); st.Phase != "running" {
		t.Fatalf("前提 running: %+v", st)
	}
	l.Restart() // 活着（fake 无 serve.json → 无 kill 目标）：仅记 intent
	if spawns() != 0 {
		t.Fatalf("存活时 Restart 不应立即 spawn: %d", spawns())
	}
	// daemon 随后死亡（重启生效路径）→ intent 拉起
	dead := false
	l.probeFn = func() probeResult {
		if dead {
			return probeResult{alive: true, version: "1.0.1"}
		}
		dead = true
		return probeResult{}
	}
	if st := l.Step(false); st.Phase != "running" || spawns() != 1 {
		t.Fatalf("死亡后 intent 应拉起: %+v spawns=%d", st, spawns())
	}
}

var _ = time.Second // 保留 time 导入（fake 配置沿用了 newFakeLoop）
