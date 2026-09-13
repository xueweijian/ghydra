package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xueweijian/ghydra/engine/internal/fsx"
)

// State 更新状态机（~/.ghydra/update.json，设计 §2.3）。
//
//	无文件                     → 正常启动
//	{pending, confirmed:false} → 自检模式（ShouldSelfCheck）
//	  自检通过 → Confirm()；失败累积 → 第 3 次自动回滚 + bad_version
//	{confirmed:true}           → 正常启动
type State struct {
	PendingVersion   string    `json:"pending_version,omitempty"`
	Confirmed        bool      `json:"confirmed"`
	BootAttempts     int       `json:"boot_attempts"`
	BadVersion       string    `json:"bad_version,omitempty"`
	DaemonWasRunning bool      `json:"daemon_was_running"`
	AppliedAt        time.Time `json:"applied_at,omitempty"`
}

// StateFileName 状态文件名（位于 ~/.ghydra/）。
const StateFileName = "update.json"

// LoadState 读取状态。文件不存在返回 (nil, false, nil)；坏 JSON 报错
// （宁拒勿猜——状态文件损坏时保守路径：当作无 pending，不触发自检；
// 坏版本记忆丢失可接受，回滚凭证在文件系统层不受影响）。
func LoadState(path string) (*State, bool, error) {
	data, err := fsx.ReadFileRetry(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("selfupdate: 读状态: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, false, fmt.Errorf("selfupdate: 状态 JSON 损坏: %w", err)
	}
	return &s, true, nil
}

// SaveState 原子落盘（tmp+fsync+rename，崩溃不留半文件）。
func SaveState(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("selfupdate: mkdir 状态目录: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("selfupdate: 序列化状态: %w", err)
	}
	return fsx.WriteFileAtomic(path, data, 0o644)
}

// ShouldSelfCheck 启动自检门：有 pending 且未确认。
func (s *State) ShouldSelfCheck() bool {
	return s != nil && s.PendingVersion != "" && !s.Confirmed
}

// RecordBootFailure 记一次自检失败，返回是否触发自动回滚（>2 次，
// 拍板 #2：三次失败换回旧版）。
func (s *State) RecordBootFailure() bool {
	s.BootAttempts++
	return s.BootAttempts > 2
}

// MarkRolledBack 回滚落地：清 pending + 记 bad_version（该版本以后跳过）。
func (s *State) MarkRolledBack(bad string) {
	s.PendingVersion = ""
	s.BadVersion = bad
}

// IsBad 版本是否曾被回滚。
func (s *State) IsBad(v string) bool {
	return s != nil && s.BadVersion != "" && s.BadVersion == v
}

// Confirm 自检通过。
func (s *State) Confirm() {
	s.Confirmed = true
}
