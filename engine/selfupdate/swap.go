package selfupdate

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/xueweijian/ghydra/engine/internal/fsx"
)

// 交换布局（设计 §2.2）：安装目录内
//   ghydra(.exe)              当前版本
//   ghydra(.exe).old          上一版（回滚凭证）
//   ghydra(.exe).bad          回滚时留档的坏版（诊断用）
//   update-staging/           已验签解包的新文件
// Windows 关键事实：运行中 exe 可 rename 不可 delete（同卷）——交换序列
// 依赖此特性，锁竞争走 fsx.RenameAtomic 退避。

var (
	// ExeName 平台主程序名（win: ghydra.exe / 其余: ghydra）。
	ExeName = "ghydra"
	// OldName 上一版凭证名。
	OldName = "ghydra.old"
	// BadName 坏版留档名。
	BadName = "ghydra.bad"
	// StagingDirName 解包暂存目录名（相对安装目录）。
	StagingDirName = "update-staging"
)

func init() {
	if runtime.GOOS == "windows" {
		ExeName = "ghydra.exe"
	}
	OldName = ExeName + ".old"
	BadName = ExeName + ".bad"
}

// OpKind 交换操作类型。
type OpKind int

const (
	OpRename     OpKind = iota // From → To（相对安装目录）
	OpRemove                   // Target 删除
	OpRestoreOld               // Target 恢复：Target.old → Target（崩溃恢复用）
)

// Op 单条交换操作（路径相对安装目录）。
type Op struct {
	Kind   OpKind
	From   string
	To     string
	Target string
}

func (o Op) String() string {
	switch o.Kind {
	case OpRename:
		return fmt.Sprintf("rename %s → %s", o.From, o.To)
	case OpRemove:
		return fmt.Sprintf("remove %s", o.Target)
	case OpRestoreOld:
		return fmt.Sprintf("restore %s (from .old)", o.Target)
	}
	return "op?"
}

// PlanSwap 生成交换序列。前置校验：staging 内全部文件在场（缺即拒，
// 不碰现有安装）。序列：每文件 [清残留 old] → [现版 → old] → [staging → 现版]。
func PlanSwap(dir string, names []string) ([]Op, error) {
	var ops []Op
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, StagingDirName, name)); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrStagingIncomplete, name)
		}
		old := name + ".old"
		if _, err := os.Stat(filepath.Join(dir, old)); err == nil {
			ops = append(ops, Op{Kind: OpRemove, Target: old})
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			ops = append(ops, Op{Kind: OpRename, From: name, To: old})
		}
		ops = append(ops, Op{Kind: OpRename, From: filepath.Join(StagingDirName, name), To: name})
	}
	return ops, nil
}

// ExecuteSwap 执行交换序列（fsx.RenameAtomic：Windows 锁退避）。
func ExecuteSwap(dir string, ops []Op) error {
	for _, op := range ops {
		switch op.Kind {
		case OpRename:
			if err := fsx.RenameAtomic(filepath.Join(dir, op.From), filepath.Join(dir, op.To)); err != nil {
				return fmt.Errorf("selfupdate: swap %s: %w", op, err)
			}
		case OpRemove:
			if err := os.Remove(filepath.Join(dir, op.Target)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("selfupdate: swap %s: %w", op, err)
			}
		case OpRestoreOld:
			if err := fsx.RenameAtomic(filepath.Join(dir, op.Target+".old"), filepath.Join(dir, op.Target)); err != nil {
				return fmt.Errorf("selfupdate: swap %s: %w", op, err)
			}
		}
	}
	return nil
}

// RecoverFromCrash 启动清扫（每次 ghydra 启动入口调用，幂等）：
//  1. 清 staging 残留（半包解压物）——失败仅告警不阻断（速赢包：
//     提权更新残留 ACL 时非提权进程删不掉，此前每条命令刷错并跳过
//     后续恢复；staging 残留无害，待提权/重装清理）；
//  2. 清 .bad 残留（保留最近一次供诊断的价值 < 干净状态，v1 删除）；
//  3. 崩溃恢复：name 缺失而 name.old 在场（交换序列中断）→ old 恢复回正身。
//
// 返回实际执行的动作（观测/测试用）。
func RecoverFromCrash(dir string, names []string) ([]Op, error) {
	var actions []Op
	staging := filepath.Join(dir, StagingDirName)
	if err := os.RemoveAll(staging); err != nil {
		log.Printf("[selfupdate] staging 清扫跳过（无权限，待提权清理）: %v", err)
	}
	for _, name := range names {
		bad := filepath.Join(dir, name+".bad")
		// v1.0.3 PR2：与 staging 清扫同降级语义——.bad 删不掉（提权
		// 残留 ACL 等）只告警，不得阻断崩溃恢复（old 恢复正身更要紧）。
		if err := os.Remove(bad); err != nil && !os.IsNotExist(err) {
			log.Printf("[selfupdate] .bad 清扫跳过（残留无害）: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			if _, errOld := os.Stat(filepath.Join(dir, name+".old")); errOld == nil {
				actions = append(actions, Op{Kind: OpRestoreOld, Target: name})
			}
		}
	}
	if err := ExecuteSwap(dir, actions); err != nil {
		return nil, err
	}
	return actions, nil
}

// RollbackSwap 回滚：坏现版留档 .bad，旧凭证恢复正身。
// 全量前置校验（任一文件缺 old 即拒，不产生半回滚）。
func RollbackSwap(dir string, names []string) error {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name+".old")); err != nil {
			return fmt.Errorf("%w: %s", ErrNoOldToRollback, name)
		}
	}
	for _, name := range names {
		if err := fsx.RenameAtomic(filepath.Join(dir, name), filepath.Join(dir, name+".bad")); err != nil {
			return fmt.Errorf("selfupdate: rollback 留档 %s: %w", name, err)
		}
		if err := fsx.RenameAtomic(filepath.Join(dir, name+".old"), filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("selfupdate: rollback 恢复 %s: %w", name, err)
		}
	}
	return nil
}
