package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/store"
)

// doctorCmd 运行一次六场景探针。默认 both：先跑 direct 对照，再跑
// proxy 通道；输出 JSON 时 stdout 只放机器可读报告，日志走 stderr。
func doctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	mode := fs.String("mode", "both", "探针模式: direct|proxy|both")
	proxyAddr := fs.String("proxy", "http://127.0.0.1:9801", "HTTP CONNECT 代理地址")
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	repo := fs.String("repo", "xueweijian/ghydra", "用于 clone/push dry-run 的仓库 owner/name")
	release := fs.String("release", "https://github.com/cli/cli/releases/latest", "Release 测试 URL")
	timeout := fs.Duration("timeout", 12*time.Second, "每个子探针超时")
	asJSON := fs.Bool("json", false, "只输出 JSON")
	since := fs.Duration("since", 7*24*time.Hour, "--report 汇总窗口")
	report := fs.Bool("report", false, "输出 SQLite 中的历史汇总，不运行探针")
	_ = fs.Parse(args)

	if *report {
		doctorReport(*dbPath, *since, *asJSON)
		return
	}
	if *mode != "direct" && *mode != "proxy" && *mode != "both" {
		log.Fatalf("无效 --mode %q（direct|proxy|both）", *mode)
	}
	if *mode == "proxy" || *mode == "both" {
		if *proxyAddr == "" {
			log.Fatal("proxy 模式必须提供 --proxy")
		}
	}

	cfg := probe.DefaultConfig()
	cfg.Timeout, cfg.ProxyURL, cfg.Repo, cfg.ReleaseURL = *timeout, *proxyAddr, *repo, *release
	runner := probe.New(cfg)
	// 每个模式各自拿完整预算；否则 direct 黑洞耗尽共享 ctx 后，
	// both 的 proxy 半边只剩几秒，双列对照会失真。
	var reports []probe.Report
	if *mode == "direct" || *mode == "both" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout+3*time.Second)
		reports = append(reports, runDoctorMode(ctx, runner, probe.ModeDirect))
		cancel()
	}
	if *mode == "proxy" || *mode == "both" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout+3*time.Second)
		reports = append(reports, runDoctorMode(ctx, runner, probe.ModeProxy))
		cancel()
	}

	if *dbPath != "" {
		if st, err := store.Open(*dbPath); err != nil {
			log.Printf("doctor 结果未落库: %v", err)
		} else {
			persistDoctorReports(st, reports)
			st.Close()
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			log.Fatal(err)
		}
		return
	}
	for _, rep := range reports {
		printDoctorReport(rep)
	}
}

func runDoctorMode(ctx context.Context, runner *probe.Runner, mode probe.Mode) probe.Report {
	log.Printf("[doctor] %s 六场景探测开始（timeout=%s）", mode, runner.Config().Timeout)
	rep := runner.Run(ctx, mode)
	log.Printf("[doctor] %s 完成：五场景 %d/%d；含 SSH 观测 %d/%d，%.0fms", mode, rep.CoveredPassed, rep.CoveredTotal, rep.Passed, rep.Total, rep.DurationMS)
	return rep
}

func persistDoctorReports(st *store.Store, reports []probe.Report) {
	for _, rep := range reports {
		runID := doctorRunID()
		for _, scenario := range rep.Scenarios {
			for _, c := range scenario.Checks {
				st.AppendDoctor(store.DoctorRecord{
					RunID: runID, StartedAt: rep.StartedAt, Mode: string(rep.Mode),
					Scenario: scenario.Scenario, Name: c.Name, Target: c.Target,
					OK: c.OK, Reachable: c.Reachable, Status: c.Status,
					Class: string(c.Class), DurationMS: c.DurationMS, TTFBMS: c.TTFBMS,
					Bytes: c.Bytes, RateBPS: c.RateBPS,
				})
			}
		}
	}
}

func printDoctorReport(rep probe.Report) {
	fmt.Printf("doctor %-6s 五场景 %d/%d（%.1f%%），含 SSH 观测 %d/%d，总耗时 %.0fms\n", rep.Mode, rep.CoveredPassed, rep.CoveredTotal, percent(rep.CoveredPassed, rep.CoveredTotal), rep.Passed, rep.Total, rep.DurationMS)
	fmt.Printf("%-10s %-18s %-6s %-16s %-7s %-15s %9s %9s %s\n",
		"SCENARIO", "CHECK", "OK", "REACH", "STATUS", "CLASS", "TTFB_MS", "RATE_BPS", "ERROR")
	for _, s := range rep.Scenarios {
		for _, c := range s.Checks {
			ok := "FAIL"
			if c.OK {
				ok = "OK"
			}
			reach := "no"
			if c.Reachable {
				reach = "yes"
			}
			fmt.Printf("%-10s %-18s %-6s %-16s %-7d %-15s %9.1f %9.0f %s\n",
				c.Scenario, c.Name, ok, reach, c.Status, c.Class, c.TTFBMS, c.RateBPS, c.Error)
		}
	}
}

func doctorReport(dbPath string, window time.Duration, asJSON bool) {
	if dbPath == "" {
		log.Fatal("--report 需要可用 SQLite 路径")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()
	summary, err := st.DoctorSummary(time.Now().Add(-window))
	if err != nil {
		log.Fatalf("读取 doctor 汇总失败: %v", err)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(summary)
		return
	}
	fmt.Printf("doctor 历史汇总（最近 %s）\n", window)
	fmt.Printf("%-8s %-14s %8s %8s %10s %12s %12s\n", "MODE", "SCENARIO", "CHECKS", "PASSED", "REACHABLE", "AVG_MS", "AVG_TTFB")
	for _, x := range summary {
		fmt.Printf("%-8s %-14s %8d %8d %10d %12.1f %12.1f\n", x.Mode, x.Scenario, x.Checks, x.Passed, x.Reachable, x.AvgDurationMS, x.AvgTTFBMS)
	}
}

// 保留一个轻量文本摘要给未来的定时任务/日志消费方；不在热路径调用。
func doctorSummaryText(rep probe.Report) string {
	return fmt.Sprintf("%s %d/%d", rep.Mode, rep.Passed, rep.Total)
}

func doctorRunID() string {
	b := make([]byte, 12)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func percent(ok, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(ok) * 100 / float64(total)
}
