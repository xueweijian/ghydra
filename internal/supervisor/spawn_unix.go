//go:build !windows

package supervisor

import (
	"os"
	"os/exec"
	"syscall"
)

// spawnDetached 拉起脱离会话的子进程（unix setsid，孤儿由 init 收养）。
//
// spawn 三课（daemon.go spawnServeAt 同源教训，勿删注释）：
//   - 日志 append 落盘：detached 子进程默认 /dev/null，daemon 日志必须
//     可追溯；
//   - fd 必须在 Start() 之后才关——提前关会让 fork/exec 拿到已关闭的
//     fd（macOS 实证 "bad file descriptor"）；
//   - cmd.Wait() 由独立 goroutine 回收（不阻塞监督循环，避免僵尸进程）。
func spawnDetached(exe string, args, env []string, logPath string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if len(env) > 0 {
		cmd.Env = env
	}
	var logFile *os.File
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			logFile = f
		}
	}
	err := cmd.Start()
	if logFile != nil {
		logFile.Close() // 子进程已持有自己的 fd 副本
	}
	if err != nil {
		return 0, err
	}
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

// killHard SIGKILL（测试清理用；监督循环本身不杀进程——优雅退出归
// serve 自己的 shutdownCh/信号处理）。
func killHard(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
