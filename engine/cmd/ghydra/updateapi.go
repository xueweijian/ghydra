package main

// updateapi.go —— P4/D7：serve 内自更新状态机 + /api/update/* 装配。
//
// 两段式架构（设计文档 P4 实施规划）：
// ① daemon 内：Check→下载→验签→swap（ServeMode：pending 留盘不 Confirm）
//   →SSE 推 pending_boot→优雅退出（shutdownCh）；
// ② 继任拉起归 GUI 壳（D1 hybrid：壳是 daemon 的 supervisor）——壳 watch
//   daemon 死亡 + pending → spawn 自身 exe serve --managed → 新进程
//   BootHook 自检→Confirm。CLI 用户走 `ghydra update apply`（外部编排）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/selfupdate"
)

const updateCheckTTL = 5 * time.Minute

// updateRunner serve 内自更新状态机（线程安全；Status 供 /api/update/status
// 与 SSE status 帧同源读取）。
type updateRunner struct {
	updater *selfupdate.Updater
	onDone  func() // apply 成功后的优雅退出钩子（装配层注入 shutdownOnce）

	mu       sync.Mutex
	st       api.UpdateStatus
	checkMu  sync.Mutex
	lastInfo *api.UpdateInfo
	lastAt   time.Time
	applySeq atomic.Int64
	inFlight atomic.Bool
}

// newUpdateRunner 装配（updater 不可用返回 nil——/api/update 503 面）。
// updater 参数显式注入（生产走 newServeUpdater；测试注入 fake——设计
// 「显式注入，零全局」同源）。
func newUpdateRunnerFrom(u *selfupdate.Updater, onDone func()) *updateRunner {
	if u == nil {
		return nil
	}
	u.ServeMode = true // 两段式：绝不编排自己的 Stop/Start
	r := &updateRunner{updater: u, onDone: onDone}
	u.OnPhase = func(phase string) { r.setPhase(phase) }
	return r
}

// newUpdateRunner 生产装配：serve 内构造 updater（失败返回 nil）。
// apiBase/trustHosts：--update-api/--update-trust-host 旗标透传（冒烟/
// 演练用；生产默认空 = 官方源 + 冻结信任域，行为零变化）。
func newUpdateRunner(dbPath, cdn, apiBase string, trustHosts map[string]bool, onDone func(), logf func(string, ...any)) *updateRunner {
	u, err := newSelfupdateUpdater(dbPath, cdn, apiBase, trustHosts)
	if err != nil || u == nil {
		if logf != nil {
			logf("[update] runner 不可用: %v", err)
		}
		return nil
	}
	return newUpdateRunnerFrom(u, onDone)
}

func (r *updateRunner) now() string { return time.Now().UTC().Format(time.RFC3339) }

func (r *updateRunner) setState(state string) {
	r.mu.Lock()
	r.st.State = state
	r.st.UpdatedAt = r.now()
	r.mu.Unlock()
}

func (r *updateRunner) setPhase(phase string) {
	r.mu.Lock()
	r.st.State = phase
	r.st.UpdatedAt = r.now()
	r.mu.Unlock()
}

func (r *updateRunner) setFailed(format string, a ...any) {
	r.mu.Lock()
	r.st.State = "failed"
	r.st.Error = fmt.Sprintf(format, a...)
	r.st.UpdatedAt = r.now()
	r.mu.Unlock()
}

// Status 状态机快照（idle 兜底）。
func (r *updateRunner) Status() api.UpdateStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	if st.State == "" {
		st.State = "idle"
	}
	if st.Current == "" {
		st.Current = Version
	}
	if st.UpdatedAt == "" {
		st.UpdatedAt = r.now()
	}
	return st
}

// Check GET /api/update/check：同步 + 5min 缓存。
func (r *updateRunner) Check() (api.UpdateInfo, error) {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	if r.lastInfo != nil && time.Since(r.lastAt) < updateCheckTTL {
		info := *r.lastInfo
		info.Cached = true
		return info, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r.setState("checking")
	plan, err := r.updater.Check(ctx, selfupdate.SelectOpts{})
	info := api.UpdateInfo{
		Current:   Version,
		CheckedAt: r.now(),
	}
	switch {
	case err == nil:
		info.Latest = plan.Version
		info.HasUpdate = true
		info.Notes = plan.Notes
		info.HTMLURL = plan.HTMLURL
	case err == selfupdate.ErrNoUpdate || err == selfupdate.ErrPrerelease || err == selfupdate.ErrBadVersion:
		info.Latest = Version // 语义「无更新」；细节前端不看
	default:
		r.setState("idle")
		info.Error = err.Error()
		return info, err
	}
	r.lastInfo = &info
	r.lastAt = time.Now()
	r.lastInfo.Cached = false
	r.setState("idle")
	return info, nil
}

// Apply POST /api/update/apply：异步单飞。忙 = api.ErrBusy（→409）。
func (r *updateRunner) Apply() (string, error) {
	if !r.inFlight.CompareAndSwap(false, true) {
		return "", api.ErrBusy
	}
	r.applySeq.Add(1)
	id := fmt.Sprintf("apply-%d", r.applySeq.Load())
	r.mu.Lock()
	r.st = api.UpdateStatus{State: "checking", Current: Version, UpdatedAt: r.now(), Error: ""}
	r.mu.Unlock()
	go func() {
		defer r.inFlight.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		plan, err := r.updater.Check(ctx, selfupdate.SelectOpts{})
		if err != nil {
			r.setFailed("check: %v", err)
			return
		}
		r.mu.Lock()
		r.st.Target = plan.Version
		r.mu.Unlock()
		if err := r.updater.ApplyPlan(ctx, plan); err != nil {
			// v1.0.3 PR3：安装目录不可写翻译成人话（GUI 无控制台，
			// 原始 ErrDirNotWritable 文案不可行动；runas 自动提权仅
			// CLI 路径——面板提权升级留 v1.1.0）。
			if errors.Is(err, selfupdate.ErrDirNotWritable) {
				r.setFailed("安装目录需要管理员权限——请退出面板后以管理员身份重新打开再升级，或从 Release 下载安装包覆盖升级（%v）", err)
				return
			}
			r.setFailed("apply: %v", err)
			return
		}
		// pending_boot：两段式第一段完成
		r.setState("pending_boot")
		if r.onDone != nil {
			r.onDone()
		}
	}()
	return id, nil
}
