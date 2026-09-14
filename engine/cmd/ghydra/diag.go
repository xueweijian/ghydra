package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/diag"
	"github.com/xueweijian/ghydra/engine/selfupdate"
	"github.com/xueweijian/ghydra/engine/store"
)

// diagCmd —— ghydra diag（W4p2 设计 D3）：一键脱敏诊断包。
//
// 数据源：store（doctor 汇总/接管存在性）、活着的 serve（/api/status 池快照）、
// serve.log 尾部、selfupdate 状态摘要。绝不导出：api-token、系统代理原值、
// 环境变量（设计表）。Secrets 泄漏扫描零命中是硬门槛——命中即拒绝导出。
func diagCmd(args []string) {
	fs := flag.NewFlagSet("diag", flag.ExitOnError)
	out := fs.String("out", "", "输出 zip 路径（默认 ghydra-diag-<yyyymmdd>.zip）")
	days := fs.Int("days", 7, "doctor 汇总回看天数")
	_ = fs.Parse(args)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		log.Fatalf("diag: HOME 不可用，缺少脱敏基准，拒绝生成")
	}
	now := time.Now()

	// store 数据（doctor 汇总 + 接管存在性——绝不读快照原值）
	st, err := store.Open(defaultDBPath())
	if err != nil {
		log.Fatalf("diag: 打开 store: %v", err)
	}
	doctor, err := st.DoctorSummary(now.AddDate(0, 0, -*days))
	if err != nil {
		log.Printf("diag: doctor 汇总读取失败（继续）: %v", err)
		doctor = nil
	}
	takeoverOn, takenAt, err := st.LoadTakeoverState()
	if err != nil {
		log.Printf("diag: 接管态读取失败（继续）: %v", err)
	}
	st.Close()

	// serve 池快照（活着的 daemon 才有；token 只用于本机请求，不写入包）
	var statusJSON []byte
	if d := loadDaemonState(); d != nil && serveAlive(d.Port) {
		if sj := fetchStatusJSON(d.Port); sj != nil {
			statusJSON = sj
		}
	}

	// serve.log 尾部原文（Build 内部 Redact + 截尾 200 行）
	var logTail string
	if b, err := os.ReadFile(filepath.Join(ghydraDir(), "serve.log")); err == nil {
		logTail = string(b)
	}

	// selfupdate 状态摘要（pending/回滚线索，排障关键）
	updateState := summarizeUpdateState()

	// Secrets：api-token 原文（仅作泄漏扫描基准，绝不写入任何 entry）
	var secrets []string
	if t := readTokenIfAny(); t != "" {
		secrets = append(secrets, t)
	}

	zipBytes, err := diag.Build(diag.Input{
		Version: Version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Now: now, Home: home,
		Doctor: doctor, TakeoverOn: takeoverOn, TakenAt: takenAt,
		ServeLogTail: logTail, StatusJSON: statusJSON,
		UpdateState: updateState, Secrets: secrets,
	})
	if err != nil {
		// 泄漏扫描命中走这里——宁可失败导出
		log.Fatalf("diag: %v", err)
	}

	path := *out
	if path == "" {
		path = fmt.Sprintf("ghydra-diag-%s.zip", now.Format("20060102"))
	}
	if err := os.WriteFile(path, zipBytes, 0o600); err != nil {
		log.Fatalf("diag: 写入 %s: %v", path, err)
	}
	log.Printf("诊断包已生成: %s（已脱敏：token/凭据/家目录/用户名；IP 保留）", path)
}

// fetchStatusJSON 本机 /api/status（?token= 与 tokenOK 的 Query 分支对齐）。
// token 读不到/请求失败 → 返回 nil（report 标 not-running，不阻塞诊断包）。
func fetchStatusJSON(port int) []byte {
	tok := readTokenIfAny()
	if tok == "" {
		return nil
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/api/status?token=%s", port, url.QueryEscape(tok))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	// 合法 JSON 才收（防代理/HTML 混入）
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return b
}

// readTokenIfAny 读 api-token 文件（只读，绝不创建——区别于 LoadOrCreateToken）。
func readTokenIfAny() string {
	p := api.DefaultTokenPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// summarizeUpdateState 更新状态一行摘要（无状态返回空）。
func summarizeUpdateState() string {
	sp := selfupdateStatePath()
	if sp == "" {
		return ""
	}
	s, ok, err := selfupdate.LoadState(sp)
	if err != nil || !ok || s == nil {
		return ""
	}
	if s.PendingVersion == "" {
		return ""
	}
	return fmt.Sprintf("pending=%s boot_attempts=%d applied_at=%s",
		s.PendingVersion, s.BootAttempts, s.AppliedAt.Format(time.RFC3339))
}
