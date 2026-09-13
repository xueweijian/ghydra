package rules

// Refresher（M3-W2 phase 3，方案 docs/GHydra-M3-W2-Phase3-Plan.md §2/§3）：
// 规则拉取管线——调度（30s 首拉/6h 周期/手动）+ 双源 A→B 编排 +
// 尺寸防线 + Apply 结果分类 + 退避状态机 + StateStore 持久化。
//
// 分层：本包不 import store——StateStore 由装配层适配（生产 store.Store，
// 测试内存实现）。拉取 Fetch 亦注入（生产 = get 通道 A/B 的包装，
// 测试 = httptest/内存 stub），「读超即断」的流式上限由生产包装层保证。
//
// 安全语义（对应 TUF 攻击分类）：
//   A3 回滚 / A4 快进  → Apply 的版本防线，拒绝路径零副作用
//   A5 无尽数据        → fetchOne 每文件硬上限（json 1MiB / sig 64KiB）
//   A1/A2/A8/A9        → Apply 验签+schema
//   A6 冻结            → 拉取失败不触碰现有快照（旧规则好过没规则），
//                        失败可观测（LastResult/Backoff + doctor）
//   seen_max 双保险    → 成功路径推进 StateStore（独立于磁盘规则文件，
//                        A7 磁盘损坏重启后防线不回落）

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrBusy 单飞闸门：已有刷新进行中。
var ErrBusy = errors.New("rules: refresh already in flight")

// 结果分类（LastResult / /api/rules / doctor 用）。
const (
	ResOK             = "ok"
	ResFetchErr       = "fetch_err"
	ResSigRejected    = "sig_rejected"    // A1/A2/A7：验签不过
	ResRollback       = "rollback"        // A3
	ResFastForward    = "fast_forward"    // A4（含 schema cap 转写归因）
	ResSchemaRejected = "schema_rejected" // A8/A9
	ResSizeExceeded   = "size_exceeded"   // A5
)

// isRejectClass 内容侧拒绝（区别于网络失败）：进 rejects 计数。
func isRejectClass(res string) bool {
	switch res {
	case ResSigRejected, ResRollback, ResFastForward, ResSchemaRejected, ResSizeExceeded:
		return true
	}
	return false
}

// DefaultRefresh 调度默认值（方案 §3 拍板冻结）。
const (
	DefaultFirstDelay = 30 * time.Second
	DefaultInterval   = 6 * time.Hour
	DefaultMaxJSON    = 1 << 20 // 1 MiB
	DefaultFetchWait  = 30 * time.Second
	defaultBackoffLen = 3
)

// defaultBackoff 1h → 4h → 24h 封顶（拷贝防外部修改）。
func defaultBackoff() []time.Duration {
	return []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}
}

// Fetch 一次 URL 拉取。生产实现必须尊重 ctx（超时/取消即断）。
type Fetch func(ctx context.Context, url string) ([]byte, error)

// RefreshState 拉取管线状态（不含 seen_max——后者走 StateStore 独立方法，
// 只增不减的防线锚点与可观测计数生命周期不同）。
type RefreshState struct {
	LastAttemptAt       time.Time
	LastOKAt            time.Time
	BackoffUntil        time.Time
	LastResult          string
	LastDetail          string
	ConsecutiveFailures int
	Attempts            int
	Successes           int
	Rejects             int
}

// StateStore rules_state 持久化的最小接口（生产由 store.Store 适配）。
type StateStore interface {
	SeenMax() (int64, bool, error)
	SetSeenMax(v int64) error
	LoadRefreshState() (RefreshState, bool, error)
	SaveRefreshState(RefreshState) error
}

// RefresherConfig 调度参数。零值字段用默认值。
type RefresherConfig struct {
	FirstDelay   time.Duration   // 启动首拉延迟（默认 30s）
	Interval     time.Duration   // 周期（默认 6h）
	MaxJSONBytes int64           // json 上限（默认 1MiB；sig 恒 64KiB）
	FetchWait    time.Duration   // 单次 fetch 超时（默认 30s）
	BackoffSteps []time.Duration // 退避序列（默认 1h/4h/24h）
	Clock        func() time.Time
}

// Refresher 规则拉取管线。NewRefresher 构造；Start/Stop 管生命周期；
// Trigger/TriggerSync 手动触发（单飞）。
type Refresher struct {
	prov    *Provider
	cfg     RefresherConfig
	fetches [2]Fetch // [A, B]；nil 跳过该源
	urls    [2]string
	st      StateStore // 可 nil（纯内存模式）

	logf func(string, ...any)

	mu      sync.Mutex
	running bool
	state   RefreshState
	onApply []func(*Snapshot)

	started, stopped bool
	stopCh           chan struct{}
	doneCh           chan struct{}

	// afterFn 可注入等待（测试；生产 time.After）。
	afterFn func(d time.Duration) <-chan time.Time
}

// NewRefresher 构造。fetchB/urlB 可为 nil/空（单源模式）；st 可 nil。
func NewRefresher(prov *Provider, cfg RefresherConfig, fetchA, fetchB Fetch, jsonURLA, jsonURLB string, st StateStore) *Refresher {
	if cfg.FirstDelay <= 0 {
		cfg.FirstDelay = DefaultFirstDelay
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxJSONBytes <= 0 {
		cfg.MaxJSONBytes = DefaultMaxJSON
	}
	if cfg.FetchWait <= 0 {
		cfg.FetchWait = DefaultFetchWait
	}
	if len(cfg.BackoffSteps) == 0 {
		cfg.BackoffSteps = defaultBackoff()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	r := &Refresher{
		prov:    prov,
		cfg:     cfg,
		st:      st,
		logf:    func(string, ...any) {},
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		afterFn: time.After,
	}
	r.fetches[0], r.fetches[1] = fetchA, fetchB
	r.urls[0], r.urls[1] = jsonURLA, jsonURLB
	return r
}

// SetLogf 装配日志钩子（必须在 Start 前调用）。
func (r *Refresher) SetLogf(f func(string, ...any)) { r.logf = f }

// OnApply 注册 Apply 成功回调（装配层：调度器种子热重建 + cdn 回落刷新）。
// 回调在刷新 goroutine 内同步执行，异常自行兜底。
func (r *Refresher) OnApply(fn func(*Snapshot)) {
	r.mu.Lock()
	r.onApply = append(r.onApply, fn)
	r.mu.Unlock()
}

// Start 启动周期循环（幂等；Stop 后不可重启——生命周期与 serve 一致）。
// 启动时从 StateStore 恢复上次状态（含退避——R2b 跨重启语义）。
func (r *Refresher) Start() {
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()

	if r.st != nil {
		if row, ok, err := r.st.LoadRefreshState(); err == nil && ok {
			r.mu.Lock()
			r.state = row
			r.mu.Unlock()
		} else if err != nil {
			r.logf("rules: restore state: %v", err)
		}
	}
	go r.loop()
}

// Stop 停止循环并等待退出（已停止为 no-op）。
func (r *Refresher) Stop() {
	r.mu.Lock()
	if !r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	r.mu.Unlock()
	close(r.stopCh)
	<-r.doneCh
}

// Trigger 手动触发（异步）。进行中返回 ErrBusy。退避不拦手动（用户
// 意图至上，R2c）。
func (r *Refresher) Trigger() (string, error) {
	if !r.tryEnter() {
		return "", ErrBusy
	}
	id := r.refreshID()
	go func() {
		defer r.leave()
		r.runOnce("manual")
	}()
	return id, nil
}

// TriggerSync 同步触发（doctor / 测试）。err 仅 ErrBusy；结果分类在
// 返回值，原始错误摘要在 State().LastDetail。
func (r *Refresher) TriggerSync() (string, error) {
	if !r.tryEnter() {
		return "", ErrBusy
	}
	defer r.leave()
	return r.runOnce("manual"), nil
}

// State 当前状态快照（拷贝）。
func (r *Refresher) State() RefreshState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// InFlight 刷新是否进行中（API dto）。
func (r *Refresher) InFlight() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// SnapshotVersion 当前生效版本（测试/诊断便利）。
func (r *Refresher) SnapshotVersion() int64 { return r.prov.Snapshot().Version }

// NextAt 下次自动拉取时刻（含退避推迟；未启动 = 零值）。
func (r *Refresher) NextAt() time.Time {
	r.mu.Lock()
	started, stopped := r.started, r.stopped
	r.mu.Unlock()
	if !started || stopped {
		return time.Time{}
	}
	now := r.clock()
	return now.Add(r.nextDelay(now))
}

func (r *Refresher) clock() time.Time { return r.cfg.Clock() }

func (r *Refresher) tryEnter() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *Refresher) leave() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

func (r *Refresher) refreshID() string {
	return "refresh-" + r.clock().UTC().Format("20060102-150405")
}

// nextDelay 下一次自动拉取的等待时长：退避未到期则等剩余，否则周期。
func (r *Refresher) nextDelay(now time.Time) time.Duration {
	if bd := r.backoffRemaining(now); bd > 0 {
		return bd
	}
	return r.cfg.Interval
}

// backoffRemaining 恢复的退避剩余（无退避 = 0）。仅用于首拉推迟，
// 不含周期 Interval（R2b：首拉等退避，而非把 Interval 当退避）。
func (r *Refresher) backoffRemaining(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.state.BackoffUntil.IsZero() && r.state.BackoffUntil.After(now) {
		return r.state.BackoffUntil.Sub(now)
	}
	return 0
}

// loop 周期主循环：首拉（尊重恢复的退避）→ 周期（退避优先于 Interval）。
func (r *Refresher) loop() {
	defer close(r.doneCh)

	d := r.cfg.FirstDelay
	if bd := r.backoffRemaining(r.clock()); bd > d {
		d = bd
	}
	select {
	case <-r.stopCh:
		return
	case <-r.afterFn(d):
	}
	r.runOnce("startup")

	for {
		select {
		case <-r.stopCh:
			return
		case <-r.afterFn(r.nextDelay(r.clock())):
		}
		r.runOnce("periodic")
	}
}

// runOnce 一次完整刷新尝试。串行（tryEnter 保证）。
func (r *Refresher) runOnce(trigger string) string {
	now := r.clock()
	r.mu.Lock()
	r.state.Attempts++
	r.state.LastAttemptAt = now
	r.mu.Unlock()

	data, sig, res, detail := r.pull()
	if res == "" && detail == "" { // pull 成功 → Apply
		snap, err := r.prov.Apply(data, sig)
		if err == nil {
			res = ResOK
			if r.st != nil {
				if err := r.st.SetSeenMax(snap.Version); err != nil {
					r.logf("rules: persist seen_max: %v", err)
				}
			}
			r.fireOnApply(snap)
		} else {
			res = classify(err)
			detail = err.Error()
		}
	}
	return r.finish(trigger, res, detail)
}

// finish 状态收尾：计数/退避/持久化。返回结果分类。
func (r *Refresher) finish(trigger, res, detail string) string {
	now := r.clock()
	r.mu.Lock()
	st := &r.state
	st.LastResult = res
	st.LastDetail = truncate(detail, 512)
	if res == ResOK {
		st.ConsecutiveFailures = 0
		st.BackoffUntil = time.Time{}
		st.LastOKAt = now
		st.Successes++
	} else {
		if isRejectClass(res) {
			st.Rejects++
		}
		st.ConsecutiveFailures++
		idx := st.ConsecutiveFailures - 1
		if idx >= len(r.cfg.BackoffSteps) {
			idx = len(r.cfg.BackoffSteps) - 1
		}
		st.BackoffUntil = now.Add(r.cfg.BackoffSteps[idx])
	}
	row := r.state
	r.mu.Unlock()

	if r.st != nil {
		if err := r.st.SaveRefreshState(row); err != nil {
			r.logf("rules: persist refresh state: %v", err)
		}
	}
	r.logf("rules: refresh(%s) -> %s %s", trigger, res, detail)
	return res
}

func (r *Refresher) fireOnApply(snap *Snapshot) {
	r.mu.Lock()
	fns := append([]func(*Snapshot){}, r.onApply...)
	r.mu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.logf("rules: onApply panic: %v", p)
				}
			}()
			fn(snap)
		}()
	}
}

// pull 双源编排（R5/R6）：单源内 json+sig 成套（防跨源错配），任一环
// 失败换下一源；全失败返回主源（A）的失败类别（主源代表整体故障）。
func (r *Refresher) pull() (data, sig []byte, res, detail string) {
	var aErr error
	for i, f := range r.fetches {
		if f == nil {
			continue
		}
		base := r.urls[i]
		d, err := r.fetchOne(f, base, r.cfg.MaxJSONBytes)
		if err == nil {
			var s []byte
			s, err = r.fetchOne(f, base+".minisig", maxSigBytes)
			if err == nil {
				return d, s, "", ""
			}
		}
		if i == 0 {
			aErr = err
		}
	}
	if aErr == nil {
		return nil, nil, ResFetchErr, "no source configured"
	}
	res = ResFetchErr
	if errors.Is(aErr, ErrTooLarge) {
		res = ResSizeExceeded
	}
	return nil, nil, res, aErr.Error()
}

// fetchOne 单文件拉取 + 硬尺寸上限（A5：读超即断——生产 Fetch 包装层
// 在流式读取阶段限长，此处 len 复核为语义兜底）。
func (r *Refresher) fetchOne(f Fetch, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.FetchWait)
	defer cancel()
	b, err := f(ctx, url)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %d bytes > limit %d (%s)", ErrTooLarge, len(b), limit, url)
	}
	return b, nil
}

// classify Apply 错误 → 结果分类。
func classify(err error) string {
	switch {
	case errors.Is(err, ErrRollback):
		return ResRollback
	case errors.Is(err, ErrFastForward):
		return ResFastForward
	case errors.Is(err, ErrSigFormat), errors.Is(err, ErrSigAlgo),
		errors.Is(err, ErrSigKeyID), errors.Is(err, ErrSigVerify):
		return ResSigRejected
	default:
		return ResSchemaRejected
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
