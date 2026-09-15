package sched

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/rules"
)

// F3 集成：真 Scheduler + 真 proxy.Server 走完整 CONNECT 路径——
// 「慢死候选不拖垮快活候选」（冷启动死 IP 池自愈的最小复现）。
//
// 池形态（对齐 2026-09-15 实证）：1 个拨号挂死型死 IP（meta 老段）
// + 1 个立即可用活 IP（DoH 就近）。修复前：单发选中死 IP → 7s 超时
// → 502。修复后：错峰竞速，活候选毫秒级胜出。
func TestSelectorRacingIntegration(t *testing.T) {
	// 活上游：本地回显
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
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
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	alive := ln.Addr().String()
	dead := "127.0.0.1:1" // 无监听：立即拒绝（确定性死候选）

	sc := New(DefaultConfig())
	deadSlow := errors.New("模拟拨号挂死（meta 老段）")
	sc.DialHost = func(host, addr string, timeout time.Duration) error {
		if addr == dead {
			time.Sleep(timeout) // 挂死到超时（预筛路径同款形态）
			return deadSlow
		}
		return nil
	}
	sel := NewSelector(sc, rules.New(rules.DefaultDomains))
	sc.AddCandidates("github.com", []string{dead, alive})

	// 真 proxy.Server + 真 CONNECT 客户端
	pln, err := proxy.NewListener("127.0.0.1:0", 16)
	if err != nil {
		t.Fatal(err)
	}
	srv := &proxy.Server{Selector: sel}
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(pln) }()
	defer func() { pln.Close(); <-done }()

	t0 := time.Now()
	c, err := net.DialTimeout("tcp", pln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	fmt.Fprintf(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if len(line) < 12 || line[:12] != "HTTP/1.1 200" {
		t.Fatalf("冷启动 CONNECT 应经竞速自愈回 200，得 %q（耗时 %v）", line, time.Since(t0))
	}
	for { // 吃掉剩余响应头
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	// 回显往返
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got, err := br.ReadString('g')
	if err != nil || got != "ping" {
		t.Fatalf("隧道回显 = %q err=%v", got, err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("死候选在池也不应拖垮首请求（%.0fms）", el.Seconds()*1000)
	}

	// 胜者已置 Active → 热路径粘上它（后续请求零竞速）
	if _, ok := sc.PickValidated("github.com"); !ok {
		t.Fatal("竞速胜者应已置 Active（PickValidated 可用）")
	}
	if st := sc.StickyIP("github.com"); st != alive {
		t.Fatalf("粘性应绑活候选 %s，得 %s", alive, st)
	}
}
