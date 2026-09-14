package supervisor

// supervisor_test.go —— P4c L1：决策内核矩阵 + Step 编排序列（全 fake 闭包，
// 零真实 IO/进程——spawn 集成见 spawn_integration_test.go）。
//
// 决策矩阵是本包的灵魂：无 pending 的死亡绝不复活（用户 off 保护）是
// D1 hybrid 架构的安全底线。

import (
	"errors"
	"testing"
	"time"
)

var errFakeSpawn = errors.New("fake spawn error")

func TestDecide(t *testing.T) {
	cases := []struct {
		name           string
		startup, alive bool
		pending        string
		want           Action
	}{
		{"启动+死+无pending=保活拉起", true, false, "", ActionSpawn},
		{"启动+死+pending=保活拉起", true, false, "2.0.0", ActionSpawn},
		{"启动+活=只观察", true, true, "", ActionNone},
		{"运行+死+无pending=绝不复活(off保护)", false, false, "", ActionNone},
		{"运行+死+pending=更新继任", false, false, "2.0.0", ActionSpawn},
		{"运行+活+pending=只观察(apply 进行中)", false, true, "2.0.0", ActionNone},
	}
	for _, c := range cases {
		if got := Decide(c.startup, c.alive, c.pending); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// newFakeLoop 构造全 fake 依赖的 Loop（白盒覆写内部闭包）。
// 返回 spawns()/states() 访问器——测试断言用。
func newFakeLoop(t *testing.T, dir string) (*Loop, func() int, func() []State) {
	t.Helper()
	spawns := 0
	var states []State
	l := New(Config{
		Dir:         dir,
		ExePath:     "/fake/exe",
		Interval:    time.Hour,
		HealthyWait: 50 * time.Millisecond,
		ProbeWait:   time.Millisecond,
		OnState:     func(s State) { states = append(states, s) },
		Logf:        func(string, ...any) {},
	})
	l.probeFn = func() probeResult { return probeResult{} }
	l.pendingFn = func() string { return "" }
	l.spawnFn = func(string) error { spawns++; return nil }
	return l, func() int { return spawns }, func() []State { return states }
}

func aliveProbe(v string) func() probeResult {
	return func() probeResult { return probeResult{alive: true, version: v} }
}

func TestStepStartupEnsureDaemon(t *testing.T) {
	l, spawns, states := newFakeLoop(t, t.TempDir())
	l.probeFn = aliveProbe("1.0.0") // 首轮探活前死亡，spawn 后立刻活
	first := true
	l.probeFn = func() probeResult {
		if first {
			first = false
			return probeResult{}
		}
		return probeResult{alive: true, version: "1.0.0"}
	}
	st := l.Step(true)
	if spawns() != 1 {
		t.Fatalf("启动保活应 spawn 1 次，got %d", spawns())
	}
	if st.Phase != "running" || st.Version != "1.0.0" {
		t.Fatalf("终态应 running@1.0.0，got %+v", st)
	}
	if len(states()) < 2 || states()[0].Phase != "restarting" {
		t.Fatalf("应先发 restarting 态，got %v", states())
	}
}

func TestStepSuccessorOnPendingDeath(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())
	l.pendingFn = func() string { return "2.0.0" }
	first := true
	l.probeFn = func() probeResult {
		if first {
			first = false
			return probeResult{} // daemon 已退出（pending 落盘后）
		}
		return probeResult{alive: true, version: "2.0.0"}
	}
	st := l.Step(false)
	if spawns() != 1 {
		t.Fatalf("pending+死亡应 spawn 继任 1 次，got %d", spawns())
	}
	if st.Phase != "updated" || st.Version != "2.0.0" {
		t.Fatalf("终态应 updated@2.0.0，got %+v", st)
	}
}

func TestStepOffGuardNoResurrection(t *testing.T) {
	l, spawns, states := newFakeLoop(t, t.TempDir())
	st := l.Step(false) // 运行期死亡、无 pending（用户 off / 前台退出）
	if spawns() != 0 {
		t.Fatal("无 pending 死亡绝不复活（off 保护）")
	}
	if st.Phase != "stopped" {
		t.Fatalf("应报 stopped，got %+v", st)
	}
	if len(states()) != 1 {
		t.Fatalf("stopped 不应有 restarting 前态，got %v", states())
	}
}

func TestStepAlivePhases(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())

	l.probeFn = aliveProbe("1.0.0")
	l.pendingFn = func() string { return "" }
	if st := l.Step(false); st.Phase != "running" {
		t.Fatalf("活+无pending=running，got %+v", st)
	}

	l.pendingFn = func() string { return "2.0.0" }
	if st := l.Step(false); st.Phase != "pending" {
		t.Fatalf("活+pending=pending（apply 进行中），got %+v", st)
	}
	if spawns() != 0 {
		t.Fatal("daemon 活着绝不 spawn")
	}
}

func TestStepGiveUpAfterHealthyWait(t *testing.T) {
	l, spawns, _ := newFakeLoop(t, t.TempDir())
	l.pendingFn = func() string { return "2.0.0" }
	l.probeFn = func() probeResult { return probeResult{} } // 永远起不来
	st := l.Step(false)
	if spawns() != 1 {
		t.Fatalf("单轮 Step 只 spawn 一次（不无限拉起），got %d", spawns())
	}
	if st.Phase != "failed" {
		t.Fatalf("HealthyWait 耗尽应 failed，got %+v", st)
	}
}

func TestStepSpawnErrorFails(t *testing.T) {
	l, _, _ := newFakeLoop(t, t.TempDir())
	l.pendingFn = func() string { return "2.0.0" }
	l.spawnFn = func(string) error { return errFakeSpawn }
	st := l.Step(true)
	if st.Phase != "failed" {
		t.Fatalf("spawn 失败应 failed，got %+v", st)
	}
}
