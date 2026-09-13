package api

// hub_test.go —— SSE hub 语义：上限/踢线/幂等退订。

import (
	"sync"
	"testing"
	"time"
)

func TestHubSubscribeLimit(t *testing.T) {
	h := newHub(3)
	for i := 0; i < 3; i++ {
		if _, err := h.subscribe(); err != nil {
			t.Fatalf("sub %d: %v", i, err)
		}
	}
	if _, err := h.subscribe(); err != ErrTooManySubs {
		t.Errorf("4th sub: %v", err)
	}
}

func TestHubKickOnFull(t *testing.T) {
	h := newHub(0)
	ch, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	// 不读的慢消费者：灌 subBuf+5 条后广播 → 第 65 条触发踢线（close）。
	// close 后缓冲里最多还有 subBuf 条旧帧可收（open==true），排空到
	// 关闭即证明被踢。
	for i := 0; i < subBuf+5; i++ {
		h.broadcast("status", map[string]int{"i": i})
	}
	openCount := 0
	for {
		select {
		case _, open := <-ch:
			if !open {
				if openCount == 0 {
					t.Fatal("closed without delivering buffered frames")
				}
				if openCount > subBuf {
					t.Errorf("delivered %d > buf %d", openCount, subBuf)
				}
				goto kicked
			}
			openCount++
		case <-time.After(time.Second):
			t.Fatal("subscriber never kicked")
		}
	}
kicked:
	if h.count() != 0 {
		t.Errorf("kicked sub should be removed, count=%d", h.count())
	}
}

func TestHubUnsubscribeIdempotent(t *testing.T) {
	h := newHub(0)
	ch, _ := h.subscribe()
	h.unsubscribe(ch)
	h.unsubscribe(ch) // 二次退订不得 panic/close 二次
	if h.count() != 0 {
		t.Errorf("count=%d", h.count())
	}
}

func TestHubBroadcastReach(t *testing.T) {
	h := newHub(0)
	n := 3
	chs := make([]chan []byte, n)
	for i := range chs {
		chs[i], _ = h.subscribe()
	}
	h.broadcast("doctor", DoctorFrame{RunID: "r1", Verdict: "ok"})
	for i, ch := range chs {
		select {
		case f := <-ch:
			if string(f)[:13] != "event: doctor" {
				t.Errorf("sub %d frame: %q", i, f)
			}
		default:
			t.Errorf("sub %d missed frame", i)
		}
	}
}

// 广播风暴下的不变量：broadcast 永不阻塞（慢读者被踢而非拖住发布方）、
// 无死锁、退订收敛。读者被踢是设计行为（本机 GUI 场景罕见）。
func TestHubBroadcastNeverBlocks(t *testing.T) {
	h := newHub(0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ch, _ := h.subscribe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
			}
		}() // 读者：尽力消费；灌满即被踢（设计行为）
	}
	t0 := time.Now()
	for i := 0; i < 200; i++ {
		h.broadcast("status", map[string]int{"i": i})
	}
	if d := time.Since(t0); d > time.Second {
		t.Errorf("broadcast blocked: %v", d)
	}
	h.mu.Lock()
	curs := make([]chan []byte, 0, len(h.subs))
	for ch := range h.subs {
		curs = append(curs, ch)
	}
	h.mu.Unlock()
	for _, ch := range curs {
		h.unsubscribe(ch) // 锁外退订（unsubscribe 自身持锁）
	}
	wg.Wait()
	if h.count() != 0 {
		t.Errorf("count=%d after teardown", h.count())
	}
}
