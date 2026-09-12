package sysproxy

import (
	"encoding/json"
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
	if (Setting{AutoDetect: true}).IsZero() {
		t.Error("WPAD 单独开启不算零态")
	}
	if (Setting{ProxyEnabled: true}).IsZero() {
		t.Error("启用位单独开启不算零态")
	}
	cases := []struct {
		s    Setting
		want string
	}{
		{Setting{PACURL: "http://127.0.0.1:9801/pac"}, "PAC http://127.0.0.1:9801/pac"},
		{Setting{ProxyServer: "127.0.0.1:9801", ProxyEnabled: true}, "PROXY 127.0.0.1:9801"},
		{Setting{ProxyServer: "127.0.0.1:9801"}, "PROXY 127.0.0.1:9801（未启用）"},
		{Setting{AutoDetect: true}, "WPAD 自动检测"},
		{Setting{}, "(无代理)"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("String(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

// TestSettingJSONRoundtrip 快照序列化往返：daemon 存取走 JSON，
// 字段完整性 = 崩溃对账恢复的正确性前提（W4.5）。
func TestSettingJSONRoundtrip(t *testing.T) {
	in := Setting{
		ProxyServer:   "192.168.1.1:8888",
		ProxyEnabled:  true,
		ProxyOverride: "localhost;127.0.0.1;<local>",
		PACURL:        "",
		AutoDetect:    true,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Setting
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("roundtrip 不一致:\n in=%+v\nout=%+v\njson=%s", in, out, b)
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
