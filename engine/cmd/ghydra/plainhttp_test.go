package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/rules"
)

// 明文 HTTP 代理路径：Closed → A（IP 择优转发）；Open → B（CDN 改写）。
// 非加速域名走普通透传。状态码与 body 全透传。
func TestServePlainHTTPRouteAB(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ORIGIN:"+r.Host)
	}))
	defer origin.Close()

	var cdnPath atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnPath.Store(r.URL.Path)
		io.WriteString(w, "CDN-OK")
	}))
	defer cdn.Close()

	m := rules.New([]string{"github.com"})
	router := channel.New(channel.DefaultConfig(), nil)
	pick := func(string) (string, bool) { return strings.TrimPrefix(origin.URL, "http://"), true }
	h := servePlainHTTP(router, m, pick, cdn.URL+"/p/")

	get := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://"+host+"/file.bin", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// 1. Closed：A 转发（Host 保持原域名）
	w := get("github.com")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ORIGIN:github.com") {
		t.Fatalf("A 转发异常: %d %q", w.Code, w.Body.String())
	}

	// 2. Open（doctor 判源头故障）：B CDN 改写，路径含完整目标 URL
	router.NotifyDoctor(channel.VerdictSourceFault)
	w = get("github.com")
	if w.Code != 200 || w.Body.String() != "CDN-OK" {
		t.Fatalf("B 改写异常: %d %q", w.Code, w.Body.String())
	}
	if p, _ := cdnPath.Load().(string); !strings.HasPrefix(p, "/p/http://github.com/file.bin") {
		t.Fatalf("CDN 收到的路径应含完整目标: %q", p)
	}

	// 3. CONNECT 形态不走明文路径（由隧道处理）；此处验证非加速域名透传
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "PASSTHROUGH")
	}))
	defer other.Close()
	pick2 := func(string) (string, bool) { return strings.TrimPrefix(other.URL, "http://"), true }
	h2 := servePlainHTTP(router, m, pick2, cdn.URL+"/p/")
	req := httptest.NewRequest("GET", other.URL+"/x", nil) // 非 github.com
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "PASSTHROUGH") {
		t.Fatalf("非加速域名应透传: %d %q", w2.Code, w2.Body.String())
	}
}
