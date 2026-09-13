package gitcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitExec 隔离环境真 git：GIT_CONFIG_GLOBAL 指向临时文件，
// GIT_CONFIG_NOSYSTEM 排除机器配置。CI（git ≥2.32）与本地一致。
func newGitExec(t *testing.T) Exec {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git 不在 PATH，跳过（CI ubuntu/windows 自带）")
	}
	return CLIExec{Bin: git}
}

func mustOut(t *testing.T, e Exec, args ...string) string {
	t.Helper()
	out, err := e.Run(args...)
	if err != nil {
		t.Fatalf("git config %v: %v", args, err)
	}
	return out
}

func listKeys(t *testing.T, e Exec) []KV {
	t.Helper()
	kvs, err := ReadURLKeys(e)
	if err != nil {
		t.Fatalf("ReadURLKeys: %v", err)
	}
	return kvs
}

func opts(pushB, ssh bool) Options {
	return Options{CDN: "https://gh.example.com/t/TOK/", PushViaB: pushB, SSHRewrite: ssh}
}

func TestDesiredPairSemantics(t *testing.T) {
	keys, err := Desired(opts(false, false))
	if err != nil {
		t.Fatal(err)
	}
	want := []Key{
		{Name: "url.https://gh.example.com/t/TOK/https://github.com/.insteadof", Value: "https://github.com/"},
		{Name: "url.https://github.com/.pushinsteadof", Value: "https://github.com/"},
	}
	if len(keys) != 2 {
		t.Fatalf("键数 %d ≠ 2: %+v", len(keys), keys)
	}
	for i, k := range keys {
		if k.Name != want[i].Name || k.Value != want[i].Value {
			t.Errorf("keys[%d] = %+v, want %+v", i, k, want[i])
		}
	}
}

func TestDesiredPushViaBNoPushInsteadOf(t *testing.T) {
	keys, err := Desired(opts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(k.Name, "pushinsteadof") {
			t.Errorf("PushViaB 不应产生 pushInsteadOf: %+v", k)
		}
	}
	if len(keys) != 1 {
		t.Fatalf("键数 %d ≠ 1", len(keys))
	}
}

func TestDesiredSSHRewrite(t *testing.T) {
	keys, err := Desired(opts(false, true))
	if err != nil {
		t.Fatal(err)
	}
	foundSSH := false
	for _, k := range keys {
		if k.Value == srcSSH && strings.HasSuffix(k.Name, ".insteadof") {
			foundSSH = true
			if !strings.HasPrefix(k.Name, "url.https://gh.example.com/t/TOK/git@github.com:.insteadof") {
				t.Errorf("SSH insteadOf 键名异常: %s", k.Name)
			}
		}
	}
	if !foundSSH {
		t.Error("SSHRewrite 未产生 git@github.com: insteadOf")
	}
}

func TestDesiredBadCDN(t *testing.T) {
	if _, err := Desired(Options{CDN: ""}); err == nil {
		t.Error("空 CDN 应报错")
	}
	if _, err := Desired(Options{CDN: "ftp://x/"}); err == nil {
		t.Error("非 http(s) CDN 应报错")
	}
}

func TestEnableDisableRoundTrip(t *testing.T) {
	e := newGitExec(t)
	// 用户既有键（公司 gitlab），enable/disable 全程不得触碰
	mustOut(t, e, "--add", "url.https://gitlab.corp/.insteadOf", "https://gitlab.internal/")
	defer func() {
		_, _ = e.Run("--unset-all", "url.https://gitlab.corp/.insteadOf")
	}()

	snap, err := Enable(e, opts(false, false), false)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	after := listKeys(t, e)
	names := map[string]int{}
	for _, k := range after {
		names[k.Name]++
	}
	if names["url.https://gh.example.com/t/TOK/https://github.com/.insteadof"] != 1 {
		t.Errorf("insteadOf 未写入: %+v", after)
	}
	if names["url.https://github.com/.pushinsteadof"] != 1 {
		t.Errorf("pushInsteadOf 未写入: %+v", after)
	}
	if names["url.https://gitlab.corp/.insteadof"] != 1 {
		t.Errorf("用户 gitlab 键被破坏: %+v", after)
	}
	if len(snap.Written) != 2 || len(snap.Prior) != 0 {
		t.Errorf("快照异常: written=%d prior=%d", len(snap.Written), len(snap.Prior))
	}

	// 幂等：重复 enable 不新增键
	if _, err := Enable(e, opts(false, false), false); err != nil {
		t.Fatalf("二次 Enable: %v", err)
	}
	if got := len(listKeys(t, e)); got != 3 {
		t.Errorf("幂等失败：url 键数 %d ≠ 3", got)
	}

	if err := Disable(e, snap); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	rest := listKeys(t, e)
	if len(rest) != 1 || rest[0].Name != "url.https://gitlab.corp/.insteadof" {
		t.Errorf("Disable 后残留/丢失: %+v", rest)
	}
}

func TestConflictForeignCDN(t *testing.T) {
	e := newGitExec(t)
	// 他人（旧 CDN）改写 github → 冲突拒绝
	mustOut(t, e, "--add", "url.https://gh.old.example/.insteadOf", "https://github.com/")
	defer func() { _, _ = e.Run("--unset-all", "url.https://gh.old.example/.insteadOf") }()

	if _, err := Enable(e, opts(false, false), false); err == nil {
		t.Fatal("外键改写 github 应拒绝")
	} else if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误信息应含 冲突: %v", err)
	}
	// 拒绝路径零写入
	if got := len(listKeys(t, e)); got != 1 {
		t.Errorf("拒绝路径写入了键: %d", got)
	}
}

func TestForceOverwritesAndRestores(t *testing.T) {
	e := newGitExec(t)
	mustOut(t, e, "--add", "url.https://gh.old.example/.insteadOf", "https://github.com/")
	defer func() { _, _ = e.Run("--unset-all", "url.https://gh.old.example/.insteadOf") }()

	snap, err := Enable(e, opts(false, false), true)
	if err != nil {
		t.Fatalf("force Enable: %v", err)
	}
	if snap.Prior["url.https://gh.old.example/.insteadof"] == nil {
		t.Errorf("外键原值未入快照: %+v", snap.Prior)
	}
	// force 后外键被清除、我们的键在
	if got := len(listKeys(t, e)); got != 2 {
		t.Errorf("force 后键数 %d ≠ 2", got)
	}
	if err := Disable(e, snap); err != nil {
		t.Fatal(err)
	}
	rest := listKeys(t, e)
	if len(rest) != 1 || rest[0].Value != "https://github.com/" {
		t.Errorf("Disable 未还原外键: %+v", rest)
	}
}

func TestSameNameConflictForce(t *testing.T) {
	e := newGitExec(t)
	// 同名键（同 base 不同值）：用户手写了我们的键形态但值不同
	name := "url.https://gh.example.com/t/TOK/https://github.com/.insteadof"
	mustOut(t, e, "--add", name, "https://other.example/")
	defer func() { _, _ = e.Run("--unset-all", name) }()

	if _, err := Enable(e, opts(false, false), false); err == nil {
		t.Fatal("同名异值应拒绝")
	}
	snap, err := Enable(e, opts(false, false), true)
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	if snap.Prior[name][0] != "https://other.example/" {
		t.Errorf("同名原值未快照: %+v", snap.Prior)
	}
	keys := listKeys(t, e)
	if len(keys) != 2 || keys[0].Value != "https://github.com/" && keys[1].Value != "https://github.com/" {
		t.Errorf("force 后目标值未生效: %+v", keys)
	}
	if err := Disable(e, snap); err != nil {
		t.Fatal(err)
	}
	rest := listKeys(t, e)
	if len(rest) != 1 || rest[0].Value != "https://other.example/" {
		t.Errorf("Disable 未还原同名原值: %+v", rest)
	}
}

func TestRewriteDemo(t *testing.T) {
	o := opts(false, false)
	if g, ok := RewriteDemo(o, "https://github.com/cli/cli.git"); !ok || !strings.HasPrefix(g, "https://gh.example.com/t/TOK/https://github.com/cli/cli.git") {
		t.Errorf("https 演示: %q %v", g, ok)
	}
	if _, ok := RewriteDemo(o, "git@github.com:cli/cli.git"); ok {
		t.Error("未开 SSHRewrite 不应命中 ssh URL")
	}
	o2 := opts(false, true)
	if g, ok := RewriteDemo(o2, "git@github.com:cli/cli.git"); !ok || !strings.Contains(g, "git@github.com:cli/cli.git") {
		t.Errorf("ssh 演示: %q %v", g, ok)
	}
}

// TestEndToEndLSRemoteRewrite 端到端证明 insteadOf 真实生效于传输层：
// cdn 仓库打 marker ref，clone 源指向 src；ls-remote <src>/repo.git 经
// 改写落到 cdn → 输出含 cdn-marker。零对象传输（PRoot 沙箱的跨仓
// 对象打包有 stat 层 bug，本地/CI 需同绿，故不走 push/clone 全量）。
func TestEndToEndLSRemoteRewrite(t *testing.T) {
	e := newGitExec(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.git")
	cdn := filepath.Join(dir, "cdn.git")
	for _, d := range []string{src, cdn} {
		out, err := exec.Command("git", "init", "--bare", "-q", d).CombinedOutput()
		if err != nil {
			t.Fatalf("init %s: %v\n%s", d, err, out)
		}
	}
	// cdn 打 marker ref（ref 文件直写，无需对象）
	mref := filepath.Join(cdn, "refs", "heads", "cdn-marker")
	if err := os.MkdirAll(filepath.Dir(mref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mref, []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustOut(t, e, "--add", "url."+cdn+".insteadof", src)
	defer func() { _, _ = e.Run("--unset-all", "url."+cdn+".insteadof") }()

	out, err := exec.Command("git", "ls-remote", src).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refs/heads/cdn-marker") {
		t.Errorf("ls-remote 未改写到 cdn（应见 cdn-marker ref）: %s", out)
	}
}

// TestEndToEndCloneRewrite 完整对象传输版：clone 实际经 insteadOf 落到
// 假 CDN base。integration 标签 CI 专跑（真实 Linux 文件系统）；本地
// PRoot 沙箱的对象跨仓传输有 stat 层 bug（unpack-objects 后 stat 不到
// 刚写的对象），沙箱跑会假失败。
func TestMain(m *testing.M) {
	// 所有用例共享一个隔离 gitconfig；跨用例通过每个用例自行清理
	tmp := filepath.Join(os.TempDir(), "gitcfg-test-gitconfig")
	os.Setenv("GIT_CONFIG_GLOBAL", tmp)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.Remove(tmp)
	os.Exit(code)
}
