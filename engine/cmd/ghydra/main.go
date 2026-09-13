// ghydra CLI — M0 PoC：SNI 转发器 + 自举链基准 + 并发压测。
//
// 用法:
//
//	ghydra bench [--json]                    四级自举链探测，输出轨迹报告
//	ghydra poc [--listen L] [--rewrite-sni N] [--upstream HOST:PORT]  本地 SNI 转发器
//	ghydra loadtest [--concurrency N] [--rounds M]  千并发全链路压测
//
// M0 验证目标见 docs/GHydra-PRD.md §8 M0。M1 起将替换为 cobra 子命令结构。
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/bootstrap"
	"path/filepath"

	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/gitcfg"
	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
	"github.com/xueweijian/ghydra/engine/sched"
	"github.com/xueweijian/ghydra/engine/sni"
	"github.com/xueweijian/ghydra/engine/store"
	"github.com/xueweijian/ghydra/engine/sysproxy"
)

// Version 构建版本（CI release 打 tag 时 -ldflags 注入；dev 兜底）。
// W4 自更新对比源；W3a 起 status 帧/设置页展示。
var Version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// 自更新启动钩子（子命令路由之前）：崩溃清扫 + pending 自检 +
	// ×3 失败自动回滚（W4 设计 §2.3）。幂等，无状态时零开销。
	// 递归护栏：①自检子进程（env 标记）不再进钩子；②只读的 version
	// 子命令跳过（否则 selfCheck→version→BootHook→selfCheck 无限套娃，
	// 冒烟实证 signal: killed）。
	if os.Getenv("GHYDRA_SELF_CHECK_CHILD") == "" && os.Args[1] != "version" {
		bootSelfUpdate(defaultDBPath(), os.Args[1] == "serve")
	}
	switch os.Args[1] {
	case "bench":
		benchCmd(os.Args[2:])
	case "doctor":
		doctorCmd(os.Args[2:])
	case "poc":
		pocCmd(os.Args[2:])
	case "loadtest":
		loadtestCmd(os.Args[2:])
	case "get":
		getCmd(os.Args[2:])
	case "git":
		gitCmd(os.Args[2:])
	case "ssh":
		sshCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "token":
		tokenCmd(os.Args[2:])
	case "version":
		versionCmd(os.Args[2:])
	case "update":
		updateCmd(os.Args[2:])
	case "on":
		onCmd(os.Args[2:])
	case "off":
		offCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ghydra (M1 dev)

用法:
  ghydra on [--port N] [--mode pac|proxy]             接管系统代理 + 后台拉起 serve
  ghydra off                                          恢复系统代理 + 停止 serve
  ghydra serve [--listen ADDR] [--scheduler on|off] [--cdn URL]   前台运行（调试用）
  ghydra get <url> [-o 文件] [--cdn URL] [--json] [--stats]  下载器（A 择优/B 续传切道）
  ghydra git enable|disable|status                   insteadOf 集成（fetch→CDN/push→直连）
  ghydra ssh enable|disable|status                   ssh config 443 写入（22 断 443 通时）
  ghydra status                                      last_good 持久化观察口
  ghydra version                                     版本（自更新自检契约）
  ghydra update [check|rollback] [--pre] [--allow-downgrade]  自更新（永不自毁）
  ghydra doctor [--mode direct|proxy|both]          六场景探针+直连对照+分类报告
  ghydra bench [--mode bootstrap|direct|proxy]     自举链或六域名存活报告
  ghydra poc [--listen ADDR] [--rewrite-sni N]      裸 SNI 转发器（调试工具）
  ghydra loadtest [--mode sni|connect] [--concurrency N] [--rounds M]  并发压测
`)
}

// statusCmd 打印 SQLite last_good（调度器池状态的持久化投影）。
// 实时池快照看 serve 日志（[sched] 前缀）；W4 doctor 会给出完整视图。
func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	_ = fs.Parse(args)

	ensureReconcile(*dbPath) // 任何命令入口都对账（崩溃残留恢复）

	if *dbPath == "" {
		fmt.Println("未配置数据库路径（HOME 不可用）")
		return
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()
	good, err := st.LastGood()
	if err != nil {
		log.Fatalf("读取失败: %v", err)
	}
	if len(good) == 0 {
		fmt.Println("暂无 last_good 记录（serve 运行并产生流量后生成）")
		return
	}
	fmt.Printf("%-40s %-22s %8s %9s  %s\n", "DOMAIN", "IP", "SCORE", "RTT_MS", "UPDATED")
	for host, e := range good {
		fmt.Printf("%-40s %-22s %8.3f %9.0f  %s\n",
			host, e.IP, e.Score, e.RTTMS, e.Updated.Format("01-02 15:04"))
	}
}

// serveCmd 启动 CONNECT 代理（M1 数据面）。
//
// W2 起 --scheduler=on（默认）：命中加速域名的连接由 IP 调度器择优
// （EWMA + 五态状态机 + 粘性 + 熔断），候选来自自举链 meta/DoH、
// SQLite last_good 恢复与池枯竭 DoH 补充；真实流量成败即探测信号。
// --scheduler=off 回退 W1 行为（直连域名，系统 DNS）——A/B 对照与
// 故障逃生通道。
// reorderFlags Go flag 包防御：flag 解析在第一个位置参数处停止，
// bool flag 后面手写取值（--scheduler on）会把 on 当位置参数、吞掉其后
// 全部 flag（W2 get 已踩过一次，serve 同样暴露）。策略：bool flag 无值、
// 其余 flag 收拢在前、位置参数统一垫底，任何书写顺序都能正确解析。
func reorderFlags(args []string, boolFlags ...string) []string {
	boolSet := map[string]bool{}
	for _, b := range boolFlags {
		boolSet[b] = true
		boolSet["-"+b] = true
	}
	var flags, pos []string
	expectVal := ""
	for _, a := range args {
		if expectVal != "" { // 上一个字符串 flag 的取值
			flags = append(flags, a)
			expectVal = ""
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if i := strings.IndexByte(name, '='); i >= 0 {
				continue // --name=value 自带取值
			}
			if !boolSet[a] && !boolSet[name] {
				expectVal = a // 字符串型 flag：下一参是其值
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "本地监听地址（仅 IPv4 回环）")
	backlog := fs.Int("backlog", 4096, "listen backlog")
	timeout := fs.Duration("dial-timeout", proxy.DefaultDialTimeout, "上游连接超时")
	schedOn := fs.Bool("scheduler", true, "IP 调度器（off = W1 直连行为，对照/逃生）")
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径（空 = 不持久化）")
	doctorInterval := fs.Duration("doctor-interval", 0, "自动 doctor 周期（0 = 关闭；ghydra on 默认 1h）")
	rulesInterval := fs.Duration("rules-interval", 6*time.Hour, "规则自动拉取周期（30s 首拉不变；L3 真机实验可调短）")
	rulesURL := fs.String("rules-url", "", "覆盖规则 A 源 URL（默认官方 repo raw；测试/镜像用，仍需合法签名）")
	doctorRepo := fs.String("doctor-repo", probe.DefaultConfig().Repo, "自动 doctor 使用的仓库 owner/name")
	cdn := fs.String("cdn", "", "B 通道 CDN 前缀（如 https://gh-proxy.com/；空 = 禁用切道，W3 默认化）")
	managed := fs.Bool("managed", false, "由 ghydra on 拉起（退出时恢复系统代理）")
	dialOverride := fs.String("dial-override", "", "host=ip[,host=ip…] 强制上游拨号 IP（演练/镜像映射）")
	apiToken := fs.String("api-token", "", "本机 API token（显式注入；默认读/建 ~/.ghydra/api-token，M3-W1）")
	apiTokenFile := fs.String("api-token-file", "", "token 文件路径（覆盖默认位置）")
	guiDist := fs.String("gui-dist", "", "前端面板目录（空 = exe 旁 dist/ 自动探测；不存在则不启面板）")
	_ = fs.Parse(reorderFlags(args, "scheduler", "managed"))

	// on 已在拉起前完成残留对账并写入快照；managed 子进程启动
	// 期间快照存在但 serve.json 还没落盘，不能把当前接管误判成
	// 上一代 kill -9 残留（W3 启动竞态）。
	if !*managed {
		ensureReconcile(*dbPath)
	}

	// M3-W2：规则三级地板（embedded → disk 验签 → remote 热更）。
	// sel/PAC 每次现取快照（零锁热生效）；调度器种子池经 refresher
	// OnApply 回调热重建（phase 3 推模型）。
	rulesDir := rules.DefaultRulesDir()
	provider := rules.NewProvider(rulesDir)
	m := provider.Snapshot().Matcher
	if snap := provider.Snapshot(); snap.Source != rules.SourceEmbedded {
		log.Printf("[rules] 磁盘规则已加载 v%d（source=%s，expires=%s）",
			snap.Version, snap.Source, snap.ExpiresAt.Format(time.RFC3339))
	} else if rulesDir != "" {
		log.Printf("[rules] 磁盘规则缺失或验签失败，使用内嵌地板 v%d", snap.Version)
	}

	// phase 3：seen_max 持久化恢复先于一切远程拉取——回滚防线跨重启
	// （R9：磁盘损坏回退 L0 后防线不回落）。
	var ruleState rules.StateStore
	var ruleDB *store.Store
	if db, err := store.Open(*dbPath); err == nil {
		ruleState = rulesStateAdapter{st: db}
		ruleDB = db
		restoreSeenMax(provider, ruleState)
		log.Printf("[rules] seen_max 锚点 = %d（embedded/disk/持久化 取大）", provider.SeenMax())
	} else {
		log.Printf("[rules] 状态库不可用（%v）：无持久化 seen_max/退避", err)
	}

	var sel proxy.UpstreamSelector = proxy.SelectorFunc(func(host string) (string, bool) {
		if provider.Snapshot().Matcher.Match(host) {
			return net.JoinHostPort(host, "443"), true
		}
		return "", false
	})

	var (
		reportEvent func(proxy.Event)
		sc          *sched.Scheduler
	)
	if *schedOn {
		sel2, shutdown, err := startScheduler(m, *dbPath)
		if err != nil {
			log.Fatalf("调度器启动失败: %v", err)
		}
		sel, reportEvent, sc = sel2, sel2.ReportEvent, sel2.Sched
		defer shutdown()
	}

	// 通道级决策器（M2-W1）：CONNECT 事件进流量窗口，doctor 判定
	// 驱动熔断/恢复，明文 HTTP 路径按其决策 A/B 改写（D1/D3）
	chRouter := channel.New(channel.DefaultConfig(), nil)
	var pick func(string) (string, bool)
	if sc != nil {
		pick = func(host string) (string, bool) { return sc.Pick(host) }
	}

	// --dial-override host=ip[,host=ip…]：强制指定上游拨号 IP（SNI/Host
	// 语义不变）。用途：故障演练（把 github 系压到本地假源站）与自建镜像
	// 映射。A 路径（serve 拨号）与 doctor 直连列探针同时生效——演练需要
	// "两列同死" 的 NetworkFault 场景。
	ovMap := map[string]string{}
	if *dialOverride != "" {
		for _, pair := range strings.Split(*dialOverride, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
				log.Fatalf("--dial-override 形态非法: %q", pair)
			}
			ovMap[kv[0]] = kv[1]
		}
		if len(ovMap) > 0 {
			orig := sel
			sel = proxy.SelectorFunc(func(host string) (string, bool) {
				ip, ok := orig.Select(host)
				if !ok {
					return "", false
				}
				if ov, hit := ovMap[host]; hit {
					return net.JoinHostPort(ov, "443"), true
				}
				return ip, ok
			})
		}
	}

	var conns atomic.Int64

	// M3-W1：控制 API 装配。apiSrv 先声明后构造（OnEvent/DoctorRun
	// 等闭包自引用）；token 空 = /api 全 401（装配失败的保守面）。
	var apiSrv *api.Server
	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once
	cdnFn := func() string { return *cdn }
	// phase 3：规则拉取管线（吃自身 A/B 狗粮）。A=IP 择优直连，
	// B=CDN 前缀代理同一路径（运行时热更）。调度器热重建走 OnApply。
	rawRulesURL := "https://raw.githubusercontent.com/xueweijian/ghydra/main/rules/current.json"
	if *rulesURL != "" {
		rawRulesURL = *rulesURL // 测试/镜像覆盖；信任锚不变（签名仍必验）
	}
	rawRulesPath := "/" + rawRulesURL
	rulesRef := rules.NewRefresher(provider, rules.RefresherConfig{
		Interval: *rulesInterval,
	}, rulesFetchA(pick), rulesFetchB(cdnFn, rawRulesPath), rawRulesURL, "", ruleState)
	rulesRef.SetLogf(func(f string, a ...any) { log.Printf("[rules] "+f, a...) })
	rulesRef.OnApply(func(snap *rules.Snapshot) { rulesSeedRebuild(sc, snap) })
	rulesRef.Start()
	defer rulesRef.Stop()
	if ruleDB != nil {
		defer ruleDB.Close()
	}

	srv := &proxy.Server{
		Selector:    sel,
		DialTimeout: *timeout,
		OnEvent: func(e proxy.Event) {
			n := conns.Add(1)
			status := "OK"
			if e.DialErr != nil {
				status = e.DialErr.Error()
			} else if e.CopyErr != "" {
				status = "copy:" + e.CopyErr
			}
			log.Printf("[conn#%d] %s -> %s accel=%t dial=%.1fms rx=%dB tx=%dB %s",
				n, e.Host, e.Target, e.Accel, e.DialMS, e.Rx, e.Tx, status)
			if apiSrv != nil {
				apiSrv.PushConn(mapConn(e)) // SSE conn 帧（250ms 合批）
			}
			if reportEvent != nil {
				reportEvent(e) // 调度器信号（非阻塞；池外目标自动忽略）
			}
			// CONNECT 成败进通道 A 流量窗口（通道级熔断的流量信号）
			if e.Accel {
				chRouter.Report(channel.Flow{Kind: channel.KindCONNECT, Host: e.Host},
					channel.Direct, e.DialErr == nil && !sched.HandshakeDead(e))
			}
		},
	}

	// 同端口托管 PAC / status（R5：主端口 http 分流，AutoConfigURL 直指）
	actualAddr := *listen

	// ---- M3-W1 控制 API：token 解析 + Deps 装配 ----
	tok := *apiToken
	if tok == "" {
		tp := *apiTokenFile
		if tp == "" {
			tp = api.DefaultTokenPath()
		}
		if t, err := api.LoadOrCreateToken(tp); err == nil {
			tok = t
		} else {
			log.Printf("[api] token 不可用: %v（/api 将全部 401）", err)
		}
	}
	doctorProxyURL := "" // 监听确定后填入（Deps 闭包读变量）
	doctorBusy := false
	var doctorMu sync.Mutex
	tasks := newTaskRegistry(getDownloaderFactory(pick, cdnFn),
		func(t api.GetTask) {
			if apiSrv != nil {
				apiSrv.PushGet(t)
			}
		})

	buildConfig := func() api.ApiConfig {
		return api.ApiConfig{
			Listen: actualAddr, Scheduler: *schedOn, CDN: cdnFn(),
			DoctorEveryS: int64(*doctorInterval / time.Second),
			DoctorRepo:   *doctorRepo, Managed: *managed,
			GUIDist: guiDirOrDefault(*guiDist),
		}
	}
	apiSrv = api.New(api.Deps{
		Status: func() api.ApiStatus {
			st := api.ApiStatus{
				APIVersion: api.APIVersion,
				Version:    Version,
				Listen:     actualAddr,
				Scheduler:  *schedOn,
				Conns:      conns.Load(),
				UptimeS:    time.Since(startTime).Seconds(),
				Channel:    mapChannel(chRouter.Snapshot()),
				CDN:        cdnFn(),
				Rules:      mapRulesStatus(provider),
			}
			if sc != nil {
				pools := map[string]api.PoolSnapshot{}
				for _, h := range sc.Hosts() {
					pools[h] = api.PoolSnapshot{Sticky: sc.StickyIP(h), IPs: mapIPs(sc.Snapshot(h))}
				}
				st.Pools = pools
			}
			// 接管态：on = 快照行存在（ensureReconcile 同源判定）。1s tick 一次
			// 单行 SELECT，本地 SQLite 微秒级可忽略；库不可用恒 false。
			if ruleDB != nil {
				if on, takenAt, err := ruleDB.LoadTakeoverState(); err == nil && on {
					st.Takeover = api.TakeoverState{On: true, Since: takenAt.UTC().Format(time.RFC3339)}
				} else if err != nil {
					log.Printf("[api] 接管态查询失败: %v", err)
				}
			}
			return st
		},
		ConfigGet: buildConfig,
		ConfigSet: func(p api.ConfigPatch) (api.ApiConfig, error) {
			if p.CDN == nil {
				return api.ApiConfig{}, api.UserError("无可热更字段（仅 cdn）")
			}
			*cdn = *p.CDN // 热更：plain HTTP/doctor/get/status 全走 cdnFn
			log.Printf("[api] cdn 热更 → %q", *p.CDN)
			return buildConfig(), nil
		},
		SystemOn: func(mode string) (api.OnResp, error) {
			if err := applyTakeover(*dbPath, mode, portOf(actualAddr)); err != nil {
				return api.OnResp{}, err
			}
			log.Printf("[api] 系统代理已接管（%s）", mode)
			return api.OnResp{OK: true, Mode: mode}, nil
		},
		SystemOff: func(shutdown bool) (api.OffResp, error) {
			if err := releaseTakeover(*dbPath); err != nil {
				return api.OffResp{}, err
			}
			log.Printf("[api] 系统代理已恢复")
			if shutdown {
				shutdownOnce.Do(func() { close(shutdownCh) })
			}
			return api.OffResp{OK: true, Shutdown: shutdown}, nil
		},
		DoctorRun: func(repo string) (string, error) {
			doctorMu.Lock()
			if doctorBusy {
				doctorMu.Unlock()
				return "", api.ErrBusy
			}
			doctorBusy = true
			doctorMu.Unlock()
			runID := doctorRunID()
			go func() {
				defer func() {
					doctorMu.Lock()
					doctorBusy = false
					doctorMu.Unlock()
				}()
				r := repo
				if r == "" {
					r = *doctorRepo
				}
				frame, err := doctorOnce(r, doctorProxyURL, cdnFn,
					chRouter.NotifyDoctor, chRouter.NotifyB, ovMap)
				frame.RunID = runID
				if err != nil {
					frame.Error = err.Error()
					frame.Verdict = "error"
				}
				apiSrv.PushDoctor(frame)
			}()
			return runID, nil
		},
		DoctorSummary: func(hours int) (api.DoctorSummaryResp, error) {
			st, err := store.Open(*dbPath)
			if err != nil {
				return api.DoctorSummaryResp{}, err
			}
			defer st.Close()
			rows, err := st.DoctorSummary(time.Now().Add(-time.Duration(hours) * time.Hour))
			if err != nil {
				return api.DoctorSummaryResp{}, err
			}
			out := api.DoctorSummaryResp{Runs: []api.DoctorSummaryRow{}}
			for _, r := range rows {
				out.Runs = append(out.Runs, api.DoctorSummaryRow{
					Mode: r.Mode, Scenario: r.Scenario, Checks: r.Checks,
					Passed: r.Passed, Reachable: r.Reachable,
					AvgDurationMS: r.AvgDurationMS, AvgTTFBMS: r.AvgTTFBMS,
					FirstAt: r.FirstAt, LastAt: r.LastAt,
				})
			}
			return out, nil
		},
		GitOp: func(action string, body []byte) (any, error) {
			switch action {
			case "enable":
				var req struct {
					CDN        string `json:"cdn"`
					PushViaB   bool   `json:"push_via_b"`
					SSHRewrite bool   `json:"ssh_rewrite"`
					Force      bool   `json:"force"`
				}
				decodeJSONBody(body, &req)
				snap, err := gitEnableCore(*dbPath,
					gitcfg.Options{CDN: req.CDN, PushViaB: req.PushViaB, SSHRewrite: req.SSHRewrite}, req.Force)
				if err != nil {
					return nil, err
				}
				return map[string]any{"ok": true, "written_count": len(snap.Written)}, nil
			case "disable":
				n, err := gitDisableCore(*dbPath)
				if err != nil {
					return nil, err
				}
				return map[string]any{"ok": true, "restored": n}, nil
			case "status":
				return gitStatusCore(*dbPath, gitcfg.Options{CDN: cdnFn()})
			default:
				return nil, api.UserError("未知 action: " + action)
			}
		},
		SSHOp: func(action string, body []byte) (any, error) {
			cfgPath := defaultSSHConfigPath()
			switch action {
			case "enable":
				var req struct {
					Alias    bool `json:"alias"`
					AssumeOK bool `json:"assume_ok"`
				}
				decodeJSONBody(body, &req)
				return sshEnableCore(*dbPath, cfgPath, req.Alias, req.AssumeOK)
			case "disable":
				return sshDisableCore(*dbPath, cfgPath)
			case "status":
				return sshStatusCore(cfgPath)
			default:
				return nil, api.UserError("未知 action: " + action)
			}
		},
		GetStart:    tasks.start,
		GetProgress: tasks.progress,
		Rules:       func() api.RulesSnapshot { return mapRulesSnapshot(provider, rulesRef) },
		RulesRefresh: func() (string, error) {
			return rulesRef.Trigger()
		},
	}, tok)

	mux := http.NewServeMux()
	mux.HandleFunc("/pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		// GUI 壳/浏览器面板与 daemon 分源（wails://、file:// 等），
		// 只读端点放行跨域；W3-W1 的 /api/* 会用 token+Host 校验收紧。
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, provider.Snapshot().Matcher.PAC(fmt.Sprintf("127.0.0.1:%d", portOf(actualAddr))))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // 只读端点，同上
		out := map[string]any{
			"listen":    actualAddr,
			"scheduler": *schedOn,
			"conns":     conns.Load(),
			"uptime_s":  time.Since(startTime).Seconds(),
		}
		if sc != nil {
			pools := map[string]any{}
			for _, host := range sc.Hosts() {
				pools[host] = map[string]any{
					"sticky": sc.StickyIP(host),
					"ips":    sc.Snapshot(host),
				}
			}
			out["pools"] = pools
		}
		out["channel"] = chRouter.Snapshot()
		if *cdn != "" {
			out["cdn"] = *cdn
		}
		writeJSON(w, out)
	})
	// 明文 http 代理路径：URL 可见 → 按通道决策 A 转发 / B CDN 改写；
	// 非代理请求（Host 形态）且面板目录在场 → 前端面板（M3-W1 双模式）
	plain := servePlainHTTP(chRouter, m, pick, cdnFn)
	gui := staticSPA(guiDirOrDefault(*guiDist))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" && gui != nil {
			gui.ServeHTTP(w, r)
			return
		}
		plain.ServeHTTP(w, r)
	}))
	mux.Handle("/api/", apiSrv.Handler())
	srv.HTTPHandler = mux

	// 端口冲突迁移（W3：9801 被占 → +1..+8）
	var ln net.Listener
	var err error
	for i := 0; i < 9; i++ {
		ln, err = proxy.NewListener(actualAddr, *backlog)
		if err == nil {
			break
		}
		if p := portOf(actualAddr); p > 0 {
			actualAddr = fmt.Sprintf("127.0.0.1:%d", p+1)
		} else {
			break
		}
	}
	if err != nil {
		log.Fatalf("监听失败（含迁移尝试）: %v", err)
	}
	doctorProxyURL = "http://" + ln.Addr().String()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			log.Printf("收到退出信号，正在关闭…")
		case <-shutdownCh:
			log.Printf("API 请求退出，正在关闭…")
		}
		if *managed {
			restoreSnapshot(*dbPath) // on 拉起的 serve：退出时恢复系统代理
		}
		ln.Close()
	}()

	var stopDoctor func()
	if *doctorInterval > 0 && *dbPath != "" {
		stopDoctor = startDoctorLoop(*doctorInterval, *dbPath, *doctorRepo,
			doctorProxyURL, cdnFn, chRouter.NotifyDoctor, chRouter.NotifyB, ovMap)
		defer stopDoctor()
	}

	apiCtx, apiCancel := context.WithCancel(context.Background())
	defer apiCancel()
	apiSrv.Start(apiCtx)

	log.Printf("ghydra serve 已启动: %s | 加速域名 %d 条 | scheduler=%t | backlog %d",
		ln.Addr().String(), len(m.Domains()), *schedOn, *backlog)
	log.Printf("PAC: http://%s/pac | 系统代理指向 %s（或 ghydra on 一键接管）",
		ln.Addr().String(), ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("serve 退出: %v", err)
	}
	if *managed {
		restoreSnapshot(*dbPath) // 正常退出路径也恢复
	}
	log.Printf("已退出，共服务 %d 条连接", conns.Load())
}

// tokenCmd 打印本机 API token（浏览器兜底模式/GUI 首次接入的取值出口）。
func tokenCmd(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON 输出")
	file := fs.String("file", "", "token 文件路径（默认 ~/.ghydra/api-token）")
	_ = fs.Parse(reorderFlags(args, "json"))
	p := *file
	if p == "" {
		p = api.DefaultTokenPath()
	}
	if p == "" {
		log.Fatalf("HOME 不可用，无法定位 token 文件")
	}
	tok, err := api.LoadOrCreateToken(p)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	if *asJSON {
		fmt.Printf("{\"token\": %q, \"path\": %q}\n", tok, p)
		return
	}
	fmt.Printf("%s\n（文件 %s；请求带 X-GHydra-Token 头，SSE 用 ?token=）\n", tok, p)
}

var startTime = time.Now()

// restoreSnapshot 恢复接管前的系统代理（managed serve 退出 hook）。
func restoreSnapshot(dbPath string) {
	if dbPath == "" {
		return
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer st.Close()
	psJSON, ok, err := st.LoadSnapshotJSON()
	if err != nil || !ok {
		return
	}
	var orig sysproxy.Setting
	if json.Unmarshal([]byte(psJSON), &orig) != nil {
		sysproxy.Clear() // 快照损坏：清除代理，避免残留 GHydra 接管态
		st.DeleteSnapshot()
		removeDaemonState()
		return
	}
	if err := sysproxy.Apply(orig); err != nil {
		log.Printf("[restore] 恢复系统代理失败: %v（原值 %s）；保留快照待对账", err, orig)
		return
	}
	st.DeleteSnapshot()
	removeDaemonState()
	log.Printf("[restore] 系统代理已恢复")
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(p)
	return n
}

// startScheduler 装配 M1-W2 调度器：候选喂数（自举链 + last_good
// 恢复）→ 主动探测（拨号函数）→ 池枯竭 DoH 兜底 → 后台刷新 →
// 事件管道。返回 shutdown 必须在退出时调用（flush 持久化）。
func startScheduler(m *rules.Matcher, dbPath string) (*sched.Selector, func(), error) {
	cfg := sched.DefaultConfig()
	sc := sched.New(cfg)
	sc.Logf = func(f string, a ...any) { log.Printf(f, a...) }

	// 持久化（可选：dbPath 空 = 纯内存）
	var st *store.Store
	if dbPath != "" {
		s, err := store.Open(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite: %w", err)
		}
		st = s
		sc.OnActive = func(host, addr string, score, rtt float64) {
			st.UpdateLastGood(host, addr, score, rtt) // 非阻塞
		}
	}

	// 主动探测拨号：保留 TCP-only 作为通用回退；HTTPS 候选的
	// Preflight/refresh 使用下面的 DialHost 做带 SNI 的 TLS 握手，
	// 避免 TCP 通但 ClientHello 后沉默的死 IP 被放入 Active。
	sc.Dial = func(addr string, timeout time.Duration) error {
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	}
	sc.DialHost = func(host, addr string, timeout time.Duration) error {
		var d net.Dialer
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		t := tls.Client(c, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host})
		if err := t.Handshake(); err != nil {
			return err
		}
		return nil
	}

	// 池枯竭兜底：DoH 解析该域名（bootstrap 的 per-host 能力）
	// 调度器池内地址统一为 host:443，避免裸 IP 在代理数据面报
	// "missing port in address"。

	resolver := bootstrap.New()
	sc.Resolve = func(host string) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		ips, err := resolver.ResolveHost(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.JoinHostPort(ip, "443"))
		}
		return out, nil
	}

	// ① 候选喂数：DoH 解析（就近可达 IP，实测质量最高）+ 自举链
	// （meta 网段 + last-good 缓存）。meta 老段在本网络可能整段不可达
	// ——Preflight 预筛保证用户连接不背死 IP 的 dial 成本。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t0 := time.Now()

		// 1a. DoH 解析 github.com（223 系就近结果，立即可用）
		var dohIPs []string
		if ips, err := resolver.ResolveHost(ctx, "github.com"); err == nil {
			for _, ip := range ips {
				dohIPs = append(dohIPs, net.JoinHostPort(ip, "443"))
			}
		}
		// 1b. 自举链（meta 网段 + 缓存 + 种子）
		res := resolver.Resolve(ctx)
		metaIPs := make([]string, 0, len(res.IPs))
		for _, ip := range res.IPs {
			metaIPs = append(metaIPs, net.JoinHostPort(ip, "443"))
		}
		if n := sc.AddCandidates("github.com", append(dohIPs, metaIPs...)); n > 0 {
			log.Printf("[sched] 候选入池 github.com: +%d（DoH %d + %s %d，%.1fs）",
				n, len(dohIPs), res.Source, len(metaIPs), time.Since(t0).Seconds())
		}
		// 1c. 并行预筛：死 IP 直接熔断，不进用户连接路径
		if n := sc.Preflight("github.com"); n > 0 {
			log.Printf("[sched] Preflight 预筛 github.com: %d 候选完成", n)
		}
	}()

	// ② last_good 恢复（经主动验证后放行）
	if st != nil {
		if good, err := st.LastGood(); err == nil {
			for host, e := range good {
				if !m.Match(host) {
					continue // 规则外域名不恢复（防规则变更残留）
				}
				n := sc.AddCandidates(host, []string{e.IP})
				if n > 0 {
					go sc.ProbeBest(host, 1)
					log.Printf("[sched] last_good 恢复 %s -> %s (score=%.3f rtt=%.0fms)",
						host, e.IP, e.Score, e.RTTMS)
				}
			}
		}
	}

	// ③ 后台刷新（活跃域名 Active top5，30s 一轮）
	stop := make(chan struct{})
	sc.StartRefreshLoop(30*time.Second, 5, stop)

	shutdown := func() {
		close(stop)
		if st != nil {
			st.Close() // flush 在途写
		}
	}
	return sched.NewSelector(sc, m), shutdown, nil
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra", "ghydra.db")
}

func benchCmd(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "输出 JSON（真机验收报告格式）")
	mode := fs.String("mode", "bootstrap", "模式: bootstrap|direct|proxy")
	proxyAddr := fs.String("proxy", "http://127.0.0.1:9801", "proxy 模式的 HTTP CONNECT 地址")
	repo := fs.String("repo", "xueweijian/ghydra", "六域名探针使用的仓库 owner/name")
	timeout := fs.Duration("timeout", 30*time.Second, "总超时")
	_ = fs.Parse(args)

	if *mode == "direct" || *mode == "proxy" {
		cfg := probe.DefaultConfig()
		cfg.Timeout, cfg.ProxyURL, cfg.Repo = *timeout, *proxyAddr, *repo
		runner := probe.New(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), *timeout+3*time.Second)
		defer cancel()
		pmode := probe.ModeDirect
		if *mode == "proxy" {
			pmode = probe.ModeProxy
		}
		checks := runner.RunGitHubDomains(ctx, pmode)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"mode": pmode, "checks": checks})
			return
		}
		passed := 0
		for _, c := range checks {
			if c.OK {
				passed++
			}
			fmt.Printf("%-10s %-4s %-15s status=%d ttfb=%.1fms class=%s %s\n", c.Name, map[bool]string{true: "OK", false: "FAIL"}[c.OK], c.Mode, c.Status, c.TTFBMS, c.Class, c.Error)
		}
		fmt.Printf("bench %s: %d/%d GitHub 域名可用\n", pmode, passed, len(checks))
		return
	}
	if *mode != "bootstrap" {
		log.Fatalf("无效 --mode %q（bootstrap|direct|proxy）", *mode)
	}

	r := bootstrap.New()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res := r.Resolve(ctx)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			log.Fatal(err)
		}
		return
	}
	fmt.Printf("四级自举链报告  总耗时 %.1fms\n", res.ElapsedMS)
	fmt.Printf("来源: %s  候选 IP: %d 个\n", res.Source, len(res.IPs))
	for _, s := range res.Steps {
		status := "FAIL"
		if s.OK {
			status = "OK"
		}
		fmt.Printf("  L%d %-45s %-4s %6.1fms  err=%s\n", s.Level, s.Name, status, s.ElapsedMS, s.Err)
	}
	for _, ip := range res.IPs {
		fmt.Printf("    %s\n", ip)
	}
}

func pocCmd(args []string) {
	fs := flag.NewFlagSet("poc", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "本地监听地址")
	rewrite := fs.String("rewrite-sni", "", "实验工具：改写 ClientHello 的 SNI（TLS1.2/1.3 下会 bad record mac，仅协议研究用）")
	upstream := fs.String("upstream", "", "可选：覆盖上游地址（HOST:PORT），默认按 SNI 域名拨 443")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second, "上游连接超时")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	log.Printf("GHydra PoC 转发器已启动: %s (rewrite-sni=%q upstream=%q)", *listen, *rewrite, *upstream)
	forwardServer(ln, *rewrite, *upstream, *dialTimeout)
}

// forwardServer 是 poc 与 loadtest 共用的转发 accept 循环。
func forwardServer(ln net.Listener, rewrite, upstream string, dialTimeout time.Duration) {
	var conns int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		id := atomic.AddInt64(&conns, 1)
		go func(c net.Conn) {
			defer c.Close()
			relay(c, rewrite, upstream, dialTimeout, id)
		}(conn)
	}
}

func relay(conn net.Conn, rewrite, upstream string, dialTimeout time.Duration, id int64) {
	start := time.Now()
	br := bufio.NewReader(conn)
	ch, err := sni.ReadClientHello(br)
	if err != nil {
		log.Printf("#%d ClientHello 解析失败: %v", id, err)
		return
	}
	host := ch.ServerName
	out := ch.Record
	if rewrite != "" {
		out, err = ch.RewriteSNI(rewrite)
		if err != nil {
			log.Printf("#%d SNI 改写失败: %v", id, err)
			return
		}
		host = rewrite
	}
	if host == "" {
		log.Printf("#%d 无 SNI，拒绝转发", id)
		return
	}
	addr := net.JoinHostPort(host, "443")
	if upstream != "" {
		addr = upstream
	}
	upstreamConn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		log.Printf("#%d %s 上游连接失败: %v", id, addr, err)
		return
	}
	defer upstreamConn.Close()
	if _, err := upstreamConn.Write(out); err != nil {
		log.Printf("#%d %s 写入上游失败: %v", id, addr, err)
		return
	}
	log.Printf("#%d SNI=%s -> %s (改写=%t)", id, ch.ServerName, upstreamConn.RemoteAddr(), rewrite != "")
	go func() {
		io.Copy(upstreamConn, br)
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			upstreamConn.Close()
		}
	}()
	n, _ := io.Copy(conn, upstreamConn)
	log.Printf("#%d 完成 %s 下行 %dB 耗时 %s", id, ch.ServerName, n, time.Since(start).Round(time.Millisecond))
}

// loadtestCmd 端到端并发压测：内置假上游 TLS 服务 + 转发器 + N×M 客户端。
// 验收口径（PRD 非功能需求）：1000 并发连接、成功率 100%、无 goroutine 泄漏。
func loadtestCmd(args []string) {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	concurrency := fs.Int("concurrency", 1000, "并发 worker 数")
	rounds := fs.Int("rounds", 3, "每 worker 建拆连接轮数")
	deadline := fs.Duration("timeout", 15*time.Second, "单连接超时")
	mode := fs.String("mode", "sni", "压测模式: sni(M0 裸SNI转发) | connect(M1 CONNECT内核+自建backlog)")
	backlog := fs.Int("backlog", 4096, "connect 模式 listen backlog")
	_ = fs.Parse(args)

	// ① 假上游（自签 github.com 证书的 HTTPS 服务）
	cert := genSelfSignedCert("github.com")
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	upSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "OK")
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	defer upSrv.Close()
	go upSrv.ServeTLS(upLn, "", "")

	// ② 中继：sni = M0 裸转发路径；connect = M1 CONNECT 内核（自建 socket backlog）
	var fwdAddr string
	switch *mode {
	case "connect":
		sel := proxy.SelectorFunc(func(string) (string, bool) { return upLn.Addr().String(), true })
		srv := &proxy.Server{
			Selector:    sel,
			DialTimeout: 5 * time.Second,
			OnEvent:     func(proxy.Event) {},
		}
		fwdLn, err := proxy.NewListener("127.0.0.1:0", *backlog)
		if err != nil {
			log.Fatal(err)
		}
		fwdAddr = fwdLn.Addr().String()
		go srv.Serve(fwdLn)
	case "sni":
		fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		fwdAddr = fwdLn.Addr().String()
		go forwardServer(fwdLn, "", upLn.Addr().String(), 5*time.Second)
	default:
		log.Fatalf("未知模式 %q（sni|connect）", *mode)
	}
	time.Sleep(200 * time.Millisecond)

	// ③ 客户端压测
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientCfg := &tls.Config{ServerName: "github.com", RootCAs: pool}

	baseGoroutines := runtime.NumGoroutine()
	var peakGoroutines int64
	var okCount, failCount int64
	errKinds := sync.Map{}
	latencies := make([]time.Duration, 0, *concurrency**rounds)
	var mu sync.Mutex

	log.SetOutput(io.Discard) // 压测期间静默 relay 日志
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < *rounds; i++ {
				if n := runtime.NumGoroutine(); int64(n) > atomic.LoadInt64(&peakGoroutines) {
					atomic.StoreInt64(&peakGoroutines, int64(n))
				}
				t0 := time.Now()
				var err error
				if *mode == "connect" {
					err = connectRound(fwdAddr, clientCfg, *deadline)
				} else {
					err = oneRound(fwdAddr, clientCfg, *deadline)
				}
				lat := time.Since(t0)
				mu.Lock()
				latencies = append(latencies, lat)
				mu.Unlock()
				if err != nil {
					atomic.AddInt64(&failCount, 1)
					kind := errKind(err)
					errKinds.Store(kind, true)
					continue
				}
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	log.SetOutput(os.Stderr)

	// ④ 泄漏检测：连接全部关闭后 goroutine 应回落
	time.Sleep(2 * time.Second)
	finalGoroutines := runtime.NumGoroutine()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}
	total := *concurrency * *rounds
	var kinds []string
	errKinds.Range(func(k, _ any) bool {
		kinds = append(kinds, k.(string))
		return true
	})

	fmt.Printf("千并发压测报告\n")
	fmt.Printf("  规模: %d 并发 × %d 轮 = %d 连接（建拆） 耗时 %s\n", *concurrency, *rounds, total, elapsed.Round(time.Millisecond))
	fmt.Printf("  成功率: %d/%d (%.2f%%)\n", okCount, total, float64(okCount)/float64(total)*100)
	if len(latencies) > 0 {
		fmt.Printf("  延迟: p50 %s  p95 %s  p99 %s  max %s\n", pct(0.50).Round(time.Microsecond), pct(0.95).Round(time.Microsecond), pct(0.99).Round(time.Microsecond), latencies[len(latencies)-1].Round(time.Microsecond))
	}
	fmt.Printf("  吞吐: %.0f conn/s\n", float64(total)/elapsed.Seconds())
	if failCount > 0 {
		fmt.Printf("  失败: %d  种类: %v\n", failCount, kinds)
	}
	fmt.Printf("  goroutine: 基线 %d → 峰值 %d → 回落 %d %s\n", baseGoroutines, peakGoroutines, finalGoroutines, leakVerdict(baseGoroutines, finalGoroutines))
	fmt.Printf("  内存: HeapAlloc %.1fMB / HeapSys %.1fMB\n", float64(ms.HeapAlloc)/1e6, float64(ms.HeapSys)/1e6)
}

// connectRound 完成一次 CONNECT 隧道全流程（M1 内核路径）。
func connectRound(addr string, cfg *tls.Config, timeout time.Duration) error {
	raw, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(timeout))
	fmt.Fprint(raw, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		return fmt.Errorf("tunnel: %q", strings.TrimSpace(line))
	}
	c := tls.Client(raw, cfg)
	defer c.Close()
	if err := c.Handshake(); err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n")
	body, err := io.ReadAll(c)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(body) == 0 || !containsStatus200(body) {
		return fmt.Errorf("bad response (%d bytes)", len(body))
	}
	return nil
}

// oneRound 完成一次：建连 → TLS 握手 → 请求 → 读完整响应 → 关闭。
func oneRound(addr string, cfg *tls.Config, timeout time.Duration) error {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("dial/tls: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	req := "GET / HTTP/1.1\r\nHost: github.com\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	body, err := io.ReadAll(c)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(body) == 0 || !containsStatus200(body) {
		return fmt.Errorf("bad response (%d bytes)", len(body))
	}
	return nil
}

func containsStatus200(head []byte) bool {
	return len(head) >= 12 && string(head[:12]) == "HTTP/1.1 200"
}

func errKind(err error) string {
	s := err.Error()
	for _, prefix := range []string{"dial/tls", "write", "read"} {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return prefix
		}
	}
	return "other"
}

func leakVerdict(base, final int) string {
	if final <= base+8 {
		return "✓ 无泄漏"
	}
	return "✗ 疑似泄漏"
}

func genSelfSignedCert(dnsName string) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	c := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	c.Leaf, _ = x509.ParseCertificate(der)
	return c
}
