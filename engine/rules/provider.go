package rules

// Provider：三级信任地板 + 无锁热快照（M3-W2 设计 §2.3 / §5）。
//
// 地板链（信任自上而下兜底）：
//   L2 remote —— Apply() 验签+schema+版本全过后落盘并热替换
//   L1 disk   —— 启动加载 ~/.ghydra/rules/current.json(.minisig)，
//                加载时重验签（A7：磁盘篡改不比网络投毒更可信）
//   L0 embedded —— go:embed 出厂规则，编译期冻结，永不失败
//
// 快照语义：Snapshot() 无锁（atomic.Pointer），返回值只读不可变；
// Matcher/PAC/分流消费方每次现取指针，规则更新零锁生效。
// Stale 判定随时间漂移，故 Snapshot 携带 ExpiresAt、由 StaleNow(now)
// 动态判（A6：过期可观测、不失效——「旧规则好过没规则」）。

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Source 快照来源（对应信任地板层级）。
type Source string

const (
	SourceEmbedded Source = "embedded"
	SourceDisk     Source = "disk"
	SourceRemote   Source = "remote"
)

// DiskBase 磁盘规则对的基础文件名（current.json + current.json.minisig）。
const DiskBase = "current.json"

// maxSigBytes minisig 文件尺寸上限：四行结构，trusted comment 上百字节，
// 64 KiB 已是数量级冗余（A5 无尽数据防线在拉取侧另有 1 MiB 上限）。
const maxSigBytes = 64 << 10

// 版本防线错误（Apply 拒绝原因，doctor/refresh 可观测）。
var (
	// ErrRollback 回滚投毒拒绝（A3）：candidate ≤ seenMax。
	ErrRollback = errors.New("rules: rollback rejected")
	// ErrFastForward 快进 DoS 拒绝（A4）：candidate > MaxVersion。
	ErrFastForward = errors.New("rules: fast-forward rejected")
)

// Snapshot 规则快照：一次 Publish 后整体只读、并发安全。
type Snapshot struct {
	Matcher      *Matcher
	Version      int64
	Source       Source
	GeneratedAt  time.Time
	ExpiresAt    time.Time
	Domains      []string
	CDNEndpoints []string
	SeedIPs      map[string][]string
}

// StaleNow 判定快照在 now 时刻是否软过期（A6）。expires_at 时刻即过期
// （闭区间语义，与 RFC3339 展示一致）。
func (s *Snapshot) StaleNow(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// Provider 规则热更新载体。零值不可用，经 NewProvider 构造。
type Provider struct {
	snap atomic.Pointer[Snapshot]

	mu      sync.Mutex // Apply 串行化（发布路径独占）
	dir     string     // 磁盘对目录；空 = 禁用 L1/L2 落盘（纯内存）
	pub     ed25519.PublicKey
	keyID   []byte
	seenMax int64 // 本进程已见最大合法版本（磁盘或远程），回滚防线锚点
}

// NewProvider 构造 Provider：加载链 disk（重验签）→ embedded 兜底，
// 信任锚 = 编译期冻结公钥（keys.go）。
func NewProvider(dir string) *Provider {
	pub, keyID, err := ActiveKey()
	if err != nil {
		// 编译期常量解析失败 = 出厂资产损坏，embedded_test 已锁定不可能；
		// 这里同样走 embedded 快照（信任地板语义：永有可用规则），
		// 仅失去磁盘/远程路径（无信任锚不加载外部输入）。
		return newProviderWithKeyRaw(dir, nil, nil)
	}
	return newProviderWithKeyRaw(dir, pub, keyID[:])
}

// newProviderWithKey 测试注入密钥路径（provider_test）。
func newProviderWithKey(dir string, pub ed25519.PublicKey, keyID [8]byte) *Provider {
	k := keyID
	if k == ([8]byte{}) {
		return newProviderWithKeyRaw(dir, pub, nil) // 零值 = 不校验（测试注入）
	}
	return newProviderWithKeyRaw(dir, pub, k[:])
}

func newProviderWithKeyRaw(dir string, pub ed25519.PublicKey, keyID []byte) *Provider {
	p := &Provider{dir: dir, pub: pub, keyID: keyID}
	if snap, version, ok := p.loadDisk(); ok {
		p.snap.Store(snap)
		p.seenMax = version
		return p
	}
	snap := loadEmbedded()
	p.snap.Store(snap)
	p.seenMax = snap.Version
	return p
}

// loadDisk 尝试 L1：成对读 → 重验签 → schema。任何一步失败都视为
// 磁盘不可信（篡改/损坏/半状态），静默回退 embedded（拒绝原因由
// 调用侧在 serve 装配时记日志）。
func (p *Provider) loadDisk() (*Snapshot, int64, bool) {
	if p.dir == "" || p.pub == nil {
		return nil, 0, false
	}
	data, sig, err := LoadPair(p.dir, DiskBase)
	if err != nil {
		return nil, 0, false
	}
	if len(sig) > maxSigBytes {
		return nil, 0, false
	}
	if err := VerifyMinisign(p.pub, p.keyID, sig, data); err != nil {
		return nil, 0, false
	}
	rf, err := ParseRules(data)
	if err != nil {
		return nil, 0, false
	}
	return buildSnapshot(rf, SourceDisk), rf.Version, true
}

// loadEmbedded 解析 L0。编译期资产，解析失败 = 出厂即坏，panic 合理
// （embedded_test.go 锁定 CI 恒绿；运行时不可能走到）。
func loadEmbedded() *Snapshot {
	rf, err := ParseRules(EmbeddedJSON)
	if err != nil {
		panic(fmt.Sprintf("rules: embedded floor corrupted (compile-time asset): %v", err))
	}
	return buildSnapshot(rf, SourceEmbedded)
}

// buildSnapshot 从已验证的 RulesFile 构建只读快照（防御拷贝）。
func buildSnapshot(rf *RulesFile, src Source) *Snapshot {
	domains := make([]string, len(rf.Domains))
	copy(domains, rf.Domains)
	cdn := make([]string, len(rf.CDNEndpoints))
	copy(cdn, rf.CDNEndpoints)
	var seeds map[string][]string
	if len(rf.SeedIPs) > 0 {
		seeds = make(map[string][]string, len(rf.SeedIPs))
		for k, v := range rf.SeedIPs {
			ips := make([]string, len(v))
			copy(ips, v)
			seeds[k] = ips
		}
	}
	return &Snapshot{
		Matcher:      New(domains),
		Version:      rf.Version,
		Source:       src,
		GeneratedAt:  rf.GeneratedAt,
		ExpiresAt:    rf.ExpiresAt,
		Domains:      domains,
		CDNEndpoints: cdn,
		SeedIPs:      seeds,
	}
}

// Snapshot 返回当前规则快照。永不返回 nil（构造即发布）。
func (p *Provider) Snapshot() *Snapshot { return p.snap.Load() }

// Apply 接受一份远程候选（data + minisig）：验签 → schema → 版本 →
// 原子落盘 → 热替换。任一步失败：快照与磁盘双不变（拒绝即无副作用）。
// 成功返回新快照。串行化保证 seenMax 与磁盘对的因果一致。
func (p *Provider) Apply(data, sig []byte) (*Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.dir == "" || p.pub == nil {
		return nil, errors.New("rules: provider has no writable floor (no dir/key)")
	}
	if len(sig) > maxSigBytes {
		return nil, fmt.Errorf("%w: minisig %d bytes exceeds %d", ErrSigFormat, len(sig), maxSigBytes)
	}
	// ① 验签（A1/A2/A7）：没过签名的字节不值得看内容
	if err := VerifyMinisign(p.pub, p.keyID, sig, data); err != nil {
		return nil, err
	}
	// ② schema（A8/A9）：签名合法 ≠ 内容合法。超版本上限在此已拒，
	// 但错误归因转写为快进 DoS（观测语义，见 ErrVersionCap）。
	rf, err := ParseRules(data)
	if err != nil {
		if errors.Is(err, ErrVersionCap) {
			return nil, fmt.Errorf("%w: %v", ErrFastForward, err)
		}
		return nil, err
	}
	// ③ 版本（A3/A4）：回滚与快进在此判，先落盘后生效
	d, why := DecideVersion(p.seenMax, rf.Version)
	switch d {
	case RejectRollback:
		return nil, fmt.Errorf("%w: %s", ErrRollback, why)
	case RejectFastForward:
		return nil, fmt.Errorf("%w: %s", ErrFastForward, why)
	}
	// ④ 原子落盘（崩溃安全：json+sig 成对，半状态 = 重启回退）
	if err := SavePairAtomic(p.dir, DiskBase, data, sig); err != nil {
		return nil, fmt.Errorf("rules: persist: %w", err)
	}
	// ⑤ 热替换（先盘后内存：进程死在两步之间 = 重启读盘，语义一致）
	snap := buildSnapshot(rf, SourceRemote)
	p.snap.Store(snap)
	p.seenMax = rf.Version
	return snap, nil
}

// SetSeenMax 提升回滚防线锚点（phase 3 装配层从 rules_state 恢复；
// 只增不减——回退 seenMax 等于亲手拆掉 A3 防线）。取
// max(embedded, disk, 持久化值) 中的持久化值在此收口。
func (p *Provider) SetSeenMax(v int64) {
	p.mu.Lock()
	if v > p.seenMax {
		p.seenMax = v
	}
	p.mu.Unlock()
}

// SeenMax 当前回滚防线锚点（诊断用）。
func (p *Provider) SeenMax() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seenMax
}

// DiskDir 返回磁盘对目录（诊断/装配用；空串 = 纯内存模式）。
func (p *Provider) DiskDir() string { return p.dir }

// DefaultRulesDir 规则目录约定：~/.ghydra/rules（与 serve.json/api-token 同根）。
func DefaultRulesDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra", "rules")
}
