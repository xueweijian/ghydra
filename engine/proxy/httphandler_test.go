package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/internal/testutil"
)

// TestServerHTTPHandler 同端口托管：GET /pac 走 HTTPHandler，
// CONNECT 走隧道（R5）。
func TestServerHTTPHandler(t *testing.T) {
	up, stopUp, err := testutil.StartTLSServer("github.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stopUp()

	mux := http.NewServeMux()
	mux.HandleFunc("/pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		fmt.Fprint(w, "function FindProxyForURL(){return \"PROXY 127.0.0.1:9801\"}")
	})

	events := make(chan Event, 4)
	srv := &Server{
		Selector: SelectorFunc(func(host string) (string, bool) {
			if host == "github.com" {
				return up.Addr, true
			}
			return "", false
		}),
		OnEvent:     func(e Event) { events <- e },
		HTTPHandler: mux,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	proxyAddr := ln.Addr().String()

	// CONNECT 正常走隧道
	c := dialAndConnect(t, proxyAddr, "github.com:443")
	fmt.Fprint(c, "PING")
	c.Close()
	select {
	case ev := <-events:
		if ev.Target != up.Addr {
			t.Fatalf("隧道目标异常: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到连接事件")
	}

	// GET /pac 拿到 PAC 文本
	hc, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	fmt.Fprint(hc, "GET /pac HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(hc)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("GET /pac 应回 200: %q", line)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "PROXY 127.0.0.1:9801") {
		t.Fatalf("PAC 正文异常: %q", body)
	}
}
