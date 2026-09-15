package main

// rulesglue.go —— 规则热更新装配辅助（M3-W2 phase 2/3）：
//   - rules.Snapshot/Refresher → api DTO（golden 锁定 schema）
//   - store.Store → rules.StateStore 适配（rules 包不 import store）
//   - get.Fetcher → rules.Fetch 包装（吃自身 A/B 狗粮；读超即断）

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/rules"
	"github.com/xueweijian/ghydra/engine/sched"
	"github.com/xueweijian/ghydra/engine/store"
)

// mapRulesStatus status/SSE status 帧的规则摘要。stale 每次现算
// （随时间漂移，不缓存）。
func mapRulesStatus(p *rules.Provider) api.RulesStatus {
	snap := p.Snapshot()
	return api.RulesStatus{
		Version: snap.Version,
		Source:  string(snap.Source),
		Stale:   snap.StaleNow(time.Now()),
	}
}

// mapRulesSnapshot GET /api/rules 全景快照。ref 可 nil（refresher 未装配）。
func mapRulesSnapshot(p *rules.Provider, ref *rules.Refresher) api.RulesSnapshot {
	snap := p.Snapshot()
	return api.RulesSnapshot{
		Version:      snap.Version,
		Source:       string(snap.Source),
		Stale:        snap.StaleNow(time.Now()),
		GeneratedAt:  snap.GeneratedAt.UTC().Format(time.RFC3339),
		ExpiresAt:    snap.ExpiresAt.UTC().Format(time.RFC3339),
		Domains:      snap.Domains,
		CDNEndpoints: snap.CDNEndpoints,
		SeedIPs:      snap.SeedIPs,
		Refresh:      mapRefreshState(ref),
	}
}

// mapRefreshState 拉取管线状态（RFC3339 UTC，空串 = 从未/无计划）。
func mapRefreshState(ref *rules.Refresher) api.RulesRefreshState {
	if ref == nil {
		return api.RulesRefreshState{}
	}
	st := ref.State()
	out := api.RulesRefreshState{
		LastResult: st.LastResult,
		Running:    ref.InFlight(),
	}
	if !st.LastAttemptAt.IsZero() {
		out.LastAt = st.LastAttemptAt.UTC().Format(time.RFC3339)
	}
	if next := ref.NextAt(); !next.IsZero() {
		out.NextAt = next.UTC().Format(time.RFC3339)
	}
	return out
}

// rulesStateAdapter store.Store → rules.StateStore（装配层单向桥）。
type rulesStateAdapter struct{ st *store.Store }

func (a rulesStateAdapter) SeenMax() (int64, bool, error) {
	row, ok, err := a.st.LoadRulesState()
	return row.SeenMax, ok, err
}
func (a rulesStateAdapter) SetSeenMax(v int64) error { return a.st.SetRulesSeenMax(v) }
func (a rulesStateAdapter) LoadRefreshState() (rules.RefreshState, bool, error) {
	row, ok, err := a.st.LoadRulesState()
	if err != nil || !ok {
		return rules.RefreshState{}, ok, err
	}
	return rules.RefreshState{
		LastAttemptAt:       row.LastAttemptAt,
		LastOKAt:            row.LastOKAt,
		BackoffUntil:        row.BackoffUntil,
		LastResult:          row.LastResult,
		LastDetail:          row.LastDetail,
		ConsecutiveFailures: row.ConsecutiveFailures,
		Attempts:            row.Attempts,
		Successes:           row.Successes,
		Rejects:             row.Rejects,
	}, true, nil
}

// SaveRefreshState 全行 UPSERT 会覆盖 seen_max 列——先读现值写回
// （刷新管线单飞内串行，无并发写；seen_max 的独立推进走 SetSeenMax）。
func (a rulesStateAdapter) SaveRefreshState(st rules.RefreshState) error {
	row, _, err := a.st.LoadRulesState() // 不存在时零值 = seen_max 保持 0
	if err != nil {
		return err
	}
	return a.st.SaveRulesState(store.RulesRefreshRow{
		SeenMax:             row.SeenMax,
		LastAttemptAt:       st.LastAttemptAt,
		LastOKAt:            st.LastOKAt,
		LastResult:          st.LastResult,
		LastDetail:          st.LastDetail,
		BackoffUntil:        st.BackoffUntil,
		ConsecutiveFailures: st.ConsecutiveFailures,
		Attempts:            st.Attempts,
		Successes:           st.Successes,
		Rejects:             st.Rejects,
	})
}

// restoreSeenMax 装配序列：持久化 seen_max → Provider（只增）。
// 磁盘规则损坏回退 L0 后防线不回落（R9 重放窗口堵死）。
func restoreSeenMax(p *rules.Provider, st rules.StateStore) {
	if st == nil {
		return
	}
	if v, ok, err := st.SeenMax(); err == nil && ok {
		p.SetSeenMax(v)
	}
}

// rulesFetchA A 通道 Fetch：IP 择优直连（pick 可 nil = 域名直连），
// 流式限长（读超即断）。
func rulesFetchA(pick func(string) (string, bool)) rules.Fetch {
	af := get.NewAFetcher(pick)
	return limitedFetch(af)
}

// rulesFetchB B 通道 Fetch：CDN 前缀 + raw 路径（cdnFn 运行时热更；
// 前缀空 = 该源禁用）。url 参数忽略——B 源地址 = cdnFn()+"/"+rawPath，
// 由闭包捕获（规则源在 repo 固定路径，热更的是 CDN 前缀）。
func rulesFetchB(cdnFn func() string, rawPath string) rules.Fetch {
	return func(ctx context.Context, _ string) ([]byte, error) {
		prefix := cdnFn()
		if prefix == "" {
			return nil, fmt.Errorf("rules: B source disabled (no cdn)")
		}
		bf := get.NewBFetcher(prefix)
		return limitedFetch(bf)(ctx, rawPath)
	}
}

// limitedFetch 把 get.Fetcher 适配为 rules.Fetch：状态码校验 +
// 流式 1MiB 上限（A5 读超即断；refresher 侧另有 len 复核）。
// body 读取必须持续响应 ctx——慢速流（每秒几字节）不能绕过
// fetchWait 超时（io.ReadAll 无 ctx 感知，禁用）。
func limitedFetch(f get.Fetcher) rules.Fetch {
	return func(ctx context.Context, url string) ([]byte, error) {
		resp, err := f.Get(ctx, url, -1)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("http %s", resp.Status)
		}
		return readLimited(ctx, resp.Body, rules.DefaultMaxJSON)
	}
}

// readLimited 带双重防线的限长读：超 limit 立即断（A5 尺寸）；
// ctx 取消/超时立即断（A5 慢速流变体）。
func readLimited(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return buf.Bytes(), ctx.Err()
		default:
		}
		n, rerr := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if int64(buf.Len()) > limit {
				return nil, fmt.Errorf("%w: stream exceeds %d bytes", rules.ErrTooLarge, limit)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), rerr
		}
	}
}

// rulesSeedRebuild 调度器种子池热重建（拍板 #3 推模型：Apply 成功
// 回调路径 fire 一次）。sc 可 nil（--scheduler off）。
func rulesSeedRebuild(sc *sched.Scheduler, snap *rules.Snapshot) {
	if sc == nil || len(snap.SeedIPs) == 0 {
		return
	}
	total := 0
	for host, ips := range snap.SeedIPs {
		addrs := make([]string, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, net.JoinHostPort(ip, "443"))
		}
		if n := sc.AddCandidates(host, addrs); n > 0 {
			total += n
			// F3：Preflight 并行预筛全部 New 候选（此前 ProbeBest 只探
			// Active，新种子等于未验证直接进轮换——首请求吃死 IP）。
			go sc.Preflight(host)
		}
	}
	if total > 0 {
		log.Printf("[rules] 种子池热重建: +%d 候选（v%d）", total, snap.Version)
	}
}

// schedReport 把 AFetcher 拨号/TLS 结果翻译成调度信号（F3：下载路径
// 失败回灌——死 IP 不再只靠 CONNECT 用户流量出局）。sc nil = no-op。
func schedReport(sc *sched.Scheduler) func(host, addr string, dialMS float64, ok bool) {
	if sc == nil {
		return nil
	}
	return func(host, addr string, dialMS float64, ok bool) {
		sc.Report(host, addr, time.Duration(dialMS*float64(time.Millisecond)), ok)
	}
}
