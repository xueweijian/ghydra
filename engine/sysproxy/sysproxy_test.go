package sysproxy

import (
	"runtime"
	"testing"
)

func TestSettingSemantics(t *testing.T) {
	if !(Setting{}).IsZero() {
		t.Error("空 Setting 应 IsZero")
	}
	if (Setting{ProxyServer: "x:1"}).IsZero() {
		t.Error("有值不应 IsZero")
	}
	if s := (Setting{PACURL: "http://127.0.0.1:9801/pac"}).String(); s != "PAC http://127.0.0.1:9801/pac" {
		t.Errorf("String = %q", s)
	}
	if s := (Setting{ProxyServer: "127.0.0.1:9801"}).String(); s != "PROXY 127.0.0.1:9801" {
		t.Errorf("String = %q", s)
	}
}

func TestCurrentNoDesktopLinux(t *testing.T) {
	// 沙箱/无桌面 Linux：Current 应返回带引导的错误而非崩溃
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux")
	}
	_, err := Current()
	if err == nil && Supported() {
		return // 有桌面的环境（CI 某些 runner）——通过
	}
	if err == nil && !Supported() {
		t.Fatal("无桌面环境 Current 不应成功")
	}
}

func TestApplyZeroClears(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("CI 上不清真实系统代理设置；真机验收覆盖")
	}
	// Linux 无桌面：Apply(空) = clearOS = no-op nil
	if err := Apply(Setting{}); err != nil {
		t.Fatalf("空 Setting 应等价清除（无桌面 no-op）: %v", err)
	}
}
