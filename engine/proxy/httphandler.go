package proxy

import (
	"fmt"
	"net"
	"net/http"
)

// connResponseWriter 把 http.Handler 的响应写到裸连接（同端口托管
// PAC/status：非 CONNECT 请求的极简 HTTP 服务，无完整 http.Server
// 的开销与超时管理——这些端点只服务本机，请求小而少）。
type connResponseWriter struct {
	conn      net.Conn
	header    http.Header
	status    int
	wroteHead bool
}

func (w *connResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *connResponseWriter) WriteHeader(code int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = code
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	for k, vs := range w.Header() {
		for _, v := range vs {
			fmt.Fprintf(w.conn, "%s: %s\r\n", k, v)
		}
	}
	w.conn.Write([]byte("\r\n"))
}

func (w *connResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(b)
}

// Flush no-op（无缓冲层）。
func (w *connResponseWriter) Flush() {}
