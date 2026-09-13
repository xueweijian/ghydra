package api

// /api/rules 端点行为（M3-W2）：快照读取 + 手动刷新（异步单飞 409）+
// 未装配 503。golden 形状在 golden_test 的 rules_snapshot.json 锁定。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRulesEndpoint(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	resp, body := call(t, "GET", ts.URL+"/api/rules", nil, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var snap RulesSnapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != 10 || snap.Source != "disk" || snap.Stale {
		t.Errorf("bad snapshot: %+v", snap)
	}
	if len(snap.Domains) == 0 || snap.SeedIPs["github.com"] == nil {
		t.Errorf("domains/seeds missing: %+v", snap)
	}
	if snap.Refresh.LastResult != "ok" || snap.Refresh.NextAt == "" {
		t.Errorf("refresh state missing: %+v", snap.Refresh)
	}
	// status 帧摘要与快照一致（版本/来源单一真相）
	resp, body = call(t, "GET", ts.URL+"/api/status", nil, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var st ApiStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Rules.Version != snap.Version || st.Rules.Source != snap.Source {
		t.Errorf("status.rules 与快照不一致: %+v vs %+v", st.Rules, snap)
	}
}

func TestRulesRefreshEndpoint(t *testing.T) {
	st := &depState{}
	_, ts := newTestServer(t, st)

	resp, body := call(t, "POST", ts.URL+"/api/rules/refresh", map[string]any{}, authHdr())
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var r struct {
		Started   bool   `json:"started"`
		RefreshID string `json:"refresh_id"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Started || r.RefreshID == "" {
		t.Errorf("bad refresh resp: %+v", r)
	}

	// 忙 = 409（进行中单飞语义）
	st.busy = true
	resp, body = call(t, "POST", ts.URL+"/api/rules/refresh", map[string]any{}, authHdr())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy: %d %s（want 409）", resp.StatusCode, body)
	}
}

func TestRulesNotAssembled(t *testing.T) {
	// Rules/RulesRefresh 均为可选 Deps：未装配 → 503（防装配遗漏裸奔语义）
	srv := New(Deps{
		Status:    fixedStatus,
		ConfigGet: func() ApiConfig { return fixedConfig() },
		ConfigSet: func(ConfigPatch) (ApiConfig, error) { return fixedConfig(), nil },
		DoctorRun: func(string) (string, error) { return "run", nil },
	}, testToken)
	ts := newBareServer(t, srv)
	resp, body := call(t, "GET", ts.URL+"/api/rules", nil, authHdr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rules: %d %s（want 503）", resp.StatusCode, body)
	}
	resp, body = call(t, "POST", ts.URL+"/api/rules/refresh", map[string]any{}, authHdr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rules/refresh: %d %s（want 503）", resp.StatusCode, body)
	}
	if !strings.Contains(body, "not assembled") {
		t.Errorf("503 body 未说明原因: %s", body)
	}
}

// newBareServer 不带 Rules Deps 的裸 Server 直挂 httptest。
func newBareServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.serve(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}
