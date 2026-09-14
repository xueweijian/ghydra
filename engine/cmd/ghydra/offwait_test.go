package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// NSIS 卸载序列依赖 off --wait（D4 安全关键）：
// 返回时 serve 必须死透（Windows 下 exe 锁定问题）。
func TestOffWaitWaitsForServeDeath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// 假 serve：长睡子进程（serve.json 指向它）
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep 不可用: %v", err)
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid

	// 测试进程是假 serve 的父进程——SIGKILL 后它变僵尸，kill(pid,0) 对
	// 僵尸仍返回 0（procAlive true）。真实卸载场景 off 与 serve 无父子
	// 关系（init 收尸）无此问题；测试侧周期收尸模拟真实环境。
	reaped := make(chan struct{})
	defer close(reaped)
	go func() {
		for {
			select {
			case <-reaped:
				return
			default:
				reapUnix(pid)
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()

	gdir := filepath.Join(home, ".ghydra")
	os.MkdirAll(gdir, 0o700)
	writeServeJSON(t, gdir, pid)

	start := time.Now()
	offCmd([]string{"--wait"}) // 旧行为 fire-and-forget 会立即返回
	elapsed := time.Since(start)

	if procAlive(pid) {
		t.Fatal("off --wait 返回后假 serve 仍存活")
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("等待语义未生效（耗时 %v，应含轮询窗口）", elapsed)
	}
	if _, err := os.Stat(filepath.Join(gdir, "serve.json")); !os.IsNotExist(err) {
		t.Error("off 后 serve.json 应被清理")
	}
}

// procAlive 边界：死 pid false、非法 pid false、自身 true。
func TestProcAlive(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("自身进程应判活")
	}
	if procAlive(0) || procAlive(-1) {
		t.Fatal("非法 pid 应判死")
	}
	// spawn 一个短命进程，等它退出后判死
	c := exec.Command("true")
	c.Start()
	pid := c.Process.Pid
	c.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for procAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if procAlive(pid) {
		t.Fatal("已退出进程应判死（或 3s 内被系统回收）")
	}
}

// offExitCode 契约测试：--wait 下恢复失败 → 非零（NSIS 弹窗依据）。
func TestOffExitCode(t *testing.T) {
	boom := errors.New("restore failed")
	if got := offExitCode(boom, true); got == 0 {
		t.Error("--wait + 恢复失败应非零退出")
	}
	if got := offExitCode(boom, false); got != 0 {
		t.Errorf("交互模式恢复失败保持退出 0（历史语义），got %d", got)
	}
	if got := offExitCode(nil, true); got != 0 {
		t.Errorf("--wait + 成功应 0，got %d", got)
	}
}

func writeServeJSON(t *testing.T, gdir string, pid int) {
	t.Helper()
	content := []byte(`{"pid":` + itoa(pid) + `,"port":9977,"started_at":0}`)
	if err := os.WriteFile(filepath.Join(gdir, "serve.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
