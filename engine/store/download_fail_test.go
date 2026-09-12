package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/get"
)

// W3：A 全失败段（0 字节 0 速率）不再从统计里消失——速率口径不变
// （中位/峰值仍只算有效段），但尝试/失败可见。
func TestDownloadStatsFailedSegVisible(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res := get.Result{
		URL:        "https://github.com/o/r/releases/download/v1/x.zip",
		TotalBytes: 8 << 20,
		GotBytes:   8 << 20,
		DurMS:      900,
		RateBPS:    8 << 20,
		FinalCh:    "B",
		Segments: []get.Segment{
			// A 失败段：dial timeout，0 字节 0 速率（真网 2026-09-13 实况形态）
			{Channel: "A", StartOff: 0, Bytes: 0, DurMS: 10000, TTFBMS: 10000, RateBPS: 0, WhyOut: "dial tcp 20.205.243.166:443: i/o timeout"},
			{Channel: "A", StartOff: 0, Bytes: 0, DurMS: 10000, TTFBMS: 10000, RateBPS: 0, WhyOut: "dial tcp 140.82.112.3:443: i/o timeout"},
			{Channel: "B", StartOff: 0, Bytes: 8 << 20, DurMS: 900, TTFBMS: 1427, RateBPS: 8 << 20},
		},
	}
	s.AppendDownload("run-fail", res)
	waitForCond(t, s, 2*time.Second, func() bool {
		st, _ := s.DownloadStats(time.Now().Add(-time.Minute))
		for _, x := range st {
			if x.Channel == "A" {
				return true
			}
		}
		return false
	})

	stats, err := s.DownloadStats(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	byCh := map[string]DownloadStat{}
	for _, st := range stats {
		byCh[st.Channel] = st
	}
	a, ok := byCh["A"]
	if !ok {
		t.Fatalf("A 全失败段也应出现在统计（可见性）: %+v", stats)
	}
	if a.Attempts != 2 || a.Failures != 2 {
		t.Fatalf("A 尝试/失败: %+v", a)
	}
	if a.Runs != 0 || a.MedianBPS != 0 {
		t.Fatalf("A 无有效段，速率口径应为零: %+v", a)
	}
	b := byCh["B"]
	if b.Runs != 1 || b.MedianBPS != 8<<20 {
		t.Fatalf("B: %+v", b)
	}
}
