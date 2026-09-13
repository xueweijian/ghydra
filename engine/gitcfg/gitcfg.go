// Package gitcfg 实现 GHydra 的 Git 深度集成（M2-W4 / PRD F5 P0）：
//
//	url.insteadOf  → fetch/clone 走 CDN（B 通道前缀反代）
//	url.pushInsteadOf → push 保持直连（git 官方语义：覆盖 insteadOf 的 push 行为）
//
// 设计铁律（D6）：
//   - 所有 gitconfig 读写经 `git config` 子命令，绝不手写 INI 解析
//     （Windows 行尾/include/多级键兼容性是深坑，git 自己最懂）。
//   - 写前快照、恢复精确到「我们 unset 掉的键的原值」；用户其他 url.*
//     键（如公司 GitLab 的 insteadOf）永不被触碰或覆盖。
//   - 冲突检测：任何涉及 github.com 的既有 rewrite 归别人所有 → 拒绝，
//     除非 --force。
package gitcfg

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Key 一条 git config 键值对（作用于 --global 作用域）。
type Key struct {
	Name  string `json:"name"`  // 完整键名，如 url.https://gh.x/t/T/https://github.com/.insteadOf
	Value string `json:"value"` // 单值（git 键天然多值，见 Snapshot.Prior）
}

// KV 是从 `git config --list --null` 读出的一行（键可多值，逐行展开）。
type KV struct {
	Name  string
	Value string
}

// Options enable 参数。
type Options struct {
	CDN        string // B 通道前缀，必须以 / 结尾（如 https://gh.x/t/TOK/）
	PushViaB   bool   // push 也走 CDN（跳过 pushInsteadOf）
	SSHRewrite bool   // 追加 insteadOf git@github.com:（SSH remote 的 fetch 走 HTTPS-B）
}

// Conflict 描述一条既有键与新键的冲突。
type Conflict struct {
	Existing KV   `json:"existing"`
	Desired  Key  `json:"desired"`
	IsOurs   bool `json:"is_ours"` // 同名同值 = 我们已写过（幂等），不算真冲突
}

// Snapshot 快照：written = enable 写入的键（disable 时 unset-all）；
// prior = 写入前这些键名下的既有值（disable 时按原序恢复）。
// 恢复精确语义：只还原我们动过的键名，其他键永不触碰。
type Snapshot struct {
	Written []Key               `json:"written"`
	Prior   map[string][]string `json:"prior"`
}

// 源前缀集合：insteadOf 的匹配端。SSH 改写开启时追加 scp-like 形态。
const (
	srcHTTPS = "https://github.com/"
	srcSSH   = "git@github.com:"
)

// Desired 按 Options 计算应写入的键集合（纯函数，测试锁语义）。
//
// insteadOf 基（base）= CDN 前缀 + 源 URL 的完整形态，gh-proxy 协议要求
// 目标以完整 URL 出现在路径中（worker 按 https?:// 切割解析）。
func Desired(opts Options) ([]Key, error) {
	cdn := opts.CDN
	if cdn == "" {
		return nil, errors.New("CDN 前缀为空")
	}
	if !strings.HasSuffix(cdn, "/") {
		cdn += "/"
	}
	if !strings.HasPrefix(cdn, "http://") && !strings.HasPrefix(cdn, "https://") {
		return nil, fmt.Errorf("CDN 前缀必须是 http(s) URL: %q", opts.CDN)
	}
	var keys []Key
	sources := []string{srcHTTPS}
	if opts.SSHRewrite {
		sources = append(sources, srcSSH)
	}
	for _, src := range sources {
		// fetch/clone → CDN
		keys = append(keys, Key{
			Name:  keyName(cdn+src, "insteadOf"),
			Value: src,
		})
		// push → 直连（pushInsteadOf 匹配原始 URL，改写回自身 = 直连语义；
		// git 官方文档：pushInsteadOf 覆盖 insteadOf 的 push 行为）
		if !opts.PushViaB {
			keys = append(keys, Key{
				Name:  keyName(src, "pushInsteadOf"),
				Value: src,
			})
		}
	}
	return keys, nil
}

// keyName 构造 url.<base>.<key> 完整键名。git 对键名最后一节大小写
// 不敏感且 --list 输出统一小写（url.insteadOf → url.<base>.insteadof），
// 这里直接写小写形态，读写一致。
func keyName(base, key string) string {
	return "url." + base + "." + strings.ToLower(key)
}

// normKey 归一化键名：最后一段（key 节）小写。subsection 大小写敏感，
// 保持原样。
func normKey(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return strings.ToLower(name)
	}
	return name[:i] + "." + strings.ToLower(name[i+1:])
}

// Exec 是 git config 执行器接口（测试注入假 git / 真 git+隔离 HOME）。
type Exec interface {
	// Run 执行 `git config --global <args...>`，返回原始 stdout。
	Run(args ...string) (string, error)
}

// CLIExec 生产执行器（git 在 PATH）。
type CLIExec struct {
	Bin string // 空 = "git"
}

func (c CLIExec) Run(args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.Command(bin, append([]string{"config", "--global"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			return out.String(), fmt.Errorf("git config %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return out.String(), err
	}
	return out.String(), nil
}

// ReadURLKeys 读出 global 作用域的全部 url.* 键（多值键逐行展开）。
// `--list --null` 输出格式为 key LF value NUL（NUL 仅作记录分隔；键值间
// 是 LF——subsection 可含空格，空格分隔不可靠）。
//
// 全新机器没有 gitconfig 文件：git 报 "unable to read config file" 且
// 退出 128——这是合法的空配置态（PRD 全新机验收路径），按空处理；
// 其他错误照常上抛。
func ReadURLKeys(e Exec) ([]KV, error) {
	out, err := e.Run("--list", "--null")
	if err != nil {
		if strings.Contains(err.Error(), "unable to read config file") {
			return nil, nil
		}
		return nil, err
	}
	var keys []KV
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		i := strings.IndexByte(rec, '\n')
		if i < 0 {
			continue // 无值的键（理论不存在，防御）
		}
		name := rec[:i]
		if strings.HasPrefix(name, "url.") {
			keys = append(keys, KV{Name: normKey(name), Value: rec[i+1:]})
		}
	}
	return keys, nil
}

// Plan 计算 enable 的动作清单与冲突。语义：
//   - 同名同值已存在 → ours（幂等，不重复写）
//   - 同名不同值存在 → 冲突（git 键多值：--add 会追加造成双重改写，
//     破坏用户语义；--force 路径 = 快照原值后 unset-all 再写我们的）
//   - 他人键改写 github（value 以我们的源前缀开头且键名非我们所写）→
//     冲突：同长前缀匹配顺序未定义，双重改写会产生歧义 URL
//   - 新键 → 直接写
func Plan(existing []KV, desired []Key) (add []Key, prior map[string][]string, conflicts []Conflict) {
	prior = map[string][]string{}
	byName := map[string][]string{}
	desiredNames := map[string]bool{}
	for _, d := range desired {
		desiredNames[normKey(d.Name)] = true
	}
	for _, k := range existing {
		byName[k.Name] = append(byName[k.Name], k.Value)
	}
	for _, k := range existing {
		if desiredNames[normKey(k.Name)] {
			continue
		}
		if k.Value == srcHTTPS || k.Value == srcSSH || strings.HasPrefix(k.Value, srcHTTPS) {
			conflicts = append(conflicts, Conflict{
				Existing: k,
				Desired:  Key{Name: k.Name, Value: "(ghydra 源前缀被他人改写)"},
				IsOurs:   false,
			})
		}
	}
	for _, d := range desired {
		vals, exists := byName[d.Name]
		if !exists {
			add = append(add, d)
			continue
		}
		ours := false
		for _, v := range vals {
			if v == d.Value {
				ours = true
				break
			}
		}
		if ours && len(vals) == 1 {
			continue // 幂等：已是我们的目标态
		}
		conflicts = append(conflicts, Conflict{Existing: KV{Name: d.Name, Value: strings.Join(vals, "\x00")}, Desired: d, IsOurs: false})
		prior[d.Name] = vals
	}
	return add, prior, conflicts
}

// Enable 执行 enable：读现状 → 冲突检查 → 快照 → 写入。
// 有真冲突且 !force 时返回冲突错误，不做任何修改。
// force 时：同名冲突键 unset 后写我们的值；他人改写 github 的外键
// 也一并 unset（原值入快照，disable 完整还原）。
func Enable(e Exec, opts Options, force bool) (Snapshot, error) {
	var snap Snapshot
	desired, err := Desired(opts)
	if err != nil {
		return snap, err
	}
	existing, err := ReadURLKeys(e)
	if err != nil {
		return snap, err
	}
	add, prior, conflicts := Plan(existing, desired)
	desiredNames := map[string]bool{}
	for _, d := range desired {
		desiredNames[normKey(d.Name)] = true
	}
	var real []Conflict
	for _, c := range conflicts {
		if !c.IsOurs {
			real = append(real, c)
		}
	}
	if len(real) > 0 && !force {
		var sb strings.Builder
		for _, c := range real {
			fmt.Fprintf(&sb, "  %s = %s\n", c.Desired.Name, c.Existing.Value)
		}
		return snap, fmt.Errorf("检测到既有 git url 改写冲突（--force 覆盖，原值会快照）:\n%s", sb.String())
	}
	// force：外键（非我们 desired 名）并入 prior + unset
	for _, c := range real {
		if desiredNames[c.Desired.Name] {
			continue // 同名冲突已入 prior
		}
		if _, dup := prior[c.Desired.Name]; dup {
			continue
		}
		var vals []string
		for _, k := range existing {
			if k.Name == c.Desired.Name {
				vals = append(vals, k.Value)
			}
		}
		prior[c.Desired.Name] = vals
	}
	snap = Snapshot{Written: desired, Prior: prior}
	for name := range prior {
		if _, err := e.Run("--unset-all", name); err != nil {
			return snap, fmt.Errorf("unset %s: %w", name, err)
		}
	}
	for _, k := range add {
		if _, err := e.Run("--add", k.Name, k.Value); err != nil {
			return snap, fmt.Errorf("add %s: %w", k.Name, err)
		}
	}
	// 同名冲突（force 路径）：unset 后写我们的目标值
	for _, k := range desired {
		if _, isPrior := prior[k.Name]; isPrior {
			if _, err := e.Run("--add", k.Name, k.Value); err != nil {
				return snap, fmt.Errorf("add %s: %w", k.Name, err)
			}
		}
	}
	return snap, nil
}

// Disable 从快照恢复：unset 我们写的全部键名 → 按原值恢复 prior。
// 幂等：重复调用安全（unset 不存在的键 git 报错，此处忽略）。
func Disable(e Exec, snap Snapshot) error {
	for _, k := range snap.Written {
		_, _ = e.Run("--unset-all", k.Name)
	}
	names := make([]string, 0, len(snap.Prior))
	for name := range snap.Prior {
		names = append(names, name)
	}
	for _, name := range names {
		for _, v := range snap.Prior[name] {
			if _, err := e.Run("--add", name, v); err != nil {
				return fmt.Errorf("恢复 %s: %w", name, err)
			}
		}
	}
	return nil
}

// RewriteDemo 演示给定 URL 在 Desired 规则下的改写结果（纯函数）。
// 返回 (改写后 URL, 是否命中)。只认我们的键（base 前缀匹配）。
func RewriteDemo(opts Options, raw string) (string, bool) {
	cdn := opts.CDN
	if !strings.HasSuffix(cdn, "/") {
		cdn += "/"
	}
	sources := []string{srcHTTPS}
	if opts.SSHRewrite {
		sources = append(sources, srcSSH)
	}
	for _, src := range sources {
		if strings.HasPrefix(raw, src) {
			return cdn + raw, true
		}
	}
	return raw, false
}

// IsManaged 判断一条既有键是否是 GHydra 管理形态（value 是我们的源前缀
// 且 name 的 base 以 value 开头——即 CDN+src 结构）。用于 status 展示。
func IsManaged(k KV, opts Options) bool {
	cdn := opts.CDN
	if !strings.HasSuffix(cdn, "/") {
		cdn += "/"
	}
	suffix := "." + k.Name[strings.LastIndex(k.Name, ".")+1:]
	if suffix != ".insteadOf" && suffix != ".pushInsteadOf" {
		return false
	}
	base := strings.TrimSuffix(strings.TrimPrefix(k.Name, "url."), suffix)
	for _, src := range []string{srcHTTPS, srcSSH} {
		if k.Value == src && base == cdn+src {
			return true
		}
		if k.Value == src && src == srcHTTPS && base == src {
			return true // pushInsteadOf 直连键
		}
	}
	return false
}
