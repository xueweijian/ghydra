//go:build windows && integration

// Windows 注册表接管集成测试（W4.5）。
//
// 运行环境：GitHub Actions windows runner（ephemeral，改 HKCU 注册表
// 无真实用户影响）。这层验证的是「Windows 平台行为证明」——注册表
// 全量写入/删除/恢复语义 + WinINet 刷新调用不报错。大陆网络下的
// 真实浏览器/Git 行为仍属现场验收（DevWorkflow 三环境分工）。
//
// 运行：go test -tags=integration ./engine/sysproxy/... -run Integration -v
package sysproxy

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

// rawRead 直读注册表（不经被测 API，独立校验）。
type rawState struct {
	autoConfigURL string
	autoConfigOK  bool
	proxyServer   string
	proxyServerOK bool
	proxyOverride string
	overrideOK    bool
	proxyEnable   uint64
	proxyEnableOK bool
	autoDetect    uint64
	autoDetectOK  bool
}

func rawCurrent() rawState {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.QUERY_VALUE)
	if err != nil {
		panic(err)
	}
	defer k.Close()
	var r rawState
	var errv error
	r.autoConfigURL, _, errv = k.GetStringValue("AutoConfigURL")
	r.autoConfigOK = errv == nil
	r.proxyServer, _, errv = k.GetStringValue("ProxyServer")
	r.proxyServerOK = errv == nil
	r.proxyOverride, _, errv = k.GetStringValue("ProxyOverride")
	r.overrideOK = errv == nil
	r.proxyEnable, _, _ = k.GetIntegerValue("ProxyEnable")
	r.proxyEnableOK = true // ProxyEnable 总是存在（默认 0）
	r.autoDetect, _, errv = k.GetIntegerValue("AutoDetect")
	r.autoDetectOK = errv == nil
	return r
}

// restore 恢复 runner 原值（测试纪律：不留痕迹）。
func restore(t *testing.T, orig rawState) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	setOrDel := func(name, val string, ok bool) {
		if ok {
			k.SetStringValue(name, val)
		} else {
			k.DeleteValue(name)
		}
	}
	setOrDel("AutoConfigURL", orig.autoConfigURL, orig.autoConfigOK)
	setOrDel("ProxyServer", orig.proxyServer, orig.proxyServerOK)
	setOrDel("ProxyOverride", orig.proxyOverride, orig.overrideOK)
	if orig.proxyEnableOK {
		k.SetDWordValue("ProxyEnable", uint32(orig.proxyEnable))
	} else {
		k.DeleteValue("ProxyEnable")
	}
	if orig.autoDetectOK {
		k.SetDWordValue("AutoDetect", uint32(orig.autoDetect))
	} else {
		k.DeleteValue("AutoDetect")
	}
}

func assertEq(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// TestIntegrationPACTakeover PAC 接管全量语义：写入 AutoConfigURL，
// 删/关手动代理与 WPAD，恢复后完全回到原值。
func TestIntegrationPACTakeover(t *testing.T) {
	orig := rawCurrent()
	t.Cleanup(func() { restore(t, orig) })

	// 接管前预置一个「手动代理 + bypass + WPAD」的复杂原值，
	// 验证恢复不丢字段（W4.5 的核心回归点）
	pre := Setting{
		ProxyServer:   "10.0.0.9:3128",
		ProxyEnabled:  true,
		ProxyOverride: "localhost;127.0.0.1;<local>",
		AutoDetect:    true,
	}
	if err := Apply(pre); err != nil {
		t.Fatalf("预置原值: %v", err)
	}
	cur, err := Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != pre {
		t.Fatalf("预置后 Current 不一致:\n want=%+v\n got =%+v", pre, cur)
	}

	// PAC 接管（ghydra on 的实际输入）
	pac := Setting{PACURL: "http://127.0.0.1:9801/pac"}
	if err := Apply(pac); err != nil {
		t.Fatalf("PAC 接管: %v", err)
	}
	r := rawCurrent()
	assertEq(t, "AutoConfigURL", r.autoConfigURL, "http://127.0.0.1:9801/pac")
	if r.proxyEnable != 0 {
		t.Errorf("PAC 模式 ProxyEnable 应为 0, got %d", r.proxyEnable)
	}
	if r.autoDetect != 0 {
		t.Errorf("PAC 模式 AutoDetect 应为 0（避免 WPAD 抢答）, got %d", r.autoDetect)
	}
	assertEq(t, "ProxyServer（值保留）", r.proxyServer, "10.0.0.9:3128")

	// 恢复（ghydra off 的实际路径 = Apply(快照)）
	if err := Apply(pre); err != nil {
		t.Fatalf("恢复原值: %v", err)
	}
	r2 := rawCurrent()
	assertEq(t, "恢复 ProxyServer", r2.proxyServer, "10.0.0.9:3128")
	assertEq(t, "恢复 ProxyOverride", r2.proxyOverride, "localhost;127.0.0.1;<local>")
	if r2.proxyEnable != 1 {
		t.Errorf("恢复 ProxyEnable 应为 1, got %d", r2.proxyEnable)
	}
	if r2.autoDetect != 1 {
		t.Errorf("恢复 AutoDetect 应为 1, got %d", r2.autoDetect)
	}
	if r2.autoConfigOK {
		t.Errorf("恢复后 AutoConfigURL 应被删除, got %q", r2.autoConfigURL)
	}
}

// TestIntegrationManualProxyTakeover 手动代理模式接管 + 禁用态快照。
func TestIntegrationManualProxyTakeover(t *testing.T) {
	orig := rawState{}
	t.Cleanup(func() { restore(t, orig) })

	// 场景 1：原值 = 有代址但未启用（用户曾配过手动代理又关了）
	disabled := Setting{ProxyServer: "10.1.2.3:8080"} // ProxyEnabled=false
	if err := Apply(disabled); err != nil {
		t.Fatalf("预置禁用态: %v", err)
	}
	r := rawCurrent()
	assertEq(t, "ProxyServer", r.proxyServer, "10.1.2.3:8080")
	if r.proxyEnable != 0 {
		t.Fatalf("禁用态 ProxyEnable 应为 0, got %d", r.proxyEnable)
	}
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if cur.ProxyEnabled {
		t.Error("Current 应报告 ProxyEnabled=false")
	}
	if cur.ProxyServer != "10.1.2.3:8080" {
		t.Errorf("Current 应保留禁用代址值: %q", cur.ProxyServer)
	}

	// ghydra on --mode proxy 的实际输入
	manual := Setting{ProxyServer: "127.0.0.1:9801", ProxyEnabled: true, ProxyOverride: "localhost;127.0.0.1"}
	if err := Apply(manual); err != nil {
		t.Fatalf("手动接管: %v", err)
	}
	r2 := rawCurrent()
	if r2.proxyEnable != 1 {
		t.Errorf("手动模式 ProxyEnable 应为 1, got %d", r2.proxyEnable)
	}
	assertEq(t, "ProxyServer", r2.proxyServer, "127.0.0.1:9801")
	assertEq(t, "ProxyOverride", r2.proxyOverride, "localhost;127.0.0.1")

	// 恢复禁用态：值写回、启用位归零
	if err := Apply(disabled); err != nil {
		t.Fatalf("恢复禁用态: %v", err)
	}
	r3 := rawCurrent()
	assertEq(t, "恢复 ProxyServer", r3.proxyServer, "10.1.2.3:8080")
	if r3.proxyEnable != 0 {
		t.Errorf("恢复禁用态 ProxyEnable 应为 0, got %d", r3.proxyEnable)
	}
	if r3.overrideOK {
		t.Errorf("恢复禁用态应删除 ProxyOverride, got %q", r3.proxyOverride)
	}
}

// TestIntegrationClearToZero 原值为「无代理」时的接管与恢复（runner 默认态）。
func TestIntegrationClearToZero(t *testing.T) {
	orig := rawCurrent()
	t.Cleanup(func() { restore(t, orig) })

	zero := Setting{}
	if err := Apply(zero); err != nil {
		t.Fatalf("Apply(零态): %v", err)
	}
	r := rawCurrent()
	if r.proxyEnable != 0 || r.autoDetect != 0 {
		t.Errorf("零态应全关: ProxyEnable=%d AutoDetect=%d", r.proxyEnable, r.autoDetect)
	}
	if r.autoConfigOK {
		t.Errorf("零态应删 AutoConfigURL, got %q", r.autoConfigURL)
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if r2 := rawCurrent(); r2.proxyEnable != 0 {
		t.Errorf("Clear 后 ProxyEnable=%d", r2.proxyEnable)
	}
}
