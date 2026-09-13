# GHydra W3-D · Worker 免费档实测报告

> **日期**：2026-09-13（关魔法窗口，大陆移动网络直连）
> **实例**：用户自有 CF 账号，`ghydra-worker` @ `gh.1ciyuan.cn`（Custom Domain）
> **结论先行**：✅ 流式透传免大文件死刑——860MB 完整交付、CPU P99 2.8ms（限额 10ms）、零错误；免费档对个人/小团队自建完全够用。

## 1. 吞吐矩阵（ghydra get，经自有 Worker，A 段全失败被切道）

| 资产 | 大小 | run1 | run2 | run3 | 备注 |
|---|---|---|---|---|---|
| BtbN/FFmpeg-Builds `ffmpeg-master-latest-linuxarm64-gpl.tar.xz` | 122 MB | **7.69 MB/s** (15.9s) | 1.18 MB/s (103.4s) | 3.95 MB/s (31.0s) | run2 撞边缘波动 |
| llvm-project `clang+llvm-23.1.1-x86_64-pc-windows-msvc.tar.zst` | 467 MB | 5.73 MB/s (81.4s) | **8.77 MB/s** (53.3s) | 8.27 MB/s (56.5s) | 大文件更稳 |
| llvm-project `clang+llvm-23.1.1-x86_64-pc-windows-msvc.tar.xz` | 860 MB | 7.47 MB/s (115.1s) | 8.26 MB/s (104.1s) | 8.07 MB/s (106.5s) | 三跑方差 <3% |

- 汇总（本地 doctor_log，2h 窗口）：**B 通道中位 8.20 MB/s、峰值 20.70 MB/s、累计 4.37 GB**；A 通道 12/12 失败（直连全阻断窗口，切道正确工作）。
- 对照：公共 gh-proxy.com 同期实测 0.26–4.04 MB/s（W2/W3 两窗口）——**自建实例稳定占优**。
- 对照 PRD 口径：Release 中位 ≥2MB/s → **4.1× 超额达成**。

## 2. CPU 限额（Workers analytics GraphQL `workersInvocationsAdaptive`）

| 指标 | 实测 | 免费档限额 | 判定 |
|---|---|---|---|
| cpuTime P50 | 399 µs | — | 白名单拒绝/小响应近乎零成本 |
| cpuTime P99 | **2 825 µs** | 10 000 µs/req | **28% 限额，860MB 流式透传不涨 CPU** |
| errors | 0 / 293 req | — | — |

**设计假设"body 流式直通不计 CPU"实测成立**——JS 不读 body， Workers 只计 JS 执行时间。500MB+ 大文件透传无 CPU 风险。

## 3. 配额消耗模型（100k req/天）

- 单次下载 = 1 个入站请求 + 2 个 subrequest（302 两跳）；请求数按入站计。
- 实测窗口：8 次大下载 + doctor + 调试请求 ≈ 300 请求（含公网爬虫对自定义域的零星扫描，白名单直接 403，cpuP50 可见成本可忽略）。
- **重度个人使用（50 次 Release/天）≈ 配额的 0.05%**。多设备小团队（<10 人）同样无压力。
- subrequest 2/req，远低于 50/req 上限。

## 4. 稳定性与可达性

- `gh.1ciyuan.cn`（Custom Domain，CF 全球边缘）大陆直连 100% 可达（本窗口全部请求成功）；workers.dev 主域名在大陆的 SNI 阻断因此完全绕开。
- 波动观察：122MB run2 掉到 1.18 MB/s（单次边缘节点抖动），467/860MB 档反而更稳（长连接摊薄握手/抖动）——**下载越大，Worker 路径越划算**。
- 302 内部跳转：github.com → `release-assets.githubusercontent.com`（2026 现役资产域名，白名单已锁定该跳）→ 签名 URL 透传下载，全链路 Worker 内消化，客户端零重定向暴露。

## 5. 工程结论与遗留

1. **免费档不阻塞发布**：个人/小团队自建在吞吐、CPU、配额三维全绿，M0-③ 遗留实验正式关闭。
2. **建议个人实例设 `TOKEN`**：公网爬虫会扫到自定义域（本窗口已观测），白名单兜底零风险但 TOKEN 可杜绝配额被蹭（README 已写）。
3. 未做：Workers Paid 档对比（无付费动机，免费档结论已够）；中国网络专化路由（CF China Network 需企业版，非目标用户群）。
4. 已知限制记录：`/releases/download/` 不缓存（资产可同名替换）；多 CDN 边缘波动靠客户端 `ghydra get` 的 Range 续传吸收。

---
*数据来源：ghydra doctor_log（SQLite，`/tmp/w3.db` 快照）+ Cloudflare GraphQL analytics + 本地计时；脚本与命令见 git history（W3 系列提交）。*
