package api

// golden_test.go —— 契约锁定（W1-Design §8）：
//   Go 侧：handler 真实响应 ↔ testdata/golden/*.json byte 比对
//   TS 侧：gui/frontend vitest 把同一批 JSON 断言进 types.ts（编译期）
// 改任何一端 schema 必须显式更新 golden（review 可见）。
// 更新方式：go test ./api/ -run TestGolden -update

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func goldenPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "golden", name)
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := goldenPath(t, name)
	got = append([]byte(strings.TrimSpace(string(got))), '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("golden %s missing（首次生成: go test ./api/ -run TestGolden -update）: %v", name, err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		t.Errorf("golden %s drifted:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestGolden(t *testing.T) {
	st := &depState{}
	srv, ts := newTestServer(t, st)

	// ---- REST 响应（走真实 handler）----
	for _, c := range []struct {
		name, method, path string
	}{
		{"status.json", "GET", "/api/status"},
		{"config.json", "GET", "/api/config"},
		{"doctor_summary.json", "GET", "/api/doctor/summary?hours=24"},
		{"get_progress.json", "GET", "/api/get/progress"},
		{"mitm_status.json", "GET", "/api/mitm/status"},
		{"rules_snapshot.json", "GET", "/api/rules"},
		{"doctor_run.json", "POST", "/api/doctor/run"},
	} {
		resp, body := call(t, c.method, ts.URL+c.path, map[string]any{}, authHdr())
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, resp.StatusCode, body)
		}
		checkGolden(t, c.name, []byte(body))
	}
	// get/start 需要真实 body
	resp, body := call(t, "POST", ts.URL+"/api/get/start",
		map[string]any{"url": "https://github.com/o/r/releases/download/v1/x.zip"}, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("get/start: %d %s", resp.StatusCode, body)
	}
	checkGolden(t, "get_start.json", []byte(body))

	// ---- SSE 帧的 data 部分（DTO 直接序列化）----
	connFrame := ConnFrame{Events: []ConnEvent{{
		Host: "github.com", Target: "140.82.112.3:443", Accel: true, OK: true,
		DialMS: 45.6, Rx: 1048576, Tx: 2048, DurMS: 812.4, TS: fixedNow(),
	}}}
	b, _ := json.Marshal(connFrame)
	checkGolden(t, "sse_conn.json", b)

	doctorFrame := DoctorFrame{
		RunID: "run-20260913-120000", Verdict: "network_fault",
		ProxyOK: 0, ProxyTotal: 5, DirectOK: 0,
		FinishedAt: fixedNow(),
	}
	b, _ = json.Marshal(doctorFrame)
	checkGolden(t, "sse_doctor.json", b)

	getFrame := GetTask{
		TaskID: "get-1", URL: "https://github.com/o/r/releases/download/v1/x.zip",
		Dst: "/home/u/Downloads/x.zip", GotBytes: 7340032, Total: 10485760,
		RateBPS: 2411724.8, Channel: "B", Done: false, StartedAt: fixedNow(),
	}
	b, _ = json.Marshal(getFrame)
	checkGolden(t, "sse_get.json", b)

	hello := HelloFrame{Proto: 1, HeartbeatS: 15, APIVersion: APIVersion}
	b, _ = json.Marshal(hello)
	checkGolden(t, "sse_hello.json", b)

	_ = srv // golden 走 HTTP 路径；srv 留作未来直接注入 Server 级断言
}

// 时间戳序列化形态锁定（RFC3339Nano 无时区偏移尾巴 = 前端解析一致）。
func TestTimeMarshaling(t *testing.T) {
	b, _ := json.Marshal(fixedNow())
	if string(b) != `"2026-09-13T12:00:00Z"` {
		t.Errorf("time shape: %s", b)
	}
	_ = time.UTC
}
