package supervisor

// spawn_integration_test.go —— P4c L2：真进程 spawn + 真 serve.json +
// 真 HTTP 探活 + 真 token 全栈（helper 进程模式：测试二进制重执行自身）。
//
// 平台矩阵：unix（setsid）在本地 linux + CI 三平台 linux/mac 跑；
// windows 走 taskkill 分支，CI windows go test 覆盖。

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestHelperServeProcess 假 serve：写 serve.json + 起 /api/status，永驻
// （由父测试 killHard 清理）。真实测试进程里直接 return（非 helper 模式）。
func TestHelperServeProcess(t *testing.T) {
	if os.Getenv("SUP_HELPER") != "1" {
		return
	}
	dir := os.Getenv("SUP_HELPER_DIR")
	port := os.Getenv("SUP_HELPER_PORT")
	tok := os.Getenv("SUP_HELPER_TOKEN")
	if dir == "" || port == "" {
		os.Exit(3)
	}
	// serve.json：pid + port（运行态真相自写——serve 契约）
	if err := writeAtomic(filepath.Join(dir, "serve.json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"port":%s,"started_at":1}`, os.Getpid(), port))); err != nil {
		os.Exit(4)
	}
	if tok != "" {
		if err := writeAtomic(filepath.Join(dir, "api-token"), []byte(tok)); err != nil {
			os.Exit(5)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if tok != "" && r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"api_version":1,"version":"9.9.9","listen":"127.0.0.1:`+port+`"}`)
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		os.Exit(6)
	}
	select {} // 永驻
}

func TestSpawnDetachedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	tok := "sup-int-token"

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"SUP_HELPER=1",
		"SUP_HELPER_DIR=" + dir,
		"SUP_HELPER_PORT=" + strconv.Itoa(port),
		"SUP_HELPER_TOKEN=" + tok,
	}
	logPath := filepath.Join(dir, "helper.log")
	pid, err := spawnDetached(exe, []string{"-test.run=TestHelperServeProcess", "-test.timeout=60s"}, env, logPath)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer killHard(pid)

	// 探活全栈：serve.json 出现 → 带token GET /api/status → 9.9.9
	l := New(Config{Dir: dir, ExePath: exe, Logf: t.Logf})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p := l.probeFiles(); p.alive {
			if p.version != "9.9.9" {
				t.Fatalf("版本不符: %q", p.version)
			}
			return // PASS：spawn + serve.json + token + HTTP 探活全通
		}
		if time.Now().After(deadline) {
			t.Fatalf("10s 内未探活（helper 日志: %s）", tailFile(logPath))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLoadServeStateGarbage 坏 serve.json 不致死（返回 nil = 未运行态）。
func TestLoadServeStateGarbage(t *testing.T) {
	dir := t.TempDir()
	writeAtomic(filepath.Join(dir, "serve.json"), []byte("{not json"))
	l := New(Config{Dir: dir, ExePath: os.TempDir() + "/x", Logf: t.Logf})
	if p := l.probeFiles(); p.alive {
		t.Fatal("坏 serve.json 不应判活")
	}
}

// ---- 测试小工具 ----

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeAtomic(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644) // 测试/ helper 无并发写，直写足够
}

func tailFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(读不到)"
	}
	s := string(b)
	if len(s) > 500 {
		s = s[len(s)-500:]
	}
	return s
}
