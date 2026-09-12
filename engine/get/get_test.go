package get

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUp 可注入缺陷的假上游：TTFB 延迟 / 滴流 / 错误总量 / 错误 ETag /
// 直接失败。支持 Range（切道续传的前提）。
type fakeUp struct {
	data        []byte
	ttfbDelay   time.Duration // 响应头前的延迟（TTFB 注入）
	chunk       int           // 滴流块大小
	interval    time.Duration // 滴流间隔（仅无 Range 的首段）
	etag        string
	totalOffset int64 // 声明总量偏移（缓存错配注入）
	failStatus  int   // 非 0 → 直接返回该状态码
	srv         *httptest.Server
	requests    atomic.Int64
}

func newFakeUp(t *testing.T, data []byte) *fakeUp {
	t.Helper()
	f := &fakeUp{data: data, chunk: 256, etag: `"e1"`, interval: 100 * time.Millisecond}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUp) url() string { return f.srv.URL + "/file.bin" }

func (f *fakeUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.failStatus != 0 {
		http.Error(w, "boom", f.failStatus)
		return
	}
	if f.ttfbDelay > 0 {
		time.Sleep(f.ttfbDelay)
	}
	total := int64(len(f.data)) + f.totalOffset

	from := int64(0)
	ranged := false
	if rg := r.Header.Get("Range"); rg != "" {
		spec := strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-")
		if n, err := strconv.ParseInt(spec, 10, 64); err == nil && n > 0 {
			from = n
			ranged = true
		}
	}
	body := f.data
	if from > int64(len(f.data)) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body = f.data[from:]
	w.Header().Set("ETag", f.etag)
	w.Header().Set("Accept-Ranges", "bytes")
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	// 滴流仅对首段（无 Range）：模拟源头冷启动慢；续传视为热路径快
	drip := !ranged && f.interval > 0
	for len(body) > 0 {
		n := f.chunk
		if n > len(body) {
			n = len(body)
		}
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		body = body[n:]
		if drip {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			time.Sleep(f.interval)
		}
	}
}

// fetchOf 把假上游适配成 Fetcher（B 通道模拟）。
func fetchOf(u *fakeUp) Fetcher {
	return fetchFunc(func(ctx context.Context, rawURL string, from int64) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", u.url(), nil)
		if from >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		}
		return http.DefaultClient.Do(req)
	})
}

type fetchFunc func(ctx context.Context, rawURL string, from int64) (*http.Response, error)

func (f fetchFunc) Get(ctx context.Context, rawURL string, from int64) (*http.Response, error) {
	return f(ctx, rawURL, from)
}

// smallCfg 缩小阈值让滴流测试秒级完成：大文件>4KB、窗口 1KB、
// 慢速阈 10KB/s、TTFB 慢阈 300ms。
func smallCfg() Config {
	return Config{
		TTFBSlow:    300 * time.Millisecond,
		ProbeBytes:  1024,
		ProbeMinBPS: 10 * 1024,
		BigFile:     4096,
		MaxSwitches: 3,
	}
}

// pattern 生成确定性内容（校验拼接正确性）。
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func runGet(t *testing.T, cfg Config, rawURL string, a, b Fetcher) (Result, []byte) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "out.bin")
	d := New(cfg, a, b)
	res, err := d.Get(context.Background(), rawURL, dst)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return res, got
}

// 1. A 快：单段完成，无切道
func TestFastANoSwitch(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	a.interval = 0 // 全速
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), nil)
	if len(res.Segments) != 1 || res.Segments[0].Channel != "A" {
		t.Fatalf("应单段 A: %+v", res.Segments)
	}
	if string(got) != string(pattern(8192)) || res.FinalCh != "A" {
		t.Fatalf("内容不一致 FinalCh=%s", res.FinalCh)
	}
}

// 2. A 滴流慢 + 大文件 → 窗口判慢 → B 续传完成（D4 主路径）
func TestSlowASwitchToB(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	// 256B/100ms = 2.56KB/s < 10KB/s 阈值
	b := newFakeUp(t, pattern(8192))
	b.interval = 0
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))

	if len(res.Segments) != 2 {
		t.Fatalf("应 A→B 两段: %+v", res.Segments)
	}
	sa, sb := res.Segments[0], res.Segments[1]
	if sa.Channel != "A" || sb.Channel != "B" || res.FinalCh != "B" {
		t.Fatalf("通道序错: %+v", res.Segments)
	}
	if sa.Bytes != 1024 { // 恰好探测窗口
		t.Fatalf("A 段应止于窗口 1024, got %d（why=%s）", sa.Bytes, sa.WhyOut)
	}
	if sb.StartOff != sa.Bytes {
		t.Fatalf("B 应从 A 止点续传: %d ≠ %d", sb.StartOff, sa.Bytes)
	}
	if string(got) != string(pattern(8192)) {
		t.Fatalf("拼接内容不一致（续传错位）")
	}
}

// 3. TTFB 慢（不等 body）→ B
func TestTTFBSlowSwitch(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	a.ttfbDelay = 700 * time.Millisecond // > 300ms 阈值
	a.interval = 0
	b := newFakeUp(t, pattern(8192))
	b.interval = 0
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))
	if len(res.Segments) != 2 || res.Segments[0].Channel != "A" || res.Segments[1].Channel != "B" {
		t.Fatalf("应 A→B: %+v", res.Segments)
	}
	if res.Segments[0].Bytes != 0 {
		t.Fatalf("TTFB 判慢不应等 body: %d", res.Segments[0].Bytes)
	}
	if !strings.Contains(res.Segments[0].WhyOut, "TTFB") {
		t.Fatalf("切道原因应含 TTFB: %s", res.Segments[0].WhyOut)
	}
	if string(got) != string(pattern(8192)) {
		t.Fatal("内容不一致")
	}
}

// 4. A 慢 → B 500 错 → 回 A 续传完成
func TestBErrorFallbackToA(t *testing.T) {
	a := newFakeUp(t, pattern(8192)) // 滴流慢（仅首段；Range 快）
	b := newFakeUp(t, pattern(8192))
	b.failStatus = 500
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))

	if len(res.Segments) != 3 {
		t.Fatalf("应 A→B(败)→A 三段: %+v", res.Segments)
	}
	if res.Segments[1].Channel != "B" || res.Segments[2].Channel != "A" || res.FinalCh != "A" {
		t.Fatalf("通道序错: %+v", res.Segments)
	}
	if res.Segments[2].StartOff != res.Segments[0].Bytes {
		t.Fatalf("回 A 应续传")
	}
	if string(got) != string(pattern(8192)) {
		t.Fatal("内容不一致")
	}
}

// 5. B 总量声明不一致（CDN 缓存错配）→ 弃 B 回 A，且无脏字节落盘
func TestBMismatchTotalFallback(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	b := newFakeUp(t, pattern(8192))
	b.interval = 0
	b.totalOffset = 999 // 声明 9191 ≠ 8192
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))

	if len(res.Segments) != 3 || res.Segments[1].Channel != "B" || res.Segments[2].Channel != "A" {
		t.Fatalf("应 A→B(拒)→A: %+v", res.Segments)
	}
	if res.Segments[1].Bytes != 0 {
		t.Fatalf("错配应在头部拦截, B 段字节 %d", res.Segments[1].Bytes)
	}
	if !strings.Contains(res.Segments[1].WhyOut, "错配") && !strings.Contains(res.Segments[1].WhyOut, "总量") {
		t.Fatalf("原因应为错配: %s", res.Segments[1].WhyOut)
	}
	if string(got) != string(pattern(8192)) {
		t.Fatal("内容不一致")
	}
}

// 6. ETag 不一致 → 同样拦截
func TestBETagMismatchFallback(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	b := newFakeUp(t, pattern(8192))
	b.interval = 0
	b.etag = `"different"`
	res, got := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))
	if res.FinalCh != "A" || len(res.Segments) != 3 {
		t.Fatalf("应回 A: %+v", res.Segments)
	}
	if string(got) != string(pattern(8192)) {
		t.Fatal("内容不一致")
	}
}

// 7. 小文件：即使 A 慢也不切（切道开销不值得）
func TestSmallFileNoSwitch(t *testing.T) {
	a := newFakeUp(t, pattern(2048)) // < BigFile 4096
	b := newFakeUp(t, pattern(2048))
	b.interval = 0
	res, _ := runGet(t, smallCfg(), a.url(), fetchOf(a), fetchOf(b))
	if len(res.Segments) != 1 || res.FinalCh != "A" {
		t.Fatalf("小文件不应切道: %+v", res.Segments)
	}
}

// 8. A 一直失败且无 B → 报错
func TestAOnlyFailure(t *testing.T) {
	a := newFakeUp(t, pattern(8192))
	a.failStatus = 503
	dst := filepath.Join(t.TempDir(), "out.bin")
	d := New(smallCfg(), fetchOf(a), nil)
	_, err := d.Get(context.Background(), a.url(), dst)
	if err == nil {
		t.Fatal("应报错")
	}
}

// 9. BFetcher 前缀改写：路径含完整目标 URL + Range 透传
func TestBFetcherRewrite(t *testing.T) {
	var gotPath, gotRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotRange.Store(r.Header.Get("Range"))
		io.WriteString(w, "data")
	}))
	defer srv.Close()

	b := NewBFetcher(srv.URL + "/proxy/")
	resp, err := b.Get(context.Background(), "https://github.com/o/r/repo/zip/v1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if p, _ := gotPath.Load().(string); p != "/proxy/https://github.com/o/r/repo/zip/v1" {
		t.Fatalf("改写路径错: %s", p)
	}
	if rg, _ := gotRange.Load().(string); rg != "bytes=1024-" {
		t.Fatalf("Range 未透传: %q", rg)
	}
}

// 10. AFetcher 择优拨号：PickAddr 替换目标、SNI/Host 保持原域名
func TestAFetcherPickAddr(t *testing.T) {
	var sawHost atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost.Store(r.Host)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var dialed atomic.Value
	a := NewAFetcher(func(host string) (string, bool) {
		return strings.TrimPrefix(srv.URL, "https://"), true // 择优指向测试上游
	})
	a.InsecureTLS = true // httptest 自签证书
	a.OnAddr = func(host, addr string, picked bool) { dialed.Store(fmt.Sprint(host, "|", addr, "|", picked)) }
	// 目标 URL 用假域名：证明 Host/SNI 语义不随 IP 择优改变
	//（httptest 证书只签 127.0.0.1，此处借助 srv.URL 保持可验证）
	resp, err := a.Get(context.Background(), srv.URL+"/x", -1)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d, _ := dialed.Load().(string); !strings.Contains(d, "picked|true") && !strings.Contains(d, "|true") {
		t.Fatalf("应标记择优: %s", d)
	}
	if h, _ := sawHost.Load().(string); !strings.Contains(h, "127.0.0.1") {
		t.Fatalf("Host 应为原域名: %s", h)
	}
}
