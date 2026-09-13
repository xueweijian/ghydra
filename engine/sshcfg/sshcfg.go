// Package sshcfg 实现 SSH 场景的 443 端口优化写入（M2-W4 / PRD F5 P0）：
// github.com:22 被断而 ssh.github.com:443 活时，把 github.com 的 SSH 流量
// 引到 443。文本级 managed 块 + 字节级快照恢复。
//
// 语义铁律（ssh_config first-match-wins）：managed 块必须插文件顶部才能
// 覆盖既有 Host github.com 段。若文件已有 github.com 冲突段 → 拒绝直接
// 插入，提供 --alias 模式（Host github.com-b 尾部追加，零修改既有内容）。
package sshcfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MarkerBegin = "# >>> ghydra managed (ssh-over-443) >>>"
	MarkerEnd   = "# <<< ghydra managed <<<"
	AliasHost   = "github.com-b"
)

// Block 生成 managed 块文本。host 是块生效的 Host 名（github.com 或
// alias 模式的 github.com-b）。
func Block(host string) string {
	return fmt.Sprintf(`%s
Host %s
    HostName ssh.github.com
    Port 443
    User git
%s`, MarkerBegin, host, MarkerEnd)
}

// Plan 是 dry-run 的输出：将执行的动作与结果文件全文。
type Plan struct {
	Action   string // "prepend" | "append" | "replace" | "remove" | "noop"
	Conflict bool   // true = 存在既有 Host github.com 段且未用 alias
	Content  string // 操作后的完整文件内容（remove 时为移除块后的内容）
	Backup   string // 快照文件路径（仅真实写入时非空）
}

// hostLineRe 匹配 Host 行的模式列表部分（HostName 不匹配：Host 后必须
// 紧跟空白）。
var hostLineRe = regexp.MustCompile(`(?im)^\s*Host\s+(\S.*)$`)

// HasGithubHost 检测文件内容是否已有覆盖 github.com 的 Host 段。
// Host 行是空格分隔的模式列表，每个模式又可是逗号分隔的 pattern-list；
// 仅显式 github.com（大小写不敏感）算冲突——通配段（*.github.com）与
// 相邻域名（github.com.evil.cn）不算。
func HasGithubHost(content string) bool {
	for _, m := range hostLineRe.FindAllStringSubmatch(content, -1) {
		for _, tok := range strings.Fields(m[1]) {
			for _, p := range strings.Split(tok, ",") {
				if strings.EqualFold(strings.TrimSpace(p), "github.com") {
					return true
				}
			}
		}
	}
	return false
}

// Enable 计算并（除非 dryRun）执行 enable。
//   - 文件无 github.com 冲突段 → managed 块插顶部（覆盖一切既有段）
//   - 已有块 → 幂等替换（内容更新）
//   - 冲突且 alias=false → 返回冲突错误
//   - alias=true → Host github.com-b 块尾部追加（冲突时也安全）
//
// 首次真实写入前把原文件字节拷贝到 backupPath（disable 还原用）。
func Enable(path string, alias, dryRun bool, backupPath string) (Plan, error) {
	var orig []byte
	exists := true
	orig, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		exists = false
		err = nil
	}
	if err != nil {
		return Plan{}, err
	}
	content := string(orig)
	host := "github.com"
	action := "prepend"

	// 先摘除既有 managed 块（幂等替换的基础），冲突检测只看用户自己的内容
	rest := content
	hadBlock := false
	if begin := strings.Index(rest, MarkerBegin); begin >= 0 {
		end := strings.Index(rest[begin:], MarkerEnd)
		if end < 0 {
			return Plan{}, fmt.Errorf("%s 存在残缺 managed 块（有始无终），请手动检查", path)
		}
		rest = rest[:begin] + rest[begin+end+len(MarkerEnd):]
		hadBlock = true
	}
	if alias {
		host = AliasHost
		action = "append"
	} else if HasGithubHost(rest) {
		return Plan{Conflict: true, Content: content}, fmt.Errorf("%s 已有覆盖 github.com 的 Host 段（first-match-wins，顶部插入无法安全覆盖）；改用 --alias 模式或手动处理", path)
	}

	if hadBlock {
		action = "replace"
		content = rest
	}
	if alias {
		content = strings.TrimRight(content, "\n") + "\n\n" + Block(host) + "\n"
	} else {
		content = Block(host) + "\n\n" + strings.TrimLeft(content, "\n")
	}
	if !exists {
		content = Block(host) + "\n"
	}

	plan := Plan{Action: action, Content: content}
	if dryRun {
		return plan, nil
	}
	if exists {
		if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
			return plan, err
		}
		if err := os.WriteFile(backupPath, orig, 0o600); err != nil {
			return plan, err
		}
		plan.Backup = backupPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return plan, err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return plan, err
	}
	return plan, nil
}

// Disable 还原：优先字节级快照还原；无快照时按 marker 块移除（幂等）。
func Disable(path, backupPath string, dryRun bool) (Plan, error) {
	var plan Plan
	if b, err := os.ReadFile(backupPath); err == nil {
		plan.Action = "restore"
		plan.Content = string(b)
		if dryRun {
			return plan, nil
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return plan, err
		}
		_ = os.Remove(backupPath)
		return plan, nil
	}
	orig, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		plan.Action = "noop"
		return plan, nil
	}
	if err != nil {
		return plan, err
	}
	content := string(orig)
	begin := strings.Index(content, MarkerBegin)
	if begin < 0 {
		plan.Action = "noop"
		plan.Content = content
		return plan, nil
	}
	end := strings.Index(content[begin:], MarkerEnd)
	if end < 0 {
		return plan, fmt.Errorf("%s 存在残缺 managed 块，请手动检查", path)
	}
	content = strings.TrimLeft(content[:begin]+content[begin+end+len(MarkerEnd):], "\n")
	if !strings.HasSuffix(content, "\n") && content != "" {
		content += "\n"
	}
	plan.Action = "remove"
	plan.Content = content
	if dryRun {
		return plan, nil
	}
	return plan, os.WriteFile(path, []byte(content), 0o600)
}

// Effective 展示块当前状态（status 用）。
func Effective(path string) (enabled bool, aliased bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	content := string(b)
	if !strings.Contains(content, MarkerBegin) {
		return false, false, nil
	}
	begin := strings.Index(content, MarkerBegin)
	seg := content[begin:]
	if i := strings.Index(seg, MarkerEnd); i >= 0 {
		seg = seg[:i]
	}
	return true, strings.Contains(seg, "Host "+AliasHost), nil
}
