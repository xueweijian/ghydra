package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// 设计 §7 L1-2 / L1-8：Releases API JSON 解析 + 资产选择（U3 域白名单 / U8 无 minisig 拒）。

func loadReleaseFixture(t *testing.T) Release {
	t.Helper()
	data, err := os.ReadFile("testdata/release_latest.json")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := ParseLatestRelease(data)
	if err != nil {
		t.Fatalf("ParseLatestRelease: %v", err)
	}
	return rel
}

func TestParseLatestRelease(t *testing.T) {
	rel := loadReleaseFixture(t)
	if rel.TagName != "v1.0.1" {
		t.Errorf("TagName = %q", rel.TagName)
	}
	if rel.Prerelease || rel.Draft {
		t.Error("fixture 不应是 prerelease/draft")
	}
	if len(rel.Assets) != 5 {
		t.Fatalf("资产数 = %d，期望 5", len(rel.Assets))
	}
	// draft 的 release 整体拒绝（官方 API latest 不返回 draft，但防御）
	if _, err := ParseLatestRelease([]byte(`{"tag_name":"v0.9.0","draft":true,"prerelease":false,"assets":[]}`)); err == nil {
		t.Error("draft release 应报错")
	}
	// 垃圾 JSON
	if _, err := ParseLatestRelease([]byte(`{`)); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func TestSelectAsset(t *testing.T) {
	rel := loadReleaseFixture(t)

	plan, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if plan.Version != "1.0.1" {
		t.Errorf("Version = %q", plan.Version)
	}
	if plan.Archive.Name != "ghydra-windows-amd64.zip" {
		t.Errorf("Archive.Name = %q", plan.Archive.Name)
	}
	if plan.Checksums.Name != "checksums.txt" || plan.Minisig.Name != "checksums.txt.minisig" {
		t.Errorf("checksums 资产解析错误: %q %q", plan.Checksums.Name, plan.Minisig.Name)
	}

	// linux 资产
	plan2, err := SelectAsset(rel, "linux", "amd64", "1.0.0", SelectOpts{})
	if err != nil || plan2.Archive.Name != "ghydra-linux-amd64.tar.gz" {
		t.Fatalf("linux 资产选择: %v %+v", err, plan2)
	}

	// 无匹配平台资产
	if _, err := SelectAsset(rel, "linux", "riscv64", "1.0.0", SelectOpts{}); err == nil {
		t.Error("无 riscv64 资产应报错")
	}
}

func TestSelectAssetVersionGate(t *testing.T) {
	rel := loadReleaseFixture(t) // v1.0.1

	// 等值 → 无更新（U2）
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.1", SelectOpts{}); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("等值应 ErrNoUpdate，得到 %v", err)
	}
	// 回滚 → 拒（U2）
	if _, err := SelectAsset(rel, "windows", "amd64", "1.2.0", SelectOpts{}); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("回滚应 ErrNoUpdate，得到 %v", err)
	}
	// 前导 v 与裸版本等价
	if _, err := SelectAsset(rel, "windows", "amd64", "v1.0.1", SelectOpts{}); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("v 前缀等值应 ErrNoUpdate，得到 %v", err)
	}
	// 降级显式允许（逃生门）
	plan, err := SelectAsset(rel, "windows", "amd64", "1.2.0", SelectOpts{AllowDowngrade: true})
	if err != nil || plan.Version != "1.0.1" {
		t.Errorf("显式降级应放行，得到 plan=%+v err=%v", plan, err)
	}
}

func TestSelectAssetPrerelease(t *testing.T) {
	rel := loadReleaseFixture(t)
	rel.TagName = "v1.1.0-beta.1"
	rel.Prerelease = true

	// 默认跳过 prerelease（拍板 #3）
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{}); !errors.Is(err, ErrPrerelease) {
		t.Errorf("默认应跳过 prerelease（ErrPrerelease），得到 %v", err)
	}
	// --pre 开启后可装
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{AllowPrerelease: true}); err != nil {
		t.Errorf("--pre 下应可装 prerelease: %v", err)
	}
	// prerelease 但版本更低：即便 --pre 也不降级（版本门槛优先）
	if _, err := SelectAsset(rel, "windows", "amd64", "1.2.0", SelectOpts{AllowPrerelease: true}); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("低版本 prerelease 应 ErrNoUpdate，得到 %v", err)
	}
}

func TestSelectAssetNoMinisig(t *testing.T) {
	rel := loadReleaseFixture(t)
	// 剔除 minisig 资产 → U8 拒
	filtered := rel.Assets[:0]
	for _, a := range rel.Assets {
		if a.Name != "checksums.txt.minisig" {
			filtered = append(filtered, a)
		}
	}
	rel.Assets = filtered
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{}); !errors.Is(err, ErrNoMinisig) {
		t.Errorf("无 minisig 应 ErrNoMinisig（U8：忘签名 = 没人更新到它），得到 %v", err)
	}
}

func TestAssetDomainAllowed(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://github.com/xueweijian/ghydra/releases/download/v1.0.1/ghydra-windows-amd64.zip", true},
		{"https://objects.githubusercontent.com/ghydra/release-assets/abc123", true},
		{"https://release-assets.githubusercontent.com/12345/owner/repo", true},
		{"https://api.github.com/repos/xueweijian/ghydra/releases/latest", true},
		{"http://github.com/xueweijian/ghydra/releases/download/v1/x.zip", false}, // 必须 https
		{"https://evil.example.com/ghydra.zip", false},                            // 外域
		{"https://github.com.evil.com/x.zip", false},                              // 后缀伪装
		{"https://user:pass@github.com/x.zip", false},                             // 带凭证拒
		{"https://github.com:8443/x.zip", false},                                  // 非标准端口拒
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := AssetDomainAllowed(c.url); got != c.want {
			t.Errorf("AssetDomainAllowed(%q) = %v，期望 %v", c.url, got, c.want)
		}
	}
}

func TestSelectAssetRejectsForeignDomain(t *testing.T) {
	// U3：资产 URL 被换成外域（源替换攻击）→ 选择阶段即拒
	rel := loadReleaseFixture(t)
	for i := range rel.Assets {
		if rel.Assets[i].Name == "ghydra-windows-amd64.zip" {
			rel.Assets[i].URL = "https://evil.example.com/payload.zip"
		}
	}
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{}); err == nil {
		t.Error("外域资产 URL 必须拒（U3）")
	}
}

func TestSelectAssetSkipsBadVersion(t *testing.T) {
	// bad_version 与版本门槛协同：check 遇到坏版本直接跳过并提示
	rel := loadReleaseFixture(t) // v1.0.1
	st := &State{BadVersion: "1.0.1"}
	if _, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{State: st}); !errors.Is(err, ErrBadVersion) {
		t.Errorf("坏版本应 ErrBadVersion，得到 %v", err)
	}
	// 更高版本不受影响
	rel2 := loadReleaseFixture(t)
	rel2.TagName = "v1.0.2"
	st2 := &State{BadVersion: "1.0.1"}
	if _, err := SelectAsset(rel2, "windows", "amd64", "1.0.0", SelectOpts{State: st2}); err != nil {
		t.Errorf("更高版本不应被 bad_version 拦: %v", err)
	}
}

// ---- P4/D7：variant 寻址（gui 变体不误选 CLI 归档——W4p1 遗留收口） ----

func loadGUIReleaseFixture(t *testing.T) Release {
	t.Helper()
	data, err := os.ReadFile("testdata/release_gui.json")
	if err != nil {
		t.Fatalf("读 fixture: %v", err)
	}
	var rel Release
	if err := json.Unmarshal(data, &rel); err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return rel
}

func TestSelectAssetVariantGUI(t *testing.T) {
	rel := loadGUIReleaseFixture(t)

	// gui 变体命中 -gui 资产（windows zip）
	plan, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{Variant: "gui"})
	if err != nil {
		t.Fatalf("gui windows: %v", err)
	}
	if plan.Archive.Name != "ghydra-windows-amd64-gui.zip" {
		t.Errorf("gui 应选 gui.zip，得到 %q", plan.Archive.Name)
	}

	// gui 变体命中 -gui 资产（darwin tar.gz）
	plan2, err := SelectAsset(rel, "darwin", "arm64", "1.0.0", SelectOpts{Variant: "gui"})
	if err != nil {
		t.Fatalf("gui darwin: %v", err)
	}
	if plan2.Archive.Name != "ghydra-darwin-arm64-gui.tar.gz" {
		t.Errorf("gui darwin 应选 gui.tar.gz，得到 %q", plan2.Archive.Name)
	}

	// gui 变体不误选 CLI 归档（darwin-amd64 无 gui 资产 → 明确报错，绝不回落 CLI）
	_, err = SelectAsset(rel, "darwin", "amd64", "1.0.0", SelectOpts{Variant: "gui"})
	if err == nil {
		t.Fatal("darwin-amd64 无 gui 资产应报错（不得静默回落 CLI 归档——交换后 gui 消失）")
	}
	if !strings.Contains(err.Error(), "gui") {
		t.Errorf("报错应说明 gui 变体缺失: %v", err)
	}

	// CLI 变体不受 gui 资产干扰（回归锁定）
	plan3, err := SelectAsset(rel, "windows", "amd64", "1.0.0", SelectOpts{})
	if err != nil || plan3.Archive.Name != "ghydra-windows-amd64.zip" {
		t.Errorf("CLI 应选 CLI.zip: %v %+v", err, plan3)
	}
}
