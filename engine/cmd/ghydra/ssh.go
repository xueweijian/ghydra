package main

// ghydra ssh — ssh config 443 写入（M2-W4 / PRD F5 P0）
// 前置：github.com:22 不通而 ssh.github.com:443 通（doctor 探针复用），
// --assume-ok 跳过探针（离线规划用）。

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/sshcfg"
	"github.com/xueweijian/ghydra/engine/store"
)

const storeSSHKind = "ssh"

func sshCmd(args []string) {
	if len(args) == 0 {
		sshUsage()
		os.Exit(2)
	}
	dbPath := defaultDBPath()
	switch args[0] {
	case "enable":
		sshEnable(args[1:], dbPath)
	case "disable":
		sshDisable(args[1:], dbPath)
	case "status":
		sshStatus(args[1:])
	case "-h", "--help", "help":
		sshUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: ghydra ssh %s\n\n", args[0])
		sshUsage()
		os.Exit(2)
	}
}

func sshUsage() {
	fmt.Fprint(os.Stderr, `ghydra ssh — ssh config 443 写入

用法:
  ghydra ssh enable [--config <path>] [--alias] [--assume-ok] [--dry-run]
  ghydra ssh disable [--config <path>]
  ghydra ssh status [--config <path>]

说明:
  22 不通而 ssh.github.com:443 通时，把 github.com 的 SSH 引到 443。
  managed 块插文件顶部（ssh_config first-match-wins）；已有 Host github.com
  段时拒绝，--alias 写 Host github.com-b（不修改既有段，需自行改 remote）。
  首次写入前整文件字节快照，disable 还原。
`)
}

func sshConfigPath(fs *flag.FlagSet) string {
	var p string
	fs.StringVar(&p, "config", defaultSSHConfigPath(), "~/.ssh/config 路径")
	return p
}

func defaultSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".ssh", "config")
}

func sshEnable(args []string, dbPath string) {
	fs := flag.NewFlagSet("ssh enable", flag.ExitOnError)
	cfgPath := sshConfigPath(fs)
	alias := fs.Bool("alias", false, "冲突时写 Host github.com-b（不改既有段）")
	assumeOK := fs.Bool("assume-ok", false, "跳过 22/443 探针")
	dryRun := fs.Bool("dry-run", false, "只打印计划")
	_ = fs.Parse(args)

	if !*assumeOK {
		fmt.Println("探测 ssh.github.com:22 / :443 …")
		r := probe.New(probe.DefaultConfig())
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		rep := r.Run(ctx, probe.ModeDirect)
		ok22, ok443 := sshProbeResult(rep)
		if ok22 && ok443 {
			fmt.Println("github.com:22 本身可达——无需 443 优化，退出。")
			return
		}
		if ok22 && !ok443 {
			fmt.Fprintln(os.Stderr, "22 通但 443 不通，无需也不应写入。")
			os.Exit(1)
		}
		if !ok22 && !ok443 {
			fmt.Fprintln(os.Stderr, "22 与 443 均不通——SSH 场景无解（M1 口径：只探测不加速）。")
			os.Exit(1)
		}
		fmt.Printf("22 不通、443 通 → 可写入（config=%s alias=%v）\n", cfgPath, *alias)
	}
	bak := sshBackupPath(dbPath)
	plan, err := sshcfg.Enable(cfgPath, *alias, *dryRun, bak)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if !plan.Conflict {
			os.Exit(1)
		}
		os.Exit(2)
	}
	fmt.Printf("[%s] action=%s\n", dryRunTag(*dryRun), plan.Action)
	fmt.Println(plan.Content)
	if *dryRun {
		return
	}
	// 快照存 base64（store 文本列）；无原文件时存空串表示"disable 时无需还原"
	if dbPath != "" && plan.Backup != "" {
		if b, err := os.ReadFile(bak); err == nil {
			saveSSHSnapshot(dbPath, base64.StdEncoding.EncodeToString(b))
		}
	} else if dbPath != "" && plan.Backup == "" {
		saveSSHSnapshot(dbPath, "") // 新建文件场景
	}
}

func dryRunTag(d bool) string {
	if d {
		return "dry-run"
	}
	return "done"
}

func saveSSHSnapshot(dbPath, b64 string) {
	if st, err := store.Open(dbPath); err == nil {
		if err := st.SaveManagedSnapshot(storeSSHKind, b64); err != nil {
			fmt.Fprintln(os.Stderr, "警告: ssh 快照保存失败:", err)
		}
		st.Close()
	}
}

func sshBackupPath(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "ssh_config.ghydra.bak")
}

func sshDisable(args []string, dbPath string) {
	fs := flag.NewFlagSet("ssh disable", flag.ExitOnError)
	cfgPath := sshConfigPath(fs)
	dryRun := fs.Bool("dry-run", false, "只打印计划")
	_ = fs.Parse(args)

	// 快照优先（字节级）
	snapData := ""
	hasSnap := false
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			if p, ok, _ := st.LoadManagedSnapshot(storeSSHKind); ok {
				snapData = p
				hasSnap = true
			}
			st.Close()
		}
	}
	bak := sshBackupPath(dbPath)
	if hasSnap {
		b, err := base64.StdEncoding.DecodeString(snapData)
		if err == nil {
			if err := os.WriteFile(bak, b, 0o600); err == nil {
				plan, err := sshcfg.Disable(cfgPath, bak, *dryRun)
				if err != nil {
					fmt.Fprintln(os.Stderr, "还原失败（快照保留）:", err)
					os.Exit(1)
				}
				fmt.Printf("[%s] action=%s（字节级快照还原）\n", dryRunTag(*dryRun), plan.Action)
				if !*dryRun && dbPath != "" {
					if st, err := store.Open(dbPath); err == nil {
						_ = st.DeleteManagedSnapshot(storeSSHKind)
						st.Close()
					}
				}
				return
			}
		}
	}
	plan, err := sshcfg.Disable(cfgPath, bak, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("[%s] action=%s\n", dryRunTag(*dryRun), plan.Action)
	if plan.Action == "remove" {
		fmt.Println(plan.Content)
	}
}

func sshStatus(args []string) {
	fs := flag.NewFlagSet("ssh status", flag.ExitOnError)
	cfgPath := sshConfigPath(fs)
	_ = fs.Parse(args)
	en, al, err := sshcfg.Effective(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch {
	case en && al:
		fmt.Printf("%s: ghydra managed（alias 模式 Host %s）\n", cfgPath, sshcfg.AliasHost)
	case en:
		fmt.Printf("%s: ghydra managed（Host github.com → ssh.github.com:443）\n", cfgPath)
	default:
		fmt.Printf("%s: 无 ghydra managed 块\n", cfgPath)
	}
}

// sshProbeResult 从 doctor 报告提取 ssh 两个 TCP 子检查。
func sshProbeResult(rep probe.Report) (ok22, ok443 bool) {
	for _, sc := range rep.Scenarios {
		if sc.Scenario != "ssh" {
			continue
		}
		for _, c := range sc.Checks {
			if c.Name == "ssh-22" {
				ok22 = c.OK
			}
			if c.Name == "ssh-443" {
				ok443 = c.OK
			}
		}
	}
	return ok22, ok443
}
