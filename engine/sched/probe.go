package sched

import (
	"sort"
	"sync"
	"time"
)

// ProbeBest 对域名当前 Active 候选做一轮主动 TCP 探测（后台刷新 /
// 启动 last_good 验证用）。失败样本同样进 EWMA 与状态机。
//
// 主动探测是「为辅」路径（设计 §1.3）：正常演进全靠被动信号，
// 这里只为维持 RTT 新鲜度与冷启动验证。返回本轮探测的 IP 数。
func (s *Scheduler) ProbeBest(host string, topN int) int {
	if s.Dial == nil {
		return 0
	}
	// 取快照（持锁最短），探测在锁外。
	type target struct {
		addr string
	}
	var targets []target
	s.mu.Lock()
	if d, ok := s.domains[host]; ok {
		for _, ip := range d.ips {
			if ip.state == StateActive {
				targets = append(targets, target{ip.addr})
			}
		}
	}
	s.mu.Unlock()

	// score 排序取 topN（探测成本控制）。
	if len(targets) > topN {
		s.mu.Lock()
		d := s.domains[host]
		sort.Slice(targets, func(i, j int) bool {
			a, b := d.find(targets[i].addr), d.find(targets[j].addr)
			return a.scoreOf(s.cfg) < b.scoreOf(s.cfg)
		})
		s.mu.Unlock()
		targets = targets[:topN]
	}

	n := 0
	for _, t := range targets {
		t0 := s.clock()
		err := s.Dial(t.addr, s.cfg.ProbeTimeout)
		s.Report(host, t.addr, s.clock().Sub(t0), err == nil)
		n++
	}
	return n
}

// Preflight 并行预筛池内全部 New 候选（启动喂数后调用）：
// 死 IP 直接熔断，避免用户连接逐个背 7s dial 成本轮完死池
// （2026-09-12 实测：meta 老段 20 个 IP 大多不可达，逐个轮转
// 等于 140s 的用户体验灾难）。活的立即入 Active 供择优。
func (s *Scheduler) Preflight(host string) int {
	if s.Dial == nil {
		return 0
	}
	s.mu.Lock()
	d, ok := s.domains[host]
	if !ok {
		s.mu.Unlock()
		return 0
	}
	var addrs []string
	for _, ip := range d.ips {
		if ip.state == StateNew {
			addrs = append(addrs, ip.addr)
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	type result struct {
		addr string
		rtt  time.Duration
		ok   bool
	}
	results := make([]result, len(addrs))
	for i, addr := range addrs {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			t0 := s.clock()
			err := s.Dial(addr, s.cfg.ProbeTimeout)
			results[i] = result{addr, s.clock().Sub(t0), err == nil}
		}(i, addr)
	}
	wg.Wait()
	for _, r := range results {
		s.Report(host, r.addr, r.rtt, r.ok)
	}
	return len(results)
}

// StartRefreshLoop 启动后台刷新：对活跃域名（IdleAfter 内有流量）
// 周期性主动探测 top-N，维持 EWMA 新鲜。stop 关闭。
// 主动探测是辅助路径——真正的可用性信号来自真实连接的 Report。
func (s *Scheduler) StartRefreshLoop(interval time.Duration, topN int, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := s.clock()
				s.mu.Lock()
				var hosts []string
				for host, d := range s.domains {
					if now.Sub(d.lastAccess) < s.cfg.IdleAfter {
						hosts = append(hosts, host)
					}
				}
				s.mu.Unlock()
				for _, host := range hosts {
					s.ProbeBest(host, topN)
				}
			}
		}
	}()
}
