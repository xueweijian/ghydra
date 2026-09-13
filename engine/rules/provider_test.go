package rules

// Provider 三级信任地板（M3-W2 设计 §2.3/§5）测试：先于实现冻结行为。
//
// 覆盖矩阵（§7 L1-5 + Apply 全路径）：
//   - L0 永在：空目录/磁盘坏 → embedded 快照可用
//   - L1 磁盘：合法签名对 → disk 快照；篡改/坏 schema → 回退 L0（A7）
//   - L2 Apply：验签→schema→版本→落盘→热替换 全序
//   - 拒绝路径三件套：伪造/回滚/快进，快照与磁盘双不变
//   - 并发：Snapshot() 无锁读与 Apply 竞争（-race CI）

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mustRulesJSON 构造一份合法 rules.json（紧凑、可签名）。
func mustRulesJSON(t *testing.T, version int64, extraDomains []string, gen, exp time.Time) []byte {
	t.Helper()
	domains := append([]string{"github.com", "*.github.com"}, extraDomains...)
	b, err := json.Marshal(RulesFile{
		SchemaVersion: 1,
		Version:       version,
		GeneratedAt:   gen.UTC(),
		ExpiresAt:     exp.UTC(),
		Domains:       domains,
		CDNEndpoints:  []string{"https://gh.1ciyuan.cn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testTimes() (gen, exp time.Time) {
	return time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
}

// newTestProvider 用测试密钥建 Provider（生产 NewProvider 冻结公钥）。
// keyID 传 nil = 不校验 blob key_id（testSigner 不写该段；key_id 匹配
// 逻辑由 minisign_test 的官方向量覆盖，provider 测试聚焦地板链）。
func newTestProvider(t *testing.T, dir string, s *testSigner) (*Provider, [8]byte) {
	t.Helper()
	var zero [8]byte
	return newProviderWithKey(dir, s.pub, zero), zero
}

func TestProviderEmbeddedFloor(t *testing.T) {
	p, _ := newTestProvider(t, t.TempDir(), newTestSigner())
	snap := p.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() = nil")
	}
	if snap.Source != SourceEmbedded {
		t.Fatalf("source = %q, want embedded", snap.Source)
	}
	if snap.Version != 1 {
		t.Fatalf("embedded version = %d, want 1", snap.Version)
	}
	if !snap.Matcher.Match("api.github.com:443") {
		t.Fatal("embedded matcher 应命中 api.github.com")
	}
	if len(snap.CDNEndpoints) == 0 {
		t.Fatal("embedded cdn endpoints 为空")
	}
}

func TestProviderDiskLoad(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	gen, exp := testTimes()
	data := mustRulesJSON(t, 2, []string{"extra.example.com"}, gen, exp)
	sig := s.signFile(data, "ghydra-rules v2")

	if err := SavePairAtomic(dir, DiskBase, data, sig); err != nil {
		t.Fatal(err)
	}
	p, _ := newTestProvider(t, dir, s)
	snap := p.Snapshot()
	if snap.Source != SourceDisk {
		t.Fatalf("source = %q, want disk", snap.Source)
	}
	if snap.Version != 2 {
		t.Fatalf("version = %d, want 2", snap.Version)
	}
	if !snap.Matcher.Match("extra.example.com") {
		t.Fatal("磁盘规则新域名未生效")
	}
}

func TestProviderDiskTamperedFallsBack(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	gen, exp := testTimes()
	data := mustRulesJSON(t, 2, []string{"extra.example.com"}, gen, exp)
	sig := s.signFile(data, "ghydra-rules v2")
	if err := SavePairAtomic(dir, DiskBase, data, sig); err != nil {
		t.Fatal(err)
	}
	// 磁盘篡改（A7）：改一字节 → 加载时重验签失败 → 回退 L0
	bad := append([]byte{}, data...)
	bad[10] ^= 0x01
	if err := SavePairAtomic(dir, DiskBase, bad, sig); err != nil {
		t.Fatal(err)
	}
	p, _ := newTestProvider(t, dir, s)
	if snap := p.Snapshot(); snap.Source != SourceEmbedded {
		t.Fatalf("source = %q, want embedded（篡改必须回退）", snap.Source)
	}
}

func TestProviderDiskSchemaRejectFallsBack(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	// 签名合法但 schema 拒：未知字段（语义投毒防线同样保护磁盘路径）
	data := []byte(`{"schema_version":1,"version":2,"generated_at":"2026-09-13T00:00:00Z","expires_at":"2026-10-28T00:00:00Z","domains":["github.com"],"cdn_endpoints":[],"seed_ips":{},"evil":1}`)
	sig := s.signFile(data, "ghydra-rules v2")
	if err := SavePairAtomic(dir, DiskBase, data, sig); err != nil {
		t.Fatal(err)
	}
	p, _ := newTestProvider(t, dir, s)
	if snap := p.Snapshot(); snap.Source != SourceEmbedded {
		t.Fatalf("source = %q, want embedded（schema 拒必须回退）", snap.Source)
	}
}

func TestApplyRemoteUpgrade(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)
	gen, exp := testTimes()

	v2 := mustRulesJSON(t, 2, []string{"new.example.org"}, gen, exp)
	sig2 := s.signFile(v2, "ghydra-rules v2")
	if _, err := p.Apply(v2, sig2); err != nil {
		t.Fatalf("Apply v2: %v", err)
	}
	snap := p.Snapshot()
	if snap.Source != SourceRemote {
		t.Fatalf("source = %q, want remote", snap.Source)
	}
	if snap.Version != 2 || !snap.Matcher.Match("new.example.org") {
		t.Fatalf("v2 未热生效: version=%d", snap.Version)
	}

	// 落盘持久化：重启（新 Provider）应从磁盘读到 v2
	p2, _ := newTestProvider(t, dir, s)
	if s2 := p2.Snapshot(); s2.Source != SourceDisk || s2.Version != 2 {
		t.Fatalf("重启后 source=%q version=%d, want disk/2", s2.Source, s2.Version)
	}
	// 磁盘字节与 Apply 输入一致（崩溃安全对）
	d, g, err := LoadPair(dir, DiskBase)
	if err != nil || string(d) != string(v2) || string(g) != string(sig2) {
		t.Fatalf("磁盘对与输入不一致: err=%v", err)
	}
}

func TestApplyRollbackRejected(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)
	gen, exp := testTimes()

	v3 := mustRulesJSON(t, 3, nil, gen, exp)
	if _, err := p.Apply(v3, s.signFile(v3, "ghydra-rules v3")); err != nil {
		t.Fatalf("Apply v3: %v", err)
	}
	// v3 后重放 v2（A3 回滚投毒）：拒，快照不动
	v2 := mustRulesJSON(t, 2, []string{"evil.example.com"}, gen, exp)
	if _, err := p.Apply(v2, s.signFile(v2, "ghydra-rules v2")); err == nil {
		t.Fatal("回滚必须被拒")
	} else if !errors.Is(err, ErrRollback) {
		t.Fatalf("错误类型 = %v, want ErrRollback", err)
	}
	if snap := p.Snapshot(); snap.Version != 3 || snap.Matcher.Match("evil.example.com") {
		t.Fatal("拒绝后快照被污染")
	}
}

func TestApplyFastForwardRejected(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)
	gen, exp := testTimes()
	// 签名完全合法但版本超上限（A4 快进 DoS）
	vbig := mustRulesJSON(t, MaxVersion+1, nil, gen, exp)
	if _, err := p.Apply(vbig, s.signFile(vbig, "ghydra-rules big")); err == nil {
		t.Fatal("快进必须被拒")
	} else if !errors.Is(err, ErrFastForward) {
		t.Fatalf("错误类型 = %v, want ErrFastForward", err)
	}
	if snap := p.Snapshot(); snap.Version != 1 {
		t.Fatal("拒绝后快照被污染")
	}
}

func TestApplyForgeryRejected(t *testing.T) {
	dir := t.TempDir()
	sGood := newTestSigner() // Provider 信任的
	p, _ := newTestProvider(t, dir, sGood)

	sEvil := newTestSigner() // 攻击者密钥（A1）
	gen, exp := testTimes()
	v2 := mustRulesJSON(t, 2, []string{"evil.example.com"}, gen, exp)
	if _, err := p.Apply(v2, sEvil.signFile(v2, "evil")); err == nil {
		t.Fatal("伪造签名必须被拒")
	}
	// 磁盘也不能被写脏
	if _, _, err := LoadPair(dir, DiskBase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("伪造输入后磁盘状态异常: %v", err)
	}
	if snap := p.Snapshot(); snap.Version != 1 || snap.Matcher.Match("evil.example.com") {
		t.Fatal("拒绝后快照被污染")
	}
}

func TestApplyBadSchemaRejected(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)
	// 签名合法但 domains 为空（A8 内容校验）
	data := []byte(`{"schema_version":1,"version":2,"generated_at":"2026-09-13T00:00:00Z","expires_at":"2026-10-28T00:00:00Z","domains":[],"cdn_endpoints":[],"seed_ips":{}}`)
	if _, err := p.Apply(data, s.signFile(data, "ghydra-rules v2")); err == nil {
		t.Fatal("schema 违规必须被拒")
	}
	if snap := p.Snapshot(); snap.Version != 1 {
		t.Fatal("拒绝后快照被污染")
	}
}

func TestSnapshotConcurrentReadApply(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	p, _ := newTestProvider(t, dir, s)
	gen, exp := testTimes()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 读侧：无锁热读
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := p.Snapshot()
				if snap == nil || snap.Matcher == nil || !snap.Matcher.Match("github.com") {
					t.Error("并发读拿到坏快照")
					return
				}
			}
		}()
	}
	// 写侧：连续升级到 v6
	for v := int64(2); v <= 6; v++ {
		data := mustRulesJSON(t, v, nil, gen, exp)
		if _, err := p.Apply(data, s.signFile(data, "ghydra-rules")); err != nil {
			t.Fatalf("Apply v%d: %v", v, err)
		}
	}
	close(stop)
	wg.Wait()
	if snap := p.Snapshot(); snap.Version != 6 {
		t.Fatalf("final version = %d, want 6", snap.Version)
	}
}

func TestSnapshotStaleNow(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	gen, _ := testTimes()
	exp := gen.Add(2 * time.Hour) // 2h 后过期

	p, _ := newTestProvider(t, dir, s)
	v2 := mustRulesJSON(t, 2, nil, gen, exp)
	if _, err := p.Apply(v2, s.signFile(v2, "ghydra-rules v2")); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()
	if snap.StaleNow(gen.Add(time.Hour)) {
		t.Fatal("未到期不应 stale")
	}
	if !snap.StaleNow(exp.Add(time.Minute)) {
		t.Fatal("到期后应 stale")
	}
	// 边界：恰在 expires_at 时刻 = 过期（RFC3339 语义闭区间）
	if !snap.StaleNow(exp) {
		t.Fatal("expires_at 时刻应判 stale")
	}
}

func TestProviderSeedsExposed(t *testing.T) {
	dir := t.TempDir()
	s := newTestSigner()
	gen, exp := testTimes()
	data := []byte(`{"schema_version":1,"version":2,"generated_at":"2026-09-13T00:00:00Z","expires_at":"2026-10-28T00:00:00Z","domains":["github.com"],"cdn_endpoints":[],"seed_ips":{"github.com":["140.82.112.3"]}}`)
	if err := SavePairAtomic(dir, DiskBase, data, s.signFile(data, "v2")); err != nil {
		t.Fatal(err)
	}
	p, _ := newTestProvider(t, dir, s)
	snap := p.Snapshot()
	if got := snap.SeedIPs["github.com"]; len(got) != 1 || got[0] != "140.82.112.3" {
		t.Fatalf("seed ips = %v", got)
	}
	if !snap.GeneratedAt.Equal(gen) || !snap.ExpiresAt.Equal(exp) {
		t.Fatalf("时间戳丢失: gen=%v exp=%v", snap.GeneratedAt, snap.ExpiresAt)
	}
}

var _ = filepath.Join // 保持 import（未来扩展用）
