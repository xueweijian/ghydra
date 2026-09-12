// Package sysproxy 实现系统代理的接管与恢复（M1-W3）。
//
// 三平台实现（D5：Windows 优先，真机验收）：
//   - Windows：注册表 Internet Settings + wininet 刷新（立即生效）
//   - macOS：  networksetup（PAC 用 -setautoproxyurl，代理用 -setwebproxy）
//   - Linux：  gsettings（GNOME）；无桌面环境时返回引导错误（R6 降级）
//
// 安全设计（D4）：Apply 前先快照原值（store 层持久化）；崩溃残留由
// 下次任何 ghydra 命令启动时的对账逻辑恢复（见 Snapshot）。
package sysproxy

import (
	"fmt"
)

// Setting 是系统代理的一个完整状态快照（空 Setting = 未设置代理）。
// PAC 模式优先于手动 ProxyServer（浏览器兼容面更广，github.com
// 等命中 PAC 走 GHydra，其余 DIRECT 不受影响）。
//
// W4.5 语义升级：Setting 是「desired-state 全量快照」而非「两字段
// 输入」——接管前的原值可能包含 ProxyEnable 位、bypass 列表
// （ProxyOverride）、WPAD 自动检测（AutoDetect）。旧版只存
// ProxyServer+PACURL，恢复时会丢掉这些字段（用户手动代理的
// bypass 列表被清空 / WPAD 被关闭 / 已禁用代理被错误启用）。
type Setting struct {
	ProxyServer   string // 手动代理 host:port；空 = 未设置该值
	ProxyEnabled  bool   // 手动代理启用位（Windows ProxyEnable）
	ProxyOverride string // bypass 列表，";" 分隔（Windows ProxyOverride / macOS bypass domains）
	PACURL        string // PAC 自动配置 URL；空 = 未用
	AutoDetect    bool   // WPAD（仅诊断快照用）：Windows 真值在 DefaultConnectionSettings
	// blob，DWORD 写不进（WinINet 刷新会归零），Apply 刻意不写它
}

// IsZero 未设置任何代理。
func (s Setting) IsZero() bool {
	return s.ProxyServer == "" && s.PACURL == "" && s.ProxyOverride == "" && !s.ProxyEnabled && !s.AutoDetect
}

// String 诊断输出。
func (s Setting) String() string {
	switch {
	case s.PACURL != "":
		return fmt.Sprintf("PAC %s", s.PACURL)
	case s.ProxyServer != "" && s.ProxyEnabled:
		return fmt.Sprintf("PROXY %s", s.ProxyServer)
	case s.ProxyServer != "":
		return fmt.Sprintf("PROXY %s（未启用）", s.ProxyServer)
	case s.AutoDetect:
		return "WPAD 自动检测"
	default:
		return "(无代理)"
	}
}

// ApplyError 提供平台降级时的用户引导（Linux 无桌面环境等场景）。
type ApplyError struct {
	Op   string
	Err  error
	Hint string // 用户可执行的下一步（gsettings 命令 / 手动设置路径）
}

func (e *ApplyError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %v\n%s", e.Op, e.Err, e.Hint)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

// Current 读当前系统代理设置。
func Current() (Setting, error) { return currentOS() }

// Apply 应用代理设置（desired-state 全量写入；调用方负责先快照）。
// PACURL 非空时用 PAC 模式；IsZero 时等价于 Clear。
func Apply(s Setting) error {
	if s.IsZero() {
		return clearOS()
	}
	return applyOS(s)
}

// Clear 清除系统代理（恢复直连）。
func Clear() error { return clearOS() }

// Supported 当前平台是否支持自动接管（用于 status/引导文案）。
func Supported() bool { return supportedOS() }
