//go:build integration

// 完整对象传输版 E2E：真实 Linux 文件系统（CI ubuntu）运行。
// 本地 PRoot 沙箱的跨仓对象打包有 stat 层 bug，跑会假失败——勿在沙箱验证本文件。
package gitcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestEndToEndCloneRewrite 端到端证明：insteadOf 改写真实生效于 clone——
// 假 CDN base 目录承载仓库副本，clone 源指向源目录，实际从 CDN 目录取出。
func TestEndToEndCloneRewrite(t *testing.T) {

	e := newGitExec(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.git")
	cdn := filepath.Join(dir, "cdn")

	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--bare", "-q", src)
	run("init", "--bare", "-q", cdn)
	// src 灌一个提交
	work := filepath.Join(dir, "work")
	run("init", "-q", work)
	cfg := func(repo string, args ...string) {
		t.Helper()
		full := append([]string{"-C", repo}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("git config %v: %v\n%s", args, err, out)
		}
	}
	cfg(work, "config", "user.email", "t@t")
	cfg(work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("hello"), 0o644)
	run("-C", work, "add", ".")
	run("-C", work, "commit", "-qm", "init")
	run("-C", work, "remote", "add", "origin", src)
	run("-C", work, "push", "-q", "origin", "HEAD")
	// cdn 是 src 的完整镜像（模拟"CDN 能取到内容"）
	out, err := exec.Command("git", "-C", cdn, "fetch", "-q", src, "+refs/*:refs/*").CombinedOutput()
	if err != nil {
		t.Fatalf("镜像 cdn: %v\n%s", err, out)
	}

	// insteadOf 前缀配对：value = src（裸路径），base = cdn（裸路径）。
	// clone <src> → 前缀命中 → 改写为 <cdn>（bare 仓库目录可直接 clone）。
	mustOut(t, e, "--add", "url."+cdn+".insteadof", src)
	defer func() { _, _ = e.Run("--unset-all", "url."+cdn+".insteadof") }()

	cloneDir := filepath.Join(dir, "clone")
	cmd := exec.Command("git", "clone", "-q", src, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+gitConfigPath(t))
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clone（应经 insteadOf 落到 cdn）: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(cloneDir, "f.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("clone 内容异常: %v %q", err, b)
	}
}

// gitConfigPath 返回测试进程的全局配置文件路径（与 CLIExec 同一文件）。
// GIT_CONFIG_GLOBAL 在 newGitExec 场景由测试主进程注入（见 TestMain）。
func gitConfigPath(t *testing.T) string {
	t.Helper()
	return os.Getenv("GIT_CONFIG_GLOBAL")
}
