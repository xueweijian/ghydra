import { beforeEach, describe, expect, it } from "vitest";
import { handleRequest, type Deps, type Env } from "../src/index";
import type { FetchLike } from "../src/fetchloop";

// ---------- 测试设施：路由表 fake fetch + 记录式 fake cache ----------

interface Route {
  match: (url: URL, init: RequestInit | undefined) => boolean;
  reply: (url: URL, init: RequestInit | undefined) => Response;
}

class FakeFetch {
  calls: { url: string; init: RequestInit | undefined }[] = [];
  constructor(private routes: Route[]) {}
  fetchLike: FetchLike = async (input, init) => {
    this.calls.push({ url: input, init });
    const u = new URL(input);
    for (const r of this.routes) {
      if (r.match(u, init)) return r.reply(u, init);
    }
    throw new Error(`no route for ${input}`);
  };
}

class FakeCache {
  store = new Map<string, Response>();
  async match(key: string | Request): Promise<Response | undefined> {
    const k = typeof key === "string" ? key : key.url;
    return this.store.get(k);
  }
  async put(key: Request | string, resp: Response): Promise<void> {
    const k = typeof key === "string" ? key : key.url;
    // 读出 body 供后续 match 返回
    const body = await resp.clone().text();
    this.store.set(k, new Response(body, { status: resp.status, headers: resp.headers }));
  }
}

const ctx = { waitUntil: (p: Promise<unknown>) => pending.push(p) };
const pending: Promise<unknown>[] = [];
async function flush(): Promise<void> {
  await Promise.all(pending.splice(0));
}

function call(env: Env, deps: Deps, path: string, init?: RequestInit) {
  return handleRequest(new Request(`https://w.example.com${path}`, init), env, ctx, deps);
}

const zipURL = "/https://github.com/rclone/rclone/releases/download/v1.67.0/rclone-v1.67.0-linux-arm64.zip";
const zipBytes = "PK-ZIP-CONTENT".repeat(100);
const zipTotal = zipBytes.length;

// Range 请求 → 206 语义
function rangedOrFull(u: URL, init: RequestInit | undefined): Response {
  const rng = init?.headers ? new Headers(init.headers as HeadersInit).get("range") : null;
  if (rng === "bytes=100-") {
    const part = zipBytes.slice(100);
    return new Response(part, {
      status: 206,
      headers: {
        "content-type": "application/zip",
        "content-length": String(part.length),
        "content-range": `bytes 100-${zipTotal - 1}/${zipTotal}`,
        etag: '"0x8DC8C925FC96FFF"',
      },
    });
  }
  return ok200();
}

function ok200(body = zipBytes): Response {
  return new Response(body, {
    status: 200,
    headers: {
      "content-type": "application/zip",
      "content-length": String(body.length),
      etag: '"0x8DC8C925FC96FFF"',
      "content-disposition": 'attachment; filename="rclone.zip"',
    },
  });
}

// ---------- 用例 ----------

describe("handleRequest 协议透传", () => {
  let ff: FakeFetch;
  beforeEach(() => {
    ff = new FakeFetch([
      {
        match: (u) => u.hostname === "github.com" && u.pathname.startsWith("/rclone/"),
        reply: (u: URL, init: RequestInit | undefined) => rangedOrFull(u, init),
      },
    ]);
  });

  it("200 直通：ETag/Content-Length/Content-Disposition 透传，无缓存头", async () => {
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL);
    expect(r.status).toBe(200);
    expect(r.headers.get("etag")).toBe('"0x8DC8C925FC96FFF"');
    expect(r.headers.get("content-length")).toBe(String(zipBytes.length));
    expect(r.headers.get("content-disposition")).toContain("rclone.zip");
    expect(r.headers.get("x-ghydra-cache")).toBeNull();
  });

  it("Range 进 → 206 + Content-Range 出", async () => {
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL, {
      headers: { range: "bytes=100-" },
    });
    expect(r.status).toBe(206);
    expect(r.headers.get("content-range")).toBe(`bytes 100-${zipTotal - 1}/${zipTotal}`);
    const sent = ff.calls[0].init?.headers as Headers;
    expect(new Headers(sent).get("range")).toBe("bytes=100-");
  });

  it("上游 404 语义原样透传", async () => {
    const f404 = new FakeFetch([
      { match: (u) => u.hostname === "github.com", reply: () => new Response("Not Found", { status: 404 }) },
    ]);
    const r = await call({}, { fetchLike: f404.fetchLike, cache: null }, zipURL);
    expect(r.status).toBe(404);
  });

  it("HEAD：上游也是 HEAD，客户端响应无 body", async () => {
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL, { method: "HEAD" });
    expect(r.status).toBe(200);
    expect(r.headers.get("etag")).not.toBeNull();
    expect(await r.text()).toBe("");
    expect(ff.calls[0].init?.method).toBe("HEAD");
  });

  it("非 GET/HEAD → 405", async () => {
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL, { method: "POST" });
    expect(r.status).toBe(405);
  });

  it("白名单外目标 → 403", async () => {
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, "/https://evil.com/x");
    expect(r.status).toBe(403);
    expect(ff.calls.length).toBe(0);
  });
});

describe("handleRequest 重定向循环", () => {
  it("302 → objects 签名 URL 内部跟随，第二跳保留 Range", async () => {
    const signed = "/rclone/signed-asset?X-Amz-Signature=abc";
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "github.com" && u.pathname.startsWith("/rclone/"),
        reply: () =>
          new Response(null, {
            status: 302,
            headers: { location: `https://objects.githubusercontent.com${signed}` },
          }),
      },
      {
        match: (u) => u.hostname === "objects.githubusercontent.com",
        reply: () =>
          new Response("RESUMED-BYTES", {
            status: 206,
            headers: { "content-range": `bytes 100-${100 + "RESUMED-BYTES".length}/${zipBytes.length}`, etag: '"E1"' },
          }),
      },
    ]);
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL, {
      headers: { range: "bytes=100-" },
    });
    expect(r.status).toBe(206);
    expect(await r.text()).toBe("RESUMED-BYTES");
    // 第二跳带原 Range
    expect(ff.calls[1].url).toContain("objects.githubusercontent.com");
    expect(new Headers(ff.calls[1].init?.headers as HeadersInit).get("range")).toBe("bytes=100-");
  });

  it("重定向到白名单外 host → 502，不发起第二跳", async () => {
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "github.com",
        reply: () => new Response(null, { status: 302, headers: { location: "https://evil.com/x" } }),
      },
    ]);
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL);
    expect(r.status).toBe(502);
    expect(ff.calls.length).toBe(1);
  });

  it("超过 5 跳 → 502", async () => {
    let n = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "github.com",
        reply: () => new Response(null, { status: 302, headers: { location: `https://github.com/hop${++n}` } }),
      },
    ]);
    const r = await call({}, { fetchLike: ff.fetchLike, cache: null }, zipURL);
    expect(r.status).toBe(502);
    expect(ff.calls.length).toBe(5);
  });
});

describe("handleRequest TOKEN 鉴权", () => {
  const ff = new FakeFetch([
    { match: (u) => u.hostname === "github.com", reply: () => ok200() },
  ]);
  it("无 token → 401；query 对 → 200；错 → 401", async () => {
    const env = { TOKEN: "s3cret" };
    expect((await call(env, { fetchLike: ff.fetchLike, cache: null }, zipURL)).status).toBe(401);
    expect((await call(env, { fetchLike: ff.fetchLike, cache: null }, zipURL + "?token=wrong")).status).toBe(401);
    expect((await call(env, { fetchLike: ff.fetchLike, cache: null }, zipURL + "?token=s3cret")).status).toBe(200);
  });
  it("Bearer 形式也可", async () => {
    const env = { TOKEN: "s3cret" };
    const r = await call(env, { fetchLike: ff.fetchLike, cache: null }, zipURL, {
      headers: { authorization: "Bearer s3cret" },
    });
    expect(r.status).toBe(200);
  });
});

describe("handleRequest 缓存", () => {
  const sha = "0123456789abcdef0123456789abcdef01234567";
  const rawURL = `/https://raw.githubusercontent.com/xueweijian/ghydra/${sha}/README.md`;

  it("SHA-pinned raw：首次回源，二次命中 x-ghydra-cache: hit 且不再回源", async () => {
    let upstreamCalls = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "raw.githubusercontent.com",
        reply: () => {
          upstreamCalls++;
          return new Response("# readme", {
            status: 200,
            headers: { "content-type": "text/plain; charset=utf-8", "content-length": "8", etag: '"R1"' },
          });
        },
      },
    ]);
    const cache = new FakeCache();
    const deps = { fetchLike: ff.fetchLike, cache: cache as unknown as Cache };
    const r1 = await call({}, deps, rawURL);
    expect(r1.status).toBe(200);
    expect(r1.headers.get("x-ghydra-cache")).toBeNull();
    await flush(); // waitUntil 里的 cache.put 落定
    const r2 = await call({}, deps, rawURL);
    expect(r2.headers.get("x-ghydra-cache")).toBe("hit");
    expect(upstreamCalls).toBe(1);
    expect(await r2.text()).toBe("# readme");
  });

  it("分支 ref（非 SHA）不缓存：二次仍回源", async () => {
    let upstreamCalls = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "raw.githubusercontent.com",
        reply: () => {
          upstreamCalls++;
          return new Response("main-branch", { status: 200, headers: { "content-length": "11" } });
        },
      },
    ]);
    const cache = new FakeCache();
    const deps = { fetchLike: ff.fetchLike, cache: cache as unknown as Cache };
    const u = "/https://raw.githubusercontent.com/xueweijian/ghydra/main/README.md";
    await call({}, deps, u);
    await call({}, deps, u);
    expect(upstreamCalls).toBe(2);
  });

  it("releases/download 永不缓存", async () => {
    let upstreamCalls = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "github.com",
        reply: () => {
          upstreamCalls++;
          return ok200();
        },
      },
    ]);
    const cache = new FakeCache();
    const deps = { fetchLike: ff.fetchLike, cache: cache as unknown as Cache };
    await call({}, deps, zipURL);
    await call({}, deps, zipURL);
    expect(upstreamCalls).toBe(2);
  });

  it("Range 请求不走缓存（206 语义直通）", async () => {
    let upstreamCalls = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "raw.githubusercontent.com",
        reply: () => {
          upstreamCalls++;
          return new Response("PART", {
            status: 206,
            headers: { "content-range": "bytes 0-3/10", "content-length": "4" },
          });
        },
      },
    ]);
    const cache = new FakeCache();
    const deps = { fetchLike: ff.fetchLike, cache: cache as unknown as Cache };
    const r = await call({}, deps, rawURL, { headers: { range: "bytes=0-3" } });
    expect(r.status).toBe(206);
    expect(cache.store.size).toBe(0);
  });

  it("CACHE=off 全禁用", async () => {
    let upstreamCalls = 0;
    const ff = new FakeFetch([
      {
        match: (u) => u.hostname === "raw.githubusercontent.com",
        reply: () => {
          upstreamCalls++;
          return new Response("x", { status: 200, headers: { "content-length": "1" } });
        },
      },
    ]);
    const cache = new FakeCache();
    const deps = { fetchLike: ff.fetchLike, cache: cache as unknown as Cache };
    await call({ CACHE: "off" }, deps, rawURL);
    await call({ CACHE: "off" }, deps, rawURL);
    expect(upstreamCalls).toBe(2);
  });
});
