package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- W3：ModeCDN（B 通道列） ---

// gh-proxy 协议假 CDN：校验请求路径 = 前缀 + 完整目标 URL，回 200。
func TestModeCDNURLRewrite(t *testing.T) {
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Timeout = 2 * time.Second
	cfg.BodyLimit = 1024
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} // test fixture only
	cfg.CDNPrefix = srv.URL + "/"
	r := New(cfg)
	checks := r.RunEndpoints(context.Background(), ModeCDN, []Endpoint{{
		Scenario: "release", Name: "probe", URL: "https://github.com/o/r/releases/download/v1/x.zip",
	}})
	if len(checks) != 1 {
		t.Fatalf("checks = %d", len(checks))
	}
	c := checks[0]
	if !c.OK || c.Status != 200 {
		t.Fatalf("cdn 检查应通过: %+v", c)
	}
	// 目标 URL 完整拼接在 CDN 前缀后（路径里含 ://）
	if !strings.Contains(gotPath, "/https://github.com/o/r/releases/download/v1/x.zip") {
		t.Errorf("改写路径 = %q, 想含完整目标 URL", gotPath)
	}
}

// CDNPrefix 缺失 → 明确报错（ClassProxyUnreachable 语义复用）。
func TestModeCDNRequiresPrefix(t *testing.T) {
	r := New(DefaultConfig())
	checks := r.RunEndpoints(context.Background(), ModeCDN, []Endpoint{{
		Scenario: "web", Name: "web", URL: "https://github.com/",
	}})
	if len(checks) != 1 || checks[0].OK {
		t.Fatalf("缺前缀应失败: %+v", checks)
	}
	if checks[0].Error == "" {
		t.Errorf("应有错误信息")
	}
}

// ModeCDN 全场景运行：无 SSH 场景（B 是 HTTP 反代），覆盖场景仍为五。
func TestModeCDNRunSkipsSSH(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Timeout = 2 * time.Second
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	cfg.CDNPrefix = srv.URL + "/"
	cfg.ReleaseURL = "https://github.com/cli/cli/releases/latest"
	rep := New(cfg).Run(context.Background(), ModeCDN)
	if rep.CoveredTotal != 5 {
		t.Errorf("覆盖场景 = %d, 想 5", rep.CoveredTotal)
	}
	hasSSH := false
	for _, s := range rep.Scenarios {
		if s.Scenario == "ssh" {
			hasSSH = true
		}
	}
	if hasSSH {
		t.Errorf("cdn 模式不应有 ssh 场景")
	}
	if rep.CoveredPassed != 5 {
		t.Errorf("五场景应全过, got %d/%d", rep.CoveredPassed, rep.CoveredTotal)
	}
}
