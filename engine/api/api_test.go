package api

// api_test.go —— guards/端点/错误映射 全覆盖（W1-Design §9）。
// golden 契约见 golden_test.go。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "t0123456789abcdef"

func fixedNow() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) }

func fixedStatus() ApiStatus {
	bOK := false
	trip := fixedNow().Add(-3 * time.Minute)
	return ApiStatus{
		APIVersion: APIVersion,
		Listen:     "127.0.0.1:9801",
		Scheduler:  true,
		Conns:      42,
		UptimeS:    1234.5,
		Pools: map[string]PoolSnapshot{
			"github.com": {
				Sticky: "140.82.112.3:443",
				IPs: []IPEntry{{
					Addr: "140.82.112.3:443", State: "Active", Score: 0.98,
					RTTMS: 45.2, FailRate: 0.01, Samples: 120, CooldownCount: 1,
				}},
			},
		},
		Channel: ChannelSnapshot{
			State: "Closed", FailRate: 0, Samples: 12, BackoffMult: 1,
			TripAt: &trip, TripWhy: "window 50% fail", BOK: &bOK,
			BSuppressed: 2, BSuppWhy: "B dead",
		},
		CDN:   "https://gh.1ciyuan.cn/",
		Rules: RulesStatus{Version: 10, Source: "disk", Stale: false},
	}
}

func fixedRulesSnapshot() RulesSnapshot {
	return RulesSnapshot{
		Version: 10, Source: "disk", Stale: false,
		GeneratedAt:  "2026-09-13T00:00:00Z",
		ExpiresAt:    "2026-10-28T00:00:00Z",
		Domains:      []string{"github.com", "*.github.com"},
		CDNEndpoints: []string{"https://gh.1ciyuan.cn", "https://gh-proxy.com"},
		SeedIPs: map[string][]string{
			"github.com": {"140.82.112.3", "140.82.121.3", "20.205.243.166"},
		},
		Refresh: RulesRefreshState{
			LastResult: "ok", LastAt: "2026-09-13T06:00:00Z",
			NextAt: "2026-09-13T12:00:00Z", Running: false,
		},
	}
}

func fixedConfig() ApiConfig {
	return ApiConfig{
		Listen: "127.0.0.1:9801", Scheduler: true, CDN: "https://gh.1ciyuan.cn/",
		DoctorEveryS: 3600, DoctorRepo: "xueweijian/ghydra", Managed: true,
	}
}

type depState struct {
	busy     bool
	lastCDN  *string
	onMode   string
	offShut  bool
	gitCalls []string
	sshCalls []string
	getCalls int
}

func testDeps(st *depState) Deps {
	return Deps{
		Status: fixedStatus,
		Rules:  func() RulesSnapshot { return fixedRulesSnapshot() },
		RulesRefresh: func() (string, error) {
			if st.busy {
				return "", ErrBusy
			}
			return "refresh-20260913-120000", nil
		},
		ConfigGet: func() ApiConfig { return fixedConfig() },
		ConfigSet: func(p ConfigPatch) (ApiConfig, error) {
			if st.busy {
				return ApiConfig{}, ErrBusy
			}
			cfg := fixedConfig()
			if p.CDN != nil {
				cfg.CDN = *p.CDN
			}
			st.lastCDN = p.CDN
			return cfg, nil
		},
		DoctorRun: func(repo string) (string, error) {
			if st.busy {
				return "", ErrBusy
			}
			return "run-20260913-120000", nil
		},
		SystemOn: func(mode string) (OnResp, error) {
			if st.busy {
				return OnResp{}, UserError("sysproxy apply failed")
			}
			st.onMode = mode
			return OnResp{OK: true, Mode: mode}, nil
		},
		SystemOff: func(shutdown bool) (OffResp, error) {
			st.offShut = shutdown
			return OffResp{OK: true, Shutdown: shutdown}, nil
		},
		DoctorSummary: func(hours int) (DoctorSummaryResp, error) {
			return DoctorSummaryResp{Runs: []DoctorSummaryRow{{
				Mode: "proxy", Scenario: "clone", Checks: 5, Passed: 5, Reachable: 5,
				AvgDurationMS: 812.3, AvgTTFBMS: 301.7,
				FirstAt: fixedNow(), LastAt: fixedNow(),
			}}}, nil
		},
		GitOp: func(action string, body []byte) (any, error) {
			st.gitCalls = append(st.gitCalls, action)
			return map[string]any{"ok": true, "action": action}, nil
		},
		SSHOp: func(action string, body []byte) (any, error) {
			st.sshCalls = append(st.sshCalls, action)
			return map[string]any{"ok": true, "action": action}, nil
		},
		GetStart: func(req GetStartReq) (GetStartResp, error) {
			st.getCalls++
			if st.busy {
				return GetStartResp{}, ErrBusy
			}
			return GetStartResp{TaskID: "get-1", Dst: "/tmp/x.zip"}, nil
		},
		GetProgress: func() GetProgressResp {
			return GetProgressResp{Recent: []GetTask{{
				TaskID: "get-1", URL: "https://github.com/o/r/releases/download/v1/x.zip",
				Dst: "/tmp/x.zip", GotBytes: 100, Total: 100, RateBPS: 2048,
				Channel: "A", Done: true, StartedAt: fixedNow(),
			}}}
		},
	}
}

func newTestServer(t *testing.T, st *depState, opts ...func(*Server)) (*Server, *httptest.Server) {
	t.Helper()
	s := New(testDeps(st), testToken)
	for _, o := range opts {
		o(s)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		s.serve(w, r)
	}))
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, method, url string, body any, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case json.RawMessage:
		rd = bytes.NewReader(b)
	default:
		jb, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(jb)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func authHdr() map[string]string { return map[string]string{"X-GHydra-Token": testToken} }

// ---- Host 校验 ----

func TestHostGuard(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	cases := []struct {
		host string
		code int
	}{
		{"evil.example.com", 403},
		{"127.0.0.1:9801", 200},
		{"LOCALHOST:1234", 200},
		{"[::1]:9801", 200},
		{"::1", 200},
		{"2130706433", 403}, // 数字形态回环不给过（浏览器不会这样发）
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
		req.Host = c.host
		req.Header.Set("X-GHydra-Token", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("host %s: %v", c.host, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("host %q: got %d want %d", c.host, resp.StatusCode, c.code)
		}
	}
}

func TestHostGuardExtraHosts(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) {
		s.deps.ExtraHosts = []string{"ghydra.box"}
	})
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Host = "ghydra.box:9999"
	req.Header.Set("X-GHydra-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("extra host: got %d want 200", resp.StatusCode)
	}
}

// ---- token ----

func TestTokenGuard(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	base := ts.URL + "/api/status"

	if resp, body := call(t, "GET", base, nil, nil); resp.StatusCode != 401 {
		t.Errorf("no token: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, "GET", base, nil, map[string]string{"X-GHydra-Token": "wrong"}); resp.StatusCode != 401 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	if resp, _ := call(t, "GET", base, nil, map[string]string{"X-GHydra-Token": testToken}); resp.StatusCode != 200 {
		t.Errorf("header token: %d", resp.StatusCode)
	}
	if resp, _ := call(t, "GET", base, nil, map[string]string{"Authorization": "Bearer " + testToken}); resp.StatusCode != 200 {
		t.Errorf("bearer token: %d", resp.StatusCode)
	}
	if resp, _ := call(t, "GET", base+"?token="+testToken, nil, nil); resp.StatusCode != 200 {
		t.Errorf("query token: %d", resp.StatusCode)
	}
	// 读端点同样要 token（D3 修订：读写一律）
	if resp, _ := call(t, "GET", ts.URL+"/api/config", nil, nil); resp.StatusCode != 401 {
		t.Errorf("read endpoint without token: %d", resp.StatusCode)
	}
}

func TestEmptyServerTokenRejectsAll(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) { s.token = "" })
	if resp, _ := call(t, "GET", ts.URL+"/api/status", nil, authHdr()); resp.StatusCode != 401 {
		t.Errorf("empty server token must reject: %d", resp.StatusCode)
	}
}

// ---- CORS ----

func TestCORSPreflight(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/get/start", nil)
	req.Header.Set("Origin", "http://wails.localhost")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "x-ghydra-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("preflight: got %d want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "x-ghydra-token") {
		t.Errorf("Allow-Headers missing token: %q", got)
	}
}

// ---- 端点 ----

func TestStatusEndpoint(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	resp, body := call(t, "GET", ts.URL+"/api/status", nil, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var st ApiStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.APIVersion != 1 || st.Listen != "127.0.0.1:9801" || st.Conns != 42 {
		t.Errorf("bad status: %+v", st)
	}
	if st.Channel.State != "Closed" || st.Channel.BOK == nil || *st.Channel.BOK != false {
		t.Errorf("bad channel: %+v", st.Channel)
	}
}

func TestConfigEndpoints(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	resp, body := call(t, "GET", ts.URL+"/api/config", nil, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}

	resp, body = call(t, "POST", ts.URL+"/api/config",
		map[string]any{"cdn": "https://gh-proxy.com/"}, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("set cdn: %d %s", resp.StatusCode, body)
	}
	if st.lastCDN == nil || *st.lastCDN != "https://gh-proxy.com/" {
		t.Errorf("cdn patch not passed: %+v", st.lastCDN)
	}
	var cfg ApiConfig
	json.Unmarshal([]byte(body), &cfg)
	if cfg.CDN != "https://gh-proxy.com/" {
		t.Errorf("resp cdn: %q", cfg.CDN)
	}

	// cdn: "" = 显式清空（B 禁用）；cdn: null = 未提供（保持）
	if _, b := call(t, "POST", ts.URL+"/api/config", map[string]any{"cdn": ""}, authHdr()); !strings.Contains(b, `"cdn":""`) {
		t.Errorf("cdn empty should clear: %s", b)
	}
	if _, b := call(t, "POST", ts.URL+"/api/config", map[string]any{"cdn": nil}, authHdr()); strings.Contains(b, `"cdn":""`) {
		t.Errorf("cdn null must be no-op: %s", b)
	}
	// 未知字段拒绝
	if resp, _ := call(t, "POST", ts.URL+"/api/config", map[string]any{"listen": "0.0.0.0:1"}, authHdr()); resp.StatusCode != 400 {
		t.Errorf("unknown field must 400, got %d", resp.StatusCode)
	}
	// 坏 JSON
	if resp, _ := call(t, "POST", ts.URL+"/api/config", json.RawMessage(`{bad`), authHdr()); resp.StatusCode != 400 {
		t.Errorf("bad json must 400, got %d", resp.StatusCode)
	}
	// ErrBusy → 409
	st.busy = true
	if resp, _ := call(t, "POST", ts.URL+"/api/config", map[string]any{}, authHdr()); resp.StatusCode != 409 {
		t.Errorf("busy: %d", resp.StatusCode)
	}
}

func TestOnOffEndpoints(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	// on：空 body 默认 pac
	if resp, _ := call(t, "POST", ts.URL+"/api/on", nil, authHdr()); resp.StatusCode != 200 {
		t.Errorf("on default: %d", resp.StatusCode)
	} else if st.onMode != "pac" {
		t.Errorf("on default mode: %q", st.onMode)
	}
	if resp, _ := call(t, "POST", ts.URL+"/api/on", map[string]any{"mode": "proxy"}, authHdr()); resp.StatusCode != 200 {
		t.Errorf("on proxy: %d", resp.StatusCode)
	}
	if resp, _ := call(t, "POST", ts.URL+"/api/on", map[string]any{"mode": "evil"}, authHdr()); resp.StatusCode != 400 {
		t.Errorf("on invalid mode: %d", resp.StatusCode)
	}
	// UserError → 400
	st.busy = true
	if resp, _ := call(t, "POST", ts.URL+"/api/on", nil, authHdr()); resp.StatusCode != 400 {
		t.Errorf("on apply fail: %d", resp.StatusCode)
	}
	st.busy = false
	// off shutdown
	if resp, _ := call(t, "POST", ts.URL+"/api/off", map[string]any{"shutdown": true}, authHdr()); resp.StatusCode != 200 {
		t.Errorf("off: %d", resp.StatusCode)
	}
	if !st.offShut {
		t.Error("shutdown flag lost")
	}
}

func TestOnOffNotAssembled(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) { s.deps.SystemOn = nil; s.deps.SystemOff = nil })
	if resp, _ := call(t, "POST", ts.URL+"/api/on", nil, authHdr()); resp.StatusCode != 503 {
		t.Errorf("want 503, got %d", resp.StatusCode)
	}
}

func TestDoctorEndpoints(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	resp, body := call(t, "POST", ts.URL+"/api/doctor/run", map[string]any{"repo": "o/r"}, authHdr())
	if resp.StatusCode != 200 || !strings.Contains(body, "run-20260913-120000") {
		t.Fatalf("doctor run: %d %s", resp.StatusCode, body)
	}
	st.busy = true
	if resp, _ := call(t, "POST", ts.URL+"/api/doctor/run", nil, authHdr()); resp.StatusCode != 409 {
		t.Errorf("doctor busy: %d", resp.StatusCode)
	}
	st.busy = false

	resp, body = call(t, "GET", ts.URL+"/api/doctor/summary?hours=2", nil, authHdr())
	if resp.StatusCode != 200 || !strings.Contains(body, `"scenario":"clone"`) {
		t.Fatalf("summary: %d %s", resp.StatusCode, body)
	}
}

func TestGitSSHEndpoints(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	for _, m := range []struct {
		path   string
		method string
		action string
	}{
		{"git/enable", "POST", "enable"}, {"git/disable", "POST", "disable"}, {"git/status", "GET", "status"},
		{"ssh/enable", "POST", "enable"}, {"ssh/disable", "POST", "disable"}, {"ssh/status", "GET", "status"},
	} {
		resp, _ := call(t, m.method, ts.URL+"/api/"+m.path, nil, authHdr())
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", m.path, resp.StatusCode)
		}
	}
	if fmt.Sprint(st.gitCalls) != "[enable disable status]" {
		t.Errorf("git calls: %v", st.gitCalls)
	}
	if fmt.Sprint(st.sshCalls) != "[enable disable status]" {
		t.Errorf("ssh calls: %v", st.sshCalls)
	}
	// 错方法 → 404（路由是 method+path 精确匹配）
	if resp, _ := call(t, "POST", ts.URL+"/api/git/status", nil, authHdr()); resp.StatusCode != 404 {
		t.Errorf("git/status POST: %d", resp.StatusCode)
	}
	// 未装配 → 503
	_, ts2 := newTestServer(t, &depState{}, func(s *Server) { s.deps.GitOp = nil })
	if resp, _ := call(t, "POST", ts2.URL+"/api/git/enable", nil, authHdr()); resp.StatusCode != 503 {
		t.Errorf("git not assembled: %d", resp.StatusCode)
	}
}

func TestGetEndpoints(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	if resp, _ := call(t, "POST", ts.URL+"/api/get/start", map[string]any{"url": ""}, authHdr()); resp.StatusCode != 400 {
		t.Errorf("empty url: %d", resp.StatusCode)
	}
	resp, body := call(t, "POST", ts.URL+"/api/get/start",
		map[string]any{"url": "https://github.com/o/r/releases/download/v1/x.zip"}, authHdr())
	if resp.StatusCode != 200 || !strings.Contains(body, `"task_id":"get-1"`) {
		t.Fatalf("get start: %d %s", resp.StatusCode, body)
	}
	st.busy = true
	if resp, _ := call(t, "POST", ts.URL+"/api/get/start", map[string]any{"url": "x"}, authHdr()); resp.StatusCode != 409 {
		t.Errorf("get busy: %d", resp.StatusCode)
	}
	st.busy = false

	resp, body = call(t, "GET", ts.URL+"/api/get/progress", nil, authHdr())
	if resp.StatusCode != 200 || !strings.Contains(body, `"done":true`) {
		t.Fatalf("progress: %d %s", resp.StatusCode, body)
	}
}

func TestMitmStub(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	resp, body := call(t, "GET", ts.URL+"/api/mitm/status", nil, authHdr())
	if resp.StatusCode != 200 || !strings.Contains(body, `"available":false`) {
		t.Fatalf("mitm status: %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{"mitm/enable", "mitm/disable", "mitm/uninstall-cert"} {
		if resp, _ := call(t, "POST", ts.URL+"/api/"+p, nil, authHdr()); resp.StatusCode != 501 {
			t.Errorf("%s: want 501 got %d", p, resp.StatusCode)
		}
	}
}

func TestNotFoundAndMethod(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	if resp, body := call(t, "GET", ts.URL+"/api/nope", nil, authHdr()); resp.StatusCode != 404 || !strings.Contains(body, "error") {
		t.Errorf("404 shape: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, "DELETE", ts.URL+"/api/status", nil, authHdr()); resp.StatusCode != 404 {
		t.Errorf("DELETE status: %d", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) { s.maxBody = 8 })
	big := strings.Repeat("x", 100)
	if resp, _ := call(t, "POST", ts.URL+"/api/config", map[string]any{"cdn": big}, authHdr()); resp.StatusCode != 413 {
		t.Errorf("want 413, got %d", resp.StatusCode)
	}
}

// ---- 内部节拍 ----

func TestStatusDiffTick(t *testing.T) {
	st := &depState{}
	s := New(testDeps(st), testToken)
	ch, err := s.hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	var last string
	s.tickStatus(&last) // 首次：广播
	select {
	case f := <-ch:
		if !strings.Contains(string(f), `"conns":42`) {
			t.Errorf("first tick frame: %s", f)
		}
	default:
		t.Fatal("first tick should broadcast")
	}
	s.tickStatus(&last) // 无变化：静默
	select {
	case f := <-ch:
		t.Fatalf("unchanged status should not broadcast: %s", f)
	default:
	}
}

func TestConnBatchFlush(t *testing.T) {
	s := New(testDeps(&depState{}), testToken)
	ch, _ := s.hub.subscribe()
	s.PushConn(ConnEvent{Host: "github.com", OK: true, Rx: 10})
	s.PushConn(ConnEvent{Host: "api.github.com", OK: false})
	s.flushConns()
	select {
	case f := <-ch:
		if !strings.Contains(string(f), "github.com") || !strings.Contains(string(f), "api.github.com") {
			t.Errorf("batch frame: %s", f)
		}
	default:
		t.Fatal("flush should broadcast batch")
	}
	s.flushConns() // 空：静默
	select {
	case f := <-ch:
		t.Fatalf("empty flush should be silent: %s", f)
	default:
	}
}

// ---- 依赖错误映射兜底 ----

func TestDepErrorFallback(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) {
		s.deps.DoctorRun = func(string) (string, error) { return "", errors.New("boom") }
	})
	if resp, body := call(t, "POST", ts.URL+"/api/doctor/run", nil, authHdr()); resp.StatusCode != 500 || !strings.Contains(body, "boom") {
		t.Errorf("internal error: %d %s", resp.StatusCode, body)
	}
}
