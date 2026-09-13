package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// 设计 §7 L1-5：交换序列纯逻辑 + 执行 + 崩溃恢复。
// 布局：<dir>/ghydra(.exe) 当前 / ghydra.old 上一版（回滚凭证）/ update-staging/ 新文件。

func TestPlanSwapNormal(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "update-staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// 现有安装 + staging 新版
	old := []byte("current-binary-v1.0.0")
	if err := os.WriteFile(filepath.Join(dir, ExeName), old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, ExeName), []byte("new-binary-v1.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	ops, err := PlanSwap(dir, []string{ExeName})
	if err != nil {
		t.Fatalf("PlanSwap: %v", err)
	}
	// 期望序列：无旧 old（首次）→ [exe→exe.old, staging→exe]
	if len(ops) != 2 {
		t.Fatalf("操作数 = %d（%+v），期望 2", len(ops), ops)
	}
	if ops[0].From != ExeName || ops[0].To != OldName {
		t.Errorf("op0 期望 %s→%s，得到 %s→%s", ExeName, OldName, ops[0].From, ops[0].To)
	}
	if ops[1].From != filepath.Join("update-staging", ExeName) || ops[1].To != ExeName {
		t.Errorf("op1 期望 staging→%s，得到 %s→%s", ExeName, ops[1].From, ops[1].To)
	}

	if err := ExecuteSwap(dir, ops); err != nil {
		t.Fatalf("ExecuteSwap: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ExeName))
	if string(got) != "new-binary-v1.0.1" {
		t.Errorf("交换后内容 = %q", got)
	}
	oldGot, _ := os.ReadFile(filepath.Join(dir, OldName))
	if string(oldGot) != "current-binary-v1.0.0" {
		t.Errorf("old 凭证内容 = %q", oldGot)
	}
}

func TestPlanSwapOldResidue(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "update-staging")
	os.MkdirAll(staging, 0o755)
	// 残留的上一轮 old（第二次更新场景）→ 序列开头要先清
	os.WriteFile(filepath.Join(dir, ExeName), []byte("v2"), 0o755)
	os.WriteFile(filepath.Join(dir, OldName), []byte("v1-very-old"), 0o755)
	os.WriteFile(filepath.Join(staging, ExeName), []byte("v3"), 0o755)

	ops, err := PlanSwap(dir, []string{ExeName})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("操作数 = %d（%+v），期望 3（先清残留 old）", len(ops), ops)
	}
	if ops[0].Kind != OpRemove || ops[0].Target != OldName {
		t.Errorf("op0 应为清残留 old（remove %s），得到 %+v", OldName, ops[0])
	}
	if err := ExecuteSwap(dir, ops); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, OldName))
	if string(got) != "v2" {
		t.Errorf("交换后 old 应为 v2，得到 %q", got)
	}
}

func TestPlanSwapStagingMissing(t *testing.T) {
	dir := t.TempDir()
	// staging 缺文件 → 计划阶段即拒（不碰现有安装）
	os.WriteFile(filepath.Join(dir, ExeName), []byte("v1"), 0o755)
	if _, err := PlanSwap(dir, []string{ExeName}); err == nil {
		t.Error("staging 缺文件必须报错")
	}
	// 现有安装未被破坏
	got, _ := os.ReadFile(filepath.Join(dir, ExeName))
	if string(got) != "v1" {
		t.Error("计划失败不应触碰现有安装")
	}
}

func TestCrashRecoveryRestoreOld(t *testing.T) {
	// 崩溃点：exe 已 rename 成 old，staging 还没就位（或新版被剔走）
	// → 启动清扫逻辑必须把 old 恢复回 exe
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, OldName), []byte("survivor-v1"), 0o755)
	os.MkdirAll(filepath.Join(dir, "update-staging"), 0o755)
	os.WriteFile(filepath.Join(dir, "update-staging", "junk.tmp"), []byte("x"), 0o644)

	actions, err := RecoverFromCrash(dir, []string{ExeName})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Kind == OpRestoreOld {
			found = true
		}
	}
	if !found {
		t.Fatalf("应产生 RestoreOld 动作，得到 %+v", actions)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ExeName))
	if string(got) != "survivor-v1" {
		t.Errorf("恢复后 exe 内容 = %q，期望 survivor-v1", got)
	}
	// staging 残留被清扫
	if _, err := os.Stat(filepath.Join(dir, "update-staging", "junk.tmp")); !os.IsNotExist(err) {
		t.Error("staging 残留应被清扫")
	}
}

func TestCrashRecoveryNoopWhenHealthy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ExeName), []byte("v1"), 0o755)
	os.WriteFile(filepath.Join(dir, OldName), []byte("v0"), 0o755) // 健康状态带 old 凭证是正常的

	actions, err := RecoverFromCrash(dir, []string{ExeName})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Kind == OpRestoreOld {
			t.Errorf("健康状态不应触发恢复，得到 %+v", actions)
		}
	}
	// exe/old 均未被触碰
	got, _ := os.ReadFile(filepath.Join(dir, ExeName))
	if string(got) != "v1" {
		t.Error("健康 exe 不应被改动")
	}
}

func TestRollbackSwap(t *testing.T) {
	// 手动回滚：exe(坏新版) ↔ old(好旧版) 交换 + 坏版留档 .bad
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ExeName), []byte("bad-v2"), 0o755)
	os.WriteFile(filepath.Join(dir, OldName), []byte("good-v1"), 0o755)

	if err := RollbackSwap(dir, []string{ExeName}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ExeName))
	if string(got) != "good-v1" {
		t.Errorf("回滚后 exe = %q，期望 good-v1", got)
	}
	bad, _ := os.ReadFile(filepath.Join(dir, BadName))
	if string(bad) != "bad-v2" {
		t.Errorf("坏版应留档 %s，得到 %q", BadName, bad)
	}
}

func TestRollbackSwapWithoutOld(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ExeName), []byte("v1"), 0o755)
	if err := RollbackSwap(dir, []string{ExeName}); err == nil {
		t.Error("无 old 凭证时回滚应报错（不能凭空回滚）")
	}
}
