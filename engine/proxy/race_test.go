package proxy

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// F3 竞速拨号（TDD 先红）。
//
// 契约（2026-09-15-v1.0.1-gui-round2-f3-f12.md）：
//   - 无已验证上游时并行竞速拨多个候选，首成者服务连接，其余取消；
//   - 被取消/未起跑的候选不惩罚（Release 归还，不报失败）；
//   - 真实失败即时报（下轮排除）；全死在预算内快速 502；
//   - 热路径（已验证上游）保持单发，不竞速。

// fakeRacer 记录调度器侧被调用情况的假 RacingSelector。
type fakeRacer struct {
	mu        sync.Mutex
	validated string // 热路径返回值（空 = 无已验证上游）
	cands     []string
	picks     int
	fails     []string
	wins      []string
	released  []string
}

func (f *fakeRacer) Select(host string) (string, bool) { return "", false }

func (f *fakeRacer) ValidatedPick(host string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.validated == "" {
		return "", false
	}
	return f.validated, true
}

func (f *fakeRacer) RacingPick(host string, n int, exclude map[string]bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.picks++
	out := make([]string, 0, n)
	for _, a := range f.cands {
		if exclude[a] {
			continue
		}
		out = append(out, a)
		if len(out) >= n {
			break
		}
	}
	return out
}

func (f *fakeRacer) RacingReportFail(host, addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, addr)
}

func (f *fakeRacer) RacingReportWin(host, addr string, dialMS float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wins = append(f.wins, addr)
}

func (f *fakeRacer) RacingRelease(host string, addrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, addrs...)
}

// startEcho 起一个立即可用的 TCP 回显上游。
func startEcho(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}

// deadAddr 返回一个确定拒绝连接的地址（无监听）。
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func raceEvents(t *testing.T, sel UpstreamSelector) (string, chan Event, func()) {
	return startProxy(t, sel)
}

// 竞速胜出：死候选 + 活候选 → 隧道建立到活候选，失败候选被上报。
func TestRaceDeadAndAlivePicksWinner(t *testing.T) {
	dead := deadAddr(t)
	good, stopEcho := startEcho(t)
	defer stopEcho()

	fr := &fakeRacer{cands: []string{dead, good}}
	addr, events, stop := raceEvents(t, fr)
	defer stop()

	c := dialAndConnect(t, addr, "github.com:443")
	defer c.Close()

	// 回显往返验证隧道真的通了
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写隧道: %v", err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('g')
	if err != nil || line != "ping" {
		t.Fatalf("回显 = %q err=%v，want ping", line, err)
	}
	c.Close()

	select {
	case ev := <-events:
		if ev.Target != good {
			t.Fatalf("胜者应为活候选 %s，得 %s", good, ev.Target)
		}
		if ev.DialErr != nil {
			t.Fatalf("胜者不应有 DialErr: %v", ev.DialErr)
		}
		if ev.Attempts < 2 {
			t.Fatalf("Attempts 应 ≥2（死+活两候选），得 %d", ev.Attempts)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等连接事件超时")
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.fails) != 1 || fr.fails[0] != dead {
		t.Fatalf("死候选应被上报失败一次: %v", fr.fails)
	}
}

// 全死：预算内快速 502，全部候选报失败。
func TestRaceAllDeadFast502(t *testing.T) {
	dead1, dead2 := deadAddr(t), deadAddr(t)
	fr := &fakeRacer{cands: []string{dead1, dead2}}
	addr, _, stop := raceEvents(t, fr)
	defer stop()

	t0 := time.Now()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "502") {
		t.Fatalf("全死应回 502，得 %q", line)
	}
	if elapsed := time.Since(t0); elapsed > 12*time.Second {
		t.Fatalf("全死应在预算内快速失败，耗时 %v", elapsed)
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.fails) < 2 {
		t.Fatalf("两个死候选都应报失败: %v", fr.fails)
	}
}

// 取消不惩罚：胜者秒成时，错峰未起跑的候选被 Release 而非报失败。
func TestRaceCancelledNotPunished(t *testing.T) {
	good, stopEcho := startEcho(t)
	defer stopEcho()
	dead := deadAddr(t) // 第 2 候选：错峰窗口内从未起跑

	fr := &fakeRacer{cands: []string{good, dead}}
	addr, events, stop := raceEvents(t, fr)
	defer stop()

	c := dialAndConnect(t, addr, "github.com:443")
	c.Close()
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("等连接事件超时")
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, f := range fr.fails {
		if f == dead {
			t.Fatal("未起跑的候选不应被报失败（取消不惩罚）")
		}
	}
	if len(fr.released) == 0 {
		t.Fatal("未起跑的候选应被 Release 归还")
	}
}

// 热路径：有已验证上游时单发直连，不进竞速。
func TestRaceWarmFastPath(t *testing.T) {
	good, stopEcho := startEcho(t)
	defer stopEcho()

	fr := &fakeRacer{validated: good}
	addr, events, stop := raceEvents(t, fr)
	defer stop()

	c := dialAndConnect(t, addr, "github.com:443")
	c.Close()
	select {
	case ev := <-events:
		if ev.Target != good {
			t.Fatalf("热路径应直连已验证上游，得 %s", ev.Target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等连接事件超时")
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.picks != 0 {
		t.Fatalf("热路径不应触发竞速 Pick，得 %d 次", fr.picks)
	}
	if len(fr.fails) > 0 || len(fr.wins) > 0 {
		t.Fatalf("热路径不应有竞速上报: fails=%v wins=%v", fr.fails, fr.wins)
	}
}
