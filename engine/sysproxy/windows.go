//go:build windows

// Windows 实现：注册表 HKCU\...\Internet Settings。
// ProxyServer 手动模式 / AutoConfigURL PAC 模式（优先）。
// 设置后调用 wininet InternetSetOption 通知所有应用立即生效
// （无需注销/重启——这是 Windows 代理工具的标准做法）。
package sysproxy

import (
	"fmt"
	"syscall"
	"unsafe"

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
	if url, _, err := k.GetStringValue("AutoConfigURL"); err == nil {
		s.PACURL = url
	}
	if enabled, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && enabled == 1 {
		if srv, _, err := k.GetStringValue("ProxyServer"); err == nil {
			s.ProxyServer = srv
		}
	}
	return s, nil
}

func applyOS(s Setting) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表: %w", err)
	}
	defer k.Close()

	if s.PACURL != "" {
		if err := k.SetStringValue("AutoConfigURL", s.PACURL); err != nil {
			return err
		}
		// PAC 模式下停用手动代理，避免叠加
		return k.SetDWordValue("ProxyEnable", 0)
	}
	if err := k.SetStringValue("ProxyServer", s.ProxyServer); err != nil {
		return err
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	return refreshWininet()
}

func clearOS() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表: %w", err)
	}
	defer k.Close()

	if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
	if err := k.DeleteValue("AutoConfigURL"); err != nil && err != registry.ErrNotExist {
		return err
	}
	return refreshWininet()
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
	_ = unsafe.Pointer(nil)
	return nil
}
