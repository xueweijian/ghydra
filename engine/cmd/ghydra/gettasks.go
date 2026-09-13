package main

// gettasks.go —— daemon 内下载任务注册表（M3-W1）。
//
// 语义：并发单飞（一次一个下载，第二个 → ErrBusy）；进度经 OnProgress
// 节流推 SSE（250ms）；终态（完成/失败/切道）立即推送；历史 ring 上限
// 32。下载器由 serveCmd 注入工厂（含调度器 pick 与 B 通道装配）。

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/get"
)

const (
	getRecentMax    = 32
	getPushThrottle = 250 * time.Millisecond
)

type taskRegistry struct {
	mu       sync.Mutex
	seq      int
	newDL    func() *get.Downloader
	active   *api.GetTask // nil = 空闲
	cancel   context.CancelFunc
	recent   []api.GetTask // 新→旧
	push     func(api.GetTask)
	lastPush time.Time
}

func newTaskRegistry(newDL func() *get.Downloader, push func(api.GetTask)) *taskRegistry {
	return &taskRegistry{newDL: newDL, push: push}
}

// defaultDst URL basename → ~/Downloads/<name>。
func defaultDst(rawURL string) string {
	name := "download.bin"
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		if b := filepath.Base(u.Path); b != "" && b != "/" && b != "." {
			name = b
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return name
	}
	return filepath.Join(home, "Downloads", name)
}

// start 启动下载任务。ErrBusy = 已有在途任务。
func (r *taskRegistry) start(req api.GetStartReq) (api.GetStartResp, error) {
	if req.URL == "" {
		return api.GetStartResp{}, api.UserError("url required")
	}
	dst := req.Dst
	if dst == "" {
		dst = defaultDst(req.URL)
	}
	if dir := filepath.Dir(dst); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return api.GetStartResp{}, fmt.Errorf("创建目标目录失败: %w", err)
		}
	}

	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		return api.GetStartResp{}, api.ErrBusy
	}
	r.seq++
	task := api.GetTask{
		TaskID:    fmt.Sprintf("get-%d", r.seq),
		URL:       req.URL,
		Dst:       dst,
		Total:     -1,
		StartedAt: time.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.active = &task
	r.cancel = cancel
	r.lastPush = time.Time{} // 首帧立即推
	r.mu.Unlock()

	dl := r.newDL()
	dl.OnProgress = func(got, total int64, ch string) {
		r.update(task.TaskID, func(t *api.GetTask) {
			t.GotBytes, t.Total, t.Channel = got, total, ch
		})
	}

	go func() {
		defer cancel()
		res, err := dl.Get(ctx, req.URL, dst)
		r.finish(task.TaskID, res, err)
	}()

	return api.GetStartResp{TaskID: task.TaskID, Dst: dst}, nil
}

// update 就地更新在途任务并按节流推送（TaskID 不匹配 = 已完结，忽略）。
func (r *taskRegistry) update(taskID string, f func(*api.GetTask)) {
	r.mu.Lock()
	if r.active == nil || r.active.TaskID != taskID {
		r.mu.Unlock()
		return
	}
	f(r.active)
	snap := *r.active
	now := time.Now()
	if r.lastPush.IsZero() || now.Sub(r.lastPush) >= getPushThrottle {
		r.lastPush = now
		if r.push != nil {
			r.push(snap)
		}
	}
	r.mu.Unlock()
}

// finish 终态：立即推 + 移入历史。
func (r *taskRegistry) finish(taskID string, res get.Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil || r.active.TaskID != taskID {
		return
	}
	t := r.active
	t.GotBytes = res.GotBytes
	if res.TotalBytes > 0 {
		t.Total = res.TotalBytes
	}
	t.RateBPS = res.RateBPS
	t.Channel = res.FinalCh
	t.Done = true
	now := time.Now()
	t.EndedAt = &now
	if err != nil {
		t.Error = err.Error()
	}
	if r.push != nil {
		r.push(*t)
	}
	r.recent = append([]api.GetTask{*t}, r.recent...)
	if len(r.recent) > getRecentMax {
		r.recent = r.recent[:getRecentMax]
	}
	r.active = nil
	r.cancel = nil
}

// progress 当前状态查询。
func (r *taskRegistry) progress() api.GetProgressResp {
	r.mu.Lock()
	defer r.mu.Unlock()
	resp := api.GetProgressResp{Recent: append([]api.GetTask{}, r.recent...)}
	if r.active != nil {
		a := *r.active
		resp.Active = &a
	}
	return resp
}
