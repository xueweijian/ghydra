# GHydra M1 实施方案 · 直连通道产品化
> **用途**：里程碑 M1（PRD §8，第 3–6 周）的完整实施规划。评审通过后按周执行，每周结束回填状态。
> **关联**：`GHydra-PRD.md` §8 M1、`GHydra-TechReference.md` 模块卡片 M2/M3/M8、`GHydra-DevWorkflow.md` 四铁律。
> **状态**：执行中（2026-09-12）。**W1 已收官**（内核/rules/listener/serve/loadtest-connect + CI 三平台绿 + 真网冒烟 6/6，证据 `docs/evidence/M1/`）；cobra 改为手写路由（TechReference §3 已回填）。W2 设计见 `GHydra-M1-W2-Design.md`。

---

## 0. 术语对齐（避免两套编号打架）

- **里程碑 M1**（PRD 时间维度）≠ **模块编号 M1–M12**（TechReference 模块维度）。
- 里程碑 M1 覆盖的模块：**M3 转发器产品化**、**M2 IP 调度器**、**M8 系统代理**、**M9 本地部分**（规则+SQLite）、**M11 子集**（doctor 探针）。
- 已完成资产：模块 M1 自举链（`engine/bootstrap/` ✅）、模块 M3 PoC（`engine/sni/` + main.go relay ✅）。

## 1. 目标与范围

**目标**（PRD 原文）：通道 A 达到可日常自用（dogfooding）。退出标准：作者本人 7 天全部 GitHub 流量走它，六场景可用率 ≥ 97%。

| In scope | Out of scope（防蔓延） |
|---|---|
| HTTP CONNECT 代理内核（正式引擎） | 通道 B / CDN / 降级决策器（M2） |
| IP 调度器（EWMA+状态机+按需探测+熔断） | ed25519 远程规则热更新（M3，M1 用内置规则） |
| 规则引擎 + SQLite（last-good / 指标） | MITM 增强模式（M4） |
| 系统代理 / PAC 三平台接管 + 恢复 | Git insteadOf / ssh:443 自动写入（M2 F5） |
| CLI：`ghydra on/off/status/doctor/serve` | 完整根因分类器（M11 全版，M1 只做探针+基础分类） |
| doctor 六场景探针 + 可用率统计 | GUI（M3）、众包遥测（M4） |
| backlog 调优 + buffer 池（M0 遗留待办） | 透明拦截模式（iptables/WFP，非本里程碑） |

**范围裁剪声明（需评审确认）**：M1 的 dogfooding 覆盖**网页 / 登录 / clone / push / Release** 五场景（全部走 HTTPS CONNECT）。SSH 场景只做**探测与计量**（doctor 报告 22/443 连通性），不承诺加速——SSH 加速依赖 F5 的 `ssh.github.com:443` 自动写入，属 M2。97% 分母 = 实际覆盖的五场景。

## 2. 架构

### 2.1 核心升级：从裸 SNI 转发到 CONNECT 代理

M0 的 PoC 是裸 TCP SNI 转发（客户端直连 8443 端口）。产品主路径改为**HTTP CONNECT 代理**——这是系统代理/PAC 的标准协议，浏览器与 git 原生支持：

```
客户端(浏览器/git)                     GitHub
    │  CONNECT github.com:443            │
    ▼                                    │
[ gh 主端口 127.0.0.1:9801 ]              │
    ├─ 规则引擎: 命中加速域名? ──否──► 系统DNS原样直连转发(零打扰)
    │            │是
    │            ▼
    │      IP 调度器: 择优 IP(粘性60s/熔断/EWMA)
    │            │
    │      连 IP:443, TLS 流原样双向 io.Copy
    │      (ClientHello 的 SNI 由客户端自带, 原样透传)
    ▼
 同端口 HTTP 服务: GET /pac → PAC 文件 / GET /status → JSON(PAC托管复用主端口)
```

ClientHello 解析保留两个用途：① CONNECT 隧道内校验 SNI 与目标一致性（防滥用 + 统计）② `ghydra poc` 调试工具。**产品路径不再依赖解析 SNI 拿域名**（CONNECT 头已给）——解析开销从热路径移到校验路径。

### 2.2 包结构（monorepo 生长）

```
engine/
  sni/        ✅ ClientHello 解析（M0）
  bootstrap/  ✅ 四级自举链（M0）
  proxy/      🆕 CONNECT 代理内核：listener(自建socket+backlog) / 隧道 / buffer池 / 非加速域名直连
  sched/      🆕 IP 调度器：池 / EWMA评分 / 五态状态机 / 粘性 / 按需探测
  rules/      🆕 规则引擎：域名匹配 / 内置规则(meta domains 生成) / PAC 生成
  store/      🆕 SQLite：last-good IP / 探测指标 / 可用率数据
  sysproxy/   🆕 系统代理接管：接口抽象 + win(regedit)/mac(networksetup)/linux(gsettings) 构建 tags
  probe/      🆕 六场景探针 + 可用率统计（doctor 引擎）
  cmd/ghydra/ 🔧 cobra 子命令化：serve/on/off/status/doctor/bench/poc/loadtest
```

## 3. 关键设计决策（调研依据附后）

**D1 · backlog 用自建 socket listener。** exa 调研确认：Go `net.Listen` 固定 `listen(fd, somaxconn)`，标准库不暴露 backlog 参数；Linux 上应用值被 `net.core.somaxconn` clamp（沙箱/许多发行版 =128，正是 M0 千并发风暴尾延迟的根因）。方案：`syscall.Socket→SetsockoptInt(SO_REUSEADDR)→Bind→Listen(fd, 4096)→net.FileListener`；同时 listener 上设 `TCP_NODELAY`（accepted conn 继承，降低转发延迟）。Windows backlog 语义不同（200 默认但可更大），单独实测后定值。**M0 的「连接复用池」说法修正**：SNI/CONNECT 隧道是端到端 TLS，上游连接无法跨客户端连接复用——实际落地为 ①`sync.Pool` 复用每连接 2×4KB buffer ②探测连接走 `http.Transport` 连接池。

**D2 · 跳过 gobreaker，直接自研 IP 状态机。** gobreaker（调研确认）是三态熔断器，语义与 IP 级五态（New/Probing/Active/Cooldown/Quarantine）+ EWMA 评分 + 同域粘性完全不匹配，引入再替换是弯路。**gobreaker 留给 M2 通道级决策器**（三态模型的正确场景）。M1 零新依赖实现调度器。

**D3 · gopsutil 砍掉**（PRD 技术栏原列）。资源自监控用 `runtime.ReadMemStats` 足够；15MB 红线下能省则省。

**D4 · 系统代理设置 = 接口抽象 + 构建标签 + 防御性恢复。** 沙箱测不了 Windows/macOS 真实设置。设计约束：设置前保存原值快照（SQLite）；`off` 与进程退出 hook（SIGINT/SIGTERM/panic）双路恢复；设置失败即整体回滚报错退出，不留半接管状态。

**D5 · 验收平台 = 用户主力 Windows 开发机**（2026-09-12 拍板）。sysproxy 实现优先序：**Windows 优先**（注册表 Internet Settings + PAC AutoConfigURL），macOS/Linux 随后；沙箱 Android/Linux 只做冒烟与逻辑验证；CI 三平台矩阵保证编译与单测。

**D6 · 可用率度量的公平性。** 探针每小时自动跑一轮（走 GHydra 通道），同时跑**直连对照组**（不过代理）——GitHub 自身故障不计入 GHydra 失分，报告两列对照。数据落 SQLite，`doctor report` 出 7 天汇总。

## 4. 四周任务分解

### W1（第 3 周）· proxy 内核 + 规则骨架
| 任务 | 交付 | 测试证据 |
|---|---|---|
| CONNECT 隧道解析（RFC 7231 §4.3.6）+ 非加速域名直连放行 | `engine/proxy/tunnel.go` | L2：假上游 CONNECT 端到端（复用 loadtest 假服务器） |
| 自建 socket listener（backlog 4096 + TCP_NODELAY + SO_REUSEADDR） | `engine/proxy/listener.go` | L2：风暴压测对比（1000 并发 p99 目标 <1s，验证 M0 根因修复） |
| buffer 池（sync.Pool）+ 每连接错误上报钩子（喂调度器） | `engine/proxy/proxy.go` | L1：池借还配对（race detector） |
| 规则引擎：域名匹配（精确/后缀/通配）+ 内置规则生成器（meta domains） | `engine/rules/` | L1：表驱动匹配用例 + 真实 meta JSON fixture |
| CLI 骨架迁移 cobra，`ghydra serve` 可用 | cmd 重构 | 手动：`curl -x http://127.0.0.1:9801 https://github.com` |

### W2（第 4 周）· IP 调度器 + SQLite
| 前置阅读（开工前 1 天） | 消化点 |
|---|---|
| FastGithub fork `FastGithub.DomainResolve/` | 池结构 / 打分 / 持久化 |
| dev-sidecar `SpeedTester`（v2.2） | 按需探测轮转 / 7s 超时 / 1 次熔断参数 |

| 任务 | 交付 | 测试证据 |
|---|---|---|
| EWMA（RTT/失败率/抖动）+ score=0.3R+0.5F+0.2J | `engine/sched/ewma.go` | L1：数学性质（收敛、权重、冷启动） |
| 五态状态机全转换 + 30s 指数退避 + 5min 复活 | `engine/sched/state.go` | L1：全路径表驱动 |
| 同域粘性 60s（熔断优先于粘性——失败信号立即换 IP） | `engine/sched/sticky.go` | L1：粘性窗口内注入失败 → 立即切换 |
| 按需探测（轮转分配+成功入池+失败跳过） | `engine/sched/probe.go` | L2：假 IP（127.0.0.1:1）注入 → 7s 内切换 |
| bootstrap → sched 喂数（meta/DoH 候选入池） | 集成 | L2：fixture 注入候选流 |
| SQLite：last-good / 池快照 / 指标（modernc.org/sqlite） | `engine/store/` | L2：读写 + 断电重启恢复 |

### W3（第 5 周）· 系统代理 / PAC
| 任务 | 交付 | 测试证据 |
|---|---|---|
| PAC 生成（域名清单 → FindProxyForURL）+ 同端口托管（/pac） | `engine/rules/pac.go` | L1：PAC 输出快照测试 |
| sysproxy 接口 + 三平台实现（构建 tags） | `engine/sysproxy/` | L1：Linux(gsettings/env) 可测；win/mac 由 CI 编译 + 用户真机验收 |
| 端口冲突自动迁移 + PAC 热更新 | `engine/proxy/port.go` | L2：占坑 → 迁移 → 新 PAC 生效 |
| 崩溃安全：信号 hook + 原值快照恢复 | `engine/sysproxy/restore.go` | L2：kill -9 后下次启动检测残留并清理（快照对账） |
| `ghydra on / off` | cmd | 手动 + 沙箱 Linux 冒烟 |

### W4（第 6 周）· doctor 探针 + v0.1 + dogfooding 启动
| 任务 | 交付 | 测试证据 |
|---|---|---|
| 六场景探针（网页/登录/clone dry-run/push dry-run/Release TTFB+速率/SSH 22+443 连通） | `engine/probe/` | L2：mock 各场景响应；L3：真机报告 |
| 基础根因分类（能分则分：DNS污染/TCP阻断/TLS重置/超时——完整版留 M11） | `engine/probe/classify.go` | L1：注入各错误类型断言分类 |
| 可用率统计（每小时自动探测 + 直连对照组 + 7 天汇总） | `engine/probe/sla.go` | L2：时间模拟（假时钟）统计正确性 |
| `ghydra status / doctor` + bench --mode proxy | cmd | 真机 L3 报告 |
| v0.1 tag + release（CI artifact + SHA256） | 发布 | CI release 流水线 |
| **dogfooding 启动**（用户主力 PC，7 天） | 验收数据 | 每日 doctor report 贴回 |

## 5. 测试与验收（映射 DevWorkflow 金字塔）

- **L1 单元（CI 三平台）**：EWMA、状态机、规则匹配、PAC、分类器、粘性/熔断交互。
- **L2 集成（CI）**：CONNECT 端到端（假上游复用 loadtest 设施）、故障注入切换、SQLite 恢复、端口迁移、崩溃恢复对账、serve 冒烟。
- **L3 真机（沙箱 + 用户 PC）**：`ghydra bench --json --mode proxy`（过代理的六域名存活率 + 调度器视角 dump：池状态/评分/切换事件流）。
- **L4 故障注入（提前引入单项）**：加速表注入假 IP → 熔断切换 <7s；完整矩阵（hosts 黑洞/断 DNS/403）仍留 M2。

## 6. 风险清单

| # | 风险 | 对策 |
|---|---|---|
| R1 | win/mac 系统代理设置沙箱不可测 | D4 接口抽象+真机验收+防御性恢复；设置逻辑保持极薄 |
| R2 | dogfooding 期间 bug 阻断用户工作 | 一键 off；退出 hook 恢复；崩溃后快照对账自愈；探针对照组区分「我们的锅 vs GitHub 的锅」 |
| R3 | 粘性窗口内 IP 变坏拖慢切换 | 熔断优先于粘性（设计如此+测试锁定） |
| R4 | GitHub IP 段/域名变更 | 自举链三重冗余已备（meta/DoH/缓存） |
| R5 | PAC 托管端口冲突 | 与代理主端口复用（http.Server 按 path 分流） |
| R6 | Linux 沙箱无完整桌面环境 | Linux sysproxy 降级为 env vars+引导文案，真机 PC 为主验收 |
| R7 | 可用率 97% 度量争议 | 探针直连对照组，报告双列（D6） |
| R8 | 自建 socket 跨平台差异（fd 标志/继承） | listener 单独集成测试 + 三平台 CI race 矩阵 + Windows 单独实测 backlog 行为 |

## 7. 依赖登记增量（TechReference §3 回填项）

| 用途 | 库 | 状态 |
|---|---|---|
| CLI | spf13/cobra | 已登记 ✓ |
| SQLite | modernc.org/sqlite | 已登记 ✓ |
| ~~资源监控~~ | ~~gopsutil~~ | **砍掉**（D3，runtime 自带够用） |
| ~~熔断~~ | ~~sony/gobreaker~~ | **M1 不引入**（D2，M2 通道级再议） |

新增第三方依赖：**零**。listener 自建走 syscall 标准库。

## 8. 退出标准（可测量版，DoD）

1. CI 全绿：三平台 race + 交叉编译 + 新增 serve 冒烟 + 基准（ClientHello <50µs 保持；新增代理层单连接额外延迟 ≤5ms 基准）
2. 沙箱 L3：`bench --mode proxy` 六域名存活率 100%（连续 3 天每日 ≥1 份报告归档）
3. 故障注入：假 IP 切换 <7s（CI L2 证据链接）
4. 用户 PC 7 天 dogfooding：五场景可用率 ≥97%（SSH 仅探测计量）+ 直连对照组数据
5. 资源红线：CI artifact ≤15MB、常驻内存 ≤120MB（status 报告字段）
6. 文档回填：TechReference §6 模块登记 + `docs/evidence/M1/` 归档（bench JSON ×3 + dogfooding 日报）

## 9. 拍板记录（2026-09-12，用户确认）

1. **主力平台 = Windows**（D5）：dogfooding artifact = windows-amd64，sysproxy 实现序 win → mac → linux。
2. **SSH 场景裁剪确认**：M1 只探测不加速，97% 分母 = 五场景。
3. **验收公平性确认**：探针直连对照组，报告双列（D6）。
4. **节奏**：不绑日历周——周仅为工作量单位，模块完成即推进，每模块收口时小结 + 测试证据归档。

**开工条件**：W2 前置阅读（FastGithub fork `FastGithub.DomainResolve/` + dev-sidecar `SpeedTester`）用 MCP 抓源码消化，先于调度器编码执行。
