# GHydra 工程约定与开发流程（Dev Workflow）
> **用途**：本项目的开发方式铁律——环境分工、CI/CD、测试策略、阶段门禁、壳选型决策规则。
> **维护规则**：流程变更先改此文档再改代码；每里程碑结束随 TechReference §6 一并回填实际执行情况。
> 关联文档：`GHydra-PRD.md`、`GHydra-TechReference.md`、**`GHydra-Pitfalls.md`（工程坑合集——动手前先查，踩坑随手追加）**

| 项 | 内容 |
|---|---|
| 版本 | v1.0 |
| 日期 | 2026-09-12 |
| 状态 | 已拍板（含 GUI 壳改 Wails 决定） |

---

## 0. 四条铁律

1. **分层验证（2026-09-12 修订）**：本机已装 Go 工具链（go1.23+，约 400MB，用户批准），承担**机械验证**——`gofmt -l` / `go vet` / `go test`（纯逻辑、不涉网，秒级反馈）；CI 承担**平台验证**——三平台 race 矩阵、交叉编译、基准记录、发布产物（公开仓库，免费不限时）。大依赖下载仍然禁止（docker / rust / node / 模型权重 / 大型 SDK）。
2. **测试优先**：先写测试后写实现；没测试的代码 = 不存在；验收标准必须能被测试证据替代。
3. **阶段串行**：上一里程碑退出标准未达成，不写下一阶段任何代码；禁止提前布局下阶段。
4. **依赖白名单**：新增 Go 依赖必须先回 TechReference §3 登记理由（体积红线已于 2026-09-13 取消，白名单目的转为供应链审计与攻击面控制）。

## 1. 三环境角色分工

| 环境 | 职责 | 能做 / 不能做 |
|---|---|---|
| 本机（沙箱） | 写代码、机械验证（gofmt / vet / 纯逻辑单测，秒级）、改文档、分析报告 | ✅ go1.23+ 工具链（已装 ~400MB）；❌ 大依赖下载、docker / rust / node、模型权重 |
| GitHub Actions | 平台验证：三平台 race 矩阵、交叉编译、基准、release | ❌ 测不了真实 GFW 行为（runner 在海外，访问 GitHub 全通） |
| 用户真机（大陆网络） | 真实网络验收 | 跑 `ghydra bench --json`，输出结构化报告贴回分析 |

## 2. CI 流水线设计

- **触发统一用 push**，不依赖 workflow_dispatch（历史教训：私有仓库 workflow 注册 0-jobs 怪癖；公开仓库无此问题，但仍统一 push 触发保持简单）
- **CI Go 版本必须 ≥1.24**：macos-latest（macOS 26）的 dyld 拒绝执行无 LC_UUID 的 Go ≤1.23 race 二进制（golang/go#68678，1.24 起 linker 生成 LC_UUID）——踩坑记录 2026-09-12
- **每次 push**：gofmt 检查 → `go vet` → 单元测试（ubuntu / windows / macos 三矩阵）→ 交叉编译（windows-amd64 / darwin-arm64 / darwin-amd64 / linux-amd64）→ 上传 artifact
- **打 tag**：上述全过 → 生成 release（产物 + SHA256 校验和）
- **每个 PR 必须全绿才可合并**（自己合并自己也走 PR，留下 CI 记录）
- 基准测试（`go test -bench`）随 CI 跑，性能回归即失败：ClientHello 解析 <50µs、代理层单连接额外延迟 ≤5ms

## 3. 测试金字塔

| 层 | 跑在哪 | 内容 |
|---|---|---|
| L1 单元 | CI | ClientHello 解析器（RFC 6066 示例字节 + Wireshark 抓包 hex，存 `testdata/` 纯文本）、EWMA 评分函数、状态机全转换路径、规则匹配、PAC 生成、DoH / meta API 响应解析（fixture JSON） |
| L2 集成 | CI | 本地起假 TLS server → SNI 转发端到端断言；SQLite 缓存读写；mock 上游 403 → 降级触发 |
| L3 真机验收 | 用户本机 | M0 起每阶段出 `bench --json`（存活率 / RTT / 吞吐 / 自举链结果）——北极星指标的最早数据源，比遥测体系早 10 周 |
| L4 故障注入 | 用户本机（M2 起） | hosts 黑洞、单 IP 封禁、断 DNS |

## 4. 阶段门禁（DoD）

每里程碑结束必须三件事，缺一不进下一阶段：

1. CI 全绿 + PRD 验收项有对应测试证据（链接到 CI run 或 bench 报告）
2. 回填 TechReference §6「已完成模块登记」（实际方案 vs 计划偏差 + 踩坑记录）
3. 真机验收 JSON 归档到 `docs/evidence/M<x>/`

## 5. 仓库结构（monorepo，随里程碑生长）

```
engine/          # Go 核心（M0 起）
worker/          # CF Worker 模板（M2 起）
gui/             # Wails 壳 + SolidJS（M3 起）
docs/            # PRD / TechReference / 本约定 / evidence/
```

## 6. GUI 壳决策规则（2026-09-12 拍板：Tauri → Wails）

**背景**：引擎是 Go，选 Wails = 纯 Go 单一技术栈，CI 免装 Rust；Tauri 的 updater 插件优势被 F6「更新走自身双通道」的自研要求抵消。前端（SolidJS + fetch localhost API）在两个壳之间基本零成本迁移，架构上 GUI 只是薄客户端，壳的选型被架构隔离。

**版本现状（2026-09-13 API 核实）**：v3 = beta.20（2026-09-10，日更逼近 3.0；官方口径 "The API is stable"）；v2 最新 v2.15.0，但**无原生托盘**（wailsapp/wails#1010，同进程 "basically impossible"）。

**版本拍板（2026-09-13 用户拍板：只用 v3 做壳，覆盖下方原决策规则）**：接受 beta 风险，换取 v3 原生托盘（SystemTray，进程内）与单实例能力。纪律：锁死精确版本号（beta.20）不出范围；任何升级必须 CI 三平台全绿后合入；3.0.0 正式版发布后第一时间迁移。原「beta 不用、v2.13+ 保底」规则作废留档。

**spike 仍必验三件事**（验证对象换成 v3 实现）：**托盘常驻 + 开机自启 + 单实例唤醒**；v3 实测翻车时的逃生门仍是 syncthing 零壳模式（纯 Go 引擎 + 浏览器 Web UI + 托盘库）。

**体积账（2026-09-13 更新：红线取消）**：安装包体积不再设上限，实测数据记录进发布说明供参考（CLI 当前 10–11MB）。Wails 仍走单二进制 hybrid（`ghydra` 默认 = 引擎+GUI，`ghydra serve` = headless）——理由是省掉第二份 Go runtime 与更简的安装/更新路径，而非体积；GUI 崩 = 引擎崩的风险由「GUI 只是薄客户端、加速常驻 daemon」+ PRD「进程崩溃服务化重启」双重兜底。

## 7. M0 三实验（技术风险清零清单）

1. ✅ SNI 转发 PoC：ClientHello 手写解析 + 改写，真实大陆网络握手 + 持续存活率
   - 2026-09-12 真机实测：透传六域名 6/6 存活；**透明改写双证伪**（TLS 1.3 ServerHello 密钥派生绑 transcript / TLS 1.2 Finished 校验绑 transcript → bad record mac），改写移出功能集，仅留实验工具
2. ✅ 四级自举链原型：断 DNS（改 127.0.0.1:53）下仍能取到 IP
   - 2026-09-12 真机实测：L2 DoH 服务 IP 直连（223.5.5.5）227ms 兜底 / L3 缓存 0.4ms 兜底；DoH 端点必须配 IP（域名形式在断 DNS 下自锁，已修复）
3. 通道 B 容量（改判）：M0 口径 = 公开数据 + 生产案例佐证（gh-proxy.com 7T/日、cf-ghproxy-worker 模板）；免费档针对性实测挪至 M2 开局，带产品需求测更准
4. ✅ 千并发（2026-09-12 沙箱实测）：3000 连接成功率 100%、goroutine 零泄漏、HeapSys 28MB；瞬时风暴 p99 尾延迟 = accept backlog × SYN 重传退避（阶梯对照 1.15s 佐证，非代码缺陷）→ M1 待办 ListenConfig backlog 调优 + 连接复用池

三实验对应 PRD M0 退出标准 ①②③（③ 已改判为公开数据佐证口径）。
