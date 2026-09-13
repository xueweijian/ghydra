package rules

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// R9 重启防重放（方案 §5 安全核心）：seen_max 持久化后，A3 防线跨
// 进程重启、跨磁盘损坏存活——攻击者控制网络也无法重放旧合法签名版。
func TestRefresherSeenMaxRestoreBlocksReplay(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	st := &fakeStateStore{}

	// --- 第一世：v3 拉取成功（store.seenMax 推进到 3） ---
	prov1, _ := newTestProvider(t, dir, s)
	r1 := NewRefresher(prov1, RefresherConfig{}, httpFetch(t, s, 3), nil,
		"https://raw.test/rules/current.json", "", st)
	if res, _ := r1.TriggerSync(); res != ResOK {
		t.Fatalf("world1: res=%q", res)
	}
	if got := prov1.SeenMax(); got != 3 {
		t.Fatalf("world1 seenMax=%d, want 3", got)
	}
	if st.seenMax != 3 {
		t.Fatalf("store seenMax=%d, want 3", st.seenMax)
	}

	// --- 第二世 A：新进程磁盘完好（disk v3）+ store 恢复 ---
	prov2, _ := newTestProvider(t, dir, s)
	applyRestore(prov2, st) // main.go 装配序列的等价物
	old, oldSig := signPair(s, 2, nil)
	r2 := NewRefresher(prov2, RefresherConfig{}, serveOld(old, oldSig), nil,
		"https://raw.test/rules/current.json", "", st)
	if res, _ := r2.TriggerSync(); res != ResRollback {
		t.Fatalf("replay v2 blocked: res=%q, want rollback", res)
	}

	// --- 第二世 B：磁盘被毁（A7）→ 回退 L0，seenMax 恢复自 store ---
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prov3, _ := newTestProvider(t, dir, s)
	if prov3.Snapshot().Version != 1 {
		t.Fatalf("after purge snapshot = %d, want embedded 1", prov3.Snapshot().Version)
	}
	applyRestore(prov3, st)
	if got := prov3.SeenMax(); got != 3 {
		t.Fatalf("restored seenMax = %d, want 3 (防线不随磁盘损坏回落)", got)
	}
	r3 := NewRefresher(prov3, RefresherConfig{}, serveOld(old, oldSig), nil,
		"https://raw.test/rules/current.json", "", st)
	if res, _ := r3.TriggerSync(); res != ResRollback {
		t.Fatalf("replay v2 after disk purge: res=%q, want rollback", res)
	}
	if prov3.Snapshot().Version != 1 {
		t.Fatalf("snapshot mutated to %d", prov3.Snapshot().Version)
	}
}

// httpFetch 产生活在 version 的双文件 fetch（R9 第一世用）。
func httpFetch(_ *testing.T, s *testSigner, version int64) Fetch {
	data, sig := signPair(s, version, nil)
	return serveOld(data, sig)
}

// applyRestore main.go 装配序列的等价物：store.SeenMax → Provider（只增）。
func applyRestore(p *Provider, st StateStore) {
	if v, ok, _ := st.SeenMax(); ok {
		p.SetSeenMax(v)
	}
}

// serveOld 恒定伺服同一对 (json, sig)。
func serveOld(data, sig []byte) Fetch {
	return func(_ context.Context, url string) ([]byte, error) {
		if strings.HasSuffix(url, ".minisig") {
			return sig, nil
		}
		return data, nil
	}
}

// R8 httptest 真 HTTP 端到端：Fetch 走真 http.Client，内容投毒（A1）
// 在传输层注入，验证验签防线对真实响应体生效。
func TestRefresherHTTPEndToEnd(t *testing.T) {
	s := newTestSigner()
	data, sig := signPair(s, 2, nil)
	poison := 0 // >0 = 篡改 json 第 N 字节
	mux := http.NewServeMux()
	mux.HandleFunc("/rules/current.json", func(w http.ResponseWriter, _ *http.Request) {
		b := append([]byte{}, data...)
		if poison > 0 {
			b[poison] ^= 0x01
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/rules/current.json.minisig", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sig)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prov, _ := newTestProvider(t, t.TempDir(), s)
	st := &fakeStateStore{}
	r := NewRefresher(prov, RefresherConfig{}, httpGet, nil, srv.URL+"/rules/current.json", "", st)

	if res, _ := r.TriggerSync(); res != ResOK {
		t.Fatalf("http e2e: res=%q", res)
	}
	if r.SnapshotVersion() != 2 {
		t.Fatalf("version = %d, want 2", r.SnapshotVersion())
	}

	// 内容投毒：json 改一字节、签名不变 → sig_rejected，快照冻结
	poison = 10
	if res, _ := r.TriggerSync(); res != ResSigRejected {
		t.Fatalf("poisoned: res=%q, want sig_rejected", res)
	}
	if r.SnapshotVersion() != 2 {
		t.Fatalf("poison mutated snapshot: %d", r.SnapshotVersion())
	}
}

// httpGet 生产 Fetch 包装的最小形态：真 HTTP GET + 尺寸上限
// （读超即断；main.go 的 channelFetch 同构）。
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("http " + resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, DefaultMaxJSON+1))
}
