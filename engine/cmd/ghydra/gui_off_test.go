//go:build !gui

package main

import (
	"strings"
	"testing"
)

// D1：默认构建（无 gui tag）零 wails 依赖。gui 子命令必须给出可行动的
// 引导（构建命令原文可复制），而不是静默失败或裸报错。
func TestGuiMainWithoutGuiTagGuides(t *testing.T) {
	var sb strings.Builder
	code := guiMainTo(&sb)
	if code != 2 {
		t.Fatalf("退出码 = %d, 要 2（用法错误类）", code)
	}
	out := sb.String()
	for _, want := range []string{"-tags gui", "go build"} {
		if !strings.Contains(out, want) {
			t.Errorf("引导输出缺 %q；got:\n%s", want, out)
		}
	}
}
