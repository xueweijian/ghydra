// elevate.go —— update 提权决策（v1.0.3 PR3）。
//
// 背景（v1.0.2 真机实证）：NSIS 装机在 Program Files，普通权限进程
// ApplyPlan 的 staging MkdirAll 必拒（ErrDirNotWritable）——当年
// v1.0.0→v1.0.1「Access is denied」三连败的根因。策略：
//
//	可写                    → 直接继续（Proceed）
//	不可写 + 未提权         → UAC runas 重跑 `ghydra update --elevated`（Relaunched）
//	不可写 + 已提权（--elevated）→ 报错（Fail）——提权后仍不可写是异态，
//	                               绝不再弹 UAC（防无限循环）
//
// 决策纯逻辑与平台壳分离（elevate_windows.go / elevate_other.go），
// 测试全注入（elevate_test.go 决策表）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type elevateAction int

const (
	elevateProceed   elevateAction = iota // 目录可写，本进程直接执行
	elevateRelaunched                     // 已弹 UAC 交棒提权进程，本进程静默退场
	elevateFail                           // 不可继续（调用方以错误退出）
)

// elevateEnvGuard 提权交棒标记的环境侧名字（--elevated 旗标在 updateCmd
// 解析时置入本进程 env，两层等价——env 判断便于非 CLI 入口复用）。
const elevateEnvGuard = "GHYDRA_ELEVATED"

type elevateDeps struct {
	self     string                                  // 自身 exe 路径（runas 目标）
	writable func(dir string) bool                   // 安装目录可写探测
	elevated func() bool                              // 提权护栏已置？
	runas    func(exe, args string) error            // UAC 弹窗重跑（非 Windows 为 nil/报错实现）
}

// decideElevate 决策表（TestElevateDecisionTable 锁定）。
func decideElevate(d elevateDeps) (elevateAction, error) {
	dir := filepath.Dir(d.self)
	if d.writable(dir) {
		return elevateProceed, nil
	}
	if d.elevated() {
		return elevateFail, fmt.Errorf("安装目录 %s 不可写（已尝试提权仍失败）——请以管理员身份运行，或从 Release 下载安装包覆盖升级", dir)
	}
	args := "update --elevated"
	if err := d.runas(d.self, args); err != nil {
		return elevateFail, fmt.Errorf("需要管理员权限（UAC 被取消或失败: %v）——请以管理员身份重新运行 ghydra update，或从 Release 下载安装包覆盖升级", err)
	}
	return elevateRelaunched, nil
}

// probeWritable 目录可写探测：建删临时目录试探（ACL/只读挂载点语义真实）。
func probeWritable(dir string) bool {
	tmp, err := os.MkdirTemp(dir, ".ghydra-probe-*")
	if err != nil {
		return false
	}
	return os.Remove(tmp) == nil
}

// maybeElevateUpdate updateCmd 装配入口：OS 非 Windows 时恒 Proceed
// （用户态安装位置可写；不可写由 sudo 场景的常规报错兜底）。
var maybeElevateUpdate = func() (elevateAction, error) {
	self, err := os.Executable()
	if err != nil {
		return elevateProceed, nil // 探测失败不拦截主流程
	}
	return decideElevate(elevateDeps{
		self:     self,
		writable: probeWritable,
		elevated: func() bool { return os.Getenv(elevateEnvGuard) != "" },
		runas:    runasRelaunch,
	})
}
