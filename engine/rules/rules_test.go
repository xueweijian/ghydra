package rules

import "testing"

func TestMatch(t *testing.T) {
	m := New(DefaultDomains)
	cases := []struct {
		host string
		want bool
	}{
		// 精确
		{"github.com", true},
		{"GITHUB.COM", true},
		{"github.com:443", true},
		{"github.com.", true},
		{" github.com ", true},
		// 通配子域（任意深度）
		{"api.github.com", true},
		{"gist.github.com", true},
		{"a.b.github.com", true},
		{"objects.githubusercontent.com", true},
		{"avatars.githubusercontent.com:443", true},
		{"github.githubassets.com", true},
		// 域名边界：前缀相同不算子域
		{"notgithub.com", false},
		{"evilgithub.com", false},
		{"github.company.com", false},
		{"githubcom", false},
		// 通配不匹配裸域
		{"githubusercontent.com", true}, // 清单中裸域单列，应为 true
		// 非加速目标
		{"example.com", false},
		{"127.0.0.1", false},
		{"localhost", false},
		{"api.github.io", false}, // Pages 不在 M1 清单
		// 畸形输入
		{"", false},
		{".", false},
		{"*", false},
	}
	for _, c := range cases {
		if got := m.Match(c.host); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestMatchEmptyMatcher(t *testing.T) {
	m := New(nil)
	if m.Match("github.com") {
		t.Error("空清单不应命中任何域名")
	}
	if n := len(m.Domains()); n != 0 {
		t.Errorf("Domains() = %d 项, want 0", n)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github.com", "github.com"},
		{"GitHub.COM", "github.com"},
		{"github.com:443", "github.com"},
		{"github.com.", "github.com"},
		{"[::1]:443", "[::1]"},
		{"::1", "::1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewIgnoresInvalid(t *testing.T) {
	m := New([]string{"github.com", "", "  ", "*.", "*"})
	if !m.Match("github.com") {
		t.Error("合法条目应保留")
	}
	if m.Match("sub.github.com") {
		t.Error("非法通配条目 '*.' 不应生效")
	}
}

func TestDomainsSnapshot(t *testing.T) {
	m := New([]string{"b.com", "a.com"})
	d := m.Domains()
	if len(d) != 2 || d[0] != "b.com" || d[1] != "a.com" {
		t.Fatalf("Domains() = %v, want [b.com a.com]（构造顺序）", d)
	}
	d[0] = "mutated"
	if got := m.Domains()[0]; got != "b.com" {
		t.Error("Domains() 应返回快照，外部修改不应影响内部状态")
	}
}

func BenchmarkMatch(b *testing.B) {
	m := New(DefaultDomains)
	hosts := []string{"github.com", "api.github.com", "objects.githubusercontent.com", "example.com", "a.very.deep.sub.domain.github.com"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Match(hosts[i%len(hosts)])
	}
}
