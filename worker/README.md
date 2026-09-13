# ghydra-worker

GHydra B 通道 Cloudflare Worker：GitHub 白名单反代（[gh-proxy](https://gh-proxy.com) 协议兼容），流式透传 + 保守缓存 + 断点续传友好。

## 为什么自建

- 公共实例（gh-proxy.com 等）速率波动大（实测 0.26–4.04 MB/s），免费档配额是他人账号的全局额度
- 自建 = 免费档 100k 请求/天独享 + 代码可审计（本模板 ~300 行 TypeScript）
- workers.dev 默认域名在大陆可达性不稳（SNI 阻断），**建议绑定自定义域**（见下文）

## 部署（二选一）

### 方式一：一键 Deploy（推荐）

[![Deploy to Cloudflare Workers](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/xueweijian/ghydra/tree/main/worker)

授权后自动 fork + 部署，全程 <1 分钟。

### 方式二：wrangler CLI

```bash
cd worker
npm install
npx wrangler login
npx wrangler deploy
```

## 绑定自定义域（大陆可达性关键步骤）

workers.dev 域名在大陆常被 SNI 阻断。托管在 Cloudflare 的域名可绑定 Custom Domain（走你自己的域，通常可达）：

1. Dashboard → Workers & Pages → `ghydra-worker` → Settings → Domains & Routes → **Add → Custom Domain**
2. 填入你的域名（如 `gh.yourdomain.com`），自动生成证书与路由
3. 或在 `wrangler.toml` 取消 `routes` 注释后重新 deploy

> 注意：免费版走 CF 全球网络（非 China Network），大陆访问的是海外边缘——可达性通常好于 workers.dev，延迟一般 100–300ms，不是国内 CDN 级速度。

验证：`curl -I https://gh.yourdomain.com/https://github.com/` 应返回 `200`。

## 接入 GHydra

```bash
# 下载器直接指定 B 前缀
ghydra get https://github.com/o/r/releases/download/v1/x.zip --cdn https://gh.yourdomain.com/

# 常驻代理
ghydra serve --cdn https://gh.yourdomain.com/
```

## 配置（wrangler.toml [vars]）

| 变量 | 默认 | 说明 |
|---|---|---|
| `WHITELIST` | 空 | 逗号分隔增补 host（只增不删默认十域） |
| `TOKEN` | 空 | 防配额被蹭。建议 `wrangler secret put TOKEN`（加密存储，不进 repo）。客户端两种用法：①**前缀形态** `https://<worker>/t/<TOKEN>/`（ghydra `--cdn` 直接可用）②`Authorization: Bearer <TOKEN>`（worker 消费后不转发源站，GitHub 私有库 token 语义不受影响） |
| `CACHE` | `on` | `off` 全禁用缓存 |

## 行为契约（与 GHydra 引擎的协议）

- **协议**：`GET https://<worker>/<完整目标URL>`；目标必须 `https://` + 白名单 host + 443
- **流式**：响应体直通不整读（大文件透传不撞 128MB 内存/CPU 限额）
- **Range**：`Range` 原样进、`206 + Content-Range` 原样出（GHydra 断点续传切道依赖）
- **一致性头**：`ETag / Content-Length / Content-Range / Accept-Ranges / Content-Disposition` 透传（GHydra 跨通道一致性守卫依赖）
- **重定向**：内部手动跟随 ≤5 跳（Release 下载 302 → objects 签名 URL 由 Worker 消化，客户端不出 CF），每跳重新校验白名单
- **缓存**（保守 V1）：只缓存内容语义不可变路径——`raw.githubusercontent.com` 的 40-hex SHA 引用、带 `v=` 参数的头像；`/releases/download/`、codeload、API、HTML 一律不缓存（GitHub 允许同名资产替换，URL 稳定 ≠ 内容稳定）。命中响应带 `x-ghydra-cache: hit`

## 白名单（默认）

`github.com` · `api.github.com` · `codeload.github.com` · `gist.github.com` · `raw.githubusercontent.com` · `objects.githubusercontent.com` · `avatars.githubusercontent.com` · `gist.githubusercontent.com` · `github-releases.githubusercontent.com` · `media.githubusercontent.com`

## 本地开发与测试

```bash
npm install
npm test          # vitest（纯逻辑，依赖全注入）
npm run typecheck # tsc --noEmit
npm run dry-run   # wrangler deploy --dry-run（配置校验，不上传）
```

## 免费额度（2026 实测前参考值）

| 项 | 免费档 |
|---|---|
| 请求数 | 100k / 天（账号全局） |
| CPU | 10ms / 请求（流式等待不计，仅 JS 执行计） |
| 带宽 | 无硬性限额（公平使用） |

大文件透传实测数据见 `docs/GHydra-M2-W3-*`（W3-D 交付）。
