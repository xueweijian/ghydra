//go:build windows && integration

// ghydra on/off 端到端集成测试（W4.5）。
//
// 在 GitHub Actions windows runner 上跑真实二进制的完整生命周期：
//   - on:  快照原值 → 拉起 serve → 探活 → PAC 接管注册表 → /pac /status 可用
//   - off: 恢复原值 → 删快照 → 停 serve → 清 serve.json
//   - 崩溃对账: on → taskkill /F（kill -9 语义，退出 hook 不执行）
//     → off 触发 ensureReconcile → 恢复原值 + 清理残留
//
// 隔离：子进程 USERPROFILE/HOME 指向 t.TempDir()（serve.json/DB 不污染
// runner 真实 home）；注册表原值捕获并在 Cleanup 恢复。
//
// 运行：go test -tags=integration ./engine/cmd/ghydra/ -run Integration -v
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/xueweijian/ghydra/engine/store"
)

const internetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
const testPort = 9801

type rawState struct {
	autoConfigURL string
	autoConfigOK  bool
	proxyServer   string
	proxyServerOK bool
	proxyOverride string
	overrideOK    bool
	proxyEnable   uint64
	autoDetect    uint64
	autoDetectOK  bool
}

func rawCurrent() rawState {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.QUERY_VALUE)
	if err != nil {
		panic(err)
	}
	defer k.Close()
	var r rawState
	var errv error
	r.autoConfigURL, _, errv = k.GetStringValue("AutoConfigURL")
	r.autoConfigOK = errv == nil
	r.proxyServer, _, errv = k.GetStringValue("ProxyServer")
	r.proxyServerOK = errv == nil
	r.proxyOverride, _, errv = k.GetStringValue("ProxyOverride")
	r.overrideOK = errv == nil
	r.proxyEnable, _, _ = k.GetIntegerValue("ProxyEnable")
	r.autoDetect, _, errv = k.GetIntegerValue("AutoDetect")
	r.autoDetectOK = errv == nil
	return r
}

func (a rawState) equal(b rawState) bool {
	// AutoDetect 只比 bit0 且缺失视为 0：WinINet 刷新可能归一化该值
	//（DWORD 是 blob 的 UI 镜像，GHydra 不管理它）
	ad := func(r rawState) uint64 {
		if !r.autoDetectOK {
			return 0
		}
		return r.autoDetect & 1
	}
	return a.autoConfigURL == b.autoConfigURL && a.autoConfigOK == b.autoConfigOK &&
		a.proxyServer == b.proxyServer && a.proxyServerOK == b.proxyServerOK &&
		a.proxyOverride == b.proxyOverride && a.overrideOK == b.overrideOK &&
		a.proxyEnable == b.proxyEnable && ad(a) == ad(b)
}

func restoreRaw(t *testing.T, orig rawState) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettings, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	setOrDel := func(name, val string, ok bool) {
		if ok {
			k.SetStringValue(name, val)
		} else {
			k.DeleteValue(name)
		}
	}
	setOrDel("AutoConfigURL", orig.autoConfigURL, orig.autoConfigOK)
	setOrDel("ProxyServer", orig.proxyServer, orig.proxyServerOK)
	setOrDel("ProxyOverride", orig.proxyOverride, orig.overrideOK)
	k.SetDWordValue("ProxyEnable", uint32(orig.proxyEnable))
	if orig.autoDetectOK {
		k.SetDWordValue("AutoDetect", uint32(orig.autoDetect))
	} else {
		k.DeleteValue("AutoDetect")
	}
}

// buildGhydra 编译被测二进制（测试进程自身是 test binary，不是 ghydra）。
func buildGhydra(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "ghydra.exe")
	out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("编译 ghydra: %v\n%s", err, out)
	}
	return exe
}

// isolatedEnv 子进程环境：USERPROFILE/HOME 指向临时目录（serve.json
// 与默认 DB 不污染 runner home）。Windows 环境块重复键行为未定义，
// 先滤掉原有键再追加。
func isolatedEnv(t *testing.T) (env []string, home string) {
	t.Helper()
	home = t.TempDir()
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if k == "USERPROFILE" || k == "HOME" {
			continue
		}
		env = append(env, e)
	}
	return append(env, "USERPROFILE="+home, "HOME="+home), home
}

func runGhydra(t *testing.T, exe string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ghydra %s 失败: %v\n--- 输出 ---\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func portAlive(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（%v）", what, timeout)
}

// TestIntegrationOnOffLifecycle on → 验证接管态 → off → 验证完全恢复。
func TestIntegrationOnOffLifecycle(t *testing.T) {
	exe := buildGhydra(t)
	env, home := isolatedEnv(t)
	orig := rawCurrent()
	t.Cleanup(func() {
		restoreRaw(t, orig)
		// 兜底清理：万一断言失败中途退出，确保 serve 停止
		if d := readServeJSON(t, home); d != nil {
			exec.Command("taskkill", "/PID", fmt.Sprint(d.PID), "/T", "/F").Run()
		}
	})

	db := filepath.Join(home, "t.db")
	runGhydra(t, exe, env, "on", "--db", db, "--port", fmt.Sprint(testPort))

	// 接管态：注册表 PAC + 端口活 + /pac /status 可用
	wantPAC := fmt.Sprintf("http://127.0.0.1:%d/pac", testPort)
	waitFor(t, "注册表 AutoConfigURL", 5*time.Second, func() bool {
		return rawCurrent().autoConfigURL == wantPAC
	})
	r := rawCurrent()
	if r.proxyEnable != 0 {
		t.Errorf("接管态应关手动代理: ProxyEnable=%d", r.proxyEnable)
	}
	if d := readServeJSON(t, home); d == nil || d.Port != testPort {
		t.Fatalf("serve.json 缺失或端口错: %+v", d)
	}
	if !portAlive(testPort) {
		t.Fatal("serve 端口不可达")
	}
	for _, path := range []string{"/pac", "/status"} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", testPort, path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 快照存在（off 的恢复依据）
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadSnapshotJSON(); !ok {
		st.Close()
		t.Fatal("on 后应存在快照")
	}
	st.Close()

	runGhydra(t, exe, env, "off", "--db", db)

	// 恢复态：注册表回到原值、端口死、serve.json 删、快照删
	waitFor(t, "注册表恢复", 5*time.Second, func() bool {
		return rawCurrent().equal(orig)
	})
	waitFor(t, "serve 端口关闭", 10*time.Second, func() bool { return !portAlive(testPort) })
	if d := readServeJSON(t, home); d != nil {
		t.Errorf("off 后 serve.json 应删除: %+v", d)
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st2.LoadSnapshotJSON(); ok {
		st2.Close()
		t.Error("off 后快照应删除")
	}
	st2.Close()
}

// TestIntegrationCrashReconcile on → taskkill /F（无退出 hook）→ off
// 触发 ensureReconcile：恢复原值 + 清理残留（W3 D4 核心场景）。
func TestIntegrationCrashReconcile(t *testing.T) {
	exe := buildGhydra(t)
	env, home := isolatedEnv(t)
	orig := rawCurrent()
	t.Cleanup(func() {
		restoreRaw(t, orig)
		if d := readServeJSON(t, home); d != nil {
			exec.Command("taskkill", "/PID", fmt.Sprint(d.PID), "/T", "/F").Run()
		}
	})

	db := filepath.Join(home, "t.db")
	runGhydra(t, exe, env, "on", "--db", db, "--port", fmt.Sprint(testPort))
	d := readServeJSON(t, home)
	if d == nil {
		t.Fatal("serve.json 缺失")
	}

	// kill -9 语义：退出 hook（restoreSnapshot）不会执行
	if out, err := exec.Command("taskkill", "/PID", fmt.Sprint(d.PID), "/T", "/F").CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	waitFor(t, "serve 端口关闭", 10*time.Second, func() bool { return !portAlive(testPort) })
	// kill 后注册表仍是接管态（残留证明）
	if got := rawCurrent().autoConfigURL; got == "" {
		t.Fatal("kill 后注册表应残留接管态")
	}

	// 任意命令入口触发对账（off 是用户自然动作）
	runGhydra(t, exe, env, "off", "--db", db)

	waitFor(t, "对账恢复注册表", 5*time.Second, func() bool {
		return rawCurrent().equal(orig)
	})
	if rd := readServeJSON(t, home); rd != nil {
		t.Errorf("对账后 serve.json 应清理: %+v", rd)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadSnapshotJSON(); ok {
		st.Close()
		t.Error("对账后快照应删除")
	}
	st.Close()
}

func readServeJSON(t *testing.T, home string) *daemonState {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".ghydra", "serve.json"))
	if err != nil {
		return nil
	}
	var d daemonState
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	return &d
}
