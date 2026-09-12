package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/store"
)

// startDoctorLoop 每个周期运行一次 direct 对照 + proxy 通道探针，
// cdnPrefix 非空时加 B 通道列（W3：Router trip 前置健康检查的数据源）。
// 首轮立即执行，随后按 interval；interval 必须由 serve 已确定的实际
// 监听地址传入。notify 可为 nil；非 nil 时每轮把蒸馏判定
// （channel.Verdict）回调给调用方（serve 的通道决策器）。
// notifyB 回调 B 列健康（nil = 无 B 通道）。
func startDoctorLoop(interval time.Duration, dbPath, repo, proxyURL, cdnPrefix string,
	notify func(channel.Verdict), notifyB func(bool)) func() {
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
		cfg.CDNPrefix = cdnPrefix
		r := probe.New(cfg)
		// 先确认本地代理监听并能返回 /status。on 启动时 doctor
		// goroutine 与 serve 并行，不能把启动瞬间的 connection refused
		// 误记成代理故障。
		if err := waitDoctorProxy(proxyURL, 15*time.Second); err != nil {
			log.Printf("[doctor-loop] 代理尚未就绪: %v", err)
			return
		}
		// direct 可能遇到黑洞并耗满超时；proxy 必须拿到独立预算，
		// 否则每小时双列数据会被前一组污染。
		ctxDirect, cancelDirect := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
		direct := r.Run(ctxDirect, probe.ModeDirect)
		cancelDirect()
		ctxProxy, cancelProxy := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
		proxy := r.Run(ctxProxy, probe.ModeProxy)
		cancelProxy()
		reports := []probe.Report{direct, proxy}
		// B 通道列：五场景经 CDN 前缀（独立预算；失败也不阻塞判定）
		if cdnPrefix != "" {
			ctxCDN, cancelCDN := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
			cdn := r.Run(ctxCDN, probe.ModeCDN)
			cancelCDN()
			reports = append(reports, cdn)
			if notifyB != nil {
				notifyB(channel.CoveredHealthy(&cdn))
			}
		}
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
		if notify != nil {
			v := channel.VerdictFromReports(&proxy, &direct)
			notify(v)
			log.Printf("[doctor-loop] 通道判定: %s", verdictName(v))
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

func waitDoctorProxy(raw string, timeout time.Duration) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("代理地址无效: %q", raw)
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + u.Host + "/status")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待 %s/status 超时", u.Host)
}

func verdictName(v channel.Verdict) string {
	switch v {
	case channel.VerdictHealthy:
		return "healthy（A 恢复）"
	case channel.VerdictSourceFault:
		return "source_fault（源头故障 → 切 B）"
	case channel.VerdictNetworkFault:
		return "network_fault（网络阻断 → 切 B）"
	case channel.VerdictUnclear:
		return "unclear（不动通道）"
	default:
		return "none（无数据）"
	}
}
