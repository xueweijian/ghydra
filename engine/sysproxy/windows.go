//go:build windows

// Windows 实现：注册表 HKCU\...\Internet Settings。
// ProxyServer 手动模式 / AutoConfigURL PAC 模式（优先）。
// 设置后调用 wininet InternetSetOption 通知所有应用立即生效
// （无需注销/重启——这是 Windows 代理工具的标准做法）。
//
// W4.5 语义：applyOS 是 desired-state 全量写入——Setting 里未设置
// 的字段会从注册表删除/归零（如 PAC 接管时删 AutoConfigURL、关
// AutoDetect），恢复时全量写回快照值。currentOS 读取全部五个值，
// 含「ProxyServer 有值但 ProxyEnable=0」的禁用态（恢复时写回值
// 但不启用——用户切回手动代理时代址还在）。
package sysproxy

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const internetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func supportedOS() bool { return true }

func currentOS() (Setting, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.QUERY_VALUE)
	if err != nil {
		return Setting{}, fmt.Errorf("打开注册表: %w", err)
	}
	defer k.Close()

	var s Setting
	// 各值独立读取、不存在不算错（默认装机只有 ProxyEnable=0）
	if url, _, err := k.GetStringValue("AutoConfigURL"); err == nil {
		s.PACURL = url
	}
	if srv, _, err := k.GetStringValue("ProxyServer"); err == nil {
		s.ProxyServer = srv
	}
	if ov, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		s.ProxyOverride = ov
	}
	if en, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && en == 1 {
		s.ProxyEnabled = true
	}
	// AutoDetect 存在位标志归一化：WinINet 会把 1 规范化为 9
	//（bit3 为内部标志；windows-2022 runner 实测）。按 bit0 判启用。
	if ad, _, err := k.GetIntegerValue("AutoDetect"); err == nil && ad&1 == 1 {
		s.AutoDetect = true
	}
	return s, nil
}

func applyOS(s Setting) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表: %w", err)
	}
	defer k.Close()

	// AutoConfigURL：非空写入，空则删除（PAC 残留会让 WinINet 继续走旧 PAC）
	if s.PACURL != "" {
		if err := k.SetStringValue("AutoConfigURL", s.PACURL); err != nil {
			return fmt.Errorf("写 AutoConfigURL: %w", err)
		}
	} else if err := k.DeleteValue("AutoConfigURL"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("删 AutoConfigURL: %w", err)
	}

	// ProxyServer/ProxyOverride：值本身保留（禁用态也写回，用户切回手动时代址还在）
	if s.ProxyServer != "" {
		if err := k.SetStringValue("ProxyServer", s.ProxyServer); err != nil {
			return fmt.Errorf("写 ProxyServer: %w", err)
		}
	}
	if s.ProxyOverride != "" {
		if err := k.SetStringValue("ProxyOverride", s.ProxyOverride); err != nil {
			return fmt.Errorf("写 ProxyOverride: %w", err)
		}
	} else if err := k.DeleteValue("ProxyOverride"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("删 ProxyOverride: %w", err)
	}

	// 启用位：PAC 接管时 ProxyEnable=0 避免与手动代理叠加；
	// AutoDetect=0 避免 WPAD 抢答（PAC 与 WPAD 同时开时行为不可预期）。
	if err := k.SetDWordValue("ProxyEnable", b2i(s.ProxyEnabled)); err != nil {
		return fmt.Errorf("写 ProxyEnable: %w", err)
	}
	if err := k.SetDWordValue("AutoDetect", b2i(s.AutoDetect)); err != nil {
		return fmt.Errorf("写 AutoDetect: %w", err)
	}
	return refreshWininet()
}

func clearOS() error {
	return applyOS(Setting{})
}

func b2i(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// refreshWininet 通知系统设置已变更并刷新（39=SETTINGS_CHANGED, 37=REFRESH）。
func refreshWininet() error {
	dll := syscall.NewLazyDLL("wininet.dll")
	proc := dll.NewProc("InternetSetOptionW")
	if r, _, _ := proc.Call(0, 39, 0, 0); r == 0 {
		return fmt.Errorf("InternetSetOption(SETTINGS_CHANGED) 失败")
	}
	if r, _, _ := proc.Call(0, 37, 0, 0); r == 0 {
		return fmt.Errorf("InternetSetOption(REFRESH) 失败")
	}
	return nil
}
