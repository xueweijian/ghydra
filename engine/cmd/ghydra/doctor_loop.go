package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/store"
)

// startDoctorLoop 每个周期运行一次 direct 对照 + proxy 通道探针。
// 首轮立即执行，随后按 interval；interval 必须由 serve 已确定的实际
// 监听地址传入。返回函数只停止循环，不强行中断当前一次探测。
func startDoctorLoop(interval time.Duration, dbPath, repo, proxyURL string) func() {
	if interval <= 0 || dbPath == "" || proxyURL == "" {
		return func() {}
	}
	stop := make(chan struct{})
	var done sync.WaitGroup
	run := func() {
		cfg := probe.DefaultConfig()
		cfg.Timeout = 12 * time.Second
		cfg.Repo = repo
		cfg.ProxyURL = proxyURL
		r := probe.New(cfg)
		// direct 可能遇到黑洞并耗满超时；proxy 必须拿到独立预算，
		// 否则每小时双列数据会被前一组污染。
		ctxDirect, cancelDirect := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
		direct := r.Run(ctxDirect, probe.ModeDirect)
		cancelDirect()
		ctxProxy, cancelProxy := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
		proxy := r.Run(ctxProxy, probe.ModeProxy)
		cancelProxy()
		reports := []probe.Report{direct, proxy}
		st, err := store.Open(dbPath)
		if err != nil {
			log.Printf("[doctor-loop] 打开 SQLite 失败: %v", err)
			return
		}
		persistDoctorReports(st, reports)
		if err := st.Close(); err != nil {
			log.Printf("[doctor-loop] SQLite 关闭失败: %v", err)
		}
		for _, rep := range reports {
			log.Printf("[doctor-loop] %s 五场景 %d/%d，SSH观测 %d/%d，%.0fms",
				rep.Mode, rep.CoveredPassed, rep.CoveredTotal, rep.Passed, rep.Total, rep.DurationMS)
		}
	}

	done.Add(1)
	go func() {
		defer done.Done()
		run() // on 后立即产生一条带直连对照的记录
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				run()
			}
		}
	}()
	return func() {
		close(stop)
		done.Wait()
	}
}
