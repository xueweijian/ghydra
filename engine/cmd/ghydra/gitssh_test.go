package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xueweijian/ghydra/engine/gitcfg"
	"github.com/xueweijian/ghydra/engine/sshcfg"
	"github.com/xueweijian/ghydra/engine/store"
)

func execLookPath(bin string) (string, error) { return exec.LookPath(bin) }

func sshcfgEnableForTest(path, backup string) (sshcfg.Plan, error) {
	return sshcfg.Enable(path, false, false, backup)
}

func base64ForTest(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// gitCmdFlowTest 端到端走 CLI 函数：enable → 快照在库 → disable → 还原。
// 真实 git，全局配置文件经 GIT_CONFIG_GLOBAL 隔离（同 engine/gitcfg 套路）。
func TestGitCmdFlow(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git 不在 PATH")
	}
	tmp := t.TempDir()
	gcfg := filepath.Join(tmp, "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	db := filepath.Join(tmp, "test.db")

	// enable（dry-run 先行：无文件副作用）
	gitEnable([]string{"--cdn", "https://gh.example.com/t/TOK/", "--dry-run"}, db)
	if _, err := os.Stat(gcfg); err == nil {
		t.Error("dry-run 不应创建 gitconfig")
	}

	// enable 真跑
	gitEnable([]string{"--cdn", "https://gh.example.com/t/TOK/"}, db)
	kvs, err := gitcfg.ReadURLKeys(gitcfg.CLIExec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 2 {
		t.Fatalf("应写入 2 键，got %d", len(kvs))
	}

	// 快照在库
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadManagedSnapshot(storeGitKind); !ok {
		t.Fatal("快照应存在")
	}
	st.Close()

	// status 不炸（输出略）
	gitStatus([]string{"--cdn", "https://gh.example.com/t/TOK/"}, db)

	// disable 还原
	gitDisable(db)
	kvs, err = gitcfg.ReadURLKeys(gitcfg.CLIExec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 0 {
		t.Fatalf("disable 后应无 url.* 键，got %+v", kvs)
	}
	st, _ = store.Open(db)
	defer st.Close()
	if _, ok, _ := st.LoadManagedSnapshot(storeGitKind); ok {
		t.Error("disable 后快照应删除")
	}
}

func TestGitDisableWithoutSnapshot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "g"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitDisable(filepath.Join(tmp, "none.db")) // 不应 panic/exit
}

func TestSSHSnapshotRoundTripInStore(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "t.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// base64 载荷 save→load→delete（sshCmd 的快照协议层）
	payload := "SG9zdCBnaXRodWIuY29tCg=="
	if err := st.SaveManagedSnapshot(storeSSHKind, payload); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LoadManagedSnapshot(storeSSHKind)
	if err != nil || !ok || got != payload {
		t.Fatalf("round trip: %q %v %v", got, ok, err)
	}
	if err := st.DeleteManagedSnapshot(storeSSHKind); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadManagedSnapshot(storeSSHKind); ok {
		t.Error("删除后不应存在")
	}
}

func TestRestoreManagedOnOffNoDB(t *testing.T) {
	restoreManagedOnOff("") // 空路径直接返回，不 panic
}

func TestRestoreManagedOnOffWithSnapshots(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git 不在 PATH")
	}
	tmp := t.TempDir()
	gcfg := filepath.Join(tmp, "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// os.UserHomeDir()：Linux/mac 读 HOME，Windows 读 USERPROFILE——
	// 两个都设，否则 defaultSSHConfigPath() 打到真实 profile（CI 抓出）。
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	db := filepath.Join(tmp, "t.db")

	// 布置 git 快照态
	snap, err := gitcfg.Enable(gitcfg.CLIExec{}, gitcfg.Options{CDN: "https://gh.example.com/t/T/"}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(snap)
	st, _ := store.Open(db)
	st.SaveManagedSnapshot(storeGitKind, string(b))
	st.Close()

	// 布置 ssh 快照态（真文件；HOME 隔离后与 defaultSSHConfigPath 同路径）
	sshCfg := filepath.Join(tmp, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(sshCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshCfg, []byte("Host old\n    HostName x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 直接以 sshcfg 写块 + 手工放快照（模拟 enable 完成后的状态）
	if _, err := sshcfgEnableForTest(sshCfg, filepath.Join(tmp, "bak")); err != nil {
		t.Fatal(err)
	}
	bakBytes, _ := os.ReadFile(filepath.Join(tmp, "bak"))
	st, _ = store.Open(db)
	st.SaveManagedSnapshot(storeSSHKind, base64ForTest(string(bakBytes)))
	st.Close()

	// off 路径还原
	restoreManagedOnOff(db)

	restored, _ := os.ReadFile(sshCfg)
	if string(restored) != "Host old\n    HostName x\n" {
		t.Errorf("ssh 未字节还原: %q", restored)
	}
	kvs, _ := gitcfg.ReadURLKeys(gitcfg.CLIExec{})
	if len(kvs) != 0 {
		t.Errorf("git 键未还原: %+v", kvs)
	}
	if !strings.Contains(string(b), "insteadof") {
		t.Errorf("快照内容异常（sanity）: %s", b)
	}
}
