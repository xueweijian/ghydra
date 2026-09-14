package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

// L2 端到端（设计 §4 冻结）：fake 环境（HOME 指向临时目录）植入已知
// token/代理凭据 → 真跑 diagCmd → 解 zip 全文 0 命中 + 结构断言。
// 与 daemon_integration_test.go 同款隔离模式（HOME/USERPROFILE）。
func TestDiagCmdEndToEndLeakScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	const (
		tok    = "feedfacefeedfacefeedfacefeedface" // api-token 原文（32 hex）
		proxy  = "http://zhang3:P@ssw0rdX@192.168.9.9:7890"
		passwd = "P@ssw0rdX"
	)

	// 1. store：seed doctor 记录 + 接管快照
	dbPath := filepath.Join(home, ".ghydra", "ghydra.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.AppendDoctor(store.DoctorRecord{
		RunID: "r1", StartedAt: time.Now(), Mode: "proxy", Scenario: "web",
		Name: "web-https", Target: "https://github.com", OK: true, Reachable: true,
		Status: 200, Class: "", DurationMS: 210, TTFBMS: 88,
	})
	// 接管快照：写入原值（diag 只应导出存在性，绝不导出此值）
	if err := st.SaveSnapshotJSON(`{"mode":"pac","pac":"http://127.0.0.1:9801/pac?secret=SEEDSNAPSHOT"}`); err != nil {
		t.Fatalf("SaveSnapshotJSON: %v", err)
	}
	st.Close()

	// 2. serve.log 植入 token 与代理凭据
	gdir := filepath.Join(home, ".ghydra")
	os.MkdirAll(gdir, 0o700)
	os.WriteFile(filepath.Join(gdir, "serve.log"),
		[]byte("dial 20.205.243.166:443 ok\nloaded token="+tok+"\nvia "+proxy+"\n"), 0o600)

	// 3. api-token 文件（diag 应读它作扫描基准，但绝不写入包）
	os.WriteFile(filepath.Join(gdir, "api-token"), []byte(tok+"\n"), 0o600)

	// 4. serve 未运行（不探活成功）→ not-running 路径
	outZip := filepath.Join(t.TempDir(), "diag.zip")
	diagCmd([]string{"--out", outZip})

	// 5. 解包断言
	b, err := os.ReadFile(outZip)
	if err != nil {
		t.Fatalf("读 zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip 打开: %v", err)
	}
	entries := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		var buf bytes.Buffer
		buf.ReadFrom(rc)
		rc.Close()
		entries[f.Name] = buf.Bytes()
	}
	for _, want := range []string{"report.json", "doctor_log.json", "serve.log.tail"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("缺 entry %s", want)
		}
	}
	if _, ok := entries["pools.json"]; ok {
		t.Error("serve 未运行不应有 pools.json")
	}
	// 泄漏扫描（核心验收）：所有敏感串零命中
	for name, content := range entries {
		for _, s := range []string{tok, proxy, passwd, "SEEDSNAPSHOT", string(home)} {
			if s != "" && bytes.Contains(content, []byte(s)) {
				t.Errorf("泄漏命中: %s 含 %q", name, s)
			}
		}
	}
	// 诊断价值保留：IP、doctor 记录的域名字段
	if !bytes.Contains(entries["serve.log.tail"], []byte("20.205.243.166")) {
		t.Error("IP 应保留")
	}
	if !strings.Contains(string(entries["report.json"]), `"takeover_on": true`) {
		t.Errorf("接管存在性应导出 true（got %s）", entries["report.json"])
	}
	var dl []store.DoctorSummary
	if err := json.Unmarshal(entries["doctor_log.json"], &dl); err != nil {
		t.Errorf("doctor_log.json 非法: %v", err)
	}
}

// 无 store/无日志的裸环境：diag 不应崩（首次安装即报障场景）。
func TestDiagCmdBareEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	outZip := filepath.Join(t.TempDir(), "diag.zip")
	diagCmd([]string{"--out", outZip})
	if _, err := os.Stat(outZip); err != nil {
		t.Fatalf("裸环境 diag 应仍产出 zip: %v", err)
	}
}
