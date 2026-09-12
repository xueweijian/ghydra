package rules

import (
	"strings"
	"testing"
)

func TestPACContainsAllDomains(t *testing.T) {
	m := New(DefaultDomains)
	pac := m.PAC("127.0.0.1:9801")

	// 每个加速域名（含通配形式）都应出现在 shExpMatch 条件里
	for _, d := range m.Domains() {
		if !strings.Contains(pac, `shExpMatch(host, "`+d+`")`) {
			t.Errorf("PAC 缺少域名 %q", d)
		}
	}
	if !strings.Contains(pac, `return "PROXY 127.0.0.1:9801";`) {
		t.Error("PAC 应返回代理地址")
	}
	if !strings.HasSuffix(strings.TrimSpace(pac), `return "DIRECT";\n}`) &&
		!strings.Contains(pac, `return "DIRECT";`) {
		t.Error("PAC 兜底应 DIRECT")
	}
}

func TestPACLoopbackDirect(t *testing.T) {
	// 本机流量不绕代理
	m := New([]string{"github.com", "*.github.com"})
	pac := m.PAC("127.0.0.1:9801")
	if !strings.Contains(pac, "127.0.0.0") {
		t.Error("PAC 应含 loopback DIRECT 判定")
	}
}

func TestPACEmptyDomains(t *testing.T) {
	m := New(nil)
	pac := m.PAC("127.0.0.1:9801")
	if strings.Contains(pac, "shExpMatch") {
		t.Error("空清单不应有域名条件")
	}
}
