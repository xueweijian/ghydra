package main

// ghydra git — insteadOf 集成（M2-W4 / PRD F5 P0）
// enable: 写 url.insteadOf（fetch→CDN）+ url.pushInsteadOf（push→直连）
// disable: 按快照精确还原
// status: 当前键、冲突、改写演示

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/xueweijian/ghydra/engine/gitcfg"
	"github.com/xueweijian/ghydra/engine/store"
)

const storeGitKind = "git"

func gitCmd(args []string) {
	if len(args) == 0 {
		gitUsage()
		os.Exit(2)
	}
	dbPath := defaultDBPath()
	switch args[0] {
	case "enable":
		gitEnable(args[1:], dbPath)
	case "disable":
		gitDisable(dbPath)
	case "status":
		gitStatus(args[1:], dbPath)
	case "-h", "--help", "help":
		gitUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: ghydra git %s\n\n", args[0])
		gitUsage()
		os.Exit(2)
	}
}

func gitUsage() {
	fmt.Fprint(os.Stderr, `ghydra git — insteadOf 集成

用法:
  ghydra git enable --cdn <URL> [--push-via-b] [--ssh-rewrite] [--force] [--dry-run]
  ghydra git disable
  ghydra git status [--cdn <URL>]

说明:
  enable 写入 global gitconfig 两个改写键：
    url.<CDN+src>.insteadOf   → fetch/clone 走 CDN（B 通道）
    url.<src>.pushInsteadOf   → push 保持直连（--push-via-b 关闭）
  快照存本地库，disable 精确还原（只动 GHydra 写过的键）。
  检测到他人改写 github.com 的 url.* 键时拒绝（--force 覆盖并快照）。
`)
}

// parseGitFlags 解析 enable/status 共用 flag。flag 放子命令后（Go flag 包
// 首个非 flag 参数即停的坑在此不受影响——参数已由 gitCmd 剥离一层）。
type gitFlags struct {
	cdn        string
	pushViaB   bool
	sshRewrite bool
	force      bool
	dryRun     bool
}

func parseGitFlags(fs *flag.FlagSet, f *gitFlags) {
	fs.StringVar(&f.cdn, "cdn", os.Getenv("GHYDRA_CDN"), "B 通道 CDN 前缀（可含 token 路径），亦可用 GHYDRA_CDN 环境变量")
	fs.BoolVar(&f.pushViaB, "push-via-b", false, "push 也走 CDN（全阻断网络；默认 push 直连）")
	fs.BoolVar(&f.sshRewrite, "ssh-rewrite", false, "git@github.com: 形态的 fetch 也改写到 CDN")
	fs.BoolVar(&f.force, "force", false, "冲突时覆盖（原值入快照）")
	fs.BoolVar(&f.dryRun, "dry-run", false, "只打印将写入的键，不落盘")
}

func (f *gitFlags) opts() gitcfg.Options {
	return gitcfg.Options{CDN: f.cdn, PushViaB: f.pushViaB, SSHRewrite: f.sshRewrite}
}

func gitEnable(args []string, dbPath string) {
	fs := flag.NewFlagSet("git enable", flag.ExitOnError)
	var f gitFlags
	parseGitFlags(fs, &f)
	_ = fs.Parse(args)
	if f.cdn == "" {
		fmt.Fprintln(os.Stderr, "必须提供 --cdn（或 GHYDRA_CDN）")
		os.Exit(2)
	}
	if f.dryRun {
		fmt.Println("[dry-run] 将写入的键:")
		execr := gitcfg.CLIExec{}
		desired, err := gitcfg.Desired(f.opts())
		if err != nil {
			fmt.Fprintln(os.Stderr, "参数错误:", err)
			os.Exit(2)
		}
		for _, k := range desired {
			fmt.Printf("  %s = %s\n", k.Name, k.Value)
		}
		if existing, err := gitcfg.ReadURLKeys(execr); err == nil {
			_, _, conflicts := gitcfg.Plan(existing, desired)
			for _, c := range conflicts {
				fmt.Printf("  冲突: %s = %s\n", c.Desired.Name, c.Existing.Value)
			}
		}
		return
	}
	snap, err := gitEnableCore(dbPath, f.opts(), f.force) // 核（与 API 共用）
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("已写入 %d 个键（push=%s ssh=%s）:\n", len(snap.Written), pushPathName(f.pushViaB), sshRewriteName(f.sshRewrite))
	for _, k := range snap.Written {
		fmt.Printf("  %s = %s\n", k.Name, k.Value)
	}
	for name, vals := range snap.Prior {
		fmt.Printf("  快照原值: %s = %s\n", name, strings.Join(vals, ", "))
	}
	demo, _ := gitcfg.RewriteDemo(f.opts(), "https://github.com/cli/cli.git")
	fmt.Printf("示例: git clone https://github.com/cli/cli.git → %s\n", demo)
}

func pushPathName(viaB bool) string {
	if viaB {
		return "CDN"
	}
	return "直连"
}

func sshRewriteName(v bool) string {
	if v {
		return "改写"
	}
	return "不动"
}

func gitDisable(dbPath string) {
	n, err := gitDisableCore(dbPath) // 核（与 API 共用）
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if n == 0 {
		fmt.Println("没有 ghydra git 快照（可能未 enable 或已恢复）")
		return
	}
	fmt.Printf("已还原 %d 个键\n", n)
}

func gitStatus(args []string, dbPath string) {
	fs := flag.NewFlagSet("git status", flag.ExitOnError)
	var f gitFlags
	parseGitFlags(fs, &f)
	_ = fs.Parse(args)
	existing, err := gitcfg.ReadURLKeys(gitcfg.CLIExec{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 git config 失败:", err)
		os.Exit(1)
	}
	fmt.Println("global url.* 改写键:")
	if len(existing) == 0 {
		fmt.Println("  （无）")
	}
	for _, k := range existing {
		marker := ""
		if f.cdn != "" && gitcfg.IsManaged(k, f.opts()) {
			marker = "  ← ghydra"
		}
		fmt.Printf("  %s = %s%s\n", k.Name, k.Value, marker)
	}
	if f.cdn != "" {
		for _, u := range []string{"https://github.com/cli/cli.git", "git@github.com:cli/cli.git"} {
			if d, hit := gitcfg.RewriteDemo(f.opts(), u); hit {
				fmt.Printf("演示: %s → %s\n", u, d)
			}
		}
	}
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			if _, ok, _ := st.LoadManagedSnapshot(storeGitKind); ok {
				fmt.Println("快照: 存在（ghydra git disable 可还原）")
			}
			st.Close()
		}
	}
}
