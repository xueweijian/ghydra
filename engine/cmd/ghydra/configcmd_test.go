package main

// configcmd_test.go —— P4/D6 L1：CLI 侧 config 值校验（与 POST /api/config
// 同源规则的表驱动覆盖）。

import "testing"

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		key, val string
		ok       bool
	}{
		{"cdn", "https://gh-proxy.com/", true},
		{"cdn", "", true},           // 空 = 关 B 通道
		{"cdn", "https://x", false}, // 缺尾 /
		{"cdn", "ftp://x/", false},

		{"listen", "127.0.0.1:9900", true},
		{"listen", "0.0.0.0:9900", false}, // 非回环（安全模型）
		{"listen", "127.0.0.1", false},    // 缺端口
		{"listen", "127.0.0.1:0", false},  // 非法端口

		{"rules_url", "https://mirror.example/rules.json", true},
		{"rules_url", "", true}, // 空 = 官方源
		{"rules_url", "notaurl", false},

		{"rules_interval", "6h", true},
		{"rules_interval", "30m", true},
		{"rules_interval", "0s", false}, // 必须为正
		{"rules_interval", "abc", false},

		{"doctor_interval", "1h", true},
		{"doctor_interval", "0s", true}, // 0 = 关
		{"doctor_interval", "-5m", false},

		{"bogus", "x", false}, // 白名单外
		{"", "x", false},
	}
	for _, c := range cases {
		err := configValidate(c.key, c.val)
		if (err == nil) != c.ok {
			t.Errorf("%s=%q: err=%v want ok=%v", c.key, c.val, err, c.ok)
		}
	}
}
