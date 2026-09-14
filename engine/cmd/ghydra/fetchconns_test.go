package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// F7：fetchConns 从 serve 免 token 的 /status 读连接数——200+合法 JSON
// 返回值；非 200 / 坏 JSON / 连不上均 false（statusCmd 保持沉默）。
func TestFetchConns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"conns":42,"listen":"127.0.0.1:9801"}`)
	}))
	defer srv.Close()
	n, ok := fetchConns(srvPort(t, srv))
	if !ok || n != 42 {
		t.Fatalf("fetchConns = (%d, %v), want (42, true)", n, ok)
	}

	// 非 200：沉默 false
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv2.Close()
	if _, ok := fetchConns(srvPort(t, srv2)); ok {
		t.Fatal("非 200 应返回 false")
	}

	// 坏 JSON：沉默 false
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not-json")
	}))
	defer srv3.Close()
	if _, ok := fetchConns(srvPort(t, srv3)); ok {
		t.Fatal("坏 JSON 应返回 false")
	}

	// 端口连不上：沉默 false
	if _, ok := fetchConns(1); ok {
		t.Fatal("不可达端口应返回 false")
	}
}

func srvPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(srv.URL[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	p, err := net.LookupPort("tcp", ps)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
