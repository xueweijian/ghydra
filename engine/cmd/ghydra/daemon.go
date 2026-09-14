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
	"encoding/base64"
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

	"github.com/xueweijian/ghydra/engine/gitcfg"
	"github.com/xueweijian/ghydra/engine/sshcfg"
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
	psJSON, ok, err := st.LoadSnapshotJSON()
	if err != nil || !ok {
		return
	}
	if d := loadDaemonState(); d != nil && serveAlive(d.Port) {
		return // serve 正常运行中，快照属于当前会话
	}
	var orig sysproxy.Setting
	if json.Unmarshal([]byte(psJSON), &orig) != nil {
		log.Printf("[reconcile] 快照损坏，清除代理并放弃恢复（原文: %s）", psJSON)
		sysproxy.Clear()
		st.DeleteSnapshot()
		removeDaemonState()
		return
	}
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
	return spawnServeAt(self, port, dbPath)
}

// spawnServeAt 用显式 exe 路径拉起托管 serve（自更新后必须传交换前捕获的
// 路径，见 daemonControlProd 注释）。
func spawnServeAt(self string, port int, dbPath string) (int, error) {
	args := []string{"serve", "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--doctor-interval", "1h"}
	if dbPath != "" {
		args = append(args, "--db", dbPath)
	}
	args = append(args, "--managed")
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = detachAttr() // 平台差异见 spawn_{windows,unix}.go
	// 托管 serve 的输出落盘（append）：detached 子进程默认 /dev/null——
	// daemon 是产品常驻进程，日志必须可追溯（W4 CI 考古同样受益）。
	// ⚠️ fd 生命周期：必须在 Start() 之后才关父进程这份拷贝——提前关会
	// 让 fork/exec 拿到已关闭的 fd（macOS 实证 "bad file descriptor"）。
	var logFile *os.File
	if d := ghydraDir(); d != "" {
		if err := os.MkdirAll(d, 0o755); err == nil {
			if f, ferr := os.OpenFile(filepath.Join(d, "serve.log"),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
				cmd.Stdout, cmd.Stderr = f, f
				logFile = f
			}
		}
	}
	err := cmd.Start()
	if logFile != nil {
		logFile.Close() // 子进程已持有自己的 fd 副本
	}
	if err != nil {
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

	// 接管（核：快照 + 应用 + 失败回滚本次快照——与 API /api/on 同一份实现）
	if err := applyTakeover(*dbPath, *mode, chosen); err != nil {
		// 回滚：停 serve + 删 serve.json，不留半接管状态（D4）
		stopServe(&daemonState{PID: pid})
		removeDaemonState()
		log.Fatalf("系统代理设置失败，已回滚: %v", err)
	}

	fmt.Printf("GHydra 已接管（%s，端口 %d，pid %d）。\n  GitHub 流量经本地代理加速；ghydra off 恢复。\n", *mode, chosen, pid)
}

// offCmd 恢复系统代理 + 停 serve。
func offCmd(args []string) {
	fs := flag.NewFlagSet("off", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite 路径")
	waitFlag := fs.Bool("wait", false, "等待 serve 进程死透（卸载器用；超时 10s 强杀兜底）")
	_ = fs.Parse(args)

	ensureReconcile(*dbPath)

	// 核：恢复快照 + 还原 git/ssh 托管（与 API /api/off 同一份实现）。
	// 恢复失败保留快照重试（下次命令/用户修复环境后仍有机会）。
	relErr := releaseTakeover(*dbPath)
	if relErr != nil {
		log.Printf("恢复未完成: %v（保留快照，可重试 ghydra off）", relErr)
	}

	d := loadDaemonState()
	stopServe(d)
	if *waitFlag && d != nil {
		// NSIS 卸载序列（D4 安全关键）：--wait 等进程死透再返回，
		// 否则 Windows 下 exe 被运行中进程锁定，卸载器删不掉。
		deadline := time.Now().Add(10 * time.Second)
		for procAlive(d.PID) && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if procAlive(d.PID) {
			killServeHard(d.PID) // 超时强杀兜底
			time.Sleep(500 * time.Millisecond)
		}
	}
	removeDaemonState()
	fmt.Println("GHydra 已退出，系统代理已恢复。")
	if code := offExitCode(relErr, *waitFlag); code != 0 {
		os.Exit(code)
	}
}

// offExitCode off 的退出码契约：--wait（卸载器路径）下恢复失败必须非零
// （NSIS 据此弹窗中止——绝不静默留下半接管态）；交互模式保持历史语义
// 退出 0（错误已打印，快照保留可重试）。
func offExitCode(releaseErr error, wait bool) int {
	if releaseErr != nil && wait {
		return 3
	}
	return 0
}

// restoreManagedOnOff off 路径的 git/ssh 快照还原。失败仅告警不阻塞
// （sysproxy 已恢复；用户可 ghydra git disable / ssh disable 单独重试）。
func restoreManagedOnOff(dbPath string) {
	if dbPath == "" {
		return
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer st.Close()
	if payload, ok, _ := st.LoadManagedSnapshot(storeGitKind); ok {
		var snap gitcfg.Snapshot
		if json.Unmarshal([]byte(payload), &snap) == nil {
			if err := gitcfg.Disable(gitcfg.CLIExec{}, snap); err == nil {
				_ = st.DeleteManagedSnapshot(storeGitKind)
				log.Printf("git insteadOf 键已还原（%d 个）", len(snap.Written))
			} else {
				log.Printf("git 键还原失败（快照保留，可 ghydra git disable 重试）: %v", err)
			}
		} else {
			log.Printf("git 快照损坏，跳过自动还原（可 ghydra git status 查看）")
		}
	}
	if payload, ok, _ := st.LoadManagedSnapshot(storeSSHKind); ok {
		b, err := base64.StdEncoding.DecodeString(payload)
		bak := sshBackupPath(dbPath)
		if err == nil && os.WriteFile(bak, b, 0o600) == nil {
			cfgPath := defaultSSHConfigPath()
			if _, err := sshcfg.Disable(cfgPath, bak, false); err == nil {
				_ = st.DeleteManagedSnapshot(storeSSHKind)
				log.Printf("ssh config 已还原（%s）", cfgPath)
			} else {
				log.Printf("ssh config 还原失败（可 ghydra ssh disable 重试）: %v", err)
			}
		}
	}
}

// daemonStatusJSON /status 端点的数据源由 serveCmd 装配（需要调度器
// 实例）；此处只提供 HTTP 帮助函数。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
