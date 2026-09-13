package api

// sse_test.go —— SSE HTTP 级语义：hello/首帧 status/query token/心跳/踢线终止。

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// readFrames 从 SSE body 读 n 个完整帧（空行分隔）。
func readFrames(t *testing.T, body io.ReadCloser, n int, timeout time.Duration) []string {
	t.Helper()
	type result struct {
		frames []string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var frames []string
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					frames = append(frames, strings.Join(cur, "\n"))
					cur = nil
					if len(frames) == n {
						done <- result{frames: frames}
						return
					}
				}
				continue
			}
			cur = append(cur, line)
		}
		done <- result{err: sc.Err()}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read frames: %v", r.err)
		}
		return r.frames
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %d frames", n)
		return nil
	}
}

func TestSSEHelloAndFirstStatus(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	req, _ := http.NewRequest("GET", ts.URL+"/api/events?token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	frames := readFrames(t, resp.Body, 2, 3*time.Second)
	if !strings.HasPrefix(frames[0], "event: hello") || !strings.Contains(frames[0], `"proto":1`) {
		t.Errorf("hello frame: %q", frames[0])
	}
	if !strings.HasPrefix(frames[1], "event: status") || !strings.Contains(frames[1], `"conns":42`) {
		t.Errorf("first status frame: %q", frames[1])
	}
}

func TestSSEAuthRequired(t *testing.T) {
	_, ts := newTestServer(t, &depState{})
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}

func TestSSEHeartbeat(t *testing.T) {
	_, ts := newTestServer(t, &depState{}, func(s *Server) { s.heartbeat = 60 * time.Millisecond })
	req, _ := http.NewRequest("GET", ts.URL+"/api/events?token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 心跳是 comment 行（": ping"），不在帧分隔语义里——按行读直到出现
	sc := bufio.NewScanner(resp.Body)
	got := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == ": ping" {
				got <- true
				return
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat within 3s")
	}
}

func TestSSEEventDelivery(t *testing.T) {
	st := &depState{}
	srv, ts := newTestServer(t, st)
	req, _ := http.NewRequest("GET", ts.URL+"/api/events?token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frames := make(chan []string, 1)
	go func() { frames <- readFrames(t, resp.Body, 3, 5*time.Second) }()
	time.Sleep(200 * time.Millisecond) // hello + 首帧 status 落地

	// 公开推送路径：doctor / get / conn（flush 立即调用）
	srv.PushDoctor(DoctorFrame{RunID: "run-9", Verdict: "network_fault"})
	select {
	case fr := <-frames:
		if !strings.Contains(fr[2], "event: doctor") || !strings.Contains(fr[2], `"run_id":"run-9"`) {
			t.Errorf("doctor frame: %q", fr[2])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("doctor frame not delivered")
	}
}

func TestSSEGetAndConnFrames(t *testing.T) {
	st := &depState{}
	srv, ts := newTestServer(t, st)
	req, _ := http.NewRequest("GET", ts.URL+"/api/events?token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frames := make(chan []string, 1)
	go func() { frames <- readFrames(t, resp.Body, 4, 5*time.Second) }()
	time.Sleep(200 * time.Millisecond)

	srv.PushGet(GetTask{TaskID: "get-7", URL: "https://github.com/x.zip", GotBytes: 5, Total: 10, Channel: "B"})
	srv.PushConn(ConnEvent{Host: "github.com", OK: true, Rx: 99})
	srv.flushConns()

	select {
	case fr := <-frames:
		if !strings.Contains(fr[2], "event: get") || !strings.Contains(fr[2], `"task_id":"get-7"`) {
			t.Errorf("get frame: %q", fr[2])
		}
		if !strings.Contains(fr[3], "event: conn") || !strings.Contains(fr[3], `"rx":99`) {
			t.Errorf("conn frame: %q", fr[3])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("get/conn frames not delivered")
	}
}
