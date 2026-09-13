package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/get"
)

// DefaultAPIBase Releases API 基址（repo 属主硬编码——U3 源替换防线之一）。
const DefaultAPIBase = "https://api.github.com/repos/xueweijian/ghydra"

// DaemonControl daemon 重启编排的抽象（实现在 cmd 装配层：serve.json +
// spawn + /api/status 探活；测试注入 fake）。
type DaemonControl interface {
	Alive() bool
	Stop() error
	Start() error
	// WaitVersion 轮询 /api/status 直到 version == want 或超时。
	WaitVersion(ctx context.Context, want string) error
}

// Updater 一次自更新会话的全部依赖（显式注入，零全局）。
type Updater struct {
	Version    string // 当前版本（无 v 前缀；CLI 装配层传 main.Version）
	APIBase    string // 空 = DefaultAPIBase
	HTTP       *http.Client
	DL         *get.Downloader
	InstallDir string
	StatePath  string
	Files      []string // 安装文件集（win: [ghydra.exe]；unix: [ghydra, ghydra-gui]）
	Daemon     DaemonControl
	Pub        ed25519.PublicKey // 空 = FrozenPublicKey()
	Log        func(format string, args ...any)
	// TrustedHosts 资产域白名单覆盖（仅集成测试用 fake server 时设置）。
	TrustedHosts map[string]bool
	// FetchJSON Releases API 拉取注入（生产：A 通道 IP 直连；默认：u.HTTP）。
	// 返回 (body, statusCode, error)。
	FetchJSON func(ctx context.Context, url string) ([]byte, int, error)
	// SelfCheckExtraEnv 自检子进程附加环境（测试注入 fake 行为用）。
	SelfCheckExtraEnv []string
	// SelfCheckTimeout 单次自检子进程超时（零 = 10s）。
	SelfCheckTimeout time.Duration
}

func (u *Updater) logf(format string, args ...any) {
	if u.Log != nil {
		u.Log("[selfupdate] "+format, args...)
	}
}

func (u *Updater) pub() ed25519.PublicKey {
	if u.Pub != nil {
		return u.Pub
	}
	return FrozenPublicKey()
}

// Check 拉取 latest Release 并过全部信任/版本门槛，产出可执行 Plan。
func (u *Updater) Check(ctx context.Context, opts SelectOpts) (Plan, error) {
	base := u.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	if u.HTTP == nil {
		u.HTTP = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/releases/latest", nil)
	if err != nil {
		return Plan{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	var (
		body   []byte
		status int
	)
	if u.FetchJSON != nil {
		body, status, err = u.FetchJSON(ctx, req.URL.String())
	} else {
		if u.HTTP == nil {
			u.HTTP = http.DefaultClient
		}
		resp, derr := u.HTTP.Do(req)
		if derr != nil {
			return Plan{}, fmt.Errorf("selfupdate: Releases API: %w", derr)
		}
		defer resp.Body.Close()
		status = resp.StatusCode
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	if err != nil {
		return Plan{}, fmt.Errorf("selfupdate: Releases API: %w", err)
	}
	if status != http.StatusOK {
		return Plan{}, fmt.Errorf("selfupdate: Releases API 状态码 %d", status)
	}
	data := body
	rel, err := ParseLatestRelease(data)
	if err != nil {
		return Plan{}, err
	}
	st, _, err := LoadState(u.StatePath)
	if err != nil {
		return Plan{}, err
	}
	opts.State = st
	opts.TrustedHosts = u.TrustedHosts
	return SelectAsset(rel, runtime.GOOS, runtime.GOARCH, u.Version, opts)
}

// ApplyPlan 全链路：下载三件套 → 双防线验签 → 解包 staging → 交换 →
// 状态落盘 → 内联自检（exe + daemon）→ 失败同步回滚（"永不自毁"）。
func (u *Updater) ApplyPlan(ctx context.Context, plan Plan) error {
	if u.DL == nil {
		return errors.New("selfupdate: Downloader 未装配")
	}
	staging := filepath.Join(u.InstallDir, StagingDirName)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("selfupdate: 清 staging: %w", err)
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("selfupdate: 建 staging: %w", err)
	}

	// 1. checksums + minisig（小文件，直接 Downloader；sha256 稍后统一验）
	csPath := filepath.Join(staging, "checksums.txt")
	if _, err := u.DL.Get(ctx, plan.Checksums.URL, csPath); err != nil {
		return fmt.Errorf("selfupdate: 下载 checksums: %w", err)
	}
	sigPath := filepath.Join(staging, "checksums.txt.minisig")
	if _, err := u.DL.Get(ctx, plan.Minisig.URL, sigPath); err != nil {
		return fmt.Errorf("selfupdate: 下载 minisig: %w", err)
	}
	csData, err := os.ReadFile(csPath)
	if err != nil {
		return err
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return err
	}
	// U1 第二防线：checksums 本身必须 minisign 验签通过
	if err := VerifyChecksumsSignature(u.pub(), sigData, csData); err != nil {
		return fmt.Errorf("selfupdate: checksums 验签失败（疑似投毒，拒）: %w", err)
	}
	cs, err := ParseChecksums(csData)
	if err != nil {
		return err
	}

	// 2. archive：下载 → 尺寸（U4）→ sha256（U1 第一防线）→ 解包
	arcPath := filepath.Join(staging, plan.Archive.Name)
	if _, err := u.DL.Get(ctx, plan.Archive.URL, arcPath); err != nil {
		return fmt.Errorf("selfupdate: 下载 %s: %w", plan.Archive.Name, err)
	}
	arcData, err := os.ReadFile(arcPath)
	if err != nil {
		return err
	}
	if plan.Archive.Size > 0 && int64(len(arcData)) != plan.Archive.Size {
		return fmt.Errorf("%w: %s 声明 %d 实得 %d", ErrSizeMismatch, plan.Archive.Name, plan.Archive.Size, len(arcData))
	}
	if int64(len(arcData)) > MaxAssetSize {
		return fmt.Errorf("%w: %d 字节", ErrSizeOverLimit, len(arcData))
	}
	if err := VerifyArchive(cs, plan.Archive.Name, arcData); err != nil {
		return fmt.Errorf("selfupdate: 资产校验失败（疑似投毒，拒）: %w", err)
	}
	if err := unpackArchive(arcPath, staging, u.Files); err != nil {
		return fmt.Errorf("selfupdate: 解包: %w", err)
	}

	// 3. 交换 + 状态（pending 未确认）
	daemonWas := u.Daemon != nil && u.Daemon.Alive()
	ops, err := PlanSwap(u.InstallDir, u.Files)
	if err != nil {
		return err
	}
	if err := ExecuteSwap(u.InstallDir, ops); err != nil {
		return err
	}
	u.logf("已交换到 v%s（%d 个文件），进入自检", plan.Version, len(u.Files))

	st := &State{PendingVersion: plan.Version, DaemonWasRunning: daemonWas, AppliedAt: time.Now().UTC()}
	if err := u.saveStateMerge(st); err != nil {
		return err
	}

	// 4. 内联自检：exe version → daemon 重启对账；任何失败同步回滚
	if err := u.selfCheck(plan.Version); err != nil {
		return u.rollbackSync(plan.Version, daemonWas, fmt.Errorf("自检失败: %w", err))
	}
	if daemonWas {
		if err := u.restartDaemonAndWait(ctx, plan.Version); err != nil {
			return u.rollbackSync(plan.Version, daemonWas, fmt.Errorf("daemon 对账失败: %w", err))
		}
	}
	st.Confirm()
	if err := u.saveStateMerge(st); err != nil {
		return err
	}
	u.logf("v%s 更新完成%s", plan.Version, daemonStatusSuffix(daemonWas))
	return nil
}

// Rollback 手动回滚（ghydra update rollback 子命令 / bad 逃生门）。
func (u *Updater) Rollback() error {
	if err := RollbackSwap(u.InstallDir, u.Files); err != nil {
		return err
	}
	st, _, _ := LoadState(u.StatePath)
	if st == nil {
		st = &State{}
	}
	if st.PendingVersion != "" {
		st.MarkRolledBack(st.PendingVersion)
	}
	u.logf("已回滚到上一版（.old 凭证）")
	if st.DaemonWasRunning && u.Daemon != nil {
		if !u.Daemon.Alive() {
			_ = u.Daemon.Stop()
			_ = u.Daemon.Start()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := u.Daemon.WaitVersion(ctx, u.Version); err != nil {
				u.logf("回滚后 daemon 对账: %v", err)
			}
		}
	}
	return u.saveStateMerge(st)
}

// BootHook 启动入口（main 子命令路由之前调用，幂等）：
// 崩溃清扫 → pending 未确认则自检 → 失败×3 自动回滚（拍板 #2）。
// inServe：当前进程是否 serve（daemon）自身——serve 存活即新版可用，
// 不额外 spawn；CLI 路径若 daemon 该在而未在则拉起。
func (u *Updater) BootHook(inServe bool) error {
	actions, err := RecoverFromCrash(u.InstallDir, u.Files)
	if err != nil {
		return err
	}
	st, exists, err := LoadState(u.StatePath)
	if err != nil || !exists || st == nil {
		return err
	}
	if len(actions) > 0 && st.ShouldSelfCheck() {
		// 交换序列中断被恢复（exe 已回到旧版）→ 该次更新判失败
		pending := st.PendingVersion
		st.MarkRolledBack(pending)
		u.logf("检测到上次更新中断（已恢复旧版），v%s 标记跳过", pending)
		return u.saveStateMerge(st)
	}
	if !st.ShouldSelfCheck() {
		if !inServe && st.DaemonWasRunning && u.Daemon != nil && !u.Daemon.Alive() {
			u.logf("daemon 应在而未在，拉起")
			if err := u.Daemon.Start(); err != nil {
				u.logf("拉起 daemon: %v", err)
			}
		}
		return nil
	}
	if err := u.selfCheck(st.PendingVersion); err != nil {
		u.logf("启动自检失败: %v", err)
		if st.RecordBootFailure() {
			u.logf("连续 %d 次失败，自动回滚（拍板 #2）", st.BootAttempts)
			if err := u.rollbackSync(st.PendingVersion, st.DaemonWasRunning && u.Daemon != nil, err); err != nil {
				return err
			}
			return errors.New("selfupdate: 新版连续自检失败，已自动回滚")
		}
		return u.saveStateMerge(st)
	}
	st.Confirm()
	u.logf("v%s 自检通过", st.PendingVersion)
	if err := u.saveStateMerge(st); err != nil {
		return err
	}
	if !inServe && st.DaemonWasRunning && u.Daemon != nil && !u.Daemon.Alive() {
		if err := u.Daemon.Start(); err != nil {
			u.logf("拉起 daemon: %v", err)
		}
	}
	return nil
}

// rollbackSync 同步回滚：swap 逆转 + 坏版留档 + daemon 回旧版 + bad_version。
func (u *Updater) rollbackSync(badVersion string, daemonWas bool, cause error) error {
	u.logf("回滚 %s: %v", badVersion, cause)
	rbErr := RollbackSwap(u.InstallDir, u.Files)
	st, _, _ := LoadState(u.StatePath)
	if st == nil {
		st = &State{}
	}
	st.MarkRolledBack(badVersion)
	if daemonWas && u.Daemon != nil {
		_ = u.Daemon.Stop()
		_ = u.Daemon.Start()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := u.Daemon.WaitVersion(ctx, u.Version); err != nil {
			u.logf("回滚后旧 daemon 对账: %v", err)
		}
	}
	if err := u.saveStateMerge(st); err != nil {
		return err
	}
	if rbErr != nil {
		return fmt.Errorf("selfupdate: 回滚失败（需手动 `ghydra update rollback`）: %w", rbErr)
	}
	return fmt.Errorf("selfupdate: 已回滚到上一版（原因: %v）", cause)
}

// selfCheck 拉子进程 `<installDir>/<ExeName> version`，输出须含目标版本。
func (u *Updater) selfCheck(wantVersion string) error {
	exe := filepath.Join(u.InstallDir, ExeName)
	timeout := u.SelfCheckTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "version")
	// 递归护栏：子进程的 BootHook 会看到 pending 未确认 → 再自检 → 套娃。
	cmd.Env = append(append(os.Environ(), u.SelfCheckExtraEnv...), "GHYDRA_SELF_CHECK_CHILD=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("子进程 %s version: %w（输出: %.120s）", ExeName, err, out.String())
	}
	if !strings.Contains(out.String(), wantVersion) {
		return fmt.Errorf("版本不匹配: 期望 %s，输出 %.120s", wantVersion, out.String())
	}
	return nil
}

func (u *Updater) restartDaemonAndWait(ctx context.Context, want string) error {
	if u.Daemon == nil {
		return errors.New("selfupdate: DaemonControl 未装配")
	}
	if err := u.Daemon.Stop(); err != nil {
		u.logf("停旧 daemon: %v（可能已退出）", err)
	}
	if err := u.Daemon.Start(); err != nil {
		return fmt.Errorf("起新 daemon: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return u.Daemon.WaitVersion(waitCtx, want)
}

// saveStateMerge 保留既有 bad_version/confirmed 语义字段（State 里可跨
// 更新存续的只有 bad_version；pending/confirmed/attempts 属于单次会话）。
func (u *Updater) saveStateMerge(st *State) error {
	if old, exists, err := LoadState(u.StatePath); err == nil && exists && old != nil {
		if st.BadVersion == "" {
			st.BadVersion = old.BadVersion
		}
	}
	return SaveState(u.StatePath, st)
}

func daemonStatusSuffix(was bool) string {
	if was {
		return "（daemon 已重启对账）"
	}
	return ""
}
