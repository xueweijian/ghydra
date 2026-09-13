package sshcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnablePrependFresh(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "Host work\n    HostName git.corp\n")
	plan, err := Enable(p, false, false, filepath.Join(dir, "bak"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "prepend" {
		t.Errorf("action=%s", plan.Action)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.HasPrefix(s, MarkerBegin) {
		t.Errorf("块不在顶部:\n%s", s)
	}
	if !strings.Contains(s, "Host github.com\n") || !strings.Contains(s, "Port 443") {
		t.Errorf("块内容缺失:\n%s", s)
	}
	// 既有内容保留在块后
	if !strings.Contains(s, "Host work\n    HostName git.corp") {
		t.Errorf("原内容丢失:\n%s", s)
	}
	// 快照字节级
	orig, _ := os.ReadFile(filepath.Join(dir, "bak"))
	if string(orig) != "Host work\n    HostName git.corp\n" {
		t.Errorf("快照不是原字节: %q", orig)
	}
}

func TestEnableIdempotentReplace(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "")
	if _, err := Enable(p, false, false, filepath.Join(dir, "bak")); err != nil {
		t.Fatal(err)
	}
	plan, err := Enable(p, false, false, filepath.Join(dir, "bak"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "replace" {
		t.Errorf("二次 enable 应 replace，got %s", plan.Action)
	}
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), MarkerBegin); got != 1 {
		t.Errorf("块重复 %d 次", got)
	}
}

func TestEnableConflictRefused(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "Host github.com\n    User octocat\n")
	plan, err := Enable(p, false, false, filepath.Join(dir, "bak"))
	if err == nil {
		t.Fatal("冲突应拒绝")
	}
	if !plan.Conflict {
		t.Errorf("plan.Conflict 应为 true")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "Host github.com\n    User octocat\n" {
		t.Error("拒绝路径不得改文件")
	}
}

func TestEnableAliasMode(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "Host github.com\n    User octocat\n")
	plan, err := Enable(p, true, false, filepath.Join(dir, "bak"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "append" {
		t.Errorf("alias 应 append，got %s", plan.Action)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "Host "+AliasHost) {
		t.Errorf("alias 块缺失:\n%s", s)
	}
	if !strings.HasPrefix(s, "Host github.com\n    User octocat") {
		t.Errorf("既有段被移动/修改:\n%s", s)
	}
	en, al, err := Effective(p)
	if err != nil || !en || !al {
		t.Errorf("Effective=%v/%v err=%v", en, al, err)
	}
}

func TestDryRunNoWrite(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "Host x\n")
	before, _ := os.ReadFile(p)
	plan, err := Enable(p, false, true, filepath.Join(dir, "bak"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "prepend" || plan.Backup != "" {
		t.Errorf("dry-run plan 异常: %+v", plan)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("dry-run 改了文件")
	}
	if _, err := os.Stat(filepath.Join(dir, "bak")); !os.IsNotExist(err) {
		t.Error("dry-run 写了快照")
	}
}

func TestDisableRestoreBytes(t *testing.T) {
	dir := t.TempDir()
	orig := "Host github.com\n    User octocat\n"
	p := write(t, dir, "config", orig)
	bak := filepath.Join(dir, "bak")
	if _, err := Enable(p, true, false, bak); err != nil {
		t.Fatal(err)
	}
	plan, err := Disable(p, bak, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "restore" {
		t.Errorf("应走快照还原，got %s", plan.Action)
	}
	b, _ := os.ReadFile(p)
	if string(b) != orig {
		t.Errorf("还原不字节级: %q", b)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Error("还原后快照应删除")
	}
}

func TestDisableRemoveByMarker(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", Block("github.com")+"\n\nHost x\n")
	plan, err := Disable(p, filepath.Join(dir, "no-bak"), false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "remove" {
		t.Errorf("action=%s", plan.Action)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), MarkerBegin) || !strings.Contains(string(b), "Host x") {
		t.Errorf("marker 移除异常: %q", b)
	}
}

func TestDisableNoop(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", "Host x\n")
	plan, err := Disable(p, filepath.Join(dir, "no-bak"), false)
	if err != nil || plan.Action != "noop" {
		t.Errorf("action=%s err=%v", plan.Action, err)
	}
}

func TestHasGithubHostVariants(t *testing.T) {
	yes := []string{
		"Host github.com\n",
		"host GITHUB.COM\n",
		"Host work github.com\n",
		"  Host\tgithub.com\n",
		"Host ssh.github.com\nHost github.com\n",
	}
	no := []string{
		"Host *.github.com\n",       // 通配不算显式冲突
		"Host github.example.com\n", // 子域不算
		"Host github.com.evil.cn\n", // 前缀拼接不算
		"#Host github.com\n",        // 注释
		"HostName github.com\n",     // HostName 不算 Host
	}
	for _, c := range yes {
		if !HasGithubHost(c) {
			t.Errorf("应判定冲突: %q", c)
		}
	}
	for _, c := range no {
		if HasGithubHost(c) {
			t.Errorf("不应判定冲突: %q", c)
		}
	}
}

func TestCorruptBlock(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "config", MarkerBegin+"\nHost x\n")
	if _, err := Enable(p, false, false, filepath.Join(dir, "bak")); err == nil {
		t.Fatal("残缺块应报错")
	}
	if _, err := Disable(p, filepath.Join(dir, "nb"), false); err == nil {
		t.Fatal("残缺块 disable 应报错")
	}
}
