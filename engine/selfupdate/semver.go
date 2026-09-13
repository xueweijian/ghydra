// Package selfupdate 实现 ghydra 自更新（M3-W4 设计 §2）。
//
// 威胁模型 U1-U8：资产 sha256 + checksums.txt 本身 minisign 验签双防线；
// semver 单调防回滚；资产域白名单；尺寸硬顶；原子交换 + 崩溃恢复 +
// 启动自检自动回滚（"永不自毁"承诺）。签名验证复用 engine/rules 的
// minisign 实现（W2 已用官方工具互操作验证）。
package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 语义化版本（主轴子集：三段 + prerelease；build metadata 忽略）。
type Version struct {
	Major, Minor, Patch int
	Pre                 string // "beta.1" / ""；不含前导连字符
}

// ParseVersion 解析 "1.0.1" / "v1.0.1-beta.1"（v 前缀容忍）。
// 拒绝：段数≠3、非数字、前导零、>6 位数（防荒谬值）、非法 prerelease 字符。
func ParseVersion(s string) (Version, error) {
	orig := s
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, fmt.Errorf("selfupdate: 版本串为空 %q", orig)
	}
	// build metadata 不参与任何语义，直接剥掉
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
		if pre == "" || !validPre(pre) {
			return Version{}, fmt.Errorf("selfupdate: 非法 prerelease %q", orig)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("selfupdate: 版本须为三段 %q", orig)
	}
	nums := [3]int{}
	for i, p := range parts {
		if len(p) > 6 {
			return Version{}, fmt.Errorf("selfupdate: 版本段超长 %q", orig)
		}
		if len(p) > 1 && p[0] == '0' {
			return Version{}, fmt.Errorf("selfupdate: 前导零拒绝 %q", orig)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("selfupdate: 非数字段 %q", orig)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// validPre prerelease 标识：点分段，每段 [0-9A-Za-z]+，纯数字段无前导零。
func validPre(pre string) bool {
	for _, seg := range strings.Split(pre, ".") {
		if seg == "" {
			return false
		}
		allDigit := true
		for _, c := range seg {
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
				allDigit = false
			default:
				return false
			}
		}
		if allDigit && len(seg) > 1 && seg[0] == '0' {
			return false
		}
	}
	return true
}

// Compare 比较：-1 v<o / 0 等值 / 1 v>o。prerelease < 正式（semver §11）。
func (v Version) Compare(o Version) int {
	if d := cmpInt(v.Major, o.Major); d != 0 {
		return d
	}
	if d := cmpInt(v.Minor, o.Minor); d != 0 {
		return d
	}
	if d := cmpInt(v.Patch, o.Patch); d != 0 {
		return d
	}
	// prerelease 优先级：空（正式）> 非空
	switch {
	case v.Pre == "" && o.Pre == "":
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	// 逐点分段比较：数字段<字母段数值比；段数多者大；数字段数值比
	a, b := strings.Split(v.Pre, "."), strings.Split(o.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aerr := strconv.Atoi(a[i])
		bn, berr := strconv.Atoi(b[i])
		switch {
		case aerr == nil && berr == nil:
			if d := cmpInt(an, bn); d != 0 {
				return d
			}
		case aerr == nil: // 数字 < 字母
			return -1
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (v Version) String() string {
	if v.Pre != "" {
		return fmt.Sprintf("%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Pre)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}
