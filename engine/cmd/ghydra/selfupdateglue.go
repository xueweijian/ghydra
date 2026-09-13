package main

// selfupdate 生产装配（M3-W4 p1）：CLI 子命令 + BootHook + DaemonControl。
//
// 信任链全景（设计 §2）：Releases API 走 A 通道 IP 直连（复用 get.AFetcher
// 的 DoH 自举 + 择优拨号）；资产下载走 get.Downloader（A 起步/B 镜像续传，
// 吃自家狗粮）；完整性由 sha256 + minisign 双防线保证（镜像不可信也不致害
// ——README 信任教学的实证）。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/rules"
	"github.com/xueweijian/ghydra/engine/selfupdate"
)

// daemonControlProd daemon 编排生产实现：serve.json 是唯一运行态真相。
//
// selfPath：**交换前捕获**的自身 exe 路径。必须预捕获——os.Executable()
// 在 Linux 读 /proc/self/exe，交换序列把运行中的 exe rename 成 .old 后，
// 该链接即指向 .old，重启 daemon 就会拉起旧版二进制（CI ubuntu 实证：
// 对账一直读到 "1.0.0"）。macOS proc_pidpath 同源风险；Windows PEB 路径
// 不受 rename 影响，但统一走本字段。
type daemonControlProd struct {
	dbPath   string
	selfPath string
}

// exePathForSpawn 拉起子进程用的 exe 路径（自更新场景必须是预捕获值）。
func (c daemonControlProd) exePathForSpawn() (string, error) {
	if c.selfPath != "" {
		return c.selfPath, nil
	}
	return os.Executable()
}

// pubKeyOverrideFile ldflags 注入（main 包符号；冒烟/演练构建用测试钥匙，
// 生产构建为空）。init 里转交 selfupdate 包（全路径 -X 在本工具链上
// 静默失效，故经 main 包中转——见 selfupdate/release.go 注释）。
var pubKeyOverrideFile string

func init() { selfupdate.SetPublicKeyOverride(pubKeyOverrideFile) }

func (c daemonControlProd) Alive() bool {
	d := loadDaemonState()
	return d != nil && serveAlive(d.Port)
}

func (c daemonControlProd) Stop() error {
	d := loadDaemonState()
	stopServe(d)
	if d == nil {
		return nil
	}
	// 等端口真正下来再返回：紧随其后的 Start 会打开同一个 SQLite——
	// 旧进程未死就 spawn 新 daemon 会撞库锁（CI ubuntu 实证）。
	// 优雅退出 >5s 时升级 SIGKILL：serve 有端口迁移逻辑，慢死的旧
	// serve 会让新 serve 迁去 +1 端口，对账就会轮询到垂死的旧进程
	// （CI ubuntu 实证："最后见到 1.0.0，期望 1.0.1"）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !serveAlive(d.Port) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	killServeHard(d.PID)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !serveAlive(d.Port) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func (c daemonControlProd) Start() error {
	d := loadDaemonState()
	port := 9801
	if d != nil && d.Port != 0 {
		port = d.Port
	}
	self, err := c.exePathForSpawn()
	if err != nil {
		return err
	}
	log.Printf("[selfupdate] 起 daemon: exe=%s port=%d", self, port)
	pid, err := spawnServeAt(self, port, c.dbPath)
	if err != nil {
		return err
	}
	return saveDaemonState(daemonState{PID: pid, Port: port, StartedAt: time.Now().Unix()})
}

// WaitVersion 轮询 /api/status 直到 version == want（token 走本机文件）。
// 每轮重读 serve.json 取**当前**端口：托管 daemon 端口冲突时会迁移
// （W4 CI windows 实证 19713→19714），写死的端口会一直轮空。
func (c daemonControlProd) WaitVersion(ctx context.Context, want string) error {
	tok, err := api.LoadOrCreateToken(api.DefaultTokenPath())
	if err != nil {
		return fmt.Errorf("读 token: %w", err)
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var last string
	var lastPort int
	n := 0
	for {
		d := loadDaemonState()
		if d == nil {
			select {
			case <-ctx.Done():
				return errors.New("serve.json 缺失，无法对账")
			case <-tick.C:
			}
			continue
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/api/status", d.Port)
		if d.Port != lastPort {
			log.Printf("[selfupdate] 对账端口 = %d（serve.json）", d.Port)
			lastPort = d.Port
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("X-GHydra-Token", tok)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var st struct {
					Version string `json:"version"`
				}
				if uerr := json.Unmarshal(body, &st); uerr == nil {
					if last == "" && st.Version != "" {
						log.Printf("[selfupdate] 对账首见 version=%q（期望 %q）", st.Version, want)
					}
					last = st.Version
					if st.Version == want {
						return nil
					}
				} else {
					err = uerr
				}
			} else {
				err = fmt.Errorf("状态码 %d", resp.StatusCode)
			}
		}
		if n%20 == 0 { // 10s 一条心跳观测（含失败原因）
			if err != nil {
				log.Printf("[selfupdate] 对账中：port=%d last=%q want=%q err=%v", lastPort, last, want, err)
			} else {
				log.Printf("[selfupdate] 对账中：port=%d last=%q want=%q", lastPort, last, want)
			}
		}
		n++
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon 版本对账超时（最后见到 %q，期望 %q）", last, want)
		case <-tick.C:
		}
	}
}

// installFiles 平台安装文件集（p1 仅主程序；p3 打包定型后 unix 加 ghydra-gui）。
func installFiles() []string {
	if runtime.GOOS == "windows" {
		return []string{selfupdate.ExeName}
	}
	return []string{selfupdate.ExeName}
}

// selfupdateStatePath 更新状态文件。
func selfupdateStatePath() string {
	d := ghydraDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, selfupdate.StateFileName)
}

// newSelfupdateUpdater 全量装配（check/apply 用：带调度器 A 通道 + B 镜像）。
// apiBase/trustHosts：镜像/演练注入（冒烟 CI 用 fake Release server）。
func newSelfupdateUpdater(dbPath, cdn, apiBase string, trustHosts map[string]bool) (*selfupdate.Updater, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	sp := selfupdateStatePath()
	if sp == "" {
		return nil, errors.New("HOME 不可用")
	}

	provider := rules.NewProvider(rules.DefaultRulesDir())
	m := provider.Snapshot().Matcher
	var pick func(host string) (string, bool)
	if sel, stop, err := startScheduler(m, dbPath); err == nil {
		defer stop()
		pick = func(host string) (string, bool) { return sel.Sched.Pick(host) }
	} else {
		log.Printf("[selfupdate] 调度器未启用（%v），A 通道走系统直连", err)
	}
	a := get.NewAFetcher(pick)
	var b get.Fetcher
	if cdn != "" {
		b = newBFetcherProd(cdn)
	}

	return &selfupdate.Updater{
		Version:      Version,
		APIBase:      apiBase,
		DL:           get.New(get.DefaultConfig(), a, b),
		TrustedHosts: trustHosts,
		FetchJSON: func(ctx context.Context, url string) ([]byte, int, error) {
			resp, err := a.Get(ctx, url, -1)
			if err != nil {
				return nil, 0, err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return body, resp.StatusCode, err
		},
		InstallDir:       filepath.Dir(self),
		StatePath:        sp,
		Files:            installFiles(),
		Daemon:           daemonControlProd{dbPath: dbPath, selfPath: self},
		SelfCheckTimeout: 30 * time.Second, // Defender 扫新 exe 可超 10s 默认值
		Log:              log.Printf,
	}, nil
}

// newBootUpdater 轻量装配（BootHook 专用：不起调度器——启动路径必须便宜；
// daemon 编排在状态指示"该在跑"时才触碰）。
func newBootUpdater(dbPath string) *selfupdate.Updater {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	sp := selfupdateStatePath()
	if sp == "" {
		return nil
	}
	return &selfupdate.Updater{
		Version:    Version,
		InstallDir: filepath.Dir(self),
		StatePath:  sp,
		Files:      installFiles(),
		Daemon:     daemonControlProd{dbPath: dbPath, selfPath: self},
		Log:        log.Printf,
	}
}

// bootSelfUpdate main 入口调用（serve 传 inServe=true）。
func bootSelfUpdate(dbPath string, inServe bool) {
	u := newBootUpdater(dbPath)
	if u == nil {
		return
	}
	if err := u.BootHook(inServe); err != nil {
		log.Printf("[selfupdate] %v", err)
	}
}

// versionCmd `ghydra version`（自更新自检的子进程契约：输出含版本串）。
func versionCmd(args []string) {
	fmt.Printf("ghydra version %s\n", Version)
}

// updateCmd `ghydra update [check|rollback]`。
func updateCmd(args []string) {
	sub := "apply"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	pre := fs.Bool("pre", false, "允许 prerelease（默认跳过）")
	downgrade := fs.Bool("allow-downgrade", false, "允许降级（逃生门，拍板 #2）")
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	cdn := fs.String("cdn", "https://gh-proxy.com/", "B 通道镜像前缀（空 = A-only）")
	apiBase := fs.String("api", "", "Releases API 覆盖（镜像/演练用；签名链不受影响）")
	trustHost := fs.String("trust-host", "", "额外信任资产域（镜像/演练用；sha256+minisign 仍强制）")
	_ = fs.Parse(reorderFlags(args, "pre", "allow-downgrade"))

	ensureReconcile(*dbPath)

	switch sub {
	case "check":
		u, err := newSelfupdateUpdater(*dbPath, *cdn, *apiBase, trustHostMap(*trustHost))
		if err != nil {
			log.Fatalf("装配: %v", err)
		}
		plan, err := u.Check(context.Background(), selfupdate.SelectOpts{AllowPrerelease: *pre, AllowDowngrade: *downgrade})
		switch {
		case errors.Is(err, selfupdate.ErrNoUpdate):
			fmt.Printf("已是最新（%s）\n", Version)
			return
		case errors.Is(err, selfupdate.ErrPrerelease):
			fmt.Println("最新为 prerelease，默认跳过（--pre 开启后可装）")
			return
		case errors.Is(err, selfupdate.ErrBadVersion):
			fmt.Println("最新版本曾启动失败被回滚，已跳过（等待更高版本）")
			return
		case err != nil:
			log.Fatalf("check: %v", err)
		}
		fmt.Printf("可更新: v%s（当前 v%s）\n%s\n资产: %s (%d bytes)\n%s\n",
			plan.Version, Version, firstLine(plan.Notes), plan.Archive.Name, plan.Archive.Size, plan.HTMLURL)
	case "rollback":
		u, err := newSelfupdateUpdater(*dbPath, "", "", nil)
		if err != nil {
			log.Fatalf("装配: %v", err)
		}
		if err := u.Rollback(); err != nil {
			log.Fatalf("rollback: %v", err)
		}
		fmt.Println("已回滚到上一版（.old 凭证）")
	default: // apply
		u, err := newSelfupdateUpdater(*dbPath, *cdn, *apiBase, trustHostMap(*trustHost))
		if err != nil {
			log.Fatalf("装配: %v", err)
		}
		plan, err := u.Check(context.Background(), selfupdate.SelectOpts{AllowPrerelease: *pre, AllowDowngrade: *downgrade})
		if err != nil {
			if errors.Is(err, selfupdate.ErrNoUpdate) {
				fmt.Printf("已是最新（%s）\n", Version)
				return
			}
			log.Fatalf("check: %v", err)
		}
		fmt.Printf("开始更新 v%s ← %s ...\n", plan.Version, plan.HTMLURL)
		if err := u.ApplyPlan(context.Background(), plan); err != nil {
			log.Fatalf("更新失败（旧版未受影响）: %v", err)
		}
		fmt.Printf("已更新到 v%s（自检通过）\n", plan.Version)
	}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// trustHostMap 逗号分隔 host → 白名单 map（空串 = nil = 冻结默认）。
func trustHostMap(s string) map[string]bool {
	if s == "" {
		return nil
	}
	m := map[string]bool{}
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			m[h] = true
		}
	}
	return m
}
