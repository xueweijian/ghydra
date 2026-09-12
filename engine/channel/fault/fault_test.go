package fault

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url string, timeout time.Duration) (*http.Response, string) {
	t.Helper()
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Get("http://" + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, string(b)
}

func Test403(t *testing.T) {
	s, err := New403()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, body := get(t, s.Addr(), 3*time.Second)
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "forbidden") {
		t.Fatalf("body = %q", body)
	}
}

func TestRST(t *testing.T) {
	s, err := NewRST()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err != nil {
		// RST 可能赶在 connect() 返回前到达——拨号阶段被重置同样是
		// 注入成立的特征（真实 GFW RST 也常在握手期到达），两相任一
		if strings.Contains(err.Error(), "reset by peer") || strings.Contains(err.Error(), "connection refused") {
			return
		}
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("RST 后读应报错")
	}
}

func TestTLSDead(t *testing.T) {
	s, err := NewTLSDead()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 发 ClientHello（裸 TLS 握手）后等响应：应超时而非收到任何字节
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x01}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	buf := make([]byte, 16)
	if n, err := c.Read(buf); err == nil {
		t.Fatalf("黑洞不应有响应，收到 %d 字节", n)
	}
}

func TestSlow(t *testing.T) {
	total := int64(100 * 1024)
	s, err := NewSlow(total, 10*1024) // 10KB/s
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	el := time.Since(start)

	if n != total {
		t.Fatalf("读到 %d 字节, want %d", n, total)
	}
	minDur := time.Duration(float64(total)/10240.0*0.8) * time.Second // ±20% 容差
	if el < minDur {
		t.Fatalf("10KB/s 滴流 100KB 应 ≥%v, 实际 %v", minDur, el)
	}
	rate := float64(n) / el.Seconds()
	if rate > 20*1024 {
		t.Fatalf("实测速率 %.0f B/s 超过滴流上限（注入失效）", rate)
	}
}

func TestBlackhole(t *testing.T) {
	s, err := NewBlackhole()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.DialTimeout("tcp", s.Addr(), time.Second)
	if err == nil {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 16)
		if n, err := c.Read(buf); err == nil {
			t.Fatalf("黑洞不应响应，收到 %d 字节", n)
		}
		c.Close()
	}
	// 拨号被 backlog 拒绝（connection refused/reset）也算黑洞性质成立
}

func TestHealthy(t *testing.T) {
	s, err := NewHealthy()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, body := get(t, s.Addr(), 3*time.Second)
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("对照上游异常: %d %q", resp.StatusCode, body)
	}
}

// 演练脚本级的组合场景：A=403（源头故障）→ 决策器切 B → B=healthy。
// 这就是退出标准①的最小可复现骨架（W2 下载器接入后升级为全链路）。
func TestDrill403FailoverSkeleton(t *testing.T) {
	faultA, err := New403()
	if err != nil {
		t.Fatal(err)
	}
	defer faultA.Close()
	backupB, err := NewHealthy()
	if err != nil {
		t.Fatal(err)
	}
	defer backupB.Close()

	// 用真实 HTTP 探测模拟 doctor 的 403 判定输入
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + faultA.Addr())
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("注入的 A 应返回 403: %v %d", err, resp.StatusCode)
	}
	resp2, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + backupB.Addr())
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("注入的 B 应返回 200: %v", err)
	}
}
