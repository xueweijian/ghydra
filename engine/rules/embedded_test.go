package rules

import "testing"

// TestEmbeddedFloor 内嵌地板必须始终能过运行时校验——它是拔网线冷启动
// 的最后防线（PRD F6 验收：断网可启动可工作）。如果它坏了，解析器就是坏了。
func TestEmbeddedFloor(t *testing.T) {
	rf, err := ParseRules(EmbeddedJSON)
	if err != nil {
		t.Fatalf("embedded floor rejected by own parser: %v", err)
	}
	if rf.Version != 1 {
		t.Fatalf("embedded version = %d, want 1", rf.Version)
	}
	// 与现有 DefaultDomains 对齐（消费方迁移前的行为等价基线）。
	m := New(rf.Domains)
	for _, d := range DefaultDomains {
		if !m.Match(d) {
			t.Fatalf("embedded loses domain %q from DefaultDomains", d)
		}
	}
	if _, _, err := ActiveKey(); err != nil {
		t.Fatalf("ActiveKey: %v", err)
	}
}
