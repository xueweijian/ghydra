# GHydra 工程约定与开发流程（Dev Workflow）
> **用途**：本项目的开发方式铁律——环境分工、CI/CD、测试策略、阶段门禁、壳选型决策规则。
> **维护规则**：流程变更先改此文档再改代码；每里程碑结束随 TechReference §6 一并回填实际执行情况。
> 关联文档：`GHydra-PRD.md`、`GHydra-TechReference.md`

| 项 | 内容 |
|---|---|
| 版本 | v1.0 |
| 日期 | 2026-09-12 |
| 状态 | 已拍板（含 GUI 壳改 Wails 决定） |

---

## 0. 四条铁律

1. **CI 即开发环境**：本机只写代码（零依赖、零 Go 工具链、零模型下载）；一切构建 / 测试 / 发布走 GitHub Actions（公开仓库，免费不限时）。
2. **测试优先**：先写测试后写实现；没测试的代码 = 不存在；验收标准必须能被测试证据替代。
3. **阶段串行**：上一里程碑退出标准未达成，不写下一阶段任何代码；禁止提前布局下阶段。
4. **依赖白名单**：新增 Go 依赖必须先回 TechReference §3 登记理由，防止包体积膨胀（安装包 ≤15MB 红线）。

## 1. 三环境角色分工

| 环境 | 职责 | 能做 / 不能做 |
|---|---|---|
| 本机（沙箱） | 写代码、改文档、分析报告 | ✅ 写文件 / 分析日志；❌ go build、拉依赖、装工具链、下模型 |
| GitHub Actions | lint → test → build → artifact → release | ❌ 测不了真实 GFW 行为（runner 在海外，访问 GitHub 全通） |
| 用户真机（大陆网络） | 真实网络验收 | 跑 `ghydra bench --json`，输出结构化报告贴回分析 |

## 2. CI 流水线设计

- **触发统一用 push**，不依赖 workflow_dispatch（历史教训：私有仓库 workflow 注册 0-jobs 怪癖；公开仓库无此问题，但仍统一 push 触发保持简单）
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

**版本现状（2026-09 核实）**：v2 活跃维护（v2.13.0 / 2026-07-06）；v3 仍为 beta.19，3.0 正式版无 ETA。

**决策规则（M3 第 11 周开局 spike，一周内出终局决定）**：
1. 若 Wails v3.0.0 正式版已发布（或 RC 稳定 ≥2 个月）→ 用 v3
2. 仍处于 beta → 用 v2.13+，锁定 minor 版本
3. spike 必验三件事：**托盘常驻 + 开机自启 + 单实例唤醒**（GUI 壳的全部刚需）
4. 三件验不过 → 退 syncthing 模式兜底（纯 Go 引擎 + 托盘库 + 浏览器 Web UI，零壳）

**体积账**：15MB 红线对任何壳都紧。Wails 走单二进制 hybrid（`ghydra` 默认 = 引擎+GUI，`ghydra serve` = headless）省掉第二份 Go runtime；GUI 崩 = 引擎崩的风险由 PRD「进程崩溃服务化重启」兜底。

## 7. M0 三实验（技术风险清零清单）

1. SNI 转发 PoC：ClientHello 手写解析 + 改写，真实大陆网络握手 + 持续存活率
2. 四级自举链原型：断 DNS（改 127.0.0.1:53）下仍能取到 IP
3. CF Worker 免费档大文件代理实测：CPU 时间限额 + 100–500MB Release 流式透传（通道 B 容量上限，M2 设计输入）

三实验对应 PRD M0 退出标准 ①②③。
