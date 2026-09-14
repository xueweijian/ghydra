// Package supervisor —— GUI 壳的 daemon 监督内核（W4p2 P4c，D1 hybrid）。
//
// 职责（零 wails 依赖，headless 可测；internal/guiapp 只做托盘/窗口薄接线）：
//   - 启动保活：壳启动探活 daemon（serve.json 端口 + /api/status），
//     未运行 → spawn `serve --managed`（D1 步骤 2，W4p2 P1 未实现此处补齐）
//   - 更新继任：watch daemon 死亡 + update.json pending → spawn 继任
//     （两段式第二段：新进程 BootHook 自检→Confirm，W4p2 p4b 契约）
//   - 无 pending 的死亡绝不复活：用户 off / 前台退出不归壳管；崩溃
//     恢复是 ensureReconcile 的职责（任何 CLI 入口触发）
//
// spawn 三课继承（F5 实证，daemon.go/spawn_unix.go 同源教训）：
//  1. exe 路径必须调用方（guiapp.Run 入口）预捕获——交换后 os.Executable()
//     在 Linux 指向 .old，会拉起旧版二进制；
//  2. 子进程日志 append 落盘（detached 子进程默认 /dev/null）；
//  3. fd 必须在 Start() 之后才关（提前关 = fork/exec 拿到已关闭 fd）。
package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/selfupdate"
)

// Action 单轮决策。
type Action int

const (
	ActionNone  Action = iota // 观察 / 不动作
	ActionSpawn               // 拉起（启动保活或更新继任）
)

// Decide 决策内核（纯函数，表驱动测试锁定）。
//
//   - startup && !alive → Spawn（启动保活，无论 pending）
//   - !startup && !alive && pending != "" → Spawn（更新继任）
//   - 其余 → None：alive 正常观察；非启动期死亡且无 pending 绝不复活
//     （off 保护——用户刚关的 daemon 被壳悄悄拉回来是产品级 bug）
func Decide(startup, alive bool, pending string) Action {
	if alive {
		return ActionNone
	}
	if startup || pending != "" {
		return ActionSpawn
	}
	return ActionNone
}

// State 阶段快照（OnState 载荷；托盘 tooltip / webview 注入的数据源）。
type State struct {
	Phase   string // starting|running|pending|restarting|updated|stopped|failed
	Version string // 探活到的 daemon 版本（空 = 未知）
	Pending string // update.json 待生效版本（空 = 无）
}

// Config 装配（零值字段用默认值；显式注入，零全局）。
type Config struct {
	Dir         string        // 运行态目录（默认 ~/.ghydra；测试注入 t.TempDir()）
	ExePath     string        // 必填：预捕获的自身 exe 路径（装配错误 New 时即爆）
	Interval    time.Duration // 观察周期（0 = 2s）
	HealthyWait time.Duration // 继任探活等待上限（0 = 30s）
	ProbeWait   time.Duration // waitHealthy 轮询间隔（0 = 500ms）
	SpawnArgs   []string      // 附加参数（默认 ["serve","--managed"]；测试注入 helper）
	SpawnEnv    []string      // 附加环境（测试 helper 用；追加在 os.Environ() 后）
	OnState     func(State)
	Logf        func(string, ...any)
}

// Loop 监督循环。probeFn/pendingFn/spawnFn 为内部依赖（New 填文件实现；
// 同包测试白盒覆写注入 fake）。
type Loop struct {
	cfg Config

	probeFn   func() probeResult
	pendingFn func() string
	spawnFn   func(pending string) error
}

type probeResult struct {
	alive   bool
	version string
}

// New 构造（ExePath 空 = panic：预捕获是调用方契约，晚爆不如早爆）。
func New(cfg Config) *Loop {
	if cfg.ExePath == "" {
		panic("supervisor: ExePath 必填（必须在 Run 入口预捕获）")
	}
	if cfg.Dir == "" {
		cfg.Dir = DefaultDir()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.HealthyWait <= 0 {
		cfg.HealthyWait = 30 * time.Second
	}
	if cfg.ProbeWait <= 0 {
		cfg.ProbeWait = 500 * time.Millisecond
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	l := &Loop{cfg: cfg}
	l.probeFn = l.probeFiles
	l.pendingFn = l.pendingFile
	l.spawnFn = l.spawn
	return l
}

// DefaultDir 运行态目录（~/.ghydra；HOME 不可用返回 ""——probe/pending
// 均按文件不存在处理，spawn 时再报错）。
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra")
}

func (l *Loop) logf(f string, a ...any) { l.cfg.Logf("[supervisor] "+f, a...) }

func (l *Loop) emit(s State) State {
	if l.cfg.OnState != nil {
		l.cfg.OnState(s)
	}
	return s
}

// Step 单轮决策 + 编排（Run 每周期调一次；导出供测试直调）。
// startup=true 表示壳启动首轮（保活语义：死也拉，无论 pending）。
func (l *Loop) Step(startup bool) State {
	p := l.probeFn()
	pend := l.pendingFn()

	if p.alive {
		phase := "running"
		if pend != "" {
			phase = "pending" // apply 进行中（downloading…pending_boot）
		}
		return l.emit(State{Phase: phase, Version: p.version, Pending: pend})
	}

	if !startup && pend == "" {
		return l.emit(State{Phase: "stopped", Pending: pend}) // off 保护
	}

	_ = l.emit(State{Phase: "restarting", Pending: pend})
	if err := l.spawnFn(pend); err != nil {
		l.logf("spawn 失败（pending=%q）: %v", pend, err)
		return l.emit(State{Phase: "failed", Pending: pend})
	}
	v, ok := l.waitHealthy()
	if !ok {
		l.logf("继任 %s 内未探活（pending=%q）——放弃本轮，等待下轮再判", l.cfg.HealthyWait, pend)
		return l.emit(State{Phase: "failed", Pending: pend})
	}
	phase := "running"
	if pend != "" {
		phase = "updated" // 继任 BootHook 已 Confirm 新版
	}
	return l.emit(State{Phase: phase, Version: v, Pending: pend})
}

// Run 阻塞监督循环（startup 首轮 + ticker 观察轮；ctx 取消退出）。
func (l *Loop) Run(ctx context.Context) {
	l.Step(true)
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Step(false)
		}
	}
}

// waitHealthy 轮询探活直至存活或 HealthyWait 耗尽。
func (l *Loop) waitHealthy() (string, bool) {
	deadline := time.Now().Add(l.cfg.HealthyWait)
	for {
		if p := l.probeFn(); p.alive {
			return p.version, true
		}
		if !time.Now().Before(deadline) {
			return "", false
		}
		time.Sleep(l.cfg.ProbeWait)
	}
}

// ---- 文件实现（Config.Dir 注入；生产 ~/.ghydra） ----

// serveState serve.json 运行态（与 cmd/ghydra daemonState 同形状——
// 运行态真相归 serve，壳只读）。
type serveState struct {
	PID       int   `json:"pid"`
	Port      int   `json:"port"`
	StartedAt int64 `json:"started_at"`
}

func (l *Loop) loadServeState() *serveState {
	if l.cfg.Dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(l.cfg.Dir, "serve.json"))
	if err != nil {
		return nil
	}
	var st serveState
	if json.Unmarshal(b, &st) != nil || st.Port <= 0 {
		return nil
	}
	return &st
}

// probeFiles 生产探活：serve.json 端口 + GET /api/status（读 api-token；
// 任何 HTTP 响应都算活——401 也说明 daemon 在，token 缺失不误判死亡）。
func (l *Loop) probeFiles() probeResult {
	st := l.loadServeState()
	if st == nil {
		return probeResult{}
	}
	cl := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(st.Port)+"/api/status", nil)
	if err != nil {
		return probeResult{}
	}
	if tok := l.readToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return probeResult{}
	}
	defer resp.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body) // 解析失败也算活
	return probeResult{alive: true, version: body.Version}
}

// readToken 读 api-token（daemon 自建自管；壳只读。空 = 未生成，探活
// 仍发无凭据请求——401 也算活）。
func (l *Loop) readToken() string {
	if l.cfg.Dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(l.cfg.Dir, "api-token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pendingFile update.json 待生效版本（p4b 两段式契约：PendingVersion
// 非空且未 Confirmed；BootHook Confirm 后自动清零）。
func (l *Loop) pendingFile() string {
	if l.cfg.Dir == "" {
		return ""
	}
	st, ok, err := selfupdate.LoadState(filepath.Join(l.cfg.Dir, selfupdate.StateFileName))
	if err != nil || !ok || st == nil {
		return ""
	}
	if st.PendingVersion != "" && !st.Confirmed {
		return st.PendingVersion
	}
	return ""
}

// spawn 拉起托管 serve（继任或启动保活同一路径；--managed 让 serve 端
// 退出 hook 恢复代理 + 端口迁移 + serve.json 自写，见 cmd/ghydra）。
func (l *Loop) spawn(pending string) error {
	args := l.cfg.SpawnArgs
	if args == nil {
		args = []string{"serve", "--managed"}
	}
	var logPath string
	if l.cfg.Dir != "" {
		logPath = filepath.Join(l.cfg.Dir, "serve.log")
	}
	env := append(os.Environ(), l.cfg.SpawnEnv...)
	pid, err := spawnDetached(l.cfg.ExePath, args, env, logPath)
	if err != nil {
		return err
	}
	l.logf("已拉起 pid=%d（exe=%s pending=%q）", pid, l.cfg.ExePath, pending)
	return nil
}
