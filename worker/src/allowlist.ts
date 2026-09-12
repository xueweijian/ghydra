// 白名单 + 入站目标 URL 防御解析。
// 协议（gh-proxy 兼容）：GET https://<worker>/<完整目标URL>
// 目标 URL 原样拼接在 Worker 域名后，pathname+search 合并解析。

export const DEFAULT_ALLOWED_HOSTS: readonly string[] = [
  "github.com",
  "api.github.com",
  "codeload.github.com",
  "gist.github.com",
  "raw.githubusercontent.com",
  "objects.githubusercontent.com",
  // release-assets 是 2024+ Release 下载 302 的真实落点（真网实证
  // 2026-09-13：objects 仍在白名单但当前重定向指向 release-assets）
  "release-assets.githubusercontent.com",
  "avatars.githubusercontent.com",
  "gist.githubusercontent.com",
  "github-releases.githubusercontent.com",
  "media.githubusercontent.com",
];

/** 默认集 + WHITELIST env 增补（逗号分隔，只增不删）。 */
export function allowedHosts(extra?: string): Set<string> {
  const set = new Set<string>();
  for (const h of DEFAULT_ALLOWED_HOSTS) set.add(h);
  if (extra) {
    for (const h of extra.split(",")) {
      const t = h.trim().toLowerCase();
      if (t) set.add(t);
    }
  }
  return set;
}

/**
 * 从 Worker 请求的 pathname+search 提取目标 URL。
 * 防御规则（任一不满足即 null → 403）：
 *  - 必须以 https:// 开头（拒绝 //host、http:、相对路径）
 *  - 无 userinfo（user@pass 形式的解析歧义）
 *  - 端口为空或 443
 *  - host 在白名单（大小写不敏感）
 */
export function parseTarget(pathAndSearch: string, allowed: Set<string>): URL | null {
  let raw = pathAndSearch;
  if (raw.startsWith("/")) raw = raw.slice(1);
  if (!/^https:\/\//i.test(raw)) return null;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  if (u.username !== "" || u.password !== "") return null;
  if (u.port !== "" && u.port !== "443") return null;
  if (!allowed.has(u.hostname.toLowerCase())) return null;
  return u;
}
