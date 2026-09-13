package selfupdate

import "testing"

// 设计 §7 L1-1：semver 解析 + 比较表驱动（先写测试后写实现）。
// v1.0 只需主轴：三段 + prerelease 优先级（1.0.0-beta < 1.0.0）；build metadata 忽略。

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		major int
		minor int
		patch int
		pre   string
	}{
		{"1.0.1", true, 1, 0, 1, ""},
		{"v1.0.1", true, 1, 0, 1, ""}, // tag 前缀 v 容忍
		{"v1.0.1-beta", true, 1, 0, 1, "beta"},
		{"v1.0.1-beta.1", true, 1, 0, 1, "beta.1"},
		{"v1.2.10", true, 1, 2, 10, ""},       // 双位数
		{"v10.20.30", true, 10, 20, 30, ""},   // 多段双位数
		{"v1.0.1+build.5", true, 1, 0, 1, ""}, // build metadata 解析忽略
		{"v1.0.1-rc.1+build", true, 1, 0, 1, "rc.1"},
		{"", false, 0, 0, 0, ""},
		{"1.0", false, 0, 0, 0, ""},     // 两段不行
		{"1.0.0.1", false, 0, 0, 0, ""}, // 四段不行
		{"vx.y.z", false, 0, 0, 0, ""},
		{"1.a.0", false, 0, 0, 0, ""},
		{"01.2.3", false, 0, 0, 0, ""}, // 前导零拒绝（semver 规范）
		{"1.0.-1", false, 0, 0, 0, ""},
		{"-1.0.0", false, 0, 0, 0, ""},
		{"999999.0.0", true, 999999, 0, 0, ""}, // 大数合法
		{"1000000.0.0", false, 0, 0, 0, ""},    // > 6 位拒绝（防荒谬值）
	}
	for _, c := range cases {
		v, err := ParseVersion(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("ParseVersion(%q) 意外报错: %v", c.in, err)
				continue
			}
			if v.Major != c.major || v.Minor != c.minor || v.Patch != c.patch || v.Pre != c.pre {
				t.Errorf("ParseVersion(%q) = %d.%d.%d-%q，期望 %d.%d.%d-%q",
					c.in, v.Major, v.Minor, v.Patch, v.Pre, c.major, c.minor, c.patch, c.pre)
			}
		} else {
			if err == nil {
				t.Errorf("ParseVersion(%q) 应报错，得到 %+v", c.in, v)
			}
		}
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1 a<b, 0 等值, 1 a>b
	}{
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.1.0", -1},
		{"1.9.0", "1.10.0", -1}, // 数值比较不是字符串
		{"2.0.0", "1.99.99", 1},
		{"1.0.0-beta", "1.0.0", -1}, // prerelease < 正式
		{"1.0.0", "1.0.0-beta", 1},
		{"1.0.0-beta", "1.0.0-rc", -1}, // prerelease 字母序
		{"1.0.0-beta.1", "1.0.0-beta.2", -1},
		{"1.0.0-beta", "1.0.0-beta.1", -1}, // 短 < 长（点分多段大）
		{"1.0.0-beta.2", "1.0.0-beta.10", -1},
		{"1.0.0+build.1", "1.0.0+build.2", 0}, // build metadata 不参与比较
		{"1.0.0-rc.1", "1.0.0-beta.99", 1},
	}
	for _, c := range cases {
		va, err1 := ParseVersion(c.a)
		vb, err2 := ParseVersion(c.b)
		if err1 != nil || err2 != nil {
			t.Fatalf("Parse(%q/%q) 报错: %v %v", c.a, c.b, err1, err2)
		}
		got := va.Compare(vb)
		if got != c.want {
			t.Errorf("Compare(%s, %s) = %d，期望 %d", c.a, c.b, got, c.want)
		}
		if c.want == 0 {
			if back := vb.Compare(va); back != 0 {
				t.Errorf("Compare(%s, %s) 应对称为 0，得到 %d", c.b, c.a, back)
			}
		}
	}
}

func TestVersionString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.0.1", "1.0.1"},
		{"v1.0.1-beta.1", "1.0.1-beta.1"},
	}
	for _, c := range cases {
		v, err := ParseVersion(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("String(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
