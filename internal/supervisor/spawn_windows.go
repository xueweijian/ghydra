//go:build windows

package supervisor

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// spawnDetached Windows 版：DETACHED_PROCESS(0x8) | CREATE_NEW_PROCESS_GROUP
// (0x200)——ghydra on 退出后 serve 存活（spawn_windows.go detachAttr 同源）。
func spawnDetached(exe string, args, env []string, logPath string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x8 | 0x200}
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

// killHard Windows：taskkill /F（进程表移除有延迟，仅测试清理用）。
func killHard(pid int) {
	if pid > 0 {
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
}
