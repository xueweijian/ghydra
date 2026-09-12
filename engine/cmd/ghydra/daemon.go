package main

// daemon.go —— ghydra on/off 的守护装配（M1-W3）。
//
// 设计（GHydra-M1-Plan §4 W3 + D4）：
//   - on: 崩溃对账 → 快照原值 → 后台拉起 serve（--managed）→ 探活
//     → 接管系统代理（PAC 优先）→ 写 serve.json（pid/port）
//   - off: 恢复快照 → 删快照 → 停 serve → 清 serve.json
//   - serve --managed 退出 hook 恢复代理；kill -9 残留由下次任何
//     命令的 ensureReconcile 对账兜底（快照在 + 端口死 = 残留）
//   - 进程活性用端口 TCP 探活判定（避开跨平台进程 API 差异）

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
	"github.com/xueweijian/ghydra/engine/sysproxy"
)

// daemonState serve 运行态（~/.ghydra/serve.json）。
type daemonState struct {
	PID       int   `json:"pid"`
	Port      int   `json:"port"`
	StartedAt int64 `json:"started_at"`
}

func ghydraDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra")
}

func daemonStatePath() string {
	d := ghydraDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "serve.json")
}

func loadDaemonState() *daemonState {
	p := daemonStatePath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var st daemonState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

func saveDaemonState(st daemonState) error {
	p := daemonStatePath()
	if p == "" {
		return fmt.Errorf("HOME 不可用")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	return os.WriteFile(p, b, 0o644)
}

func removeDaemonState() {
	if p := daemonStatePath(); p != "" {
		os.Remove(p)
	}
}

// serveAlive 端口探活（TCP 可连即活）。
func serveAlive(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ensureReconcile 崩溃对账：快照存在 + serve 已死 = 上代进程没走
// 退出 hook（kill -9 / 断电）→ 恢复原值。任何 ghydra 命令入口调用。
func ensureReconcile(dbPath string) {
	if dbPath == "" {
		return
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer st.Close()
	ps, pac, ok, err := st.LoadSnapshot()
	if err != nil || !ok {
		return
	}
	if d := loadDaemonState(); d != nil && serveAlive(d.Port) {
		return // serve 正常运行中，快照属于当前会话
	}
	orig := sysproxy.Setting{ProxyServer: ps, PACURL: pac}
	if err := sysproxy.Apply(orig); err != nil {
		log.Printf("[reconcile] 恢复原值失败（%v），请手动检查系统代理设置", err)
		return
	}
	st.DeleteSnapshot()
	removeDaemonState()
	if orig.IsZero() {
		log.Printf("[reconcile] 检测到崩溃残留的系统代理，已清除（恢复直连）")
	} else {
		log.Printf("[reconcile] 检测到崩溃残留的系统代理，已恢复原值: %s", orig)
	}
}

// spawnServe 后台拉起 serve（脱离终端会话）。
func spawnServe(port int, dbPath string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	args := []string{"serve", "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--doctor-interval", "1h"}
	if dbPath != "" {
		args = append(args, "--db", dbPath)
	}
	args = append(args, "--managed")
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = detachAttr() // 平台差异见 spawn_{windows,unix}.go
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait() // 回收子进程资源（不阻塞）
	return cmd.Process.Pid, nil
}

// stopServe 停止 serve（unix SIGTERM 优雅 / windows 强杀——off 已先
// 恢复代理，强杀无副作用）。
func stopServe(st *daemonState) {
	if st == nil {
		return
	}
	killServe(st.PID) // 平台差异见 spawn_{windows,unix}.go
}

// onCmd 接管系统代理 + 拉起 serve。
func onCmd(args []string) {
	fs := flag.NewFlagSet("on", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	port := fs.Int("port", 9801, "主端口（被占自动迁移 +1..+8）")
	mode := fs.String("mode", "pac", "代理模式: pac（推荐）| proxy（手动代理）")
	_ = fs.Parse(args)

	ensureReconcile(*dbPath)

	if d := loadDaemonState(); d != nil && serveAlive(d.Port) {
		log.Fatalf("serve 已在运行（端口 %d）。先执行 ghydra off", d.Port)
	}

	// 端口迁移探测（serve 实际监听时再次 fallback，这里提前选定）
	chosen := *port
	for i := 0; i < 9; i++ {
		if !serveAlive(chosen) {
			break
		}
		chosen++
	}
	if chosen != *port {
		log.Printf("端口 %d 被占，迁移到 %d", *port, chosen)
	}

	// 快照原值（Current 失败继续——Linux 无桌面场景 PAC 接管降级提示）
	if cur, err := sysproxy.Current(); err == nil {
		if st, err := store.Open(*dbPath); err == nil {
			if err := st.SaveSnapshot(cur.ProxyServer, cur.PACURL); err != nil {
				log.Fatalf("快照保存失败: %v", err)
			}
			st.Close()
		}
	} else {
		log.Printf("读取当前系统代理失败（%v）——快照跳过，off 时将直接清除", err)
	}

	pid, err := spawnServe(chosen, *dbPath)
	if err != nil {
		log.Fatalf("拉起 serve 失败: %v", err)
	}
	// 探活（serve 含自举链预筛，最多等 15s）
	alive := false
	for i := 0; i < 75; i++ {
		if serveAlive(chosen) {
			alive = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !alive {
		stopServe(&daemonState{PID: pid})
		log.Fatalf("serve 15s 内未就绪（pid %d），已回滚。请前台运行 ghydra serve 查看日志", pid)
	}
	if err := saveDaemonState(daemonState{PID: pid, Port: chosen, StartedAt: time.Now().Unix()}); err != nil {
		log.Printf("serve.json 写入失败: %v", err)
	}

	// 接管
	var setting sysproxy.Setting
	if *mode == "proxy" {
		setting = sysproxy.Setting{ProxyServer: fmt.Sprintf("127.0.0.1:%d", chosen)}
	} else {
		setting = sysproxy.Setting{PACURL: fmt.Sprintf("http://127.0.0.1:%d/pac", chosen)}
	}
	if err := sysproxy.Apply(setting); err != nil {
		// 回滚：停 serve + 删快照，不留半接管状态（D4）
		stopServe(&daemonState{PID: pid})
		removeDaemonState()
		if st, err := store.Open(*dbPath); err == nil {
			st.DeleteSnapshot()
			st.Close()
		}
		log.Fatalf("系统代理设置失败，已回滚: %v", err)
	}

	fmt.Printf("GHydra 已接管（%s，端口 %d，pid %d）。\n  GitHub 流量经本地代理加速；ghydra off 恢复。\n", *mode, chosen, pid)
}

// offCmd 恢复系统代理 + 停 serve。
func offCmd(args []string) {
	fs := flag.NewFlagSet("off", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	_ = fs.Parse(args)

	ensureReconcile(*dbPath)

	// 先恢复代理（浏览器立即回到直连，不等 serve 退出）
	if *dbPath != "" {
		if st, err := store.Open(*dbPath); err == nil {
			if ps, pac, ok, _ := st.LoadSnapshot(); ok {
				orig := sysproxy.Setting{ProxyServer: ps, PACURL: pac}
				if err := sysproxy.Apply(orig); err != nil {
					// 恢复失败不能删快照；下次命令/用户手动修复环境后
					// 仍需有机会重试（与 ensureReconcile 一致）。
					log.Printf("恢复原值失败: %v（原值 %s）；保留快照重试", err, orig)
				} else {
					st.DeleteSnapshot()
				}
			}
			st.Close()
		}
	}

	d := loadDaemonState()
	stopServe(d)
	removeDaemonState()
	fmt.Println("GHydra 已退出，系统代理已恢复。")
}

// daemonStatusJSON /status 端点的数据源由 serveCmd 装配（需要调度器
// 实例）；此处只提供 HTTP 帮助函数。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
