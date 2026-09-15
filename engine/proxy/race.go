// race.go —— F3 竞速拨号（冷启动自愈，2026-09-15 实证修复）。
//
// 实证背景：CONNECT 单发派一个上游、失败即 502 不换 IP；冷启动期
// 死 IP（meta 老段 dial 超时 / SNI 阻断 RST）占比高，用户侧失败率
// 88% 打熔断。本文件实现「已验证热路径 + 错峰竞速 + 同域单飞」：
//   - 热路径：粘性/Active 单发（稳态秒级，零额外成本）；
//   - 竞速：无已验证上游时错峰并行拨 2-3 候选（happy-eyeballs 式），
//     首成者服务连接、其余取消；真实失败即时报（下轮排除），
//     被取消/未起跑的候选不惩罚（Release 归还）；
//   - 单飞：同域同时只一组竞速（追随者等一轮，胜者已置 Active+
//     粘性后走热路径）——千并发不放大连接风暴。
package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// RacingSelector 是 UpstreamSelector 的可选竞速能力（F3）。
// 生产实现：sched.Selector；测试可注入 fake。
type RacingSelector interface {
	UpstreamSelector
	// ValidatedPick 热路径：只返回已验证上游（粘性/Active），无则 false。
	ValidatedPick(host string) (string, bool)
	// RacingPick 返回最多 n 个竞速候选（排除 exclude；空 = 池枯竭）。
	RacingPick(host string, n int, exclude map[string]bool) []string
	// RacingReportFail 上报候选真实失败（进 Cooldown，下轮排除）。
	RacingReportFail(host, addr string)
	// RacingReportWin 上报候选成功（拨号 ms 喂 EWMA；胜者/迟到胜者）。
	RacingReportWin(host, addr string, dialMS float64)
	// RacingRelease 归还未实际拨号的候选（取消不惩罚）。
	RacingRelease(host string, addrs []string)
}

// F3 默认参数（方案冻结稿：拨号帽 5s + 竞速宽 3 + 错峰 300ms + 预算 10s）。
const (
	DefaultRaceWidth   = 3                      // 每轮并行候选上限
	DefaultRaceStagger = 300 * time.Millisecond // 候选错峰起跑间隔
	DefaultRaceBudget  = 10 * time.Second       // 单请求竞速总预算（两轮封顶）
)

// raceSlot 同域竞速单飞门：busy 时后来者等待一轮结束。
type raceSlot struct {
	mu   sync.Mutex
	busy bool
	done chan struct{} // 本轮结束（close 广播）
}

// acquireRaceSlot 获取 host 的竞速权。true = 本连接当 scout（持有权，
// 用完必须 releaseRaceSlot）；false = 已有邻轮在跑（等到结束或预算尽，
// 外层重走热路径——scout 胜者已置 Active+粘性）。
func (s *Server) acquireRaceSlot(host string, deadline time.Time) bool {
	s.raceMu.Lock()
	if s.raceSlots == nil {
		s.raceSlots = make(map[string]*raceSlot)
	}
	slot := s.raceSlots[host]
	if slot == nil {
		slot = &raceSlot{}
		s.raceSlots[host] = slot
	}
	s.raceMu.Unlock()

	slot.mu.Lock()
	if !slot.busy {
		slot.busy = true
		slot.done = make(chan struct{})
		slot.mu.Unlock()
		return true
	}
	done := slot.done
	slot.mu.Unlock()

	wait := time.Until(deadline)
	if wait <= 0 {
		return false
	}
	select {
	case <-done:
	case <-time.After(wait):
	}
	return false
}

// releaseRaceSlot 释放竞速权并广播本轮结束。
func (s *Server) releaseRaceSlot(host string) {
	s.raceMu.Lock()
	slot := s.raceSlots[host]
	s.raceMu.Unlock()
	if slot == nil {
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.busy {
		slot.busy = false
		close(slot.done)
	}
}

func (s *Server) raceWidth() int {
	if s.RaceWidth > 0 {
		return s.RaceWidth
	}
	return DefaultRaceWidth
}

func (s *Server) raceStagger() time.Duration {
	if s.RaceStagger > 0 {
		return s.RaceStagger
	}
	return DefaultRaceStagger
}

func (s *Server) raceBudget() time.Duration {
	if s.RaceBudget > 0 {
		return s.RaceBudget
	}
	return DefaultRaceBudget
}

// upstreamResult 一次加速上游获取的完整结果（handle 组装 Event 用）。
type upstreamResult struct {
	conn     net.Conn
	target   string // 实际拨号地址（IP 或降级域名）
	accel    bool
	err      error
	dialMS   float64 // 胜者实际拨号耗时（EWMA 真值；不含竞速等待）
	attempts int     // 实际拨号过的候选数（含热路径与兜底）
}

// connectAccel 加速 CONNECT 的上游获取（F3）。
//
// ① 已验证热路径单发（粘性/Active）；失败即报并进入竞速。
// ② 竞速轮 ≤2、预算内、同域单飞：错峰并行拨多个候选，首成者胜，
//    胜者立即报 Win（置 Active+粘性，追随者走热路径）。
// ③ 兜底：仅当从未竞速（池枯竭）时域名直连（系统 DNS，legacy 语义）；
//    竞速打满仍无出路则快速失败回 502，绝不静默挂 30s。
func (s *Server) connectAccel(rs RacingSelector, host, authority string) upstreamResult {
	exclude := map[string]bool{}
	deadline := time.Now().Add(s.raceBudget())
	raced := false

	for round := 0; round < 2; round++ {
		// ① 热路径（round>0 重试：上一轮胜者可能已置 Active+粘性）。
		if addr, ok := rs.ValidatedPick(host); ok && addr != "" && !exclude[addr] {
			t0 := time.Now()
			c, err := net.DialTimeout("tcp", addr, s.dialTimeout())
			if err == nil {
				return upstreamResult{conn: c, target: addr, accel: true, dialMS: msSince(t0), attempts: 1}
			}
			rs.RacingReportFail(host, addr)
			exclude[addr] = true
			s.logf("[race] %s 热路径失败 %s: %v", host, addr, err)
		}
		if time.Until(deadline) < 2*time.Second && (raced || len(exclude) > 0) {
			break // 预算不足以再竞速一轮
		}
		// ② 竞速轮（单飞门）。
		if !s.acquireRaceSlot(host, deadline) {
			if !time.Now().Before(deadline) {
				break
			}
			round-- // 等到邻轮结束：本轮不消耗（重走热路径）
			continue
		}
		res := s.dialRaceRound(rs, host, exclude)
		if res.candidates > 0 {
			raced = true
		}
		for _, a := range res.fails {
			rs.RacingReportFail(host, a)
			exclude[a] = true
		}
		for _, w := range res.lateWins {
			rs.RacingReportWin(host, w.addr, w.ms)
		}
		rs.RacingRelease(host, res.released)
		if res.conn != nil {
			rs.RacingReportWin(host, res.winner, res.winMS)
			s.releaseRaceSlot(host)
			return upstreamResult{conn: res.conn, target: res.winner, accel: true,
				dialMS: res.winMS, attempts: res.dialed}
		}
		s.releaseRaceSlot(host)
	}

	// ③ 兜底：池枯竭（从未竞速）时域名直连；竞速尽则快速失败。
	if !raced {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", authority, s.dialTimeout())
		return upstreamResult{conn: c, target: authority, accel: true,
			err: err, dialMS: msSince(t0), attempts: 1}
	}
	return upstreamResult{accel: true, attempts: len(exclude),
		err: fmt.Errorf("竞速拨号 %d 候选全部失败（%s）", len(exclude), host)}
}

// raceWin 迟到胜者（胜者已定后才完成拨号的活候选）。
type raceWin struct {
	addr string
	ms   float64
}

// raceRound 一轮竞速的结果。
type raceRound struct {
	conn        net.Conn
	winner      string
	winMS       float64
	fails       []string  // 真实失败（报 Cooldown）
	lateWins    []raceWin // 迟到胜者（报 Win 置 Active）
	released    []string  // 未起跑候选（Release 归还）
	dialed      int       // 实际起跑数
	candidates  int       // 本轮候选总数（0 = 池枯竭）
}

// dialRaceRound 错峰并行拨候选，首成者胜、其余取消。
func (s *Server) dialRaceRound(rs RacingSelector, host string, exclude map[string]bool) raceRound {
	addrs := rs.RacingPick(host, s.raceWidth(), exclude)
	if len(addrs) == 0 {
		return raceRound{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type res struct {
		addr string
		conn net.Conn
		err  error
		ms   float64
	}
	ch := make(chan res, len(addrs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	started := make([]bool, len(addrs))
	for i, addr := range addrs {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			if i > 0 { // 错峰起跑：给快候选先手机会
				t := time.NewTimer(s.raceStagger())
				defer t.Stop()
				select {
				case <-t.C:
				case <-ctx.Done():
					return // 未起跑：由调用方 Release（不惩罚）
				}
			}
			mu.Lock()
			started[i] = true
			mu.Unlock()
			t0 := time.Now()
			c, err := (&net.Dialer{Timeout: s.dialTimeout()}).DialContext(ctx, "tcp", addr)
			ms := msSince(t0)
			if err != nil && ctx.Err() != nil {
				return // cancel 引起的错误不算真实失败
			}
			ch <- res{addr: addr, conn: c, err: err, ms: ms}
		}(i, addr)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var winner *res
	var rest []res
collect:
	for {
		select {
		case r := <-ch:
			if r.err == nil && r.conn != nil && winner == nil {
				winner = &r
				cancel() // 其余取消
			} else {
				rest = append(rest, r)
			}
		case <-done:
			break collect
		}
	}
drain:
	for { // done 后余量全收（缓冲不丢）
		select {
		case r := <-ch:
			rest = append(rest, r)
		default:
			break drain
		}
	}

	mu.Lock()
	out := raceRound{candidates: len(addrs)}
	for i, ok := range started {
		if ok {
			out.dialed++
		} else {
			out.released = append(out.released, addrs[i])
		}
	}
	mu.Unlock()
	for _, r := range rest {
		switch {
		case r.err != nil:
			out.fails = append(out.fails, r.addr)
		case r.conn != nil: // 迟到胜者：连接已无用，关掉并报活
			r.conn.Close()
			out.lateWins = append(out.lateWins, raceWin{r.addr, r.ms})
		default:
			out.lateWins = append(out.lateWins, raceWin{r.addr, r.ms})
		}
	}
	if winner != nil {
		out.conn, out.winner, out.winMS = winner.conn, winner.addr, winner.ms
	}
	return out
}
