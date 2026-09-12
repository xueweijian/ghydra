package get

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCDN gh-proxy 协议假上游：任意路径 → 固定内容 + Range 语义。
func fakeCDN() *httptest.Server {
	handler := func(w http.ResponseWriter, r *http.Request) {
		body := []byte("CDN-BODY-0123456789")
		w.Header().Set("ETag", `"cdn-etag"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if rng := r.Header.Get("Range"); rng != "" {
			var from int
			fmt.Sscanf(rng, "bytes=%d-", &from)
			if from > 0 && from < len(body) {
				part := body[from:]
				w.Header().Set("Content-Length", fmt.Sprint(len(part)))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(body)-1, len(body)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(part)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
	// 直接 HandlerFunc：ServeMux 会对 "/https://.." 路径 cleanPath 折叠 //
	// 触发 301，客户端跟随产生第二请求（真实 CDN 无此行为，纯 mux 伪影）
	return httptest.NewTLSServer(http.HandlerFunc(handler))
}

func TestBDNSDialUsesDoHAddr(t *testing.T) {
	up := fakeCDN()
	defer up.Close()

	var resolveCalls int32
	dns := &BDNS{
		ResolveHost: func(_ context.Context, host string) ([]string, error) {
			atomic.AddInt32(&resolveCalls, 1)
			if host != "cdn.example.com" {
				t.Errorf("解析 host = %q, 想 cdn.example.com", host)
			}
			return []string{up.Listener.Addr().String()}, nil // ip:port 注入
		},
	}
	var mu sync.Mutex
	var gotAddr string
	var gotDoH bool
	b := &BFetcher{
		Prefix:      "https://cdn.example.com/",
		DNS:         dns,
		InsecureTLS: true, // httptest 自签
		OnAddr: func(_, addr string, doh bool) {
			mu.Lock()
			gotAddr, gotDoH = addr, doh
			mu.Unlock()
		},
	}
	resp, err := b.Get(context.Background(), "https://github.com/o/r/releases/download/v1/x.zip", -1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, 想 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != `"cdn-etag"` {
		t.Errorf("ETag 未透传: %q", resp.Header.Get("ETag"))
	}
	mu.Lock()
	defer mu.Unlock()
	if !gotDoH {
		t.Errorf("期望走 DoH 拨号路径")
	}
	if gotAddr != up.Listener.Addr().String() {
		t.Errorf("拨号目标 = %q, 想 %q", gotAddr, up.Listener.Addr().String())
	}
}

func TestBDNSRangeThroughDoH(t *testing.T) {
	up := fakeCDN()
	defer up.Close()
	dns := &BDNS{
		ResolveHost: func(context.Context, string) ([]string, error) {
			return []string{up.Listener.Addr().String()}, nil
		},
	}
	b := &BFetcher{Prefix: "https://cdn.example.com/", DNS: dns, InsecureTLS: true}
	resp, err := b.Get(context.Background(), "https://github.com/x.zip", 5)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, 想 206（Range 续传语义）", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr == "" {
		t.Errorf("Content-Range 缺失")
	}
}

func TestBDNSCacheHit(t *testing.T) {
	up := fakeCDN()
	defer up.Close()
	var calls int32
	dns := &BDNS{
		ResolveHost: func(context.Context, string) ([]string, error) {
			atomic.AddInt32(&calls, 1)
			return []string{up.Listener.Addr().String()}, nil
		},
	}
	b := &BFetcher{Prefix: "https://cdn.example.com/", DNS: dns, InsecureTLS: true}
	for i := 0; i < 3; i++ {
		resp, err := b.Get(context.Background(), "https://github.com/x.zip", -1)
		if err != nil {
			t.Fatalf("第 %d 次 Get: %v", i+1, err)
		}
		resp.Body.Close()
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("ResolveHost 调用 %d 次, 想 1（TTL 缓存）", n)
	}
}

// DoH 死 → 系统 DNS 兜底直拨（前缀 host 直接给 IP 字面量，系统路径可达）。
func TestBDNSFallsBackToSystemDial(t *testing.T) {
	up := fakeCDN()
	defer up.Close()
	dns := &BDNS{
		ResolveHost: func(context.Context, string) ([]string, error) {
			return nil, fmt.Errorf("doh down")
		},
	}
	var mu sync.Mutex
	var gotDoH = true
	b := &BFetcher{
		Prefix:      "https://" + up.Listener.Addr().String() + "/",
		DNS:         dns,
		InsecureTLS: true,
		OnAddr: func(_, _ string, doh bool) {
			mu.Lock()
			gotDoH = doh
			mu.Unlock()
		},
	}
	resp, err := b.Get(context.Background(), "https://github.com/x.zip", -1)
	if err != nil {
		t.Fatalf("Get: %v（DoH 失败应兜底系统直拨）", err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if gotDoH {
		t.Errorf("期望走系统兜底路径")
	}
}

// DoH 全 IP 拨不通 → 驱逐缓存 → 下次 Get 重新解析拿到新结果。
func TestBDNSDropOnAllDialFail(t *testing.T) {
	up := fakeCDN()
	defer up.Close()
	var calls int32
	dns := &BDNS{
		ResolveHost: func(context.Context, string) ([]string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return []string{"127.0.0.1:1"}, nil // 拨不通的黑端口
			}
			return []string{up.Listener.Addr().String()}, nil
		},
	}
	b := &BFetcher{Prefix: "https://cdn.example.com/", DNS: dns, InsecureTLS: true}

	// 第一次：DoH 黑端口拨不通 → 系统 DNS 兜底也会失败（域名不存在）→ 报错可接受
	if _, err := b.Get(context.Background(), "https://github.com/x.zip", -1); err == nil {
		t.Fatalf("第一次 Get 应失败（黑端口 + 假域名）")
	}
	// 第二次：缓存已驱逐 → 重新解析 → 拿到活地址
	resp, err := b.Get(context.Background(), "https://github.com/x.zip", -1)
	if err != nil {
		t.Fatalf("第二次 Get: %v（驱逐后应重解析成功）", err)
	}
	resp.Body.Close()
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("ResolveHost 调用 %d 次, 想 2（失败驱逐 + 重解析）", n)
	}
}

// 负缓存：DoH 短暂失败期间不雪崩（30s 内不重试 DoH，直接系统兜底）。
func TestBDNSNegativeCache(t *testing.T) {
	up := fakeCDN()
	defer up.Close()
	var calls int32
	dns := &BDNS{
		NegativeTTL: time.Hour, // 测试放大（注意：裸数字是 ns 不是 s）
		ResolveHost: func(context.Context, string) ([]string, error) {
			atomic.AddInt32(&calls, 1)
			return nil, fmt.Errorf("doh down")
		},
	}
	b := &BFetcher{Prefix: "https://" + up.Listener.Addr().String() + "/", DNS: dns, InsecureTLS: true}
	for i := 0; i < 3; i++ {
		resp, err := b.Get(context.Background(), "https://github.com/x.zip", -1)
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		resp.Body.Close()
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("负缓存失效：ResolveHost 调用 %d 次, 想 1", n)
	}
}
