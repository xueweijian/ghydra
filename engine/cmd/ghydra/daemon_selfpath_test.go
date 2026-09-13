package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归锁（W4 CI ubuntu 实证的真 bug）：自更新交换后，os.Executable() 在
// Linux 读 /proc/self/exe——运行中的 exe 被 rename 成 .old 后该链接指向
// .old，重启 daemon 会拉起**旧版二进制**（对账永远读到旧 version）。
// 修复 = 交换前捕获 selfPath，Start/WaitVersion 全程用它。

func TestExePathForSpawnUsesCapturedPath(t *testing.T) {
	const captured = "/opt/ghydra/ghydra"
	c := daemonControlProd{dbPath: "/tmp/x.db", selfPath: captured}
	got, err := c.exePathForSpawn()
	if err != nil {
		t.Fatal(err)
	}
	if got != captured {
		t.Errorf("应使用预捕获路径 %q，得到 %q", captured, got)
	}
}

func TestExePathForSpawnFallsBackToExecutable(t *testing.T) {
	c := daemonControlProd{dbPath: "/tmp/x.db"} // selfPath 空 = 非自更新路径
	got, err := c.exePathForSpawn()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.Executable()
	if got != want {
		t.Errorf("兜底应为 os.Executable()=%q，得到 %q", want, got)
	}
}

// TestCapturedPathSurvivesRename 复现 CI 现场：把"运行中的 exe"改名成
// .old，证明预捕获字符串仍然指向新 exe（而 /proc/self/exe 语义会漂）。
func TestCapturedPathSurvivesRename(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "ghydra")
	if err := os.WriteFile(live, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	captured := live // 交换前捕获

	// 模拟交换：live → live.old，staging → live（新二进制就位）
	if err := os.Rename(live, live+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := daemonControlProd{selfPath: captured}
	got, err := c.exePathForSpawn()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-binary" {
		t.Errorf("预捕获路径应指向新二进制，读到 %q（说明起了旧版）", data)
	}
}
