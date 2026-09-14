package diag

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

func sampleInput() Input {
	return Input{
		Version: "v1.0.0-rc1", GOOS: "linux", GOARCH: "arm64",
		Now:  time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC),
		Home: "/home/zhang3",
		Doctor: []store.DoctorSummary{
			{Mode: "proxy", Scenario: "web", Checks: 3, Passed: 3, Reachable: 3, AvgDurationMS: 210.5, AvgTTFBMS: 88.2},
		},
		TakeoverOn:   true,
		TakenAt:      time.Date(2026, 9, 14, 7, 30, 0, 0, time.UTC),
		ServeLogTail: "dial 20.205.243.166:443 ok\n" + "/home/zhang3/.ghydra/serve.log opened\n",
		StatusJSON:   []byte(`{"pools":{"github.com":{"n":3}},"takeover":{"on":true}}`),
	}
}

func zipEntries(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip 打开失败: %v", err)
	}
	m := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("entry %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		rc.Close()
		m[f.Name] = buf.Bytes()
	}
	return m
}

// L1：zip 结构断言（smoke 前置——单测里先锁 entry 清单与内容形态）。
func TestBuildZipStructure(t *testing.T) {
	in := sampleInput()
	b, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := zipEntries(t, b)
	for _, name := range []string{"report.json", "doctor_log.json", "serve.log.tail", "pools.json"} {
		if _, ok := m[name]; !ok {
			t.Errorf("缺 entry %s（有: %v）", name, keys(m))
		}
	}
	var rep struct {
		Version   string `json:"version"`
		GOOS      string `json:"goos"`
		Takeover  bool   `json:"takeover_on"`
		Serve     string `json:"serve_status"`
		Redaction string `json:"redaction"`
	}
	if err := json.Unmarshal(m["report.json"], &rep); err != nil {
		t.Fatalf("report.json 解析: %v", err)
	}
	if rep.Version != "v1.0.0-rc1" || rep.GOOS != "linux" {
		t.Errorf("report 元数据错: %+v", rep)
	}
	if !rep.Takeover || rep.Serve != "running" {
		t.Errorf("takeover/serve 状态错: %+v", rep)
	}
	if !strings.Contains(string(m["serve.log.tail"]), "20.205.243.166") {
		t.Error("IP 应保留（不误伤）")
	}
	if strings.Contains(string(m["serve.log.tail"]), "/home/zhang3") {
		t.Error("家目录应脱敏")
	}
}

// 无 serve（StatusJSON=nil）：pools.json 缺席 + report.serve_status=not-running。
func TestBuildServeNotRunning(t *testing.T) {
	in := sampleInput()
	in.StatusJSON = nil
	b, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m := zipEntries(t, b)
	if _, ok := m["pools.json"]; ok {
		t.Error("serve 未运行不应有 pools.json")
	}
	if !strings.Contains(string(m["report.json"]), "not-running") {
		t.Error("report 应标注 not-running")
	}
}

// —— 核心验收：泄漏扫描（设计 §4 冻结——植入 token/代理凭据 → zip 全文 0 命中）——
func TestLeakScanZeroHit(t *testing.T) {
	const (
		tok      = "0123456789abcdef0123456789abcdef" // api-token 原文形态
		proxyURL = "http://zhang3:SuperSecret99@192.168.1.9:7890"
	)
	in := sampleInput()
	// 恶意场景：敏感串出现在所有可能的输入通道
	in.ServeLogTail = "loaded token=" + tok + " via " + proxyURL + "\ndial 20.205.243.166 ok\n"
	in.StatusJSON = []byte(`{"cdn":"` + proxyURL + `","token_hint":"` + tok + `"}`)
	in.Secrets = []string{tok, "SuperSecret99", proxyURL, "zhang3"}

	b, err := Build(in)
	if err != nil {
		t.Fatalf("Build（泄漏应报错或脱敏，不应 panic）: %v", err)
	}
	m := zipEntries(t, b)
	for name, content := range m {
		for _, s := range in.Secrets {
			if s != "" && bytes.Contains(content, []byte(s)) {
				t.Errorf("泄漏扫描命中：entry %s 含敏感串 %q（核心验收失败）", name, s)
			}
		}
	}
	// 正向：诊断价值保留——IP 与域名仍在
	if !bytes.Contains(m["serve.log.tail"], []byte("20.205.243.166")) {
		t.Error("脱敏不应误伤 IP（诊断价值）")
	}
}

// Secrets 泄漏且无法脱敏时（如已嵌入二进制字段值中间），Build 必须报错拒绝导出。
func TestBuildRejectsUnredactableSecret(t *testing.T) {
	in := sampleInput()
	// 构造 Redact 正则覆盖不到的自定义敏感串（非 hex/非 URL 凭据/非键值对形态）
	weird := "ZZQCustomMarker"
	in.StatusJSON = []byte(`{"note":"path ` + weird + ` embedded"} `)
	in.Secrets = []string{weird}
	if _, err := Build(in); err == nil {
		t.Fatal("未脱敏干净的敏感串应让 Build 报错，而非带泄导出")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
