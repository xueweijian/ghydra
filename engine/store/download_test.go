package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/get"
)

// 异步写需要等 writeLoop 处理；store 无 Flush 接口，轮询到可见。
func waitForCond(t *testing.T, s *Store, deadline time.Duration, cond func() bool) {
	t.Helper()
	t0 := time.Now()
	for time.Since(t0) < deadline {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待异步落库超时")
}

func TestAppendDownloadAndStats(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 一次 A→B 下载：A 段 1MB@1MB/s，B 段 7MB@8MB/s
	res := get.Result{
		URL:        "https://github.com/o/r/releases/download/v1/big.zip",
		TotalBytes: 8 << 20,
		GotBytes:   8 << 20,
		DurMS:      1000,
		RateBPS:    8 << 20,
		FinalCh:    "B",
		Segments: []get.Segment{
			{Channel: "A", StartOff: 0, Bytes: 1 << 20, DurMS: 1000, TTFBMS: 100, RateBPS: 1 << 20, WhyOut: "前1024KB=1024KB/s 低于阈值"},
			{Channel: "B", StartOff: 1 << 20, Bytes: 7 << 20, DurMS: 875, TTFBMS: 50, RateBPS: 8 << 20},
		},
	}
	s.AppendDownload("run-1", res)
	waitForCond(t, s, 2*time.Second, func() bool {
		st, _ := s.DownloadStats(time.Now().Add(-time.Minute))
		return len(st) == 2
	})

	stats, err := s.DownloadStats(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("应有 A/B 两行: %+v", stats)
	}
	byCh := map[string]DownloadStat{}
	for _, st := range stats {
		byCh[st.Channel] = st
	}
	if byCh["A"].Runs != 1 || byCh["B"].Runs != 1 {
		t.Fatalf("Runs: %+v", byCh)
	}
	if byCh["B"].MedianBPS != 8<<20 {
		t.Fatalf("B 中位速率: %v", byCh["B"].MedianBPS)
	}
	if byCh["A"].MedianBPS != 1<<20 {
		t.Fatalf("A 中位速率: %v", byCh["A"].MedianBPS)
	}
	if byCh["B"].TotalMB < 7 || byCh["A"].TotalMB < 1 {
		t.Fatalf("总量: %+v", byCh)
	}
}

func TestAppendDownloadOKSemantics(t *testing.T) {
	// 中间段带切道原因不算失败、最终段成功 → 两段都 OK 落库
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := get.Result{
		URL: "https://github.com/x/y/z",
		Segments: []get.Segment{
			{Channel: "A", Bytes: 100, RateBPS: 1, WhyOut: "TTFB 800ms 超过 300ms"},
			{Channel: "B", Bytes: 900, RateBPS: 100},
		},
	}
	s.AppendDownload("run-2", res)
	waitForCond(t, s, 2*time.Second, func() bool {
		st, _ := s.DownloadStats(time.Now().Add(-time.Minute))
		return len(st) == 2
	})
	// OK 语义走 DoctorSummary 验证
	sum, err := s.DoctorSummary(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range sum {
		if d.Mode == "download" {
			found = true
			if d.Checks != 2 || d.Passed != 2 {
				t.Fatalf("download 段应全 OK: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("DoctorSummary 应含 download")
	}
}

func TestShortURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/releases/download/v1/x.zip": "github.com/o/r/releases",
		"https://objects.githubusercontent.com/a/b/c":       "objects.githubusercontent.com/a/b/c",
		"github.com": "github.com",
	}
	for in, want := range cases {
		if got := shortURL(in); got != want {
			t.Errorf("shortURL(%q) = %q want %q", in, got, want)
		}
	}
}
