package probe

// F5：「可达但慢」独立分级（WARN 级，不是 FAIL）。
//
// 契约：2xx 且（速率 < 100KB/s 或 TTFB > 2s）→ Class=slow、OK 保持
// true（退出码与可用率分子不受影响）；边界值不判慢；非 2xx / 已失败
// 的检查不动。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMaybeSlowBoundaries(t *testing.T) {
	mk := func(rate float64, ttfb float64) Check {
		return Check{OK: true, Class: ClassOK, Status: 200, RateBPS: rate, TTFBMS: ttfb}
	}
	cases := []struct {
		name string
		c    Check
		want Class
	}{
		{"速率 50KB/s 判慢", mk(50*1024, 100), ClassSlow},
		{"速率恰 100KB/s 不判（边界含）", mk(100*1024, 100), ClassOK},
		{"TTFB 2.1s 判慢", mk(1024*1024, 2100), ClassSlow},
		{"TTFB 恰 2s 不判（边界含）", mk(1024*1024, 2000), ClassOK},
		{"快而顺不判", mk(1024*1024, 300), ClassOK},
		{"无速率样本（0）只看 TTFB", mk(0, 2100), ClassSlow},
		{"无速率样本 + TTFB 快 → 不判", mk(0, 300), ClassOK},
	}
	for _, tc := range cases {
		c := tc.c
		maybeSlow(&c, 200)
		if c.Class != tc.want {
			t.Errorf("%s: class=%v want %v", tc.name, c.Class, tc.want)
		}
		if !c.OK {
			t.Errorf("%s: WARN 级不得降 OK（退出红线）", tc.name)
		}
	}

	// 非 2xx（401 等）：可达但不判慢
	c := Check{OK: true, Class: ClassOK, Status: http.StatusUnauthorized, RateBPS: 50 * 1024}
	maybeSlow(&c, http.StatusUnauthorized)
	if c.Class != ClassOK {
		t.Errorf("非 2xx 不判慢: %v", c.Class)
	}
	// 已失败的不动
	c2 := Check{OK: false, Class: ClassTimeout, Status: 0, RateBPS: 1}
	maybeSlow(&c2, 0)
	if c2.Class != ClassTimeout {
		t.Errorf("失败检查不动: %v", c2.Class)
	}
}

// 慢源站端到端：TTFB > 2s 的 200 响应 → Class=slow 且 OK=true。
func TestRunHTTPSlowEndpoint(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2200 * time.Millisecond) // TTFB 越过 2s 阈
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "slow but alive")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Timeout = 10 * time.Second
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} // test fixture only
	r := New(cfg)
	checks := r.RunEndpoints(context.Background(), ModeDirect, []Endpoint{{
		Scenario: "test", Name: "slow", URL: srv.URL,
	}})
	if len(checks) != 1 {
		t.Fatalf("checks = %d", len(checks))
	}
	c := checks[0]
	if c.Class != ClassSlow {
		t.Fatalf("慢源站应 Class=slow，得 %v（check: %+v）", c.Class, c)
	}
	if !c.OK || !c.Reachable || c.Status != http.StatusOK {
		t.Fatalf("WARN 级语义：可达且通过（ok=%v reach=%v status=%d）", c.OK, c.Reachable, c.Status)
	}
}
