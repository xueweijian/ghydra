// GHydra Worker 入口：白名单反代 GitHub（gh-proxy 协议兼容）。
// 关键不变量：
//  1. body 流式直通（JS 不读取——Workers 只计 CPU 不计等待，整读会撞内存/CPU 限额）
//  2. ETag/Content-Length/Content-Range/Accept-Ranges 必须透传
//     （ghydra 下载器的一致性守卫与断点续传依赖它们）
//  3. Range 原样进、206+Content-Range 原样出
// 缓存/依赖全部注入化（deps），生产环境注入 caches.default 与全局 fetch，
// 测试注入 fake——纯逻辑可测，不依赖 workers 专属 mock 设施。

import { allowedHosts, parseTarget } from "./allowlist";
import { fetchWithRedirects, HopError, type FetchLike } from "./fetchloop";
import { isCacheable, CACHE_MAX_BYTES } from "./cache";

export interface Env {
  /** 逗号分隔增补 host（只增不删默认集）。 */
  WHITELIST?: string;
  /** 设置后要求 ?token= 或 Authorization: Bearer 匹配（防自有实例被蹭配额）。 */
  TOKEN?: string;
  /** "on"（默认）/ "off"。 */
  CACHE?: string;
}

export interface Deps {
  fetchLike?: FetchLike;
  cache?: Cache | null; // null = 禁用（测试）
}

const PASS_REQ_HEADERS = ["range", "if-range", "authorization", "user-agent", "accept"];
const PASS_RESP_HEADERS = [
  "content-type",
  "content-length",
  "content-range",
  "accept-ranges",
  "etag",
  "last-modified",
  "cache-control",
  "content-disposition",
];

function jsonError(status: number, msg: string): Response {
  return new Response(JSON.stringify({ error: msg }), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

export async function handleRequest(
  request: Request,
  env: Env,
  ctx: Pick<ExecutionContext, "waitUntil">,
  deps: Deps = {},
): Promise<Response> {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return jsonError(405, "只支持 GET/HEAD");
  }

  // 可选鉴权
  if (env.TOKEN) {
    const url = new URL(request.url);
    const q = url.searchParams.get("token");
    const auth = request.headers.get("authorization");
    const bearer = auth?.startsWith("Bearer ") ? auth.slice(7) : null;
    if (q !== env.TOKEN && bearer !== env.TOKEN) {
      return jsonError(401, "token 不匹配");
    }
  }

  // 目标解析（pathname+search 合并：目标自带 query 时 search 属于目标 URL）
  const u = new URL(request.url);
  const target = parseTarget(u.pathname + u.search, allowedHosts(env.WHITELIST));
  if (!target) return jsonError(403, "目标 URL 不在白名单或形态非法");

  const cacheEnabled = env.CACHE !== "off" && deps.cache !== null;
  const cacheable = cacheEnabled && request.method === "GET" && !request.headers.has("range") && isCacheable(target);

  // 缓存命中
  if (cacheable && deps.cache) {
    const hit = await deps.cache.match(target.toString());
    if (hit) {
      const h = new Headers(hit.headers);
      h.set("x-ghydra-cache", "hit");
      return new Response(hit.body, { status: hit.status, headers: h });
    }
  }

  // 上游请求头（白名单透传）
  const upHeaders = new Headers();
  for (const k of PASS_REQ_HEADERS) {
    const v = request.headers.get(k);
    if (v) upHeaders.set(k, v);
  }

  const fetchLike = deps.fetchLike ?? ((input: string, init?: RequestInit) => fetch(input, init));
  try {
    const { resp } = await fetchWithRedirects(target, fetchLike, {
      method: request.method,
      headers: upHeaders,
      isAllowedHost: (h) => allowedHosts(env.WHITELIST).has(h),
    });

    // 响应头白名单透传
    const h = new Headers();
    for (const k of PASS_RESP_HEADERS) {
      const v = resp.headers.get(k);
      if (v) h.set(k, v);
    }

    // 缓存写入：只对 200 + content-length 已知且 ≤ 上限
    if (cacheable && deps.cache && resp.status === 200) {
      const len = Number(resp.headers.get("content-length") ?? "0");
      if (len > 0 && len <= CACHE_MAX_BYTES) {
        const key = new Request(target.toString(), { method: "GET" });
        ctx.waitUntil(deps.cache.put(key, resp.clone()));
      }
    }

    return new Response(request.method === "HEAD" ? null : resp.body, { status: resp.status, headers: h });
  } catch (e) {
    if (e instanceof HopError) return jsonError(502, e.message);
    const msg = e instanceof Error ? e.message : String(e);
    return jsonError(502, `上游请求失败: ${msg.slice(0, 128)}`);
  }
}

export default {
  fetch: (request: Request, env: Env, ctx: ExecutionContext) => handleRequest(request, env, ctx),
};
