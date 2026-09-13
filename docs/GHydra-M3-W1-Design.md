# GHydra M3-W1 设计 · 控制 API + 前端骨架

> 状态：已定稿（2026-09-13）。前置：W0 壳 spike 收官（9c1a78e，v3 beta.20 锁定，CI 四 job 绿）。
> 映射：M3-Plan §W1 / D3。产出本文件后开工。

## 0. 范围

**做**：
1. `engine/api`：REST 全套 + SSE + token + Host 校验 + MITM 桩端点
2. serve 集成：token 文件生命周期、`/api/*` 挂载、`--gui-dist` 静态托管（SPA fallback）
3. `engine/get`：OnProgress 回调 + 下载任务注册表（daemon 内 get 任务化）
4. doctor 手动触发（异步单飞 + SSE 报告帧）
5. 前端骨架：types/client/SSE hook + 布局路由 + 状态页接 SSE + 其余页面占位
6. golden JSON 双端 schema 锁定（Go ↔ TS）
7. CLI 出口 `ghydra token`（浏览器兜底模式取 token）

**不做（防膨胀）**：
- GUI 拉起/连接 daemon 的生命周期编排 → W4（D1 hybrid 的 launcher 部分）
- MITM 引擎本体 → W2/W3（本 W1 只留 501 桩）
- /config 完整读写面 → 只读 + cdn 单字段写；rules.json 热更是 W5
- token 的 Windows ACL 强化 → W6 打包收尾
- 前端完整功能页 → W4

## 1. 安全模型（对 M3-Plan D3 的收紧修订）

D3 原文「只读接口免 token」。**修订：`/api/*` 读写一律要求 token**，理由：

- W0 已给 `/status` `/pac` 放了 `ACAO:*`（GUI 壳跨域需要）。若 `/api/status`、`/api/events` 只读免 token 且 CORS 放开，任意恶意网页的 JS 可静默读走：择优 IP 池、连接流量、通道熔断状态——免费给攻击者做「本机是否装有代理工具」的指纹探测。DNS rebinding 防不住这个（那是 Host 校验的职责，正常跨域请求 Host 合法）。
- token 在本机就是一切钥匙：能读 token 文件的本地进程本来就能干任何事。所以「读也要 token」没有增加可用性成本——GUI 壳直接读文件（无感），浏览器兜底模式用户粘一次（localStorage）。
- 模型简单一致 = 审计面小。PAC 例外：`/pac` 必须免 token 免 CORS 限制（浏览器 PAC 引擎不发自定义头），保持 W0 行为不动。

**分层**：
| 层 | 机制 | 挡什么 |
|---|---|---|
| 绑定 | 127.0.0.1 only（现状） | 局域网直接访问 |
| Host 校验 | r.Host ∈ {127.0.0.1, localhost, [::1]}（端口忽略，大小写归一） | DNS rebinding |
| token | 读写全要；`X-GHydra-Token` 头 / `Authorization: Bearer` / `?token=`（EventSource 不能自定义头，SSE 走 query） | 跨域网页 JS、本机无权限进程 |
| CORS | `ACAO: *` + `Access-Control-Allow-Headers: x-ghydra-token, authorization, content-type` + OPTIONS 预检 204 | 不挡（token 是核心）；wails 壳（http://wails.localhost / wails:// 等 origin 不定）与浏览器模式都能过预检 |
| SSE 连接上限 | 8 并发（超出 503） | 本机资源耗尽 |

**token 生命周期**：独立文件 `~/.ghydra/api-token`（0600），daemon 启动 load-or-create（32B crypto/rand hex）。不进 serve.json——`ghydra serve` 裸跑（CI/dev）也自然工作，on/off 生命周期与 token 解耦。flags：`--api-token`（直接注入，CI/测试）、`--api-token-file`（自定义路径）。CLI `ghydra token` 打印（+`--json`）；`ghydra diag`（W6）导出时脱敏。

## 2. API 契约

统一约定：JSON；错误 `{ "error": "msg" }` + 4xx/5xx；401 = token 缺失/错误，403 = Host 校验失败，503 = 资源上限，501 = 桩（MITM）。

| Method & Path | Auth | Req | Resp | 备注 |
|---|---|---|---|---|
| GET `/api/status` | token | — | `ApiStatus`（golden） | listen/scheduler/conns/uptime + pools + channel + cdn + api 版本 |
| GET `/api/config` | token | — | `ApiConfig` | 运行配置快照（只读字段标注 `hot: false`） |
| POST `/api/config` | token | `{"cdn": "https://..."}` | 同 GET | 仅 cdn 可热更（B 端点运行时换）；未知字段 400 列名 |
| POST `/api/on` | token | `{"mode": "pac"\|"proxy"}` | `{"ok": true, "mode": ...}` | daemon 视角接管：快照+sysproxy.Apply；失败回滚快照（对齐 onCmd D4） |
| POST `/api/off` | token | `{"shutdown": false}` | `{"ok": true, "shutdown": ...}` | 恢复快照+还原 git/ssh 托管（对齐 offCmd）；shutdown=true 时 daemon 优雅退出（SIGTERM 等价路径） |
| POST `/api/doctor/run` | token | `{"repo": "owner/name"?}` | `{"run_id": "...", "started": true}` | 异步单飞；跑完 SSE `doctor` 帧 + store 持久化；已在跑 → 409 |
| GET `/api/doctor/summary` | token | `?hours=24` | `{"runs": [DoctorSummary...]}` | 直连 store.DoctorSummary |
| POST `/api/git/enable` | token | `{"cdn": "...", "push_via_b": false, "ssh_rewrite": false, "force": false}` | `{"written": [...], "snapshot": true}` | 复用 gitEnable 核心（含托管快照） |
| POST `/api/git/disable` | token | — | `{"restored": true}` | 复用 gitDisable |
| GET `/api/git/status` | token | — | gitStatus 输出 JSON 化 | |
| POST `/api/ssh/{enable,disable}` | token | `{"alias": ""?}` | 同理 | 复用 sshEnable/sshDisable |
| GET `/api/ssh/status` | token | — | 同理 | |
| POST `/api/get/start` | token | `{"url": "...", "dst": "..."?}` | `{"task_id": "...", "dst": "..."}` | dst 空 = `~/Downloads/<basename>`；并发单飞（一次一个下载）→ 409 |
| GET `/api/get/progress` | token | — | `{"active": {...}\|null, "recent": [...]}` | 内存注册表（上限 32 历史） |
| GET `/api/mitm/status` | token | — | `{"available": false, "enabled": false, "reason": "engine lands in W2"}` | 桩，schema 先行（W4 GUI 直接消费） |
| POST `/api/mitm/{enable,disable,uninstall-cert}` | token | — | 501 | 桩 |
| GET `/api/events` | token(query) | — | SSE 流 | §3 |

`/pac`、`/status`（M2 形状）保持不变——drill/doctor_loop 在用，W1 不动它们。

## 3. SSE 协议（`/api/events`）

- Content-Type `text/event-stream`；心跳 `: ping\n\n` 每 15s（防中间层掐空闲连接）。
- 帧类型（`event:` 名）：
  - `status` —— 聚合帧：`ApiStatus` 全量。**1s 节拍 diff**（与上一帧 JSON 相同则不发）。生产端：serve 内 ticker goroutine 拉快照。连接建立即发首帧（前端秒出画面）。
  - `conn` —— 单条连接事件（proxy.Event 精简：host/rx/tx/dial_ms/accel/ok/ts）。≥250ms 合并批量：`{"events": [...]}`（防高频连接刷屏）。
  - `doctor` —— 手动/周期 doctor 完成：`{"run_id", "verdict", "proxy_ok", "direct_ok", "cdn_ok", "finished_at"}` + 完整 report 字段。
  - `get` —— 下载进度：`{"task_id", "url", "got_bytes", "total_bytes", "rate_bps", "channel", "done", "error"}`，**250ms 节流**（OnProgress 回调原始频率更高）。
  - `hello` —— 连接确认帧：`{"proto": 1, "heartbeat_s": 15}`。
- hub 实现：`map[chan []byte]struct{}` + mutex；Subscribe 带上限；Write 超时 5s 丢帧（慢消费者不拖 hub；连续超时踢线）。断线重连由前端 EventSource 自带（query token 重放）。

## 4. 装配与依赖方向

```
engine/cmd/ghydra (serveCmd)
  ├─ 组装 closures ──► engine/api.New(Deps{...})
  │     Deps: StatusFn, ConfigGetFn, ConfigSetFn, OnFn, OffFn,
  │           DoctorRunFn, DoctorSummaryFn, GitFn, SSHFn, GetStartFn,
  │           GetProgressFn, ConnsFn (订阅 proxy.Event 流), MITMStub
  └─ mux.Handle("/api/", apiServer)   // /api/ 前缀整体接管
```

- **engine/api 不 import channel/probe/get/store**（只认函数签名与自身 DTO）→ 单测零依赖、httptest 全覆盖。
- cmd 侧把现有逻辑（onCmd/offCmd/gitEnable/sshEnable/doctorLoop 内构造）抽成可复用函数供 CLI 与 API 两条入口共用——**单一实现，两入口**（防 CLI/API 行为漂移）。
- `Deps.MITM` 由 api 包提供常量桩实现（W2 换真实现，只动 cmd 装配）。

## 5. get 任务化

- `get.Downloader` 加 `OnProgress func(got, total int64, ch string)`（stream 循环内按块回调；nil = 零开销）。
- api 侧 `TaskRegistry`：`start` 单飞锁 + active 指针 + ring 历史（32）；进度经节流器进 SSE；结束写 `store.AppendDownload`（run_id 复用 doctorRunID 风格）。
- 失败任务保留 error 与段轨迹（Result 已有 Segments——诊断价值，W4 下载页直接展示）。

## 6. dist 双模式托管

- serve 新 flag `--gui-dist`（默认：exe 同目录 `dist/`）。存在 → `mux.Handle("/", staticSPA(dist))`（index.html fallback，assets MIME 正确）；不存在 → 维持现状（明文 HTTP 代理路径）——CLI-only 安装零影响。
- gui 壳：go:embed（现状不变）。**同一份 dist 两种入口**（W1 退出标准③的双模式冒烟）。
- 注：engine 与 gui 是两个 Go module，go:embed 跨不了——目录托管是结构必然，不是妥协。

## 7. 前端骨架（gui/frontend）

```
src/
  api/client.ts    // token 管理（localStorage ghydra.token + 引导输入）+ apiFetch 包装（401→事件）
  api/sse.ts       // useEvents()：EventSource ?token=，帧分发 signal
  types.ts         // 与 golden 逐一对齐（vitest 编译期锁定）
  pages/
    Status.tsx     // 接 SSE：channel 快照 + conn 流 + doctor 摘要（本 W1 完整）
    Boost.tsx      // on/off/git/ssh 开关骨架（按钮就绪，W4 补交互细节）
    Downloads.tsx  // get 任务列表骨架（SSE get 帧驱动）
    Mitm.tsx       // 占位（消费 /api/mitm/status 的 available:false → "W2 上线"）
    Settings.tsx   // cdn 编辑 + daemon 地址 + token 输入
  App.tsx          // 侧栏布局 + 路由
```

token 引导 UX：任一 API 401 → 顶部横幅「粘贴 API token（终端运行 `ghydra token`）」→ localStorage。壳模式后续（W4）改为壳进程直接读文件注入，localStorage 作为兜底模式通道。

## 8. golden 双端锁定

- Go：`engine/api/testdata/golden/{status,config,doctor_summary,get_progress,mitm_status,conn_frame,doctor_frame,get_frame}.json`——golden 测试从真实 handler 构造响应序列化，与提交文件 byte 比对（`GOFLAGS` 无关、平台无关：固定时间戳注入 `Now` 函数）。
- TS：`gui/frontend/src/__tests__/golden.test.ts` —— `import golden from '../../engine/api/testdata/golden/status.json'` 赋给 `ApiStatus`（vitest + esbuild JSON import）。类型漂移 = 前端 job 编译失败。
- 两端 CI 都跑 → 改任何一端 schema 必须显式改 golden（review 可见）。

## 9. 测试矩阵

| 层 | 用例 |
|---|---|
| api 单测（httptest） | Host guard（evil.com→403、localhost:1234→过）；token guard（缺/错→401、query/header/Bearer 三形态）；CORS 预检（OPTIONS→204+头）；每端点 happy + 错误路径；on 失败回滚断言；doctor 并发 409；get 并发 409；MITM 桩 501；SSE（首帧 hello、status diff 不重发、心跳、连接上限 503、慢消费者踢线） |
| golden | 上述 §8 |
| serve 集成（本地+CI） | 固定 `--api-token test123` 起 serve → curl：无 token 401 / 带 token 200 / SSE 首帧可读 / `--gui-dist` GET / 200 且 index.html 即 dist 产物 / 无 dist 目录时明文代理路径回归不变 |
| 前端 | vitest：golden 类型锁定 + client（mock fetch 401 分发）+ sse 帧解析 |
| 回归 | ci.yml 全矩阵（/status /pac /drill 零变化） |

CI 变更：
- `ci.yml`：无新 job（api 测试进 `./...`）；ubuntu job 加 serve 集成冒烟步。
- `ci-gui.yml`：前端 job 加 `npx vitest run`（golden 引擎目录在同 checkout）。

## 10. 风险

| # | 风险 | 对策 |
|---|---|---|
| W1-R1 | SSE 经 WebView2/WKWebView 长连接兼容性（代理/缓冲掐流） | 心跳 15s；前端 EventSource 原生重连；W1 双模式冒烟实测（xvfb + 浏览器） |
| W1-R2 | on/off 抽函数重构动 M2 代码面 | 只抽不改语义；daemon_integration_test 回归；drill CI 是黑盒护栏 |
| W1-R3 | query token 进访问日志 | serve 日志目前不打 URL query（只打 host/结果）；加断言测试 |
| W1-R4 | token 文件权限在 Windows 弱化（0600≠ACL） | W1 接受（威胁模型=本机文件读取者已是 owner）；W6 安装器收尾 |
| W1-R5 | api 契约过早冻结（W4 才是真消费者） | golden 机制让 schema 演进成本=改一个文件，刻意低摩擦 |

## 11. W1 退出标准

1. `engine/api` httptest 全绿（含 rebinding/token 攻击面用例）+ golden 双端锁定绿。
2. serve 集成冒烟绿（token/SSE/dist 托管/无 dist 回归），ci.yml + ci-gui.yml 全绿。
3. 双模式：xvfb 壳内前端加载 + serve 直托管 dist，同一构建产物。
4. 前端骨架：状态页实时（SSE 驱动，含 conn 流），其余页面占位就绪。
5. `ghydra token` 可用；drill/E2E 既有测试零回归。

## 12. 实施顺序

1. `engine/api`：hub + guards + 端点骨架 + 单测（纯包内闭环，最快见效）
2. get OnProgress + 任务注册表 → api 接线
3. cmd 抽函数（on/off/git/ssh/doctor 核心逻辑 → 双入口）
4. serve 装配（token 文件/flags/--gui-dist/`/api/` 挂载/SSE 生产者）
5. golden 生成 + 前端 types/client/sse + 状态页
6. CI 两 workflow 更新 + 集成冒烟 + 全绿
7. `ghydra token` + 文档收尾（本文件补实施结果）

## 13. 实施结果（2026-09-13 收官）

- **交付 commit**：afd7510（设计）/ 07fd713（engine/api）/ 847b07f（serve 装配 + 双入口核 + get 任务化）/ 4f0a29f（前端骨架 + CI）。
- **测试**：engine/api 全绿（guards/端点/SSE/hub/golden + 时间戳形态锁定）；get 全绿（含 OnProgress 契约：表头首帧/节流/终态/切道通道名）；cmd 全仓回归（daemon 集成测试零破坏）；serve 冒烟 12/12（smoke_w1.sh，CI 化为 api-smoke job）；前端 vitest 11/11（golden 双端锁定）+ vite build。
- **真面板验证**（serve 托管 dist + 浏览器）：SSE hello/status/conn 帧端到端实时；连接卡片/IP 池/连接流渲染正确；A 路径失败事件如实红显。发现并修复：`daemonBase()` 浏览器模式应默认 `location.origin`（serve 同源托管），壳模式回落 9801。
- **对设计的两处偏差**（记录在案）：
  1. DoctorFrame 增加 `error` 字段（手动触发失败也要上屏）；
  2. git/ssh 的 CLI 保留原有富展示，核逻辑单源在 engine/gitcfg、engine/sshcfg——cmd 层编排不强行合并（输出形态差异属于展示层，非行为漂移风险点）。
- **遗留到 W4**：窄屏 conn 流表格样式、壳进程注入 token（localStorage 为兜底）、Boost 页 CDN 取值贯通设置页。
