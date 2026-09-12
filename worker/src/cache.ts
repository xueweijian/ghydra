// 保守缓存 V1：只缓存「内容按语义不可变」的路径。
//  - raw.githubusercontent.com/<u>/<r>/<40-hex-sha>/…：git 语义，SHA 引用不可变
//  - avatars.githubusercontent.com?…&v=<n>：版本参数即内容版本
// 不缓存 /releases/download/（GitHub 允许同名资产替换，URL 稳定≠内容稳定）、
// codeload 归档（每次生成）、API、HTML。V2 若实测有收益再上 ETag revalidation。

const SHA_RE = /^[0-9a-f]{40}$/i;

export const CACHE_MAX_BYTES = 50 * 1024 * 1024;

export function isCacheable(target: URL): boolean {
  const h = target.hostname;
  if (h === "raw.githubusercontent.com") {
    const parts = target.pathname.split("/").filter(Boolean);
    return parts.length >= 4 && SHA_RE.test(parts[2]);
  }
  if (h === "avatars.githubusercontent.com") {
    return target.searchParams.has("v");
  }
  return false;
}
