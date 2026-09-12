# GHydra M2 W3 设计 · Worker 模板 + B 通道硬化

> **用途**：W3（M2-Plan §2 第 3 周）开工前细化设计。输入 = M2-Plan D5/D6 + W2 真网验收的两个发现（B 通道 DNS 单点故障、gh-proxy.com 公共实例速率波动 0.26–4.04 MB/s）。
> **状态**：草案（2026-09-13），待拍板后编码。
> **约束**：沙箱禁 npm 大依赖——本地只写代码，Worker 验证全走 CI；真网实测需用户 CF 账号 + 关魔法窗口。

---

## 0. 两句话总结

W2 真网验收证明了双通道战略成立，也暴露了两个 B 通道弱点：**公共实例不可控（速率波动 15 倍）**和**系统 DNS 单点故障（resolv.conf 被魔法写成 1.1.1.1 时 B 直接死）**。W3 就是把 B 通道从"借别人的"变成"自己的"：自建 Worker（可控、可缓存、可白名单）+ DoH 自举（不依赖系统 DNS）。

## 1. 回顾：W2 真网验收输入（2026-09-13 关魔法窗口）

| 发现 | W3 对策 |
|---|---|
| A 全阻断时 doctor 经隧道 4/5，但 `get` 两次 A dial timeout——**探测通过≠连接活** | 不改 W3 范围（调度器粘性/熔断已是 W1 交付），实测数据留给 W4 演练脚本 |
| **B 通道死于 DNS**：`lookup gh-proxy.com on 1.1.1.1 timeout`（魔法重开又写坏 resolv.conf） | **§3 BFetcher DoH 自举**——B 通道完全不依赖系统 DNS |
| B 速率 0.26 → 4.04 MB/s，公共 CDN 波动 15 倍 | **§2 自建 Worker**——自有实例 + 可选缓存 |
| 跨通道 ETag 一致性守卫工作正常 | Worker 必须透传 ETag/Content-Length/Content-Range（进测试） |
| `get --stats` 过滤 rate>0，A 失败段不可见 | 顺手项：stats 加尝试/失败计数列 |

## 2. Worker 模板（`worker/`，TypeScript）

### 2.1 协议（gh-proxy 兼容）

`GET https://<worker>/https://github.com/o/r/releases/download/v1/x.zip` —— 完整目标 URL 原样拼接在 Worker 域名后。`#` 分隔符也接受（gh-proxy 兼容）。响应流式透传。

**关键设计——重定向内部跟随（手动循环）**：Release 下载真实路径是 `github.com → 302 → objects.githubusercontent.com`（签名 URL）。客户端直接跟随会撞向死域名，所以 Worker 必须自己消化跳转：`fetch(url, {redirect:"manual"})`，3xx 时取 `Location` **校验白名单后**以原请求头（含 Range）续跳，≤5 跳。不用 fetch 默认 follow——Range 头在 redirect 中的保留行为属实现细节，手动循环语义自控。

**Range 透传**：ghydra 的断点续传切道（A→B Range 续传）依赖 `Range` 进、`206 + Content-Range` 出。Worker 只转发不解析。进 vitest 断言。

### 2.2 白名单（安全边界）

默认 host 集：`github.com / api.github.com / codeload.github.com / gist.github.com / raw.githubusercontent.com / objects.githubusercontent.com / avatars.githubusercontent.com / gist.githubusercontent.com / github-releases.githubusercontent.com / media.githubusercontent.com`。

URL 解析防御（拒绝即 403 + 一行日志）：非 `https:` scheme、带 userinfo（`user@host`）、非 443 端口、host 不在白名单、`//` 开头路径。`WHITELIST` env 可增补（逗号分隔），不可清空默认集。

可选 `TOKEN` env：设置后要求 `?token=` 或 `Authorization` 匹配，防自有实例被蹭配额（免费档 100k req/天是全局的）。默认不开（个人部署 + 白名单已够）。

### 2.3 流式与资源模型

`return new Response(resp.body, {status, headers})` —— body 是 ReadableStream 直通，**JS 不逐块读**（Workers 计费只算 CPU 时间不算等待，流直通近乎零 CPU；整读会撞 128MB 内存 + CPU 限额，500MB 透传就死）。这是免费档实测能过 100MB+ 的理论依据，实测验证之。

### 2.4 缓存（保守 V1，env `CACHE=on` 默认开）

| 路径 | 缓存? | 理由 |
|---|---|---|
| `raw.githubusercontent.com/<u>/<r>/<40-hex-sha>/...` | ✅ | git 语义：SHA 引用内容不可变，stale 零风险 |
| `avatars.githubusercontent.com`（带 `v=` 版本参数） | ✅ | 参数即版本，同上 |
| `/releases/download/` | ❌ | GitHub 允许同名资产替换——URL 稳定≠内容稳定，stale 风险 > 收益（单用户场景收益本就小） |
| codeload 归档 / API / HTML | ❌ | 归档每次生成；API 语义不能 stale。V2 若实测有收益再上 ETag revalidation |

缓存键 = 请求 URL，`cache.put` 只对 200（不缓存 206/3xx）。命中响应带 `X-GHydra-Cache: hit`。

### 2.5 工程结构

```
worker/
  src/index.ts        # 入口：路由 → 白名单 → 重定向循环 → 流式响应 → 缓存
  src/allowlist.ts    # 白名单 + URL 防御解析（纯函数，单测重点）
  src/fetchloop.ts    # 重定向手动跟随（纯逻辑）
  test/index.test.ts  # vitest + @cloudflare/vitest-pool-workers（真实 workerd 运行时）
  wrangler.toml       # compatibility_date / vars / 占位 routes
  README.md           # 一键 Deploy 按钮 + 自定义域绑定 + workers.dev 大陆可达性说明（M2-R1）
```

一键 Deploy：README 置顶 `https://deploy.workers.cloudflare.com/?url=<repo>/worker` 链接（PRD 要求）。

## 3. B 通道 DoH 自举（Go 侧，`engine/get`）

现状：`BFetcher` 注释明说"CDN 侧走系统网络"——W2 真网证明这是单点故障。

改造：
1. BFetcher 增加 `resolver`（默认 = bootstrap DoH alidns/doh.pub **IP 直连**（M0 已有 dohDialMap 基建）+ 系统 DNS 兜底）。
2. 解析结果带 TTL 内存缓存；TTL 过半或 dial 失败 → 刷新；负结果缓存 30s 防雪崩。
3. 拨号 IP 直连 + `TLSClientConfig.ServerName`/`Host` 保持 CDN 域名语义（证书/SNI 不变，同 M0 dohDialMap 手法）。
4. L2 测试：系统 DNS 指向黑洞（127.0.0.1:53）时 B 通道仍可用（fake DoH + fake CDN origin 注入）；DoH 也死时回退系统 DNS 行为不回退崩溃。

## 4. doctor B 探针列 + trip 前置检查（M2-R1 对策）

W1 的 Verdict 蒸馏只有 direct/proxy 两列；NetworkFault → trip A→B 时**不检查 B 是否活着**——往死通道里切是真实缺陷。

1. doctor 增加 B 列：经 B 前缀对白名单小资源（如 `github.com/favicon.ico`）发 HEAD，记状态/时延/分类进 doctor_log。
2. Router trip 前消费最近一次 B 探针结果：B 死 → 保持 A（记日志），B 活 → 照切。
3. 蒸馏信号升级：`B dead + direct dead + proxy dead` → 全阻断（GFW 全杀）报告形态，不盲目 trip。

## 5. CI 验证（沙箱零 npm）

新增独立 job `worker`（仅 ubuntu，不进三平台矩阵）：`setup-node 20` → `npm ci`（wrangler + vitest + vitest-pool-workers，~200MB 在 CI 不心疼）→ `npx vitest run` + `npx wrangler deploy --dry-run`（配置校验）。Go 矩阵照旧。

## 6. 免费档实测矩阵（需用户 CF 账号 + 关魔法窗口）

| 项 | 方法 |
|---|---|
| 部署 | 用户 dashboard 一键 Deploy 或 CI 注入 `CLOUDFLARE_API_TOKEN` secret 自动 deploy |
| 吞吐 | 10/100/300/500MB Release 资产各 ×3 次，`ghydra get --cdn <自有worker>` 关魔法实测 |
| CPU 限额 | Workers dashboard 实况 + 每请求 CPU ms，验证"流式不计 CPU" |
| 配额 | 100k req/天下的单用户余量估算 |
| 可达性 | workers.dev 大陆直连成功率；不行 → 自定义域绑定路径（README 已备） |
| 对照 | 同窗口 gh-proxy.com vs 自建 worker 速率对照，数据进 docs/ |

退出：≥100MB 完整透传（或实测出上限并文档化为设计输入）。

## 7. 任务分解与顺序（本地三轮 + 一个用户窗口）

| # | 任务 | 依赖网络 | 验收 |
|---|---|---|---|
| 1 | Worker 源码 + vitest + wrangler.toml + README（本地写） | 否 | CI worker job 绿 |
| 2 | Go：BFetcher DoH 自举 + L2 测试 | 否 | DNS 黑洞测试绿 |
| 3 | Go：doctor B 列 + trip 前置检查 + `get --stats` 尝试/失败列 | 否 | 全矩阵 CI 绿 |
| 4 | 真网窗口：deploy + 免费档矩阵 + 自建 worker 真网下载 | 用户 CF + 关魔法 | §6 退出 |

## 8. 风险

| # | 风险 | 对策 |
|---|---|---|
| W3-R1 | vitest-pool-workers 在 CI 的 workerd 兼容性抖动 | 锁定版本；若不稳降级 miniflare 手搭 |
| W3-R2 | workers.dev 大陆可达性差 | 自定义域说明 + doctor B 列持续观测 |
| W3-R3 | 一键 Deploy 按钮对 monorepo 子目录路径支持 | deploy.workers.cloudflare.com 支持 `?url=` 指向子目录（README 验证）；不行给 wrangler 手动三步 |
| W3-R4 | 签名 URL 跳转带 `Location` 回源（objects 域若也被墙，Worker 出口在 CF 边缘不受影响） | Worker 内部跳转在 CF 侧发起，大陆可达性与源无关——理论安全，实测确认 |
