package main

// apicore.go —— on/off/git/ssh/doctor 的核心逻辑（M3-W1）。
//
// 单一实现、双入口：CLI 命令（人读输出 + flag 解析）与本机 API
// （JSON）共用这里的核，防止两条入口行为漂移（W1-Design §4）。
// 约定：核函数不做 os.Exit / 不直接 fmt 用户文案；错误返回
// api.UserError（→400）或普通 error（→500）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/gitcfg"
	"github.com/xueweijian/ghydra/engine/probe"
	"github.com/xueweijian/ghydra/engine/sshcfg"
	"github.com/xueweijian/ghydra/engine/store"
	"github.com/xueweijian/ghydra/engine/sysproxy"
)

// ---- 系统代理接管（POST /api/on）----

// applyTakeover 快照原值（仅当无快照）+ 应用接管；失败回滚本次新建的
// 快照（已有快照 = 重复 on，失败不动它——那是上次接管的恢复凭证）。
// mode: "pac"（默认）| "proxy"。daemon 视角：serve 已在监听。
func applyTakeover(dbPath, mode string, port int) error {
	var hasSnap bool
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			_, hasSnap, _ = st.LoadSnapshotJSON()
			if !hasSnap {
				if cur, cerr := sysproxy.Current(); cerr == nil {
					if b, merr := json.Marshal(cur); merr == nil {
						if serr := st.SaveSnapshotJSON(string(b)); serr != nil {
							st.Close()
							return fmt.Errorf("快照保存失败: %w", serr)
						}
					}
				} else {
					log.Printf("[api] 读取当前系统代理失败（%v）——快照跳过，off 时将直接清除", cerr)
				}
			}
			st.Close()
		}
	}

	var setting sysproxy.Setting
	if mode == "proxy" {
		setting = sysproxy.Setting{ProxyServer: fmt.Sprintf("127.0.0.1:%d", port), ProxyEnabled: true}
	} else {
		setting = sysproxy.Setting{PACURL: fmt.Sprintf("http://127.0.0.1:%d/pac", port)}
	}
	if err := sysproxy.Apply(setting); err != nil {
		if !hasSnap && dbPath != "" { // 回滚本次新建的快照
			if st, e := store.Open(dbPath); e == nil {
				st.DeleteSnapshot()
				st.Close()
			}
		}
		return fmt.Errorf("系统代理设置失败: %w", err)
	}
	return nil
}

// releaseTakeover 恢复快照原值 + 还原 git/ssh 托管快照（不碰 serve
// 进程与 serve.json——进程生命周期归调用方）。返回聚合错误。
func releaseTakeover(dbPath string) error {
	var errs []error
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			psJSON, ok, _ := st.LoadSnapshotJSON()
			if ok {
				var orig sysproxy.Setting
				if json.Unmarshal([]byte(psJSON), &orig) == nil {
					if err := sysproxy.Apply(orig); err != nil {
						errs = append(errs, fmt.Errorf("恢复系统代理失败: %w", err))
					} else {
						st.DeleteSnapshot()
					}
				} else {
					sysproxy.Clear() // 快照损坏：清除代理不留接管态
					st.DeleteSnapshot()
				}
			}
			st.Close()
		}
	}
	restoreManagedOnOff(dbPath) // git/ssh 托管还原（内部已容忍失败）
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// ---- git（POST /api/git/*）----

// gitEnableCore 参数校验 + 写键 + 落快照。opts.CDN 必填。
func gitEnableCore(dbPath string, opts gitcfg.Options, force bool) (gitcfg.Snapshot, error) {
	if opts.CDN == "" {
		return gitcfg.Snapshot{}, api.UserError("cdn required（--cdn 或请求体 cdn 字段）")
	}
	snap, err := gitcfg.Enable(gitcfg.CLIExec{}, opts, force)
	if err != nil {
		return snap, err
	}
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			if b, merr := json.Marshal(snap); merr == nil {
				if serr := st.SaveManagedSnapshot(storeGitKind, string(b)); serr != nil {
					log.Printf("[api] 警告: git 快照保存失败（disable 需手工清理）: %v", serr)
				}
			}
			st.Close()
		}
	}
	return snap, nil
}

// gitDisableCore 按快照精确还原；返回还原键数。无快照 = (0, nil)。
func gitDisableCore(dbPath string) (int, error) {
	if dbPath == "" {
		return 0, api.UserError("无本地库，无法定位快照")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	payload, ok, err := st.LoadManagedSnapshot(storeGitKind)
	if err != nil || !ok {
		return 0, nil
	}
	var snap gitcfg.Snapshot
	if json.Unmarshal([]byte(payload), &snap) != nil {
		return 0, fmt.Errorf("git 快照损坏，请手工检查 git config --global url.* 键")
	}
	if err := gitcfg.Disable(gitcfg.CLIExec{}, snap); err != nil {
		return 0, fmt.Errorf("恢复失败（快照保留）: %w", err)
	}
	if err := st.DeleteManagedSnapshot(storeGitKind); err != nil {
		log.Printf("[api] 警告: git 快照删除失败: %v", err)
	}
	return len(snap.Written), nil
}

// GitKeyInfo git status 的单键展示。
type GitKeyInfo struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Managed bool   `json:"managed"` // 命中 ghydra 改写形态
}

// GitStatusInfo GET /api/git/status 响应。
type GitStatusInfo struct {
	Keys     []GitKeyInfo `json:"keys"`
	Snapshot bool         `json:"snapshot"` // 存在可还原快照
}

func gitStatusCore(dbPath string, opts gitcfg.Options) (GitStatusInfo, error) {
	existing, err := gitcfg.ReadURLKeys(gitcfg.CLIExec{})
	if err != nil {
		return GitStatusInfo{}, err
	}
	info := GitStatusInfo{Keys: []GitKeyInfo{}}
	for _, k := range existing {
		info.Keys = append(info.Keys, GitKeyInfo{
			Name: k.Name, Value: k.Value,
			Managed: opts.CDN != "" && gitcfg.IsManaged(k, opts),
		})
	}
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			_, ok, _ := st.LoadManagedSnapshot(storeGitKind)
			info.Snapshot = ok
			st.Close()
		}
	}
	return info, nil
}

// ---- ssh（POST /api/ssh/*）----

// SSHEnableResult POST /api/ssh/enable 响应。
type SSHEnableResult struct {
	Action        string `json:"action"` // sshcfg plan action
	SkipReason    string `json:"skip_reason,omitempty"`
	OK22, OK443   bool   `json:"-"`
	Probed        bool   `json:"probed"`
	ConfigPath    string `json:"config_path"`
	SnapshotSaved bool   `json:"snapshot_saved"`
}

// sshEnableCore 探针（可跳过）+ 写 config + 落快照。语义与 CLI 相同：
// 22/443 均通 = 无需优化（不算错误）；22 通 443 不通 = 拒绝写入。
func sshEnableCore(dbPath, cfgPath string, alias, assumeOK bool) (SSHEnableResult, error) {
	res := SSHEnableResult{ConfigPath: cfgPath}
	if !assumeOK {
		r := probe.New(probe.DefaultConfig())
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		rep := r.Run(ctx, probe.ModeDirect)
		res.OK22, res.OK443 = sshProbeResult(rep)
		res.Probed = true
		switch {
		case res.OK22 && res.OK443:
			res.SkipReason = "22 本身可达，无需 443 优化"
			return res, nil
		case res.OK22 && !res.OK443:
			return res, api.UserError("22 通但 443 不通，无需也不应写入")
		case !res.OK22 && !res.OK443:
			return res, api.UserError("22 与 443 均不通——SSH 场景无解")
		}
	}
	bak := sshBackupPath(dbPath)
	plan, err := sshcfg.Enable(cfgPath, alias, false, bak)
	if err != nil {
		return res, err
	}
	res.Action = plan.Action
	if dbPath != "" {
		if plan.Backup != "" {
			if b, err := os.ReadFile(bak); err == nil {
				saveSSHSnapshot(dbPath, base64.StdEncoding.EncodeToString(b))
				res.SnapshotSaved = true
			}
		} else {
			saveSSHSnapshot(dbPath, "") // 新建文件场景
			res.SnapshotSaved = true
		}
	}
	return res, nil
}

// SSHDisableResult POST /api/ssh/disable 响应。
type SSHDisableResult struct {
	Action string `json:"action"`
}

func sshDisableCore(dbPath, cfgPath string) (SSHDisableResult, error) {
	// 快照优先（字节级还原）——与 CLI sshDisable 同语义
	if dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			if snapData, ok, _ := st.LoadManagedSnapshot(storeSSHKind); ok {
				st.Close()
				bak := sshBackupPath(dbPath)
				if b, err := base64.StdEncoding.DecodeString(snapData); err == nil {
					if err := os.WriteFile(bak, b, 0o600); err == nil {
						if _, err := sshcfg.Disable(cfgPath, bak, false); err == nil {
							if st, e := store.Open(dbPath); e == nil {
								_ = st.DeleteManagedSnapshot(storeSSHKind)
								st.Close()
							}
							return SSHDisableResult{Action: "restore-snapshot"}, nil
						}
					}
				}
			} else {
				st.Close()
			}
		}
	}
	bak := sshBackupPath(dbPath)
	plan, err := sshcfg.Disable(cfgPath, bak, false)
	if err != nil {
		return SSHDisableResult{}, err
	}
	return SSHDisableResult{Action: plan.Action}, nil
}

// SSHStatusInfo GET /api/ssh/status 响应。
type SSHStatusInfo struct {
	Enabled    bool   `json:"enabled"`
	Alias      bool   `json:"alias"`
	ConfigPath string `json:"config_path"`
}

func sshStatusCore(cfgPath string) (SSHStatusInfo, error) {
	en, al, err := sshcfg.Effective(cfgPath)
	if err != nil {
		return SSHStatusInfo{}, err
	}
	return SSHStatusInfo{Enabled: en, Alias: al, ConfigPath: cfgPath}, nil
}

// ---- doctor（POST /api/doctor/run）----

// doctorOnce 跑一轮三列探针（direct/proxy/cdn），持久化并回调决策器。
// startDoctorLoop 的周期任务与 API 手动触发共用本函数。
// cdnFn 运行时取值（支持 POST /api/config 热更）；notify/notifyB 可 nil。
func doctorOnce(repo, proxyURL string, cdnFn func() string,
	notify func(channel.Verdict), notifyB func(bool), dialOverride map[string]string) (api.DoctorFrame, error) {

	frame := api.DoctorFrame{FinishedAt: time.Now()}
	cdnPrefix := cdnFn()
	if err := waitDoctorProxy(proxyURL, 15*time.Second); err != nil {
		return frame, fmt.Errorf("代理尚未就绪: %w", err)
	}

	cfg := probe.DefaultConfig()
	cfg.Timeout = 12 * time.Second
	cfg.Repo = repo
	cfg.ProxyURL = proxyURL
	cfg.CDNPrefix = cdnPrefix
	cfg.DialOverride = dialOverride
	r := probe.New(cfg)

	// direct 可能遇黑洞耗满超时；proxy 拿独立预算（与 loop 同口径）
	ctxD, cancelD := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
	direct := r.Run(ctxD, probe.ModeDirect)
	cancelD()
	ctxP, cancelP := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
	prox := r.Run(ctxP, probe.ModeProxy)
	cancelP()
	reports := []probe.Report{direct, prox}
	frame.ProxyOK, frame.ProxyTotal = prox.CoveredPassed, prox.CoveredTotal
	frame.DirectOK = direct.CoveredPassed

	if cdnPrefix != "" {
		ctxC, cancelC := context.WithTimeout(context.Background(), cfg.Timeout+3*time.Second)
		cdn := r.Run(ctxC, probe.ModeCDN)
		cancelC()
		reports = append(reports, cdn)
		n := cdn.CoveredPassed
		frame.CDNOK = &n
		if notifyB != nil {
			notifyB(channel.CoveredHealthy(&cdn))
		}
	}

	if dbPath := defaultDBPath(); dbPath != "" {
		if st, err := store.Open(dbPath); err == nil {
			persistDoctorReports(st, reports)
			st.Close()
		}
	}

	if notify != nil {
		notify(channel.VerdictFromReports(&prox, &direct))
	}
	frame.Verdict = verdictName(channel.VerdictFromReports(&prox, &direct))
	return frame, nil
}
