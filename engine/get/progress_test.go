package get

// progress_test.go —— OnProgress 回调契约（M3-W1）：
//   - 表头解析后立即首帧（total 尽早可见）
//   - 节流（≤256KB 或 100ms 窗口，不逐块刷）
//   - 终态保证（Get 返回前必有一次 got==total 的回调）
//   - 切道后通道名跟随

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type progRecord struct {
	got, total int64
	ch         string
}

func TestOnProgressFastFile(t *testing.T) {
	data := make([]byte, 2<<20) // 2MB：跨多个 256KB 节流窗口
	for i := range data {
		data[i] = byte(i)
	}
	up := newFakeUp(t, data)
	up.chunk = 64 << 10
	up.interval = 0 // 关滴流：本用例测节流，不测慢上游

	var mu sync.Mutex
	var recs []progRecord
	d := New(DefaultConfig(), NewAFetcher(nil), nil)
	d.OnProgress = func(got, total int64, ch string) {
		mu.Lock()
		recs = append(recs, progRecord{got, total, ch})
		mu.Unlock()
	}

	dst := filepath.Join(t.TempDir(), "out.bin")
	res, err := d.Get(context.Background(), up.url(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if res.GotBytes != int64(len(data)) {
		t.Fatalf("got %d", res.GotBytes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) == 0 {
		t.Fatal("no progress callbacks")
	}
	first := recs[0]
	if first.got != 0 {
		t.Errorf("first callback got=%d want 0（表头即首帧）", first.got)
	}
	if first.total != int64(len(data)) {
		t.Errorf("first callback total=%d want %d", first.total, len(data))
	}
	last := recs[len(recs)-1]
	if last.got != int64(len(data)) || last.ch != "A" {
		t.Errorf("final callback: %+v", last)
	}
	// 单调不减
	for i := 1; i < len(recs); i++ {
		if recs[i].got < recs[i-1].got {
			t.Errorf("non-monotonic at %d: %d < %d", i, recs[i].got, recs[i-1].got)
		}
	}
	// 2MB 文件回调用不着每次 64KB 读都发：上限 = 表头1 + ~8窗口 + 终态1（留裕量）
	if len(recs) > 32 {
		t.Errorf("throttle broken: %d callbacks for 2MB", len(recs))
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
}

func TestOnProgressChannelSwitch(t *testing.T) {
	data := make([]byte, 12<<20) // > BigFile 10MB：A 慢 → 切 B
	for i := range data {
		data[i] = byte(i ^ 0xA5)
	}
	upA := newFakeUp(t, data)
	upA.chunk = 8 << 10 // 8KB/50ms = 160KB/s < 200KB/s 慢阈值 → 判慢切 B
	upA.interval = 50 * time.Millisecond
	upB := newFakeUp(t, data)

	var mu sync.Mutex
	seenB := false
	lastCh := ""
	cfg := DefaultConfig()
	cfg.ProbeBytes = 64 << 10 // 缩小测速窗口：8 滴 ≈ 400ms 内完成判定
	d := New(cfg, NewAFetcher(nil), NewBFetcher(upB.srv.URL+"/"))
	d.OnProgress = func(got, total int64, ch string) {
		mu.Lock()
		if ch == "B" {
			seenB = true
		}
		lastCh = ch
		mu.Unlock()
	}

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := d.Get(context.Background(), upA.url(), dst); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seenB {
		t.Error("no B-channel progress callbacks")
	}
	if lastCh != res_FinalCh_B {
		t.Errorf("last channel %q", lastCh)
	}
}

// res_FinalCh_B 语义别名（避免测试里裸字符串拼错）。
const res_FinalCh_B = "B"
