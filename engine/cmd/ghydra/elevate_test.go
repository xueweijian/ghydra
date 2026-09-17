// elevate_test.go —— v1.0.3 PR3 提权决策表（L1，全依赖注入）。
// Windows 真机语义（v1.0.2 实证）：Program Files 下普通权限 MkdirAll
// 必拒（ErrDirNotWritable）→ UAC runas 重跑 `ghydra update --elevated`；
// 提权后仍不可写（异态）→ 明确报错，绝不无限弹 UAC。
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestElevateDecisionTable(t *testing.T) {
	cases := []struct {
		name     string
		writable bool
		elevated bool // GHYDRA_ELEVATED 标记（--elevated 旗标传入）
		runasErr error
		want     elevateAction
		wantErr  string // 非空 = 期望 fail 且错误文案含此关键词
	}{
		{"可写直接继续", true, false, nil, elevateProceed, ""},
		{"可写忽略提权标记", true, true, nil, elevateProceed, ""},
		{"不可写+未提权+UAC同意→交棒", false, false, nil, elevateRelaunched, ""},
		{"不可写+已提权→报错防循环", false, true, nil, elevateFail, "管理员"},
		{"不可写+UAC拒绝→指引", false, false, errors.New("SE_ERR_ACCESSDENIED"), elevateFail, "管理员"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runasCalled := false
			d := elevateDeps{
				writable: func(string) bool { return tc.writable },
				elevated: func() bool { return tc.elevated },
				runas: func(exe, args string) error {
					runasCalled = true
					if !strings.Contains(args, "update") || !strings.Contains(args, "--elevated") {
						t.Errorf("runas args 应含 update --elevated；got %q", args)
					}
					if !strings.HasSuffix(exe, "ghydra.exe") && !strings.HasSuffix(exe, "ghydra") {
						t.Errorf("runas exe 应是自身；got %q", exe)
					}
					return tc.runasErr
				},
				self: "C:\\x\\ghydra.exe",
			}
			got, err := decideElevate(d)
			if got != tc.want {
				t.Errorf("action = %v, want %v（runasCalled=%v err=%v）", got, tc.want, runasCalled, err)
			}
			switch tc.want {
			case elevateProceed:
				if runasCalled {
					t.Error("可写时不得触发 runas")
				}
			case elevateRelaunched:
				if !runasCalled {
					t.Error("应已触发 runas")
				}
				if err != nil {
					t.Errorf("交棒路径不应带错误: %v", err)
				}
			case elevateFail:
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("应报含 %q 的错误；got %v", tc.wantErr, err)
				}
			}
		})
	}
}

// probeWritable 实测：临时目录可写；父路径为文件 → 不可写（ENOTDIR 跨平台）。
func TestProbeWritable(t *testing.T) {
	if !probeWritable(t.TempDir()) {
		t.Error("临时目录应可写")
	}
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if probeWritable(filepath.Join(blocker, "sub")) {
		t.Error("文件路径下应不可写")
	}
}
