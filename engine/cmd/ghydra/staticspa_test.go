package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F13：/wails/ 前缀必须 404——浏览器面板模式不提供壳 runtime（index.html
// 引用的 /wails/runtime.js 在此场景应静默失败）；若走 SPA fallback 回成
// index.html，前端会把 HTML 当 JS 解析触发语法错误。其余未知路径仍回
// index.html（SPA 路由语义不变）；dir 缺失返回 nil（CLI-only 不变）。
func TestStaticSPAWailsPrefix404(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>panel</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := staticSPA(dir)
	if h == nil {
		t.Fatal("dir 在场时 handler 不应为 nil")
	}

	// 1. /wails/runtime.js → 404（body 绝不能是 index.html）
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/wails/runtime.js", nil))
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "panel") {
		t.Fatalf("/wails/runtime.js 期望 404 且非 HTML，得到 %d %q", w.Code, w.Body.String()[:min(40, w.Body.Len())])
	}

	// 2. 未知路径 → SPA fallback（200 + index.html）
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "panel") {
		t.Fatalf("SPA fallback 异常: %d %q", w.Code, w.Body.String())
	}

	// 3. dir 缺失 → nil（CLI-only 安装零变化）
	if staticSPA(filepath.Join(dir, "nope")) != nil {
		t.Fatal("dir 缺失应返回 nil")
	}
}
