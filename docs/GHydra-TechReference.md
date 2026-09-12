# GHydra 技术参考地图（Tech Reference Map）
> **用途**：开发任一模块前，先读对应参考项目代码 → 消化「学到什么」→ 按「超越点」实现新方案。
> **维护规则**：每完成一个模块，回来更新此文档（补充实际踩坑 & 最终采用的实现）。
> 关联文档：`/workspace/GHydra-PRD.md`

---

## 0. 总索引表（模块 → 抄谁 → 超越什么）

| # | GHydra 模块 | 首要参考 | 次要参考 | 超越核心点 |
|---|---|---|---|---|
| M1 | 四级自举 DNS | dev-sidecar | Ghips、dnscrypt-proxy | 种子IP带Host头直连meta（自举闭环） |
| M2 | IP 调度器 | dev-sidecar v2.2 | FastGithub | EWMA评分+状态机+同域粘性 |
| M3 | SNI 转发器 | dev-sidecar | FastGithub | 纯TCP不解密、免证书、手写解析 |
| M4 | MITM 增强（默认关） | FastGithub | dev-sidecar | 一键卸载证书、二次确认 |
| M5 | CDN 通道 | cf-ghproxy-worker | gh-proxy(ghproxy) | 自带Worker模板+适配器抽象 |
| M6 | 路由决策/熔断 | sony/gobreaker | dev-sidecar | 熔断+吞吐启发双信号 |
| M7 | Git 集成 | dev-sidecar | 官方git文档 | insteadOf按仓库大小自动分流 |
| M8 | 系统代理接管 | clash-verge-rev | dev-sidecar | PAC自动生成+端口冲突自迁移 |
| M9 | 远程配置/自更新 | dev-sidecar | minisign | ed25519签名+原子替换+回滚 |
| M10 | GUI | — (反例:dev-sidecar) | Watt Toolkit、syncthing(Web UI 模式) | Wails 纯 Go 壳 ≤15MB，GUI只调本地API |
| M11 | 体检引擎 | 自研（无先例） | Ghips测速思路 | 六场景探针+根因分类器 |
| M12 | 众包遥测 | 自研 | — | 匿名哈希、无用户标识 |

---

## 1. 参考项目档案（含 2026 现状，引用前先核对）

### 1.1 Ghips — L0 hosts 择优
- 仓库：`https://github.com/aardio/Ghips`（aardio 语言，379★，**2022-11 后停更**）
- **必读文件**
  - `main.aardio` → `winform.plusUpdateIps.oncommand`：调 `api.github.com/meta` 取 web 段 + `thread.manage` 并发测速 + **`HTTP/1.1 301` 指纹校验**（防假IP）
  - `main.aardio` → `winform.plusUpdateDns.oncommand`：`ip-api.com` 反查 CDN IP + `fsys.hosts.ownCacls()` 夺权 + `fsys.hosts.update()` 写 hosts
  - `main.aardio` → `sys.runAsTask`：开机静默提权（计划任务免 UAC）
  - `lib/config.aardio`：配置持久化的最小样例
- **学到什么**：301 指纹校验思路；单实例 atom 锁；hosts 权限处理
- **教训（引以为戒）**：① 兜底 IP 硬编码会腐烂 ② 无自举 ③ 定时测速写死，无运行时调度 ④ 域名清单缺失（objects/githubassets/api 均无）
- **超越点**：全部由 M1/M2/M3 的架构性设计覆盖

### 1.2 FastGithub — L2 本地DNS+反代（原仓库已 404）
- 仓库：原 `dotnetcore/FastGithub` **已 404**；活跃 fork：`https://github.com/WangGithubUser/FastGitHub`（C#，822★，最后提交 2025-01）
- **必读目录**（fork 的顶层即架构图）
  - `FastGithub.DomainResolve/` — 域名解析 + IP 测速择优（**调度器怎么建池、怎么打分**）
  - `FastGithub.HttpServer/` — 本地假 HTTPS 服务器 + **每主机自签 CA + 动态证书签发**（MITM 的教科书实现）
  - `FastGithub.PacketIntercept/` — 流量拦截层
  - `FastGithub.Http/` — 上游转发
  - `@dnscrypt-proxy/` — 它捆绑的 dnscrypt-proxy（**生产级 DoH/DoT 客户端实现，值得单独读**）
  - `FastGithub.FlowAnalyze/`、`FastGithub.Configuration/`
- **学到什么**：本地 DNS 服务整体形态；CA 动态签发；googleapis CDN 资源替换；其 README 的「合法性声明」章节（合规话术直接借鉴）
- **超越点**：默认走 M3 SNI 转发（不解密），MITM 降为 M4 可选项；Go 单二进制替代 .NET

### 1.3 dev-sidecar — L3 系统代理+拦截器（**最活跃，24k★**）
- 仓库：`https://github.com/docmirror/dev-sidecar`（Node.js/Electron，v2.2.0 发布于 2026-07）
- **必读位置（按 changelog 反推）**
  - `packages/core/` — 核心逻辑（Electron 只是壳）
  - `packages/core/src/config/remote_config.json` — **远程规则热更新**（托管在 Gitee 镜像，搜 commit `401935e`）
  - 搜关键词 `SpeedTester` — IP 测速与按需探测
  - 搜关键词 `sni` — SNI 拦截器（README 原话「github 直连加速通过修改 SNI 实现，感谢 FastGithub 提供思路」；配置形如 `"sni": "g.cn"`）
  - 搜关键词 `fake server` / `h2` — HTTP/2 多路复用假服务器（ALPN 协商）
  - 搜关键词 `nonce` — CSP `strict-dynamic` 绕过（GitHub 站点专用）
  - `@starknt/sysproxy` — 系统代理设置的跨平台原生模块
  - 「DNS服务管理」——DoH/UDP/TCP/TLS 四类 DNS + `forSNI` 专用 DNS 配置
- **v2.2.0 精华参数（直接抄进 M2）**：按需 IP 探测（轮转分配+成功入池+失败永久跳过）；坏 IP 连接超时 15-21s→**7s**；熔断阈值 3 次→**1 次**；成功重置错误计数
- **学到什么**：拦截器中间件架构（proxy/redirect/success/abort）；远程配置；油猴脚本集成
- **教训（引以为戒）**：Electron 带来 147MB 体积和 200MB+ 内存 → 这正是 M10 用 Tauri 的理由
- **超越点**：体积 1/10；默认零遥测；双通道（它没有 CDN 降级）

### 1.4 gh-proxy / ghproxy — L4 CDN 反代（Go）
- 站点：`https://gh-proxy.com`（自称日流量 7T）；开源实现：`WJQSERVER-STUDIO/ghproxy`（Go）
- **学到什么**：多边缘节点策略（Cloudflare v4/v6 + Fastly）；Release/Raw/Archive/头像/API 全域代理路径改写；缓存与自动重定向
- **超越点**：M5 不依赖公共节点，把它的「代理路径改写」逻辑抽成协议适配器，默认指向用户自部署的 Worker

### 1.5 cf-ghproxy-worker — L4 自建节点（Cloudflare Workers）
- 仓库：`https://github.com/Aethersailor/cf-ghproxy-worker`（TypeScript + wrangler）
- **学到什么**：Worker 反代 GitHub 请求的完整代码；「Deploy to Cloudflare Workers」一键部署按钮；缓存规则与白名单
- **超越点**：M5 内置同款模板 + 一键部署向导 + 从 GHydra 端健康探测用户自己的 Worker

### 1.6 Watt Toolkit（原 Steam++）— L3 反代框架（C#）
- 仓库：`https://github.com/BeyondDimension/SteamTools`
- **学到什么**：基于 `YARP.ReverseProxy` 的本地反代组织方式；「加速脚本注入网页」的产品化做法；多站点加速的服务抽象（Steam/Epic/GitHub 共用框架）
- **超越点**：M4 的插件化加速站点（M4 里程碑 HuggingFace/npm/DockerHub）可参考它的多站点模型

### 1.7 GitHub 官方资源（不是项目但必须读）
- `https://api.github.com/meta` — IP 段 + **`domains` 字段（`*.github.com/*.githubassets.com/*.githubusercontent.com`）**，域名清单唯一权威来源，别再手写
- `https://docs.github.com/zh/rest/meta/meta` — meta 接口文档

---

## 2. 逐模块实施卡片（做之前看这里）

### M1 四级自举 DNS
- [ ] 读 Ghips `plusUpdateIps.oncommand`（meta 消费方式）
- [ ] 读 dev-sidecar「DNS服务管理」四类型实现
- [ ] 读 FastGithub `@dnscrypt-proxy/`（生产级 DoH）
- **新实现**：`meta API → DoH(dns.alidns.com/resolve JSON) → SQLite last-good → 二进制内冻结种子(带Host头直连meta重试)`；DoH 端点可配多个轮询
- **验证方法**：把系统 DNS 改成 127.0.0.1，链路仍能取到 IP

### M2 IP 调度器
- [ ] 读 dev-sidecar `SpeedTester` + v2.2.0 调度参数（7s/1次/按需探测）
- [ ] 读 FastGithub `FastGithub.DomainResolve/`（池结构与打分）
- [ ] 用 `sony/gobreaker` 起步，后换自研状态机
- **新实现**：`score = 0.3·RTT_EWMA + 0.5·失败率_EWMA + 0.2·抖动`；状态机 New→Probing→Active⇄Cooldown→Quarantine(5min复活)；同域粘性 60s
- **验证方法**：注入单 IP 故障，观测 7s 内切换且不扩散

### M3 SNI 转发器（核心壁垒）
- [ ] 读 dev-sidecar SNI 拦截器（改写思路）
- [ ] 读 RFC 6066 §3（SNI 扩展格式）+ RFC 8446 §4.1.2（ClientHello）
- **新实现**：Go 手写 ClientHello 解析 ~150 行（record→handshake→extensions→server_name），**不引 utls**（合规可审计）；可选 SNI 改写；纯 `io.Copy` 双向转发不解密
- **验证方法**：Wireshark 证明流量未解密；解析单次 <50µs

### M4 MITM 增强（默认关）
- [ ] 读 FastGithub `FastGithub.HttpServer/`（CA+动态签发）
- [ ] 读 dev-sidecar fake server（HTTP/2 ALPN、CSP nonce）
- **新实现**：开关默认关；开启二次确认；证书一键卸载并校验系统存储无残留

### M5 CDN 通道
- [ ] 读 cf-ghproxy-worker 全部源码（就一个 Worker，很小）
- [ ] 读 gh-proxy 的路径改写规则（站点首页有完整示例矩阵）
- **新实现**：`Channel` 接口抽象（协议适配器）→ 实现 `ghProxyAdapter` / `ownWorkerAdapter`；对用户自部署 Worker 做健康探测与用量上报

### M6 路由决策器
- [ ] 读 `sony/gobreaker` 源码（熔断三态）
- **新实现**：熔断(状态) + 吞吐启发(TTFB>2s 或 前1MB<200KB/s 且目标>10MB) 双信号；403 特征直判切 B 通道

### M7 Git 集成
- [ ] 读 dev-sidecar 修复 git push 错误的 PR/说明
- [ ] 读 git 官方文档：`url.<base>.insteadOf`、`http.sslBackend`、ssh_config 的 `Hostname/Port`
- **新实现**：按仓库大小自动写 insteadOf 分流；探测 22 端口失败自动写 `ssh.github.com:443` 到 `~/.ssh/config`（带备份）

### M8 系统代理接管
- [ ] 参考 clash-verge-rev 的 sysproxy 实现（Windows 注册表 / macOS networksetup / Linux gsettings）
- [ ] 读 dev-sidecar `@starknt/sysproxy` 用法
- **新实现**：本地生成 PAC（域名清单命中→127.0.0.1:port，其余 DIRECT，国内流量零打扰）；端口被占自动迁移并热更新 PAC

### M9 远程配置与自更新
- [ ] 读 dev-sidecar `remote_config.json` 及其 Gitee 托管流程
- [ ] 读 minisign 的文件格式（`filippo.io/ed25519` 实现）
- **新实现**：规则 JSON ed25519 签名；下载→验签→原子替换→失败回滚；程序自更新流量强制走自身双通道，双目录切换永不自毁

### M10 GUI（Wails + SolidJS）
- [ ] 反面教材：dev-sidecar 147MB 包
- [ ] 核对 Wails v3 是否已发 3.0 正式版（2026-09 时为 beta.19，v2 活跃维护至 v2.13.0/2026-07）
- [ ] 备选参考：syncthing 的「纯 Go 引擎 + 浏览器 Web UI」零壳模式（体积最小，体验降级的兜底方案）
- **新实现**：GUI 只通过本地 HTTP API 控制引擎；引擎必须可 headless（服务器/CI 场景）；总包 ≤15MB。壳选型决策规则见 DevWorkflow §6：v2.13+ 保底，M3 spike 时若 v3 已转正则切 v3；spike 必验托盘常驻 + 开机自启 + 单实例唤醒三件套，验不过则退 syncthing 模式

### M11 体检引擎（自研，无先例）
- [ ] 唯一可借：Ghips 的「301 指纹」判定法推广到多场景
- **新实现**：六探针（网页/登录态/IDE授权/clone/push/Release+SSH）；根因分类器：DNS污染→TCP阻断→TLS重置→源头403→限速，每类对应处方文案

### M12 众包遥测（自研）
- [ ] 设计匿名化：`hash(城市+运营商)`，无设备号无 IP 明文
- **新实现**：ClickHouse 聚合出「IP×城市×运营商×时段」质量表，回灌默认规则

---

## 3. Go 依赖速查（选型已定，勿临时更换）

| 用途 | 库 | 备注 |
|---|---|---|
| DNS 服务/客户端 | `github.com/miekg/dns` | UDP/TCP/53 |
| DoH | 自写 JSON 客户端 | alidns `/resolve` 接口，几十行 |
| 熔断（起步） | `github.com/sony/gobreaker` | M2 成熟后自研状态机替换 |
| SQLite | `modernc.org/sqlite` | pure-Go，免 CGO，交叉编译友好 |
| 签名 | `filippo.io/ed25519` | minisign 兼容格式 |
| HTTP/2 | `golang.org/x/net/http2` | 仅 M4 需要 |
| CLI | `github.com/spf13/cobra` | M1 阶段 |
| TLS 解析 | **手写** | 不用 utls（合规审计考量） |
| GUI | Wails（v2.13+ 保底 / v3 视 M3 时转正与否）+ SolidJS | 纯 Go 壳；引擎独立进程，经本地 HTTP API 通信 |

---

## 4. 协议规范速查

| 规范 | 对应模块 |
|---|---|
| RFC 6066（TLS SNI 扩展） | M3 |
| RFC 8446 §4.1.2（TLS1.3 ClientHello）/ RFC 5246 | M3 |
| RFC 8484（DNS over HTTPS） | M1 |
| RFC 7858（DNS over TLS） | M1 备用 |
| RFC 9113（HTTP/2） | M4 |
| git smart HTTP 协议 + `url.insteadOf` 官方文档 | M7 |
| Netscape PAC 规范（FindProxyForURL） | M8 |

---

## 5. 学习顺序（依赖排序）

```
M3 SNI转发器 → M2 IP调度器 → M1 自举DNS   （三者构成通道A，先啃最硬的）
→ M8 系统代理（让通道A生效）
→ M5 CDN通道 → M6 路由决策               （通道B+双通道）
→ M7 Git集成 → M11 体检                   （产品化）
→ M9 配置/更新 → M10 GUI                 （打磨）
→ M4 MITM增强 → M12 遥测                 （锦上添花）
```

## 6. 已完成模块登记（做完回填）

| 模块 | 完成日期 | 参考了哪些代码 | 实际方案与计划的偏差 | 踩坑记录 |
|---|---|---|---|---|
| — | — | — | — | — |
