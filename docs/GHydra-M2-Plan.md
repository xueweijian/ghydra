# GHydra M2 实施方案 · 双通道与降级

> 状态：设计中（2026-09-13 起草）。前置：M1 W1–W4 已交付；W4.5（Windows CI 平台证明）待 CI 验证。
> PRD 映射：§M2「双通道与降级」；退出标准：注入源头 403 后 30s 内自动降级；Release 中位速度 ≥ 2MB/s。

## 0. 背景与硬约束

- **不解密、不建隧道**（PRD 合规声明）：GHydra 不做 MITM（FastGithub 的 CA 签发路线明确排除），不在 CF Workers 上做 TCP relay。这决定了通道 B 的形态 = **URL 前缀反向代理**（gh-proxy 协议），不是转发代理。
- **SNI 改写已密码学证伪**（M0 结论）：TLS 1.2/1.3 均在握手校验爆，任何「把 CONNECT 流量改道 CDN」的透明方案在协议层死路。
- 因此双通道不是「同一流量两条路」，而是**按流量形态分流**——这是 M2 最核心的设计判断，见 D1。

## 1. 设计决策（D1–D6）

### D1 · 通道 B 的服务边界（按流量形态分流）

| 流量形态 | 通道 | 说明 |
|---|---|---|
| 浏览器/API HTTPS（CONNECT 隧道） | **A 专属** | B 无法承接：URL 前缀反代要求 URL 出现在请求路径里，CONNECT 内不可见（除非 MITM，已排除） |
| git clone/fetch/push（HTTPS） | A/B 可切 | `url.insteadOf` 把仓库 URL 重写到 B 前缀——git 层面的 URL 改写，不碰 TLS（D6） |
| Release/归档/raw 下载 | A/B 可切 | `ghydra get` 下载器自主择路 + 吞吐启发 + Range 断点续传切道（D4） |
| serve 明文 HTTP 代理路径 | A/B 可切 | http:// 请求 URL 可见，HTTPHandler 直接 CDN 改写（增量小，W2 做） |
| SSH（22/443） | A 专属（M1 已定） | ssh config 写 `ssh.github.com:443` 属连通性优化，非通道切换 |

**推论**：2025-04 型事件中浏览器场景无 B 兜底是架构事实（同类产品同样如此，PRD 竞品表 L4 行印证）。M2 的 403 抗压覆盖：git 工作流 + 下载流（开发者日流量的主体）。浏览器场景的解在 M3+ 评估（浏览器扩展改写链接 / 或维持现状并在 doctor 里明确归因）。

### D2 · 决策信号：doctor 探针 + 实时流量 + 手动 override

通道决策器（`engine/channel`）的输入三路：
1. **doctor 周期探针**（已有 `engine/probe`）：六场景 + 直连对照组。源头 403 / 全 IP 段 TCP+TLS 死 → 通道 A 整体 quarantine 的触发信号。
2. **实时流量 Event**（已有 `proxy.Event`，扩展字段）：连续熔断/枯竭补充失败率超阈值 → A 降级评估。
3. **手动 override**：`ghydra channel set A|B|auto`——逃生通道，用户显式指定时决策器只透传。

### D3 · 两级熔断的分层：IP 级（sched）与通道级（channel）

- **IP 级**（M1 已有）：sched 五态状态机管单 IP，通道 A 内部实现细节。
- **通道级**（M2 新增）：A 整体 closed/open/half-open 三态。open 判据 = doctor 判定源头级故障（403 特征/全段死）或滑动窗口内 A 失败率 > 阈值。half-open = 探针半开复验。
- 关系：通道级状态只决定「新请求走哪条道」，不干预 sched 内部；A 恢复由 doctor 探针驱动，不由流量驱动（避免用户流量当探针时的体验损耗）。
- 自研（约 150 行），不引 gobreaker——M1 已验证 gobreaker 三态粒度不匹配，通道级需求同样简单。

### D4 · 吞吐启发与下载器

`ghydra get <url>`（新子命令，W2）：
- 默认 A 起步：TTFB > 2s 或前 1MB < 200KB/s 且总量 > 10MB → 切 B，`Range: bytes=N-` 续传（PRD P1 指标：进行中连接断点续传）。
- B 也慢/失败 → 回 A 报告双通道对照（诊断价值）。
- 校验：ETag/Content-Length 前后一致性，防 CDN 缓存错配；SHA256 可选。
- 指标上报进 doctor_log（Release 中位速度 ≥ 2MB/s 的验收数据源）。

### D5 · Worker 模板（TypeScript，W3）

- gh-proxy 协议：`https://<worker>/<完整目标URL>` 前缀反代。
- 域名白名单：github.com / api / codeload / objects / raw / avatars / gist + release assets CDN。
- 流式透传（Workers 原生 Response 流，不整读进内存）；`immutable` 资源自适应缓存（带 hash 的 assets、归档按 ETag）。
- 免费档实测项（M0 ③ 挪来）：100–500MB Release 的 CPU 时间限额行为、100k req/天额度下的真实容量——设计输入而非发布阻塞。
- 一键 Deploy 按钮（PRD 要求）+ wrangler.toml 模板 + 用户自有域名绑定说明（workers.dev 在大陆的可达性波动，README 说明）。

### D6 · Git 集成的安全边界（W4）

- `insteadOf` 写入前快照（复用 store 快照模式，独立 git 快照表/文件）；`ghydra off` / `git restore` 恢复。
- 只动 `url.<base>.insteadOf` 键，不碰用户其他 git config；写前检测用户已有 insteadOf（冲突则报告不覆盖）。
- `ssh.github.com:443` 检测（M1 doctor 已探测）→ 可选写入 `~/.ssh/config` 的 `Host github.com-alt` 段（不修改已有 Host 段）。
- B 前缀 URL 只对 fetch/clone 生效；push 默认仍走 A（PRD：小操作直连）——`insteadOf` 粒度按仓库 remote 协议判断，push 大对象场景由吞吐启发另测。

## 2. 四周分解（节奏不绑日历周，模块完成即推进）

### W1 · 通道决策器 + 故障注入框架（纯本地，无网络依赖）
| 任务 | 交付 | 测试 |
|---|---|---|
| `engine/channel`：Channel 状态机（closed/open/half-open）+ 滑动窗口失败率 + 信号聚合（doctor/流量/override） | `engine/channel/` | L1 状态机全转移 + 并发安全 |
| A/B 适配器接口（`Route(flow) (handler, error)` 形态）+ fake channel | `engine/channel/adapter.go` | 故障注入矩阵 |
| 故障注入框架：fake 403 / TCP 死 / TLS 死 / 慢响应 / DNS 污染（复用 testutil 假上游设施） | `engine/channel/fault/` | 注入 → 30s 内切道断言（退出标准①的 CI 化） |
| doctor 判定信号接入：源头 403 特征 → A quarantine | `engine/probe` → `channel` | 分类器单测 |

### W2 · ghydra get 下载器 + 吞吐启发
| 任务 | 交付 | 测试 |
|---|---|---|
| 下载器核心：择路、TTFB/前 1MB 测速、Range 断点续传切道、双通道对照报告 | cmd `ghydra get` | 本地假上游 L2（慢 A + 快 B 注入） |
| serve 明文 HTTP 路径的 CDN 改写 | `engine/proxy/httphandler.go` | L2 |
| Release 速度指标入 doctor_log | store | 汇总查询 |

### W3 · Worker 模板 + 免费档实测
| 任务 | 交付 | 测试 |
|---|---|---|
| TypeScript Worker（白名单/流式/缓存）+ 一键 Deploy | `worker/` | wrangler dev 本地 + deploy 冒烟 |
| 免费档实测：100/300/500MB 透传 + CPU 限额行为 | 实测报告（docs/） | 需用户 CF 账号（M0 ③ 遗留） |
| B 通道适配器接 worker/gh-proxy 公共端点 | `engine/channel` | L2 + 真网 |

### W4 · Git 集成 + 演练 + v0.5
| 任务 | 交付 | 测试 |
|---|---|---|
| insteadOf 自动化（快照/恢复/冲突检测） | cmd `ghydra git` | 本地 git 仓库 L2 全流程 |
| ssh config 443 检测写入 | cmd | dry-run 断言 |
| 故障演练脚本（注入 403/RST/DNS 污染全矩阵） | `scripts/` | CI 化退出标准① |
| v0.5 beta tag + release | CI | 全绿后 |

## 3. 风险增量

| # | 风险 | 对策 |
|---|---|---|
| M2-R1 | workers.dev 大陆可达性波动 | 支持自定义域名；doctor 加 B 通道探针列 |
| M2-R2 | B 前缀对 GitHub API 类请求语义漂移（重定向/相对路径） | 白名单限下载类域；API 走 A |
| M2-R3 | insteadOf 污染用户 git 环境 | 快照恢复 + 冲突检测 + dry-run |
| M2-R4 | 切道抖动（A half-open 探活反复横跳） | half-open 探针间隔退避 + doctor 驱动恢复 |

## 4. 验收口径

- 退出①（30s 降级）：CI 故障注入矩阵绿（fake 源头 403 → 新请求走 B ≤30s）。
- 退出②（2MB/s）：真机关魔法 `ghydra get` Release 实测，中位数入报告（与 direct 对照）。
- v0.5 发布前置：W4.5 Windows CI 持续绿 + 现场验收清单通过（借 Windows 真机时一并做）。
