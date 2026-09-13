package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 设计 §7 L1-6：update.json 状态机（pending→confirmed / boot_attempts /
// >2 自动回滚 / bad_version 记录与跳过 / daemon_was_running 分支）。

func TestStateRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	s := &State{
		PendingVersion:   "1.0.1",
		DaemonWasRunning: true,
		AppliedAt:        time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	got, exists, err := LoadState(path)
	if err != nil || !exists {
		t.Fatalf("LoadState: %v exists=%v", err, exists)
	}
	if got.PendingVersion != "1.0.1" || !got.DaemonWasRunning || got.Confirmed {
		t.Errorf("roundtrip 丢失: %+v", got)
	}
	if !got.AppliedAt.Equal(s.AppliedAt) {
		t.Errorf("AppliedAt = %v，期望 %v", got.AppliedAt, s.AppliedAt)
	}
}

func TestStateLoadMissing(t *testing.T) {
	_, exists, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || exists {
		t.Errorf("缺失文件应 (nil,false,nil)，得到 %v %v", err, exists)
	}
}

func TestStateLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	os.WriteFile(path, []byte("{broken"), 0o644)
	if _, _, err := LoadState(path); err == nil {
		t.Error("坏 json 必须报错（宁拒勿猜）")
	}
}

func TestStateSaveAtomic(t *testing.T) {
	// 写入必须是原子替换：写中途崩溃不留半文件（tmp+rename，复用 W2 纪律）
	path := filepath.Join(t.TempDir(), "update.json")
	os.WriteFile(path, []byte(`{"pending_version":"0.9.0","confirmed":false}`), 0o644)
	if err := SaveState(path, &State{PendingVersion: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadState(path)
	if err != nil || got.PendingVersion != "1.0.0" {
		t.Fatalf("覆盖写后 = %+v %v", got, err)
	}
	// 目录里不残留 tmp
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "update.json" {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

func TestStateSelfCheckGate(t *testing.T) {
	// 自检门：pending 非空且未确认
	if (&State{}).ShouldSelfCheck() {
		t.Error("空状态不应自检")
	}
	if (&State{PendingVersion: "1.0.1"}).ShouldSelfCheck() {
		// pending 且 confirmed=false → 应自检（true）
		t.Log("pending 未确认 → 自检（正确路径）")
	} else {
		t.Error("pending 未确认应触发自检")
	}
	if (&State{PendingVersion: "1.0.1", Confirmed: true}).ShouldSelfCheck() {
		t.Error("已确认不应自检")
	}
}

func TestStateBootFailuresRollback(t *testing.T) {
	s := &State{PendingVersion: "1.0.1"}

	if s.RecordBootFailure(); s.BootAttempts != 1 {
		t.Fatalf("第一次失败 attempts = %d", s.BootAttempts)
	}
	if s.RecordBootFailure() {
		t.Error("第二次失败（attempts=2）不应触发回滚（阈值 >2）")
	}
	if !s.RecordBootFailure() {
		t.Error("第三次失败（attempts=3）应触发自动回滚")
	}
	if s.BootAttempts != 3 {
		t.Errorf("attempts = %d，期望 3", s.BootAttempts)
	}

	// 回滚落地：pending 清空 + bad_version 记录
	s.MarkRolledBack("1.0.1")
	if s.PendingVersion != "" {
		t.Error("回滚后 pending 应清空")
	}
	if s.BadVersion != "1.0.1" {
		t.Errorf("BadVersion = %q", s.BadVersion)
	}
	if !s.IsBad("1.0.1") {
		t.Error("IsBad(1.0.1) 应为 true")
	}
	if s.IsBad("1.0.2") {
		t.Error("IsBad(1.0.2) 应为 false")
	}
}

func TestStateConfirm(t *testing.T) {
	s := &State{PendingVersion: "1.0.1", BootAttempts: 1}
	s.Confirm()
	if !s.Confirmed {
		t.Error("Confirm 后应为已确认")
	}
	if s.ShouldSelfCheck() {
		t.Error("确认后不应再自检")
	}
	// 确认后 boot_attempts 等中间态保留与否不敏感，但 BadVersion 必须保留语义
	s2 := &State{BadVersion: "1.0.1"}
	s2.Confirm()
	if !s2.IsBad("1.0.1") {
		t.Error("确认动作不应清除 bad_version 记忆")
	}
}

func TestStateBadVersionMemory(t *testing.T) {
	// bad_version 状态侧语义（选择协同见 release_test.go）。
	s := &State{PendingVersion: "1.0.1"}
	s.MarkRolledBack("1.0.1")
	if !s.IsBad("1.0.1") || s.IsBad("1.0.2") {
		t.Error("IsBad 语义错误")
	}
	s2 := &State{BadVersion: "1.0.1"}
	s2.Confirm()
	if !s2.IsBad("1.0.1") {
		t.Error("确认动作不应清除 bad_version 记忆")
	}
}
