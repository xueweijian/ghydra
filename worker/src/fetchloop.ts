// 重定向手动跟随：Release 下载真实路径是
// github.com → 302 → objects.githubusercontent.com（签名 URL）。
// 客户端直接跟随会撞向死域名，Worker 必须自己消化跳转；
// 每跳重新校验白名单，且 Range 等请求头原样保留
// （ghydra 断点续传切道依赖 Range 进、206+Content-Range 出）。

export const MAX_HOPS = 5;
const REDIRECTABLE = new Set([301, 302, 303, 307, 308]);

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface LoopOpts {
  method: string;
  headers: Headers;
  maxHops?: number;
  /** 每跳目标 host 校验（小写）。 */
  isAllowedHost: (h: string) => boolean;
}

export class HopError extends Error {}

/**
 * 手动重定向循环。非 3xx 直接返回最终响应（body 未消费，流式直通）。
 * 3xx 时：释放 body → 解析 Location（支持相对）→ 白名单/形态校验 → 续跳。
 * 303 语义：非 GET/HEAD 方法转 GET（Range 等头保留，续传意图不变）。
 */
export async function fetchWithRedirects(
  target: URL,
  fetchLike: FetchLike,
  opts: LoopOpts,
): Promise<{ resp: Response; hops: string[] }> {
  let u = target;
  let method = opts.method;
  const hops: string[] = [];
  const max = opts.maxHops ?? MAX_HOPS;
  for (;;) {
    const resp = await fetchLike(u.toString(), { method, headers: opts.headers, redirect: "manual" });
    if (!REDIRECTABLE.has(resp.status)) return { resp, hops };
    hops.push(u.hostname);
    await resp.body?.cancel();
    if (hops.length >= max) throw new HopError(`重定向超过 ${max} 跳`);
    const loc = resp.headers.get("location");
    if (!loc) throw new HopError("重定向缺少 Location");
    let next: URL;
    try {
      next = new URL(loc, u);
    } catch {
      throw new HopError(`非法 Location: ${loc.slice(0, 64)}`);
    }
    if (next.protocol !== "https:" || !opts.isAllowedHost(next.hostname.toLowerCase())) {
      throw new HopError(`重定向到非白名单 host: ${next.hostname}`);
    }
    if (next.username !== "" || next.password !== "" || (next.port !== "" && next.port !== "443")) {
      throw new HopError("重定向目标形态可疑");
    }
    if (resp.status === 303 && method !== "GET" && method !== "HEAD") method = "GET";
    u = next;
  }
}
