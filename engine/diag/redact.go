// Package diag —— 脱敏诊断包（W4p2 设计 D3）。
//
// 信任模型（双防线）：
//  1. Redact 正则脱敏：所有文本 entry 出 zip 前必经；
//  2. Secrets 精确串扫描：调用方把已知敏感原文（如 api-token）传入，
//     任一 entry 命中即报错拒导——宁可导不出，不可带泄导出。
//
// 绝不导出（设计表）：api-token 内容、系统代理原值（只导接管存在性）、
// 环境变量、家目录外绝对路径（Redact 家目录→~ 兜底）。
package diag

import (
	"regexp"
	"strings"
)

var (
	// URL 凭据：scheme://user:pass@host（用户名与密码一起去除）
	reURLCred = regexp.MustCompile(`(?i)\b(https?|socks5h?)://([^/:\s]+):([^@\s]+)@`)
	// Authorization 头
	reAuth = regexp.MustCompile(`(?i)\b((?:authorization|proxy-authorization):\s*(?:bearer|basic|token)\s+)[A-Za-z0-9._~+/=-]+`)
	// token 类键值对（json/flag/yaml 常见形态）
	reKV = regexp.MustCompile(`(?i)\b((?:api[_-]?token|access[_-]?token|token|password|passwd|secret|api[_-]?key)["']?\s*[:=]\s*["']?)([A-Za-z0-9._~+/=-]{6,})`)
	// 裸 32 hex（api-token 文件形态）；40 hex commit sha 因两侧 hex 类
	// 限定不误伤（RE2 无 lookahead → 捕获组实现）
	reHex32 = regexp.MustCompile(`(^|[^0-9a-fA-F])([0-9a-fA-F]{32})($|[^0-9a-fA-F])`)
)

// Redact 返回脱敏后的文本（幂等）。home 为用户家目录（脱敏为 ~），
// 其 basename 作为用户名一并脱敏（仅在合理边界处，避免误伤普通词）。
func Redact(home, s string) string {
	if s == "" {
		return s
	}
	out := s
	if home != "" {
		out = strings.ReplaceAll(out, home, "~")
		// Windows 反斜杠形态
		out = strings.ReplaceAll(out, strings.ReplaceAll(home, `/`, `\`), "~")
		if u := baseUser(home); len(u) >= 3 {
			// 用户名：词边界限定（RE2 无 lookbehind；\b 为 ASCII 词边界），
			// 避免普通词误伤（"the user agent" 的 user 不在边界规则内被替换）
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(u) + `\b`)
			out = re.ReplaceAllString(out, "<user>")
		}
	}
	out = reURLCred.ReplaceAllString(out, `$1://<redact>@`)
	out = reAuth.ReplaceAllString(out, `$1<redact>`)
	out = reKV.ReplaceAllString(out, `$1<redact>`)
	// 裸 32 hex：前后都不得是 hex（RE2 无 lookahead，用捕获组兜两侧；
	// 相邻两个 32hex 仅隔 1 字符的极端场景漏脱——Secrets 精确扫描兜底）
	out = reHex32.ReplaceAllString(out, `$1<redact-32hex>$3`)
	return out
}

func baseUser(home string) string {
	h := strings.TrimRight(strings.ReplaceAll(home, `\`, `/`), `/`)
	if i := strings.LastIndexByte(h, '/'); i >= 0 {
		return h[i+1:]
	}
	return h
}

// TailLines 取尾部 n 行（诊断包固定截尾，防日志爆炸）。
func TailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
