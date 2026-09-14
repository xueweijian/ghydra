package diag

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

// Input 汇集诊断数据（全部由调用方装配；包本身不做 IO 探活）。
type Input struct {
	Version string
	GOOS    string
	GOARCH  string
	Now     time.Time
	Home    string // 脱敏基准（家目录→~、basename→<user>）

	Doctor     []store.DoctorSummary // 近 7 天汇总（store.DoctorSummary）
	TakeoverOn bool                  // 托管快照存在性（只导 on，绝不导原值）
	TakenAt    time.Time

	ServeLogTail string // serve.log 尾部原文（内部 Redact + 截尾）
	StatusJSON   []byte // /api/status 原文（serve 活着才有；nil=not-running）
	UpdateState  string // selfupdate 状态摘要（可空）

	// Secrets 已知敏感原文（api-token 等）：任何 entry 命中即报错拒导
	//（第二道防线，Redact 正则之外的强保证——泄漏扫描测试即验此语义）。
	Secrets []string
}

// report.json 结构（诊断包读者契约，尽量稳定）。
type report struct {
	Version     string    `json:"version"`
	GOOS        string    `json:"goos"`
	GOARCH      string    `json:"goarch"`
	GeneratedAt time.Time `json:"generated_at"`
	ServeStatus string    `json:"serve_status"` // running | not-running
	TakeoverOn  bool      `json:"takeover_on"`
	TakenAt     time.Time `json:"taken_at,omitempty"`
	UpdateState string    `json:"update_state,omitempty"`
	Redaction   string    `json:"redaction"` // 脱敏声明（读包者须知）
}

// Build 生成 zip 字节流。文本 entry 全部过 Redact；Secrets 在**压缩前的
// 明文 entry** 上零命中是硬门槛（命中即 error，不导出——压缩后字节扫描
// 会被 deflate 打散明文造成漏报，故必须在明文层做）。
func Build(in Input) ([]byte, error) {
	if in.Home == "" {
		return nil, fmt.Errorf("diag: home 为空，无法脱敏（拒绝生成）")
	}
	entries := map[string][]byte{}

	put := func(name string, b []byte) { entries[name] = []byte(Redact(in.Home, string(b))) }

	// report.json
	rep := report{
		Version: in.Version, GOOS: in.GOOS, GOARCH: in.GOARCH,
		GeneratedAt: in.Now, Redaction: "redacted: tokens/credentials/home/usernames removed; IPs preserved",
	}
	rep.ServeStatus = "not-running"
	if len(in.StatusJSON) > 0 {
		rep.ServeStatus = "running"
	}
	rep.TakeoverOn = in.TakeoverOn
	if in.TakeoverOn {
		rep.TakenAt = in.TakenAt
	}
	rep.UpdateState = in.UpdateState
	rb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	put("report.json", rb)

	// doctor_log.json（近 7 天汇总）
	db, err := json.MarshalIndent(in.Doctor, "", "  ")
	if err != nil {
		return nil, err
	}
	put("doctor_log.json", db)

	// pools.json（/api/status 原样，Redact 后；serve 未运行则无此 entry）
	if len(in.StatusJSON) > 0 {
		put("pools.json", in.StatusJSON)
	}

	// serve.log.tail（截尾 200 行 + Redact）
	put("serve.log.tail", []byte(TailLines(in.ServeLogTail, 200)))

	// 第二道防线：Secrets 精确串在明文 entry 上零命中
	for name, content := range entries {
		for _, s := range in.Secrets {
			if s == "" {
				continue
			}
			if bytes.Contains(content, []byte(s)) {
				return nil, fmt.Errorf("diag: 泄漏扫描命中敏感串（%s @ %s），拒绝导出", name, s[:min(8, len(s))])
			}
		}
	}

	// 打包（entry 名稳定排序，输出可复现）
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(entries[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
